// AIFIRST: CR-2026-049 TASK-12 — drift domain types.
// API schemas translate the server's snake_case JSON at the boundary; views
// use these camelCase names exclusively. Enums are strings so unknown values
// parse and render the "unknown" fallback instead of crashing.

export type DriftScanHealth =
  | "ok"
  | "not_configured"
  | "uninitialized"
  | "failed"
  | "stale"
  | "unknown";

export interface DriftLatencyMS {
  sampleCount: number;
  p50: number | null;
  p90: number | null;
}

export interface DriftOverview {
  v: number;
  scanHealth: DriftScanHealth;
  lastPlanStatus: string;
  lastSuccessAt: string | null;
  repositoryIds: string[];
  bypassCount: number;
  wipOnTrunkCount: number;
  resolveLatencyMs: DriftLatencyMS;
}

export type DriftFindingStatus = "open" | "acknowledged" | "resolved" | "wontfix" | "unknown";
export type DriftFindingKind = "alignment-drift" | "impact-stale" | "bypass-commit" | "wip-on-trunk" | "unknown";

export interface DriftFinding {
  id: string;
  repositoryId: string;
  specId: string | null;
  crId: string | null;
  kind: DriftFindingKind;
  severity: string;
  summary: string;
  evidence: Record<string, unknown>;
  status: DriftFindingStatus;
  foundAt: string;
  resolvedAt: string | null;
}

export interface DriftFindingsResponse {
  v: number;
  findings: DriftFinding[];
  nextCursor: string | null;
}

export interface DriftFindingsParams {
  status?: string;
  kind?: string;
  repositoryId?: string;
  limit?: number;
  cursor?: string;
  signal?: AbortSignal;
}

export interface DriftPatchRequest {
  fromStatus: string;
  toStatus: string;
}
