package service

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// draftSweepStorage implements storage.Storage for the sweeper tests. It
// records every key handed to DeleteObject; failAll injects a delete error;
// blockFirst + entered/release turn the FIRST delete into a latch for the
// BLOCK-011 bind race test (the blocked key is captured for the caller).
type draftSweepStorage struct {
	mu         sync.Mutex
	deleted    []string
	failAll    bool
	blockFirst bool
	blockedKey string
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
}

func (f *draftSweepStorage) DeleteObject(ctx context.Context, key string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, key)
	fail := f.failAll
	block := f.blockFirst && f.blockedKey == ""
	if block {
		f.blockedKey = key
	}
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("injected object delete failure")
	}
	if block {
		f.once.Do(func() { close(f.entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.release:
		}
	}
	return nil
}

func (f *draftSweepStorage) blockedOn() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blockedKey
}

func (f *draftSweepStorage) Upload(context.Context, string, []byte, string, string) (string, error) {
	return "", nil
}
func (f *draftSweepStorage) Delete(context.Context, string) {}
func (f *draftSweepStorage) DeleteKeys(context.Context, []string) {}
func (f *draftSweepStorage) KeyFromURL(rawURL string) string { return rawURL }
func (f *draftSweepStorage) ObjectURL(key string) string     { return key }
func (f *draftSweepStorage) CdnDomain() string               { return "" }
func (f *draftSweepStorage) GetReader(context.Context, string) (io.ReadCloser, error) {
	return nil, nil
}

func (f *draftSweepStorage) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// seedDraftSweepWorkspace creates an isolated user + workspace pair so the
// sweeper tests never touch the shared session-kernel fixtures.
func seedDraftSweepWorkspace(t *testing.T, pool *pgxpool.Pool) (workspaceID, userID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Draft Sweep Test", fmt.Sprintf("draft-sweep-%d@multica.ai", suffix)).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ($1, $2, 'DSW') RETURNING id`,
		fmt.Sprintf("DSW %d", suffix), fmt.Sprintf("dsw-%d", suffix)).Scan(&workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return workspaceID, userID
}

// seedDraftAttachment inserts an unbound draft row `age` in the past (negative
// duration). urlSuffix distinguishes objects in the fake storage. Returns the
// row id and its storage key.
func seedDraftAttachment(t *testing.T, pool *pgxpool.Pool, workspaceID, userID string, age time.Duration, urlSuffix string) (string, string) {
	t.Helper()
	id := dbid.NewV7()
	idStr := util.UUIDToString(id)
	url := "http://store/" + idStr + "/" + urlSuffix
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO attachment (id, workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes, created_at)
		VALUES ($1, $2, 'member', $3, 'draft.txt', $4, 'text/plain', 4, now() - make_interval(secs => $5))
	`, idStr, workspaceID, userID, url, -age.Seconds()); err != nil {
		t.Fatalf("seed draft attachment: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, idStr)
	})
	return idStr, url
}

// seedBoundAttachment inserts a draft-age row that is already bound to an
// issue: the sweeper must leave both the row and its object alone (the
// sweeper never uses DeleteAttachment, BLOCK-011).
func seedBoundAttachment(t *testing.T, pool *pgxpool.Pool, workspaceID, userID, issueID string, age time.Duration) (string, string) {
	t.Helper()
	id := dbid.NewV7()
	idStr := util.UUIDToString(id)
	url := "http://store/" + idStr + "/bound"
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO attachment (id, workspace_id, issue_id, uploader_type, uploader_id, filename, url, content_type, size_bytes, created_at)
		VALUES ($1, $2, $3, 'member', $4, 'bound.txt', $5, 'text/plain', 4, now() - make_interval(secs => $6))
	`, idStr, workspaceID, issueID, userID, url, -age.Seconds()); err != nil {
		t.Fatalf("seed bound attachment: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, idStr)
	})
	return idStr, url
}

func seedDraftSweepIssue(t *testing.T, pool *pgxpool.Pool, workspaceID, userID string) string {
	t.Helper()
	var issueID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, position)
		VALUES ($1, 'draft sweep bind target', 'todo', 'medium', $2, 'member', 0)
		RETURNING id
	`, workspaceID, userID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return issueID
}

// TestSweepChatDraftAttachmentsAgeBoundary is AC-28: the predicate is a
// STRICT 168h cutoff. Within one transaction now() is stable, so the exact
// boundary classification is pinned: 168h00s is not selected, 168h01s is.
// The full sweep then uses generous margins so wall-clock drift between
// seeding and sweeping cannot flake: 167h retained, 169h swept, and an
// already-bound old row left untouched (object included).
func TestSweepChatDraftAttachmentsAgeBoundary(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID := seedDraftSweepWorkspace(t, pool)

	// Pin the boundary predicate in a single transaction.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin boundary tx: %v", err)
	}
	exactID := dbid.NewV7()
	overID := dbid.NewV7()
	for _, seed := range []struct {
		id pgtype.UUID
		s  string
	}{
		{exactID, `now() - interval '168 hours'`},
		{overID, `now() - interval '168 hours' - interval '1 second'`},
	} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO attachment (id, workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes, created_at)
			VALUES ($1, $2, 'member', $3, 'draft.txt', 'http://store/' || $1::text, 'text/plain', 4, `+seed.s+`)
		`, util.UUIDToString(seed.id), workspaceID, userID); err != nil {
			t.Fatalf("seed boundary row: %v", err)
		}
	}
	var exactSelected, overSelected bool
	if err := tx.QueryRow(ctx, `SELECT created_at < now() - interval '168 hours' FROM attachment WHERE id = $1`, util.UUIDToString(exactID)).Scan(&exactSelected); err != nil {
		t.Fatalf("classify exact boundary row: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT created_at < now() - interval '168 hours' FROM attachment WHERE id = $1`, util.UUIDToString(overID)).Scan(&overSelected); err != nil {
		t.Fatalf("classify over boundary row: %v", err)
	}
	_ = tx.Rollback(ctx)
	if exactSelected {
		t.Fatal("AC-28: a row created exactly 168h00s ago must be retained this round")
	}
	if !overSelected {
		t.Fatal("AC-28: a row created 168h01s ago must be selected")
	}

	// Full sweep with margins.
	retainedID, _ := seedDraftAttachment(t, pool, workspaceID, userID, -167*time.Hour, "retained")
	boundID, boundURL := seedBoundAttachment(t, pool, workspaceID, userID, seedDraftSweepIssue(t, pool, workspaceID, userID), -169*time.Hour)
	sweptID, sweptURL := seedDraftAttachment(t, pool, workspaceID, userID, -169*time.Hour, "swept")

	st := &draftSweepStorage{}
	deleted, err := SweepChatDraftAttachments(ctx, db.New(pool), pool, st, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	keys := st.deletedKeys()
	if len(keys) != 1 || keys[0] != sweptURL {
		t.Fatalf("deleted keys = %v, want exactly [%s]", keys, sweptURL)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, sweptID).Scan(&count); err != nil {
		t.Fatalf("count swept row: %v", err)
	}
	if count != 0 {
		t.Fatal("swept row still exists")
	}
	for _, id := range []string{retainedID, boundID} {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("count retained row: %v", err)
		}
		if count != 1 {
			t.Fatalf("row %s must be retained", id)
		}
	}
	// The bound row's object must never be handed to storage (BLOCK-011:
	// the sweeper path has no DeleteAttachment and never touches bound rows).
	for _, k := range keys {
		if k == boundURL {
			t.Fatalf("bound row's object was deleted: %v", keys)
		}
	}
}

// TestSweepChatDraftAttachmentsStorageFailureRetriesNextTick: a failing
// object delete keeps the row (next tick retries); an empty URL is skipped
// without touching storage; a nil storage deletes nothing.
func TestSweepChatDraftAttachmentsStorageFailureRetriesNextTick(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID := seedDraftSweepWorkspace(t, pool)

	failingID, failingURL := seedDraftAttachment(t, pool, workspaceID, userID, -169*time.Hour, "failing")
	emptyURLID := dbid.NewV7()
	emptyURLIDStr := util.UUIDToString(emptyURLID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO attachment (id, workspace_id, uploader_type, uploader_id, filename, url, content_type, size_bytes, created_at)
		VALUES ($1, $2, 'member', $3, 'draft.txt', '', 'text/plain', 4, now() - interval '169 hours')
	`, emptyURLIDStr, workspaceID, userID); err != nil {
		t.Fatalf("seed empty-url row: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, emptyURLIDStr)
	})
	nilID, _ := seedDraftAttachment(t, pool, workspaceID, userID, -169*time.Hour, "nil-storage")

	// Round 1: storage deletes fail. Nothing may be deleted.
	failing := &draftSweepStorage{failAll: true}
	deleted, err := SweepChatDraftAttachments(ctx, db.New(pool), pool, failing, 100)
	if err != nil {
		t.Fatalf("sweep with failing storage: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d with failing storage, want 0", deleted)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, failingID).Scan(&count); err != nil {
		t.Fatalf("count failing row: %v", err)
	}
	if count != 1 {
		t.Fatal("row must survive a failed object delete")
	}

	// Round 2: storage healthy — the same row is swept on the next tick.
	healthy := &draftSweepStorage{}
	deleted, err = SweepChatDraftAttachments(ctx, db.New(pool), pool, healthy, 100)
	if err != nil {
		t.Fatalf("sweep with healthy storage: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d on retry, want 1", deleted)
	}
	keys := healthy.deletedKeys()
	if len(keys) != 1 || keys[0] != failingURL {
		t.Fatalf("deleted keys = %v, want exactly [%s]", keys, failingURL)
	}
	// The empty-URL row is skipped: never handed to storage, row retained.
	for _, k := range keys {
		if k == "" {
			t.Fatal("empty-URL row reached storage")
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, emptyURLIDStr).Scan(&count); err != nil {
		t.Fatalf("count empty-url row: %v", err)
	}
	if count != 1 {
		t.Fatal("empty-URL row must be left for a later round")
	}

	// Nil storage: no deletes, no error.
	deleted, err = SweepChatDraftAttachments(ctx, db.New(pool), pool, nil, 100)
	if err != nil {
		t.Fatalf("sweep with nil storage: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d with nil storage, want 0", deleted)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, nilID).Scan(&count); err != nil {
		t.Fatalf("count nil-storage row: %v", err)
	}
	if count != 1 {
		t.Fatal("nil storage must not delete rows")
	}
}

// TestSweepChatDraftAttachmentsBindRaceDoesNotDeleteObject is the BLOCK-011
// concurrency fixture: the sweeper has already scanned its candidates and is
// blocked deleting ONE candidate's object (holding that row's lock); while it
// waits, a send binds the OTHER candidate. After release, the sweeper's
// locked re-read of the bound candidate misses (already bound) and its object
// is never deleted; the bound row survives.
func TestSweepChatDraftAttachmentsBindRaceDoesNotDeleteObject(t *testing.T) {
	pool := newProjectChatSessionTestPool(t)
	ctx := context.Background()
	workspaceID, userID := seedDraftSweepWorkspace(t, pool)
	issueID := seedDraftSweepIssue(t, pool, workspaceID, userID)

	firstID, firstURL := seedDraftAttachment(t, pool, workspaceID, userID, -169*time.Hour, "first")
	secondID, secondURL := seedDraftAttachment(t, pool, workspaceID, userID, -169*time.Hour, "second")

	st := &draftSweepStorage{
		blockFirst: true,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	done := make(chan struct{})
	var swept int
	var sweepErr error
	go func() {
		swept, sweepErr = SweepChatDraftAttachments(ctx, db.New(pool), pool, st, 100)
		close(done)
	}()
	select {
	case <-st.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("sweeper never reached the blocking object delete")
	}

	// The sweeper is blocked on one candidate's object delete while holding
	// that row's lock. The send path binds the OTHER candidate with the same
	// Lock+Bind pair the send transaction uses.
	blockedOn := st.blockedOn()
	var boundID, boundURL, releasedID string
	switch blockedOn {
	case firstURL:
		boundID, boundURL = secondID, secondURL
		releasedID = firstID
	case secondURL:
		boundID, boundURL = firstID, firstURL
		releasedID = secondID
	default:
		t.Fatalf("sweeper blocked on unexpected key %q", blockedOn)
	}

	sendTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin bind tx: %v", err)
	}
	qtx := db.New(pool).WithTx(sendTx)
	if _, err := qtx.LockUnboundDraftAttachments(ctx, db.LockUnboundDraftAttachmentsParams{
		WorkspaceID:   u(workspaceID),
		AttachmentIds: []pgtype.UUID{u(boundID)},
	}); err != nil {
		t.Fatalf("lock draft: %v", err)
	}
	bound, err := qtx.BindUnboundDraftAttachments(ctx, db.BindUnboundDraftAttachmentsParams{
		IssueID:       u(issueID),
		WorkspaceID:   u(workspaceID),
		AttachmentIds: []pgtype.UUID{u(boundID)},
		UploaderType:  "member",
		UploaderID:    u(userID),
	})
	if err != nil {
		t.Fatalf("bind draft: %v", err)
	}
	if len(bound) != 1 {
		t.Fatalf("bound = %d rows, want 1", len(bound))
	}
	if err := sendTx.Commit(ctx); err != nil {
		t.Fatalf("commit bind: %v", err)
	}

	close(st.release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("sweeper never finished after release")
	}
	if sweepErr != nil {
		t.Fatalf("sweep: %v", sweepErr)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want 1 (only the blocked candidate)", swept)
	}
	keys := st.deletedKeys()
	if len(keys) != 1 || keys[0] != blockedOn {
		t.Fatalf("deleted keys = %v, want exactly [%s]", keys, blockedOn)
	}
	for _, k := range keys {
		if k == boundURL {
			t.Fatalf("bound row's object must never be deleted: %v", keys)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1 AND issue_id = $2`, boundID, issueID).Scan(&count); err != nil {
		t.Fatalf("count bound row: %v", err)
	}
	if count != 1 {
		t.Fatal("bound row must survive the sweep, still bound")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attachment WHERE id = $1`, releasedID).Scan(&count); err != nil {
		t.Fatalf("count released row: %v", err)
	}
	if count != 0 {
		t.Fatal("blocked candidate must be swept after release")
	}
}
