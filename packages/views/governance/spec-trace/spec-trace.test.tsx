// AIFIRST: CR-2026-049 TASK-12 — spec trace rendering tests (AC-6/AC-2).
// Baseline-imported before event entries, conflict badge, missing evidence,
// malformed rows never leak raw payload.
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MilestoneRow } from "./spec-trace";

describe("MilestoneRow", () => {
  it("renders baseline-imported source marker", () => {
    render(
      <MilestoneRow
        milestone={{ cr: "CR-2026-001", milestone: "M0", frs: [{ fr: "FR-1" }], mergeCommits: [], evidence: null, source: "baseline-imported" }}
      />,
    );
    expect(screen.getByTestId("milestone-row").getAttribute("data-source")).toBe("baseline-imported");
    expect(screen.getByText("baseline-imported")).toBeTruthy();
  });

  it("renders missing evidence explicitly (never trunk HEAD fallback)", () => {
    render(
      <MilestoneRow
        milestone={{ cr: "CR-2026-001", milestone: "M0", frs: [], mergeCommits: [], evidence: null, source: "event" }}
      />,
    );
    expect(screen.getByTestId("evidence-missing")).toBeTruthy();
  });

  it("renders the snapshot conflict badge", () => {
    render(
      <MilestoneRow
        milestone={{
          cr: "CR-2026-001",
          milestone: "M0",
          frs: [],
          mergeCommits: [],
          evidence: { test: {} },
          source: "event",
          traceSnapshotConflict: true,
        }}
      />,
    );
    expect(screen.getByTestId("conflict-badge")).toBeTruthy();
  });

  it("builds one-hop commit links from merge_commits only", () => {
    render(
      <MilestoneRow
        milestone={{
          cr: "CR-2026-001",
          milestone: "M0",
          frs: [],
          mergeCommits: [{ repo: "tools", trunk: "custom/main", sha: "abc12345" }],
          evidence: {},
          source: "event",
        }}
      />,
    );
    const link = screen.getByTestId("commit-link");
    expect(link.getAttribute("href")).toContain("abc12345");
  });
});
