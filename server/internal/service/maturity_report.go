package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/maturity"
)

// Weekly report envelope (SDD §3.5/§3.7): the daemon writes the markdown file
// and hands the structured envelope back through the task result; the server
// only verifies the SHA and persists the projection. The server never reads
// the daemon filesystem.

var weekPattern = regexp.MustCompile(`^\d{4}-W\d{2}$`)

// BuildReportEnvelope validates the week and computes the envelope fields:
// report_key (workspace:week), content SHA-256 and the canonical relative
// path. The caller stores it in agent_task_queue.result.
func BuildReportEnvelope(
	workspaceID pgtype.UUID,
	week string,
	markdown []byte,
	taskID, chatSessionID pgtype.UUID,
	configRevs []string,
) (maturity.MaturityReport, error) {
	if !workspaceID.Valid {
		return maturity.MaturityReport{}, fmt.Errorf("report envelope: workspace id required")
	}
	if !weekPattern.MatchString(week) {
		return maturity.MaturityReport{}, fmt.Errorf("report envelope: week must be YYYY-Www, got %q", week)
	}
	if len(markdown) == 0 {
		return maturity.MaturityReport{}, fmt.Errorf("report envelope: markdown is empty")
	}
	sum := sha256.Sum256(markdown)
	return maturity.MaturityReport{
		Schema:        "ai-first.maturity-report/v1",
		ReportKey:     uuid.UUID(workspaceID.Bytes).String() + ":" + week,
		Week:          week,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		RelativePath:  fmt.Sprintf("docs/org-admin/maturity-review-%s.md", week),
		Markdown:      string(markdown),
		ContentSha256: hex.EncodeToString(sum[:]),
		SourceTaskID:  uuidStringOrEmpty(taskID),
		ChatSessionID: uuidStringOrEmpty(chatSessionID),
		ConfigRevs:    configRevs,
	}, nil
}

// VerifyReportSHA checks a report envelope body against its committed digest.
func VerifyReportSHA(markdown []byte, contentSHA256 string) bool {
	sum := sha256.Sum256(markdown)
	return hex.EncodeToString(sum[:]) == contentSHA256
}

func uuidStringOrEmpty(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}
