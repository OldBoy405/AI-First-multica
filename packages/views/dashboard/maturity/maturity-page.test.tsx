"use client";

import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { api } from "@multica/core/api";
import { MaturityPage } from "./maturity-page";
import { MaturitySuggestionsPanel } from "./maturity-suggestions";
import type {
  MaturityOverallResponse,
  MaturityProjectRankingsResponse,
  MaturitySuggestionResponse,
} from "@multica/core/types";

vi.mock("@multica/core", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (state: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "user-1" } }),
}));
vi.mock("@multica/core/api", () => ({
  api: { ensureOrgAdminWorkspace: vi.fn().mockResolvedValue({ projectId: "p1" }) },
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({
    queryKey: ["members"],
    queryFn: () => Promise.resolve([{ user_id: "user-1", role: "owner" }]),
  }),
}));
vi.mock("@multica/core/runtimes", () => ({
  runtimeDisplayLabel: (runtime: { name: string }) => runtime.name,
  runtimeListOptions: () => ({
    queryKey: ["runtimes"],
    queryFn: () => Promise.resolve([{ id: "runtime-1", name: "Local", status: "online" }]),
  }),
}));
vi.mock("../../navigation", () => ({
  AppLink: ({ href, children, ...props }: React.ComponentProps<"a">) => (
    <a href={href} {...props}>{children}</a>
  ),
}));
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
    maturityTokenTrendOptions: (wsId: string) => ({
      queryKey: ["maturity", wsId, "trend"],
      queryFn: () => Promise.resolve(TREND),
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
  bucketDate: "2026-08-19",
  configRev: "abcdef0123456789",
  observation: {
    active: true,
    calibrationStatus: "observing",
    observationWeeks: 4,
    firstBucketDate: "2026-08-19",
    elapsedDays: 0,
  },
  headline: {
    activeMembers: 3,
    totalTokens: 150,
    costUsd: null,
    costStatus: "unavailable",
  },
  totalScore: null,
  dimensions: [
    {
      key: "AIF",
      score: null,
      dataStatus: "empty",
      metrics: [
        {
          key: "token_intensity",
          raw: {
            value: 50,
            numerator: 150,
            denominator: 3,
            unit: "tokens_per_member_day",
            dataStatus: "ready",
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
            dataStatus: "ready",
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
      key: "gate_first_pass_rate",
      datum: {
        value: 0.8,
        numerator: 4,
        denominator: 5,
        unit: "ratio",
        dataStatus: "ready",
        reason: null,
        attribution: null,
      },
    },
    {
      key: "traceability_complete_rate",
      datum: {
        value: null,
        numerator: null,
        denominator: null,
        unit: "ratio",
        dataStatus: "unavailable",
        reason: "trace_channel_pending_cr_c",
        attribution: null,
      },
    },
  ],
  dataStatus: "ready",
};

const TREND = {
  dimension: "project",
  from: "2026-08-18",
  to: "2026-08-19",
  series: [{
    id: "p1",
    label: "Alpha",
    points: [
      { date: "2026-08-18", tokens: 100, costUsd: null, costStatus: "unavailable", configRev: "old" },
      { date: "2026-08-19", tokens: 150, costUsd: null, costStatus: "unavailable", configRev: "new" },
    ],
  }],
  dataStatus: "ready",
} as const;

const RANKINGS: MaturityProjectRankingsResponse = {
  scope: "project",
  bucketDate: "2026-08-19",
  metric: "total",
  items: [
    {
      rank: 1,
      projectId: "p1",
      projectName: "Alpha",
      value: null,
      dataStatus: "unavailable",
    },
  ],
  nextCursor: null,
  dataStatus: "ready",
};

const CONFIG = {
  configRev: "abcdef0123456789",
  observationWeeks: 4,
  calibrationStatus: "observing",
  dimensions: [{ key: "AIF", metrics: ["token_intensity", "ai_penetration"] }],
  metrics: [
    {
      key: "token_intensity",
      weight: 0.125,
      floor: 0,
      target: 1,
      unit: "tokens_per_member_day",
      knownGameability: "Can be inflated by verbose prompts.",
    },
  ],
  baselineSuggestions: [],
  priceConfigRev: null,
};

const SUGGESTIONS: MaturitySuggestionResponse = {
  latest: null,
  dataStatus: "empty",
};

const HISTORY = { items: [], nextCursor: null, dataStatus: "empty" };

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

  it("renders date controls, Owner marker, daily trend and config break", async () => {
    renderPage();
    expect(await screen.findByTestId("maturity-owner-mode")).toBeTruthy();
    expect(screen.getByRole("group", { name: "maturity date range" })).toBeTruthy();
    expect(await screen.findByTestId("maturity-trend")).toBeTruthy();
    expect(screen.getByTestId("config-revision-break")).toBeTruthy();
    expect(screen.getByTestId("maturity-token-quality-pair")).toBeTruthy();
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
    fireEvent.click(screen.getByRole("button", { name: "Initialise Org Admin" }));
    await waitFor(() =>
      expect(api.ensureOrgAdminWorkspace).toHaveBeenCalledWith("ws-1", "runtime-1"),
    );
  });
});
