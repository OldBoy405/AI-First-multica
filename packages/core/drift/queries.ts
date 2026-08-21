import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { DriftFindingsParams } from "../types";

// AIFIRST: CR-2026-049 TASK-12 — drift query keys + options.
// Overview re-polls on a 5-minute cadence (health is cheap to recompute);
// findings pages cache per cursor/filter.

export const driftKeys = {
  all: (wsId: string) => ["drift", wsId] as const,
  overview: (wsId: string) => [...driftKeys.all(wsId), "overview"] as const,
  findings: (wsId: string, params: DriftFindingsParams) =>
    [
      ...driftKeys.all(wsId),
      "findings",
      params.status ?? "",
      params.kind ?? "",
      params.repositoryId ?? "",
      params.limit ?? 50,
      params.cursor ?? "",
    ] as const,
};

export const driftOverviewOptions = (wsId: string) =>
  queryOptions({
    queryKey: driftKeys.overview(wsId),
    queryFn: () => api.getDriftOverview(wsId),
    staleTime: 60 * 1000,
    refetchInterval: 5 * 60 * 1000,
  });

export const driftFindingsOptions = (wsId: string, params: DriftFindingsParams) =>
  queryOptions({
    queryKey: driftKeys.findings(wsId, params),
    queryFn: () => api.listDriftFindings(wsId, params),
    staleTime: 60 * 1000,
  });
