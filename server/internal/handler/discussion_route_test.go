package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discussionRouteFixture wires the full Discussion-to-Team-Agent routing
// topology: a configured project (DC + Team Agent bindings), its Discussion
// container, a human @-mention that activated the coordinator, and the
// coordinator's completion comment (@-mentioning the Team Agent) hanging
// under that activation — the exact shape createAgentComment produces.
type discussionRouteFixture struct {
	DiscussionIssue db.Issue
	ProjectID       string
	CoordinatorID   string
	TeamAgentID     string
	ActivatorID     string // the human who activated the coordinator
	ActivationID    string // human's @DC comment
	RouteCommentID  string // DC completion comment @-mentioning the Team Agent
}

// activatorID selects the activation human: "" uses the workspace owner
// (testUserID), which bypasses the capacity guard — queue-full scenarios
// must pass a plain member instead (same split as the CR-2026-004 capacity
// tests).
func newDiscussionRouteFixture(t *testing.T, queueLimit int, seedQueuedTasks int, activatorID string) discussionRouteFixture {
	t.Helper()
	ctx := context.Background()

	if activatorID == "" {
		activatorID = testUserID
	}

	var coordinatorID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&coordinatorID); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}
	teamAgentID := createHandlerTestAgent(t, "dc route team agent", []byte("{}"))

	settingsJSON := map[string]any{
		service.ProjectSettingDiscussionCoordinatorID: coordinatorID,
		service.ProjectSettingTeamAgentID:             teamAgentID,
	}
	if queueLimit > 0 {
		settingsJSON[service.ProjectSettingTeamAgentQueueLimit] = queueLimit
	}
	raw, err := json.Marshal(settingsJSON)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var projectID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO project (workspace_id, title, settings) VALUES ($1, 'dc route fixture', $2)
		RETURNING id
	`, testWorkspaceID, raw).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE project_id = $1`, projectID)
		testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID)
	})

	insertIssue := func(title string, originType *string) string {
		t.Helper()
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO issue (workspace_id, project_id, creator_type, creator_id, title, origin_type, number)
			VALUES ($1, $2, 'member', $3, $4, $5, $6)
			RETURNING id
		`, testWorkspaceID, projectID, testUserID, title, originType, number).Scan(&id); err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		return id
	}

	discussionOrigin := "project_discussion"
	discussionIssueID := insertIssue("dc route discussion container", &discussionOrigin)

	insertComment := func(issueID, authorType, authorID, content string, parentID *string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type, parent_id)
			VALUES ($1, $2, $3, $4, $5, 'comment', $6)
			RETURNING id
		`, issueID, testWorkspaceID, authorType, authorID, content, parentID).Scan(&id); err != nil {
			t.Fatalf("create comment: %v", err)
		}
		return id
	}

	activationID := insertComment(discussionIssueID, "member", activatorID,
		"[@DC](mention://agent/"+coordinatorID+") please coordinate", nil)
	routeCommentID := insertComment(discussionIssueID, "agent", coordinatorID,
		"[@Team](mention://agent/"+teamAgentID+") please implement the converged plan", &activationID)

	// Seed project-level pending tasks to drive the queue-full scenarios. They
	// hang on an ORDINARY project issue (never the Discussion container with
	// the same agent — that pair would be merged-into-pending instead of
	// re-targeted, hiding the queue-full path under test).
	if seedQueuedTasks > 0 {
		fillerIssueID := insertIssue("dc route queue filler", nil)
		for i := 0; i < seedQueuedTasks; i++ {
			if _, err := testPool.Exec(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority)
				SELECT $1, runtime_id, $2, 'queued', 0 FROM agent WHERE id = $1
			`, teamAgentID, fillerIssueID); err != nil {
				t.Fatalf("seed queued task: %v", err)
			}
		}
	}

	discussionIssue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(discussionIssueID))
	if err != nil {
		t.Fatalf("load discussion issue: %v", err)
	}
	return discussionRouteFixture{
		DiscussionIssue: discussionIssue,
		ProjectID:       projectID,
		CoordinatorID:   coordinatorID,
		TeamAgentID:     teamAgentID,
		ActivatorID:     activatorID,
		ActivationID:    activationID,
		RouteCommentID:  routeCommentID,
	}
}

// runDiscussionRouteEnqueue computes the triggers for the coordinator's
// routing comment (the T02 filter admits exactly the routing class here) and
// hands them to the real enqueue path under test.
func runDiscussionRouteEnqueue(t *testing.T, fx discussionRouteFixture) {
	t.Helper()
	ctx := context.Background()
	content := "[@Team](mention://agent/" + fx.TeamAgentID + ") please implement the converged plan"
	triggers := testHandler.computeCommentAgentTriggers(ctx, fx.DiscussionIssue, content, nil, "agent", fx.CoordinatorID, commentTriggerComputeOptions{})
	if len(triggers) != 1 {
		t.Fatalf("expected exactly one routing trigger, got %d: %+v", len(triggers), triggers)
	}
	testHandler.enqueueCommentAgentTriggers(ctx, fx.DiscussionIssue, util.MustParseUUID(fx.RouteCommentID), triggers)
}

// TestDiscussionRoute_RetargetsToChatContainer pins DD-5/AC-3: a
// coordinator-authored @TeamAgent mention on Discussion does NOT enqueue on
// the Discussion container — it lands a routing comment on the project chat
// container plus a Team Agent task hanging on that chat container, with the
// activation human stamped as the task originator (TSUG-001 explicit
// resolution via the completion comment's parent chain).
func TestDiscussionRoute_RetargetsToChatContainer(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionRouteFixture(t, 0, 0, "")
	ctx := context.Background()

	runDiscussionRouteEnqueue(t, fx)

	// The chat container was lazily created on the same project.
	var chatIssueID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM issue WHERE project_id = $1 AND origin_type = 'project_chat'
	`, fx.ProjectID).Scan(&chatIssueID); err != nil {
		t.Fatalf("expected a lazily created chat container issue: %v", err)
	}

	// No task may hang on the Discussion container.
	var discussionTasks int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1
	`, fx.DiscussionIssue.ID).Scan(&discussionTasks); err != nil {
		t.Fatalf("count discussion tasks: %v", err)
	}
	if discussionTasks != 0 {
		t.Fatalf("expected zero tasks on the Discussion container, got %d", discussionTasks)
	}

	// The routing comment: DC-authored on the chat container, carrying the
	// original mention content.
	var routeCommentContent, routeAuthorType, routeAuthorID string
	var routeCommentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id, author_type, author_id, content FROM comment
		WHERE issue_id = $1 AND author_type = 'agent'
		ORDER BY created_at DESC LIMIT 1
	`, chatIssueID).Scan(&routeCommentID, &routeAuthorType, &routeAuthorID, &routeCommentContent); err != nil {
		t.Fatalf("expected a routing comment on the chat container: %v", err)
	}
	if routeAuthorID != fx.CoordinatorID {
		t.Fatalf("routing comment author = %s, want coordinator %s", routeAuthorID, fx.CoordinatorID)
	}
	if !strings.Contains(routeCommentContent, "mention://agent/"+fx.TeamAgentID) {
		t.Fatalf("routing comment must carry the Team Agent mention, got %q", routeCommentContent)
	}

	// The Team Agent task hangs on the chat container, triggered by the
	// routing comment, with the activation human as originator.
	var taskAgentID, taskIssueID, taskTriggerCommentID string
	var originator *string
	if err := testPool.QueryRow(ctx, `
		SELECT agent_id, issue_id, trigger_comment_id, originator_user_id FROM agent_task_queue
		WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 1
	`, chatIssueID).Scan(&taskAgentID, &taskIssueID, &taskTriggerCommentID, &originator); err != nil {
		t.Fatalf("expected a Team Agent task on the chat container: %v", err)
	}
	if taskAgentID != fx.TeamAgentID {
		t.Fatalf("task agent = %s, want team agent %s", taskAgentID, fx.TeamAgentID)
	}
	if taskTriggerCommentID != routeCommentID {
		t.Fatalf("task trigger comment = %s, want routing comment %s", taskTriggerCommentID, routeCommentID)
	}
	if originator == nil || *originator != fx.ActivatorID {
		got := "<nil>"
		if originator != nil {
			got = *originator
		}
		t.Fatalf("task originator = %s, want activation human %s (TSUG-001 explicit resolution)", got, fx.ActivatorID)
	}
}

// TestDiscussionRoute_QueueFullPostSystemComment pins DD-6 for the routing
// path: when the shared project queue is at capacity, the route must not
// enqueue, must not leave a ghost routing comment on the chat container, and
// must leave a structured, DC-authored system comment on the Discussion
// container (queue N/M) — never a silent log.
func TestDiscussionRoute_QueueFullPostSystemComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// Limit 1 + one seeded pending task on the project → full. A plain member
	// activates: the workspace owner would bypass the capacity guard.
	activatorID := createPlainMemberUser(t, "dc-route-full")
	fx := newDiscussionRouteFixture(t, 1, 1, activatorID)
	ctx := context.Background()

	runDiscussionRouteEnqueue(t, fx)

	// No ghost routing comment / task may exist anywhere on the project.
	var ghostCount int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM comment c JOIN issue i ON i.id = c.issue_id
		WHERE i.project_id = $1 AND i.origin_type = 'project_chat'
	`, fx.ProjectID).Scan(&ghostCount); err != nil {
		t.Fatalf("count chat comments: %v", err)
	}
	if ghostCount != 0 {
		t.Fatalf("expected no ghost routing comment on a full queue, got %d", ghostCount)
	}
	var newTasks int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE project_id = $1)
	`, fx.ProjectID).Scan(&newTasks); err != nil {
		t.Fatalf("count project tasks: %v", err)
	}
	if newTasks != 1 { // only the seeded capacity-filler
		t.Fatalf("expected only the seeded task on a full queue, got %d", newTasks)
	}

	// The auditable notice: a DC-authored system comment on the Discussion
	// container carrying the structured queue depth.
	var noticeContent, noticeType, noticeAuthorID string
	if err := testPool.QueryRow(ctx, `
		SELECT content, type, author_id FROM comment
		WHERE issue_id = $1 AND type = 'system'
		ORDER BY created_at DESC LIMIT 1
	`, fx.DiscussionIssue.ID).Scan(&noticeContent, &noticeType, &noticeAuthorID); err != nil {
		t.Fatalf("expected a system comment on the Discussion container: %v", err)
	}
	if noticeAuthorID != fx.CoordinatorID {
		t.Fatalf("system comment author = %s, want coordinator %s", noticeAuthorID, fx.CoordinatorID)
	}
	if !strings.Contains(noticeContent, "queue is full (1/1)") {
		t.Fatalf("system comment must carry the structured queue depth, got %q", noticeContent)
	}
}

// TestDiscussionActivation_QueueFullPostSystemComment pins DD-6 for the
// activation path: a member's @DC mention on a full queue leaves the same
// auditable system comment instead of a silent enqueue failure.
func TestDiscussionActivation_QueueFullPostSystemComment(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	activatorID := createPlainMemberUser(t, "dc-activation-full")
	fx := newDiscussionRouteFixture(t, 1, 1, activatorID)
	ctx := context.Background()

	activation := "[@DC](mention://agent/" + fx.CoordinatorID + ") please coordinate"
	triggers := testHandler.computeCommentAgentTriggers(ctx, fx.DiscussionIssue, activation, nil, "member", fx.ActivatorID, commentTriggerComputeOptions{})
	if len(triggers) != 1 {
		t.Fatalf("expected exactly one activation trigger, got %d: %+v", len(triggers), triggers)
	}
	testHandler.enqueueCommentAgentTriggers(ctx, fx.DiscussionIssue, util.MustParseUUID(fx.ActivationID), triggers)

	// No DC activation task may have been enqueued.
	var dcTasks int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2
	`, fx.DiscussionIssue.ID, fx.CoordinatorID).Scan(&dcTasks); err != nil {
		t.Fatalf("count dc tasks: %v", err)
	}
	if dcTasks != 0 {
		t.Fatalf("expected no activation task on a full queue, got %d", dcTasks)
	}

	var noticeContent string
	if err := testPool.QueryRow(ctx, `
		SELECT content FROM comment
		WHERE issue_id = $1 AND type = 'system'
		ORDER BY created_at DESC LIMIT 1
	`, fx.DiscussionIssue.ID).Scan(&noticeContent); err != nil {
		t.Fatalf("expected a system comment on the Discussion container: %v", err)
	}
	if !strings.Contains(noticeContent, "queue is full (1/1)") {
		t.Fatalf("system comment must carry the structured queue depth, got %q", noticeContent)
	}
}
