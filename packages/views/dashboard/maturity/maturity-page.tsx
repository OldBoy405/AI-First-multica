"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core";
import {
  maturityOverallOptions,
  maturityRankingsOptions,
  maturityConfigOptions,
} from "@multica/core/maturity";
import type { MaturityOverallResponse } from "@multica/core/types";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { MaturitySuggestionsPanel } from "./maturity-suggestions";
import { MaturityDefinitions } from "./maturity-definitions";

// AIFIRST: AI maturity dashboard (CR-2026-047 TASK-09). Observation period
// renders only the three pillars (dimensions / trend / project rankings) —
// no radar chart. Quantity metrics and governance guardrails share the page;
// the footer carries the anti-Goodhart statement.

const METRIC_OPTIONS = [
  "token_intensity",
  "ai_penetration",
  "cr_throughput_per_capita",
  "project_collab_scale",
  "project_active_rate",
  "prototype_direct_rate",
  "team_agent_depth",
  "process_completion_rate",
] as const;

function costLabel(status: string): string {
  switch (status) {
    case "authoritative":
      return "Provider-reported";
    case "mixed":
      return "Mixed (provider + estimate)";
    case "estimated":
      return "Estimated";
    default:
      return "Unavailable";
  }
}

export function MaturityPage() {
  const wsId = useWorkspaceId();
  const overall = useQuery(maturityOverallOptions(wsId));
  const cfg = useQuery(maturityConfigOptions(wsId));
  const [metric, setMetric] = useState<string>("total");
  const rankings = useQuery(
    maturityRankingsOptions(wsId, { metric, limit: 20 }),
  );

  if (overall.isLoading) {
    return (
      <div className="space-y-4 p-6" data-testid="maturity-loading">
        <Skeleton className="h-8 w-64" />
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  if (overall.isError) {
    return (
      <div className="p-6 text-muted-foreground" data-testid="maturity-error">
        Failed to load maturity data. Please retry.
      </div>
    );
  }
  const data: MaturityOverallResponse | undefined = overall.data;
  const observation = data?.observation;
  const empty = data?.data_status === "empty" || !data?.headline;

  return (
    <div className="mx-auto max-w-5xl space-y-6 p-6" data-testid="maturity-page">
      <header className="space-y-1">
        <h1 className="text-2xl font-semibold tracking-tight">AI Maturity</h1>
        <p className="text-sm text-muted-foreground">
          Bucket {data?.bucket_date ?? "—"} · config{" "}
          {data?.config_rev?.slice(0, 8) ?? "—"} · updated daily at 00:30
          Asia/Shanghai (previous local day)
        </p>
        {observation?.active && (
          <p
            className="rounded-md bg-muted px-3 py-2 text-sm"
            data-testid="maturity-observing"
          >
            Observation period (week {Math.floor((observation.elapsed_days ?? 0) / 7) + 1} of{" "}
            {observation.observation_weeks}): scores are hidden until calibration.
          </p>
        )}
      </header>

      {empty ? (
        <div className="rounded-md border p-6 text-muted-foreground" data-testid="maturity-empty">
          No snapshot yet — the first bucket appears after the next 00:30 rollup.
        </div>
      ) : (
        <>
          <section
            className="grid grid-cols-3 gap-3"
            data-testid="maturity-headline"
          >
            <StatCard label="Members" value={String(data.headline?.active_members ?? 0)} />
            <StatCard label="Tokens" value={String(data.headline?.total_tokens ?? 0)} />
            <StatCard
              label="Cost"
              value={
                data.headline?.cost_usd == null
                  ? "—"
                  : `$${data.headline.cost_usd.toFixed(4)}`
              }
              sub={costLabel(data.headline?.cost_status ?? "unavailable")}
            />
          </section>

          <section data-testid="maturity-dimensions" className="space-y-3">
            <h2 className="text-lg font-medium">Dimensions</h2>
            {data.dimensions.map((d) => (
              <div key={d.key} className="rounded-md border p-4">
                <div className="flex items-baseline justify-between">
                  <span className="font-medium">{d.key}</span>
                  <span className="text-sm text-muted-foreground">
                    {d.score == null ? "—" : d.score.toFixed(1)}
                  </span>
                </div>
                <div className="mt-2 grid grid-cols-2 gap-2 text-sm">
                  {d.metrics.map((m) => (
                    <div key={m.key} className="flex justify-between gap-2">
                      <span className="text-muted-foreground">{m.key}</span>
                      <span data-testid={`metric-${m.key}`}>
                        {m.raw.value == null
                          ? m.raw.data_status
                          : m.raw.value.toFixed(3)}
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </section>

          <section data-testid="maturity-governance" className="space-y-3">
            <h2 className="text-lg font-medium">Governance</h2>
            <div className="grid grid-cols-2 gap-3 text-sm md:grid-cols-3">
              {data.governance.map((g) => (
                <div key={g.key} className="rounded-md border p-3">
                  <div className="text-muted-foreground">{g.key}</div>
                  <div data-testid={`governance-${g.key}`}>
                    {g.datum.value == null
                      ? g.datum.data_status === "unavailable"
                        ? "Unmeasured (pending CR-C trace channel)"
                        : g.datum.data_status
                      : g.datum.value.toFixed(3)}
                  </div>
                </div>
              ))}
            </div>
          </section>

          <section data-testid="maturity-rankings" className="space-y-3">
            <div className="flex items-center justify-between">
              <h2 className="text-lg font-medium">Project rankings</h2>
              <select
                aria-label="ranking metric"
                value={metric}
                onChange={(e) => setMetric(e.target.value)}
                className="rounded-md border bg-background px-2 py-1 text-sm"
              >
                <option value="total">total</option>
                {METRIC_OPTIONS.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </div>
            {rankings.data?.items.length ? (
              <table className="w-full text-sm">
                <tbody>
                  {rankings.data.items.map((item) => (
                    <tr key={item.project_id} className="border-b">
                      <td className="w-8 py-2 text-muted-foreground">{item.rank}</td>
                      <td className="py-2">{item.project_name}</td>
                      <td className="py-2 text-right" data-testid={`rank-${item.rank}`}>
                        {item.value == null ? item.data_status : item.value.toFixed(3)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            ) : (
              <div className="rounded-md border p-4 text-muted-foreground">
                No projects ranked yet.
              </div>
            )}
          </section>
        </>
      )}

      <MaturitySuggestionsPanel wsId={wsId} />

      <section className="space-y-2 text-sm text-muted-foreground">
        <h2 className="text-base font-medium text-foreground">Method</h2>
        <MaturityDefinitions cfg={cfg.data} />
        <p>
          v1 “active members” = workspace members present at rollup time (no
          join/leave history is kept). Tokens are behavioural data, not
          performance review inputs.
        </p>
        <p className="text-xs" data-testid="maturity-anti-goodhart">
          Tokens are behaviour data, not individual performance metrics.
        </p>
      </section>

      <footer className="text-xs text-muted-foreground">
        <a href="/">Back to workspace</a>
      </footer>
    </div>
  );
}

function StatCard({
  label,
  value,
  sub,
}: {
  label: string;
  value: string;
  sub?: string;
}) {
  return (
    <div className="rounded-md border p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="text-xl font-semibold">{value}</div>
      {sub ? <div className="text-xs text-muted-foreground">{sub}</div> : null}
    </div>
  );
}
