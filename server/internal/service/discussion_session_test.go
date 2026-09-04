package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CR-2026-059 TASK-02 (SDD §4.3/§4.6): pure-logic verification for the
// Coordinator trigger matrix, the send fingerprint order invariance and the
// merge-forward rendering. DB-backed send/ensure vectors live in the handler
// package tests (TASK-03, plan cmd-02).

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

func agentWithID(t *testing.T, s string) *db.Agent {
	t.Helper()
	return &db.Agent{ID: pgtype.UUID{Bytes: mustUUID(t, s), Valid: true}}
}

func TestDetectCoordinatorTriggerMatrix(t *testing.T) {
	const (
		coordinator = "11111111-1111-1111-1111-111111111111"
		otherAgent  = "22222222-2222-2222-2222-222222222222"
	)
	configured := mustUUID(t, coordinator)
	nilUUID := uuid.Nil
	routable := agentWithID(t, coordinator)
	mentionContent := "please look at this [@Coordinator](mention://agent/" + coordinator + ")"
	otherMentionContent := "please look at this [@Other](mention://agent/" + otherAgent + ")"

	cases := []struct {
		name     string
		content  string
		request  string
		conf     uuid.UUID
		routable *db.Agent
		want     triggerDecision
	}{
		{
			name:    "analyze with routable coordinator enqueues",
			request: "analyze", conf: configured, routable: routable,
			want: triggerDecision{NeedTask: true, Reason: "none"},
		},
		{
			name:    "summarize with routable coordinator enqueues",
			request: "summarize", conf: configured, routable: routable,
			want: triggerDecision{NeedTask: true, Reason: "none"},
		},
		{
			name:    "analyze with archived coordinator is unavailable",
			request: "analyze", conf: configured,
			want: triggerDecision{Reason: "unavailable"},
		},
		{
			name:    "summarize with hard-deleted coordinator is unavailable",
			request: "summarize", conf: configured,
			want: triggerDecision{Reason: "unavailable"},
		},
		{
			name:    "analyze without any coordinator is not_configured",
			request: "analyze", conf: nilUUID,
			want: triggerDecision{Reason: "not_configured"},
		},
		{
			name:    "mention of configured routable coordinator enqueues",
			content: mentionContent, request: "none", conf: configured, routable: routable,
			want: triggerDecision{NeedTask: true, Reason: "none"},
		},
		{
			name:    "mention of configured archived coordinator is unavailable",
			content: mentionContent, request: "none", conf: configured,
			want: triggerDecision{Reason: "unavailable"},
		},
		{
			name:    "mention of configured hard-deleted coordinator is unavailable",
			content: mentionContent, request: "none", conf: configured,
			want: triggerDecision{Reason: "unavailable"},
		},
		{
			name:    "mention of another agent is an ordinary message",
			content: otherMentionContent, request: "none", conf: configured, routable: routable,
			want: triggerDecision{Reason: "none"},
		},
		{
			name:    "mention of another agent with unconfigured project is ordinary",
			content: otherMentionContent, request: "none", conf: nilUUID,
			want: triggerDecision{Reason: "none"},
		},
		{
			name:    "mention of another agent while coordinator unavailable is ordinary",
			content: otherMentionContent, request: "none", conf: configured,
			want: triggerDecision{Reason: "none"},
		},
		{
			name:    "request=mention with routable coordinator mention enqueues once",
			content: mentionContent, request: "mention", conf: configured, routable: routable,
			want: triggerDecision{NeedTask: true, Reason: "none"},
		},
		{
			name:    "request=mention with archived configured coordinator is unavailable",
			content: mentionContent, request: "mention", conf: configured,
			want: triggerDecision{Reason: "unavailable"},
		},
		{
			name:    "no trigger at all",
			content: "hello", request: "none", conf: configured, routable: routable,
			want: triggerDecision{Reason: "none"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectCoordinatorTrigger(tc.content, tc.request, tc.conf, tc.routable)
			if got.NeedTask != tc.want.NeedTask || got.Reason != tc.want.Reason {
				t.Fatalf("detectCoordinatorTrigger = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDiscussionSendFingerprintOrderInvariant(t *testing.T) {
	// AC-26: the same attachment set in a different request order must yield
	// the same fingerprint (never a 409 idempotency_key_reused on replay).
	ids := []uuid.UUID{
		mustUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		mustUUID(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		mustUUID(t, "cccccccc-cccc-cccc-cccc-cccccccccccc"),
	}
	a := DiscussionSendInput{Content: "same content", AttachmentIDs: ids, CoordinatorRequest: "none"}
	b := DiscussionSendInput{
		Content:            "same content",
		AttachmentIDs:      []uuid.UUID{ids[2], ids[0], ids[1]},
		CoordinatorRequest: "none",
	}
	fa, err := discussionSendFingerprint(a)
	if err != nil {
		t.Fatalf("fingerprint a: %v", err)
	}
	fb, err := discussionSendFingerprint(b)
	if err != nil {
		t.Fatalf("fingerprint b: %v", err)
	}
	if fa != fb {
		t.Fatalf("attachment order must not change the fingerprint:\n a=%s\n b=%s", fa, fb)
	}
	// Fingerprint uses trimmed content (SDD §4.6), so surrounding whitespace
	// must NOT change it; a real content difference must.
	c := DiscussionSendInput{Content: "same content ", AttachmentIDs: ids, CoordinatorRequest: "none"}
	fc, err := discussionSendFingerprint(c)
	if err != nil {
		t.Fatalf("fingerprint c: %v", err)
	}
	if fc != fa {
		t.Fatal("fingerprint must trim content: surrounding whitespace must not change it")
	}
	d := DiscussionSendInput{Content: "different content", AttachmentIDs: ids, CoordinatorRequest: "none"}
	fd, err := discussionSendFingerprint(d)
	if err != nil {
		t.Fatalf("fingerprint d: %v", err)
	}
	if fd == fa {
		t.Fatal("content must participate in the fingerprint")
	}
	e := DiscussionSendInput{Content: "same content", AttachmentIDs: ids, CoordinatorRequest: "analyze"}
	fe, err := discussionSendFingerprint(e)
	if err != nil {
		t.Fatalf("fingerprint e: %v", err)
	}
	if fe == fa {
		t.Fatal("coordinator_request must participate in the fingerprint")
	}
}

func TestMergeForwardMessageFingerprintDedupPreservesOrder(t *testing.T) {
	// SDD §4.6: dedup by first occurrence, order preserved, register_cr joins
	// the fingerprint.
	mk := func(s string) db.ChatMessage {
		return db.ChatMessage{
			ID:        pgtype.UUID{Bytes: mustUUID(t, s), Valid: true},
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
	}
	base := []db.ChatMessage{mk("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"), mk("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")}
	reordered := []db.ChatMessage{base[1], base[0]}
	duped := []db.ChatMessage{base[0], base[1], base[0]}

	fb := mergeForwardMessageFingerprint(base, false)
	fr := mergeForwardMessageFingerprint(reordered, false)
	if fb == fr {
		t.Fatal("message order is mandated by PRD and must be preserved in the fingerprint")
	}
	fd := mergeForwardMessageFingerprint(duped, false)
	if fd != fb {
		t.Fatal("duplicates must collapse by first occurrence and not change the fingerprint")
	}
	if mergeForwardMessageFingerprint(base, true) == fb {
		t.Fatal("register_cr must participate in the fingerprint")
	}
}

func TestBuildMergedForwardContentFromMessages(t *testing.T) {
	// Structure mirrors buildMergedForwardContent; NULL authors degrade to the
	// role literal (private/legacy semantics). No DB access on the NULL path.
	msgs := []db.ChatMessage{
		{
			Role:      "user",
			Content:   "first message\nsecond line",
			CreatedAt: pgtype.Timestamptz{Time: time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC), Valid: true},
		},
		{
			Role:      "assistant",
			Content:   "the reply",
			CreatedAt: pgtype.Timestamptz{Time: time.Date(2026, 9, 5, 1, 1, 0, 0, time.UTC), Valid: true},
		},
	}
	svc := &TaskService{}
	out := buildMergedForwardContentFromMessages(context.Background(), svc, msgs, true)
	for _, want := range []string{
		"## Trigger message",
		"> first message",
		"> second line",
		"## Conversation history (2 messages)",
		"- [user 2026-09-05T01:00:00Z] first message second line",
		"- [assistant 2026-09-05T01:01:00Z] the reply",
		"## 升级为 CR",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("merged content missing %q:\n%s", want, out)
		}
	}
}
