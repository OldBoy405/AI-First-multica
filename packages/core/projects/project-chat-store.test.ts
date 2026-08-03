import { describe, it, expect, beforeEach } from "vitest";
import {
  useProjectChatStore,
  projectChatDraftKey,
} from "./project-chat-store";
import type { Attachment } from "../types";

function reset() {
  useProjectChatStore.setState({
    drafts: {},
    activeMode: {},
    tutorialSeen: {},
    agentRequestFilter: {},
    draftAttachments: {},
  });
}

function makeAttachment(id: string): Attachment {
  return {
    id,
    filename: `${id}.png`,
    url: `https://cdn.example/${id}.png`,
    content_type: "image/png",
    size_bytes: 1,
  } as Attachment;
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

  // CR-2026-012 DD-10: draft attachment slots follow the same isolation
  // semantics as drafts, and rehydrate onto {} for older snapshots.
  describe("draftAttachments (CR-2026-012)", () => {
    it("defaults to an empty map (old-snapshot rehydration compatibility)", () => {
      expect(useProjectChatStore.getState().draftAttachments).toEqual({});
    });

    it("isolates attachments by projectId AND mode", () => {
      const { addDraftAttachment } = useProjectChatStore.getState();
      addDraftAttachment("p1", "team_agent", makeAttachment("a1"));
      addDraftAttachment("p1", "private_ask", makeAttachment("a2"));
      addDraftAttachment("p2", "team_agent", makeAttachment("a3"));

      const { draftAttachments } = useProjectChatStore.getState();
      expect(draftAttachments[projectChatDraftKey("p1", "team_agent")]?.map((a) => a.id)).toEqual(["a1"]);
      expect(draftAttachments[projectChatDraftKey("p1", "private_ask")]?.map((a) => a.id)).toEqual(["a2"]);
      expect(draftAttachments[projectChatDraftKey("p2", "team_agent")]?.map((a) => a.id)).toEqual(["a3"]);
    });

    it("upserts by id instead of duplicating", () => {
      const { addDraftAttachment } = useProjectChatStore.getState();
      addDraftAttachment("p1", "team_agent", makeAttachment("a1"));
      addDraftAttachment("p1", "team_agent", {
        ...makeAttachment("a1"),
        filename: "renamed.png",
      });

      const rows =
        useProjectChatStore.getState().draftAttachments[
          projectChatDraftKey("p1", "team_agent")
        ];
      expect(rows).toHaveLength(1);
      expect(rows?.[0]?.filename).toBe("renamed.png");
    });

    it("ignores attachments without an id", () => {
      const { addDraftAttachment } = useProjectChatStore.getState();
      addDraftAttachment("p1", "team_agent", {
        ...makeAttachment("a1"),
        id: "",
      });
      expect(
        useProjectChatStore.getState().draftAttachments[
          projectChatDraftKey("p1", "team_agent")
        ],
      ).toBeUndefined();
    });

    it("setDraftAttachments replaces; empty array drops the slot", () => {
      const { setDraftAttachments } = useProjectChatStore.getState();
      const key = projectChatDraftKey("p1", "team_agent");
      setDraftAttachments("p1", "team_agent", [
        makeAttachment("a1"),
        makeAttachment("a2"),
      ]);
      expect(useProjectChatStore.getState().draftAttachments[key]).toHaveLength(2);

      setDraftAttachments("p1", "team_agent", []);
      expect(key in useProjectChatStore.getState().draftAttachments).toBe(false);
    });

    it("clearing the draft also clears its attachment slot", () => {
      const { setDraft, setDraftAttachments } = useProjectChatStore.getState();
      const key = projectChatDraftKey("p1", "team_agent");
      setDraft("p1", "team_agent", "with files");
      setDraftAttachments("p1", "team_agent", [makeAttachment("a1")]);
      expect(useProjectChatStore.getState().draftAttachments[key]).toHaveLength(1);

      setDraft("p1", "team_agent", "");
      expect(key in useProjectChatStore.getState().draftAttachments).toBe(false);
      // Other slots survive.
      const otherKey = projectChatDraftKey("p2", "team_agent");
      useProjectChatStore
        .getState()
        .setDraftAttachments("p2", "team_agent", [makeAttachment("b1")]);
      setDraft("p1", "team_agent", "");
      expect(otherKey in useProjectChatStore.getState().draftAttachments).toBe(true);
    });
  });
});
