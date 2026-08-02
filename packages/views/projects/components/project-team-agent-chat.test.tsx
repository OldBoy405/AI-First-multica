// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
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
// taskId -> messages, mutated per test (default: no messages, matching the
// original hardcoded stub) so the DD-4 filter tests can prove TimelineView
// really is gated on `filterOn` rather than just always-empty.
const messagesByTask: Record<string, unknown[]> = {};

vi.mock("@multica/core/chat/queries", () => ({
  taskMessagesOptions: (taskId: string) => ({
    queryKey: ["task-messages", taskId],
    queryFn: async () => messagesByTask[taskId] ?? [],
    enabled: true,
  }),
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

// ─── Model-selector seams (CR-2026-006 TASK-05 / TSUG-003) ─────────────────
// A single mutable config drives the four permission×runtime combos. Mutated
// per test, read lazily inside the mock factories (same pattern as `sendMock`).
const cfg: {
  canEdit: boolean;
  runtimeStatus: string;
  agent: { id: string; model: string; runtime_id: string } | null;
} = {
  canEdit: true,
  runtimeStatus: "online",
  agent: null,
};

// Stubbed so we don't pull the inspector's PropertyPicker tree into this test;
// the wrapper testids in the composer carry the state distinction, this only
// needs to expose canEdit + a way to fire onChange.
vi.mock("../../agents/components/inspector/model-picker", () => ({
  ModelPicker: ({
    canEdit,
    value,
    onChange,
  }: {
    canEdit: boolean;
    value: string;
    onChange: (m: string) => void;
  }) =>
    canEdit ? (
      <button
        data-testid="stub-model-interactive"
        onClick={() => onChange("claude-opus")}
      >
        {value}
      </button>
    ) : (
      <span data-testid="stub-model-readonly">{value}</span>
    ),
}));

vi.mock("@multica/core/permissions", () => ({
  useAgentPermissions: () => ({ canEdit: { allowed: cfg.canEdit, reason: "" } }),
}));

vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({
    queryKey: ["agents"],
    queryFn: async () => (cfg.agent ? [cfg.agent] : []),
  }),
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({
    queryKey: ["runtimes"],
    queryFn: async () => [{ id: "rt-1", status: cfg.runtimeStatus }],
  }),
  runtimeModelsOptions: (rid: string | null) => ({
    queryKey: ["models", rid],
    queryFn: async () => ({
      models: [{ id: "claude-opus", label: "Claude Opus" }],
      supported: true,
    }),
    enabled: !!rid,
  }),
}));

vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: { updateAgent: vi.fn().mockResolvedValue({}) },
  };
});

// ─── Project store + send mutation + queue status mocked ───────────────────
// Callable-store shape (selectorFn + getState), matching the Zustand mock
// convention used elsewhere in this repo (see chat-input.test.tsx).
const projectChatState = {
  drafts: {} as Record<string, string>,
  agentRequestFilter: {} as Record<string, boolean>,
  setDraft: vi.fn((projectId: string, mode: string, text: string) => {
    const key = `${projectId}:${mode}`;
    if (text) projectChatState.drafts[key] = text;
    else delete projectChatState.drafts[key];
  }),
  setAgentRequestFilter: vi.fn((projectId: string, value: boolean) => {
    projectChatState.agentRequestFilter[projectId] = value;
  }),
};
const sendMock = { mutateAsync: vi.fn(), isPending: false };
// CR-2026-007 T05: stop/cancel mutation shared instance, mirroring sendMock.
const cancelMock = { mutateAsync: vi.fn(), isPending: false };

vi.mock("@multica/core/projects", () => ({
  projectChatDraftKey: (projectId: string, mode: string) => `${projectId}:${mode}`,
  useProjectChatStore: Object.assign(
    (selector?: (s: typeof projectChatState) => unknown) =>
      selector ? selector(projectChatState) : projectChatState,
    { getState: () => projectChatState },
  ),
  useSendProjectChatMessage: () => sendMock,
  useCancelProjectQueueTask: () => cancelMock,
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

function task(id: string, at: string, overrides: Partial<AgentTask> = {}): AgentTask {
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
    ...overrides,
  };
}

beforeEach(() => {
  for (const k of Object.keys(projectChatState.drafts)) delete projectChatState.drafts[k];
  for (const k of Object.keys(projectChatState.agentRequestFilter)) {
    delete projectChatState.agentRequestFilter[k];
  }
  for (const k of Object.keys(messagesByTask)) delete messagesByTask[k];
  projectChatState.setDraft.mockClear();
  projectChatState.setAgentRequestFilter.mockClear();
  sendMock.mutateAsync = vi.fn();
  sendMock.isPending = false;
  cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "cancelled" });
  cancelMock.isPending = false;
  cfg.canEdit = true;
  cfg.runtimeStatus = "online";
  cfg.agent = null;
});

afterEach(() => {
  cleanup();
});

const streamBaseProps = {
  wsId: "ws-1",
  projectId: "proj-1",
  canConfigure: false,
  filterOn: false,
};

describe("TeamAgentStreamView", () => {
  it("coalesces comments and task execution cards in chronological order", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
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
    projectChatState.drafts["proj-1:team_agent"] = "hello agent";
    sendMock.mutateAsync = vi.fn().mockResolvedValue({ comment_id: "c1", task_id: "t1" });
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(sendMock.mutateAsync).toHaveBeenCalledWith("hello agent");
    expect(projectChatState.drafts["proj-1:team_agent"]).toBeUndefined();
  });

  it("renders a visible pending bubble immediately, cleared once the send settles", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "hello agent";
    let resolveSend!: (v: { comment_id: string; task_id: string }) => void;
    sendMock.mutateAsync = vi.fn(
      () => new Promise((resolve) => (resolveSend = resolve)),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);

    // Pending state must appear synchronously on click — before the request
    // settles — not only as a button spinner (CLAUDE.md pending-message rule).
    act(() => {
      screen.getByTestId("project-chat-send").click();
    });
    expect(screen.getByTestId("project-chat-pending-message").textContent).toBe(
      "hello agent",
    );

    await act(async () => {
      resolveSend({ comment_id: "c1", task_id: "t1" });
      await Promise.resolve();
    });
    expect(screen.queryByTestId("project-chat-pending-message")).toBeNull();
  });

  it("keeps the draft and toasts on 502 enqueue_failed", async () => {
    const { toast } = await import("sonner");
    projectChatState.drafts["proj-1:team_agent"] = "retry me";
    sendMock.mutateAsync = vi
      .fn()
      .mockRejectedValue(new ApiError("bad", 502, "Bad Gateway", { code: "enqueue_failed" }));
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(projectChatState.drafts["proj-1:team_agent"]).toBe("retry me");
    expect(toast.error).toHaveBeenCalled();
  });

  it("disables the input and shows depth/limit on 429 project_queue_full", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "busy send";
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
    expect(projectChatState.drafts["proj-1:team_agent"]).toBe("busy send");
    expect(screen.getByTestId("project-chat-composer-input")).toBeDisabled();
    expect(screen.getByTestId("project-chat-queue-full").textContent).toContain("5/5");
  });

  it("never locks the composer for owner/admin (canConfigure)", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "admin send";
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

// ─── TSUG-003: two disabled states must stay distinct ──────────────────────
// Decision order: permission first (selector interactivity), then runtime
// availability (selector content + send). The four combos below each render a
// distinct selector state, so "I can't edit" is never conflated with "no
// runtime configured".
describe("TeamAgentComposer model selector (TSUG-003)", () => {
  const props = {
    projectId: "proj-1",
    wsId: "ws-1",
    teamAgentId: "agent-1",
    canConfigure: false,
  };

  beforeEach(() => {
    cfg.agent = { id: "agent-1", model: "gpt-5", runtime_id: "rt-1" };
    projectChatState.drafts["proj-1:team_agent"] = "hi"; // isolate send-disable to runtime state
  });

  it("permission + runtime → interactive picker, send enabled", async () => {
    cfg.canEdit = true;
    cfg.runtimeStatus = "online";
    renderWithProviders(<TeamAgentComposer {...props} />);

    expect(await screen.findByTestId("project-chat-model-picker")).toBeTruthy();
    expect(screen.queryByTestId("project-chat-model-readonly")).toBeNull();
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    expect(screen.getByTestId("project-chat-send")).not.toBeDisabled();
  });

  it("permission + no runtime → runtime guide, send disabled", async () => {
    cfg.canEdit = true;
    cfg.runtimeStatus = "offline";
    renderWithProviders(<TeamAgentComposer {...props} />);

    const guide = await screen.findByTestId("project-chat-model-runtime-guide");
    expect(guide.textContent).toBe(
      enProjects.chat.stream.runtime_guide,
    );
    expect(screen.queryByTestId("project-chat-model-picker")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).toBeDisabled(),
    );
  });

  it("no permission + runtime → read-only badge, send enabled", async () => {
    cfg.canEdit = false;
    cfg.runtimeStatus = "online";
    renderWithProviders(<TeamAgentComposer {...props} />);

    expect(await screen.findByTestId("project-chat-model-readonly")).toBeTruthy();
    expect(screen.getByTestId("stub-model-readonly").textContent).toBe("gpt-5");
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    // The read-only badge renders as soon as the agent loads, so wait for the
    // runtime query to settle before asserting send is re-enabled.
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).not.toBeDisabled(),
    );
  });

  it("no permission + no runtime → read-only badge (not the guide), send disabled", async () => {
    cfg.canEdit = false;
    cfg.runtimeStatus = "offline";
    renderWithProviders(<TeamAgentComposer {...props} />);

    // Permission decides the selector: a non-editor sees the badge, never the
    // "start a runtime" guide — the two disabled states are not conflated.
    expect(await screen.findByTestId("project-chat-model-readonly")).toBeTruthy();
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).toBeDisabled(),
    );
  });

  it("selecting a model persists to the agent's model field", async () => {
    cfg.canEdit = true;
    cfg.runtimeStatus = "online";
    const { api } = await import("@multica/core/api");
    renderWithProviders(<TeamAgentComposer {...props} />);

    const trigger = await screen.findByTestId("stub-model-interactive");
    await act(async () => {
      trigger.click();
    });

    await waitFor(() =>
      expect(api.updateAgent).toHaveBeenCalledWith("agent-1", {
        model: "claude-opus",
      }),
    );
  });
});

// ─── CR-2026-007 T05: stop button, filter summary, withdrawn badge, copy ───

describe("TaskExecutionCard stop button (DD-1)", () => {
  it("shows Stop for the task's own originator on a running task", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.getByTestId("project-chat-task-stop")).toBeTruthy();
  });

  it("hides Stop on a running task started by someone else", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-2",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.queryByTestId("project-chat-task-stop")).toBeNull();
  });

  it("shows Stop for workspace owner/admin even when they aren't the originator", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        canConfigure
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-2",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.getByTestId("project-chat-task-stop")).toBeTruthy();
  });

  it("hides Stop once the task has left the running status (queued/completed)", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "queued",
            originator_user_id: "member-1",
          }),
          task("t2", "2026-01-01T00:00:01Z", {
            status: "completed",
            originator_user_id: "member-1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.queryByTestId("project-chat-task-stop")).toBeNull();
  });

  it("calls the cancel mutation with the task id and stays silent on a cancelled result", async () => {
    const { toast } = await import("sonner");
    // `toast.error` is a single module-level mock shared across every test in
    // this file (see the `vi.mock("sonner", …)` above); clear its call log so
    // this "no toast" assertion isn't tripped by an earlier, unrelated test.
    vi.mocked(toast.error).mockClear();
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );

    await act(async () => {
      screen.getByTestId("project-chat-task-stop").click();
    });

    expect(cancelMock.mutateAsync).toHaveBeenCalledWith("t1");
    expect(toast.error).not.toHaveBeenCalled();
  });

  it("toasts 'already finished' when the settled status isn't cancelled (TSUG-007)", async () => {
    cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "completed" });
    const { toast } = await import("sonner");
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );

    await act(async () => {
      screen.getByTestId("project-chat-task-stop").click();
    });

    expect(toast.error).toHaveBeenCalledWith(enProjects.chat.stream.cancel_already_finished);
  });

  it("toasts the server message when cancel throws an ApiError", async () => {
    cancelMock.mutateAsync = vi
      .fn()
      .mockRejectedValue(new ApiError("no access to this agent", 403, "Forbidden"));
    const { toast } = await import("sonner");
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "running",
            originator_user_id: "member-1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );

    await act(async () => {
      screen.getByTestId("project-chat-task-stop").click();
    });

    expect(toast.error).toHaveBeenCalledWith("no access to this agent");
  });
});

describe("TaskExecutionCard agent-requests-only filter (DD-4)", () => {
  it("renders TimelineView when the filter is off", async () => {
    messagesByTask["t1"] = [
      { task_id: "t1", issue_id: "issue-1", seq: 1, type: "text", content: "hi" },
    ];
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        filterOn={false}
        comments={[]}
        tasks={[task("t1", "2026-01-01T00:00:00Z", { status: "completed" })]}
        currentUserId="member-1"
      />,
    );
    expect(await screen.findByTestId("timeline-view")).toBeTruthy();
    expect(container.querySelector('[data-testid="project-chat-task-summary-output"]')).toBeNull();
  });

  it("hides TimelineView and renders only result.output as plain text when the filter is on", async () => {
    messagesByTask["t1"] = [
      { task_id: "t1", issue_id: "issue-1", seq: 1, type: "text", content: "hi" },
    ];
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        filterOn
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", {
            status: "completed",
            result: {
              output: "All done.",
              pr_url: "https://example.com/pr/1",
              tool_calls: [{ name: "bash" }],
            },
          }),
        ]}
        currentUserId="member-1"
      />,
    );

    // Give the (disabled) messages query a tick — it must not populate
    // TimelineView even if it somehow resolved.
    await Promise.resolve();
    expect(container.querySelector('[data-testid="timeline-view"]')).toBeNull();

    const summary = screen.getByTestId("project-chat-task-summary-output");
    expect(summary.textContent).toBe("All done.");
    // The whole `result` object must never be dumped into the DOM.
    expect(container.textContent).not.toContain("pr_url");
    expect(container.textContent).not.toContain("tool_calls");
    expect(container.textContent).not.toContain("bash");
  });

  it("shows the empty-output placeholder when result.output is an empty string", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        filterOn
        comments={[]}
        tasks={[
          task("t1", "2026-01-01T00:00:00Z", { status: "completed", result: { output: "" } }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.getByTestId("project-chat-task-summary-output").textContent).toBe(
      enProjects.chat.stream.no_text_reply,
    );
  });

  it("round-trips: toggling the filter on then off restores the original render", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const tasks = [task("t1", "2026-01-01T00:00:00Z", { status: "completed" })];
    const tree = (filterOn: boolean) => (
      <QueryClientProvider client={qc}>
        <I18nProvider locale="en" resources={RESOURCES}>
          <TeamAgentStreamView
            {...streamBaseProps}
            filterOn={filterOn}
            comments={[]}
            tasks={tasks}
            currentUserId="member-1"
          />
        </I18nProvider>
      </QueryClientProvider>
    );

    const { rerender, container } = render(tree(false));
    const before = container.innerHTML;

    rerender(tree(true));
    expect(screen.getByTestId("project-chat-task-summary-output")).toBeTruthy();

    rerender(tree(false));
    expect(container.innerHTML).toBe(before);
  });
});

describe("UserBubble withdrawn badge (DD-5)", () => {
  it("badges a comment whose id is the trigger_comment_id of a cancelled task", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[comment("c1", "2026-01-01T00:00:00Z")]}
        tasks={[
          task("t1", "2026-01-01T00:00:01Z", {
            status: "cancelled",
            trigger_comment_id: "c1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.getByTestId("project-chat-withdrawn-badge")).toBeTruthy();
  });

  it("does not badge a comment unrelated to any cancelled task", () => {
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[comment("c1", "2026-01-01T00:00:00Z")]}
        tasks={[
          task("t1", "2026-01-01T00:00:01Z", {
            status: "completed",
            trigger_comment_id: "c1",
          }),
        ]}
        currentUserId="member-1"
      />,
    );
    expect(screen.queryByTestId("project-chat-withdrawn-badge")).toBeNull();
  });
});

describe("UserBubble copy button (FR-7)", () => {
  const writeText = vi.fn().mockResolvedValue(undefined);

  beforeEach(() => {
    writeText.mockClear();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
  });

  it("copies the bubble's full text via the Clipboard API and toasts success", async () => {
    const { toast } = await import("sonner");
    renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[comment("c1", "2026-01-01T00:00:00Z")]}
        tasks={[]}
        currentUserId="member-1"
      />,
    );

    await act(async () => {
      screen.getByTestId("project-chat-copy").click();
      await Promise.resolve();
    });

    expect(writeText).toHaveBeenCalledWith("msg-c1");
    expect(toast.success).toHaveBeenCalledWith(enProjects.chat.stream.copied);
  });
});
