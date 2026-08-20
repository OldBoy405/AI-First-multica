// AIFIRST: CR-2026-048 TASK-09 (FR-21): runtime requirement tags.
// The server hands over the parsed frontmatter as strings; a sequence value
// arrives JSON-encoded (mirroring coerceFrontmatterValue), a scalar arrives
// as written. Anything unparsable degrades to "no tags", never an error.
export function parseRequirements(raw: string | undefined): string[] {
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (Array.isArray(parsed)) return parsed.map(String).filter(Boolean);
  } catch {
    // Not JSON: a plain scalar list such as "git, node".
  }
  return raw
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}
