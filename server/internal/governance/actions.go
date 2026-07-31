// AIFIRST: governance package — AI-First platform governance layer (fork-only code,
// CONTRIBUTING.AIFIRST.md rule 1: custom code lives in new directories).
// The CR projection worker (crsync), signed approvals (approval) and reconcile job
// from CR-2026-002 land in this package in later tasks.
package governance

// New activity_log action values (CR-2026-002 TASK-04). The action column is free
// text, so no migration is needed. Both are "zero new probes" audit channels
// (P1 design §C.5 / §B.4): reported through the existing task callback family,
// and the recorded details never contain command argument bodies or evidence
// file contents — only countable minimal facts.
const (
	// ActionGitguardDenied records one git invocation rejected by gitguard
	// (FORBIDDEN_SUBCOMMAND / FORBIDDEN_FLAG). details: {caller, sub, code}.
	ActionGitguardDenied = "aifirst.gitguard_denied"
	// ActionEvidenceDrift records one post-approval evidence drift detected by
	// crctl gate/validate. details: {cr_id, stage, expected_digest, actual_digest,
	// detected_at}. The P3 governance board counts EVIDENCE_DRIFT exclusively
	// from rows with this action.
	ActionEvidenceDrift = "aifirst.evidence_drift"
)
