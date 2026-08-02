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
vi.mock("@multica/core/api", () => ({
  api: { getProjectDiscussion: (...args: unknown[]) => mockGetProjectDiscussion(...args) },
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

function renderPane() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <DiscussionPane projectId="proj-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("DiscussionPane (CR-2026-009 TASK-03)", () => {
  beforeEach(() => {
    mockGetProjectDiscussion.mockReset();
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
