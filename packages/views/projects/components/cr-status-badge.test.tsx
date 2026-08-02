import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const mockGetProjectGates = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getProjectGates: (...args: unknown[]) => mockGetProjectGates(...args),
  },
}));

import { CrStatusBadge } from "./cr-status-badge";

function renderBadge() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <CrStatusBadge wsId="ws-1" projectId="proj-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

function gateCR(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    cr_id: "CR-2026-011",
    title: "Gate接合",
    status: "requirement-reviewing",
    needs_reconcile: false,
    updated_at: "2026-08-02T10:00:00Z",
    pending_stage: "requirement",
    can_approve: true,
    evidence: {},
    evidence_digest: "",
    key_id: "",
    pending_advance: false,
    gate_nodes: [],
    ...overrides,
  };
}

describe("CrStatusBadge (CR-2026-011 TASK-05, SDD DD-7)", () => {
  beforeEach(() => {
    mockGetProjectGates.mockReset();
  });

  it("renders nothing when there are no in-flight CRs", async () => {
    mockGetProjectGates.mockResolvedValue({ crs: [] });
    renderBadge();
    await waitFor(() => {
      expect(mockGetProjectGates).toHaveBeenCalled();
    });
    expect(screen.queryByTestId("cr-status-badge")).toBeNull();
  });

  it("shows the CR id and translated status label for a single CR", async () => {
    mockGetProjectGates.mockResolvedValue({ crs: [gateCR()] });
    renderBadge();
    await waitFor(() => {
      expect(screen.getByTestId("cr-status-badge")).toBeTruthy();
    });
    expect(screen.getByText("CR-2026-011")).toBeTruthy();
    expect(screen.getByText("Requirement Review")).toBeTruthy();
    // Single CR: no popover trigger wrapper.
    expect(screen.queryByTestId("cr-status-badge-trigger")).toBeNull();
  });

  it("falls back to the raw status string for an unrecognized status", async () => {
    mockGetProjectGates.mockResolvedValue({
      crs: [gateCR({ status: "some-future-status" })],
    });
    renderBadge();
    await waitFor(() => {
      expect(screen.getByText("some-future-status")).toBeTruthy();
    });
  });

  it("shows a needs_reconcile hint when the projection is catching up", async () => {
    mockGetProjectGates.mockResolvedValue({ crs: [gateCR({ needs_reconcile: true })] });
    renderBadge();
    await waitFor(() => {
      expect(screen.getByTestId("cr-needs-reconcile")).toBeTruthy();
    });
  });

  it("picks the most recently updated CR as primary and lists the rest in a popover", async () => {
    mockGetProjectGates.mockResolvedValue({
      crs: [
        gateCR({ cr_id: "CR-2026-010", status: "developing", updated_at: "2026-08-01T00:00:00Z" }),
        gateCR({ cr_id: "CR-2026-011", status: "requirement-reviewing", updated_at: "2026-08-02T00:00:00Z" }),
      ],
    });
    renderBadge();
    await waitFor(() => {
      expect(screen.getByTestId("cr-status-badge-trigger")).toBeTruthy();
    });
    // Primary (most recent) shows directly in the trigger.
    expect(screen.getByText("CR-2026-011")).toBeTruthy();
    expect(screen.queryByText("CR-2026-010")).toBeNull();

    await userEvent.click(screen.getByTestId("cr-status-badge-trigger"));
    await waitFor(() => {
      expect(screen.getByText("CR-2026-010")).toBeTruthy();
    });
  });

  it("renders nothing while gates data has not loaded", () => {
    mockGetProjectGates.mockReturnValue(new Promise(() => {}));
    renderBadge();
    expect(screen.queryByTestId("cr-status-badge")).toBeNull();
  });
});
