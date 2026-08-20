package commitprefix

// AIFIRST: CR-2026-049 TASK-08 — E5 commit-prefix declaration DTO (hand-written).
// The generated read-only copy (config_gen.go, produced by gen/generate-prefixes.mjs)
// declares generatedPrefixes / generatedConfigRev; this file owns the shared DTO
// and is the only runtime import surface for the declaration.

// RepoPrefixDecl is the per-repository E5 declaration (SDD §3.3): the generated
// canonical URL must exactly match a workspace.repos entry; trunk comes from
// the declaration only (workspace.repos has no ref field); Prefixes are matched
// case-sensitively with strings.HasPrefix by the scan classifier.
type RepoPrefixDecl struct {
	ID           string
	CanonicalURL string
	Owner        string
	Repo         string
	Trunk        string
	Prefixes     []string
}
