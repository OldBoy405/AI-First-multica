package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// newDraftGateRequest builds a request to a single-attachment handler with
// the chi URL param set and the given actor + workspace headers.
func newDraftGateRequest(method, path, attachmentID, userID, workspaceID string) (*http.Request, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, path, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", attachmentID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}
	return req, httptest.NewRecorder()
}

// seedSecondMember adds a second member to the shared handler test workspace
// and removes them on cleanup.
func seedSecondMember(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := testPool.QueryRow(ctx, `INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Second Member", fmt.Sprintf("second-member-%d@multica.ai", time.Now().UnixNano())).Scan(&userID); err != nil {
		t.Fatalf("seed second user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
		testWorkspaceID, userID); err != nil {
		t.Fatalf("seed second member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

// multipartFileBody builds a one-file multipart form the way the web
// composer does (file field only, no issue_id).
func multipartFileBody(t *testing.T, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	return &body, writer.FormDataContentType()
}

// uploadResponseID extracts the attachment id from an UploadFile response.
func uploadResponseID(t *testing.T, raw string) string {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("decode upload response: %v; body: %s", err, raw)
	}
	id, _ := resp["id"].(string)
	return id
}

// TestUploadFileToleratesMissingIssueID pins the TASK-09 backend contract:
// a workspace upload without any bind target (no issue_id) creates an unbound
// draft row — the frontend composer stops sending issueId (TASK-10).
func TestUploadFileToleratesMissingIssueID(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	origStorage := testHandler.Storage
	testHandler.Storage = &mockStorage{}
	defer func() { testHandler.Storage = origStorage }()

	body, contentType := multipartFileBody(t, "draft-upload.txt", "draft bytes")
	req := httptest.NewRequest("POST", "/api/upload-file", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-User-ID", testUserID)
	req.Header.Set("X-Workspace-ID", testWorkspaceID)

	w := httptest.NewRecorder()
	testHandler.UploadFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload without issue_id: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	id := uploadResponseID(t, w.Body.String())
	if id == "" {
		t.Fatalf("upload response missing id: %s", w.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM attachment WHERE id = $1`, id)
	})

	var bound int
	if err := testPool.QueryRow(context.Background(), `
		SELECT (issue_id IS NOT NULL OR comment_id IS NOT NULL OR chat_session_id IS NOT NULL
		        OR chat_message_id IS NOT NULL OR task_id IS NOT NULL)::int
		FROM attachment WHERE id = $1
	`, id).Scan(&bound); err != nil {
		t.Fatalf("load draft row: %v", err)
	}
	if bound != 0 {
		t.Fatal("upload without bind targets must create an unbound draft row")
	}
}

// TestDraftAttachmentUploaderGate is AC-14 / SDD §4.9: an unbound draft row
// is visible only to its member uploader. Another member of the same
// workspace gets 404 on GET, preview, and delete (no existence leak), and
// the draft can never appear in an issue attachment list (it has no
// issue_id).
func TestDraftAttachmentUploaderGate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	ctx := context.Background()

	draftID := seedAttachmentURL(t, "https://cdn.example.com/drafts/gate.txt", "gate.txt", "text/plain", 4)
	otherUserID := seedSecondMember(t)

	// Uploader reads their own draft (GET /api/attachments/{id}).
	req, w := newDraftGateRequest("GET", "/api/attachments/"+draftID, draftID, testUserID, testWorkspaceID)
	testHandler.GetAttachmentByID(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("uploader GET own draft: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Another member: GET -> 404.
	req, w = newDraftGateRequest("GET", "/api/attachments/"+draftID, draftID, otherUserID, testWorkspaceID)
	testHandler.GetAttachmentByID(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("other member GET draft: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Another member: preview content -> 404 (same loader).
	req, w = newDraftGateRequest("GET", "/api/attachments/"+draftID+"/content", draftID, otherUserID, testWorkspaceID)
	testHandler.GetAttachmentContent(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("other member preview draft: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Another member: delete -> 404 (no admin bypass for drafts).
	req, w = newDraftGateRequest("DELETE", "/api/attachments/"+draftID, draftID, otherUserID, testWorkspaceID)
	testHandler.DeleteAttachment(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("other member delete draft: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// The draft row carries no issue_id, so ListAttachmentsByIssue (the
	// project attachment list) can never return it.
	var issueBound int
	if err := testPool.QueryRow(ctx, `SELECT (issue_id IS NOT NULL)::int FROM attachment WHERE id = $1`, draftID).Scan(&issueBound); err != nil {
		t.Fatalf("load draft row: %v", err)
	}
	if issueBound != 0 {
		t.Fatal("draft row must not carry an issue_id")
	}

	// Uploader deletes their own draft -> 204.
	req, w = newDraftGateRequest("DELETE", "/api/attachments/"+draftID, draftID, testUserID, testWorkspaceID)
	testHandler.DeleteAttachment(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("uploader delete own draft: expected 204, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDraftAttachmentDownloadUploaderGate covers the bare-navigation download
// path (loadAttachmentForDownload): workspace membership alone must not reveal
// another member's draft; the uploader still downloads.
func TestDraftAttachmentDownloadUploaderGate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture unavailable")
	}
	store := &mockStorage{}
	origStorage := testHandler.Storage
	origCfg := testHandler.cfg
	testHandler.Storage = store
	testHandler.cfg.AttachmentDownloadMode = "proxy"
	t.Cleanup(func() {
		testHandler.Storage = origStorage
		testHandler.cfg = origCfg
	})

	key := "drafts/download-gate.txt"
	store.put(key, []byte("download body"))
	draftID := seedAttachmentURL(t, "https://cdn.example.com/"+key, "download-gate.txt", "text/plain", 14)
	otherUserID := seedSecondMember(t)

	// Other member: bare download -> 404 (no existence leak).
	req := httptest.NewRequest("GET", "/api/attachments/"+draftID+"/download", nil)
	req.Header.Set("X-User-ID", otherUserID)
	w := httptest.NewRecorder()
	newDownloadRouter().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("other member download draft: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Uploader: bare download -> 200.
	req = httptest.NewRequest("GET", "/api/attachments/"+draftID+"/download", nil)
	req.Header.Set("X-User-ID", testUserID)
	w = httptest.NewRecorder()
	newDownloadRouter().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("uploader download own draft: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
