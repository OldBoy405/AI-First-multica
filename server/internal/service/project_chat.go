package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueposition"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// projectChatIssueTitle is the fixed title of every project's hidden Team Agent
// chat container issue. Never shown in any issue-listing surface (all of them
// exclude origin_type='project_chat'), so it only needs to be stable, not nice.
const projectChatIssueTitle = "Team Agent Chat"

// EnsureProjectChatIssue returns the hidden container issue that anchors a
// project's Team Agent group chat (CR-2026-006), creating it on first use.
//
// The message stream reuses the existing comment/timeline/websocket stack by
// hanging every chat message off this one issue. It is stamped
// origin_type='project_chat' so all issue-listing queries filter it out.
//
// Concurrency: the fast path is a plain read. On a miss we open a tx, take an
// advisory lock keyed on (workspace, project) to serialize concurrent
// first-opens, re-check inside the lock, and only then create. The partial
// unique index issue_project_chat_unique is the belt-and-suspenders backstop
// if two creators ever slip past the lock.
func (s *IssueService) EnsureProjectChatIssue(ctx context.Context, workspaceID, projectID, callerID pgtype.UUID) (db.Issue, error) {
	if !workspaceID.Valid || !projectID.Valid {
		return db.Issue{}, fmt.Errorf("ensure project chat issue: workspace and project required")
	}

	existing, err := s.Queries.GetProjectChatIssue(ctx, db.GetProjectChatIssueParams{
		ProjectID:   projectID,
		WorkspaceID: workspaceID,
	})
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("lookup project chat issue: %w", err)
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.Issue{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Serialize concurrent first-opens on the same project. Released on commit.
	lockKey := strings.Join([]string{"project-chat", util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
	if err := qtx.LockIssueDuplicateKey(ctx, lockKey); err != nil {
		return db.Issue{}, fmt.Errorf("lock project chat key: %w", err)
	}

	// Re-check under the lock — another opener may have created it while we
	// waited on the advisory lock.
	if existing, err := qtx.GetProjectChatIssue(ctx, db.GetProjectChatIssueParams{
		ProjectID:   projectID,
		WorkspaceID: workspaceID,
	}); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, fmt.Errorf("recheck project chat issue: %w", err)
	}

	number, err := qtx.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return db.Issue{}, fmt.Errorf("increment issue counter: %w", err)
	}
	position, err := issueposition.NextTopPosition(ctx, tx, workspaceID, "todo")
	if err != nil {
		return db.Issue{}, fmt.Errorf("next top position: %w", err)
	}

	issue, err := qtx.CreateIssueWithOrigin(ctx, db.CreateIssueWithOriginParams{
		WorkspaceID:  workspaceID,
		Title:        projectChatIssueTitle,
		Description:  pgtype.Text{},
		Status:       "todo",
		Priority:     "none",
		AssigneeType: pgtype.Text{},
		AssigneeID:   pgtype.UUID{},
		// The container has no meaningful author; attribute it to the member
		// who first opened the chat. It is never surfaced, so this only
		// affects the (also hidden) issue's own creator record.
		CreatorType:   "member",
		CreatorID:     callerID,
		ParentIssueID: pgtype.UUID{},
		Position:      position,
		StartDate:     pgtype.Date{},
		DueDate:       pgtype.Date{},
		Number:        number,
		ProjectID:     projectID,
		OriginType:    pgtype.Text{String: "project_chat", Valid: true},
		OriginID:      pgtype.UUID{},
		Stage:         pgtype.Int4{},
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create project chat issue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Issue{}, fmt.Errorf("commit project chat issue: %w", err)
	}
	return issue, nil
}
