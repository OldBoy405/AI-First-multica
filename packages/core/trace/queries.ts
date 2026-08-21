import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { SpecSearchParams } from "../types";

// AIFIRST: CR-2026-049 TASK-12 — spec trace query keys + options.

export const traceKeys = {
  all: (wsId: string) => ["spec-trace", wsId] as const,
  timeline: (wsId: string, specId: string) => [...traceKeys.all(wsId), "timeline", specId] as const,
  search: (wsId: string, params: SpecSearchParams) =>
    [...traceKeys.all(wsId), "search", params.q ?? "", params.owner ?? ""] as const,
};

export const specTraceOptions = (wsId: string, specId: string) =>
  queryOptions({
    queryKey: traceKeys.timeline(wsId, specId),
    queryFn: () => api.getSpecTrace(wsId, specId),
    staleTime: 60 * 1000,
  });

export const specSearchOptions = (wsId: string, params: SpecSearchParams) =>
  queryOptions({
    queryKey: traceKeys.search(wsId, params),
    queryFn: () => api.searchSpecs(wsId, params),
    staleTime: 60 * 1000,
  });
