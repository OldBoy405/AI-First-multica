// @vitest-environment jsdom
import type { ComponentProps } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import type { QueueStatusWithItems } from "@multica/core/api/schemas";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

// Keep the real ApiError class (used via `instanceof` in the component) but
// stub `api.getBaseUrl()`, which `resolvePublicFileUrl` calls unconditionally
// as an eager argument even when the avatar URL itself is null.
vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: { getBaseUrl: () => "http://127.0.0.1:8080" },
  };
});

// Mutable fixture read lazily by the mocked queryFn (same pattern as
// project-team-agent-chat.test.tsx's `cfg` object).
let queueData: QueueStatusWithItems = { queue_depth: 0, queue_limit: 10, items: [] };

const cancelMock: {
  mutateAsync: ReturnType<typeof vi.fn>;
  isPending: boolean;
  variables: string | undefined;
} = { mutateAsync: vi.fn(), isPending: false, variables: undefined };

vi.mock("@multica/core/projects", () => ({
  projectQueueItemsOptions: (wsId: string, id: string) => ({
    queryKey: ["queue-items", wsId, id],
    queryFn: () => Promise.resolve(queueData),
  }),
  useCancelProjectQueueTask: () => cancelMock,
}));

import { ProjectQueueBar } from "./project-queue-bar";

function twoItemQueue(): QueueStatusWithItems {
  return {
    queue_depth: 2,
    queue_limit: 10,
    items: [
      {
        task_id: "t1",
        status: "queued",
        priority: 100,
        created_at: "2026-08-01T00:00:00Z",
        originator: { id: "user-1", name: "Alice", avatar_url: null },
        summary: "Fix the login bug",
      },
      {
        task_id: "t2",
        status: "dispatched",
        priority: 100,
        created_at: "2026-08-01T00:01:00Z",
        originator: null,
        summary: "",
      },
    ],
  };
}

function renderBar(props: Partial<ComponentProps<typeof ProjectQueueBar>> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ProjectQueueBar
          wsId="ws-1"
          projectId="proj-1"
          currentUserId="user-1"
          canConfigure={false}
          {...props}
        />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

async function expandList() {
  fireEvent.click(await screen.findByTestId("project-queue-bar-toggle"));
  return screen.getAllByTestId("project-queue-bar-item");
}

describe("ProjectQueueBar (CR-2026-007 TASK-04)", () => {
  beforeEach(() => {
    queueData = { queue_depth: 0, queue_limit: 10, items: [] };
    cancelMock.mutateAsync = vi.fn();
    cancelMock.isPending = false;
    cancelMock.variables = undefined;
  });

  afterEach(cleanup);

  it("renders nothing while the queue is empty (count === 0 collapses)", async () => {
    const { container } = renderBar();
    await waitFor(() =>
      expect(container.querySelector('[data-testid="project-queue-bar"]')).toBeNull(),
    );
  });

  it("shows the queue depth count, collapsed by default", async () => {
    queueData = twoItemQueue();
    renderBar();

    const toggle = await screen.findByTestId("project-queue-bar-toggle");
    expect(toggle.textContent).toContain("2 tasks queued");
    expect(screen.queryByTestId("project-queue-bar-list")).toBeNull();
  });

  it("expands to list items with originator/summary/status, using placeholders for null originator and empty summary", async () => {
    queueData = twoItemQueue();
    renderBar();

    const items = await expandList();
    expect(items).toHaveLength(2);
    const [first, second] = items as [HTMLElement, HTMLElement];
    expect(first.textContent).toContain("Alice");
    expect(first.textContent).toContain("Fix the login bug");
    expect(first.textContent).toContain("Queued");
    expect(second.textContent).toContain("System task");
    expect(second.textContent).toContain("(No message summary)");
    expect(second.textContent).toContain("Dispatched");
  });

  it("shows the clear-conversation button only on the current user's own item", async () => {
    queueData = twoItemQueue();
    renderBar({ currentUserId: "user-1", canConfigure: false });

    const items = await expandList();
    const [first, second] = items as [HTMLElement, HTMLElement];
    expect(first.querySelector('[data-testid="project-queue-bar-cancel"]')).not.toBeNull();
    // item2 has a null originator and belongs to nobody in particular.
    expect(second.querySelector('[data-testid="project-queue-bar-cancel"]')).toBeNull();
  });

  it("lets an owner/admin clear any item, including one with no originator", async () => {
    queueData = twoItemQueue();
    renderBar({ currentUserId: "someone-else", canConfigure: true });

    const items = await expandList();
    const [first, second] = items as [HTMLElement, HTMLElement];
    expect(first.querySelector('[data-testid="project-queue-bar-cancel"]')).not.toBeNull();
    expect(second.querySelector('[data-testid="project-queue-bar-cancel"]')).not.toBeNull();
  });

  it("calls the cancel mutation with the task id and stays silent on a cancelled result", async () => {
    queueData = twoItemQueue();
    cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "cancelled" });
    const { toast } = await import("sonner");
    renderBar({ currentUserId: "user-1" });

    const items = await expandList();
    const button = (items[0] as HTMLElement).querySelector(
      '[data-testid="project-queue-bar-cancel"]',
    ) as HTMLElement;
    await act(async () => {
      fireEvent.click(button);
    });

    expect(cancelMock.mutateAsync).toHaveBeenCalledWith("t1");
    expect(toast.error).not.toHaveBeenCalled();
  });

  it("toasts the already-finished message on a non-cancelled terminal result (TSUG-007 branch 2)", async () => {
    queueData = twoItemQueue();
    cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "completed" });
    const { toast } = await import("sonner");
    renderBar({ currentUserId: "user-1" });

    const items = await expandList();
    const button = (items[0] as HTMLElement).querySelector(
      '[data-testid="project-queue-bar-cancel"]',
    ) as HTMLElement;
    await act(async () => {
      fireEvent.click(button);
    });

    expect(toast.error).toHaveBeenCalledWith(enProjects.chat.queue_bar.cancel_already_finished);
  });

  it("toasts the thrown ApiError's message (branch 3, e.g. 403)", async () => {
    queueData = twoItemQueue();
    cancelMock.mutateAsync = vi
      .fn()
      .mockRejectedValue(new ApiError("not allowed to cancel this task", 403, "Forbidden"));
    const { toast } = await import("sonner");
    renderBar({ currentUserId: "user-1" });

    const items = await expandList();
    const button = (items[0] as HTMLElement).querySelector(
      '[data-testid="project-queue-bar-cancel"]',
    ) as HTMLElement;
    await act(async () => {
      fireEvent.click(button);
    });

    expect(toast.error).toHaveBeenCalledWith("not allowed to cancel this task");
  });
});
