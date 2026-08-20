// AIFIRST: CR-2026-049 TASK-12 — trace/spec-search domain types.
// API schemas translate the server's snake_case JSON at the boundary; views
// use these camelCase names exclusively.

export interface TraceMilestoneView {
  cr: string;
  milestone: string;
  frs: unknown[];
  mergeCommits: unknown[];
  /** explicit null when the milestone carries no evidence (FR-7: never fall back to trunk HEAD) */
  evidence: unknown;
  source: "event" | "baseline-imported" | string;
  traceSnapshotConflict?: boolean;
}

export interface TraceEventItem {
  eventId: number;
  crId: string;
  commitSha: string;
  occurredAt: string | null;
  state: "ok" | "baseline-imported" | "malformed" | string;
  errorCode?: string;
  milestone?: TraceMilestoneView;
}

export interface SpecTraceResponse {
  v: number;
  workspaceId: string;
  specId: string;
  events: TraceEventItem[];
}

export interface SpecSearchItem {
  specId: string;
  latestCrId: string;
  owners: Record<string, { id: string }> | Record<string, never>;
  updatedAt: string;
}

export interface SpecSearchResponse {
  v: number;
  specs: SpecSearchItem[];
  nextCursor: string | null;
}

export interface SpecSearchParams {
  q?: string;
  owner?: string;
  limit?: number;
  cursor?: string;
  signal?: AbortSignal;
}
