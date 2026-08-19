import { keepPreviousData, queryOptions } from "@tanstack/react-query";
import { api } from "../api";

// AIFIRST: AI maturity dashboard query keys + options (CR-2026-047 TASK-09).
// Snapshots roll once per local day (00:30 Asia/Shanghai), so a mounted page
// re-polls on a 5-minute cadence and anything older than a minute refetches
// on mount — the same contract as the usage dashboard.

export const maturityKeys = {
  all: (wsId: string) => ["maturity", wsId] as const,
  overall: (wsId: string, date?: string) => [...maturityKeys.all(wsId), "overall", date ?? "latest"] as const,
  tokenTrend: (wsId: string, dimension: string, dimensionId: string, from?: string, to?: string) =>
    [...maturityKeys.all(wsId), "token-trend", dimension, dimensionId, from ?? "", to ?? ""] as const,
  rankings: (wsId: string, date: string | undefined, metric: string, limit: number) =>
    [...maturityKeys.all(wsId), "rankings", date ?? "", metric, limit] as const,
  suggestions: (wsId: string) => [...maturityKeys.all(wsId), "suggestions"] as const,
  suggestionHistory: (wsId: string, limit: number, cursor?: string) =>
    [...maturityKeys.all(wsId), "suggestions-history", limit, cursor ?? ""] as const,
  config: (wsId: string) => [...maturityKeys.all(wsId), "config"] as const,
};

const STALE_TIME = 60 * 1000;
const REFETCH_INTERVAL = 5 * 60 * 1000;

export const maturityOverallOptions = (wsId: string, date?: string) =>
  queryOptions({
    queryKey: maturityKeys.overall(wsId, date),
    queryFn: () => api.getMaturityOverall(wsId, date),
    staleTime: STALE_TIME,
    refetchInterval: REFETCH_INTERVAL,
    placeholderData: keepPreviousData,
  });

export const maturityTokenTrendOptions = (
  wsId: string,
  params: { dimension: "project" | "user" | "model"; dimension_id?: string; from?: string; to?: string },
) =>
  queryOptions({
    queryKey: maturityKeys.tokenTrend(wsId, params.dimension, params.dimension_id ?? "", params.from, params.to),
    queryFn: () => api.getMaturityTokenTrend(wsId, params),
    staleTime: STALE_TIME,
    refetchInterval: REFETCH_INTERVAL,
  });

export const maturityRankingsOptions = (
  wsId: string,
  params: { date?: string; metric?: string; limit?: number; cursor?: string },
) =>
  queryOptions({
    queryKey: maturityKeys.rankings(wsId, params.date, params.metric ?? "total", params.limit ?? 20),
    queryFn: () => api.getMaturityRankings(wsId, params),
    staleTime: STALE_TIME,
    refetchInterval: REFETCH_INTERVAL,
    placeholderData: keepPreviousData,
  });

export const maturitySuggestionsOptions = (wsId: string) =>
  queryOptions({
    queryKey: maturityKeys.suggestions(wsId),
    queryFn: () => api.getMaturitySuggestions(wsId),
    staleTime: STALE_TIME,
    refetchInterval: REFETCH_INTERVAL,
  });

export const maturitySuggestionHistoryOptions = (wsId: string, limit = 12, cursor?: string) =>
  queryOptions({
    queryKey: maturityKeys.suggestionHistory(wsId, limit, cursor),
    queryFn: () => api.getMaturitySuggestionHistory(wsId, { limit, cursor }),
    staleTime: STALE_TIME,
    refetchInterval: REFETCH_INTERVAL,
    placeholderData: keepPreviousData,
  });

export const maturityConfigOptions = (wsId: string) =>
  queryOptions({
    queryKey: maturityKeys.config(wsId),
    queryFn: () => api.getMaturityConfig(wsId),
    staleTime: 5 * 60 * 1000,
  });
