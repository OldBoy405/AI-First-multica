// AIFIRST: AI maturity dashboard domain types (CR-2026-047 TASK-08).
// API schemas translate the server's snake_case JSON at the boundary; views
// and query code use these camelCase names exclusively.

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
  dataStatus: MaturityDataStatus;
  reason: string | null;
  attribution: {
    attributedCount: number;
    unattributedCount: number;
    coverage: number | null;
  } | null;
}

export interface MaturityReport {
  schema: "ai-first.maturity-report/v1";
  reportKey: string;
  week: string;
  generatedAt: string;
  relativePath: string;
  markdown: string;
  contentSha256: string;
  sourceTaskId: string;
  chatSessionId: string;
  configRevs: string[];
}

export interface MaturityObservation {
  active: boolean;
  calibrationStatus: string;
  observationWeeks: number;
  firstBucketDate: string;
  elapsedDays: number;
}

export interface MaturityHeadline {
  activeMembers: number;
  totalTokens: number;
  costUsd: number | null;
  costStatus: string;
}

export interface MaturityOverallResponse {
  bucketDate: string | null;
  configRev: string | null;
  observation: MaturityObservation | null;
  headline: MaturityHeadline | null;
  totalScore: number | null;
  dimensions: Array<{
    key: string;
    score: number | null;
    dataStatus: MaturityDataStatus;
    metrics: Array<{
      key: string;
      raw: MaturityMetricValue;
      score: number | null;
    }>;
  }>;
  governance: Array<{ key: string; datum: MaturityMetricValue }>;
  dataStatus: "ready" | "empty";
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
      costUsd: number | null;
      costStatus: string;
      configRev?: string;
    }>;
  }>;
  dataStatus: "ready" | "empty";
}

export interface MaturityProjectRankingsResponse {
  scope: "project";
  bucketDate: string | null;
  metric: string;
  items: Array<{
    rank: number;
    projectId: string;
    projectName: string;
    value: number | null;
    dataStatus: MaturityDataStatus;
  }>;
  nextCursor: string | null;
  dataStatus: "ready" | "empty";
}

export interface MaturitySuggestionResponse {
  latest: MaturityReport | null;
  dataStatus: "ready" | "empty";
}

export interface MaturitySuggestionHistoryResponse {
  items: MaturityReport[];
  nextCursor: string | null;
  dataStatus: "ready" | "empty";
}

export interface MaturityConfigResponse {
  configRev: string;
  observationWeeks: number;
  calibrationStatus: string;
  dimensions: Array<{ key: string; metrics: string[] }>;
  metrics: Array<{
    key: string;
    weight: number;
    floor: number;
    target: number;
    unit: string;
    knownGameability: string;
  }>;
  baselineSuggestions: Array<{
    metricKey: string;
    sampleCount: number;
    floorP10: number;
    targetP75: number;
  }>;
  priceConfigRev: string | null;
}
