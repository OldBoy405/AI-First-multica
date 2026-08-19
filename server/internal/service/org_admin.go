package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Org Admin workspace bootstrap (SDD §4.6): one idempotent project, one
// system agent, one Autopilot with a weekly schedule trigger — all keyed on
// stable identities (project.settings system_key, agent system_key,
// autopilot title+project), never on editable display names.

const (
	orgAdminSystemKey    = "org-admin-workspace"
	orgAdminAgentKey     = "org-admin"
	orgAdminAutopilotTitle = "AI Maturity Weekly Report"
)

// OrgAdminRef is the stable id set returned by EnsureOrgAdminWorkspace.
type OrgAdminRef struct {
	ProjectID   pgtype.UUID
	AgentID     pgtype.UUID
	AutopilotID pgtype.UUID
	TriggerID   pgtype.UUID
}

// EnsureOrgAdminWorkspace finds-or-creates the Org Admin project, agent,
// autopilot and weekly schedule trigger under one transaction guarded by the
// per-workspace advisory lock. Repeated calls insert zero rows.
func EnsureOrgAdminWorkspace(
	ctx context.Context,
	queries *db.Queries,
	txStarter TxStarter,
	workspaceID, ownerID, runtimeID pgtype.UUID,
) (*OrgAdminRef, error) {
	if !workspaceID.Valid || !runtimeID.Valid {
		return nil, errors.New("org admin: workspace and runtime ids are required")
	}
	tx, err := txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var locked bool
	if err := tx.QueryRow(ctx,
		"SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))",
		"org-admin:"+uuid.UUID(workspaceID.Bytes).String(),
	).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("org admin: bootstrap lock busy")
	}
	qtx := db.New(tx)

	// Project: logical idempotency key in settings.system_key.
	project, err := findOrCreateOrgAdminProject(ctx, qtx, workspaceID)
	if err != nil {
		return nil, err
	}

	// Agent: stable system_key; re-check inside the lock before creating.
	agent, err := qtx.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{
		WorkspaceID: workspaceID, SystemKey: pgtype.Text{String: orgAdminAgentKey, Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		created, err := qtx.CreateSystemUserAgent(ctx, db.CreateSystemUserAgentParams{
			WorkspaceID:        workspaceID,
			Name:               "Org Admin",
			Description:        "System agent producing the weekly AI maturity report.",
			AvatarUrl:          pgtype.Text{},
			RuntimeMode:        "local",
			RuntimeID:          runtimeID,
			Model:              pgtype.Text{},
			Visibility:         "workspace",
			PermissionMode:     "public_to",
			MaxConcurrentTasks: 1,
			OwnerID:            ownerID,
			SystemKey:          pgtype.Text{String: orgAdminAgentKey, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("create org admin agent: %w", err)
		}
		agent = created
	}

	// Autopilot + weekly schedule trigger.
	autopilot, err := qtx.MaturityOrgAdminAutopilot(ctx, db.MaturityOrgAdminAutopilotParams{
		WorkspaceID: workspaceID, ProjectID: project.ID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		created, err := qtx.CreateAutopilot(ctx, db.CreateAutopilotParams{
			WorkspaceID:        workspaceID,
			Title:              orgAdminAutopilotTitle,
			Description:        pgtype.Text{String: "Weekly AI maturity report for the workspace.", Valid: true},
			AssigneeType:       "agent",
			AssigneeID:         agent.ID,
			Status:             "active",
			ExecutionMode:      "run_only",
			IssueTitleTemplate: pgtype.Text{},
			ProjectID:          project.ID,
			CreatedByType:      "member",
			CreatedByID:        ownerID,
		})
		if err != nil {
			return nil, fmt.Errorf("create org admin autopilot: %w", err)
		}
		autopilot = created
	}

	trigger, err := qtx.MaturityOrgAdminScheduleTrigger(ctx, autopilot.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		created, err := qtx.CreateAutopilotTrigger(ctx, db.CreateAutopilotTriggerParams{
			AutopilotID:     autopilot.ID,
			Kind:            "schedule",
			Enabled:         true,
			CronExpression:  pgtype.Text{String: "0 9 * * 1", Valid: true},
			Timezone:        pgtype.Text{String: "Asia/Shanghai", Valid: true},
			Label:           pgtype.Text{String: "Weekly maturity report", Valid: true},
			PublishedByType: pgtype.Text{},
			PublishedByID:   pgtype.UUID{},
		})
		if err != nil {
			return nil, fmt.Errorf("create org admin schedule trigger: %w", err)
		}
		trigger = created
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &OrgAdminRef{
		ProjectID: project.ID, AgentID: agent.ID,
		AutopilotID: autopilot.ID, TriggerID: trigger.ID,
	}, nil
}

func findOrCreateOrgAdminProject(ctx context.Context, qtx *db.Queries, workspaceID pgtype.UUID) (db.Project, error) {
	projects, err := qtx.ListProjects(ctx, db.ListProjectsParams{WorkspaceID: workspaceID})
	if err != nil {
		return db.Project{}, err
	}
	for _, p := range projects {
		var settings map[string]any
		if len(p.Settings) > 0 {
			_ = json.Unmarshal(p.Settings, &settings)
		}
		if settings["system_key"] == orgAdminSystemKey {
			return p, nil
		}
	}
	created, err := qtx.CreateProject(ctx, db.CreateProjectParams{
		WorkspaceID: workspaceID,
		Title:       "Org Admin",
		Description: pgtype.Text{String: "System project hosting the weekly AI maturity reports.", Valid: true},
		Icon:        pgtype.Text{},
		Status:      "in_progress",
		LeadType:    pgtype.Text{String: "agent", Valid: true},
		LeadID:      pgtype.UUID{},
		Priority:    "none",
		StartDate:   pgtype.Date{},
		DueDate:     pgtype.Date{},
	})
	if err != nil {
		return db.Project{}, fmt.Errorf("create org admin project: %w", err)
	}
	// Stamp the logical idempotency key.
	patch, _ := json.Marshal(map[string]any{"system_key": orgAdminSystemKey})
	updated, err := qtx.UpdateProjectSettings(ctx, db.UpdateProjectSettingsParams{
		ID: created.ID, WorkspaceID: workspaceID, Patch: patch,
	})
	if err != nil {
		return db.Project{}, fmt.Errorf("stamp org admin system key: %w", err)
	}
	return updated, nil
}
