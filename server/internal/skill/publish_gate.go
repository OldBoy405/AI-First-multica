// AIFIRST: CR-2026-048 TASK-04: org-publish gate for skills.
//
// Pure function surface: no DB, no services, no imports beyond redact and
// stdlib. The handler resolves the approved appeal set and the content hash;
// everything else lives here so it can be table-tested.
package skill

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/pkg/redact"
)

// Structured publish rejection reasons (422 `reasons` contract).
const (
	ReasonFrontmatterNameMissing        = "frontmatter_name_missing"
	ReasonFrontmatterDescriptionMissing = "frontmatter_description_missing"
	ReasonOwnerMissing                  = "owner_actor_missing"
)

func metadataFieldMissingReason(field string) string { return "metadata_" + field + "_missing" }

// WarningProtectedPaths is the only non-blocking warning.
const WarningProtectedPaths = "permission_declaration_touches_protected_paths"

// MetadataCardFields are the four required frontmatter keys for an org-visible
// skill (P3 design §3.3).
var MetadataCardFields = []string{
	"applicable-scenarios",
	"context-dependencies",
	"permission-declaration",
	"failure-handling",
}

// ProtectedPathPatterns mirrors tools skills/shared/controlled-shell/rules.json
// #protectedPaths (deny + ask, regex sources compiled case-insensitively).
// The server process has no tools checkout, so the list is a constant here and
// TestProtectedPathPatternsPin pins it against the tools package content.
var ProtectedPathPatterns = []string{
	`change-requests/_backlog\.ya?ml$`,
	`change-requests/_history\.ya?ml$`,
	`change-requests/[^/]+/cr\.md$`,
	`change-requests/[^/]+/approval\.ya?ml$`,
	`change-requests/[^/]+/review-loop\.ya?ml$`,
	`review-annotations/[^/]+\.ya?ml$`,
	`(^|/)specs/[^/]+/(PRD|SDD|traceability)\.(md|ya?ml)$`,
	`(^|/)delivery/`,
	`change-requests/[^/]+/test-report\.md$`,
}

var protectedPathRegexps = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(ProtectedPathPatterns))
	for _, src := range ProtectedPathPatterns {
		out = append(out, regexp.MustCompile(`(?i)`+src))
	}
	return out
}()

// GateFinding locates one secret hit with its file and the appeal id that an
// owner can approve. Excerpts are Text()-redacted, never plaintext.
type GateFinding struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	PatternID string `json:"pattern_id"`
	Excerpt   string `json:"excerpt"`
	AppealID  string `json:"appeal_id"`
}

// GateResult is the full publish verdict. Blocked() = any reason or finding.
type GateResult struct {
	Reasons  []string      `json:"reasons"`
	Findings []GateFinding `json:"findings"`
	Warnings []string      `json:"warnings"`
}

// Blocked reports whether the gate refuses the publish.
func (g GateResult) Blocked() bool { return len(g.Reasons) > 0 || len(g.Findings) > 0 }

// Release drops the findings an owner already approved (per-item release,
// never a whole-package pass). Callers scan once and release afterwards.
func (g GateResult) Release(approved func(appealID string) bool) GateResult {
	var kept []GateFinding
	for _, f := range g.Findings {
		if !approved(f.AppealID) {
			kept = append(kept, f)
		}
	}
	g.Findings = kept
	return g
}

// AppealID binds an appeal to the exact content hash of the skill so an
// approval can never outlive the content it was granted for: any content
// change yields a different hash and the old appeal stops matching.
func AppealID(skillRef, contentHash, file string, line int, patternID string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%s", skillRef, contentHash, file, line, patternID)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// EvaluatePublish runs every org-publish check against the effective content.
//
//   - content: effective SKILL.md body (request override or stored row).
//   - files: path -> content for all skill files (superset scan: existing
//     files plus replacements; zero bookkeeping).
//   - ownerActor: effective owner (request override or stored row).
//   - skillRef, contentHash: identity inputs for AppealID (contentHash comes
//     from skillbundle.BuildManifest(...).Hash, computed by the caller).
//
// Owner-approved findings are dropped afterwards via GateResult.Release.
func EvaluatePublish(content string, files map[string]string, ownerActor, skillRef, contentHash string) GateResult {
	var res GateResult

	meta := ParseSkillMetadata(content)
	if meta.Name == "" {
		res.Reasons = append(res.Reasons, ReasonFrontmatterNameMissing)
	}
	if meta.Description == "" {
		res.Reasons = append(res.Reasons, ReasonFrontmatterDescriptionMissing)
	}
	if strings.TrimSpace(ownerActor) == "" {
		res.Reasons = append(res.Reasons, ReasonOwnerMissing)
	}
	for _, field := range MetadataCardFields {
		if strings.TrimSpace(meta.Fields[field]) == "" {
			res.Reasons = append(res.Reasons, metadataFieldMissingReason(field))
		}
	}
	if decl := meta.Fields["permission-declaration"]; decl != "" {
		for _, re := range protectedPathRegexps {
			if re.MatchString(decl) {
				res.Warnings = append(res.Warnings, WarningProtectedPaths)
				break
			}
		}
	}

	scan := func(path, text string) {
		for _, f := range redact.Findings(text) {
			res.Findings = append(res.Findings, GateFinding{
				File:      path,
				Line:      f.Line,
				PatternID: f.PatternID,
				Excerpt:   f.Excerpt,
				AppealID:  AppealID(skillRef, contentHash, path, f.Line, f.PatternID),
			})
		}
	}
	scan("SKILL.md", content)
	for path, text := range files {
		scan(path, text)
	}
	return res
}
