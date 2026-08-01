import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const mockGetQueueStatus = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getProjectQueueStatus: (...args: unknown[]) => mockGetQueueStatus(...args),
  },
}));

import { ProjectQueueStatus } from "./project-queue-status";

function renderStatus() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ProjectQueueStatus wsId="ws-1" projectId="proj-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("ProjectQueueStatus (CR-2026-004 FR-5)", () => {
  beforeEach(() => {
    mockGetQueueStatus.mockReset();
  });

  it("shows depth/limit without the full-queue hint when below capacity", async () => {
    mockGetQueueStatus.mockResolvedValue({ queue_depth: 3, queue_limit: 50 });
    renderStatus();
    await waitFor(() => {
      expect(screen.getByTestId("project-queue-status")).toBeTruthy();
    });
    expect(screen.getByText(/3\/50/)).toBeTruthy();
    expect(screen.queryByTestId("project-queue-full-hint")).toBeNull();
  });

  it("shows the busy hint when the queue is at capacity", async () => {
    mockGetQueueStatus.mockResolvedValue({ queue_depth: 2, queue_limit: 2 });
    renderStatus();
    await waitFor(() => {
      expect(screen.getByTestId("project-queue-full-hint")).toBeTruthy();
    });
    expect(screen.getByText(/2\/2/)).toBeTruthy();
  });

  it("renders nothing while the status has not loaded", async () => {
    mockGetQueueStatus.mockReturnValue(new Promise(() => {}));
    renderStatus();
    expect(screen.queryByTestId("project-queue-status")).toBeNull();
  });
});
