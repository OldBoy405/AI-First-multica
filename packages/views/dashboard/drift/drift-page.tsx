"use client";

// AIFIRST: CR-2026-049 TASK-12 — drift governance card + findings list
// (SDD §3.6/§3.7). The card separates the six health states from "no drift":
// only ok + zero unresolved findings renders "无漂移"; uninitialized/failed/
// stale/not_configured each render their own copy. The list uses keyset
// pagination and PATCH state buttons (terminal states disabled).

import { useCallback, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core";
import { api } from "@multica/core/api";
import { driftOverviewOptions, driftFindingsOptions, driftKeys } from "@multica/core/drift/queries";
import type { DriftFinding, DriftFindingStatus, DriftScanHealth } from "@multica/core/types";

const noDriftLabel = "无漂移";

const HEALTH_COPY: Record<DriftScanHealth, string> = {
  ok: "无漂移",
  not_configured: "未配置平台仓库",
  uninitialized: "扫描尚未初始化",
  failed: "最近一次扫描失败",
  stale: "扫描数据已过期",
  unknown: "状态未知",
};

export function healthCopy(health: string): string {
  return HEALTH_COPY[health as DriftScanHealth] ?? "状态未知";
}

export function DriftCard() {
  const wsId = useWorkspaceId();
  const overview = useQuery(driftOverviewOptions(wsId));
  const o = overview.data;
  const unresolved = (o?.bypassCount ?? 0) + (o?.wipOnTrunkCount ?? 0);
  const clean = (o?.scanHealth ?? "unknown") === "ok" && unresolved === 0;
  const countsLabel = `bypass ${o?.bypassCount ?? 0} · wip ${o?.wipOnTrunkCount ?? 0}`;
  const latencyLabel =
    o?.resolveLatencyMs.p50 != null ? `p50 ${o.resolveLatencyMs.p50}ms` : "resolve latency: no samples";
  return (
    <div data-testid="drift-card">
      <div data-testid="drift-health">{healthCopy(o?.scanHealth ?? "unknown")}</div>
      {clean && <div data-testid="drift-clean">{noDriftLabel}</div>}
      {!clean && o && o.scanHealth !== "ok" && <div data-testid="drift-health-reason">{healthCopy(o.scanHealth)}</div>}
      <div data-testid="drift-counts">{countsLabel}</div>
      <div data-testid="drift-latency">{latencyLabel}</div>
    </div>
  );
}

const loadMoreLabel = "load more";
const allLabel = "all";
const openLabel = "open";
const acknowledgedLabel = "acknowledged";
const resolvedLabel = "resolved";
const wontfixLabel = "wontfix";

const NEXT_FOR: Record<string, DriftFindingStatus[]> = {
  open: ["acknowledged", "resolved", "wontfix"],
  acknowledged: ["resolved", "wontfix"],
};

export function FindingRow({ finding, onPatch }: { finding: DriftFinding; onPatch: (id: string, from: string, to: string) => void }) {
  const next = NEXT_FOR[finding.status] ?? [];
  return (
    <div data-testid="finding-row" data-status={finding.status} data-kind={finding.kind}>
      <div>
        {finding.repositoryId} · {finding.kind} · {finding.severity}
      </div>
      <div>{finding.summary}</div>
      {next.map((to) => {
        const label = `→ ${to}`;
        return (
          <button
            key={to}
            data-testid={`patch-${to}`}
            onClick={() => onPatch(finding.id, finding.status, to)}
          >
            {label}
          </button>
        );
      })}
      {(finding.status === "resolved" || finding.status === "wontfix") && <span data-testid="terminal-badge">{finding.status}</span>}
    </div>
  );
}

export function DriftFindingsList({ status, kind }: { status?: string; kind?: string }) {
  const wsId = useWorkspaceId();
  const queryClient = useQueryClient();
  const params = useMemo(() => ({ status, kind, limit: 20 }), [status, kind]);
  const pages = useInfiniteQuery({
    ...driftFindingsOptions(wsId, params),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor ?? undefined,
  });
  const findings = pages.data?.pages.flatMap((p) => p.findings) ?? [];
  const onPatch = useCallback(
    async (id: string, from: string, to: string) => {
      await api.patchDriftFinding(wsId, id, { fromStatus: from, toStatus: to });
      await queryClient.invalidateQueries({ queryKey: driftKeys.all(wsId) });
    },
    [wsId, queryClient],
  );
  return (
    <div data-testid="drift-findings-list">
      {findings.map((f) => (
        <FindingRow key={f.id} finding={f} onPatch={onPatch} />
      ))}
      {pages.hasNextPage && (
        <button data-testid="load-more" onClick={() => pages.fetchNextPage()}>
          {loadMoreLabel}
        </button>
      )}
    </div>
  );
}

export function DriftPage() {
  const [status, setStatus] = useState("");
  return (
    <div className="p-6" data-testid="drift-page">
      <DriftCard />
      <select data-testid="status-filter" value={status} onChange={(e) => setStatus(e.target.value)}>
        <option value="">{allLabel}</option>
        <option value="open">{openLabel}</option>
        <option value="acknowledged">{acknowledgedLabel}</option>
        <option value="resolved">{resolvedLabel}</option>
        <option value="wontfix">{wontfixLabel}</option>
      </select>
      <DriftFindingsList status={status || undefined} />
    </div>
  );
}
