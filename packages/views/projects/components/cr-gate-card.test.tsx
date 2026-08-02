// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { GateNode, ProjectGateCR } from "@multica/core/api/schemas";
import enProjects from "../../locales/en/projects.json";
import enCommon from "../../locales/en/common.json";

const mockApproveCr = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return { ...actual, api: { approveCr: (...args: unknown[]) => mockApproveCr(...args) } };
});

import { ApiError } from "@multica/core/api";
import { CrGateCard } from "./cr-gate-card";

afterEach(() => {
  cleanup();
  mockApproveCr.mockReset();
});

const RESOURCES = { en: { projects: enProjects, common: enCommon } };

function renderCard(cr: ProjectGateCR, node: GateNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        <CrGateCard cr={cr} node={node} wsId="ws-1" projectId="proj-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

function baseCR(overrides: Partial<ProjectGateCR> = {}): ProjectGateCR {
  return {
    cr_id: "CR-2026-011",
    title: "Gate接合",
    status: "requirement-reviewing",
    needs_reconcile: false,
    updated_at: "2026-08-02T10:00:00Z",
    pending_stage: "requirement",
    can_approve: true,
    evidence: { "review-annotations/requirement.yml": "sha256:aabbccddee" },
    evidence_digest: "aabbccddeeffaabbccddeeff",
    key_id: "k1",
    pending_advance: false,
    gate_nodes: [],
    ...overrides,
  };
}

const approvalNode: GateNode = {
  node_id: "n1",
  kind: "human_approval",
  seq: 5,
  status: "running",
  stage: "requirement",
  attempt: 1,
  started_at: "2026-08-02T10:00:00Z",
};

describe("CrGateCard — ApprovalCard variant", () => {
  it("renders the CR id, stage, and evidence for a pending approval", () => {
    renderCard(baseCR(), approvalNode);
    expect(screen.getByTestId("cr-gate-approval-card")).toBeTruthy();
    expect(screen.getByText("CR-2026-011")).toBeTruthy();
    expect(screen.getByText("Requirement")).toBeTruthy();
    expect(screen.getByText(/review-annotations\/requirement\.yml/)).toBeTruthy();
  });

  it("shows a read-only waiting message when can_approve is false, with no buttons", () => {
    renderCard(baseCR({ can_approve: false }), approvalNode);
    expect(screen.getByTestId("cr-gate-readonly")).toBeTruthy();
    expect(screen.queryByText("Approve")).toBeNull();
    expect(screen.queryByText("Reject")).toBeNull();
  });

  it("shows the pending_advance message instead of buttons once a grant is issued (TSUG-001)", () => {
    renderCard(baseCR({ pending_advance: true }), approvalNode);
    expect(screen.getByTestId("cr-gate-pending-advance")).toBeTruthy();
    expect(screen.queryByText("Approve")).toBeNull();
  });

  it("shows a needs_reconcile warning inline", () => {
    renderCard(baseCR({ needs_reconcile: true }), approvalNode);
    expect(screen.getByTestId("cr-gate-needs-reconcile")).toBeTruthy();
  });

  it("submits an approve decision with the evidence digest", async () => {
    mockApproveCr.mockResolvedValue({
      grant: { v: 1, cr_id: "CR-2026-011", stage: "requirement", decision: "approve", approver: "u1", approved_at: "", evidence_digest: "aabbccddeeffaabbccddeeff", key_id: "k1", signature: "sig" },
    });
    renderCard(baseCR(), approvalNode);

    await act(async () => {
      screen.getByText("Approve").click();
    });

    expect(mockApproveCr).toHaveBeenCalledWith("ws-1", "CR-2026-011", {
      stage: "requirement",
      decision: "approve",
      reject_reason: undefined,
      evidence_digest: "aabbccddeeffaabbccddeeff",
    });
  });

  it("reveals a required reason field on reject, and blocks submit until filled", async () => {
    renderCard(baseCR(), approvalNode);
    const user = userEvent.setup();

    await user.click(screen.getByText("Reject"));
    expect(screen.getByTestId("cr-gate-reject-form")).toBeTruthy();
    const confirmButton = screen.getByText("Confirm reject");
    expect(confirmButton.closest("button")).toBeDisabled();

    await user.type(screen.getByPlaceholderText("Reason for rejection…"), "not ready");
    expect(confirmButton.closest("button")).not.toBeDisabled();

    mockApproveCr.mockResolvedValue({ grant: {} });
    await act(async () => {
      confirmButton.click();
    });
    expect(mockApproveCr).toHaveBeenCalledWith("ws-1", "CR-2026-011", {
      stage: "requirement",
      decision: "reject",
      reject_reason: "not ready",
      evidence_digest: "aabbccddeeffaabbccddeeff",
    });
  });

  it("shows the EVIDENCE_DRIFT message on a 409 without crashing", async () => {
    mockApproveCr.mockRejectedValue(
      new ApiError("conflict", 409, "Conflict", { error: "EVIDENCE_DRIFT", expected: "a", current: "b" }),
    );
    renderCard(baseCR(), approvalNode);

    await act(async () => {
      screen.getByText("Approve").click();
    });

    expect(screen.getByTestId("cr-gate-error").textContent).toMatch(/Evidence changed/);
  });

  it("shows the forbidden message on a 403", async () => {
    mockApproveCr.mockRejectedValue(new ApiError("forbidden", 403, "Forbidden", { error: "FORBIDDEN_APPROVER" }));
    renderCard(baseCR(), approvalNode);

    await act(async () => {
      screen.getByText("Approve").click();
    });

    expect(screen.getByTestId("cr-gate-error").textContent).toMatch(/permission/);
  });
});

describe("CrGateCard — BlockedCard variant", () => {
  it("renders the blocker list and attempt count", () => {
    const node: GateNode = {
      node_id: "n2",
      kind: "skill",
      seq: 4,
      status: "blocked",
      stage: "requirement",
      attempt: 2,
      detail: {
        blockers: [{ id: "REQ-BLOCK-001", location: "FR-3", issue: "not testable", suggestion: "add a number" }],
      },
    };
    renderCard(baseCR(), node);
    expect(screen.getByTestId("cr-gate-blocked-card")).toBeTruthy();
    expect(screen.getByText(/not testable/)).toBeTruthy();
    expect(screen.getByText("Attempt 2/3")).toBeTruthy();
  });
});

describe("CrGateCard — HistoryRow variant", () => {
  it("renders a collapsed passed row and expands blockers on click when present", async () => {
    const node: GateNode = {
      node_id: "n3",
      kind: "skill",
      seq: 4,
      status: "passed",
      stage: "requirement",
      attempt: 2,
      detail: { blockers: [{ id: "b1", location: "FR-1", issue: "fixed now" }] },
    };
    renderCard(baseCR(), node);
    expect(screen.getByTestId("cr-gate-history-row")).toBeTruthy();
    expect(screen.getByText("Review passed")).toBeTruthy();
    expect(screen.queryByText(/fixed now/)).toBeNull();

    const user = userEvent.setup();
    await user.click(screen.getByText("Review passed"));
    expect(screen.getByText(/fixed now/)).toBeTruthy();
  });

  it("labels a passed human_approval node by its OWN stage, not the CR's current pending_stage", () => {
    // Regression: cr.pending_stage has moved on to tech-design, but this
    // node is the requirement gate's own history — must still say Requirement.
    const node: GateNode = {
      node_id: "n1",
      kind: "human_approval",
      seq: 5,
      status: "passed",
      stage: "requirement",
      attempt: 1,
    };
    renderCard(baseCR({ pending_stage: "tech-design" }), node);
    expect(screen.getByText("Requirement")).toBeTruthy();
  });

  it("shows Cancelled for a failed node", () => {
    const node: GateNode = {
      node_id: "n1",
      kind: "human_approval",
      seq: 5,
      status: "failed",
      stage: "requirement",
      attempt: 1,
    };
    renderCard(baseCR(), node);
    expect(screen.getByText("Cancelled")).toBeTruthy();
  });
});
