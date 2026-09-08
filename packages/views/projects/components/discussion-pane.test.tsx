// @vitest-environment jsdom
import { forwardRef, useImperativeHandle, useRef } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatMessage, TimelineEntry } from "@multica/core/types";
import { NavigationProvider, type NavigationAdapter } from "../../navigation";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const SESSION_ID = "550e8400-e29b-41d4-a716-446655440000";

// ─── Discussion data sources ──────────────────────────────────────────────
const mockGetProjectDiscussion = vi.hoisted(() => vi.fn());
const mockListChatMessagesPage = vi.hoisted(() => vi.fn());
const mockSendChatMessage = vi.hoisted(() => vi.fn());
const mockPatchChatSessionConfig = vi.hoisted(() => vi.fn());
const mockMergeForwardDiscussion = vi.hoisted(() => vi.fn());
const mockGetProjectGates = vi.hoisted(() => vi.fn());
const mockPromoteDiscussion = vi.hoisted(() => vi.fn());
const mockListAgents = vi.hoisted(() => vi.fn());
const mockUpdateAgent = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: {
      getProjectDiscussion: (...args: unknown[]) => mockGetProjectDiscussion(...args),
      listChatMessagesPage: (...args: unknown[]) => mockListChatMessagesPage(...args),
      sendChatMessage: (...args: unknown[]) => mockSendChatMessage(...args),
      patchChatSessionConfig: (...args: unknown[]) => mockPatchChatSessionConfig(...args),
      mergeForwardDiscussion: (...args: unknown[]) => mockMergeForwardDiscussion(...args),
      promoteDiscussion: (...args: unknown[]) => mockPromoteDiscussion(...args),
      getProjectGates: (...args: unknown[]) => mockGetProjectGates(...args),
      listAgents: (...args: unknown[]) => mockListAgents(...args),
      updateAgent: (...args: unknown[]) => mockUpdateAgent(...args),
    },
  };
});
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/ws-1/issues/${id}`,
    projectDetail: (id: string) => `/ws-1/projects/${id}`,
  }),
}));
vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));
vi.mock("@multica/core/hooks/use-file-upload", () => ({
  useFileUpload: () => ({ uploadWithToast: vi.fn(async () => null) }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "me" } }),
}));

// ─── useIssueTimeline (legacy read-only stream). ──────────────────────────
const timelineState = vi.hoisted(() => ({
  timeline: [] as TimelineEntry[],
  submitComment: vi.fn(async () => true),
  toggleReaction: vi.fn(),
}));
vi.mock("../../issues/hooks/use-issue-timeline", () => ({
  useIssueTimeline: () => ({
    timeline: timelineState.timeline,
    submitComment: timelineState.submitComment,
    toggleReaction: timelineState.toggleReaction,
  }),
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (_type: string, _id: string) => "Ada" }),
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <div data-testid="avatar" />,
}));

// ─── ContentEditor stub ───────────────────────────────────────────────────
const editorLast = vi.hoisted(() => ({ value: "" }));
vi.mock("../../editor", () => ({
  ReadonlyContent: ({ content }: { content: string }) => <div>{content}</div>,
  useFileDropZone: () => ({ isDragOver: false, dropZoneProps: {} }),
  FileDropOverlay: () => null,
  ContentEditor: forwardRef(function MockContentEditor(
    props: { defaultValue?: string; onUpdate?: (md: string) => void; placeholder?: string },
    ref: React.Ref<unknown>,
  ) {
    const valueRef = useRef(props.defaultValue ?? "");
    useImperativeHandle(ref, () => ({
      getMarkdown: () => valueRef.current,
      clearContent: () => { valueRef.current = ""; },
    }));
    return (
      <textarea
        data-testid="discussion-editor"
        placeholder={props.placeholder}
        onChange={(e) => {
          valueRef.current = e.target.value;
          editorLast.value = e.target.value;
          props.onUpdate?.(e.target.value);
        }}
      />
    );
  }),
}));

import { DiscussionPane } from "./discussion-pane";
import { ApiError } from "@multica/core/api";

function discussionContext() {
  return {
    session_id: SESSION_ID,
    issue_id: null,
    legacy_issue_id: null,
    coordinator_agent_id: "",
    model: "",
    thinking_level: "",
    model_source: "session_default",
    thinking_level_source: "session_default",
    degraded: false,
  };
}

function sharedMessage(id: string, content: string, at: string, author?: { type: "member" | "agent"; id: string } | null): ChatMessage {
  return {
    id,
    chat_session_id: SESSION_ID,
    role: author ? "user" : "user",
    content,
    task_id: null,
    created_at: at,
    author_type: author?.type ?? null,
    author_id: author?.id ?? null,
  };
}

const navigation: NavigationAdapter = {
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  pathname: "/ws-1",
  searchParams: new URLSearchParams(),
  hash: "",
  getShareableUrl: (path: string) => `https://app.example.test${path}`,
};

function renderPane(canConfigure = false) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <NavigationProvider value={navigation}>
          <DiscussionPane projectId="proj-1" canConfigure={canConfigure} />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("DiscussionPane (CR-2026-059 TASK-04)", () => {
  beforeEach(() => {
    mockGetProjectDiscussion.mockReset().mockResolvedValue(discussionContext());
    mockListChatMessagesPage.mockReset().mockResolvedValue({
      messages: [], limit: 50, has_more: false, next_cursor: null,
    });
    mockSendChatMessage.mockReset();
    mockPatchChatSessionConfig.mockReset();
    mockMergeForwardDiscussion.mockReset();
    mockPromoteDiscussion.mockReset();
    mockGetProjectGates.mockReset().mockResolvedValue({ crs: [] });
    mockListAgents.mockReset().mockResolvedValue([]);
    mockUpdateAgent.mockReset();
    timelineState.timeline = [];
    timelineState.submitComment.mockReset().mockResolvedValue(true);
    timelineState.toggleReaction.mockReset();
    localStorage.clear();
  });
  afterEach(cleanup);

  it("shows the empty-state greeting when there are no messages yet", async () => {
    renderPane();
    await waitFor(() => expect(screen.getByText(/Start a discussion/)).toBeTruthy());
  });

  it("renders shared messages with author bubbles and a fallback for NULL authors", async () => {
    mockListChatMessagesPage.mockResolvedValue({
      messages: [
        sharedMessage("m1", "hello team", "2026-01-01T00:00:01.000Z", { type: "member", id: "u1" }),
        sharedMessage("m2", "from nowhere", "2026-01-01T00:00:02.000Z", null),
      ],
      limit: 50, has_more: false, next_cursor: null,
    });
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(2));
    expect(screen.getByText("hello team")).toBeTruthy();
    // Member-author bubbles resolve a name; NULL authors keep the fallback
    // marker (no author label), matching the baseline rendering.
    expect(screen.getAllByTestId("discussion-author")).toHaveLength(1);
    expect(screen.getByTestId("discussion-author").textContent).toContain("Ada");
    expect(screen.getByText("from nowhere")).toBeTruthy();
  });

  it("sends through the shared session with a fresh Idempotency-Key (session identity)", async () => {
    mockSendChatMessage.mockResolvedValue({
      message_id: "m-new", task_id: null, supports_queue: false, queued: false,
    });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("discussion-editor")).toBeTruthy());

    fireEvent.change(screen.getByTestId("discussion-editor"), { target: { value: "hi all" } });
    fireEvent.click(screen.getByTestId("discussion-send"));

    await waitFor(() => expect(mockSendChatMessage).toHaveBeenCalledTimes(1));
    const [sessionId, content, attachmentIds, opts] = mockSendChatMessage.mock.calls[0] as [
      string, string, string[] | undefined, { idempotencyKey?: string } | undefined,
    ];
    expect(sessionId).toBe(SESSION_ID);
    expect(content).toBe("hi all");
    expect(attachmentIds).toBeUndefined();
    expect(opts?.idempotencyKey).toBeTruthy();
  });

  it("hard-degrades to read-only when session_id is missing (AC-17)", async () => {
    mockGetProjectDiscussion.mockResolvedValue({ ...discussionContext(), session_id: "" });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("discussion-retry")).toBeTruthy());
    // No composer, no send surface.
    expect(screen.queryByTestId("discussion-editor")).toBeNull();
    expect(screen.queryByTestId("discussion-send")).toBeNull();
  });

  it("the legacy issue stream is read-only and selectable for comment_ids merge-forward", async () => {
    mockGetProjectDiscussion.mockResolvedValue({
      ...discussionContext(),
      legacy_issue_id: "44444444-4444-4444-4444-444444444444",
    });
    timelineState.timeline = [
      {
        type: "comment",
        id: "c1",
        actor_type: "member",
        actor_id: "u1",
        content: "legacy note",
        created_at: "2026-01-01T00:00:01.000Z",
      },
    ];
    mockMergeForwardDiscussion.mockResolvedValue({ comment_id: "m1", task_id: "t1" });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("discussion-legacy-stream")).toBeTruthy());

    fireEvent.click(screen.getByTestId("discussion-legacy-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(1));
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    await screen.findByTestId("merge-forward-preview");
    // No in-flight gates → register-CR pre-checked (REQ-SUG-002 default).
    await waitFor(() => expect(screen.getByTestId("merge-forward-register-cr")).toBeChecked());
    fireEvent.click(screen.getByTestId("merge-forward-confirm"));

    // Legacy arm: comment_ids selection, NO idempotency key.
    await waitFor(() => expect(mockMergeForwardDiscussion).toHaveBeenCalledTimes(1));
    const [projectId, selection, registerCr, idempotencyKey] = mockMergeForwardDiscussion.mock.calls[0] as [
      string, { commentIds: string[] }, boolean, string | undefined,
    ];
    expect(projectId).toBe("proj-1");
    expect(selection).toEqual({ commentIds: ["c1"] });
    expect(registerCr).toBe(true);
    expect(idempotencyKey).toBeUndefined();
  });

  it("merge-forward from the shared stream uses messageIds + Idempotency-Key (B-DP-07)", async () => {
    mockListChatMessagesPage.mockResolvedValue({
      messages: [
        sharedMessage("m1", "first idea", "2026-01-01T00:00:01.000Z", { type: "member", id: "u1" }),
        sharedMessage("m2", "second idea", "2026-01-01T00:00:02.000Z", { type: "member", id: "u1" }),
      ],
      limit: 50, has_more: false, next_cursor: null,
    });
    mockMergeForwardDiscussion.mockResolvedValue({ comment_id: "m1", task_id: "t1" });
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(2));

    fireEvent.click(screen.getByTestId("discussion-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(2));
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    await screen.findByTestId("merge-forward-preview");
    await waitFor(() => expect(screen.getByTestId("merge-forward-register-cr")).toBeChecked());
    fireEvent.click(screen.getByTestId("merge-forward-confirm"));

    await waitFor(() => expect(mockMergeForwardDiscussion).toHaveBeenCalledTimes(1));
    const [projectId, selection, registerCr, idempotencyKey] = mockMergeForwardDiscussion.mock.calls[0] as [
      string, { messageIds: string[] }, boolean, string | undefined,
    ];
    expect(projectId).toBe("proj-1");
    expect(selection).toEqual({ messageIds: ["m1"] });
    expect(registerCr).toBe(true);
    expect(idempotencyKey).toBeTruthy();
  });

  it("config changes go through PATCH config and never call updateAgent", async () => {
    const coordinatorId = "66666666-6666-6666-6666-666666666666";
    mockGetProjectDiscussion.mockResolvedValue({
      ...discussionContext(),
      coordinator_agent_id: coordinatorId,
    });
    renderPane(true);
    await waitFor(() => expect(screen.getByTestId("discussion-config-row")).toBeTruthy());
    expect(mockUpdateAgent).not.toHaveBeenCalled();
    expect(mockPatchChatSessionConfig).not.toHaveBeenCalled();
  });

  it("owner/admin sees the coordinator picker; members do not", async () => {
    renderPane(true);
    await waitFor(() => expect(screen.getByTestId("discussion-coordinator-picker")).toBeTruthy());
    cleanup();
    renderPane(false);
    await waitFor(() => expect(screen.getByTestId("discussion-editor")).toBeTruthy());
    expect(screen.queryByTestId("discussion-coordinator-picker")).toBeNull();
  });

  it("keeps the selection and preview on a 429 queue-full rejection (DD-6)", async () => {
    mockListChatMessagesPage.mockResolvedValue({
      messages: [
        sharedMessage("m1", "first idea", "2026-01-01T00:00:01.000Z", { type: "member", id: "u1" }),
      ],
      limit: 50, has_more: false, next_cursor: null,
    });
    mockMergeForwardDiscussion.mockRejectedValue(
      new ApiError("full", 429, "Too Many Requests", { queue_depth: 5, queue_limit: 5 }),
    );
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(1));
    fireEvent.click(screen.getByTestId("discussion-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(1));
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    await screen.findByTestId("merge-forward-preview");

    fireEvent.click(screen.getByTestId("merge-forward-confirm"));
    await waitFor(() => expect(mockMergeForwardDiscussion).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId("merge-forward-preview")).toBeTruthy();
    expect(screen.getByTestId("discussion-batch-bar")).toBeTruthy();
    expect(screen.getByTestId("discussion-selected-count").textContent).toContain("1");
  });

  it("promotes the selected messages into a work Issue and shows the target link (AC-9)", async () => {
    mockListChatMessagesPage.mockResolvedValue({
      messages: [
        sharedMessage("m1", "promote me", "2026-01-01T00:00:01.000Z", { type: "member", id: "u1" }),
      ],
      limit: 50, has_more: false, next_cursor: null,
    });
    mockPromoteDiscussion.mockResolvedValue({
      issue_id: "aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
      issue_number: 7,
      session_id: SESSION_ID,
      source_refs: { session_id: SESSION_ID, message_ids: ["m1"], attachment_ids: [] },
      created: true,
      upgrade_to_cr: false,
      run_id: null,
    });
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(1));
    const messagesCallsBefore = mockListChatMessagesPage.mock.calls.length;

    fireEvent.click(screen.getByTestId("discussion-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(1));
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByTestId("discussion-promote-cta"));

    await waitFor(() => expect(mockPromoteDiscussion).toHaveBeenCalledTimes(1));
    const [projectId, body, key] = mockPromoteDiscussion.mock.calls[0] as [string, Record<string, unknown>, string];
    expect(projectId).toBe("proj-1");
    expect(body.session_id).toBe(SESSION_ID);
    expect(body.message_ids).toEqual(["m1"]);
    expect(body.upgrade_to_cr).toBe(false);
    expect(key).toBeTruthy();

    // Success: selection exits, the target Issue link is visible, and the
    // Discussion stream was NOT reloaded (AC-9).
    await waitFor(() => expect(screen.getByTestId("discussion-promoted-link")).toBeTruthy());
    expect(screen.queryByTestId("discussion-batch-bar")).toBeNull();
    expect(screen.getByTestId("discussion-promoted-link").textContent).toContain("#7");
    expect(mockListChatMessagesPage.mock.calls.length).toBe(messagesCallsBefore);
  });

  it("upgrades the selected messages to a CR and keeps the selection on failure (AC-9/AC-11)", async () => {
    mockListChatMessagesPage.mockResolvedValue({
      messages: [
        sharedMessage("m1", "upgrade me", "2026-01-01T00:00:01.000Z", { type: "member", id: "u1" }),
      ],
      limit: 50, has_more: false, next_cursor: null,
    });
    mockPromoteDiscussion
      .mockRejectedValueOnce(new ApiError("bad selection", 400, "Bad Request", { code: "invalid_promotion_selection" }))
      .mockResolvedValueOnce({
        issue_id: "aaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
        issue_number: 8,
        session_id: SESSION_ID,
        source_refs: { session_id: SESSION_ID, message_ids: ["m1"], attachment_ids: [] },
        created: true,
        upgrade_to_cr: true,
        run_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
      });
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(1));

    fireEvent.click(screen.getByTestId("discussion-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(1));
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);

    // First attempt fails: the selection is KEPT (AC-11), retry allowed.
    fireEvent.click(screen.getByTestId("discussion-promote-cr-cta"));
    await waitFor(() => expect(mockPromoteDiscussion).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId("discussion-batch-bar")).toBeTruthy();
    expect(screen.getByTestId("discussion-selected-count").textContent).toContain("1");

    // Retry succeeds with upgrade_to_cr=true and a fresh key.
    fireEvent.click(screen.getByTestId("discussion-promote-cr-cta"));
    await waitFor(() => expect(mockPromoteDiscussion).toHaveBeenCalledTimes(2));
    const [, body, key] = mockPromoteDiscussion.mock.calls[1] as [string, Record<string, unknown>, string];
    expect(body.upgrade_to_cr).toBe(true);
    expect(key).toBeTruthy();
    await waitFor(() => expect(screen.getByTestId("discussion-promoted-link")).toBeTruthy());
    expect(screen.getByTestId("discussion-promoted-link").textContent).toContain("#8");
  });
});
