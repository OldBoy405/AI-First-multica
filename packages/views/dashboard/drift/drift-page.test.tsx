// AIFIRST: CR-2026-049 TASK-12 — drift card/list rendering tests (AC-2/AC-3).
// Health states are separated from "无漂移"; unknown health renders the
// fallback copy; terminal finding states disable PATCH buttons.
import { describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { DriftCard, FindingRow, healthCopy } from "./drift-page";
import type { DriftFinding } from "@multica/core/types";

function withQuery(ui: React.ReactNode) {
  return render(<QueryClientProvider client={new QueryClient()}>{ui}</QueryClientProvider>);
}

vi.mock("@multica/core", () => ({ useWorkspaceId: () => "ws-1" }));

describe("healthCopy", () => {
  it("separates ok-with-zero from the six states", () => {
    expect(healthCopy("ok")).toBe("无漂移");
    expect(healthCopy("uninitialized")).toBe("扫描尚未初始化");
    expect(healthCopy("failed")).toBe("最近一次扫描失败");
    expect(healthCopy("stale")).toBe("扫描数据已过期");
    expect(healthCopy("not_configured")).toBe("未配置平台仓库");
    expect(healthCopy("something-new")).toBe("状态未知");
  });
});

describe("DriftCard", () => {
  it("renders without crashing on missing data", () => {
    withQuery(<DriftCard />);
    expect(screen.getByTestId("drift-card")).toBeTruthy();
  });
});

describe("FindingRow", () => {
  const base: DriftFinding = {
    id: "f1",
    repositoryId: "tools",
    specId: null,
    crId: null,
    kind: "bypass-commit",
    severity: "warn",
    summary: "bypass",
    evidence: {},
    status: "open",
    foundAt: "2026-08-20T12:00:00Z",
    resolvedAt: null,
  };

  it("open finding offers acknowledged/resolved/wontfix", () => {
    render(<FindingRow finding={base} onPatch={vi.fn()} />);
    expect(screen.getByTestId("patch-acknowledged")).toBeTruthy();
    expect(screen.getByTestId("patch-resolved")).toBeTruthy();
    expect(screen.getByTestId("patch-wontfix")).toBeTruthy();
  });

  it("terminal states render the badge and offer no PATCH buttons", () => {
    render(<FindingRow finding={{ ...base, status: "resolved" }} onPatch={vi.fn()} />);
    expect(screen.getByTestId("terminal-badge")).toBeTruthy();
    expect(screen.queryByTestId("patch-acknowledged")).toBeNull();
  });

  it("unknown status never crashes (fallback rendering)", () => {
    render(<FindingRow finding={{ ...base, status: "unknown", kind: "unknown" }} onPatch={vi.fn()} />);
    expect(screen.getByTestId("finding-row").getAttribute("data-status")).toBe("unknown");
  });
});
