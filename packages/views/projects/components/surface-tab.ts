// URL <-> Issues/Chat surface tab mapping for the project detail view
// (CR-2026-006). Kept as pure helpers so the ?tab= round-trip is unit-testable
// without mounting the full ProjectDetail tree.

export type ProjectSurfaceTab = "issues" | "chat";

/** Parse the active surface tab; unknown/absent ?tab= falls back to issues. */
export function projectSurfaceTab(params: URLSearchParams): ProjectSurfaceTab {
  return params.get("tab") === "chat" ? "chat" : "issues";
}

/** Build the href for a surface tab, dropping ?tab= for the default (issues). */
export function projectSurfaceHref(
  pathname: string,
  params: URLSearchParams,
  next: string,
): string {
  const nextParams = new URLSearchParams(params);
  if (next === "issues") nextParams.delete("tab");
  else nextParams.set("tab", next);
  const qs = nextParams.toString();
  return qs ? `${pathname}?${qs}` : pathname;
}
