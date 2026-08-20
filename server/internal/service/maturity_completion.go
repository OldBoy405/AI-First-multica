package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/maturity"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const maturityReportSchema = "ai-first.maturity-report/v1"

// persistMaturityReportCompletion canonicalizes the daemon output to the
// direct report envelope indexed by migration 379 and writes the Owner inbox
// item in the same transaction as task completion. Any validation/notification
// error rolls the completion back so the daemon's terminal retry can recover.
func persistMaturityReportCompletion(
	ctx context.Context,
	qtx *db.Queries,
	task db.AgentTaskQueue,
	result []byte,
) ([]byte, error) {
	// Direct follow-up turns run on the same Org Admin agent and chat but are
	// ordinary conversations, not scheduled report projections.
	if !task.AutopilotRunID.Valid {
		return result, nil
	}
	agent, err := qtx.GetAgent(ctx, task.AgentID)
	if err != nil {
		return nil, fmt.Errorf("maturity report completion: load agent: %w", err)
	}
	if !agent.SystemKey.Valid || agent.SystemKey.String != orgAdminAgentKey {
		return result, nil
	}
	if !task.ProjectID.Valid || !task.ChatSessionID.Valid {
		return nil, fmt.Errorf("maturity report completion: project and chat session are required")
	}
	projectID, err := qtx.MaturityOrgAdminProjectID(ctx, agent.WorkspaceID)
	if err != nil || !projectID.Valid || projectID.Bytes != task.ProjectID.Bytes {
		return nil, fmt.Errorf("maturity report completion: task is not bound to the Org Admin project")
	}

	report, err := decodeMaturityReportResult(result)
	if err != nil {
		return nil, err
	}
	if err := validateMaturityReport(report, agent.WorkspaceID, task.ID, task.ChatSessionID); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("maturity report completion: encode envelope: %w", err)
	}
	details, err := json.Marshal(map[string]string{
		"report_key": report.ReportKey, "relative_path": report.RelativePath,
		"chat_session_id": report.ChatSessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("maturity report completion: encode inbox details: %w", err)
	}
	inboxExists, err := qtx.MaturityReportInboxExistsLocked(ctx, db.MaturityReportInboxExistsLockedParams{
		WorkspaceID: agent.WorkspaceID, RecipientID: agent.OwnerID, ReportKey: report.ReportKey,
	})
	if err != nil {
		return nil, fmt.Errorf("maturity report completion: lock Owner inbox item: %w", err)
	}
	if !inboxExists {
		if _, err := qtx.CreateInboxItem(ctx, db.CreateInboxItemParams{
			WorkspaceID: agent.WorkspaceID, RecipientType: "member", RecipientID: agent.OwnerID,
			Type: "maturity_report_ready", Severity: "info",
			Title:     "AI maturity report ready: " + report.Week,
			Body:      pgtype.Text{String: "Open the Org Admin report or continue the Team Agent conversation.", Valid: true},
			ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: agent.ID,
			Details: details,
		}); err != nil {
			return nil, fmt.Errorf("maturity report completion: create Owner inbox item: %w", err)
		}
	}
	return canonical, nil
}

func decodeMaturityReportResult(result []byte) (maturity.MaturityReport, error) {
	var payload protocol.TaskCompletedPayload
	if err := json.Unmarshal(result, &payload); err == nil && strings.TrimSpace(payload.Output) != "" {
		result = []byte(payload.Output)
	}
	var report maturity.MaturityReport
	if err := json.Unmarshal(result, &report); err != nil {
		return report, fmt.Errorf("maturity report completion: output must be a JSON envelope: %w", err)
	}
	return report, nil
}

func validateMaturityReport(report maturity.MaturityReport, workspaceID, taskID, chatSessionID pgtype.UUID) error {
	expected, err := BuildReportEnvelope(
		workspaceID, report.Week, []byte(report.Markdown), taskID, chatSessionID, report.ConfigRevs,
	)
	if err != nil {
		return fmt.Errorf("maturity report completion: %w", err)
	}
	if report.Schema != expected.Schema || report.ReportKey != expected.ReportKey ||
		report.RelativePath != expected.RelativePath || report.ContentSha256 != expected.ContentSha256 {
		return fmt.Errorf("maturity report completion: canonical envelope fields do not match")
	}
	if report.SourceTaskID != expected.SourceTaskID || report.ChatSessionID != expected.ChatSessionID {
		return fmt.Errorf("maturity report completion: task/chat binding mismatch")
	}
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		return fmt.Errorf("maturity report completion: generated_at must be RFC3339: %w", err)
	}
	if !VerifyReportSHA([]byte(report.Markdown), report.ContentSha256) {
		return fmt.Errorf("maturity report completion: content SHA mismatch")
	}
	for _, heading := range []string{
		"## Individual efficiency", "## Team delivery", "## Knowledge compounding",
		"## Risk & yield", "## Cost",
	} {
		if !strings.Contains(report.Markdown, heading) {
			return fmt.Errorf("maturity report completion: missing section %q", heading)
		}
	}
	if len(report.ConfigRevs) == 0 {
		return fmt.Errorf("maturity report completion: config_revs is empty")
	}
	return nil
}
