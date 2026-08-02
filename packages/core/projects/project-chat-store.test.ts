import { describe, it, expect, beforeEach } from "vitest";
import {
  useProjectChatStore,
  projectChatDraftKey,
} from "./project-chat-store";

function reset() {
  useProjectChatStore.setState({
    drafts: {},
    activeMode: {},
    tutorialSeen: {},
    agentRequestFilter: {},
  });
}

describe("useProjectChatStore (CR-2026-006)", () => {
  beforeEach(reset);

  it("isolates drafts by projectId AND mode", () => {
    const { setDraft } = useProjectChatStore.getState();
    setDraft("p1", "team_agent", "hi team");
    setDraft("p1", "private_ask", "secret");
    setDraft("p2", "team_agent", "other project");

    const { drafts } = useProjectChatStore.getState();
    expect(drafts[projectChatDraftKey("p1", "team_agent")]).toBe("hi team");
    expect(drafts[projectChatDraftKey("p1", "private_ask")]).toBe("secret");
    expect(drafts[projectChatDraftKey("p2", "team_agent")]).toBe("other project");
    // Same mode, different project must not collide.
    expect(drafts[projectChatDraftKey("p1", "team_agent")]).not.toBe(
      drafts[projectChatDraftKey("p2", "team_agent")],
    );
  });

  it("clears a draft when set to empty", () => {
    const { setDraft } = useProjectChatStore.getState();
    setDraft("p1", "team_agent", "draft");
    expect(
      useProjectChatStore.getState().drafts[
        projectChatDraftKey("p1", "team_agent")
      ],
    ).toBe("draft");

    setDraft("p1", "team_agent", "");
    expect(
      projectChatDraftKey("p1", "team_agent") in
        useProjectChatStore.getState().drafts,
    ).toBe(false);
  });

  it("tracks activeMode per project", () => {
    const { setActiveMode } = useProjectChatStore.getState();
    setActiveMode("p1", "discussion");
    setActiveMode("p2", "private_ask");
    const { activeMode } = useProjectChatStore.getState();
    expect(activeMode["p1"]).toBe("discussion");
    expect(activeMode["p2"]).toBe("private_ask");
  });

  it("marks the tutorial seen per projectId+mode", () => {
    const { dismissTutorial } = useProjectChatStore.getState();
    dismissTutorial("p1", "team_agent");
    const { tutorialSeen } = useProjectChatStore.getState();
    expect(tutorialSeen[projectChatDraftKey("p1", "team_agent")]).toBe(true);
    expect(
      tutorialSeen[projectChatDraftKey("p1", "private_ask")],
    ).toBeUndefined();
  });

  it("tracks the agent-request filter per project, defaulting to off (CR-2026-007)", () => {
    // Absent key = filter off; older persisted snapshots rehydrate onto {}.
    expect(useProjectChatStore.getState().agentRequestFilter["p1"]).toBeUndefined();

    const { setAgentRequestFilter } = useProjectChatStore.getState();
    setAgentRequestFilter("p1", true);
    expect(useProjectChatStore.getState().agentRequestFilter["p1"]).toBe(true);
    // Other projects stay unaffected.
    expect(useProjectChatStore.getState().agentRequestFilter["p2"]).toBeUndefined();

    setAgentRequestFilter("p1", false);
    expect(useProjectChatStore.getState().agentRequestFilter["p1"]).toBe(false);
  });
});
