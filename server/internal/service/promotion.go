package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// AIFIRST: CR-2026-061 (SDD §4.1–§4.6): Discussion → work Issue promotion
// service core. The ONE new issue write path (FR-1/FR-2): everything funnels
// through IssueService.createInTx inside a single transaction together with
// the idempotency reservation, the context_refs entry and the pre-built
// requirement-authoring run (NFR-4). This file NEVER writes chat_message /
// attachment / chat_session rows (FR-5 zero-write invariant).

const (
	// promotionIdempotencyScope is the chat_idempotency scope_type value
	// introduced by migration 505; scope_id semantics = project_id (SDD §2.2).
	promotionIdempotencyScope = "discussion_promotion"
	// promotionAdvisoryPrefix is the project-level advisory lock key prefix
	// (D-4): deliberately independent from the Discussion session prefix so
	// promotions never queue behind message sends.
	promotionAdvisoryPrefix = "project-discussion-promotion"
	// promotionMaxItems is the defensive per-selection cap, same value as
	// mergeForwardMaxComments (SDD §7.3).
	promotionMaxItems = 50
	// promotionContextRefKind is the fixed machine-readable type marker of
	// promotion context_refs entries (SDD §2.1).
	promotionContextRefKind = "discussion_promotion"
	// promotionCreatedStatus / promotionCreatedPriority are the defaults for
	// the created work issue (PRD: an ordinary backlog issue, no assignee).
	promotionCreatedStatus   = "backlog"
	promotionCreatedPriority = "none"
	// promotionFirstNodeRef is the requirement-authoring template's first
	// node ref (requirement-register skill).
	promotionFirstNodeRef = "requirement-register"
	// promotionCreatedDescriptionSummaryLimit / -Count bound the default
	// description digest (SDD §4.6).
	promotionSummaryRuneLimit   = 120
	promotionSummaryMaxMessages = 5
)

// promotionFirstNodeID is the requirement-authoring template's first node id
// (tools pipeline-templates/requirement-authoring.pipeline.json nodes[0].id,
// SDD §2.3).
var promotionFirstNodeID = pgtype.UUID{Bytes: uuid.MustParse("00000000-0000-0000-0011-000000000001"), Valid: true}

// Promotion sentinels (SDD §4.4); HTTP mapping lives in the handler (TASK-03).
var (
	// ErrInvalidPromotionSelection → 400 invalid_promotion_selection.
	ErrInvalidPromotionSelection = errors.New("invalid promotion selection")
	// ErrPromotionRunCreateFailed → 502 pipeline_run_create_failed (AC-8).
	ErrPromotionRunCreateFailed = errors.New("promotion pipeline run creation failed")
	// ErrPromotionForbidden → 403 forbidden_promotion (in-transaction member
	// re-check, SDD §4.3 step 8 / §13.4).
	ErrPromotionForbidden = errors.New("forbidden promotion")
)

// PromoteDiscussionParams carries the handler-validated promotion request.
// The handler owns the zero-write pre-steps 1–6 (shape, project, member,
// capacity, session, selection ownership); this service re-asserts only what
// correctness requires inside the transaction.
type PromoteDiscussionParams struct {
	WorkspaceID    pgtype.UUID
	ProjectID      pgtype.UUID
	SessionID      pgtype.UUID
	CallerID       pgtype.UUID
	MessageIDs     []pgtype.UUID
	AttachmentIDs  []pgtype.UUID
	Title          *string
	Description    *string
	UpgradeToCR    bool
	IdempotencyKey string
}

// PromotionSourceRefs is the canonical source selection echoed in responses
// and entries.
type PromotionSourceRefs struct {
	SessionID     pgtype.UUID
	MessageIDs    []pgtype.UUID
	AttachmentIDs []pgtype.UUID
}

// promotionResultSourceRefs is the response's source_refs shape (SDD §3.1).
type promotionResultSourceRefs struct {
	SessionID     pgtype.UUID   `json:"session_id"`
	MessageIDs    []pgtype.UUID `json:"message_ids"`
	AttachmentIDs []pgtype.UUID `json:"attachment_ids"`
}

// PromotionResult is the response stored for replay and returned to the
// handler (SDD §3.1).
type PromotionResult struct {
	IssueID     pgtype.UUID               `json:"issue_id"`
	IssueNumber int32                     `json:"issue_number"`
	SessionID   pgtype.UUID               `json:"session_id"`
	SourceRefs  promotionResultSourceRefs `json:"source_refs"`
	Created     bool                      `json:"created"`
	UpgradeToCR bool                      `json:"upgrade_to_cr"`
	RunID       pgtype.UUID               `json:"run_id"`
}

// promotionContextRefEntry is the issue.context_refs array element written by
// promotion (SDD §2.1). Unknown extra fields from other entry kinds are
// tolerated by json.Unmarshal (rewrites re-marshal the same fields only).
type promotionContextRefEntry struct {
	Kind          string   `json:"kind"`
	SessionID     string   `json:"session_id"`
	MessageIDs    []string `json:"message_ids"`
	AttachmentIDs []string `json:"attachment_ids"`
	DedupeKey     string   `json:"dedupe_key"`
	PromotedBy    string   `json:"promoted_by"`
	PromotedAt    string   `json:"promoted_at"`
	PipelineRunID string   `json:"pipeline_run_id,omitempty"`
}

// promotionRunInputs is pipeline_run.inputs for the pre-built run (SDD §2.3).
type promotionRunInputs struct {
	SessionID     string   `json:"session_id"`
	MessageIDs    []string `json:"message_ids"`
	AttachmentIDs []string `json:"attachment_ids"`
	PromotedBy    string   `json:"promoted_by"`
	PromotedAt    string   `json:"promoted_at"`
}

// promotionRunExecutionContext is pipeline_run.execution_context — the
// machine-readable registration-intent marker, deterministic so replays never
// drift (SDD §2.4).
type promotionRunExecutionContext struct {
	Intent     string `json:"intent"`
	ExpectBind bool   `json:"expect_bind"`
	BindHint   struct {
		Endpoint string `json:"endpoint"`
		Cli      string `json:"cli"`
	} `json:"bind_hint"`
	Source struct {
		ProjectID     string   `json:"project_id"`
		SessionID     string   `json:"session_id"`
		MessageIDs    []string `json:"message_ids"`
		AttachmentIDs []string `json:"attachment_ids"`
	} `json:"source"`
}

// canonicalUUIDs lowercases, sorts and deduplicates a UUID list (SDD §4.1).
// Invalid (zero) ids are dropped — the handler already rejected them; this is
// the last line of defense so digest inputs are always canonical.
func canonicalUUIDs(ids []pgtype.UUID) []string {
	if len(ids) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !id.Valid {
			continue
		}
		s := uuid.UUID(id.Bytes).String()
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// canonicalUUIDSlice returns the canonical copy as pgtype.UUID (response echo).
func canonicalUUIDSlice(ids []pgtype.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, s := range canonicalUUIDs(ids) {
		out = append(out, pgtype.UUID{Bytes: uuid.MustParse(s), Valid: true})
	}
	return out
}

// promotionFingerprint is the canonical idempotency fingerprint (SDD §4.1):
// fixed key order, lowercase canonical UUIDs, includes projectId and
// upgrade_to_cr; title/description never participate.
func promotionFingerprint(projectID, sessionID pgtype.UUID, msg, att []pgtype.UUID, upgradeToCR bool) string {
	doc := struct {
		ProjectID     string   `json:"projectId"`
		SessionID     string   `json:"session_id"`
		MessageIDs    []string `json:"message_ids"`
		AttachmentIDs []string `json:"attachment_ids"`
		UpgradeToCR   bool     `json:"upgrade_to_cr"`
	}{
		ProjectID:     uuid.UUID(projectID.Bytes).String(),
		SessionID:     uuid.UUID(sessionID.Bytes).String(),
		MessageIDs:    canonicalUUIDs(msg),
		AttachmentIDs: canonicalUUIDs(att),
		UpgradeToCR:   upgradeToCR,
	}
	return sha256Hex(doc)
}

// promotionDedupeKey is the cross-key source dedupe key (SDD §4.1/FR-6):
// derived ONLY from the source set — no projectId, no upgrade_to_cr — so a
// plain promotion followed by an upgrade promotion resolves to the SAME issue.
func promotionDedupeKey(sessionID pgtype.UUID, msg, att []pgtype.UUID) string {
	doc := struct {
		SessionID     string   `json:"session_id"`
		MessageIDs    []string `json:"message_ids"`
		AttachmentIDs []string `json:"attachment_ids"`
	}{
		SessionID:     uuid.UUID(sessionID.Bytes).String(),
		MessageIDs:    canonicalUUIDs(msg),
		AttachmentIDs: canonicalUUIDs(att),
	}
	return sha256Hex(doc)
}

func sha256Hex(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// Fixed-shape structs always marshal; a failure here is a
		// programming error — surface it loudly rather than silently
		// producing a degenerate digest.
		panic(fmt.Sprintf("promotion digest marshal: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func promotionAdvisoryKey(workspaceID, projectID pgtype.UUID) string {
	return strings.Join([]string{promotionAdvisoryPrefix, util.UUIDToString(workspaceID), util.UUIDToString(projectID)}, "|")
}

func parsePromotionUUID(s string) (pgtype.UUID, error) {
	return util.ParseUUID(s)
}

func parsePromotionContextRefs(raw []byte) ([]promotionContextRefEntry, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries []promotionContextRefEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode issue context_refs: %w", err)
	}
	return entries, nil
}

// findPromotionEntry locates the entry carrying a dedupe_key; ok=false means
// the entry (or key) is absent.
func findPromotionEntry(entries []promotionContextRefEntry, dedupeKey string) (promotionContextRefEntry, int, bool) {
	for i, e := range entries {
		if e.Kind == promotionContextRefKind && e.DedupeKey == dedupeKey {
			return e, i, true
		}
	}
	return promotionContextRefEntry{}, -1, false
}

// buildPromotionEntry assembles the §2.1 entry from canonical inputs.
func buildPromotionEntry(p PromoteDiscussionParams, dedupeKey string, runID pgtype.UUID, promotedAt time.Time) promotionContextRefEntry {
	entry := promotionContextRefEntry{
		Kind:          promotionContextRefKind,
		SessionID:     util.UUIDToString(p.SessionID),
		MessageIDs:    canonicalUUIDs(p.MessageIDs),
		AttachmentIDs: canonicalUUIDs(p.AttachmentIDs),
		DedupeKey:     dedupeKey,
		PromotedBy:    util.UUIDToString(p.CallerID),
		PromotedAt:    promotedAt.UTC().Format(time.RFC3339),
	}
	if p.UpgradeToCR && runID.Valid {
		entry.PipelineRunID = util.UUIDToString(runID)
	}
	return entry
}

func entrySourceRefs(e promotionContextRefEntry) (PromotionSourceRefs, error) {
	refs := PromotionSourceRefs{
		MessageIDs:    make([]pgtype.UUID, 0, len(e.MessageIDs)),
		AttachmentIDs: make([]pgtype.UUID, 0, len(e.AttachmentIDs)),
	}
	var err error
	refs.SessionID, err = parsePromotionUUID(e.SessionID)
	if err != nil {
		return refs, fmt.Errorf("entry session_id: %w", err)
	}
	for _, s := range e.MessageIDs {
		id, perr := parsePromotionUUID(s)
		if perr != nil {
			return refs, fmt.Errorf("entry message_id %q: %w", s, perr)
		}
		refs.MessageIDs = append(refs.MessageIDs, id)
	}
	for _, s := range e.AttachmentIDs {
		id, perr := parsePromotionUUID(s)
		if perr != nil {
			return refs, fmt.Errorf("entry attachment_id %q: %w", s, perr)
		}
		refs.AttachmentIDs = append(refs.AttachmentIDs, id)
	}
	return refs, nil
}

// buildPromotionRunPlan assembles the run inputs + execution_context
// payloads (SDD §2.3/§2.4). Run id lives on the row, not in the JSON, so
// replays of equal inputs never drift.
func (s *IssueService) buildPromotionRunPlan(p PromoteDiscussionParams, runID pgtype.UUID) (*PromotionRunPlan, error) {
	promotedAt := time.Now().UTC().Format(time.RFC3339)
	inputs := promotionRunInputs{
		SessionID:     util.UUIDToString(p.SessionID),
		MessageIDs:    canonicalUUIDs(p.MessageIDs),
		AttachmentIDs: canonicalUUIDs(p.AttachmentIDs),
		PromotedBy:    util.UUIDToString(p.CallerID),
		PromotedAt:    promotedAt,
	}
	var execCtx promotionRunExecutionContext
	execCtx.Intent = "discussion-promotion"
	execCtx.ExpectBind = true
	execCtx.BindHint.Endpoint = "POST /api/crs/{crID}/bind-promotion-run"
	execCtx.BindHint.Cli = "multica cr bind-promotion-run"
	execCtx.Source.ProjectID = util.UUIDToString(p.ProjectID)
	execCtx.Source.SessionID = util.UUIDToString(p.SessionID)
	execCtx.Source.MessageIDs = canonicalUUIDs(p.MessageIDs)
	execCtx.Source.AttachmentIDs = canonicalUUIDs(p.AttachmentIDs)

	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("%w: encode promotion run inputs: %v", ErrPromotionRunCreateFailed, err)
	}
	execJSON, err := json.Marshal(execCtx)
	if err != nil {
		return nil, fmt.Errorf("%w: encode promotion execution context: %v", ErrPromotionRunCreateFailed, err)
	}
	return &PromotionRunPlan{RunID: runID, Inputs: inputsJSON, ExecutionContext: execJSON}, nil
}

// insertPromotionRun writes the pre-built run + first node, adopting the
// surviving run id on a 506 unique violation (concurrent backfill for the
// same issue, SDD §4.3/D-8). Any other failure is ErrPromotionRunCreateFailed
// so the caller maps 502 and the transaction rolls back (AC-8).
func insertPromotionRun(ctx context.Context, qtx *db.Queries, workspaceID, issueID, runID, startedBy pgtype.UUID, plan *PromotionRunPlan) (pgtype.UUID, error) {
	if _, err := qtx.InsertPipelineRun(ctx, db.InsertPipelineRunParams{
		ID:               runID,
		WorkspaceID:      workspaceID,
		IssueID:          issueID,
		Inputs:           []byte(plan.Inputs),
		ExecutionContext: []byte(plan.ExecutionContext),
		StartedBy:        startedBy,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Concurrent backfill won the unique race: re-read the surviving
			// run and adopt its id (no error — the run exists, which is the
			// invariant we wanted).
			existing, rerr := qtx.FindActiveRequirementRunForIssue(ctx, db.FindActiveRequirementRunForIssueParams{
				WorkspaceID: workspaceID,
				IssueID:     issueID,
			})
			if rerr != nil {
				return pgtype.UUID{}, fmt.Errorf("%w: re-read promotion run after unique conflict: %v", ErrPromotionRunCreateFailed, rerr)
			}
			return existing.ID, nil
		}
		return pgtype.UUID{}, fmt.Errorf("%w: insert promotion pipeline run: %v", ErrPromotionRunCreateFailed, err)
	}
	if _, err := qtx.InsertPipelineNodeRun(ctx, db.InsertPipelineNodeRunParams{
		RunID:   runID,
		NodeID:  promotionFirstNodeID,
		Ref:     pgtype.Text{String: promotionFirstNodeRef, Valid: true},
		Kind:    "skill",
		Seq:     1,
		Status:  "running",
		Attempt: 1,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: insert promotion pipeline node run: %v", ErrPromotionRunCreateFailed, err)
	}
	return runID, nil
}

// defaultPromotionTitle builds `Discussion promotion — {project.name}
// {YYYY-MM-DD HH:mm}` (SDD §4.6); create branch only.
func (s *IssueService) defaultPromotionTitle(ctx context.Context, qtx *db.Queries, workspaceID, projectID pgtype.UUID) (string, error) {
	project, err := qtx.GetProjectInWorkspace(ctx, db.GetProjectInWorkspaceParams{
		ID:          projectID,
		WorkspaceID: workspaceID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrProjectNotFound
		}
		return "", fmt.Errorf("load project for promotion title: %w", err)
	}
	return fmt.Sprintf("Discussion promotion — %s %s", project.Title, time.Now().Format("2006-01-02 15:04")), nil
}

// buildDefaultPromotionDescription renders the source-message digest plus a
// source annotation block (SDD §4.6); create branch only. Messages are read
// inside the promotion transaction (read-only; FR-5 zero chat writes).
func (s *IssueService) buildDefaultPromotionDescription(ctx context.Context, qtx *db.Queries, p PromoteDiscussionParams) pgtype.Text {
	messages := make([]db.ChatMessage, 0, len(p.MessageIDs))
	for _, id := range p.MessageIDs {
		if !id.Valid {
			continue
		}
		m, err := qtx.GetChatMessageInWorkspace(ctx, db.GetChatMessageInWorkspaceParams{
			ID:          id,
			WorkspaceID: p.WorkspaceID,
		})
		if err != nil {
			continue // digest is best-effort; missing rows drop out
		}
		messages = append(messages, m)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].CreatedAt.Time.Before(messages[j].CreatedAt.Time) })

	var b strings.Builder
	shown := 0
	for _, m := range messages {
		if shown >= promotionSummaryMaxMessages {
			break
		}
		author := m.Role
		if s.TaskService != nil {
			author = chatMessageAuthorDisplayName(ctx, s.TaskService, m)
		}
		b.WriteString(author)
		b.WriteString(": ")
		b.WriteString(summarizePromotionContent(m.Content, promotionSummaryRuneLimit))
		b.WriteString("\n")
		shown++
	}
	if len(messages) > promotionSummaryMaxMessages {
		b.WriteString("…\n")
	}

	b.WriteString("\n来源：Discussion session ")
	b.WriteString(util.UUIDToString(p.SessionID))
	b.WriteString(" · ")
	b.WriteString(strconv.Itoa(len(p.MessageIDs)))
	b.WriteString(" 条消息 · ")
	b.WriteString(strconv.Itoa(len(p.AttachmentIDs)))
	b.WriteString(" 个附件")
	return pgtype.Text{String: b.String(), Valid: true}
}

// summarizePromotionContent flattens whitespace and truncates to runeLimit
// runes with an ellipsis (SDD §4.6).
func summarizePromotionContent(content string, runeLimit int) string {
	flat := strings.Join(strings.Fields(content), " ")
	runes := []rune(flat)
	if len(runes) <= runeLimit {
		return flat
	}
	return string(runes[:runeLimit]) + "…"
}

// PromoteDiscussion is the full promotion flow (SDD §4.3 steps 7–12): one
// transaction holding the project-level advisory lock, in order — member
// re-check → idempotency reservation (four branches) → dedupe lookup →
// create (via createInTx) or backfill branch → finalize → commit. The
// handler performs the zero-write pre-steps 1–6 in the fixed PRD order.
func (s *IssueService) PromoteDiscussion(ctx context.Context, p PromoteDiscussionParams) (PromotionResult, error) {
	// Defensive boundary re-checks (SDD §7.3): the handler enforces these
	// before any lock; re-asserting keeps the service safe for non-HTTP
	// callers. Both failures happen before any write.
	if len(p.MessageIDs) > promotionMaxItems || len(p.AttachmentIDs) > promotionMaxItems {
		return PromotionResult{}, ErrInvalidPromotionSelection
	}
	if len(p.MessageIDs) == 0 && len(p.AttachmentIDs) == 0 {
		return PromotionResult{}, ErrInvalidPromotionSelection
	}
	if !p.WorkspaceID.Valid || !p.ProjectID.Valid || !p.SessionID.Valid || !p.CallerID.Valid {
		return PromotionResult{}, ErrInvalidPromotionSelection
	}
	if p.IdempotencyKey == "" {
		return PromotionResult{}, ErrInvalidPromotionSelection
	}

	fingerprint := promotionFingerprint(p.ProjectID, p.SessionID, p.MessageIDs, p.AttachmentIDs, p.UpgradeToCR)
	dedupeKey := promotionDedupeKey(p.SessionID, p.MessageIDs, p.AttachmentIDs)
	issueCountPolicy := ResolveIssueCountPolicy(ctx, s.Entitlements, p.WorkspaceID)

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return PromotionResult{}, fmt.Errorf("begin promotion tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	// Step 7 (D-4): project-level advisory lock — same-project promotions
	// serialize; message sends use a different prefix and never queue.
	if err := qtx.LockIssueDuplicateKey(ctx, promotionAdvisoryKey(p.WorkspaceID, p.ProjectID)); err != nil {
		return PromotionResult{}, fmt.Errorf("lock promotion key: %w", err)
	}

	// Step 8: member re-check inside the lock, before the first write
	// (SDD §13.4) — serializes with concurrent member revocation.
	if _, err := qtx.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      p.CallerID,
		WorkspaceID: p.WorkspaceID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PromotionResult{}, ErrPromotionForbidden
		}
		return PromotionResult{}, fmt.Errorf("re-check promotion member: %w", err)
	}

	// Step 9: idempotency reservation, four branches (SDD §4.3/FR-7).
	_, err = qtx.InsertChatIdempotencyReservation(ctx, db.InsertChatIdempotencyReservationParams{
		WorkspaceID: p.WorkspaceID,
		UserID:      p.CallerID,
		ScopeType:   promotionIdempotencyScope,
		ScopeID:     p.ProjectID,
		Key:         p.IdempotencyKey,
		Fingerprint: fingerprint,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		winner, werr := qtx.GetChatIdempotencyByKey(ctx, db.GetChatIdempotencyByKeyParams{
			WorkspaceID: p.WorkspaceID,
			UserID:      p.CallerID,
			ScopeType:   promotionIdempotencyScope,
			ScopeID:     p.ProjectID,
			Key:         p.IdempotencyKey,
		})
		if werr != nil {
			if errors.Is(werr, pgx.ErrNoRows) {
				// Winner rolled back between the conflict and the read; the
				// key is free again — proceed as the holder.
			} else {
				return PromotionResult{}, fmt.Errorf("load promotion idempotency winner: %w", werr)
			}
		} else if winner.Fingerprint != fingerprint {
			// Same key, different payload → fixed 409, zero writes; this
			// branch deliberately precedes the dedupe lookup (PRD priority).
			return PromotionResult{}, ErrIdempotencyKeyReused
		} else if winner.ResponseBody != nil {
			// Replay: return the stored first-attempt response; nothing was
			// written in this transaction. PRD FR-7 / SDD §3.1: a replay
			// echoes the stored body verbatim EXCEPT created, which is always
			// false — the row was created by the first attempt.
			var replayed PromotionResult
			if uerr := json.Unmarshal(winner.ResponseBody, &replayed); uerr != nil {
				return PromotionResult{}, fmt.Errorf("decode promotion replay body: %w", uerr)
			}
			replayed.Created = false
			return replayed, nil
		}
		// Same fingerprint + NULL body: the previous execution was
		// interrupted after reserving; this request takes over and finalizes
		// the winner row.
	} else if err != nil {
		return PromotionResult{}, fmt.Errorf("reserve promotion idempotency: %w", err)
	}

	// Step 10: dedupe lookup → create or backfill branch.
	var issue db.Issue
	var created bool
	var runID pgtype.UUID
	refs := PromotionSourceRefs{
		SessionID:     p.SessionID,
		MessageIDs:    canonicalUUIDSlice(p.MessageIDs),
		AttachmentIDs: canonicalUUIDSlice(p.AttachmentIDs),
	}

	dupIssue, derr := qtx.FindPromotionDuplicateIssue(ctx, db.FindPromotionDuplicateIssueParams{
		WorkspaceID: p.WorkspaceID,
		DedupeKey:   dedupeKey,
	})
	switch {
	case errors.Is(derr, pgx.ErrNoRows):
		// Create branch (SDD §4.3): default title/description apply here
		// only; dedupe hits and replays never overwrite (FR-6).
		created = true
		if p.UpgradeToCR {
			runID = dbid.NewV7()
		}
		title := ""
		if p.Title != nil {
			title = *p.Title
		} else {
			title, err = s.defaultPromotionTitle(ctx, qtx, p.WorkspaceID, p.ProjectID)
			if err != nil {
				return PromotionResult{}, err
			}
		}
		description := pgtype.Text{}
		if p.Description != nil {
			description = pgtype.Text{String: *p.Description, Valid: true}
		} else {
			description = s.buildDefaultPromotionDescription(ctx, qtx, p)
		}

		entry := buildPromotionEntry(p, dedupeKey, runID, time.Now())
		entryJSON, jerr := json.Marshal([]promotionContextRefEntry{entry})
		if jerr != nil {
			return PromotionResult{}, fmt.Errorf("encode promotion context refs: %w", jerr)
		}

		var plan *PromotionRunPlan
		if p.UpgradeToCR {
			plan, err = s.buildPromotionRunPlan(p, runID)
			if err != nil {
				return PromotionResult{}, err
			}
		}

		res, cerr := s.createInTx(ctx, tx, qtx, IssueCreateParams{
			WorkspaceID:    p.WorkspaceID,
			Title:          title,
			Description:    description,
			Status:         promotionCreatedStatus,
			Priority:       promotionCreatedPriority,
			AssigneeType:   pgtype.Text{},
			AssigneeID:     pgtype.UUID{},
			CreatorType:    "member",
			CreatorID:      p.CallerID,
			ProjectID:      p.ProjectID,
			AllowDuplicate: true,
			ContextRefs:    entryJSON,
			PromotionRun:   plan,
		}, IssueCreateOpts{}, issueCountPolicy)
		if cerr != nil {
			return PromotionResult{}, cerr
		}
		issue = res.issue

	case derr != nil:
		return PromotionResult{}, fmt.Errorf("find promotion duplicate: %w", derr)

	default:
		// Dedupe-hit branch (SDD §4.3): source refs come from the matched
		// entry; the upgrade backfill merges pipeline_run_id into the SAME
		// entry in this transaction.
		issue = dupIssue
		entries, perr := parsePromotionContextRefs(dupIssue.ContextRefs)
		if perr != nil {
			return PromotionResult{}, perr
		}
		entry, idx, ok := findPromotionEntry(entries, dedupeKey)
		if !ok {
			return PromotionResult{}, fmt.Errorf("dedupe hit for key %q but entry missing from context_refs", dedupeKey)
		}
		erefs, eerr := entrySourceRefs(entry)
		if eerr != nil {
			return PromotionResult{}, eerr
		}
		refs = erefs

		if p.UpgradeToCR {
			if entry.PipelineRunID != "" {
				rid, perr := parsePromotionUUID(entry.PipelineRunID)
				if perr != nil {
					return PromotionResult{}, fmt.Errorf("entry pipeline_run_id: %w", perr)
				}
				runID = rid
			} else {
				// Backfill: no run yet — create one and merge its id into
				// the entry (SDD §4.3/§4.5: backfill and bind can never
				// interleave because a bindable run implies the entry
				// already carried pipeline_run_id).
				plan, perr := s.buildPromotionRunPlan(p, dbid.NewV7())
				if perr != nil {
					return PromotionResult{}, perr
				}
				adopted, aerr := insertPromotionRun(ctx, qtx, p.WorkspaceID, issue.ID, plan.RunID, p.CallerID, plan)
				if aerr != nil {
					return PromotionResult{}, aerr
				}
				runID = adopted
				entry.PipelineRunID = util.UUIDToString(runID)
				entries[idx] = entry
				merged, merr := json.Marshal(entries)
				if merr != nil {
					return PromotionResult{}, fmt.Errorf("encode merged promotion context refs: %w", merr)
				}
				if uerr := qtx.SetIssueContextRefPipelineRun(ctx, db.SetIssueContextRefPipelineRunParams{
					ID:          issue.ID,
					ContextRefs: merged,
				}); uerr != nil {
					return PromotionResult{}, fmt.Errorf("set promotion pipeline run ref: %w", uerr)
				}
			}
		}
	}

	// Step 11: assemble the response and finalize the idempotency row in the
	// same transaction (SDD §4.3).
	result := PromotionResult{
		IssueID:     issue.ID,
		IssueNumber: issue.Number,
		SessionID:   p.SessionID,
		SourceRefs: promotionResultSourceRefs{
			SessionID:     refs.SessionID,
			MessageIDs:    refs.MessageIDs,
			AttachmentIDs: refs.AttachmentIDs,
		},
		Created:     created,
		UpgradeToCR: p.UpgradeToCR,
		RunID:       runID,
	}
	body, err := json.Marshal(result)
	if err != nil {
		return PromotionResult{}, fmt.Errorf("encode promotion response: %w", err)
	}
	updated, ferr := qtx.FinalizeChatIdempotency(ctx, db.FinalizeChatIdempotencyParams{
		WorkspaceID:    p.WorkspaceID,
		UserID:         p.CallerID,
		ScopeType:      promotionIdempotencyScope,
		ScopeID:        p.ProjectID,
		Key:            p.IdempotencyKey,
		ResponseStatus: 201,
		ResponseBody:   body,
	})
	if ferr != nil || updated != 1 {
		return PromotionResult{}, fmt.Errorf("finalize promotion idempotency: rows=%d err=%v", updated, ferr)
	}

	// Step 12: commit; post-commit events fire only for the create branch
	// (SDD §4.3 / SDD-CLOSE-05 — promotion introduces no new event type).
	if err := tx.Commit(ctx); err != nil {
		return PromotionResult{}, fmt.Errorf("commit promotion: %w", err)
	}
	if created {
		actorID := util.UUIDToString(issue.CreatorID)
		s.publishIssueCreated(issue, nil, nil, "member", actorID, IssueCreateOpts{})
		s.captureCreatedAnalytics(issue, "member", actorID, IssueCreateOpts{})
	}
	return result, nil
}
