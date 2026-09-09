// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import type { AgentTask, TimelineEntry } from "@multica/core/types";
import type { ProjectGateCR } from "@multica/core/api/schemas";
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

vi.mock("../../agents/components/inspector/thinking-picker", () => ({
  ThinkingPicker: ({
    canEdit,
    value,
    onChange,
  }: {
    canEdit: boolean;
    value: string;
    onChange: (l: string) => void;
  }) =>
    canEdit ? (
      <button
        data-testid="stub-thinking-interactive"
        onClick={() => onChange("high")}
      >
        {value}
      </button>
    ) : (
      <span data-testid="stub-thinking-readonly">{value}</span>
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
      models: [
        {
          id: "claude-opus",
          label: "Claude Opus",
          thinking: { supported_levels: [{ value: "high" }] },
        },
      ],
      supported: true,
    }),
    enabled: !!rid,
  }),
}));

vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: {
      updateAgent: vi.fn().mockResolvedValue({}),
      // CR-2026-056: the chat path persists via the session-config PATCH.
      patchProjectChatConfig: vi.fn().mockResolvedValue({}),
      // CR-2026-062: the stop path reads the container issue's task-runs
      // (AgentTask[] — the array shape is part of the contract under test).
      listTasksByIssue: vi.fn(async () => taskRunsState),
    },
  };
});

// ─── Project store + send mutation + queue status mocked ───────────────────
// Callable-store shape (selectorFn + getState), matching the Zustand mock
// convention used elsewhere in this repo (see chat-input.test.tsx).
const projectChatState = {
  drafts: {} as Record<string, string>,
  draftAttachments: {} as Record<string, unknown[]>,
  agentRequestFilter: {} as Record<string, boolean>,
  setDraft: vi.fn((projectId: string, mode: string, text: string) => {
    const key = `${projectId}:${mode}`;
    if (text) projectChatState.drafts[key] = text;
    else {
      delete projectChatState.drafts[key];
      delete projectChatState.draftAttachments[key];
    }
  }),
  setDraftAttachments: vi.fn((projectId: string, mode: string, attachments: unknown[]) => {
    const key = `${projectId}:${mode}`;
    if (attachments.length > 0) projectChatState.draftAttachments[key] = attachments;
    else delete projectChatState.draftAttachments[key];
  }),
  addDraftAttachment: vi.fn((projectId: string, mode: string, attachment: { id?: string }) => {
    if (!attachment.id) return;
    const key = `${projectId}:${mode}`;
    const existing = projectChatState.draftAttachments[key] ?? [];
    projectChatState.draftAttachments[key] = [...existing, attachment];
  }),
  setAgentRequestFilter: vi.fn((projectId: string, value: boolean) => {
    projectChatState.agentRequestFilter[projectId] = value;
  }),
};
const sendMock = { mutateAsync: vi.fn(), isPending: false };
const approveMock = { mutateAsync: vi.fn(), isPending: false };
// CR-2026-007 T05: stop/cancel mutation shared instance, mirroring sendMock.
const cancelMock = { mutateAsync: vi.fn(), isPending: false };
const requestPresenterMock = { mutate: vi.fn(), isPending: false };
// CR-2026-010: resolves to "no presenter" by default so existing tests (which
// predate the presenter guard) keep seeing an unlocked composer; the
// presenter-guard describe block below overrides this per test.
let presenterStateMock: {
  presenter: { user_id: string } | null;
  pending_requests: unknown[];
  my_request: unknown | null;
} = { presenter: null, pending_requests: [], my_request: null };

// CR-2026-056: the composer reads the session's effective config from the
// chat context; mutable per test, read lazily in the mock factory.
const chatCfg: {
  session_id: string;
  issue_id: string | null;
  team_agent_id: string;
  model: string;
  thinking_level: string;
} = {
  session_id: "session-1",
  issue_id: "issue-chat-1",
  team_agent_id: "agent-1",
  model: "gpt-5",
  thinking_level: "",
};

// CR-2026-062 stop-path fixtures (dual-source, §4.3.1): queue items are the
// server-filtered queued/dispatched window; task-runs are the container
// issue's AgentTask[] timeline. Read lazily inside the mock factories.
const queueItemsState: {
  queue_depth: number;
  queue_limit: number;
  items: { task_id: string; status: string }[];
} = { queue_depth: 0, queue_limit: 10, items: [] };
const taskRunsState: AgentTask[] = [];

vi.mock("@multica/core/projects", () => ({
  projectChatDraftKey: (projectId: string, mode: string) => `${projectId}:${mode}`,
  projectChatOptions: (_wsId: string, id: string) => ({
    queryKey: ["projects", "chat", id],
    queryFn: async () => chatCfg,
  }),
  useProjectChatStore: Object.assign(
    (selector?: (s: typeof projectChatState) => unknown) =>
      selector ? selector(projectChatState) : projectChatState,
    { getState: () => projectChatState },
  ),
  useSendProjectChatMessage: () => sendMock,
  // CR-2026-011 TASK-06: CrGateCard's ApprovalCard variant calls this.
  useApproveCr: () => approveMock,
  useCancelProjectQueueTask: () => cancelMock,
  useRequestPresenter: () => requestPresenterMock,
  // Never resolves — keeps `queue` undefined so the live-queue path never
  // interferes with the tests that drive the 429 latch explicitly.
  projectQueueStatusOptions: (_wsId: string, id: string) => ({
    queryKey: ["queue-status", id],
    queryFn: () => new Promise(() => {}),
  }),
  // CR-2026-062: the composer's dual-source stop reads the same items cache
  // as ProjectQueueBar (server-filtered queued+dispatched). Fresh copies so a
  // mid-test mutation is observable after invalidation.
  projectQueueItemsOptions: (_wsId: string, id: string) => ({
    queryKey: ["queue-items", id],
    queryFn: async () => ({
      queue_depth: queueItemsState.queue_depth,
      queue_limit: queueItemsState.queue_limit,
      items: [...queueItemsState.items],
    }),
  }),
  projectPresenterOptions: (_wsId: string, id: string) => ({
    queryKey: ["presenter", id],
    queryFn: async () => presenterStateMock,
  }),
}));

// CR-2026-012 TASK-06: the composer is ChatInputCore behind an adapter; this
// shim keeps the historical textarea/send-button testids and the adapter
// contract (draft read, setDraft on change, clearDraft via commitInput) so
// the pane-level send/lock/latch assertions stay meaningful without pulling
// the real Tiptap editor into jsdom. CR-2026-062 extends it with the
// stop/send affordance decision (§4.2) and the leftAdornment toolbar slot.
type ComposerCommit = (options?: { extraDraftKeys?: string[]; clearEditor?: boolean }) => void;
vi.mock("../../chat/components/chat-input", () => ({
  ChatInputCore: ({
    draftAdapter,
    onSend,
    disabled,
    isRunning,
    onStop,
    allowSubmitWhileRunning,
    leftAdornment,
  }: {
    draftAdapter: {
      draftKey: string;
      draft: string;
      setDraft: (key: string, content: string) => void;
      clearDraft: (key: string) => void;
    };
    onSend: (
      content: string,
      attachmentIds: string[] | undefined,
      commitInput: ComposerCommit,
    ) => void | boolean | Promise<void | boolean>;
    disabled?: boolean;
    isRunning?: boolean;
    onStop?: () => void;
    allowSubmitWhileRunning?: boolean;
    leftAdornment?: React.ReactNode;
  }) => {
    const commit: ComposerCommit = (options) => {
      draftAdapter.clearDraft(draftAdapter.draftKey);
      for (const key of options?.extraDraftKeys ?? []) {
        if (key !== draftAdapter.draftKey) draftAdapter.clearDraft(key);
      }
    };
    // Mirrors the real ChatInputCore affordance decision (SDD §4.2): a
    // queue-capable run swaps an empty composer to Stop, live content to
    // Queue Send; without allowSubmitWhileRunning it stays stop-only.
    const running =
      !!isRunning && (!allowSubmitWhileRunning || !draftAdapter.draft.trim());
    return (
      <div data-testid="project-chat-composer-inner">
        <textarea
          data-testid="project-chat-composer-input"
          value={draftAdapter.draft}
          disabled={disabled}
          onChange={(e) => draftAdapter.setDraft(draftAdapter.draftKey, e.target.value)}
        />
        {running ? (
          <button type="button" data-testid="project-chat-stop" onClick={onStop}>
            stop
          </button>
        ) : (
          <button
            type="button"
            data-testid="project-chat-send"
            disabled={disabled || !draftAdapter.draft.trim()}
            onClick={() => void onSend(draftAdapter.draft, undefined, commit)}
          >
            send
          </button>
        )}
        <div data-testid="project-chat-left-adornment">{leftAdornment}</div>
      </div>
    );
  },
}));

import {
  TeamAgentComposer,
  TeamAgentStreamView,
} from "./project-team-agent-chat";

const RESOURCES = { en: { projects: enProjects, common: enCommon } };

function wrapWithProviders(ui: React.ReactNode, qc: QueryClient) {
  return (
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        {ui}
      </I18nProvider>
    </QueryClientProvider>
  );
}

function renderWithProviders(ui: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(wrapWithProviders(ui, qc));
  return { qc, ...view };
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

function gateCR(overrides: Partial<ProjectGateCR> & { nodeAt: string; nodeStatus: string; nodeKind?: string }): ProjectGateCR {
  const { nodeAt, nodeStatus, nodeKind = "human_approval", ...rest } = overrides;
  return {
    cr_id: "CR-2026-011",
    title: "t",
    status: "requirement-reviewing",
    needs_reconcile: false,
    updated_at: nodeAt,
    pending_stage: "requirement",
    can_approve: true,
    evidence: {},
    evidence_digest: "",
    key_id: "",
    pending_advance: false,
    gate_nodes: [
      {
        node_id: "n1",
        kind: nodeKind,
        seq: 5,
        status: nodeStatus,
        stage: "requirement",
        attempt: 1,
        started_at: nodeAt,
      },
    ],
    ...rest,
  };
}

beforeEach(() => {
  for (const k of Object.keys(projectChatState.drafts)) delete projectChatState.drafts[k];
  for (const k of Object.keys(projectChatState.draftAttachments)) {
    delete projectChatState.draftAttachments[k];
  }
  for (const k of Object.keys(projectChatState.agentRequestFilter)) {
    delete projectChatState.agentRequestFilter[k];
  }
  for (const k of Object.keys(messagesByTask)) delete messagesByTask[k];
  projectChatState.setDraft.mockClear();
  projectChatState.setDraftAttachments.mockClear();
  projectChatState.addDraftAttachment.mockClear();
  projectChatState.setAgentRequestFilter.mockClear();
  sendMock.mutateAsync = vi.fn();
  sendMock.isPending = false;
  cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "cancelled" });
  cancelMock.isPending = false;
  queueItemsState.queue_depth = 0;
  queueItemsState.queue_limit = 10;
  queueItemsState.items = [];
  taskRunsState.length = 0;
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

  it("interleaves gate-node cards from crs by timestamp (CR-2026-011 TASK-06)", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[comment("c1", "2026-01-01T00:00:02.000Z")]}
        tasks={[task("t1", "2026-01-01T00:00:01.000Z")]}
        crs={[
          gateCR({ cr_id: "CR-A", nodeAt: "2026-01-01T00:00:00.500Z", nodeStatus: "running" }),
          gateCR({
            cr_id: "CR-B",
            nodeAt: "2026-01-01T00:00:03.000Z",
            nodeStatus: "blocked",
            nodeKind: "skill",
            // A review-blocked CR is NOT at an approval gate: pending_stage
            // is empty, so no current ApprovalCard is rendered for it
            // (CR-2026-053 FR-B6) — only the blocked card stays.
            pending_stage: "",
          }),
        ]}
        currentUserId="member-1"
      />,
    );

    const nodes = Array.from(
      container.querySelectorAll(
        '[data-testid="project-chat-task-card"],[data-testid="project-chat-user-bubble"],[data-testid="cr-gate-approval-card"],[data-testid="cr-gate-blocked-card"]',
      ),
    ).map((el) => el.getAttribute("data-testid"));

    // approval(0.5s) → task(1.0s) → comment(2.0s) → blocked(3.0s).
    expect(nodes).toEqual([
      "cr-gate-approval-card",
      "project-chat-task-card",
      "project-chat-user-bubble",
      "cr-gate-blocked-card",
    ]);
  });

  // ── CR-2026-053 TASK-07 (FR-B6, AC-C1~C4): pending_stage non-empty renders
  // the ONE current ApprovalCard even without a human_approval/running node. ──

  it("renders one ApprovalCard when pending_stage is set and gate_nodes is empty (AC-C1)", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[]}
        crs={[
          gateCR({
            cr_id: "CR-NODE-LESS",
            nodeAt: "2026-01-01T00:00:00.000Z",
            nodeStatus: "completed",
            gate_nodes: [],
          }),
        ]}
      />,
    );
    expect(
      container.querySelectorAll('[data-testid="cr-gate-approval-card"]').length,
    ).toBe(1);
  });

  it("renders exactly ONE ApprovalCard when a human_approval/running node also exists (AC-C2)", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[]}
        crs={[
          gateCR({ cr_id: "CR-ONE", nodeAt: "2026-01-01T00:00:00.000Z", nodeStatus: "running" }),
        ]}
      />,
    );
    expect(
      container.querySelectorAll('[data-testid="cr-gate-approval-card"]').length,
    ).toBe(1);
  });

  it("keeps blocked cards and history nodes while the current card is rendered (AC-C3)", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[]}
        crs={[
          gateCR({
            cr_id: "CR-MIXED",
            nodeAt: "2026-01-01T00:00:00.000Z",
            nodeStatus: "blocked",
            nodeKind: "skill",
          }),
        ]}
      />,
    );
    expect(
      container.querySelectorAll('[data-testid="cr-gate-approval-card"]').length,
    ).toBe(1);
    expect(
      container.querySelectorAll('[data-testid="cr-gate-blocked-card"]').length,
    ).toBe(1);
  });

  it("renders no current ApprovalCard when pending_stage is empty (AC-C4)", () => {
    const { container } = renderWithProviders(
      <TeamAgentStreamView
        {...streamBaseProps}
        comments={[]}
        tasks={[]}
        crs={[
          gateCR({
            cr_id: "CR-NO-GATE",
            nodeAt: "2026-01-01T00:00:00.000Z",
            nodeStatus: "completed",
            nodeKind: "skill",
            pending_stage: "",
          }),
        ]}
      />,
    );
    expect(
      container.querySelectorAll('[data-testid="cr-gate-approval-card"]').length,
    ).toBe(0);
  });
});

describe("TeamAgentComposer", () => {
  const props = { projectId: "proj-1", wsId: "ws-1", issueId: "issue-chat-1", sessionId: "session-1", canConfigure: false };

  it("clears the draft on a successful send", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "hello agent";
    sendMock.mutateAsync = vi.fn().mockResolvedValue({ comment_id: "c1", task_id: "t1" });
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(sendMock.mutateAsync).toHaveBeenCalledWith(
      expect.objectContaining({ content: "hello agent" }),
    );
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

// ─── CR-2026-056 session-config selector ──────────────────────────────────
// The selector now binds the SESSION's effective model/thinking (PATCH
// /chat/config), not the agent row. Permission is owner/admin (canConfigure
// prop); runtime availability decides the editor's content. The value shown
// is the chat context's model, never agent.model.
describe("TeamAgentComposer session-config selector (CR-2026-056)", () => {
  const props = {
    projectId: "proj-1",
    wsId: "ws-1",
    issueId: "issue-chat-1",
    sessionId: "session-1",
    teamAgentId: "agent-1",
    canConfigure: false,
  };

  beforeEach(() => {
    cfg.agent = { id: "agent-1", model: "gpt-5", runtime_id: "rt-1" };
    chatCfg.model = "gpt-5";
    chatCfg.thinking_level = "";
    projectChatState.drafts["proj-1:team_agent"] = "hi"; // isolate send-disable to runtime state
  });

  it("owner/admin + runtime → interactive picker, send enabled", async () => {
    cfg.runtimeStatus = "online";
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    expect(await screen.findByTestId("project-chat-model-picker")).toBeTruthy();
    expect(screen.queryByTestId("project-chat-model-readonly")).toBeNull();
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    expect(screen.getByTestId("project-chat-send")).not.toBeDisabled();
  });

  it("owner/admin + no runtime → runtime guide, send disabled", async () => {
    cfg.runtimeStatus = "offline";
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    const guide = await screen.findByTestId("project-chat-model-runtime-guide");
    expect(guide.textContent).toBe(
      enProjects.chat.stream.runtime_guide,
    );
    expect(screen.queryByTestId("project-chat-model-picker")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).toBeDisabled(),
    );
  });

  it("plain member + runtime → read-only badge (session model), send enabled", async () => {
    cfg.runtimeStatus = "online";
    renderWithProviders(<TeamAgentComposer {...props} />);

    expect(await screen.findByTestId("project-chat-model-readonly")).toBeTruthy();
    expect(screen.getByTestId("stub-model-readonly").textContent).toBe("gpt-5");
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).not.toBeDisabled(),
    );
  });

  it("plain member + no runtime → read-only badge (not the guide), send disabled", async () => {
    cfg.runtimeStatus = "offline";
    renderWithProviders(<TeamAgentComposer {...props} />);

    expect(await screen.findByTestId("project-chat-model-readonly")).toBeTruthy();
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-send")).toBeDisabled(),
    );
  });

  it("selecting a model PATCHes the session config with the session id and never updateAgent (AC-1)", async () => {
    cfg.runtimeStatus = "online";
    const { api } = await import("@multica/core/api");
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    const trigger = await screen.findByTestId("stub-model-interactive");
    await act(async () => {
      trigger.click();
    });

    await waitFor(() =>
      expect(api.patchProjectChatConfig).toHaveBeenCalledWith("proj-1", "session-1", {
        model: "claude-opus",
      }),
    );
    expect(api.updateAgent).not.toHaveBeenCalled();
  });

  it("selecting a thinking level PATCHes the session config with the session id (AC-2)", async () => {
    cfg.runtimeStatus = "online";
    chatCfg.model = "claude-opus"; // exposes the stub catalog's thinking levels
    const { api } = await import("@multica/core/api");
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    const trigger = await screen.findByTestId("stub-thinking-interactive");
    await act(async () => {
      trigger.click();
    });

    await waitFor(() =>
      expect(api.patchProjectChatConfig).toHaveBeenCalledWith("proj-1", "session-1", {
        thinking_level: "high",
      }),
    );
    expect(api.updateAgent).not.toHaveBeenCalled();
  });

  it("the send mutation carries the session id (CR-2026-056 §3.1)", async () => {
    projectChatState.drafts["proj-1:team_agent"] = "hello agent";
    sendMock.mutateAsync = vi.fn().mockResolvedValue({ session_id: "session-1", issue_id: "issue-chat-1", comment_id: "c1", task_id: "t1" });
    renderWithProviders(<TeamAgentComposer {...props} />);

    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });

    expect(sendMock.mutateAsync).toHaveBeenCalledWith(
      expect.objectContaining({ sessionId: "session-1", content: "hello agent" }),
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
      <TeamAgentStreamView {...streamBaseProps} comments={[]} tasks={[]} presenterNotices={NOTICES} />,
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
        {...streamBaseProps}
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
  const props = { projectId: "proj-1", wsId: "ws-1", issueId: "issue-chat-1", sessionId: "session-1", canConfigure: false };

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

// ─── CR-2026-062 TASK-02: two-layer column DOM ─────────────────────────────

describe("TeamAgentStreamView two-layer column (CR-2026-062)", () => {
  it("nests the reading column inside a gutter layer as separate DOM nodes", () => {
    renderWithProviders(<TeamAgentStreamView {...streamBaseProps} comments={[]} tasks={[]} />);

    const noEarlier = screen.getByTestId("project-chat-no-earlier");
    const inner = noEarlier.parentElement!;
    const outer = inner.parentElement!;
    // Two separate layers: gutter outside, reading column inside (a
    // single-element merge would put gutter padding inside the max-w cap).
    expect(outer).not.toBe(inner);
    expect(outer.className).toContain("px-5"); // CHAT_GUTTER base
    expect(outer.className).not.toContain("max-w-4xl");
    expect(inner.className).toContain("max-w-4xl"); // CHAT_COLUMN
    expect(inner.className).toContain("mx-auto");
  });
});

// ─── CR-2026-062 TASK-02: composer alignment + toolbar ─────────────────────

describe("TeamAgentComposer alignment + toolbar (CR-2026-062)", () => {
  const props = {
    projectId: "proj-1",
    wsId: "ws-1",
    issueId: "issue-chat-1",
    sessionId: "session-1",
    teamAgentId: "agent-1",
    canConfigure: false,
  };

  beforeEach(() => {
    cfg.agent = { id: "agent-1", model: "gpt-5", runtime_id: "rt-1" };
    chatCfg.model = "gpt-5";
    chatCfg.thinking_level = "";
  });

  it("wraps the banner zone in a two-layer gutter>column block above the surface", async () => {
    renderWithProviders(<TeamAgentComposer {...props} />);

    const composer = await screen.findByTestId("project-chat-composer");
    const root = composer.parentElement!;
    // Composer wrapper drops its own gutters; border stays.
    expect(root.className).toContain("shrink-0");
    expect(root.className).toContain("border-t");
    expect(root.className).not.toContain("px-4");
    // The banner block (always rendered, even without banners) is the
    // gutter>column sibling right above the composer.
    const bannerOuter = composer.previousElementSibling!;
    expect(bannerOuter.className).toContain("px-5"); // CHAT_GUTTER base
    expect(bannerOuter.className).toContain("pt-3");
    expect(bannerOuter.className).not.toContain("max-w-4xl");
    const bannerInner = bannerOuter.firstElementChild!;
    expect(bannerInner).not.toBe(bannerOuter);
    expect(bannerInner.className).toContain("max-w-4xl"); // CHAT_COLUMN
  });

  it("removes the model row and renders the control testids inside the composer subtree with sr-only labels", async () => {
    cfg.runtimeStatus = "online";
    chatCfg.model = "claude-opus"; // exposes the stub catalog's thinking levels
    renderWithProviders(<TeamAgentComposer {...props} />);

    const composer = await screen.findByTestId("project-chat-composer");
    // The independent row (and its anchor) is gone — no replacement anchor.
    expect(screen.queryByTestId("project-chat-model-row")).toBeNull();
    // The three-state control testids live in the composer subtree now.
    const readonly = await screen.findByTestId("project-chat-model-readonly");
    expect(composer.contains(readonly)).toBe(true);
    const thinking = await screen.findByTestId("project-chat-thinking-picker");
    expect(composer.contains(thinking)).toBe(true);
    expect(screen.queryByTestId("project-chat-model-picker")).toBeNull();
    expect(screen.queryByTestId("project-chat-model-runtime-guide")).toBeNull();
    // Category semantics survive through sr-only labels (B-002).
    expect(readonly.querySelector(".sr-only")?.textContent).toBe(
      enProjects.chat.stream.model_label,
    );
    expect(thinking.querySelector(".sr-only")?.textContent).toBe(
      enProjects.chat.stream.thinking_label,
    );
  });

  it("owner/admin + runtime → interactive picker in the toolbar, PATCH target unchanged (AC-4)", async () => {
    cfg.runtimeStatus = "online";
    const { api } = await import("@multica/core/api");
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    const trigger = await screen.findByTestId("stub-model-interactive");
    expect(screen.queryByTestId("project-chat-model-row")).toBeNull();
    await act(async () => {
      trigger.click();
    });
    await waitFor(() =>
      expect(api.patchProjectChatConfig).toHaveBeenCalledWith("proj-1", "session-1", {
        model: "claude-opus",
      }),
    );
    expect(api.updateAgent).not.toHaveBeenCalled();
  });

  it("owner/admin + no runtime → runtime guide inside the composer subtree", async () => {
    cfg.runtimeStatus = "offline";
    renderWithProviders(<TeamAgentComposer {...props} canConfigure />);

    const guide = await screen.findByTestId("project-chat-model-runtime-guide");
    expect(guide.textContent).toBe(enProjects.chat.stream.runtime_guide);
    const composer = screen.getByTestId("project-chat-composer");
    expect(composer.contains(guide)).toBe(true);
  });
});

// ─── CR-2026-062 TASK-02: dual-source running-stop (SDD §4.3.1, B-005) ─────

describe("TeamAgentComposer dual-source stop path (CR-2026-062)", () => {
  const props = {
    projectId: "proj-1",
    wsId: "ws-1",
    issueId: "issue-chat-1",
    sessionId: "session-1",
    canConfigure: false,
  };
  const AT = "2026-01-01T00:00:00Z";

  function makeSendResult(taskId: string, issueId: string) {
    sendMock.mutateAsync = vi
      .fn()
      .mockResolvedValue({ task_id: taskId, issue_id: issueId, comment_id: "c1" });
  }

  function itemsWith(taskId: string) {
    queueItemsState.items = [{ task_id: taskId, status: "queued" }];
    queueItemsState.queue_depth = 1;
  }

  async function sendAndSettle() {
    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });
    await waitFor(() => expect(sendMock.mutateAsync).toHaveBeenCalledTimes(1));
  }

  beforeEach(() => {
    projectChatState.drafts["proj-1:team_agent"] = "hello";
  });

  it.each(["queued", "dispatched"] as const)(
    "%s: items + timeline both hold the sent task → stop renders and cancels it",
    async (status) => {
      makeSendResult("t1", "issue-1");
      itemsWith("t1");
      taskRunsState.push(task("t1", AT, { status }));
      renderWithProviders(<TeamAgentComposer {...props} />);

      await sendAndSettle();
      const stop = await screen.findByTestId("project-chat-stop");
      await act(async () => {
        stop.click();
      });
      expect(cancelMock.mutateAsync).toHaveBeenCalledWith("t1");
    },
  );

  it.each(["running", "waiting_local_directory"] as const)(
    "%s while absent from queue items → stop still renders (timeline alone supports it)",
    async (status) => {
      makeSendResult("t1", "issue-1");
      // Server filters running out of the items list; only the timeline says active.
      queueItemsState.items = [];
      taskRunsState.push(task("t1", AT, { status }));
      renderWithProviders(<TeamAgentComposer {...props} />);

      await sendAndSettle();
      expect(await screen.findByTestId("project-chat-stop")).toBeTruthy();
    },
  );

  it.each(["completed", "failed", "cancelled"] as const)(
    "terminal %s overrides stale queue items → button returns to send",
    async (status) => {
      makeSendResult("t1", "issue-1");
      itemsWith("t1"); // stale residue that must lose to the terminal timeline
      taskRunsState.push(task("t1", AT, { status }));
      renderWithProviders(<TeamAgentComposer {...props} />);

      await sendAndSettle();
      await waitFor(() =>
        expect(screen.queryByTestId("project-chat-stop")).toBeNull(),
      );
      expect(screen.getByTestId("project-chat-send")).toBeTruthy();
    },
  );

  it("non-cancelled terminal cancel result toasts already-finished (TSUG-007 branch 2)", async () => {
    makeSendResult("t1", "issue-1");
    itemsWith("t1");
    taskRunsState.push(task("t1", AT, { status: "running" }));
    cancelMock.mutateAsync = vi.fn().mockResolvedValue({ status: "completed" });
    const { toast } = await import("sonner");
    renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    const stop = await screen.findByTestId("project-chat-stop");
    await act(async () => {
      stop.click();
    });
    expect(toast.error).toHaveBeenCalledWith(
      enProjects.chat.stream.cancel_already_finished,
    );
  });

  it("hard degradation (i): first send returns an empty task_id → no dead stop button", async () => {
    sendMock.mutateAsync = vi
      .fn()
      .mockResolvedValue({ task_id: "", issue_id: "issue-1", comment_id: "c1" });
    renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    expect(screen.queryByTestId("project-chat-stop")).toBeNull();
    expect(screen.getByTestId("project-chat-send")).toBeTruthy();
  });

  it("hard degradation (ii): active A → next send succeeds with empty task_id → stop disappears and A is never cancelled", async () => {
    sendMock.mutateAsync = vi
      .fn()
      .mockResolvedValueOnce({ task_id: "A", issue_id: "issue-1", comment_id: "c1" })
      .mockResolvedValueOnce({ task_id: "", issue_id: "issue-1", comment_id: "c2" });
    itemsWith("A");
    taskRunsState.push(task("A", AT, { status: "running" }));
    const view = renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    expect(await screen.findByTestId("project-chat-stop")).toBeTruthy();

    // Type the next message: live content + allowSubmitWhileRunning flips the
    // affordance back to the send button (queue send). The test store has no
    // subscription, so re-render explicitly to observe the new draft.
    projectChatState.drafts["proj-1:team_agent"] = "second";
    await act(async () => {
      view.rerender(wrapWithProviders(<TeamAgentComposer {...props} />, view.qc));
    });
    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });
    await waitFor(() => expect(sendMock.mutateAsync).toHaveBeenCalledTimes(2));
    // Both ids were atomically cleared — no stop, and the old target A is
    // never cancelled from the empty-id branch.
    await waitFor(() =>
      expect(screen.queryByTestId("project-chat-stop")).toBeNull(),
    );
    expect(cancelMock.mutateAsync).not.toHaveBeenCalled();
  });

  it("hard degradation (iii): send failure keeps draft and old target → retry affordance is send; clearing the draft brings stop back to cancel A", async () => {
    sendMock.mutateAsync = vi
      .fn()
      .mockResolvedValueOnce({ task_id: "A", issue_id: "issue-1", comment_id: "c1" })
      .mockRejectedValueOnce(
        new ApiError("boom", 502, "Bad Gateway", { code: "enqueue_failed" }),
      );
    itemsWith("A");
    taskRunsState.push(task("A", AT, { status: "running" }));
    const view = renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    expect(await screen.findByTestId("project-chat-stop")).toBeTruthy();

    // The failed resend keeps the draft: live content + allowSubmitWhileRunning
    // → the affordance is the send (retry) button, NOT stop (§4.2).
    projectChatState.drafts["proj-1:team_agent"] = "second";
    await act(async () => {
      view.rerender(wrapWithProviders(<TeamAgentComposer {...props} />, view.qc));
    });
    await act(async () => {
      screen.getByTestId("project-chat-send").click();
    });
    await waitFor(() => expect(sendMock.mutateAsync).toHaveBeenCalledTimes(2));
    expect(projectChatState.drafts["proj-1:team_agent"]).toBe("second");
    expect(screen.queryByTestId("project-chat-stop")).toBeNull();
    expect(screen.getByTestId("project-chat-send")).toBeTruthy();

    // Clearing the draft flips the affordance back to stop; the retained
    // target A is cancelled on click.
    projectChatState.drafts["proj-1:team_agent"] = "";
    await act(async () => {
      view.rerender(wrapWithProviders(<TeamAgentComposer {...props} />, view.qc));
    });
    const stop = await screen.findByTestId("project-chat-stop");
    await act(async () => {
      stop.click();
    });
    expect(cancelMock.mutateAsync).toHaveBeenCalledWith("A");
  });

  it("miss matrix (iv): empty task-runs list falls back to items-only", async () => {
    makeSendResult("t1", "issue-1");
    itemsWith("t1");
    taskRunsState.length = 0; // AgentTaskListSchema fallback semantics
    renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    expect(await screen.findByTestId("project-chat-stop")).toBeTruthy();
  });

  it("miss matrix (iv): empty task-runs and items without the sent id → send state", async () => {
    makeSendResult("t1", "issue-1");
    queueItemsState.items = [{ task_id: "other", status: "queued" }];
    taskRunsState.length = 0;
    renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    await waitFor(() =>
      expect(screen.queryByTestId("project-chat-stop")).toBeNull(),
    );
    expect(screen.getByTestId("project-chat-send")).toBeTruthy();
  });

  it("miss matrix (v): non-empty task-runs WITHOUT sentTaskId (another running task) → stop not rendered; the item appearing brings it back", async () => {
    makeSendResult("t1", "issue-1");
    queueItemsState.items = [];
    // Another task of the container issue is running — its state must never
    // leak into this composer's affordance (pick strictly by id).
    taskRunsState.push(task("other-task", AT, { status: "running" }));
    const { qc } = renderWithProviders(<TeamAgentComposer {...props} />);

    await sendAndSettle();
    await waitFor(() =>
      expect(screen.queryByTestId("project-chat-stop")).toBeNull(),
    );
    expect(screen.getByTestId("project-chat-send")).toBeTruthy();

    // The sent task reaches the queue-items window → items-only fallback.
    queueItemsState.items = [{ task_id: "t1", status: "dispatched" }];
    await act(async () => {
      await qc.invalidateQueries({ queryKey: ["queue-items"] });
    });
    expect(await screen.findByTestId("project-chat-stop")).toBeTruthy();
  });
});
