import { describe, expect, it } from "vitest";
import {
  SpecTraceResponseSchema,
  SpecSearchResponseSchema,
  DriftOverviewSchema,
  DriftFindingsResponseSchema,
  DriftFindingSchema,
  EMPTY_SPEC_TRACE,
  EMPTY_SPEC_SEARCH,
  EMPTY_DRIFT_OVERVIEW,
  EMPTY_DRIFT_FINDINGS,
} from "./schemas";

// AIFIRST: CR-2026-049 TASK-12 — trace/spec-search/drift schema tests.
// Valid payloads camelize; malformed payloads must NOT crash callers — the
// client layers parseWithFallback on top, so schemas only need to be strict
// where the server contract is strict (v, events array, findings array) and
// lenient on enum strings (unknown → "unknown" fallback).

describe("SpecTraceResponseSchema", () => {
  it("camelizes a valid timeline", () => {
    const parsed = SpecTraceResponseSchema.parse({
      v: 1,
      workspace_id: "ws",
      spec_id: "s",
      events: [
        {
          event_id: 1,
          cr_id: "CR-2026-001",
          commit_sha: "abc",
          occurred_at: "2026-08-20T12:00:00Z",
          state: "ok",
          milestone: {
            cr: "CR-2026-001",
            milestone: "M0",
            frs: [{ fr: "FR-1" }],
            merge_commits: [],
            evidence: null,
            source: "baseline-imported",
          },
        },
      ],
    });
    expect(parsed.workspaceId).toBe("ws");
    expect(parsed.events[0]!.eventId).toBe(1);
    expect(parsed.events[0]!.milestone?.frs).toHaveLength(1);
    expect(parsed.events[0]!.milestone?.source).toBe("baseline-imported");
  });

  it("keeps unknown state values (views render unknown fallback)", () => {
    const parsed = SpecTraceResponseSchema.parse({
      v: 1,
      workspace_id: "ws",
      spec_id: "s",
      events: [{ event_id: 1, cr_id: "x", commit_sha: "", occurred_at: null, state: "future-state" }],
    });
    expect(parsed.events[0]!.state).toBe("future-state");
  });

  it("tolerates missing event fields with safe defaults", () => {
    const parsed = SpecTraceResponseSchema.parse({ v: 1, workspace_id: "ws", spec_id: "s", events: [{}] });
    expect(parsed.events[0]!.state).toBe("malformed");
    expect(parsed.events[0]!.crId).toBe("");
  });

  it("exports a safe empty fallback", () => {
    expect(EMPTY_SPEC_TRACE.events).toEqual([]);
    expect(EMPTY_SPEC_TRACE.specId).toBe("");
  });
});

describe("SpecSearchResponseSchema", () => {
  it("camelizes specs and cursor", () => {
    const parsed = SpecSearchResponseSchema.parse({
      v: 1,
      specs: [{ spec_id: "alpha", latest_cr_id: "CR-2026-049", owners: { requirement: { id: "Ray" } }, updated_at: "t" }],
      next_cursor: "cur",
    });
    expect(parsed.specs[0]!.specId).toBe("alpha");
    expect(parsed.specs[0]!.owners).toEqual({ requirement: { id: "Ray" } });
    expect(parsed.nextCursor).toBe("cur");
  });
  it("empty envelope when specs missing", () => {
    const parsed = SpecSearchResponseSchema.parse({});
    expect(parsed.specs).toEqual([]);
    expect(EMPTY_SPEC_SEARCH.nextCursor).toBeNull();
  });
});

describe("DriftOverviewSchema", () => {
  it("camelizes the six-state overview", () => {
    const parsed = DriftOverviewSchema.parse({
      v: 1,
      scan_health: "ok",
      last_plan_status: "SUCCESS",
      last_success_at: "2026-08-20T12:00:00Z",
      repository_ids: ["tools"],
      bypass_count: 1,
      wip_on_trunk_count: 2,
      resolve_latency_ms: { sample_count: 3, p50: 1000, p90: 5000 },
    });
    expect(parsed.scanHealth).toBe("ok");
    expect(parsed.wipOnTrunkCount).toBe(2);
    expect(parsed.resolveLatencyMs.p50).toBe(1000);
  });
  it("unknown scan_health falls back to unknown, never crashes", () => {
    const parsed = DriftOverviewSchema.parse({ scan_health: "quantum" });
    expect(parsed.scanHealth).toBe("quantum");
  });
  it("empty fallback is safe", () => {
    expect(EMPTY_DRIFT_OVERVIEW.scanHealth).toBe("unknown");
    expect(EMPTY_DRIFT_OVERVIEW.resolveLatencyMs.p50).toBeNull();
  });
});

describe("DriftFindingsResponseSchema", () => {
  it("camelizes a keyset page", () => {
    const parsed = DriftFindingsResponseSchema.parse({
      v: 1,
      findings: [
        {
          id: "f1",
          repository_id: "tools",
          spec_id: null,
          cr_id: null,
          kind: "bypass-commit",
          severity: "warn",
          summary: "s",
          evidence: { commit_sha: "x" },
          status: "open",
          found_at: "2026-08-20T12:00:00Z",
          resolved_at: null,
        },
      ],
      next_cursor: null,
    });
    expect(parsed.findings[0]!.repositoryId).toBe("tools");
    expect(parsed.findings[0]!.kind).toBe("bypass-commit");
    expect(parsed.findings[0]!.resolvedAt).toBeNull();
  });
  it("unknown kind/status keep their string (fallback rendering)", () => {
    const f = DriftFindingSchema.parse({ id: "f", kind: "weird-kind", status: "weird-status" });
    expect(f.kind).toBe("weird-kind");
    expect(f.status).toBe("weird-status");
  });
  it("empty fallback is safe", () => {
    expect(EMPTY_DRIFT_FINDINGS.findings).toEqual([]);
  });
});
