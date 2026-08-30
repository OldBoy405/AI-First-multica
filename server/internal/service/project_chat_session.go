package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProjectChatSessionAdvisoryPrefix is the authoritative project-level advisory
// key prefix shared by GET Ensure, adoption, Bind, PATCH, rebind and the
// forwarding EnsureProjectChatIssue (SDD §2.1, BLOCK-016). The full key is
// {prefix}|{workspace_id}|{project_id}.
const ProjectChatSessionAdvisoryPrefix = "project-chat-session"

var (
	// ErrChatSessionNotFound: the session row does not exist in this
	// workspace/project (handler: 404).
	ErrChatSessionNotFound = errors.New("chat session not found")
	// ErrChatSessionClosedOrChanged: the session is closed or its agent no
	// longer matches the project's current binding (handler: 409
	// chat_session_closed_or_changed).
	ErrChatSessionClosedOrChanged = errors.New("chat session closed or agent changed")
	// ErrTeamAgentNotConfigured: the project has no Team Agent bound; config
	// and send operations refuse (handler: 409 team_agent_not_configured).
	ErrTeamAgentNotConfigured = errors.New("project has no Team Agent configured")
	// ErrForbiddenChatConfig: the caller is not a workspace owner/admin
	// (handler: 403 forbidden_chat_config, AC-6).
	ErrForbiddenChatConfig = errors.New("chat config requires owner or admin")
)

// ProjectChatSessionView is the display payload for GET / PATCH / container /
// messages (SDD §3.1). IssueID is nil until a container is bound.
type ProjectChatSessionView struct {
	SessionID   string
	IssueID     *string
	TeamAgentID string
	Model       string
	ModelSource ChatConfigSource
	// ThinkingLevel carries the effective thinking level; an empty string is
	// a legal value ("follow the runtime default").
	ThinkingLevel       string
	ThinkingLevelSource ChatConfigSource
}

// ChatConfigFieldPatch is the handler-resolved three-state patch for one
// field (SDD FR-6): absent = keep, Clear = write SQL NULL, Value = set.
type ChatConfigFieldPatch struct {
	Present bool
	Clear   bool
	Value   string
}

func projectChatSessionAdvisoryKey(workspaceID, projectID pgtype.UUID) string {
	return strings.Join([]string{ProjectChatSessionAdvisoryPrefix, util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
}

// teamAgentIDFromSettings reads the bound Team Agent out of a project settings
// bag. Zero UUID = unconfigured (the frontend's setup CTA state, not an error
// for GET).
func teamAgentIDFromSettings(settings []byte) pgtype.UUID {
	if len(settings) == 0 {
		return pgtype.UUID{}
	}
	var bag map[string]any
	if err := json.Unmarshal(settings, &bag); err != nil {
		return pgtype.UUID{}
	}
	return uuidSettingFromBag(bag, ProjectSettingTeamAgentID)
}

// EnsureProjectChatSession resolves (lazily creating on first use) the active
// Team Agent chat session for a project (SDD §4.1). GET-only entry point: it
// never creates the container issue, never calls EnsureProjectChatIssue.
//
// Unconfigured projects return a view with an empty TeamAgentID and no session
// created. The project-level advisory (shared with PATCH/Bind/rebind/forwarding
// Ensure, BLOCK-016) is held while the binding is re-read, so a concurrent
// rebind can never leave a session stamped with a stale agent.
func (s *IssueService) EnsureProjectChatSession(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID) (*ProjectChatSessionView, error) {
	if !workspaceID.Valid || !projectID.Valid {
		return nil, fmt.Errorf("ensure project chat session: workspace and project required")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	if err := qtx.LockIssueDuplicateKey(ctx, projectChatSessionAdvisoryKey(workspaceID, projectID)); err != nil {
		return nil, fmt.Errorf("lock project chat session key: %w", err)
	}
	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: projectID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("reload project under session lock: %w", err)
	}
	teamAgentID := teamAgentIDFromSettings(project.Settings)
	if !teamAgentID.Valid {
		return &ProjectChatSessionView{}, nil
	}

	agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID: teamAgentID, WorkspaceID: workspaceID,
	})
	baseModel, baseThinking := pgtype.Text{}, pgtype.Text{}
	if err == nil {
		baseModel, baseThinking = snapshotAgentDefaults(agent)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load team agent: %w", err)
	}
	// A binding pointing at a deleted/foreign agent still creates the session
	// (binding lifecycle, not session lifecycle); defaults degrade to the
	// empty follow-runtime sentinel.

	session, err := qtx.GetActiveProjectChatSession(ctx, db.GetActiveProjectChatSessionParams{
		WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		id := uuid.New()
		session, err = qtx.InsertProjectChatSession(ctx, db.InsertProjectChatSessionParams{
			ID:                pgtype.UUID{Bytes: id, Valid: true},
			WorkspaceID:       workspaceID,
			ProjectID:         projectID,
			AgentID:           teamAgentID,
			BaseModel:         baseModel,
			BaseThinkingLevel: baseThinking,
			CreatedBy:         callerID,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Two concurrent first-opens collapse to one row via the
				// partial unique index; the loser reselects under the lock.
				session, err = qtx.GetActiveProjectChatSession(ctx, db.GetActiveProjectChatSessionParams{
					WorkspaceID: workspaceID, ProjectID: projectID,
				})
			}
			if err != nil {
				return nil, fmt.Errorf("create project chat session: %w", err)
			}
		}
		if err := s.adoptLegacyContainerIfEligible(ctx, qtx, session, workspaceID, projectID); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("lookup active project chat session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit session ensure: %w", err)
	}
	return s.buildProjectChatSessionView(ctx, s.Queries, session)
}

// snapshotAgentDefaults converts the agent's current model/thinking_level into
// the base_* snapshot written at session creation. NULL agent values snapshot
// as the empty follow-runtime sentinel.
func snapshotAgentDefaults(agent db.Agent) (pgtype.Text, pgtype.Text) {
	model := agent.Model
	if !model.Valid {
		model = pgtype.Text{String: "", Valid: true}
	}
	thinking := agent.ThinkingLevel
	if !thinking.Valid {
		thinking = pgtype.Text{String: "", Valid: true}
	}
	return model, thinking
}

// adoptLegacyContainerIfEligible implements the §2.1 adoption predicate for
// the session that was JUST inserted (execution point 1): COUNT==1 (no closed
// history, no second session), session.issue_id NULL, and exactly one legacy
// origin_id-NULL container row.
func (s *IssueService) adoptLegacyContainerIfEligible(ctx context.Context, qtx *db.Queries, session db.ProjectChatSession, workspaceID, projectID pgtype.UUID) error {
	count, err := qtx.CountProjectChatSessions(ctx, db.CountProjectChatSessionsParams{
		WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if err != nil {
		return fmt.Errorf("count project chat sessions: %w", err)
	}
	if count != 1 {
		return nil
	}
	legacy, err := qtx.GetLegacyUnboundProjectChatIssue(ctx, db.GetLegacyUnboundProjectChatIssueParams{
		WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if err != nil {
		return fmt.Errorf("load legacy container: %w", err)
	}
	if len(legacy) != 1 {
		return nil
	}
	claimed, err := qtx.AdoptLegacyProjectChatIssue(ctx, db.AdoptLegacyProjectChatIssueParams{
		ID: legacy[0].ID, OriginID: session.ID, WorkspaceID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // concurrently claimed; nothing to adopt
		}
		return fmt.Errorf("adopt legacy container: %w", err)
	}
	if _, err := qtx.BindProjectChatSessionIssue(ctx, db.BindProjectChatSessionIssueParams{
		ID: session.ID, IssueID: claimed.ID,
	}); err != nil {
		return fmt.Errorf("bind adopted container: %w", err)
	}
	return nil
}

// buildProjectChatSessionView resolves the display values (SDD §4.2, no §4.3
// validation — GET is read-only) and renders the view. The Team Agent path can
// never emit agent_default: new-table sessions always snapshot base_*.
func (s *IssueService) buildProjectChatSessionView(ctx context.Context, q *db.Queries, session db.ProjectChatSession) (*ProjectChatSessionView, error) {
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID: session.AgentID, WorkspaceID: session.WorkspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("load team agent for view: %w", err)
	}
	resolved := ResolveChatConfig(
		session.BaseModel, session.ModelOverride, agent.Model,
		session.BaseThinkingLevel, session.ThinkingLevelOverride, agent.ThinkingLevel,
	)
	view := &ProjectChatSessionView{
		SessionID:           util.UUIDToString(session.ID),
		TeamAgentID:         util.UUIDToString(session.AgentID),
		Model:               resolved.Model,
		ModelSource:         resolved.ModelSource,
		ThinkingLevel:       resolved.ThinkingLevel,
		ThinkingLevelSource: resolved.ThinkingLevelSource,
	}
	if session.IssueID.Valid {
		id := util.UUIDToString(session.IssueID)
		view.IssueID = &id
	}
	return view, nil
}

// BindProjectChatContainer binds (or returns the already-bound) container
// issue for an active session, running inside the caller's transaction
// (SDD §4.4). The caller must already hold the project-level advisory
// {prefix}|{ws}|{project} and pass the team_agent_id re-read inside that
// lock — never a pre-lock snapshot. It never commits: the send transaction
// owns the commit (FR-16); the explicit POST container path commits its own
// short transaction around this call.
func (s *IssueService) BindProjectChatContainer(
	ctx context.Context,
	qtx *db.Queries,
	tx pgx.Tx,
	sessionID, workspaceID, projectID, teamAgentID, callerID pgtype.UUID,
) (db.Issue, error) {
	// Session-scoped serialization for container+messages of the SAME session
	// (not the project-level protocol; SDD §4.4).
	sessionKey := strings.Join([]string{"project-chat", util.UUIDToString(workspaceID), util.UUIDToString(sessionID)}, "|")
	if err := qtx.LockIssueDuplicateKey(ctx, sessionKey); err != nil {
		return db.Issue{}, fmt.Errorf("lock session container key: %w", err)
	}

	session, err := qtx.LockProjectChatSessionByID(ctx, db.LockProjectChatSessionByIDParams{
		ID: sessionID, WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, ErrChatSessionNotFound
	}
	if err != nil {
		return db.Issue{}, fmt.Errorf("lock session: %w", err)
	}
	if session.Status != "active" || session.AgentID != teamAgentID {
		return db.Issue{}, ErrChatSessionClosedOrChanged
	}

	if session.IssueID.Valid {
		issue, err := qtx.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{
			ID: session.IssueID, WorkspaceID: workspaceID,
		})
		if err != nil {
			return db.Issue{}, fmt.Errorf("load bound issue: %w", err)
		}
		return issue, nil
	}

	issue, err := qtx.GetIssueByOrigin(ctx, db.GetIssueByOriginParams{
		WorkspaceID: workspaceID, OriginType: pgtype.Text{String: "project_chat", Valid: true}, OriginID: sessionID,
	})
	if err == nil {
		if _, berr := qtx.BindProjectChatSessionIssue(ctx, db.BindProjectChatSessionIssueParams{
			ID: sessionID, IssueID: issue.ID,
		}); berr != nil {
			return db.Issue{}, fmt.Errorf("bind existing origin issue: %w", berr)
		}
		return issue, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("lookup origin issue: %w", err)
	}

	// §2.1 adoption (execution point 2): only the FIRST session row may claim
	// a legacy container; COUNT==1 closes the window after any rebind and
	// plugs the "GET saw legacy=0, forwarding inserted origin_id=NULL, Bind
	// creates a second container" split.
	count, err := qtx.CountProjectChatSessions(ctx, db.CountProjectChatSessionsParams{
		WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("count project chat sessions: %w", err)
	}
	if count == 1 {
		legacy, err := qtx.GetLegacyUnboundProjectChatIssue(ctx, db.GetLegacyUnboundProjectChatIssueParams{
			WorkspaceID: workspaceID, ProjectID: projectID,
		})
		if err != nil {
			return db.Issue{}, fmt.Errorf("load legacy container: %w", err)
		}
		if len(legacy) == 1 {
			claimed, err := qtx.AdoptLegacyProjectChatIssue(ctx, db.AdoptLegacyProjectChatIssueParams{
				ID: legacy[0].ID, OriginID: sessionID, WorkspaceID: workspaceID,
			})
			if err == nil {
				if _, berr := qtx.BindProjectChatSessionIssue(ctx, db.BindProjectChatSessionIssueParams{
					ID: sessionID, IssueID: claimed.ID,
				}); berr != nil {
					return db.Issue{}, fmt.Errorf("bind adopted container: %w", berr)
				}
				return claimed, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return db.Issue{}, fmt.Errorf("adopt legacy container: %w", err)
			}
		}
	}

	// Fresh container bound by origin (origin_id = session.id).
	issue, err = createContainerIssueInTx(ctx, qtx, tx, workspaceID, projectID, callerID,
		"project_chat", projectChatIssueTitle, sessionID)
	if err != nil {
		return db.Issue{}, err
	}
	if _, berr := qtx.BindProjectChatSessionIssue(ctx, db.BindProjectChatSessionIssueParams{
		ID: sessionID, IssueID: issue.ID,
	}); berr != nil {
		return db.Issue{}, fmt.Errorf("bind fresh container: %w", berr)
	}
	return issue, nil
}

// UpdateProjectChatSessionConfig applies the three-state PATCH to the active
// session's overrides (SDD §3.1 / §4.7.1): project advisory -> locked session
// row -> binding CAS -> resolve -> catalog + validation -> write. The caller
// must pass the provider string resolved for the bound agent's runtime.
func (s *IssueService) UpdateProjectChatSessionConfig(
	ctx context.Context,
	workspaceID, projectID, sessionID, callerID pgtype.UUID,
	provider string,
	modelPatch, thinkingPatch ChatConfigFieldPatch,
) (*ProjectChatSessionView, error) {
	if !workspaceID.Valid || !projectID.Valid || !sessionID.Valid {
		return nil, fmt.Errorf("update project chat session config: ids required")
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	if err := qtx.LockIssueDuplicateKey(ctx, projectChatSessionAdvisoryKey(workspaceID, projectID)); err != nil {
		return nil, fmt.Errorf("lock project chat session key: %w", err)
	}
	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID: projectID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("reload project under session lock: %w", err)
	}
	teamAgentID := teamAgentIDFromSettings(project.Settings)
	if !teamAgentID.Valid {
		return nil, ErrTeamAgentNotConfigured
	}

	member, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID: callerID, WorkspaceID: workspaceID,
	})
	if err != nil || !isOwnerOrAdmin(member.Role) {
		return nil, ErrForbiddenChatConfig
	}

	agent, err := qtx.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
		ID: teamAgentID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("load team agent: %w", err)
	}

	session, err := qtx.LockProjectChatSessionByID(ctx, db.LockProjectChatSessionByIDParams{
		ID: sessionID, WorkspaceID: workspaceID, ProjectID: projectID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrChatSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock session: %w", err)
	}
	if session.Status != "active" || session.AgentID != teamAgentID {
		return nil, ErrChatSessionClosedOrChanged
	}

	modelOverride := applyChatConfigFieldPatch(session.ModelOverride, modelPatch)
	thinkingOverride := applyChatConfigFieldPatch(session.ThinkingLevelOverride, thinkingPatch)
	resolved := ResolveChatConfig(
		session.BaseModel, modelOverride, agent.Model,
		session.BaseThinkingLevel, thinkingOverride, agent.ThinkingLevel,
	)

	if s.ChatCatalog == nil {
		return nil, ErrInvalidModelOrThinkingLevel
	}
	catalog, err := LoadChatCatalogForConfig(ctx, qtx, s.ChatCatalog, agent)
	if err != nil {
		return nil, err
	}
	if err := ValidateResolvedChatConfig(resolved.Model, resolved.ThinkingLevel, provider, catalog); err != nil {
		return nil, err
	}

	updated, err := qtx.PatchProjectChatSessionConfig(ctx, db.PatchProjectChatSessionConfigParams{
		ID:                   sessionID,
		WorkspaceID:          workspaceID,
		ModelOverride:        modelOverride,
		ThinkingLevelOverride: thinkingOverride,
	})
	if err != nil {
		return nil, fmt.Errorf("patch session config: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit config patch: %w", err)
	}
	return s.buildProjectChatSessionView(ctx, s.Queries, updated)
}

// applyChatConfigFieldPatch folds the three-state patch into the current
// override column value (SDD FR-6). An empty string never lands in an
// override column: it means "clear".
func applyChatConfigFieldPatch(current pgtype.Text, patch ChatConfigFieldPatch) pgtype.Text {
	if !patch.Present {
		return current
	}
	if patch.Clear {
		return pgtype.Text{}
	}
	return pgtype.Text{String: patch.Value, Valid: true}
}

// createContainerIssueInTx creates the hidden container issue row inside the
// caller's transaction. It never commits (SDD §4.4/§4.5: the caller owns the
// transaction). originID stamps origin_id for session-bound containers; the
// legacy forwarding path passes the zero UUID (NULL).
func createContainerIssueInTx(
	ctx context.Context,
	qtx *db.Queries,
	tx pgx.Tx,
	workspaceID, projectID, callerID pgtype.UUID,
	originType, title string,
	originID pgtype.UUID,
) (db.Issue, error) {
	number, err := qtx.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return db.Issue{}, fmt.Errorf("increment issue counter: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, workspaceID, "todo")
	if err != nil {
		return db.Issue{}, fmt.Errorf("next top position: %w", err)
	}

	issue, err := qtx.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
		WorkspaceID: workspaceID,
		Title:       title,
		Description: pgtype.Text{},
		Status:      "todo",
		Priority:    "medium",
		AssigneeType: pgtype.Text{},
		AssigneeID:   pgtype.UUID{},
		CreatorType:   "member",
		CreatorID:     callerID,
		ParentIssueID: pgtype.UUID{},
		Position:      position,
		StartDate:     pgtype.Date{},
		DueDate:       pgtype.Date{},
		Number:        number,
		ProjectID:     projectID,
		OriginType:    pgtype.Text{String: originType, Valid: true},
		OriginID:      originID,
		Stage:         pgtype.Int4{},
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create %s issue: %w", originType, err)
	}
	return issue, nil
}
