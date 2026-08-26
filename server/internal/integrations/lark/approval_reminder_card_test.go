package lark

// AIFIRST: CR-2026-051 TASK-04 — the five-class test suite for the
// approval-reminder card transport (SDD §4.5 table): parameter validation
// with zero HTTP, happy path (open_id DM + interactive + card body + CTA),
// token-invalidation retry, JSON escaping of hostile CR titles, and the stub
// shape. Reuses the existing lark package fakes (newLarkFake / stubToken /
// newTestClient / testCreds / writeJSON / tokenN).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"log/slog"
)

func approvalReminderParams() ApprovalReminderParams {
	return ApprovalReminderParams{
		InstallationID: testCreds(),
		OpenID:         OpenID("ou_approver"),
		CRID:           "CR-2026-051",
		CRTitle:        "IM 渠道审批接入",
		StageLabel:     "需求审批",
		ApproveURL:     "https://multica.test/ws/projects/proj-1?tab=chat",
	}
}

func TestApprovalReminderCardValidation(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_v", 7200)
	c := newTestClient(fake, time.Now)

	p := approvalReminderParams()
	p.OpenID = ""
	err := c.SendApprovalReminderCard(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "missing open_id") {
		t.Fatalf("empty open_id: got %v", err)
	}

	p = approvalReminderParams()
	p.ApproveURL = ""
	err = c.SendApprovalReminderCard(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "missing approve url") {
		t.Fatalf("empty approve url: got %v", err)
	}

	if got := fake.tokenN.Load(); got != 0 {
		t.Errorf("validation failures must not hit the token endpoint, tokenN=%d", got)
	}
	if got := fake.sendN.Load(); got != 0 {
		t.Errorf("validation failures must not hit /messages, sendN=%d", got)
	}
}

func TestApprovalReminderCardHappyPath(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_h", 7200)

	var capturedBody map[string]string
	fake.mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		fake.sendN.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		if got := r.URL.Query().Get("receive_id_type"); got != "open_id" {
			t.Errorf("receive_id_type = %q, want open_id", got)
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_reminder"}})
	})

	c := newTestClient(fake, time.Now)
	if err := c.SendApprovalReminderCard(context.Background(), approvalReminderParams()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if fake.sendN.Load() != 1 {
		t.Fatalf("sendN = %d, want 1", fake.sendN.Load())
	}
	if capturedBody["receive_id"] != "ou_approver" {
		t.Errorf("receive_id = %q", capturedBody["receive_id"])
	}
	if capturedBody["msg_type"] != "interactive" {
		t.Errorf("msg_type = %q", capturedBody["msg_type"])
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(capturedBody["content"]), &card); err != nil {
		t.Fatalf("card JSON must parse: %v", err)
	}
	header, _ := card["header"].(map[string]any)
	htitle, _ := header["title"].(map[string]any)
	if htitle["content"] != "待人工审批" {
		t.Errorf("header title = %v", htitle["content"])
	}
	elements, _ := card["elements"].([]any)
	var textContents []string
	var buttonURLs []string
	for _, el := range elements {
		m, _ := el.(map[string]any)
		switch m["tag"] {
		case "div":
			txt, _ := m["text"].(map[string]any)
			if s, ok := txt["content"].(string); ok {
				textContents = append(textContents, s)
			}
		case "action":
			actions, _ := m["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["tag"] == "button" {
					buttonURLs = append(buttonURLs, am["url"].(string))
				}
			}
		}
	}
	joined := strings.Join(textContents, "\n")
	for _, want := range []string{"CR-2026-051", "IM 渠道审批接入", "需求审批"} {
		if !strings.Contains(joined, want) {
			t.Errorf("card body missing %q: %q", want, joined)
		}
	}
	if len(buttonURLs) != 1 || buttonURLs[0] != "https://multica.test/ws/projects/proj-1?tab=chat" {
		t.Errorf("CTA buttons = %v", buttonURLs)
	}
	if strings.Contains(capturedBody["content"], `"tag":"button","type":"danger"`) {
		t.Error("card must not carry approve/reject actions")
	}
}

func TestApprovalReminderCardTokenInvalidation(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_t", 7200)

	var sendCalls int
	fake.mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		fake.sendN.Add(1)
		sendCalls++
		if sendCalls == 1 {
			writeJSON(w, map[string]any{"code": codeTokenExpired, "msg": "expired"})
			return
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_ok"}})
	})

	c := newTestClient(fake, time.Now)
	err := c.SendApprovalReminderCard(context.Background(), approvalReminderParams())
	if err == nil || !strings.Contains(err.Error(), "code=99991663") {
		t.Fatalf("first send must fail with token-expired, got %v", err)
	}
	if fake.tokenN.Load() != 1 {
		t.Fatalf("tokenN after first = %d, want 1", fake.tokenN.Load())
	}
	// Token was invalidated: the retry must fetch a fresh token.
	if err := c.SendApprovalReminderCard(context.Background(), approvalReminderParams()); err != nil {
		t.Fatalf("retry after invalidation: %v", err)
	}
	if fake.tokenN.Load() != 2 {
		t.Errorf("tokenN after retry = %d, want 2 (invalidateToken must have dropped the cache)", fake.tokenN.Load())
	}
}

func TestApprovalReminderCardJSONEscaping(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("tok_e", 7200)
	var capturedBody map[string]string
	fake.mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		fake.sendN.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		writeJSON(w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_esc"}})
	})
	c := newTestClient(fake, time.Now)

	hostile := "标题\"引号\\反斜杠\n换行😀emoji"
	p := approvalReminderParams()
	p.CRTitle = hostile
	if err := c.SendApprovalReminderCard(context.Background(), p); err != nil {
		t.Fatalf("send: %v", err)
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(capturedBody["content"]), &card); err != nil {
		t.Fatalf("card JSON must parse despite hostile title: %v", err)
	}
	raw := capturedBody["content"]
	if !strings.Contains(raw, "标题\\\"引号") {
		t.Errorf("quotes must be escaped in the card JSON: %q", raw)
	}
	// Round-trip: the title survives byte-for-byte.
	var back struct {
		Elements []struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	var joined string
	for _, e := range back.Elements {
		joined += e.Text.Content + "\n"
	}
	if !strings.Contains(joined, hostile) {
		t.Errorf("hostile title must round-trip losslessly:\n%q", joined)
	}
}

func TestApprovalReminderCardStubShape(t *testing.T) {
	stub := NewStubAPIClient(slog.Default())
	if stub.IsConfigured() {
		t.Fatal("stub must report IsConfigured()=false")
	}
	err := stub.SendApprovalReminderCard(context.Background(), approvalReminderParams())
	if !errors.Is(err, ErrAPIClientNotConfigured) {
		t.Fatalf("stub send = %v, want ErrAPIClientNotConfigured", err)
	}
}
