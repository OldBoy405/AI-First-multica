// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { useProjectChatStore } from "@multica/core/projects";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const mocks = vi.hoisted(() => ({
  getProjectChat: vi.fn(),
  getProjectPrivateChat: vi.fn(),
  patchChatSessionConfig: vi.fn(),
  listChatMessages: vi.fn(),
  getPendingChatTask: vi.fn(),
  sendChatMessage: vi.fn(),
  cancelTaskById: vi.fn(),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    getProjectChat: (...a: unknown[]) => mocks.getProjectChat(...a),
    getProjectPrivateChat: (...a: unknown[]) => mocks.getProjectPrivateChat(...a),
    patchChatSessionConfig: (...a: unknown[]) => mocks.patchChatSessionConfig(...a),
    listChatMessages: (...a: unknown[]) => mocks.listChatMessages(...a),
    getPendingChatTask: (...a: unknown[]) => mocks.getPendingChatTask(...a),
    sendChatMessage: (...a: unknown[]) => mocks.sendChatMessage(...a),
    cancelTaskById: (...a: unknown[]) => mocks.cancelTaskById(...a),
  },
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({
    queryKey: ["runtimes", "test"],
    queryFn: () => [{ id: "rt-1", status: "online" }],
  }),
  runtimeModelsOptions: (rid: string | null) => ({
    queryKey: ["models", rid],
    queryFn: () => ({ models: [{ id: "claude-1", label: "Claude 1" }], supported: true }),
    enabled: !!rid,
  }),
}));

vi.mock("@multica/core/agents", () => ({
  useAgentPresenceDetail: () => ({
    availability: "online",
    workload: "idle",
    runningCount: 0,
    queuedCount: 0,
    capacity: 1,
  }),
}));

vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({
    queryKey: ["agents", "test"],
    queryFn: () => [{ id: "agent-1", runtime_id: "rt-1", model: "claude-1" }],
  }),
}));

// The message list is virtuoso-heavy and covered by its own tests; this suite
// asserts the pane's wiring (which messages/pendingTask it feeds in).
vi.mock("../../chat/components/chat-message-list", () => ({
  ChatMessageList: (props: { messages: unknown[]; pendingTask?: { task_id?: string } }) => (
    <div
      data-testid="stub-message-list"
      data-count={props.messages.length}
      data-pending={props.pendingTask?.task_id ?? ""}
    />
  ),
}));

vi.mock("../../agents/components/inspector/model-picker", () => ({
  ModelPicker: (props: { value: string; canEdit: boolean; onChange?: (m: string) => void }) => (
    <button
      type="button"
      data-testid="stub-model-picker"
      data-value={props.value}
      data-canedit={String(props.canEdit)}
      onClick={() => props.onChange?.("claude-2")}
    >
      {props.value}
    </button>
  ),
}));

// CR-2026-012 TASK-06: the composer is ChatInputCore behind an adapter; this
// shim keeps the historical textarea/send/stop testids and the adapter
// contract (draft read, setDraft on change, clearDraft via commitInput,
// submitting + running disable) so the pane-level send-flow assertions stay
// meaningful without pulling the real Tiptap editor into jsdom.
vi.mock("../../chat/components/chat-input", async () => {
  const React = await import("react");
  type Adapter = {
    draftKey: string;
    draft: string;
    setDraft: (key: string, content: string) => void;
    clearDraft: (key: string) => void;
  };
  type Commit = (options?: { extraDraftKeys?: string[]; clearEditor?: boolean }) => void;
  function ChatInputCore(props: {
    draftAdapter: Adapter;
    onSend: (
      content: string,
      attachmentIds: string[] | undefined,
      commitInput: Commit,
    ) => void | boolean | Promise<void | boolean>;
    onStop?: () => void;
    isRunning?: boolean;
    disabled?: boolean;
  }) {
    const [submitting, setSubmitting] = React.useState(false);
    const commit: Commit = (options) => {
      props.draftAdapter.clearDraft(props.draftAdapter.draftKey);
      for (const key of options?.extraDraftKeys ?? []) {
        if (key !== props.draftAdapter.draftKey) props.draftAdapter.clearDraft(key);
      }
    };
    const handle = async () => {
      setSubmitting(true);
      try {
        await props.onSend(props.draftAdapter.draft, undefined, commit);
      } finally {
        setSubmitting(false);
      }
    };
    return (
      <div>
        <textarea
          data-testid="private-ask-composer-input"
          value={props.draftAdapter.draft}
          disabled={props.disabled || submitting || props.isRunning}
          onChange={(e) =>
            props.draftAdapter.setDraft(props.draftAdapter.draftKey, e.target.value)
          }
        />
        {props.isRunning && props.onStop ? (
          <button
            type="button"
            data-testid="private-ask-stop"
            onClick={() => void props.onStop?.()}
          >
            stop
          </button>
        ) : (
          <button
            type="button"
            data-testid="private-ask-send"
            disabled={props.disabled || submitting || !props.draftAdapter.draft.trim()}
            onClick={() => void handle()}
          >
            send
          </button>
        )}
      </div>
    );
  }
  return { ChatInputCore };
});

import { ProjectPrivateAsk } from "./project-private-ask";

function renderPane() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ProjectPrivateAsk projectId="proj-1" wsId="ws-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

const SESSION = {
  id: "sess-1",
  session_id: "sess-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  status: "active",
  model: "claude-1",
  thinking_level: "",
  model_source: "session_default",
  thinking_level_source: "session_default",
};

describe("ProjectPrivateAsk (CR-2026-008 TASK-04)", () => {
  beforeEach(() => {
    Object.values(mocks).forEach((m) => m.mockReset());
    mocks.getProjectChat.mockResolvedValue({ issue_id: "i1", team_agent_id: "agent-1" });
    mocks.getProjectPrivateChat.mockResolvedValue(SESSION);
    mocks.listChatMessages.mockResolvedValue([]);
    mocks.getPendingChatTask.mockResolvedValue({});
    useProjectChatStore.setState({
      drafts: {},
      activeMode: {},
      tutorialSeen: {},
      draftAttachments: {},
    });
    localStorage.clear();
  });
  afterEach(cleanup);

  it("never imports the global chat controller/store (FR-3 static guard)", () => {
    // vitest root is packages/views, so resolve from cwd (import.meta.url is
    // not a file: URL under the test transform).
    const source = readFileSync(
      join(process.cwd(), "projects", "components", "project-private-ask.tsx"),
      "utf8",
    );
    // Guard the import statements (comments may explain the ban in prose).
    expect(source).not.toMatch(/import[^;]*use-chat-controller/s);
    expect(source).not.toMatch(/import[^;]*\buseChatStore\b/s);
  });

  it("without a Team Agent shows the configure hint and never get-or-creates", async () => {
    mocks.getProjectChat.mockResolvedValue({ issue_id: "", team_agent_id: "" });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("private-ask-unconfigured")).toBeTruthy());
    expect(mocks.getProjectPrivateChat).not.toHaveBeenCalled();
  });

  it("resolves the session lazily and greets on an empty conversation", async () => {
    renderPane();
    await waitFor(() => expect(mocks.getProjectPrivateChat).toHaveBeenCalledWith("proj-1"));
    await waitFor(() =>
      expect(screen.getByText(enProjects.chat.greetings.private_ask)).toBeTruthy(),
    );
    // Writable session-config picker (FR-3/FR-12) bound to the session's own
    // effective model — not the Team Agent's agent row.
    const picker = await screen.findByTestId("stub-model-picker");
    expect(picker.getAttribute("data-value")).toBe("claude-1");
    expect(picker.getAttribute("data-canedit")).toBe("true");
  });

  it("PATCHes the session config with session_id when the picker changes (BLOCK-007)", async () => {
    mocks.patchChatSessionConfig.mockResolvedValue({ ...SESSION, model: "claude-2" });
    renderPane();
    const picker = await screen.findByTestId("stub-model-picker");

    fireEvent.click(picker);

    await waitFor(() =>
      expect(mocks.patchChatSessionConfig).toHaveBeenCalledWith("sess-1", {
        model: "claude-2",
      }),
    );
  });

  it("hard degradation (empty session_id) renders read-only with a retry affordance (AC-27)", async () => {
    mocks.getProjectPrivateChat.mockResolvedValue({ ...SESSION, session_id: "" });
    renderPane();

    await waitFor(() =>
      expect(screen.getByTestId("private-ask-config-unavailable")).toBeTruthy(),
    );
    expect(screen.getByTestId("private-ask-config-retry")).toBeTruthy();
    // No session mounts: no composer, no PATCH surface.
    expect(screen.queryByTestId("private-ask-composer-input")).toBeNull();
  });

  it("sends through the session endpoint and clears the draft on success", async () => {
    mocks.sendChatMessage.mockResolvedValue({ message: { id: "m1" } });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("private-ask-composer-input")).toBeTruthy());

    fireEvent.change(screen.getByTestId("private-ask-composer-input"), {
      target: { value: "why does the build fail?" },
    });
    fireEvent.click(screen.getByTestId("private-ask-send"));

    await waitFor(() =>
      expect(mocks.sendChatMessage).toHaveBeenCalledWith(
        "sess-1",
        "why does the build fail?",
        undefined,
      ),
    );
    await waitFor(() =>
      expect(
        (screen.getByTestId("private-ask-composer-input") as HTMLTextAreaElement).value,
      ).toBe(""),
    );
  });

  it("shows a pending-message bubble while the send is in flight (CLAUDE.md pending-message pattern)", async () => {
    let resolveSend!: (v: unknown) => void;
    mocks.sendChatMessage.mockReturnValue(new Promise((resolve) => { resolveSend = resolve; }));
    renderPane();
    await waitFor(() => expect(screen.getByTestId("private-ask-composer-input")).toBeTruthy());

    fireEvent.change(screen.getByTestId("private-ask-composer-input"), {
      target: { value: "still thinking?" },
    });
    fireEvent.click(screen.getByTestId("private-ask-send"));

    // Visible pending state appears immediately — not silent optimism, and
    // not just a spinner on the button.
    const bubble = await screen.findByTestId("private-ask-pending-message");
    expect(bubble.textContent).toContain("still thinking?");
    // The empty-state greeting must not still be showing behind the bubble.
    expect(screen.queryByText(enProjects.chat.greetings.private_ask)).toBeNull();
    // Composer disabled while a send is in flight.
    expect(
      (screen.getByTestId("private-ask-composer-input") as HTMLTextAreaElement).disabled,
    ).toBe(true);

    resolveSend({ message: { id: "m1" } });
    await waitFor(() => expect(screen.queryByTestId("private-ask-pending-message")).toBeNull());
  });

  it("keeps the draft when sending fails", async () => {
    mocks.sendChatMessage.mockRejectedValue(new Error("boom"));
    renderPane();
    await waitFor(() => expect(screen.getByTestId("private-ask-composer-input")).toBeTruthy());

    fireEvent.change(screen.getByTestId("private-ask-composer-input"), {
      target: { value: "keep me" },
    });
    fireEvent.click(screen.getByTestId("private-ask-send"));

    await waitFor(() => expect(mocks.sendChatMessage).toHaveBeenCalled());
    expect(
      (screen.getByTestId("private-ask-composer-input") as HTMLTextAreaElement).value,
    ).toBe("keep me");
  });

  it("shows stop instead of send while a task is pending, and cancels it (FR-9)", async () => {
    mocks.getPendingChatTask.mockResolvedValue({
      task_id: "task-9",
      status: "running",
      created_at: new Date().toISOString(),
    });
    mocks.cancelTaskById.mockResolvedValue({});
    renderPane();

    await waitFor(() => expect(screen.getByTestId("private-ask-stop")).toBeTruthy());
    expect(screen.queryByTestId("private-ask-send")).toBeNull();
    // The pending task rides into the message list (status pill + live timeline).
    expect(screen.getByTestId("stub-message-list").getAttribute("data-pending")).toBe("task-9");

    fireEvent.click(screen.getByTestId("private-ask-stop"));
    await waitFor(() => expect(mocks.cancelTaskById).toHaveBeenCalledWith("task-9"));
  });
});
