"use client";

import { useState } from "react";
import { BarChart3 } from "lucide-react";
import {
  DailyCostChart,
  DailyTokensChart,
  DailyTimeChart,
  DailyTasksChart,
  WeeklyCostChart,
  WeeklyTokensChart,
  WeeklyTimeChart,
  WeeklyTasksChart,
} from "../../runtimes/components/charts";
import { aggregateByWeek, weekStartIso } from "../../runtimes/utils";
import { useT } from "../../i18n";
import {
  aggregateDailyCost,
  aggregateDailyTasks,
  aggregateDailyTime,
  aggregateDailyTokens,
  aggregateWeeklyTasks,
  aggregateWeeklyTime,
  formatDuration,
} from "../utils";
import { Segmented, type Dim } from "./dashboard-shared";
import { DimSegmented } from "./dim-segmented";

type UsageMetric = "tokens" | "cost" | "time" | "tasks";

/**
 * Spend over time: one x-axis, four metrics behind a toggle so the reader can
 * mentally overlay them by flipping between them.
 *
 * Errors used to be the fifth metric here. It is a different question — "what
 * broke" rather than "what did it cost" — and sharing a toggle with the spend
 * metrics meant the only way to see failures was to hide spend. It now has its
 * own chart on the Errors tab.
 */
type DashboardUsageTrendProps = {
  allowedDims: readonly Dim[];
  dailyCost: ReturnType<typeof aggregateDailyCost>;
  dailyTokens: ReturnType<typeof aggregateDailyTokens>;
  dailyTime: ReturnType<typeof aggregateDailyTime>;
  dailyTasks: ReturnType<typeof aggregateDailyTasks>;
  weeklyCost: ReturnType<typeof aggregateByWeek>["weeklyCostStack"];
  weeklyTokens: ReturnType<typeof aggregateByWeek>["weeklyTokens"];
  weeklyTime: ReturnType<typeof aggregateWeeklyTime>;
  weeklyTasks: ReturnType<typeof aggregateWeeklyTasks>;
  lessThanMinuteLabel: string;
};

// AIFIRST: generic token-series variant reused by the maturity dashboard (CR-2026-047).
type SimpleUsageTrendProps = {
  title: string;
  emptyLabel: string;
  series: Array<{
    id: string;
    label: string;
    points: Array<{
      date: string;
      tokens: number;
      costUsd: number | null;
      configRev?: string;
    }>;
  }>;
};

export function UsageTrendCard(props: DashboardUsageTrendProps | SimpleUsageTrendProps) {
  return "series" in props ? <SimpleUsageTrendCard {...props} /> : <DashboardUsageTrendCard {...props} />;
}

function SimpleUsageTrendCard({ title, emptyLabel, series }: SimpleUsageTrendProps) {
  const [dim, setDim] = useState<Dim>("daily");
  const pointCount = series.reduce((count, item) => count + item.points.length, 0);
  const allowedDims: readonly Dim[] = pointCount >= 14 ? ["daily", "weekly"] : ["daily"];
  const effectiveDim = allowedDims.includes(dim) ? dim : "daily";
  const displayed = series.map((item) => {
    if (effectiveDim === "daily") return item;
    const weeks = new Map<string, { tokens: number; costUsd: number; hasCost: boolean; configRev?: string }>();
    for (const point of item.points) {
      const week = weekStartIso(point.date);
      const current = weeks.get(week) ?? { tokens: 0, costUsd: 0, hasCost: false };
      current.tokens += point.tokens;
      if (point.costUsd != null) {
        current.costUsd += point.costUsd;
        current.hasCost = true;
      }
      current.configRev = point.configRev;
      weeks.set(week, current);
    }
    return {
      ...item,
      points: [...weeks.entries()].map(([date, point]) => ({
        date,
        tokens: point.tokens,
        costUsd: point.hasCost ? point.costUsd : null,
        configRev: point.configRev,
      })),
    };
  });
  const max = displayed.reduce(
    (outer, item) => Math.max(outer, ...item.points.map((point) => point.tokens)),
    0,
  );

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-body font-semibold">{title}</h3>
        <DimSegmented allowedDims={allowedDims} value={effectiveDim} onChange={setDim} />
      </div>
      {displayed.length === 0 ? (
        <p className="rounded-md border border-dashed p-6 text-center text-caption text-muted-foreground">
          {emptyLabel}
        </p>
      ) : (
        <div className="space-y-4">
          {displayed.map((item) => (
            <div key={item.id}>
              <p className="mb-2 text-label font-medium">{item.label}</p>
              <div className="space-y-2">
                {item.points.map((point, index) => (
                  <div key={`${item.id}-${point.date}`} className="grid grid-cols-[6.5rem_1fr_auto] items-center gap-2 text-caption">
                    <span>
                      {point.date}
                      {point.configRev && index > 0 && point.configRev !== item.points[index - 1]?.configRev ? (
                        <span className="ml-1 text-muted-foreground" data-testid="config-revision-break">revision</span>
                      ) : null}
                    </span>
                    <span className="h-2 overflow-hidden rounded-full bg-muted">
                      <span className="block h-full rounded-full bg-chart-1" style={{ width: `${max > 0 ? (point.tokens / max) * 100 : 0}%` }} />
                    </span>
                    <span className="tabular-nums">{point.tokens.toLocaleString()} tokens</span>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function DashboardUsageTrendCard({
  allowedDims,
  dailyCost,
  dailyTokens,
  dailyTime,
  dailyTasks,
  weeklyCost,
  weeklyTokens,
  weeklyTime,
  weeklyTasks,
  lessThanMinuteLabel,
}: DashboardUsageTrendProps) {
  const { t } = useT("usage");
  const [metric, setMetric] = useState<UsageMetric>("tokens");
  const [dim, setDim] = useState<Dim>("daily");
  // Derived, never reset: when the range narrows to 1d the card simply draws
  // the one dimension that range allows, and a later widening restores the
  // reader's choice. Writing the correction back into state would make the
  // card forget it.
  const effectiveDim: Dim = allowedDims.includes(dim) ? dim : allowedDims[0]!;
  const weekly = effectiveDim === "weekly";

  // Empty-state is per-metric so each toggle option independently decides
  // whether it has data — e.g. tokens recorded but no terminal runs yet
  // should show Tokens normally while Time / Tasks fall through to empty.
  const costData = weekly ? weeklyCost : dailyCost;
  const tokensData = weekly ? weeklyTokens : dailyTokens;
  const timeData = weekly ? weeklyTime : dailyTime;
  const tasksData = weekly ? weeklyTasks : dailyTasks;

  const totalCost = costData.reduce((sum, d) => sum + d.total, 0);
  const totalTokens = tokensData.reduce(
    (sum, d) => sum + d.input + d.output + d.cacheRead + d.cacheWrite,
    0,
  );
  const totalSeconds = timeData.reduce((sum, d) => sum + d.totalSeconds, 0);
  const totalTasks = tasksData.reduce((sum, d) => sum + d.completed + d.failed, 0);
  const isEmpty =
    metric === "cost"
      ? totalCost === 0
      : metric === "tokens"
        ? totalTokens === 0
        : metric === "time"
          ? totalSeconds === 0
          : totalTasks === 0;

  const title = weekly
    ? metric === "cost"
      ? t(($) => $.weekly.title_cost)
      : metric === "tokens"
        ? t(($) => $.weekly.title_tokens)
        : metric === "time"
          ? t(($) => $.weekly.title_time)
          : t(($) => $.weekly.title_tasks)
    : metric === "cost"
      ? t(($) => $.daily.title_cost)
      : metric === "tokens"
        ? t(($) => $.daily.title_tokens)
        : metric === "time"
          ? t(($) => $.daily.title_time)
          : t(($) => $.daily.title_tasks);

  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <h4 className="text-body font-semibold">{title}</h4>
        <div className="flex flex-wrap items-center gap-2">
          <Segmented
            label={t(($) => $.daily.metric_label)}
            value={metric}
            onChange={setMetric}
            options={[
              { label: t(($) => $.daily.metric_tokens), value: "tokens" as const },
              { label: t(($) => $.daily.metric_cost), value: "cost" as const },
              { label: t(($) => $.daily.metric_time), value: "time" as const },
              { label: t(($) => $.daily.metric_tasks), value: "tasks" as const },
            ]}
          />
          <DimSegmented allowedDims={allowedDims} value={effectiveDim} onChange={setDim} />
        </div>
      </div>
      <div className="min-h-[240px]">
        {isEmpty ? (
          <div className="flex aspect-[3/1] flex-col items-center justify-center gap-2 rounded-md border border-dashed bg-muted/20 p-6 text-center">
            <BarChart3 className="h-5 w-5 text-faint-foreground" />
            <p className="text-caption text-muted-foreground">
              {t(($) => $.daily.no_data)}
            </p>
          </div>
        ) : weekly ? (
          metric === "cost" ? (
            <WeeklyCostChart data={weeklyCost} />
          ) : metric === "tokens" ? (
            <WeeklyTokensChart data={weeklyTokens} />
          ) : metric === "time" ? (
            <WeeklyTimeChart
              data={weeklyTime}
              formatY={(s) => formatDuration(s, lessThanMinuteLabel)}
              formatTooltip={(s) => formatDuration(s, lessThanMinuteLabel)}
            />
          ) : (
            <WeeklyTasksChart data={weeklyTasks} />
          )
        ) : metric === "cost" ? (
          <DailyCostChart data={dailyCost} />
        ) : metric === "tokens" ? (
          <DailyTokensChart data={dailyTokens} />
        ) : metric === "time" ? (
          <DailyTimeChart
            data={dailyTime}
            formatY={(s) => formatDuration(s, lessThanMinuteLabel)}
            formatTooltip={(s) => formatDuration(s, lessThanMinuteLabel)}
          />
        ) : (
          <DailyTasksChart data={dailyTasks} />
        )}
      </div>
    </div>
  );
}
