package handler

// AIFIRST: CR-2026-048 TASK-08: market read endpoint. One call returns the
// org-visible workspace skills plus builtins, each with its deduplicated
// completed-task usage count. Workspace identity comes exclusively from the
// authenticated context (hard invariant 1).

import (
	"net/http"

	skillpkg "github.com/multica-ai/multica/server/internal/skill"
)

// MarketSkill is one workspace skill in the market list.
type MarketSkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	OwnerActor  string `json:"owner_actor"`
	UsageCount  int64  `json:"usage_count"`
	// Source is the frontmatter `source` marker (e.g. "session-export"),
	// parsed server-side so the list filter uses the same parse as the detail
	// page (FR-23). Empty when the skill declares none.
	Source string `json:"source"`
}

// MarketBuiltin is one builtin skill in the market list. Builtins have no
// skill rows; their identity is the synthesized "builtin:<name>" ref.
type MarketBuiltin struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	UsageCount  int64  `json:"usage_count"`
}

// SkillMarketResponse is the GET /api/skills/market payload.
type SkillMarketResponse struct {
	Workspace []MarketSkill   `json:"workspace"`
	Builtin   []MarketBuiltin `json:"builtin"`
}

// GetSkillMarket serves the market list for the authenticated workspace.
func (h *Handler) GetSkillMarket(w http.ResponseWriter, r *http.Request) {
	wsID := h.resolveWorkspaceID(r)
	if _, ok := h.requireWorkspaceRole(w, r, wsID, "workspace not found", "owner", "admin", "member"); !ok {
		return
	}
	wsUUID := parseUUID(wsID)

	skills, err := h.Queries.ListOrgSkillSummariesByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list skills: "+err.Error())
		return
	}
	usageRows, err := h.Queries.MarketSkillUsage(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load skill usage: "+err.Error())
		return
	}
	usage := make(map[string]int64, len(usageRows))
	for _, row := range usageRows {
		usage[row.SkillRef] = row.UsageCount
	}

	out := SkillMarketResponse{
		Workspace: make([]MarketSkill, 0, len(skills)),
		Builtin:   make([]MarketBuiltin, 0),
	}
	for _, s := range skills {
		out.Workspace = append(out.Workspace, MarketSkill{
			ID:          uuidToString(s.ID),
			Name:        s.Name,
			Description: s.Description,
			Version:     s.Version,
			OwnerActor:  s.OwnerActor.String,
			UsageCount:  usage[uuidToString(s.ID)],
			Source:      skillpkg.ParseSkillMetadata(s.Content).Fields["source"],
		})
	}
	for _, b := range h.TaskService.BuiltinSkills() {
		out.Builtin = append(out.Builtin, MarketBuiltin{
			Name:        b.Name,
			Description: b.Description,
			UsageCount:  usage["builtin:"+b.Name],
		})
	}
	writeJSON(w, http.StatusOK, out)
}
