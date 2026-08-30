// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { useProjectChatStore } from "@multica/core/projects";
import { NavigationProvider } from "../../navigation/context";
import type { NavigationAdapter } from "../../navigation/types";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const mockGetProjectChat = vi.hoisted(() => vi.fn());
const mockGetProjectPresenter = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getProjectChat: (...args: unknown[]) => mockGetProjectChat(...args),
    getProjectPresenter: (...args: unknown[]) => mockGetProjectPresenter(...args),
  },
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Presenting Member" }),
}));

vi.mock("@multica/core/workspace/queries", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@multica/core/workspace/queries")>();
  return {
    ...actual,
    memberListOptions: (_wsId: string) => ({
      queryKey: ["members"],
      queryFn: async () => [],
    }),
  };
});

// PresenterHeader always mounts PresenterControlSheet (Sheet content, closed
// by default) alongside the trigger button, so its dependencies need the
// same seams even though these tests never open the panel.
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "u-1" } }),
}));

// The panel only gates on config state; the Team Agent stream + composer are
// covered by project-team-agent-chat.test.tsx. Stub the child so this test
// stays focused on the delegation and doesn't pull in the timeline/WS stack.
vi.mock("./project-team-agent-chat", () => ({
  ProjectTeamAgentChat: (props: { issueId: string; sessionId: string }) => (
    <div
      data-testid="project-team-agent-chat"
      data-issue={props.issueId}
      data-session={props.sessionId}
    />
  ),
}));

// Same reasoning for the Private Ask pane (CR-2026-008): its session
// get-or-create, stream and composer are covered by project-private-ask.test.tsx.
vi.mock("./project-private-ask", () => ({
  ProjectPrivateAsk: () => <div data-testid="project-private-ask" />,
}));

// Discussion's own stream/composer are covered by discussion-pane.test.tsx
// (CR-2026-009). Stub it here for the same reason as ProjectTeamAgentChat
// above — this file only asserts tab-switch delegation.
vi.mock("./discussion-pane", () => ({
  DiscussionPane: (props: { projectId: string }) => (
    <div data-testid="discussion-pane" data-project={props.projectId} />
  ),
}));

import { ProjectChatPanel } from "./project-chat-panel";

function makeNavAdapter(overrides: Partial<NavigationAdapter> = {}): NavigationAdapter {
  return {
    push: () => {},
    replace: () => {},
    back: () => {},
    pathname: "/",
    searchParams: new URLSearchParams(),
    getShareableUrl: (p) => p,
    ...overrides,
  };
}

function renderPanel(canConfigure: boolean, nav: NavigationAdapter = makeNavAdapter()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <NavigationProvider value={nav}>
          <ProjectChatPanel projectId="proj-1" canConfigure={canConfigure} />
        </NavigationProvider>
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("ProjectChatPanel (CR-2026-006 TASK-03)", () => {
  beforeEach(() => {
    mockGetProjectChat.mockReset();
    mockGetProjectPresenter.mockReset();
    mockGetProjectPresenter.mockResolvedValue({ presenter: null, pending_requests: [], my_request: null });
    useProjectChatStore.setState({ drafts: {}, activeMode: {}, tutorialSeen: {} });
    localStorage.clear();
  });
  afterEach(cleanup);


  it("switching to a non-Team-Agent mode triggers no network request", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true);
    // Team Agent is the default mode and fetches once.
    await waitFor(() => expect(mockGetProjectChat).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByRole("tab", { name: /Private Ask/ }));
    fireEvent.click(screen.getByRole("tab", { name: /Discussion/ }));
    // No additional fetches from the placeholder modes.
    expect(mockGetProjectChat).toHaveBeenCalledTimes(1);
  });

  it("unconfigured Team Agent shows the configure CTA for owner/admin", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "" });
    renderPanel(true);
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-unconfigured-admin")).toBeTruthy(),
    );
    expect(screen.queryByTestId("project-chat-unconfigured-member")).toBeNull();
  });

  it("unconfigured Team Agent tells non-admins to contact the owner", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "" });
    renderPanel(false);
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-unconfigured-member")).toBeTruthy(),
    );
    expect(screen.queryByTestId("project-chat-unconfigured-admin")).toBeNull();
  });

  it("?mode=discussion deep-links straight to the Discussion tab (CR-2026-009)", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true, makeNavAdapter({ searchParams: new URLSearchParams("mode=discussion") }));
    await waitFor(() => expect(screen.getByTestId("discussion-pane")).toBeTruthy());
  });

  it("an invalid ?mode= value is ignored and falls back to Team Agent", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true, makeNavAdapter({ searchParams: new URLSearchParams("mode=not-a-real-mode") }));
    await waitFor(() => expect(screen.getByTestId("project-team-agent-chat")).toBeTruthy());
  });

  it("configured Team Agent renders the message stream for the backing issue (TASK-04)", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true);
    await waitFor(() =>
      expect(screen.getByTestId("project-team-agent-chat")).toBeTruthy(),
    );
    expect(screen.getByTestId("project-team-agent-chat").getAttribute("data-issue")).toBe("i1");
    expect(screen.getByTestId("project-team-agent-chat").getAttribute("data-session")).toBe("s1");
  });

  // CR-2026-056 AC-11: a session with no container yet is fully configured —
  // the stream mounts with an empty issue id, not the unconfigured guide.
  it("renders the stream without a container (issue_id null, AC-11)", async () => {
    mockGetProjectChat.mockResolvedValue({
      session_id: "s1",
      issue_id: null,
      team_agent_id: "a1",
      model: "claude-opus",
      thinking_level: "high",
      model_source: "session_default",
      thinking_level_source: "session_default",
    });
    renderPanel(true);
    await waitFor(() =>
      expect(screen.getByTestId("project-team-agent-chat")).toBeTruthy(),
    );
    expect(screen.getByTestId("project-team-agent-chat").getAttribute("data-issue")).toBe("");
    expect(screen.queryByTestId("project-chat-unconfigured-admin")).toBeNull();
  });

  // CR-2026-056 AC-27 hard degradation: an empty session_id lands read-only
  // in the unconfigured branch with an explicit retry affordance.
  it("hard-degraded chat context renders read-only with a retry affordance (AC-27)", async () => {
    mockGetProjectChat.mockResolvedValue({
      session_id: "",
      issue_id: null,
      team_agent_id: "a1",
      model: "",
      thinking_level: "",
      model_source: "runtime_default",
      thinking_level_source: "runtime_default",
    });
    renderPanel(false);
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-unconfigured-member")).toBeTruthy(),
    );
    expect(screen.getByTestId("project-chat-config-retry")).toBeTruthy();
    expect(screen.queryByTestId("project-team-agent-chat")).toBeNull();
  });

  // CR-2026-010 TASK-05: presenter header on the Team Agent tab.
  it("shows the Owner/Admin default when no presenter is active", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    mockGetProjectPresenter.mockResolvedValue({ presenter: null, pending_requests: [], my_request: null });
    renderPanel(true);
    await waitFor(() => expect(mockGetProjectPresenter).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Owner/Admin")).toBeTruthy();
  });

  it("shows the current presenter's name when a presenter is active", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    mockGetProjectPresenter.mockResolvedValue({
      presenter: { user_id: "u-1", status: "active", granted_by: "u-2", created_at: "2026-01-01T00:00:00Z" },
      pending_requests: [],
      my_request: null,
    });
    renderPanel(true);
    expect(await screen.findByText("Presenter: Presenting Member")).toBeTruthy();
  });

  it("hides the presenter header on non-Team-Agent tabs", async () => {
    mockGetProjectChat.mockResolvedValue({ session_id: "s1", issue_id: "i1", team_agent_id: "a1" });
    mockGetProjectPresenter.mockResolvedValue({ presenter: null, pending_requests: [], my_request: null });
    renderPanel(true);
    await screen.findByText("Owner/Admin");

    fireEvent.click(screen.getByRole("tab", { name: /Private Ask/ }));
    expect(screen.queryByText("Owner/Admin")).toBeNull();
  });
});
