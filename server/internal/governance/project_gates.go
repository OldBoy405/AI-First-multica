// AIFIRST: project-scoped gates endpoint (CR-2026-011 TASK-04, SDD §4.3/DD-2).
//
// The single data entry point the project chat window's gate UI reads: 16-
// state CR badge + pending approval card + gate-node history, all in one
// round trip. Mounted inside the EXISTING /api/projects/{id} route group
// (router.go), which already wraps its routes in
// middleware.RequireWorkspaceMember — this file reuses that, it does not
// define a new project-membership check (SDD TSUG-003).
package governance

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/middleware"
)

// pendingApprovalStage maps a CR status to the approvalStages key it is
// currently blocking on (gates.json#approvalStages[*].expect), or "" if the
// CR is not sitting at an approval gate right now.
func pendingApprovalStage(status string) string {
	switch status {
	case "requirement-reviewing":
		return "requirement"
	case "tech-design-review-pending":
		return "tech-design"
	case "task-breakdown":
		return "dev-start"
	case "code-reviewing":
		return "code"
	default:
		return ""
	}
}

// canApprove decides whether userID may approve/reject at the given stage for
// a CR in workspaceID.
//
// ponytail: this checks workspace owner/admin only. SDD DD-5 additionally
// wanted "cr.owners.{requirement|development}.id" to grant approval rights —
// but that field is a free-text string self-reported by crctl's --caller flag
// (P1 §A.2), and there is no existing bridge from it to a Multica user
// account (HandleApprove's only check today, before this task, was
// requireHumanActor — no role check at all). Inventing a name-matching
// heuristic here would create false confidence without solving the real
// problem. Ceiling: owners recorded in git-side cr.owners cannot approve
// unless they also hold owner/admin in this workspace. Upgrade path: define
// a real identity bridge (e.g. crctl --caller carrying a Multica user id,
// or an explicit owners-to-members mapping step at CR registration), then
// extend this function to also check cr.owners[roleForStage(stage)].id
// against the resolved member's identity.
func canApprove(ctx context.Context, pool *pgxpool.Pool, workspaceID, userID string) (bool, error) {
	var role string
	err := pool.QueryRow(ctx, `
		SELECT role FROM member WHERE workspace_id = $1::uuid AND user_id = $2::uuid`,
		workspaceID, userID).Scan(&role)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	return role == "owner" || role == "admin", nil
}

type gateNodeView struct {
	NodeID      string  `json:"node_id"`
	Kind        string  `json:"kind"`
	Seq         int     `json:"seq"`
	Status      string  `json:"status"`
	Attempt     int     `json:"attempt"`
	Detail      []byte  `json:"detail,omitempty"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
}

type projectGateCR struct {
	CRID           string            `json:"cr_id"`
	Title          string            `json:"title"`
	Status         string            `json:"status"`
	NeedsReconcile bool              `json:"needs_reconcile"`
	UpdatedAt      string            `json:"updated_at"`
	PendingStage   string            `json:"pending_stage,omitempty"`
	CanApprove     bool              `json:"can_approve"`
	Evidence       map[string]string `json:"evidence,omitempty"`
	EvidenceDigest string            `json:"evidence_digest,omitempty"`
	KeyID          string            `json:"key_id,omitempty"`
	PendingAdvance bool              `json:"pending_advance"`
	GateNodes      []gateNodeView    `json:"gate_nodes"`
}

// HandleProjectGates is GET /api/projects/{id}/gates.
func (a *ApprovalService) HandleProjectGates(w http.ResponseWriter, r *http.Request) {
	workspaceID := middleware.WorkspaceIDFromContext(r.Context())
	projectID := chi.URLParam(r, "id")
	userID := r.Header.Get("X-User-ID")

	var projectWorkspaceID string
	if err := a.pool.QueryRow(r.Context(),
		`SELECT workspace_id::text FROM project WHERE id = $1::uuid`, projectID).Scan(&projectWorkspaceID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}
	if projectWorkspaceID != workspaceID {
		// Same 404 as "not found" — do not reveal the project exists in a
		// different workspace.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
		return
	}

	// SDD TSUG-003: cr.shell_issue_id is nullable for CRs registered before
	// the projection link existed; those rows fall out of this join by
	// design (they have no way to associate with a project) — not a bug.
	rows, err := a.pool.Query(r.Context(), `
		SELECT cr.cr_id, cr.title, cr.status, cr.needs_reconcile, cr.updated_at::text
		FROM cr
		JOIN issue ON issue.id = cr.shell_issue_id
		WHERE issue.project_id = $1::uuid
		  AND cr.status NOT IN ('archived', 'rejected', 'withdrawn')
		ORDER BY cr.updated_at DESC`, projectID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	type crBase struct {
		CRID           string
		Title          string
		Status         string
		NeedsReconcile bool
		UpdatedAt      string
	}
	var bases []crBase
	for rows.Next() {
		var c crBase
		if err := rows.Scan(&c.CRID, &c.Title, &c.Status, &c.NeedsReconcile, &c.UpdatedAt); err != nil {
			rows.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "scan failed"})
			return
		}
		bases = append(bases, c)
	}
	rows.Close()

	result := make([]projectGateCR, 0, len(bases))
	for _, b := range bases {
		view := projectGateCR{
			CRID: b.CRID, Title: b.Title, Status: b.Status,
			NeedsReconcile: b.NeedsReconcile, UpdatedAt: b.UpdatedAt,
		}
		stage := pendingApprovalStage(b.Status)
		view.PendingStage = stage

		if stage != "" {
			if userID != "" {
				if ok, err := canApprove(r.Context(), a.pool, workspaceID, userID); err == nil {
					view.CanApprove = ok
				}
			}
			evidence, err := a.latestEvidence(r, b.CRID)
			if err == nil {
				view.Evidence = evidence
				view.EvidenceDigest = CanonicalDigestFromEvidence(evidence)
				view.KeyID = a.keyID
			}
			var approvedCount int
			if err := a.pool.QueryRow(r.Context(), `
				SELECT count(*) FROM approval_record
				WHERE cr_id = $1 AND stage = $2 AND decision = 'approve' AND evidence_digest = $3`,
				b.CRID, stage, view.EvidenceDigest).Scan(&approvedCount); err == nil {
				view.PendingAdvance = approvedCount > 0
			}
		}

		nodeRows, err := a.pool.Query(r.Context(), `
			SELECT pnr.node_id::text, pnr.kind, pnr.seq, pnr.status, pnr.attempt, pnr.detail,
			       pnr.started_at::text, pnr.completed_at::text
			FROM pipeline_node_run pnr
			JOIN pipeline_run pr ON pr.id = pnr.run_id
			WHERE pr.cr_id = $1
			ORDER BY pnr.seq, pnr.attempt`, b.CRID)
		if err == nil {
			for nodeRows.Next() {
				var n gateNodeView
				var startedAt, completedAt *string
				if err := nodeRows.Scan(&n.NodeID, &n.Kind, &n.Seq, &n.Status, &n.Attempt, &n.Detail, &startedAt, &completedAt); err == nil {
					n.StartedAt, n.CompletedAt = startedAt, completedAt
					view.GateNodes = append(view.GateNodes, n)
				}
			}
			nodeRows.Close()
		}
		result = append(result, view)
	}

	writeJSON(w, http.StatusOK, map[string]any{"crs": result})
}
