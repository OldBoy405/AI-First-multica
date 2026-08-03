package service

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
)

// ProjectSettingDiscussionCoordinatorID is the project.settings key holding
// the agent bound as the project's Discussion Coordinator (CR-2026-012).
// Mentioning this agent in the project Discussion container activates it;
// an unset key keeps the CR-2026-009 red line (no agent on Discussion).
// Stored as the agent UUID string, mirroring ProjectSettingTeamAgentID.
const ProjectSettingDiscussionCoordinatorID = "discussion_coordinator_agent_id"

// ProjectDiscussionCoordinatorID resolves the agent bound as the project's
// Discussion Coordinator. A zero-value UUID means "unconfigured" — the
// Discussion container then stays agent-free (CR-2026-009 red line). The
// read is fail-safe in every branch: a missing project, malformed settings
// bag, or invalid UUID string all degrade to "unconfigured" rather than
// erroring, so a broken setting can never wedge the comment-trigger path.
func (s *TaskService) ProjectDiscussionCoordinatorID(ctx context.Context, projectID pgtype.UUID) pgtype.UUID {
	coordinatorID, _ := s.ProjectDiscussionAgentIDs(ctx, projectID)
	return coordinatorID
}

// ProjectDiscussionAgentIDs resolves the Discussion Coordinator binding and
// the Team Agent binding from ONE project.settings read — the comment-trigger
// hot path needs both (activation filter + routing target, SDD §8) and the
// settings bag is a single JSONB column. Both values follow the same
// fail-safe contract as ProjectDiscussionCoordinatorID: zero UUID =
// unconfigured.
func (s *TaskService) ProjectDiscussionAgentIDs(ctx context.Context, projectID pgtype.UUID) (coordinatorID, teamAgentID pgtype.UUID) {
	if !projectID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	raw, err := s.Queries.GetProjectSettings(ctx, projectID)
	if err != nil || len(raw) == 0 {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}
	}
	return uuidSettingFromBag(settings, ProjectSettingDiscussionCoordinatorID),
		uuidSettingFromBag(settings, ProjectSettingTeamAgentID)
}

// uuidSettingFromBag pulls one agent-UUID setting out of an already-parsed
// settings bag. Missing key, wrong JSON type, or invalid UUID string all
// degrade to the zero UUID.
func uuidSettingFromBag(settings map[string]any, key string) pgtype.UUID {
	v, ok := settings[key].(string)
	if !ok {
		return pgtype.UUID{}
	}
	id, err := util.ParseUUID(v)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}

// discussionCoordinatorIDFromSettings parses the coordinator binding out of
// the raw project.settings JSONB bag. Kept separate from the DB read so the
// parsing branches are unit-testable without a pool (same shape as the
// handler-side projectTeamAgentID tests).
func discussionCoordinatorIDFromSettings(raw []byte) pgtype.UUID {
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return pgtype.UUID{}
	}
	v, ok := settings[ProjectSettingDiscussionCoordinatorID].(string)
	if !ok {
		return pgtype.UUID{}
	}
	id, err := util.ParseUUID(v)
	if err != nil {
		return pgtype.UUID{}
	}
	return id
}
