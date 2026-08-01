// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { useProjectChatStore } from "@multica/core/projects";
import enCommon from "../../locales/en/common.json";
import enProjects from "../../locales/en/projects.json";

const TEST_RESOURCES = { en: { common: enCommon, projects: enProjects } };

const mockGetProjectChat = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getProjectChat: (...args: unknown[]) => mockGetProjectChat(...args),
  },
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

// The panel only gates on config state; the Team Agent stream + composer are
// covered by project-team-agent-chat.test.tsx. Stub the child so this test
// stays focused on the delegation and doesn't pull in the timeline/WS stack.
vi.mock("./project-team-agent-chat", () => ({
  ProjectTeamAgentChat: (props: { issueId: string }) => (
    <div data-testid="project-team-agent-chat" data-issue={props.issueId} />
  ),
}));

import { ProjectChatPanel } from "./project-chat-panel";

function renderPanel(canConfigure: boolean) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ProjectChatPanel projectId="proj-1" canConfigure={canConfigure} />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

describe("ProjectChatPanel (CR-2026-006 TASK-03)", () => {
  beforeEach(() => {
    mockGetProjectChat.mockReset();
    useProjectChatStore.setState({ drafts: {}, activeMode: {}, tutorialSeen: {} });
    localStorage.clear();
  });
  afterEach(cleanup);


  it("switching to a non-Team-Agent mode triggers no network request", async () => {
    mockGetProjectChat.mockResolvedValue({ issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true);
    // Team Agent is the default mode and fetches once.
    await waitFor(() => expect(mockGetProjectChat).toHaveBeenCalledTimes(1));

    fireEvent.click(screen.getByRole("tab", { name: /Private Ask/ }));
    fireEvent.click(screen.getByRole("tab", { name: /Discussion/ }));
    // No additional fetches from the placeholder modes.
    expect(mockGetProjectChat).toHaveBeenCalledTimes(1);
  });

  it("unconfigured Team Agent shows the configure CTA for owner/admin", async () => {
    mockGetProjectChat.mockResolvedValue({ issue_id: "i1", team_agent_id: "" });
    renderPanel(true);
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-unconfigured-admin")).toBeTruthy(),
    );
    expect(screen.queryByTestId("project-chat-unconfigured-member")).toBeNull();
  });

  it("unconfigured Team Agent tells non-admins to contact the owner", async () => {
    mockGetProjectChat.mockResolvedValue({ issue_id: "i1", team_agent_id: "" });
    renderPanel(false);
    await waitFor(() =>
      expect(screen.getByTestId("project-chat-unconfigured-member")).toBeTruthy(),
    );
    expect(screen.queryByTestId("project-chat-unconfigured-admin")).toBeNull();
  });

  it("configured Team Agent renders the message stream for the backing issue (TASK-04)", async () => {
    mockGetProjectChat.mockResolvedValue({ issue_id: "i1", team_agent_id: "a1" });
    renderPanel(true);
    await waitFor(() =>
      expect(screen.getByTestId("project-team-agent-chat")).toBeTruthy(),
    );
    expect(screen.getByTestId("project-team-agent-chat").getAttribute("data-issue")).toBe("i1");
  });
});
