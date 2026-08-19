import { describe, expect, it } from "vitest";
import { parseWithFallback } from "./schema";
import {
  MaturityOverallResponseSchema,
  MaturityTokenTrendResponseSchema,
  MaturityProjectRankingsResponseSchema,
  MaturitySuggestionResponseSchema,
  MaturitySuggestionHistoryResponseSchema,
  MaturityConfigResponseSchema,
  EMPTY_MATURITY_OVERALL,
  EMPTY_MATURITY_TOKEN_TREND,
  EMPTY_MATURITY_RANKINGS,
  EMPTY_MATURITY_SUGGESTIONS,
  EMPTY_MATURITY_SUGGESTION_HISTORY,
  EMPTY_MATURITY_CONFIG,
} from "./schemas";
import type {
  MaturityOverallResponse,
  MaturityTokenTrendResponse,
  MaturityProjectRankingsResponse,
  MaturitySuggestionResponse,
  MaturitySuggestionHistoryResponse,
  MaturityConfigResponse,
} from "../types/maturity";

describe("maturity schemas parseWithFallback leniency", () => {
  it("parses a complete overall response", () => {
    const raw = {
      bucket_date: "2026-08-19",
      config_rev: "a".repeat(40),
      observation: { active: true, calibration_status: "observing", observation_weeks: 4, first_bucket_date: "2026-08-19", elapsed_days: 0 },
      headline: { active_members: 3, total_tokens: 150, cost_usd: null, cost_status: "unavailable" },
      total_score: null,
      dimensions: [{ key: "AIF", score: null, data_status: "empty", metrics: [{ key: "token_intensity", raw: { value: 1, numerator: 150, denominator: 3, unit: "tokens_per_member_day", data_status: "ready", reason: null, attribution: { attributed_count: 1, unattributed_count: 0, coverage: 1 } }, score: null }] }],
      governance: [{ key: "gate_first_pass_rate", datum: { value: null, numerator: null, denominator: null, unit: "ratio", data_status: "empty", reason: null, attribution: null } }],
      data_status: "ready",
      future_field: "tolerated",
    };
    const out = parseWithFallback<MaturityOverallResponse>(raw, MaturityOverallResponseSchema, EMPTY_MATURITY_OVERALL, { endpoint: "test" });
    expect(out.data_status).toBe("ready");
    expect(out.dimensions[0]?.metrics[0]?.raw.value).toBe(1);
  });

  it("malformed overall payload falls back without throwing", () => {
    const out = parseWithFallback<MaturityOverallResponse>(null, MaturityOverallResponseSchema, EMPTY_MATURITY_OVERALL, { endpoint: "test" });
    expect(out.data_status).toBe("empty");
    expect(out.dimensions).toEqual([]);
  });

  it("unknown enum values degrade to strings (never crash)", () => {
    const raw = { data_status: "weird_future_status", dimensions: [], governance: [] };
    const out = parseWithFallback<MaturityOverallResponse>(raw, MaturityOverallResponseSchema, EMPTY_MATURITY_OVERALL, { endpoint: "test" });
    expect(out.data_status).toBe("weird_future_status");
  });

  it("token-trend, rankings, suggestions, history, config all degrade safely", () => {
    const trend = parseWithFallback<MaturityTokenTrendResponse>(
      { dimension: "model", series: "not-an-array" },
      MaturityTokenTrendResponseSchema,
      EMPTY_MATURITY_TOKEN_TREND,
      { endpoint: "test" },
    );
    expect(trend.series).toEqual([]);

    const rankings = parseWithFallback<MaturityProjectRankingsResponse>(
      { items: [{ rank: "one", value: null }] },
      MaturityProjectRankingsResponseSchema,
      EMPTY_MATURITY_RANKINGS,
      { endpoint: "test" },
    );
    expect(rankings.items).toEqual([]); // malformed item -> whole list falls back

    const sugg = parseWithFallback<MaturitySuggestionResponse>(
      { latest: { report_key: "k" } },
      MaturitySuggestionResponseSchema,
      EMPTY_MATURITY_SUGGESTIONS,
      { endpoint: "test" },
    );
    expect(sugg.latest?.report_key).toBe("k");

    const hist = parseWithFallback<MaturitySuggestionHistoryResponse>(
      { items: [{}] },
      MaturitySuggestionHistoryResponseSchema,
      EMPTY_MATURITY_SUGGESTION_HISTORY,
      { endpoint: "test" },
    );
    expect(hist.items).toHaveLength(1);

    const cfg = parseWithFallback<MaturityConfigResponse>(
      {},
      MaturityConfigResponseSchema,
      EMPTY_MATURITY_CONFIG,
      { endpoint: "test" },
    );
    expect(cfg.calibration_status).toBe("observing");
    expect(cfg.metrics).toEqual([]);
  });
});
