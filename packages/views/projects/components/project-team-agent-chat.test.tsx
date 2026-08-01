import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import type { AgentTask, TimelineEntry } from "@multica/core/types";
import enProjects from "../../locales/en/projects.json";
import enCommon from "../../locales/en/common.json";

// ─── Heavy presentational deps stubbed to keep the render pure ─────────────
vi.mock("../../editor", () => ({
  ReadonlyContent: ({ content }: { content: string }) => <div>{content}</div>,
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <div data-testid="avatar" />,
}));
vi.mock("../../chat/components/chat-message-list", () => ({
  TimelineView: ({ items }: { items: unknown[] }) => (
    <div data-testid="timeline-view">{items.length}</div>
  ),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Someone" }),
}));
vi.mock("@multica/core/chat/queries", () => ({
  taskMessagesOptions: (taskId: string) => ({
    queryKey: ["task-messages", taskId],
    queryFn: async () => [],
    enabled: true,
  }),
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

// ─── Project store + send mutation + queue status mocked ───────────────────
const drafts: Record<string, string> = {};
const setDraft = vi.fn((projectId: string, mode: string, text: string) => {
  const key = `${projectId}:${mode}`;
  if (text) drafts[key] = text;
  else delete drafts[key];
});
const sendMock = { mutateAsync: vi.fn(), isPending: false };

vi.mock("@multica/core/projects", () => ({
  projectChatDraftKey: (projectId: string, mode: string) => `${projectId}:${mode}`,
  useProjectChatStore: (selector: (s: unknown) => unknown) =>
    selector({ drafts, setDraft }),
  useSendProjectChatMessage: () => sendMock,
  // Never resolves — keeps `queue` undefined so the live-queue path never
  // interferes with the tests that drive the 429 latch explicitly.
  projectQueueStatusOptions: (_wsId: string, id: string) => ({
    queryKey: ["queue-status", id],
    queryFn: () => new Promise(() => {}),
  }),
}));

import {
  TeamAgentComposer,
  TeamAgentStreamView,
} from "./project-team-agent-chat";

const RESOURCES = { en: { projects: enProjects, common: enCommon } };

function renderWithProviders(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        {ui}
      </I18nProvider>
    </QueryClientProvider>,
  );
}

function comment(id: string, at: string): TimelineEntry {
  return {
    type: "comment",
    id,
    actor_type: "member",
    actor_id: "member-1",
    created_at: at,
    content: `msg-${id}`,
  };
}

function task(id: string, at: string): AgentTask {
  return {
    id,
    agent_id: "agent-1",
    runtime_id: "rt-1",
    issue_id: "issue-1",
    status: "completed",
    priority: 0,
    dispatched_at: null,
    started_at: at,
    completed_at: at,
    result: null,
    error: null,
    created_at: at,
  };
}

beforeEach(() => {
  for (const k of Object.keys(drafts)) delete drafts[k];
  setDraft.mockClear();
  sendMock.mutateAsync = vi.fn();
  sendMock.isPending = false;
});

describe("TeamAgentStreamView", () => {
  it("coalesces comments and task execution cards in chronological order", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        comments={[comment("c1", "2026-01-01T00:00:01.500Z")]}
        tasks={[
          task("t1", "2026-01-01T00:00:01.000Z"),
          task("t2", "2026-01-01T00:00:02.000Z"),
        ]}
        currentUserId="member-1"
      />,
    );

    // Pinned "no earlier messages" divider is always present.
    expect(screen.getByTestId("project-chat-no-earlier")).toBeTruthy();

    const nodes = Array.from(
      container.querySelectorAll(
        '[data-testid="project-chat-task-card"],[data-testid="project-chat-user-bubble"]',
      ),
    ).map((el) => el.getAttribute("data-testid"));

    // task(1.0s) → comment(1.5s) → task(2.0s): the two streams interleave.
    expect(nodes).toEqual([
      "project-chat-task-card",
      "project-chat-user-bubble",
      "project-chat-task-card",
    ]);
  });
});

describe("TeamAgentComposer", () => {
  const props = { projectId: "proj-1", wsId: "ws-1", canConfigure: false };

  it("clears the draft on a successful send", async () => {
    drafts["proj-1:team_agent"] = "hello agent";
    sendMock.mutateAsync = vi.fn().mockResolvedValue({ comment_id: "c1", task_id: "t1" });
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(sendMock.mutateAsync).toHaveBeenCalledWith("hello agent");
    expect(drafts["proj-1:team_agent"]).toBeUndefined();
  });

  it("keeps the draft and toasts on 502 enqueue_failed", async () => {
    const { toast } = await import("sonner");
    drafts["proj-1:team_agent"] = "retry me";
    sendMock.mutateAsync = vi
      .fn()
      .mockRejectedValue(new ApiError("bad", 502, "Bad Gateway", { code: "enqueue_failed" }));
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(drafts["proj-1:team_agent"]).toBe("retry me");
    expect(toast.error).toHaveBeenCalled();
  });

  it("disables the input and shows depth/limit on 429 project_queue_full", async () => {
    drafts["proj-1:team_agent"] = "busy send";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("full", 429, "Too Many Requests", {
        code: "project_queue_full",
        queue_depth: 5,
        queue_limit: 5,
      }),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    await waitFor(() => {
      expect(screen.getByTestId("project-chat-queue-full")).toBeTruthy();
    });
    // Draft is retained for the resend, input is locked, depth/limit surfaced.
    expect(drafts["proj-1:team_agent"]).toBe("busy send");
    expect(screen.getByTestId("project-chat-composer-input")).toBeDisabled();
    expect(screen.getByTestId("project-chat-queue-full").textContent).toContain("5/5");
  });

  it("never locks the composer for owner/admin (canConfigure)", async () => {
    drafts["proj-1:team_agent"] = "admin send";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("full", 429, "Too Many Requests", {
        code: "project_queue_full",
        queue_depth: 5,
        queue_limit: 5,
      }),
    );
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(screen.queryByTestId("project-chat-queue-full")).toBeNull();
    expect(screen.getByTestId("project-chat-composer-input")).not.toBeDisabled();
  });
});
