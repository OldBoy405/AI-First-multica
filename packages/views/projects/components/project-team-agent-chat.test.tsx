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

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "member-1" } }),
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
  setDraft: vi.fn((projectId: string, mode: string, text: string) => {
    const key = `${projectId}:${mode}`;
    if (text) projectChatState.drafts[key] = text;
    else delete projectChatState.drafts[key];
  }),
};
const sendMock = { mutateAsync: vi.fn(), isPending: false };
const requestPresenterMock = { mutate: vi.fn(), isPending: false };
// CR-2026-010: resolves to "no presenter" by default so existing tests (which
// predate the presenter guard) keep seeing an unlocked composer; the
// presenter-guard describe block below overrides this per test.
let presenterStateMock: {
  presenter: { user_id: string } | null;
  pending_requests: unknown[];
  my_request: unknown | null;
} = { presenter: null, pending_requests: [], my_request: null };

vi.mock("@multica/core/projects", () => ({
  projectChatDraftKey: (projectId: string, mode: string) => `${projectId}:${mode}`,
  useProjectChatStore: Object.assign(
    (selector?: (s: typeof projectChatState) => unknown) =>
      selector ? selector(projectChatState) : projectChatState,
    { getState: () => projectChatState },
  ),
  useSendProjectChatMessage: () => sendMock,
  useRequestPresenter: () => requestPresenterMock,
  // Never resolves — keeps `queue` undefined so the live-queue path never
  // interferes with the tests that drive the 429 latch explicitly.
  projectQueueStatusOptions: (_wsId: string, id: string) => ({
    queryKey: ["queue-status", id],
    queryFn: () => new Promise(() => {}),
  }),
  projectPresenterOptions: (_wsId: string, id: string) => ({
    queryKey: ["presenter", id],
    queryFn: async () => presenterStateMock,
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

function activity(
  id: string,
  action: string,
  details: Record<string, string>,
  at: string,
): TimelineEntry {
  return {
    type: "activity",
    id,
    actor_type: "member",
    actor_id: details.by_user_id ?? "member-1",
    action,
    details,
    created_at: at,
  };
}

// useActorName is globally stubbed in this file to always return "Someone"
// regardless of the id passed, so every {{var}} in a notice template resolves
// to the same name — this fills the template the same way for assertions.
function fillNoticeTemplate(tpl: string): string {
  return tpl.replace(/\{\{\w+\}\}/g, "Someone");
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
  for (const k of Object.keys(projectChatState.drafts)) delete projectChatState.drafts[k];
  projectChatState.setDraft.mockClear();
  sendMock.mutateAsync = vi.fn();
  sendMock.isPending = false;
  cfg.canEdit = true;
  cfg.runtimeStatus = "online";
  cfg.agent = null;
  requestPresenterMock.mutate.mockClear();
  requestPresenterMock.isPending = false;
  presenterStateMock = { presenter: null, pending_requests: [], my_request: null };
});

afterEach(() => {
  cleanup();
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

// CR-2026-010 TASK-06 AC2: six activity actions each render a distinct notice
// card with matching copy; existing comment/task cards are unaffected.
describe("TeamAgentStreamView presenter notices", () => {
  const NOTICES = [
    activity("a1", "presenter_requested", { to_user_id: "u1", by_user_id: "u1" }, "2026-01-01T00:00:00Z"),
    activity("a2", "presenter_approved", { to_user_id: "u1", by_user_id: "u2" }, "2026-01-01T00:00:01Z"),
    activity("a3", "presenter_rejected", { to_user_id: "u1", by_user_id: "u2" }, "2026-01-01T00:00:02Z"),
    activity("a4", "presenter_transferred", { from_user_id: "u1", to_user_id: "u2", by_user_id: "u1" }, "2026-01-01T00:00:03Z"),
    activity("a5", "presenter_revoked", { from_user_id: "u1", by_user_id: "u2" }, "2026-01-01T00:00:04Z"),
    activity("a6", "presenter_released", { from_user_id: "u1", by_user_id: "u1" }, "2026-01-01T00:00:05Z"),
  ];

  it("renders one notice card per action, with copy matching chat.notices[action]", () => {
    renderWithProviders(
      <TeamAgentStreamView comments={[]} tasks={[]} presenterNotices={NOTICES} />,
    );

    const cards = screen.getAllByTestId("project-chat-presenter-notice");
    expect(cards).toHaveLength(6);
    expect(cards.map((c) => c.getAttribute("data-action"))).toEqual([
      "presenter_requested",
      "presenter_approved",
      "presenter_rejected",
      "presenter_transferred",
      "presenter_revoked",
      "presenter_released",
    ]);
    for (const [i, card] of cards.entries()) {
      const action = NOTICES[i]!.action as keyof typeof enProjects.chat.notices;
      expect(card.textContent).toContain(fillNoticeTemplate(enProjects.chat.notices[action]));
    }
  });

  it("does not disturb existing comment/task card rendering when notices are interleaved", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        comments={[comment("c1", "2026-01-01T00:00:01.500Z")]}
        tasks={[task("t1", "2026-01-01T00:00:01.000Z")]}
        presenterNotices={[activity("a1", "presenter_released", {}, "2026-01-01T00:00:02.000Z")]}
        currentUserId="member-1"
      />,
    );
    expect(container.querySelectorAll('[data-testid="project-chat-user-bubble"]')).toHaveLength(1);
    expect(container.querySelectorAll('[data-testid="project-chat-task-card"]')).toHaveLength(1);
    expect(container.querySelectorAll('[data-testid="project-chat-presenter-notice"]')).toHaveLength(1);
  });
});

// CR-2026-010 TASK-06 AC3: presenter_required must be a distinct rejection
// from 429 project_queue_full — separate banner, separate copy, never
// conflated even though both share the "locked" state.
describe("TeamAgentComposer presenter guard", () => {
  const props = { projectId: "proj-1", wsId: "ws-1", canConfigure: false };

  it("locks the composer and names the current presenter", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "blocked send";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("forbidden", 403, "Forbidden", {
        code: "presenter_required",
        presenter_user_id: "u-presenter",
      }),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    await waitFor(() =>
      expect(screen.getByTestId("project-chat-presenter-required")).toBeTruthy(),
    );
    expect(projectChatState.drafts["proj-1:team_agent"]).toBe("blocked send");
    expect(screen.getByTestId("project-chat-composer-input")).toBeDisabled();
    expect(screen.queryByTestId("project-chat-queue-full")).toBeNull();
    expect(screen.getByTestId("project-chat-presenter-required").textContent).toContain(
      fillNoticeTemplate(enProjects.chat.presenter.locked_title),
    );
  });

  it("shows the no-active-presenter copy when presenter_user_id is empty", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "blocked send";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("forbidden", 403, "Forbidden", { code: "presenter_required", presenter_user_id: "" }),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    await waitFor(() =>
      expect(screen.getByTestId("project-chat-presenter-required").textContent).toContain(
        enProjects.chat.presenter.locked_title_default,
      ),
    );
  });

  it("requests presenter access on click, and disables the button once a request is pending", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "blocked send";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("forbidden", 403, "Forbidden", { code: "presenter_required", presenter_user_id: "u-presenter" }),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });
    await waitFor(() => screen.getByTestId("project-chat-presenter-required"));

    const requestButton = screen.getByRole("button", { name: enProjects.chat.presenter.request_cta });
    await act(async () => {
      requestButton.click();
    });
    expect(requestPresenterMock.mutate).toHaveBeenCalledTimes(1);
  });

  it("presenter_required never renders the 429 queue-full banner (distinct rejection reasons)", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "send 1";
    sendMock.mutateAsync = vi.fn().mockRejectedValue(
      new ApiError("forbidden", 403, "Forbidden", { code: "presenter_required", presenter_user_id: "u-presenter" }),
    );
    renderWithProviders(<TeamAgentComposer {...props} />);
    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });
    await waitFor(() => screen.getByTestId("project-chat-presenter-required"));
    expect(screen.queryByTestId("project-chat-queue-full")).toBeNull();
    // Distinct copy from the queue-full banner's title, not a shared string.
    expect(screen.getByTestId("project-chat-presenter-required").textContent).not.toContain(
      enProjects.chat.stream.queue_full_title,
    );
  });
});
