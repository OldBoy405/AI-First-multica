"use client";

import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { MaturityPage } from "./maturity-page";
import { MaturitySuggestionsPanel } from "./maturity-suggestions";
import type {
  MaturityOverallResponse,
  MaturityProjectRankingsResponse,
  MaturitySuggestionResponse,
} from "@multica/core/types";

vi.mock("@multica/core", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/maturity", async () => {
  const actual = await vi.importActual("@multica/core/maturity");
  return {
    ...actual,
    maturityOverallOptions: (wsId: string, date?: string) => ({
      queryKey: ["maturity", wsId, "overall", date ?? "latest"],
      queryFn: () => Promise.resolve(OVERALL),
      staleTime: 0,
    }),
    maturityRankingsOptions: (wsId: string) => ({
      queryKey: ["maturity", wsId, "rankings"],
      queryFn: () => Promise.resolve(RANKINGS),
      staleTime: 0,
    }),
    maturityConfigOptions: (wsId: string) => ({
      queryKey: ["maturity", wsId, "config"],
      queryFn: () => Promise.resolve(CONFIG),
      staleTime: 0,
    }),
    maturitySuggestionsOptions: (wsId: string) => ({
      queryKey: ["maturity", wsId, "suggestions"],
      queryFn: () => Promise.resolve(SUGGESTIONS),
      staleTime: 0,
    }),
    maturitySuggestionHistoryOptions: (wsId: string) => ({
      queryKey: ["maturity", wsId, "history"],
      queryFn: () => Promise.resolve(HISTORY),
      staleTime: 0,
    }),
  };
});
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ chat: () => "/ws-1/chat" }),
}));
vi.mock("@multica/ui/components/ui/skeleton", () => ({
  Skeleton: ({ children }: { children?: React.ReactNode }) => <div>{children}</div>,
}));

const OVERALL: MaturityOverallResponse = {
  bucket_date: "2026-08-19",
  config_rev: "abcdef0123456789",
  observation: {
    active: true,
    calibration_status: "observing",
    observation_weeks: 4,
    first_bucket_date: "2026-08-19",
    elapsed_days: 0,
  },
  headline: {
    active_members: 3,
    total_tokens: 150,
    cost_usd: null,
    cost_status: "unavailable",
  },
  total_score: null,
  dimensions: [
    {
      key: "AIF",
      score: null,
      data_status: "empty",
      metrics: [
        {
          key: "token_intensity",
          raw: {
            value: 50,
            numerator: 150,
            denominator: 3,
            unit: "tokens_per_member_day",
            data_status: "ready",
            reason: null,
            attribution: null,
          },
          score: null,
        },
        {
          key: "ai_penetration",
          raw: {
            value: 0.33,
            numerator: 1,
            denominator: 3,
            unit: "ratio",
            data_status: "ready",
            reason: null,
            attribution: null,
          },
          score: null,
        },
      ],
    },
  ],
  governance: [
    {
      key: "traceability_complete_rate",
      datum: {
        value: null,
        numerator: null,
        denominator: null,
        unit: "ratio",
        data_status: "unavailable",
        reason: "trace_channel_pending_cr_c",
        attribution: null,
      },
    },
  ],
  data_status: "ready",
};

const RANKINGS: MaturityProjectRankingsResponse = {
  scope: "project",
  bucket_date: "2026-08-19",
  metric: "total",
  items: [
    {
      rank: 1,
      project_id: "p1",
      project_name: "Alpha",
      value: null,
      data_status: "unavailable",
    },
  ],
  next_cursor: null,
  data_status: "ready",
};

const CONFIG = {
  config_rev: "abcdef0123456789",
  observation_weeks: 4,
  calibration_status: "observing",
  dimensions: [{ key: "AIF", metrics: ["token_intensity", "ai_penetration"] }],
  metrics: [
    {
      key: "token_intensity",
      weight: 0.125,
      floor: 0,
      target: 1,
      unit: "tokens_per_member_day",
      known_gameability: "",
    },
  ],
  price_config_rev: null,
};

const SUGGESTIONS: MaturitySuggestionResponse = {
  latest: null,
  data_status: "empty",
};

const HISTORY = { items: [], next_cursor: null, data_status: "empty" };

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MaturityPage />
    </QueryClientProvider>,
  );
}

describe("MaturityPage", () => {
  it("renders observing banner and no radar", async () => {
    renderPage();
    expect(await screen.findByTestId("maturity-page")).toBeTruthy();
    expect(screen.getByTestId("maturity-observing")).toBeTruthy();
    // no radar: the page has no radar test id at all
    expect(screen.queryByTestId("maturity-radar")).toBeNull();
  });

  it("renders governance with unavailable copy, not zero", async () => {
    renderPage();
    expect(await screen.findByTestId("governance-traceability_complete_rate")).toBeTruthy();
    const node = screen.getByTestId("governance-traceability_complete_rate");
    expect(node.textContent).toContain("Unmeasured");
    expect(node.textContent).not.toContain("0.000");
  });

  it("renders rankings without any user entry", async () => {
    renderPage();
    expect(await screen.findByTestId("maturity-rankings")).toBeTruthy();
    expect(screen.queryByText(/user ranking/i)).toBeNull();
    expect(screen.getByTestId("rank-1")).toBeTruthy();
  });

  it("renders anti-goodhart footer", async () => {
    renderPage();
    expect(await screen.findByTestId("maturity-anti-goodhart")).toBeTruthy();
  });
});

describe("MaturitySuggestionsPanel", () => {
  it("renders the empty state when no report exists", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MaturitySuggestionsPanel wsId="ws-1" />
      </QueryClientProvider>,
    );
    expect(await screen.findByTestId("suggestions-empty")).toBeTruthy();
  });
});
