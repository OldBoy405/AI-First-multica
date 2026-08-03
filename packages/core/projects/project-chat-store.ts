import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import {
  createWorkspaceAwareStorage,
  registerForWorkspaceRehydration,
} from "../platform/workspace-storage";
import { defaultStorage } from "../platform/storage";
import type { Attachment } from "../types";

// The three chat modes of a project group-chat window (CR-2026-006).
export type ProjectChatMode = "team_agent" | "private_ask" | "discussion";

/**
 * Draft/tutorial storage key. Drafts are isolated per project AND per mode so
 * switching tabs (or projects) never leaks one composer's text into another.
 */
export function projectChatDraftKey(
  projectId: string,
  mode: ProjectChatMode,
): string {
  return `${projectId}:${mode}`;
}

interface ProjectChatStore {
  /** `${projectId}:${mode}` → composer draft text. */
  drafts: Record<string, string>;
  /** projectId → last active mode (defaults to team_agent). */
  activeMode: Record<string, ProjectChatMode>;
  /** `${projectId}:${mode}` → true once the first-entry tutorial bubble is dismissed. */
  tutorialSeen: Record<string, boolean>;
  /**
   * projectId → "agent requests only" filter toggle (CR-2026-007 DD-4).
   * Absent key = off. Older persisted snapshots lack this field entirely;
   * the `{}` initial value below is what they rehydrate onto.
   */
  agentRequestFilter: Record<string, boolean>;
  /**
   * `${projectId}:${mode}` → attachment rows referenced by that composer
   * draft (CR-2026-012 DD-10). Same isolation semantics as `drafts`:
   * switching tabs or projects never leaks one composer's files into
   * another. Older persisted snapshots lack this field; the `{}` initial
   * value below is what they rehydrate onto (zustand's shallow merge keeps
   * the initial for absent keys).
   */
  draftAttachments: Record<string, Attachment[]>;
  setDraft: (projectId: string, mode: ProjectChatMode, text: string) => void;
  setActiveMode: (projectId: string, mode: ProjectChatMode) => void;
  dismissTutorial: (projectId: string, mode: ProjectChatMode) => void;
  setAgentRequestFilter: (projectId: string, value: boolean) => void;
  /** Replace the draft attachments for a composer slot. */
  setDraftAttachments: (
    projectId: string,
    mode: ProjectChatMode,
    attachments: Attachment[],
  ) => void;
  /** Append (or upsert by id) one attachment row to a composer slot. */
  addDraftAttachment: (
    projectId: string,
    mode: ProjectChatMode,
    attachment: Attachment,
  ) => void;
}

// A dedicated store — deliberately NOT the global chat store, whose
// activeSessionId is a single shared singleton for the floating chat and the
// /chat page. A third consumer there would fight over the active session.
export const useProjectChatStore = create<ProjectChatStore>()(
  persist(
    (set) => ({
      drafts: {},
      activeMode: {},
      tutorialSeen: {},
      agentRequestFilter: {},
      draftAttachments: {},
      setDraft: (projectId, mode, text) =>
        set((s) => {
          const key = projectChatDraftKey(projectId, mode);
          const drafts = { ...s.drafts };
          if (text) drafts[key] = text;
          else delete drafts[key];
          // Clearing the draft also drops its attachment slot — a cleared
          // composer must not resurrect stale files on the next visit
          // (DD-10 symmetry with the global chat store's clearInputDraft).
          const draftAttachments = { ...s.draftAttachments };
          if (!text) delete draftAttachments[key];
          return { drafts, draftAttachments };
        }),
      setActiveMode: (projectId, mode) =>
        set((s) => ({ activeMode: { ...s.activeMode, [projectId]: mode } })),
      dismissTutorial: (projectId, mode) =>
        set((s) => ({
          tutorialSeen: {
            ...s.tutorialSeen,
            [projectChatDraftKey(projectId, mode)]: true,
          },
        })),
      setAgentRequestFilter: (projectId, value) =>
        set((s) => ({
          agentRequestFilter: { ...s.agentRequestFilter, [projectId]: value },
        })),
      setDraftAttachments: (projectId, mode, attachments) =>
        set((s) => {
          const key = projectChatDraftKey(projectId, mode);
          const next = { ...s.draftAttachments };
          if (attachments.length > 0) next[key] = attachments;
          else delete next[key];
          return { draftAttachments: next };
        }),
      addDraftAttachment: (projectId, mode, attachment) =>
        set((s) => {
          if (!attachment.id) return s;
          const key = projectChatDraftKey(projectId, mode);
          const existing = s.draftAttachments[key] ?? [];
          const nextForKey = existing.some((a) => a.id === attachment.id)
            ? existing.map((a) => (a.id === attachment.id ? attachment : a))
            : [...existing, attachment];
          return {
            draftAttachments: { ...s.draftAttachments, [key]: nextForKey },
          };
        }),
    }),
    {
      name: "multica_project_chat",
      storage: createJSONStorage(() =>
        createWorkspaceAwareStorage(defaultStorage),
      ),
    },
  ),
);

registerForWorkspaceRehydration(() => useProjectChatStore.persist.rehydrate());
