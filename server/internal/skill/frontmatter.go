// Package skill provides shared utilities for working with SKILL.md files.
package skill

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Keeping the trailing newline inside group 1 matters: yaml.v3's `|` clip
// chomping only preserves a final newline when the input itself contains one.
var frontmatterPattern = regexp.MustCompile(`(?s)\A---\r?\n(.*?\r?\n)---`)

// ParseSkillFrontmatter extracts name and description from the YAML frontmatter
// block of a SKILL.md file. Returns empty strings when the frontmatter is
// absent or malformed so callers can keep treating missing metadata as a
// non-fatal condition, matching the behaviour of the legacy line-based parser.
//
// Thin wrapper over ParseSkillMetadata; kept for source compatibility.
func ParseSkillFrontmatter(content string) (name, description string) {
	meta := ParseSkillMetadata(content)
	return meta.Name, meta.Description
}

// SkillMetadata is the full scalar frontmatter surface of a SKILL.md file:
// name/description plus every other scalar key (the market metadata card
// fields, `source: session-export` markers, requirement lists, ...).
// Structured values are JSON-encoded by coerceFrontmatterValue, mirroring
// the TS parseFrontmatter behaviour.
type SkillMetadata struct {
	Name        string
	Description string
	Fields      map[string]string
}

// ParseSkillMetadata decodes the whole YAML frontmatter block. Absent or
// malformed frontmatter yields empty Name/Description and an empty Fields
// map, never an error: the publish gate reports the specific missing field
// as a structured reason instead of failing the parse.
func ParseSkillMetadata(content string) SkillMetadata {
	out := SkillMetadata{Fields: map[string]string{}}
	if !strings.HasPrefix(content, "---") {
		return out
	}
	match := frontmatterPattern.FindStringSubmatch(content)
	if match == nil {
		return out
	}

	var fm map[string]any
	if err := yaml.Unmarshal([]byte(match[1]), &fm); err != nil {
		return out
	}
	for k, v := range fm {
		out.Fields[k] = coerceFrontmatterValue(v)
	}
	// Trimmed because both fields are single-line labels wherever they are
	// consumed, while YAML block scalars (`description: |`, `description: >`)
	// carry a trailing newline by clip chomping. Storing that newline made the
	// imported skill differ from its own trimmed form, which the skill detail
	// page read as an unsaved edit (MUL-5645). Normalize at the parse seam so
	// no import path has to remember to.
	out.Name = strings.TrimSpace(out.Fields["name"])
	out.Description = strings.TrimSpace(out.Fields["description"])
	return out
}

// coerceFrontmatterValue renders a decoded YAML value as a string, mirroring the
// TS side: nil becomes empty, strings pass through, other scalars use their
// literal form, and structured values (sequences/mappings) are JSON-encoded.
func coerceFrontmatterValue(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case uint64:
		return strconv.FormatUint(val, 10)
	case float64:
		return strconv.FormatFloat(val, 'g', -1, 64)
	default:
		encoded, err := json.Marshal(val)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}
