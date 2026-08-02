package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discussionExclusionFixture creates one project with a Discussion container
// issue (origin_type='project_discussion') plus one ordinary issue in the
// same project, and a comment on the container carrying a unique token — so
// full-text search leakage (SDD §4.1, the buildSearchQuery comment-content
// subquery) can be asserted, not just title-based listing exclusion.
type discussionExclusionFixture struct {
	ProjectID        string
	ContainerIssueID string
	OrdinaryIssueID  string
	SearchToken      string // unique token only present in the container's comment
}

func newDiscussionExclusionFixture(t *testing.T) discussionExclusionFixture {
	t.Helper()
	ctx := context.Background()

	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id
	`, testWorkspaceID, "Discussion Exclusion Test Project").Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	nextNumber := func() int {
		t.Helper()
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		return number
	}

	var containerID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, creator_type, creator_id, title, origin_type, number)
		VALUES ($1, $2, 'member', $3, 'Discussion Exclusion Container', 'project_discussion', $4)
		RETURNING id
	`, testWorkspaceID, projectID, testUserID, nextNumber()).Scan(&containerID); err != nil {
		t.Fatalf("create container issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, containerID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, containerID)
	})

	var ordinaryID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, project_id, creator_type, creator_id, title, number)
		VALUES ($1, $2, 'member', $3, 'Discussion Exclusion Ordinary Issue', $4)
		RETURNING id
	`, testWorkspaceID, projectID, testUserID, nextNumber()).Scan(&ordinaryID); err != nil {
		t.Fatalf("create ordinary issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, ordinaryID)
	})

	searchToken := "zzqxfrobnicate9284"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
		VALUES ($1, $2, 'member', $3, $4)
	`, testWorkspaceID, containerID, testUserID, "discussing "+searchToken+" over here"); err != nil {
		t.Fatalf("create container comment: %v", err)
	}

	return discussionExclusionFixture{
		ProjectID:        projectID,
		ContainerIssueID: containerID,
		OrdinaryIssueID:  ordinaryID,
		SearchToken:      searchToken,
	}
}

// TestDiscussionContainerExcludedFromSqlcQueries covers the five sqlc-generated
// exclusion sites (SDD §4.1): ListIssues, CountIssues, ListOpenIssues
// (issue.sql), CountIssuesByProject and GetProjectIssueStats (project.sql).
// Each must never return/count the Discussion container, while the ordinary
// issue in the same project and workspace must still show up.
func TestDiscussionContainerExcludedFromSqlcQueries(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionExclusionFixture(t)
	ctx := context.Background()
	wsUUID := util.MustParseUUID(testWorkspaceID)

	containsIssue := func(ids []pgtype.UUID, target string) bool {
		for _, id := range ids {
			if util.UUIDToString(id) == target {
				return true
			}
		}
		return false
	}

	t.Run("ListIssues", func(t *testing.T) {
		rows, err := testHandler.Queries.ListIssues(ctx, db.ListIssuesParams{
			WorkspaceID: wsUUID, Limit: 1000, Offset: 0,
		})
		if err != nil {
			t.Fatalf("ListIssues: %v", err)
		}
		ids := make([]pgtype.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		if containsIssue(ids, fx.ContainerIssueID) {
			t.Fatalf("ListIssues returned the Discussion container issue")
		}
		if !containsIssue(ids, fx.OrdinaryIssueID) {
			t.Fatalf("ListIssues did not return the ordinary issue (fixture broken, not just over-exclusion)")
		}
	})

	t.Run("ListOpenIssues", func(t *testing.T) {
		rows, err := testHandler.Queries.ListOpenIssues(ctx, db.ListOpenIssuesParams{WorkspaceID: wsUUID})
		if err != nil {
			t.Fatalf("ListOpenIssues: %v", err)
		}
		ids := make([]pgtype.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		if containsIssue(ids, fx.ContainerIssueID) {
			t.Fatalf("ListOpenIssues returned the Discussion container issue")
		}
		if !containsIssue(ids, fx.OrdinaryIssueID) {
			t.Fatalf("ListOpenIssues did not return the ordinary issue (fixture broken, not just over-exclusion)")
		}
	})

	t.Run("CountIssues excludes the container", func(t *testing.T) {
		total, err := testHandler.Queries.CountIssues(ctx, db.CountIssuesParams{WorkspaceID: wsUUID})
		if err != nil {
			t.Fatalf("CountIssues: %v", err)
		}
		// The container must not inflate the count relative to a direct
		// count of non-container issues in the workspace.
		var expected int64
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM issue
			WHERE workspace_id = $1
			  AND origin_type IS DISTINCT FROM 'project_chat'
			  AND origin_type IS DISTINCT FROM 'project_discussion'
		`, testWorkspaceID).Scan(&expected); err != nil {
			t.Fatalf("reference count: %v", err)
		}
		if total != expected {
			t.Fatalf("CountIssues = %d, want %d (matching the exclusion predicate)", total, expected)
		}
	})

	t.Run("CountIssuesByProject excludes the container", func(t *testing.T) {
		count, err := testHandler.Queries.CountIssuesByProject(ctx, util.MustParseUUID(fx.ProjectID))
		if err != nil {
			t.Fatalf("CountIssuesByProject: %v", err)
		}
		if count != 1 {
			t.Fatalf("CountIssuesByProject = %d, want 1 (only the ordinary issue)", count)
		}
	})

	t.Run("GetProjectIssueStats excludes the container", func(t *testing.T) {
		rows, err := testHandler.Queries.GetProjectIssueStats(ctx, []pgtype.UUID{util.MustParseUUID(fx.ProjectID)})
		if err != nil {
			t.Fatalf("GetProjectIssueStats: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("GetProjectIssueStats returned %d rows, want 1", len(rows))
		}
		if rows[0].TotalCount != 1 {
			t.Fatalf("GetProjectIssueStats total_count = %d, want 1 (only the ordinary issue)", rows[0].TotalCount)
		}
	})
}

// TestDiscussionContainerExcludedFromHTTPListingSurfaces covers the two
// hand-written-SQL handler methods (SDD §4.1: issue.go ListIssues/
// ListGroupedIssues board/list/swimlane/gantt/my-issues surfaces) plus the
// full-text search endpoint — the one SDD flagged as safety-critical, since
// buildSearchQuery's comment-content subquery would otherwise leak Discussion
// message bodies into global issue search results.
func TestDiscussionContainerExcludedFromHTTPListingSurfaces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionExclusionFixture(t)

	t.Run("GET /api/issues", func(t *testing.T) {
		req := newRequest("GET", "/api/issues?workspace_id="+testWorkspaceID, nil)
		rr := httptest.NewRecorder()
		testHandler.ListIssues(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Issues []struct {
				ID string `json:"id"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		found := map[string]bool{}
		for _, i := range resp.Issues {
			found[i.ID] = true
		}
		if found[fx.ContainerIssueID] {
			t.Fatalf("GET /api/issues returned the Discussion container issue")
		}
		if !found[fx.OrdinaryIssueID] {
			t.Fatalf("GET /api/issues did not return the ordinary issue (fixture broken, not just over-exclusion)")
		}
	})

	t.Run("GET /api/issues/grouped", func(t *testing.T) {
		req := newRequest("GET", "/api/issues/grouped?workspace_id="+testWorkspaceID, nil)
		rr := httptest.NewRecorder()
		testHandler.ListGroupedIssues(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Groups []struct {
				Issues []struct {
					ID string `json:"id"`
				} `json:"issues"`
			} `json:"groups"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, g := range resp.Groups {
			for _, i := range g.Issues {
				if i.ID == fx.ContainerIssueID {
					t.Fatalf("GET /api/issues/grouped returned the Discussion container issue")
				}
			}
		}
	})

	t.Run("GET /api/issues/search does not leak Discussion comment content", func(t *testing.T) {
		req := newRequest("GET", "/api/issues/search?workspace_id="+testWorkspaceID+"&q="+fx.SearchToken, nil)
		rr := httptest.NewRecorder()
		testHandler.SearchIssues(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Issues []struct {
				ID string `json:"id"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		for _, i := range resp.Issues {
			if i.ID == fx.ContainerIssueID {
				t.Fatalf("search for a token that only exists in a Discussion comment leaked the container issue into global search results")
			}
		}
	})
}
