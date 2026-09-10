"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core";
import {
  maturityOverallOptions,
  maturityRankingsOptions,
  maturityTokenTrendOptions,
  maturityConfigOptions,
} from "@multica/core/maturity";
import type { MaturityOverallResponse } from "@multica/core/types";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";
import { Leaderboard } from "../components/leaderboard";
import { UsageTrendCard } from "../components/usage-trend-card";
import { MaturitySuggestionsPanel } from "./maturity-suggestions";
import { MaturityDefinitions } from "./maturity-definitions";
import { DriftCard } from "../drift";

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
  const { t } = useT("usage");
  const wsId = useWorkspaceId();
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [dimension, setDimension] = useState<"project" | "user" | "model">("project");
  const [metric, setMetric] = useState<string>("token_intensity");
  const overall = useQuery(maturityOverallOptions(wsId, to || undefined));
  const cfg = useQuery(maturityConfigOptions(wsId));
  const trend = useQuery(
    maturityTokenTrendOptions(wsId, {
      dimension,
      dimensionId: dimension === "user" ? "self" : undefined,
      from: from || undefined,
      to: to || undefined,
    }),
  );
  const rankings = useQuery(
    maturityRankingsOptions(wsId, { date: to || undefined, metric, limit: 20 }),
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
        {t(($) => $.maturity.error)}
      </div>
    );
  }
  const data: MaturityOverallResponse | undefined = overall.data;
  const observation = data?.observation;
  const empty = data?.dataStatus === "empty" || !data?.headline;

  return (
    <div className="mx-auto max-w-5xl space-y-6 p-6" data-testid="maturity-page">
      <header className="space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h1 className="text-title-lg font-semibold tracking-tight">{t(($) => $.maturity.title)}</h1>
            <p className="text-body text-muted-foreground" data-testid="maturity-owner-mode">
              {t(($) => $.maturity.owner_mode)}
            </p>
          </div>
          <div className="flex items-center gap-2 text-body" role="group" aria-label={t(($) => $.maturity.date_range_label)}>
            <label htmlFor="maturity-from">{t(($) => $.maturity.from)}</label>
            <input id="maturity-from" type="date" value={from} onChange={(e) => setFrom(e.target.value)} className="rounded-md border bg-background px-2 py-1" />
            <label htmlFor="maturity-to">{t(($) => $.maturity.to)}</label>
            <input id="maturity-to" type="date" value={to} onChange={(e) => setTo(e.target.value)} className="rounded-md border bg-background px-2 py-1" />
          </div>
        </div>
        <p className="text-body text-muted-foreground">
          {t(($) => $.maturity.bucket_line, {
            bucket: data?.bucketDate ?? "—",
            config: data?.configRev?.slice(0, 8) ?? "—",
          })}
        </p>
        {observation?.active && (
          <p
            className="rounded-md bg-muted px-3 py-2 text-body"
            data-testid="maturity-observing"
          >
            {t(($) => $.maturity.observing, {
              week: Math.floor((observation.elapsedDays ?? 0) / 7) + 1,
              total: observation.observationWeeks,
            })}
          </p>
        )}
      </header>

      {empty ? (
        <div className="rounded-md border p-6 text-muted-foreground" data-testid="maturity-empty">
          {t(($) => $.maturity.empty)}
        </div>
      ) : (
        <>
          <section
            className="grid grid-cols-3 gap-3"
            data-testid="maturity-headline"
          >
            <StatCard label="Members" value={String(data.headline?.activeMembers ?? 0)} />
            <StatCard label="Tokens" value={String(data.headline?.totalTokens ?? 0)} />
            <StatCard
              label="Cost"
              value={
                data.headline?.costUsd == null
                  ? "—"
                  : `$${data.headline.costUsd.toFixed(4)}`
              }
              sub={costLabel(data.headline?.costStatus ?? "unavailable")}
            />
          </section>

          <section data-testid="maturity-dimensions" className="space-y-3">
            <h2 className="text-title font-medium">{t(($) => $.maturity.dimensions)}</h2>
            {data.dimensions.map((d) => (
              <div key={d.key} className="rounded-md border p-4">
                <div className="flex items-baseline justify-between">
                  <span className="font-medium">{d.key}</span>
                  <span className="text-body text-muted-foreground">
                    {d.score == null ? "—" : d.score.toFixed(1)}
                  </span>
                </div>
                <div className="mt-2 grid grid-cols-2 gap-2 text-body">
                  {d.metrics.map((m) => (
                    <div key={m.key} className="flex justify-between gap-2">
                      <span className="text-muted-foreground">{m.key}</span>
                      <span data-testid={`metric-${m.key}`}>
                        {m.raw.value == null
                          ? m.raw.dataStatus
                          : m.raw.value.toFixed(3)}
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </section>

          <section className="grid gap-3 md:grid-cols-2" data-testid="maturity-token-quality-pair">
            <StatCard
              label="Token intensity"
              value={String(data.dimensions.flatMap((d) => d.metrics).find((m) => m.key === "token_intensity")?.raw.value ?? "—")}
            />
            <StatCard
              label="Gate first-pass rate"
              value={String(data.governance.find((g) => g.key === "gate_first_pass_rate")?.datum.value ?? "—")}
            />
          </section>

          <section data-testid="maturity-governance" className="space-y-3">
            <h2 className="text-title font-medium">{t(($) => $.maturity.governance)}</h2>
            <div className="grid grid-cols-2 gap-3 text-body md:grid-cols-3">
              {data.governance.map((g) => (
                <div key={g.key} className="rounded-md border p-3">
                  <div className="text-muted-foreground">{g.key}</div>
                  <div data-testid={`governance-${g.key}`}>
                    {g.datum.value == null
                      ? g.datum.dataStatus === "unavailable"
                        ? "Unmeasured (pending CR-C trace channel)"
                        : g.datum.dataStatus
                      : g.datum.value.toFixed(3)}
                  </div>
                </div>
              ))}
            </div>
          </section>

          <section data-testid="maturity-trend" className="space-y-3">
            <div className="flex items-center justify-between gap-3">
              <h2 className="text-title font-medium">{t(($) => $.maturity.daily_trend)}</h2>
              <select aria-label={t(($) => $.maturity.trend_dimension_label)} value={dimension} onChange={(e) => setDimension(e.target.value as "project" | "user" | "model")} className="rounded-md border bg-background px-2 py-1 text-body">
                <option value="project">{t(($) => $.maturity.dim_project)}</option>
                <option value="user">{t(($) => $.maturity.dim_user)}</option>
                <option value="model">{t(($) => $.maturity.dim_model)}</option>
              </select>
            </div>
            {trend.isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : trend.isError ? (
              <div className="rounded-md border p-4 text-muted-foreground">{t(($) => $.maturity.trend_error)}</div>
            ) : (
              <UsageTrendCard
                title={t(($) => $.maturity.token_usage_over_time)}
                emptyLabel="No trend data for this range."
                series={trend.data?.series ?? []}
              />
            )}
          </section>

          <section data-testid="maturity-rankings" className="space-y-3">
            <div className="flex items-center justify-between">
              <h2 className="text-title font-medium">{t(($) => $.maturity.rankings)}</h2>
              <select
                aria-label={t(($) => $.maturity.ranking_metric_label)}
                value={metric}
                onChange={(e) => setMetric(e.target.value)}
                className="rounded-md border bg-background px-2 py-1 text-body"
              >
                <option value="total">{t(($) => $.maturity.metric_total)}</option>
                {METRIC_OPTIONS.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </div>
            {rankings.isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : rankings.isError ? (
              <div className="rounded-md border p-4 text-muted-foreground">{t(($) => $.maturity.rankings_error)}</div>
            ) : (
              <Leaderboard
                title={t(($) => $.maturity.rankings)}
                valueLabel={metric}
                emptyLabel="No projects ranked yet."
                rows={(rankings.data?.items ?? []).map((item) => ({
                  id: item.projectId,
                  rank: item.rank,
                  label: item.projectName,
                  value: item.value,
                  status: item.dataStatus,
                }))}
              />
            )}
          </section>
        </>
      )}

      <MaturitySuggestionsPanel wsId={wsId} />

      {/* AIFIRST: CR-2026-049 TASK-12 — drift governance card (E5 finding summary). */}
      <section className="space-y-2">
        <h2 className="text-title-sm font-medium text-foreground">{t(($) => $.maturity.drift)}</h2>
        <DriftCard />
      </section>

      <section className="space-y-2 text-body text-muted-foreground">
        <h2 className="text-title-sm font-medium text-foreground">{t(($) => $.maturity.method)}</h2>
        <MaturityDefinitions cfg={cfg.data} />
        <p>{t(($) => $.maturity.method_note)}</p>
        <p className="text-caption" data-testid="maturity-anti-goodhart">
          {t(($) => $.maturity.anti_goodhart)}
        </p>
      </section>

      <footer className="text-caption text-muted-foreground">
        <AppLink href="/">{t(($) => $.maturity.back_to_workspace)}</AppLink>
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
      <div className="text-body text-muted-foreground">{label}</div>
      <div className="text-title-lg font-semibold">{value}</div>
      {sub ? <div className="text-caption text-muted-foreground">{sub}</div> : null}
    </div>
  );
}
