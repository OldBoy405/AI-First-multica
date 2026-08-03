// @vitest-environment jsdom
import { forwardRef, useImperativeHandle, useRef } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { TimelineEntry } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

// ─── Discussion tab data source (CR-2026-009) ──────────────────────────────
const mockGetProjectDiscussion = vi.hoisted(() => vi.fn());
// CR-2026-012 TASK-07: merge-forward endpoint + CR gates (register-CR default).
const mockMergeForwardDiscussion = vi.hoisted(() => vi.fn());
const mockGetProjectGates = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => {
  const actual = await importActual<typeof import("@multica/core/api")>();
  return {
    ...actual,
    api: {
      getProjectDiscussion: (...args: unknown[]) => mockGetProjectDiscussion(...args),
      getProjectGates: (...args: unknown[]) => mockGetProjectGates(...args),
      mergeForwardDiscussion: (...args: unknown[]) => mockMergeForwardDiscussion(...args),
    },
  };
});
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

// ─── useIssueTimeline (CR-2026-009): mocked as a whole so this test drives
// the stream/composer through a controllable timeline + spy mutations,
// without pulling in the full API/WS mock stack the hook's own dedicated
// test (use-issue-timeline.test.tsx) already covers. ───────────────────────
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
  useActorName: () => ({ getActorName: () => "Ada" }),
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <div data-testid="avatar" />,
}));

// ─── ContentEditor stub (same pattern as chat-input.test.tsx): a plain
// textarea driving onUpdate, with an imperative handle for getMarkdown /
// clearContent so the composer's submit flow is exercised without TipTap. ──
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

function renderPane(canConfigure = false) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <DiscussionPane projectId="proj-1" canConfigure={canConfigure} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("DiscussionPane (CR-2026-009 TASK-03)", () => {
  beforeEach(() => {
    mockGetProjectDiscussion.mockReset();
    mockMergeForwardDiscussion.mockReset();
    mockGetProjectGates.mockReset().mockResolvedValue({ crs: [] });
    timelineState.timeline = [];
    timelineState.submitComment.mockReset().mockResolvedValue(true);
    timelineState.toggleReaction.mockReset();
    localStorage.clear();
  });
  afterEach(cleanup);

  it("shows the empty-state greeting when there are no messages yet", async () => {
    mockGetProjectDiscussion.mockResolvedValue({ issue_id: "disc-1" });
    renderPane();
    await waitFor(() => expect(screen.getByText(/Start a discussion/)).toBeTruthy());
  });

  it("renders each comment as a discussion message with avatar and reaction bar", async () => {
    mockGetProjectDiscussion.mockResolvedValue({ issue_id: "disc-1" });
    timelineState.timeline = [
      {
        type: "comment",
        id: "c1",
        actor_type: "member",
        actor_id: "u1",
        content: "hello team",
        created_at: new Date().toISOString(),
      },
      // Non-comment entries (activity) must be filtered out — Discussion has
      // no task/activity noise, only human messages.
      {
        type: "activity",
        id: "a1",
        actor_type: "member",
        actor_id: "u1",
        created_at: new Date().toISOString(),
      },
    ];
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(1));
    expect(screen.getByText("hello team")).toBeTruthy();
    expect(screen.getByTestId("avatar")).toBeTruthy();
  });

  it("sending a message calls submitComment and clears the composer on success", async () => {
    mockGetProjectDiscussion.mockResolvedValue({ issue_id: "disc-1" });
    renderPane();
    await waitFor(() => expect(screen.getByTestId("discussion-editor")).toBeTruthy());

    fireEvent.change(screen.getByTestId("discussion-editor"), { target: { value: "hi all" } });
    fireEvent.click(screen.getByTestId("discussion-send"));

    await waitFor(() =>
      expect(timelineState.submitComment).toHaveBeenCalledWith("hi all", undefined),
    );
  });

  it("the send button is disabled while the composer is empty", async () => {
    mockGetProjectDiscussion.mockResolvedValue({ issue_id: "disc-1" });
    renderPane();
    await waitFor(() =>
      expect(screen.getByTestId("discussion-send")).toHaveProperty("disabled", true),
    );
  });
});

// ─── Multi-select + merge-forward (CR-2026-012 TASK-07) ──────────────────────

function discussionComment(id: string, content: string, at: string): TimelineEntry {
  return {
    type: "comment",
    id,
    actor_type: "member",
    actor_id: "u1",
    content,
    created_at: at,
  };
}

describe("DiscussionPane merge-forward (CR-2026-012 TASK-07)", () => {
  beforeEach(() => {
    mockGetProjectDiscussion.mockReset().mockResolvedValue({ issue_id: "disc-1" });
    mockMergeForwardDiscussion.mockReset();
    mockGetProjectGates.mockReset().mockResolvedValue({ crs: [] });
    timelineState.timeline = [];
    timelineState.submitComment.mockReset().mockResolvedValue(true);
    timelineState.toggleReaction.mockReset();
    localStorage.clear();
  });
  afterEach(cleanup);

  async function enterSelectModeWithMessages() {
    timelineState.timeline = [
      discussionComment("c1", "first idea", "2026-01-01T00:00:01.000Z"),
      discussionComment("c2", "second idea", "2026-01-01T00:00:02.000Z"),
      discussionComment("c3", "third idea", "2026-01-01T00:00:03.000Z"),
    ];
    renderPane();
    await waitFor(() => expect(screen.getAllByTestId("discussion-message")).toHaveLength(3));
    fireEvent.click(screen.getByTestId("discussion-select-entry"));
    await waitFor(() => expect(screen.getAllByTestId("discussion-select-checkbox")).toHaveLength(3));
  }

  it("selects messages, previews the three-part structure, and forwards on confirm", async () => {
    mockMergeForwardDiscussion.mockResolvedValue({ comment_id: "m1", task_id: "t1" });
    await enterSelectModeWithMessages();

    // Select c1 + c3 (out of order — the preview must render ascending).
    const checkboxes = screen.getAllByTestId("discussion-select-checkbox");
    fireEvent.click(checkboxes[2]!);
    fireEvent.click(checkboxes[0]!);
    await waitFor(() =>
      expect(screen.getByTestId("discussion-selected-count").textContent).toContain("2"),
    );

    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    const preview = await screen.findByTestId("merge-forward-preview");
    // Trigger message = the EARLIEST selected message.
    expect(screen.getByTestId("merge-forward-trigger").textContent).toContain("first idea");
    // History lists both selected messages in ascending order.
    const history = screen.getByTestId("merge-forward-history").textContent ?? "";
    expect(history).toContain("first idea");
    expect(history).toContain("third idea");
    expect(history).not.toContain("second idea");
    expect(history.indexOf("first idea")).toBeLessThan(history.indexOf("third idea"));
    expect(preview.textContent).toContain("2 messages");
    // No in-flight gates → register-CR pre-checked (REQ-SUG-002 default).
    await waitFor(() =>
      expect(screen.getByTestId("merge-forward-register-cr")).toBeChecked(),
    );

    fireEvent.click(screen.getByTestId("merge-forward-confirm"));
    await waitFor(() =>
      expect(mockMergeForwardDiscussion).toHaveBeenCalledWith("proj-1", ["c1", "c3"], true),
    );
    // Success exits the multi-select mode.
    await waitFor(() =>
      expect(screen.queryByTestId("discussion-batch-bar")).toBeNull(),
    );
  });

  it("keeps the selection and preview on a 429 queue-full rejection (DD-6)", async () => {
    mockMergeForwardDiscussion.mockRejectedValue(
      new ApiError("full", 429, "Too Many Requests", { queue_depth: 5, queue_limit: 5 }),
    );
    await enterSelectModeWithMessages();
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    await screen.findByTestId("merge-forward-preview");

    fireEvent.click(screen.getByTestId("merge-forward-confirm"));
    await waitFor(() => expect(mockMergeForwardDiscussion).toHaveBeenCalledTimes(1));
    // Both the preview and the multi-select state survive the rejection.
    expect(screen.getByTestId("merge-forward-preview")).toBeTruthy();
    expect(screen.getByTestId("discussion-batch-bar")).toBeTruthy();
    expect(screen.getByTestId("discussion-selected-count").textContent).toContain("1");
  });

  it("defaults register-CR to unchecked when the gates endpoint errors (TSUG-003)", async () => {
    mockGetProjectGates.mockRejectedValue(new Error("no approval service"));
    await enterSelectModeWithMessages();
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[1]!);
    fireEvent.click(screen.getByTestId("discussion-merge-cta"));
    await screen.findByTestId("merge-forward-preview");

    // Give the (failing) gates query a tick to settle.
    await waitFor(() =>
      expect(screen.getByTestId("merge-forward-register-cr")).not.toBeChecked(),
    );
  });

  it("cancel exits multi-select with zero side effects", async () => {
    await enterSelectModeWithMessages();
    fireEvent.click(screen.getAllByTestId("discussion-select-checkbox")[0]!);
    fireEvent.click(screen.getByText(enProjects.chat.merged_forward.cancel));

    await waitFor(() => expect(screen.queryByTestId("discussion-batch-bar")).toBeNull());
    expect(screen.queryAllByTestId("discussion-select-checkbox")).toHaveLength(0);
    expect(screen.getByTestId("discussion-select-entry")).toBeTruthy();
  });

  it("owner/admin sees the coordinator picker; members do not", async () => {
    renderPane(true);
    await waitFor(() => expect(screen.getByTestId("discussion-coordinator-picker")).toBeTruthy());
    cleanup();
    renderPane(false);
    // Composer renders once the discussion context resolves — the picker row
    // must stay absent for non-owner/admin callers.
    await waitFor(() => expect(screen.getByTestId("discussion-editor")).toBeTruthy());
    expect(screen.queryByTestId("discussion-coordinator-picker")).toBeNull();
  });
});
