package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// discussionTriggerFixture wires one Discussion container issue
// (origin_type='project_discussion') plus a squad-assigned Discussion
// container issue, both anchored on the seeded workspace agent/squad, so the
// four computeCommentAgentTriggers branches that could otherwise enqueue an
// agent (explicit @agent mention, explicit @squad mention, parent-author
// reply continuation, and the assigned-squad-leader fallback for
// agent-authored comments) can each be exercised against a Discussion
// container and asserted empty (CR-2026-009 red line).
type discussionTriggerFixture struct {
	DiscussionIssue      db.Issue // origin_type='project_discussion', unassigned
	SquadDiscussionIssue db.Issue // origin_type='project_discussion', assignee_type='squad'
	OrdinaryIssue        db.Issue // origin_type NULL — regression control
	AgentID              string
	SquadID              string
	LeaderID             string
}

func newDiscussionTriggerFixture(t *testing.T) discussionTriggerFixture {
	t.Helper()
	ctx := context.Background()

	var agentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id FROM agent WHERE workspace_id = $1 ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load seeded agent: %v", err)
	}

	var squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, $2, '', $3, $4)
		RETURNING id
	`, testWorkspaceID, "Discussion Trigger Exemption Squad", agentID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	// originType is a *string, not string: the column has a CHECK constraint
	// on a fixed value list, so the "no origin" regression-control issue must
	// insert SQL NULL, not an empty string (which the constraint rejects).
	insertIssue := func(title string, originType *string, assigneeType, assigneeID string) string {
		t.Helper()
		// Pick the next per-workspace issue number; without it every insert
		// lands on the default number=0 and trips uq_issue_workspace_number
		// (see newSelfMentionFixture for the same pattern).
		var number int
		if err := testPool.QueryRow(ctx, `
			UPDATE workspace
			SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
			WHERE id = $1 RETURNING issue_counter
		`, testWorkspaceID).Scan(&number); err != nil {
			t.Fatalf("next issue number: %v", err)
		}

		var id string
		var err error
		if assigneeType == "" {
			err = testPool.QueryRow(ctx, `
				INSERT INTO issue (workspace_id, creator_type, creator_id, title, origin_type, number)
				VALUES ($1, 'member', $2, $3, $4, $5)
				RETURNING id
			`, testWorkspaceID, testUserID, title, originType, number).Scan(&id)
		} else {
			err = testPool.QueryRow(ctx, `
				INSERT INTO issue (workspace_id, creator_type, creator_id, title, origin_type, assignee_type, assignee_id, number)
				VALUES ($1, 'member', $2, $3, $4, $5, $6, $7)
				RETURNING id
			`, testWorkspaceID, testUserID, title, originType, assigneeType, assigneeID, number).Scan(&id)
		}
		if err != nil {
			t.Fatalf("create issue %q: %v", title, err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, id)
			testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id)
		})
		return id
	}

	discussionOrigin := "project_discussion"
	discussionID := insertIssue("discussion trigger exemption (unassigned)", &discussionOrigin, "", "")
	squadDiscussionID := insertIssue("discussion trigger exemption (squad-assigned)", &discussionOrigin, "squad", squadID)
	ordinaryID := insertIssue("discussion trigger exemption regression control", nil, "", "")

	loadIssue := func(id string) db.Issue {
		t.Helper()
		issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(id))
		if err != nil {
			t.Fatalf("load issue %s: %v", id, err)
		}
		return issue
	}

	return discussionTriggerFixture{
		DiscussionIssue:      loadIssue(discussionID),
		SquadDiscussionIssue: loadIssue(squadDiscussionID),
		OrdinaryIssue:        loadIssue(ordinaryID),
		AgentID:              agentID,
		SquadID:              squadID,
		LeaderID:             agentID,
	}
}

// TestComputeCommentAgentTriggers_DiscussionContainerNeverEnqueuesAgent pins
// the CR-2026-009 red line: a comment on a Discussion container issue must
// never produce an agent trigger, regardless of which of the four routing
// branches would otherwise fire. The short-circuit lives at the very top of
// computeCommentAgentTriggers (comment.go), so this also covers a comment
// created directly against the API rather than through the Discussion tab's
// own send path — there is no separate code path to miss.
func TestComputeCommentAgentTriggers_DiscussionContainerNeverEnqueuesAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newDiscussionTriggerFixture(t)
	ctx := context.Background()

	agentMention := "[@Agent](mention://agent/" + fx.AgentID + ") please handle this"
	squadMention := "[@Squad](mention://squad/" + fx.SquadID + ") please handle this"

	t.Run("explicit @agent mention", func(t *testing.T) {
		got := testHandler.computeCommentAgentTriggers(ctx, fx.DiscussionIssue, agentMention, nil, "member", testUserID, commentTriggerComputeOptions{})
		if len(got) != 0 {
			t.Fatalf("expected no triggers on a Discussion container, got %d: %+v", len(got), got)
		}
	})

	t.Run("explicit @squad mention", func(t *testing.T) {
		got := testHandler.computeCommentAgentTriggers(ctx, fx.DiscussionIssue, squadMention, nil, "member", testUserID, commentTriggerComputeOptions{})
		if len(got) != 0 {
			t.Fatalf("expected no triggers on a Discussion container, got %d: %+v", len(got), got)
		}
	})

	t.Run("reply to an agent-authored parent comment", func(t *testing.T) {
		parent := &db.Comment{AuthorType: "agent"}
		got := testHandler.computeCommentAgentTriggers(ctx, fx.DiscussionIssue, "thanks, following up", parent, "member", testUserID, commentTriggerComputeOptions{})
		if len(got) != 0 {
			t.Fatalf("expected no triggers on a Discussion container, got %d: %+v", len(got), got)
		}
	})

	t.Run("agent-authored comment on a squad-assigned Discussion container", func(t *testing.T) {
		got := testHandler.computeCommentAgentTriggers(ctx, fx.SquadDiscussionIssue, "worker result posted", nil, "agent", fx.AgentID, commentTriggerComputeOptions{})
		if len(got) != 0 {
			t.Fatalf("expected no triggers on a squad-assigned Discussion container, got %d: %+v", len(got), got)
		}
	})

	// Regression: the same @agent mention on a non-Discussion issue must
	// still enqueue exactly as before — the red line must not leak past
	// origin_type='project_discussion'.
	t.Run("regression: ordinary issue still enqueues on @agent mention", func(t *testing.T) {
		got := testHandler.computeCommentAgentTriggers(ctx, fx.OrdinaryIssue, agentMention, nil, "member", testUserID, commentTriggerComputeOptions{})
		if len(got) != 1 {
			t.Fatalf("expected exactly one trigger on an ordinary issue, got %d: %+v", len(got), got)
		}
		if util.UUIDToString(got[0].Agent.ID) != fx.AgentID {
			t.Fatalf("expected trigger for agent %s, got %s", fx.AgentID, util.UUIDToString(got[0].Agent.ID))
		}
	})
}
