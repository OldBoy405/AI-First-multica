package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// AIFIRST: CR-2026-051 FR-5/FR-6 — approval reminder card transport. The card
// is sent to a member's open_id (private chat) when a CR enters one of the
// four human-approval gates; the single CTA opens the existing web project
// chat tab where the human signs the approval. Deliberately a separate method
// from SendBindingPromptCard (different card body and CTA) — see the interface
// declaration in client.go.

// ApprovalReminderParams carries the data needed to render and send the
// approval-reminder card (single CTA: open the approval session in the web
// app). Mirrors the field style of BindingPromptParams.
type ApprovalReminderParams struct {
	InstallationID InstallationCredentials
	OpenID         OpenID
	CRID           string
	// CRTitle is the CR's title from the cr row; when empty only the CR ID
	// is rendered (no blank line).
	CRTitle string
	// StageLabel is the display name of the gate being entered (mapped by
	// the caller via stageLabel; logs always use the raw status instead).
	StageLabel string
	// ApproveURL is the absolute URL the CTA opens. Empty is rejected
	// before any HTTP happens.
	ApproveURL string
}

// SendApprovalReminderCard sends the approval-reminder card to one open_id.
// http implementation: validates params, renders the card, then reuses the
// shared open_id private-chat transport (sendCardToOpenID).
func (c *httpAPIClient) SendApprovalReminderCard(ctx context.Context, p ApprovalReminderParams) error {
	if p.OpenID == "" {
		return errors.New("lark http client: missing open_id")
	}
	if p.ApproveURL == "" {
		return errors.New("lark http client: missing approve url")
	}
	cardJSON, err := approvalReminderTemplate(p)
	if err != nil {
		return fmt.Errorf("lark http client: render approval reminder: %w", err)
	}
	return c.sendCardToOpenID(ctx, p.InstallationID, p.OpenID, cardJSON, "send approval reminder")
}

// SendApprovalReminderCard on the stub client refuses transport with
// ErrAPIClientNotConfigured, same as the other stub methods (zero HTTP).
func (s *stubAPIClient) SendApprovalReminderCard(ctx context.Context, p ApprovalReminderParams) error {
	s.log.Warn("lark stub client: SendApprovalReminderCard called", "open_id", string(p.OpenID))
	return ErrAPIClientNotConfigured
}

// approvalReminderTemplate renders the approval-reminder card. Strictly the
// FR-6 five items: header title, CR ID (plus title when present), the gate's
// display stage, one fixed explanation line, and a single url button pointing
// at ApproveURL. Built via map + json.Marshal — never string concatenation —
// so quotes/backslashes/newlines/emoji in the CR title are escaped by the
// encoder. No approve/reject actions, no callback, no token, no evidence.
func approvalReminderTemplate(p ApprovalReminderParams) (string, error) {
	crLine := "**" + p.CRID + "**"
	if p.CRTitle != "" {
		crLine += "：" + p.CRTitle
	}
	doc := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "orange",
			"title":    map[string]any{"tag": "plain_text", "content": "待人工审批"},
		},
		"elements": []any{
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": crLine + "\n阶段：" + p.StageLabel,
				},
			},
			map[string]any{
				"tag": "div",
				"text": map[string]any{
					"tag":     "lark_md",
					"content": "该 CR 已进入人工审批门禁，请在 Multica 中完成审批。",
				},
			},
			map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":  "button",
						"text": map[string]any{"tag": "plain_text", "content": "前往审批"},
						"type": "primary",
						"url":  p.ApproveURL,
					},
				},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
