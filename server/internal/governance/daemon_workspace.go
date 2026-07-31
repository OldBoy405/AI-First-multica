// AIFIRST: daemon workspace binding resolution (CR-2026-002 TASK-11 defect
// fix, folded into the TASK-05 surface).
//
// The original design trusted only the mdt_ daemon-token binding — but the
// upstream mdt_ minting flow is not live yet (auth.GenerateDaemonToken has no
// callers), so real daemons authenticate through the PAT fallback and every
// governance call was rejected 403. Upstream's own convention for that path
// (see middleware/daemon_auth.go) is "downstream daemon handlers check
// workspace membership the usual way": DaemonAuth stamps the server-set
// X-User-ID header, and the handler gates on the member table.
//
// Trust model: mdt_ binding first (authoritative); PAT fallback binds to a
// workspace the PAT's user is a member of — X-Workspace-ID selects it when
// the user belongs to several, membership always verified server-side. The
// request body remains untrusted either way.
package governance

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/middleware"
)

// resolveDaemonWorkspace returns (workspaceID, "") on success or ("", reason)
// when the caller must reject with 403.
func resolveDaemonWorkspace(r *http.Request, pool *pgxpool.Pool) (string, string) {
	if wid := middleware.DaemonWorkspaceIDFromContext(r.Context()); wid != "" {
		return wid, ""
	}
	userID := r.Header.Get("X-User-ID") // server-set by DaemonAuth PAT path
	if userID == "" {
		return "", "daemon token with workspace binding required"
	}
	if requested := r.Header.Get("X-Workspace-ID"); requested != "" {
		var ok bool
		err := pool.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM member WHERE workspace_id = $1::uuid AND user_id = $2::uuid)`,
			requested, userID).Scan(&ok)
		if err != nil || !ok {
			return "", "not a member of the requested workspace"
		}
		return requested, ""
	}
	// No explicit selection: unambiguous only when the user has exactly one
	// membership (the single-org deployment normal case).
	var n int
	var wid *string
	err := pool.QueryRow(r.Context(), `
		SELECT count(*), min(workspace_id::text) FROM member WHERE user_id = $1::uuid`,
		userID).Scan(&n, &wid)
	if err != nil || n == 0 || wid == nil {
		return "", "no workspace membership for this token"
	}
	if n > 1 {
		return "", "multiple workspaces; send X-Workspace-ID"
	}
	return *wid, ""
}
