package lark

// AIFIRST: CR-2026-051 FR-8 — approval reminder: unconditional subscribe,
// zero-I/O sync callback, bounded async delivery chain. Logs follow SDD §4.6
// (four result kinds + a three-value diagnostic phase set outside FR-8.2).

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Skip reasons: the PRD FR-8.2 closed set of nine. Never a tenth.
const (
	reasonProjectUnresolved   = "project-unresolved"
	reasonWorkspaceMismatch   = "workspace-mismatch"
	reasonNoApprover          = "no-approver"
	reasonBindingMissing      = "binding-missing"
	reasonInstallationRevoked = "installation-revoked"
	reasonInstallationMissing = "installation-missing"
	reasonAppURLMissing       = "app-url-missing"
	reasonFeishuDisabled      = "feishu-disabled"
	reasonOverloaded          = "overloaded"
)

// Error classes (PRD §4.4): the closed four.
const (
	errorClassTimeout       = "timeout"
	errorClassRateLimited   = "rate-limited"
	errorClassNotConfigured = "not-configured"
	errorClassOther         = "other"
)

// Delivery steps (SDD §4.6): closed set; never carries response bodies,
// tokens, credentials, or diffs.
const (
	stepProjectChain      = "project-chain"
	stepApproverQuery     = "approver-query"
	stepBindingQuery      = "binding-query"
	stepCredentialHydrate = "credential-hydrate"
	stepCredentialDecrypt = "credential-decrypt"
	stepSend              = "send"
)

// Diagnostic phase values: the complete closed set of non-result Error-level
// lines (counted per phase). Diagnostic, not a result classification.
const (
	phaseConstruct      = "construct"
	phaseRegister       = "register"
	phasePanicRecovered = "panic-recovered"
)

// approvalGateStageLabels maps the four human-approval gate statuses to card
// display names (SDD §4.3). Unexported, initialized once, read-only at
// runtime; the static assertion in approval_reminder_test.go guards that.
// stageLabel falls back to the raw status for unknown values.
var approvalGateStageLabels = map[string]string{
	"requirement-reviewing":      "需求审批",
	"tech-design-review-pending": "架构审批",
	"task-breakdown":             "开发启动审批",
	"code-reviewing":             "代码审批",
}

func stageLabel(status string) string {
	if label, ok := approvalGateStageLabels[status]; ok {
		return label
	}
	return status
}

// installationCredentialSource is the narrow slice of InstallationService the
// reminder needs; the concrete *InstallationService satisfies it as-is.
type installationCredentialSource interface {
	CredentialsResolver // DecryptAppSecret(inst Installation) (string, error)
	GetInWorkspace(ctx context.Context, id, workspaceID pgtype.UUID) (Installation, error)
}

// ApprovalReminderConfig wires the reminder's dependencies. Everything is
// constructor-injected; the only package-level state is approvalGateStageLabels.
type ApprovalReminderConfig struct {
	Pool                           *pgxpool.Pool
	Client                         APIClient
	Credentials                    installationCredentialSource
	AppURL                         string
	Logger                         *slog.Logger
	MaxInFlight                    int
	EventTimeout, RecipientTimeout time.Duration
}

type ApprovalReminder struct {
	pool             *pgxpool.Pool
	client           APIClient
	credentials      installationCredentialSource
	appURL           string
	logger           *slog.Logger
	sem              chan struct{}
	eventTimeout     time.Duration
	recipientTimeout time.Duration
}

// NewApprovalReminder never returns nil. Missing dependencies produce one
// phase=construct Error (missing = pool,client,credentials joined in that
// fixed order, no result field) and a degraded object that later logs
// feishu-disabled skips. Zero values degrade only at three documented points
// (MaxInFlight→8, EventTimeout→60s, RecipientTimeout→10s); explicit non-zero
// values always take effect (CONTRIBUTING.AIFIRST rule 6). Logger→slog.Default.
func NewApprovalReminder(cfg ApprovalReminderConfig) *ApprovalReminder {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var missing []string
	if cfg.Pool == nil {
		missing = append(missing, "pool")
	}
	if cfg.Client == nil {
		missing = append(missing, "client")
	}
	if cfg.Credentials == nil {
		missing = append(missing, "credentials")
	}
	if len(missing) > 0 {
		logger.Error("approval reminder: dependency missing",
			"phase", phaseConstruct, "missing", strings.Join(missing, ","))
	}
	maxInFlight, eventTimeout, recipientTimeout := cfg.MaxInFlight, cfg.EventTimeout, cfg.RecipientTimeout
	if maxInFlight == 0 {
		maxInFlight = 8
	}
	if eventTimeout == 0 {
		eventTimeout = 60 * time.Second
	}
	if recipientTimeout == 0 {
		recipientTimeout = 10 * time.Second
	}
	return &ApprovalReminder{
		pool: cfg.Pool, client: cfg.Client, credentials: cfg.Credentials,
		appURL: cfg.AppURL, logger: logger,
		sem:          make(chan struct{}, maxInFlight),
		eventTimeout: eventTimeout, recipientTimeout: recipientTimeout,
	}
}

// Register subscribes unconditionally (FR-8.3): an unconfigured Lark
// deployment must still consume the event to emit the feishu-disabled skip.
// nil bus → one phase=register Error (missing=bus) and return, no panic.
func (r *ApprovalReminder) Register(bus *events.Bus) {
	if bus == nil {
		r.logger.Error("approval reminder: cannot register",
			"phase", phaseRegister, "missing", "bus")
		return
	}
	bus.Subscribe(protocol.EventCRApprovalGateEntered, r.handleEvent)
}

// parsePayload asserts the real protocol type (never a map from a forged
// producer) and complete locating fields.
func parsePayload(v any) (protocol.ApprovalGateEnteredPayload, bool) {
	p, ok := v.(protocol.ApprovalGateEnteredPayload)
	if !ok || p.CRID == "" || p.Status == "" || p.EventID == "" {
		return protocol.ApprovalGateEnteredPayload{}, false
	}
	return p, true
}

// handleEvent is the synchronous bus callback: zero I/O.
func (r *ApprovalReminder) handleEvent(e events.Event) {
	p, ok := parsePayload(e.Payload)
	if !ok {
		r.logger.Warn("approval reminder: bad gate-event payload")
		return
	}
	if _, ok := approvalGateStageLabels[p.Status]; !ok {
		return // defensive: not one of the four gates
	}
	if e.WorkspaceID == "" {
		r.logger.Warn("approval reminder: event without workspace anchor")
		return
	}
	select {
	case r.sem <- struct{}{}:
		go r.deliver(p, e.WorkspaceID)
	default:
		r.logSkipEvent(p, e.WorkspaceID, reasonOverloaded)
	}
}

// deliver runs the async body: self-owned recover (the bus recover does not
// cover spawned goroutines), semaphore release, and a fresh timeout context.
func (r *ApprovalReminder) deliver(p protocol.ApprovalGateEnteredPayload, anchorWorkspaceID string) {
	defer func() {
		if v := recover(); v != nil {
			r.logger.Error("approval reminder: panic in deliver",
				"phase", phasePanicRecovered,
				"cr_id", p.CRID, "stage", p.Status,
				"workspace_id", anchorWorkspaceID, "event_id", p.EventID,
				"recovered", v)
		}
	}()
	defer func() { <-r.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), r.eventTimeout)
	defer cancel()

	// Lark availability first, CTA base URL second — both zero-DB.
	if r.pool == nil || r.client == nil || !r.client.IsConfigured() || r.credentials == nil {
		r.logSkipEvent(p, anchorWorkspaceID, reasonFeishuDisabled)
		return
	}
	if r.appURL == "" {
		r.logSkipEvent(p, anchorWorkspaceID, reasonAppURLMissing)
		return
	}

	r.deliverToRecipients(ctx, p, anchorWorkspaceID)
}

// deliverToRecipients is the fail-closed read chain (SDD §4.2). Every hop
// carries the DaemonAuth anchor; the payload's ShellIssueID is never a query
// input (BL-3). DB errors are result=failed, never a skip reason.
func (r *ApprovalReminder) deliverToRecipients(ctx context.Context, p protocol.ApprovalGateEnteredPayload, anchorWorkspaceID string) {
	// AIFIRST: CR-2026-051 FR-3 — project chain: cr → issue → project →
	// workspace.slug, one round-trip, every hop carries the anchor (INNER
	// JOIN = fail-closed). Deliberately NOT the project_gates.go join shape
	// (primary-key join cannot detect a cross-workspace shell_issue_id).
	var projectID, slug, title string
	err := r.pool.QueryRow(ctx, `SELECT p.id::text, w.slug, c.title FROM cr c JOIN issue i ON i.id = c.shell_issue_id AND i.workspace_id = $1 JOIN project p ON p.id = i.project_id AND p.workspace_id = $1 JOIN workspace w ON w.id = $1 WHERE c.workspace_id = $1 AND c.cr_id = $2`,
		anchorWorkspaceID, p.CRID).Scan(&projectID, &slug, &title)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			r.logFailEvent(p, anchorWorkspaceID, stepProjectChain, errorClassOf(err))
			return
		}
		// Zero rows: pick the reason with a second anchored query
		// (reason-selection only — never produces recipients).
		var shellNull bool
		if serr := r.pool.QueryRow(ctx, `SELECT shell_issue_id IS NULL FROM cr WHERE workspace_id = $1 AND cr_id = $2`,
			anchorWorkspaceID, p.CRID).Scan(&shellNull); serr != nil && !errors.Is(serr, pgx.ErrNoRows) {
			r.logFailEvent(p, anchorWorkspaceID, stepProjectChain, errorClassOf(serr))
			return
		}
		if shellNull {
			r.logSkipEvent(p, anchorWorkspaceID, reasonProjectUnresolved)
		} else {
			r.logSkipEvent(p, anchorWorkspaceID, reasonWorkspaceMismatch)
		}
		return
	}
	if slug == "" {
		r.logSkipEvent(p, anchorWorkspaceID, reasonWorkspaceMismatch)
		return
	}

	// AC-4 case ①'s reason carrier: the approver set comes exclusively from
	// this role-filtered query (FR-4) — non-owner/admin members never enter
	// the loop, so no recipient-level record exists for them.
	rows, err := r.pool.Query(ctx, `SELECT user_id::text FROM member WHERE workspace_id = $1 AND role IN ('owner','admin')`,
		anchorWorkspaceID)
	if err != nil {
		r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
		return
	}
	var approvers []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
			return
		}
		approvers = append(approvers, userID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		r.logFailEvent(p, anchorWorkspaceID, stepApproverQuery, errorClassOf(err))
		return
	}
	if len(approvers) == 0 {
		r.logSkipEvent(p, anchorWorkspaceID, reasonNoApprover)
		return
	}

	approveURL := r.appURL + "/" + slug + "/projects/" + projectID + "?tab=chat"

	// attemptedOpenIDs means "already attempted", not "already sent" (BL-2):
	// registration happens before any fallible action, so a first failure
	// never lets a later recipient retry the same open_id.
	attemptedOpenIDs := map[string]struct{}{}
	for _, userID := range approvers {
		// AIFIRST: CR-2026-051 FR-5 — effective-binding candidates for one
		// member. LEFT JOIN (not INNER) so a dangling installation_id
		// (channel_* has NO foreign keys) stays observable as
		// installation-missing; ci.* is fetched but not filtered here —
		// chooseEffective filters so the failure modes stay distinguishable.
		brows, err := r.pool.Query(ctx, `SELECT b.id::text, b.channel_user_id, b.installation_id::text, ci.id::text, ci.workspace_id::text, ci.channel_type, ci.status FROM channel_user_binding b LEFT JOIN channel_installation ci ON ci.id = b.installation_id WHERE b.workspace_id = $1 AND b.multica_user_id = $2 AND b.channel_type = 'feishu' ORDER BY b.bound_at DESC, b.id ASC`,
			anchorWorkspaceID, userID)
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
			continue
		}
		var candidates []approvalBindingCandidate
		bindOK := true
		for brows.Next() {
			var c approvalBindingCandidate
			if err := brows.Scan(&c.BindingID, &c.OpenID, &c.InstallationID, &c.InstID,
				&c.InstWorkspaceID, &c.InstChannelType, &c.InstStatus); err != nil {
				bindOK = false
				r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
				break
			}
			candidates = append(candidates, c)
		}
		brows.Close()
		if !bindOK {
			continue
		}
		if err := brows.Err(); err != nil {
			r.logFail(p, anchorWorkspaceID, userID, "", stepBindingQuery, errorClassOf(err))
			continue
		}
		if len(candidates) == 0 {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonBindingMissing)
			continue
		}
		pick, reason := chooseEffective(candidates, anchorWorkspaceID)
		if pick == nil {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reason)
			continue
		}

		// BL-2: register the attempt before any fallible action.
		if _, dup := attemptedOpenIDs[string(pick.OpenID)]; dup {
			continue // same open_id: one attempt per event (FR-5/AC-3)
		}
		attemptedOpenIDs[string(pick.OpenID)] = struct{}{}

		// BL-1: workspace-scoped full installation read; config JSONB is
		// decoded upstream (installationFromRow), never by raw SQL here.
		installationUUID, err := util.ParseUUID(*pick.InstallationID)
		anchorUUID, uerr := util.ParseUUID(anchorWorkspaceID)
		if err != nil || uerr != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialHydrate, errorClassOf(errors.Join(err, uerr)))
			continue
		}
		inst, err := r.credentials.GetInWorkspace(ctx, installationUUID, anchorUUID)
		if err != nil {
			if errors.Is(err, ErrInstallationNotFound) {
				r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonInstallationMissing)
			} else {
				r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialHydrate, errorClassOf(err))
			}
			continue
		}
		// Re-check tenant/status after hydration (TOCTOU window between
		// classification and send); hydration wins on disagreement.
		if util.UUIDToString(inst.WorkspaceID) != anchorWorkspaceID {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonWorkspaceMismatch)
			continue
		}
		if inst.Status != "active" {
			r.logSkipRecipient(p, anchorWorkspaceID, userID, reasonInstallationRevoked)
			continue
		}
		creds, err := installationCredentialsFor(inst, r.credentials)
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepCredentialDecrypt, errorClassOf(err))
			continue
		}

		rctx, rcancel := context.WithTimeout(ctx, r.recipientTimeout)
		err = r.client.SendApprovalReminderCard(rctx, ApprovalReminderParams{
			InstallationID: creds, OpenID: pick.OpenID,
			CRID: p.CRID, CRTitle: title, StageLabel: stageLabel(p.Status), ApproveURL: approveURL,
		})
		rcancel()
		if err != nil {
			r.logFail(p, anchorWorkspaceID, userID, pick.OpenID, stepSend, errorClassOf(err))
			continue
		}
		r.logSent(p, anchorWorkspaceID, userID, pick.OpenID)
	}
}

// approvalBindingCandidate: one candidate row from the binding query;
// nullable installation columns stay pointers so chooseEffective can tell
// missing / revoked / mismatched apart.
type approvalBindingCandidate struct {
	BindingID       string
	OpenID          OpenID
	InstallationID  *string
	InstID          *string
	InstWorkspaceID *string
	InstChannelType *string
	InstStatus      *string
}

// chooseEffective returns the first fully-valid candidate in SQL order, else
// nil + the most-specific reason: workspace-mismatch > installation-revoked >
// installation-missing > binding-missing (fallback for a non-empty set).
func chooseEffective(rows []approvalBindingCandidate, anchorWorkspaceID string) (*approvalBindingCandidate, string) {
	seenMissing, seenMismatch, seenRevoked := false, false, false
	for i := range rows {
		row := &rows[i]
		if row.InstallationID == nil || row.InstID == nil {
			seenMissing = true
			continue
		}
		if row.InstWorkspaceID == nil || *row.InstWorkspaceID != anchorWorkspaceID ||
			row.InstChannelType == nil || *row.InstChannelType != "feishu" {
			seenMismatch = true
			continue
		}
		if row.InstStatus == nil || *row.InstStatus != "active" {
			seenRevoked = true
			continue
		}
		return row, ""
	}
	switch {
	case seenMismatch:
		return nil, reasonWorkspaceMismatch
	case seenRevoked:
		return nil, reasonInstallationRevoked
	case seenMissing:
		return nil, reasonInstallationMissing
	default:
		return nil, reasonBindingMissing
	}
}

// errorClassOf classifies into the four closed error_class values (never
// logs response bodies, tokens, credentials, or diffs).
func errorClassOf(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errorClassTimeout
	case errors.Is(err, ErrAPIClientNotConfigured):
		return errorClassNotConfigured
	case isRateLimitError(err):
		return errorClassRateLimited
	default:
		return errorClassOther
	}
}

func isRateLimitError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 99991400 || apiErr.Code == 230020
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate_limit")
}

// Result log entries (SDD §4.6). stage is always the raw CR status literal,
// never the card label. Event-level failures carry no recipient fields.
func (r *ApprovalReminder) logEventBase(p protocol.ApprovalGateEnteredPayload, workspaceID string) []any {
	return []any{"cr_id", p.CRID, "stage", p.Status, "workspace_id", workspaceID, "event_id", p.EventID}
}

func (r *ApprovalReminder) logSent(p protocol.ApprovalGateEnteredPayload, workspaceID, userID string, openID OpenID) {
	r.logger.Info("approval reminder sent", append(r.logEventBase(p, workspaceID),
		"recipient_user_id", userID, "recipient_open_id", string(openID), "result", "sent")...)
}

func (r *ApprovalReminder) logFail(p protocol.ApprovalGateEnteredPayload, workspaceID, userID string, openID OpenID, step, errClass string) {
	attrs := append(r.logEventBase(p, workspaceID),
		"recipient_user_id", userID, "result", "failed", "error_class", errClass, "step", step)
	if openID != "" {
		attrs = append(attrs, "recipient_open_id", string(openID))
	}
	r.logger.Warn("approval reminder failed", attrs...)
}

func (r *ApprovalReminder) logFailEvent(p protocol.ApprovalGateEnteredPayload, workspaceID, step, errClass string) {
	r.logger.Warn("approval reminder failed", append(r.logEventBase(p, workspaceID),
		"result", "failed", "error_class", errClass, "step", step)...)
}

func (r *ApprovalReminder) logSkipEvent(p protocol.ApprovalGateEnteredPayload, workspaceID, reason string) {
	r.logger.Info("approval reminder skipped", append(r.logEventBase(p, workspaceID),
		"result", "skipped", "reason", reason)...)
}

func (r *ApprovalReminder) logSkipRecipient(p protocol.ApprovalGateEnteredPayload, workspaceID, userID, reason string) {
	r.logger.Info("approval reminder skipped", append(r.logEventBase(p, workspaceID),
		"recipient_user_id", userID, "result", "skipped", "reason", reason)...)
}
