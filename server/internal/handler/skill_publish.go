package handler

// AIFIRST: CR-2026-048 TASK-07: single entry point for the org-publish gate,
// shared by UpdateSkill (SDD §3.1 triggers 1 and 2) and the runtime-local
// overwrite import. One scan, then owner-approved findings are released.

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	skillpkg "github.com/multica-ai/multica/server/internal/skill"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/skillbundle"
)

// runPublishGate evaluates the effective skill content plus every file and
// drops findings an owner approved. contentHash comes from the same
// skillbundle manifest used for package change detection, so any content
// change invalidates previously granted appeals by construction.
func runPublishGate(ctx context.Context, q *db.Queries, wsID pgtype.UUID, skill db.Skill, content, owner string, fileSet map[string]string) skillpkg.GateResult {
	bundleFiles := make([]skillbundle.File, 0, len(fileSet))
	for path, text := range fileSet {
		bundleFiles = append(bundleFiles, skillbundle.File{Path: path, Content: text})
	}
	skillID := uuidToString(skill.ID)
	contentHash := skillbundle.BuildManifest(skillbundle.Skill{
		ID:          skillID,
		Source:      skillbundle.SourceWorkspace,
		Name:        skill.Name,
		Description: skill.Description,
		Content:     content,
		Files:       bundleFiles,
	}).Hash
	return skillpkg.EvaluatePublish(content, fileSet, owner, skillID, contentHash).
		Release(func(appealID string) bool {
			row, err := q.GetAppealDecision(ctx, db.GetAppealDecisionParams{WorkspaceID: wsID, AppealID: appealID})
			// A later rejection supersedes an earlier approval: the query
			// returns the most recent decision of either kind.
			return err == nil && row.Action == appealActionApproved
		})
}
