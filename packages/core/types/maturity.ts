// AIFIRST: AI maturity dashboard wire types (CR-2026-047 TASK-08).
// Mirrors server/internal/maturity/api.go. Enum-typed fields stay lenient
// strings at the zod layer; these TS types are the strict view.

export type MaturityDataStatus =
  | "ready"
  | "empty"
  | "unavailable"
  | "not_applicable";

export interface MaturityMetricValue {
  value: number | null;
  numerator: number | null;
  denominator: number | null;
  unit: string;
  data_status: MaturityDataStatus;
  reason: string | null;
  attribution: {
    attributed_count: number;
    unattributed_count: number;
    coverage: number | null;
  } | null;
}

export interface MaturityReport {
  schema: "ai-first.maturity-report/v1";
  report_key: string;
  week: string;
  generated_at: string;
  relative_path: string;
  markdown: string;
  content_sha256: string;
  source_task_id: string;
  chat_session_id: string;
  config_revs: string[];
}

export interface MaturityObservation {
  active: boolean;
  calibration_status: string;
  observation_weeks: number;
  first_bucket_date: string;
  elapsed_days: number;
}

export interface MaturityHeadline {
  active_members: number;
  total_tokens: number;
  cost_usd: number | null;
  cost_status: string;
}

export interface MaturityOverallResponse {
  bucket_date: string | null;
  config_rev: string | null;
  observation: MaturityObservation | null;
  headline: MaturityHeadline | null;
  total_score: number | null;
  dimensions: Array<{
    key: string;
    score: number | null;
    data_status: MaturityDataStatus;
    metrics: Array<{
      key: string;
      raw: MaturityMetricValue;
      score: number | null;
    }>;
  }>;
  governance: Array<{ key: string; datum: MaturityMetricValue }>;
  data_status: "ready" | "empty";
}

export interface MaturityTokenTrendResponse {
  dimension: "project" | "user" | "model";
  from: string;
  to: string;
  series: Array<{
    id: string;
    label: string;
    points: Array<{
      date: string;
      tokens: number;
      cost_usd: number | null;
      cost_status: string;
      config_rev?: string;
    }>;
  }>;
  data_status: "ready" | "empty";
}

export interface MaturityProjectRankingsResponse {
  scope: "project";
  bucket_date: string | null;
  metric: string;
  items: Array<{
    rank: number;
    project_id: string;
    project_name: string;
    value: number | null;
    data_status: MaturityDataStatus;
  }>;
  next_cursor: string | null;
  data_status: "ready" | "empty";
}

export interface MaturitySuggestionResponse {
  latest: MaturityReport | null;
  data_status: "ready" | "empty";
}

export interface MaturitySuggestionHistoryResponse {
  items: MaturityReport[];
  next_cursor: string | null;
  data_status: "ready" | "empty";
}

export interface MaturityConfigResponse {
  config_rev: string;
  observation_weeks: number;
  calibration_status: string;
  dimensions: Array<{ key: string; metrics: string[] }>;
  metrics: Array<{
    key: string;
    weight: number;
    floor: number;
    target: number;
    unit: string;
    known_gameability: string;
  }>;
  baseline_suggestions: Array<{
    metric_key: string;
    sample_count: number;
    floor_p10: number;
    target_p75: number;
  }>;
  price_config_rev: string | null;
}
