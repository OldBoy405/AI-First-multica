package governance

// AIFIRST: CR-2026-049 TASK-13 — cross-language golden test (SDD §7.2 AC-1).
// The same 191KB accumulated traceability document is parsed twice: once by
// the tools zero-dependency YAML subset parser (Node, the canonical trace
// payload source — golden JSON committed in testdata) and once by Go's yaml.v3
// here. Both sides canonicalize (object keys sorted, then JSON) and must
// deep-equal: a divergence means one parser silently drops or reinterprets
// structure, which for trace payloads would be silent data loss.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func canonicalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = canonicalize(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmtKey(k)] = canonicalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonicalize(val)
		}
		return out
	default:
		return v
	}
}

func fmtKey(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	b, _ := json.Marshal(k)
	return string(b)
}

func TestCrossLanguageTraceabilityGolden(t *testing.T) {
	yamlPath := filepath.Join("testdata", "traceability-golden.yml")
	goldenPath := filepath.Join("testdata", "traceability-golden.json")

	yamlBytes, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("read yaml fixture: %v", err)
	}
	goldenBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden json: %v", err)
	}

	// Go side: yaml.v3 into any, then canonical JSON (Go marshals maps with
	// sorted keys — the same canonicalization the golden file already has).
	var doc any
	if err := yaml.Unmarshal(yamlBytes, &doc); err != nil {
		t.Fatalf("yaml.v3 parse: %v", err)
	}
	goCanon, err := json.MarshalIndent(canonicalize(doc), "", "  ")
	if err != nil {
		t.Fatalf("go canonical marshal: %v", err)
	}
	goCanon = append(goCanon, '\n')

	// Deep-compare via generic JSON trees (semantic equality, not byte order
	// of object keys — both sides are already sorted, but numbers 1 vs 1.0
	// still need semantic compare).
	var goDoc, nodeDoc any
	if err := json.Unmarshal(goCanon, &goDoc); err != nil {
		t.Fatalf("go json reparse: %v", err)
	}
	if err := json.Unmarshal(goldenBytes, &nodeDoc); err != nil {
		t.Fatalf("golden json reparse: %v", err)
	}
	diff := deepDiff(goDoc, nodeDoc, "$")
	if len(diff) > 0 {
		for _, d := range diff {
			t.Errorf("golden mismatch at %s: go=%v node=%v", d.path, d.goVal, d.nodeVal)
		}
		t.Fatalf("cross-language golden diverged: %d mismatches", len(diff))
	}

	// Sanity: the document still carries the 36-segment baseline.
	var top map[string]any
	if err := json.Unmarshal(goCanon, &top); err != nil {
		t.Fatal(err)
	}
	milestones, _ := top["milestones"].([]any)
	if len(milestones) != 36 {
		t.Errorf("milestones = %d, want 36", len(milestones))
	}
}

type diffEntry struct {
	path           string
	goVal, nodeVal any
}

func deepDiff(a, b any, path string) []diffEntry {
	var out []diffEntry
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []diffEntry{{path, a, b}}
		}
		for k, v := range av {
			if bvv, exists := bv[k]; exists {
				out = append(out, deepDiff(v, bvv, path+"/"+k)...)
			} else {
				out = append(out, diffEntry{path + "/" + k, v, "<missing>"})
			}
		}
		for k, v := range bv {
			if _, exists := av[k]; !exists {
				out = append(out, diffEntry{path + "/" + k, "<missing>", v})
			}
		}
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return []diffEntry{{path, a, b}}
		}
		if len(av) != len(bv) {
			return []diffEntry{{path + "#len", len(av), len(bv)}}
		}
		for i := range av {
			out = append(out, deepDiff(av[i], bv[i], path+"/"+itoa(i))...)
		}
	default:
		if !jsonEqual(a, b) {
			out = append(out, diffEntry{path, a, b})
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
