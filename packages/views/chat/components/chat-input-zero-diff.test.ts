/**
 * CR-2026-062 TASK-01 — symbol-level zero-diff checker (plan §6.2 cmd-09, B-006).
 *
 * Extraction source: packages/views/chat/components/chat-input.tsx
 *   @ baseline 117fc6be657f91d43df5892b52782a18329c7aed
 *   (multica CR-2026-062 requirement worktree, BEFORE any modification).
 * Snapshot regions (1-based lines, verbatim from the baseline, LF-normalized):
 *   S1  interface ChatInputProps { … }                    L58–160
 *   S2  export function ChatInput({ … }: ChatInputProps)  L162–790 (full body)
 *   S3  export interface ChatInputDraftAdapter { … }      L810–827
 *   S4  interface ChatInputCoreProps extends … { … }      L829–833
 *
 * Contract (SDD §9 zero_diff / TASK-01): CR-2026-062 may only modify the
 * `ChatInputCore` function body (L835–end). This checker fails when any of
 * the four regions drifts from the baseline, or when the five symbol anchors
 * no longer appear exactly once. It runs BEFORE the first edit (proving the
 * snapshots were captured correctly against the unmodified baseline) and is
 * re-run AFTER every edit (proving the modification never touched the four
 * zero-diff regions).
 *
 * Line-ending discipline (AGENTS.md #1): a Windows checkout can rewrite
 * LF → CRLF (autocrlf); every read is normalized to `\n` before comparing.
 * Read/parse failures are HARD failures — never silently degrade.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { describe, expect, it } from "vitest";

// ---------------------------------------------------------------------------
// Baseline snapshots — verbatim LF-normalized text of the four zero-diff
// regions. Generated once from 117fc6be before any modification.
// ---------------------------------------------------------------------------

// The two large regions stay in fixtures/*.txt instead of inline template
// literals: scripts/check-ui-radius-tokens.mjs scans string literals, and a
// verbatim copy of the source would re-report tokens (e.g. a bare `rounded`
// inside a comment) that the real file only carries as a comment.
function loadBaselineFixture(name: string): string {
  const here = dirname(fileURLToPath(import.meta.url));
  const raw = readFileSync(join(here, "fixtures", name), "utf8");
  if (raw.length === 0) {
    throw new Error(`chat-input-zero-diff: baseline fixture read empty (${name})`);
  }
  return raw.replace(/\r\n/g, "\n");
}

const S1 = loadBaselineFixture("chat-input-baseline-props.txt");
const S2 = loadBaselineFixture("chat-input-baseline-body.txt");

const S3 = `export interface ChatInputDraftAdapter {
  /** Storage key scoping the in-progress draft (session / project / mode). */
  readonly draftKey: string;
  /** React \`key\` for the editor instance (forces remount on identity swap). */
  readonly editorKey: string;
  /** Current draft text for \`draftKey\`. */
  readonly draft: string;
  /** Current draft attachment rows for \`draftKey\`. */
  readonly attachments: Attachment[];
  /** Persist the draft text for a key. */
  setDraft(key: string, content: string): void;
  /** Replace the draft attachments for a key. */
  setAttachments(key: string, attachments: Attachment[]): void;
  /** Append (or upsert by id) one attachment row for a key. */
  addAttachment(key: string, attachment: Attachment): void;
  /** Drop both text and attachments persisted for a key. */
  clearDraft(key: string): void;
}`;

const S4 = `interface ChatInputCoreProps extends ChatInputProps {
  /** Draft persistence backend (CR-2026-012 DD-9). Embedded project
   *  surfaces (Team Agent pane, Private Ask pane) pass their own adapter. */
  draftAdapter: ChatInputDraftAdapter;
}`;

/** Symbol anchors — each must appear EXACTLY once in chat-input.tsx. */
const ANCHORS = [
  "interface ChatInputProps {",
  "export function ChatInput({",
  "export interface ChatInputDraftAdapter {",
  "interface ChatInputCoreProps extends ChatInputProps {",
  "export function ChatInputCore({",
] as const;

function loadChatInputSource(): string {
  // Resolved relative to this file (import.meta.url) — never a hardcoded
  // absolute path, so the checker keeps working across worktrees/checkouts.
  const here = dirname(fileURLToPath(import.meta.url));
  const sourcePath = join(here, "chat-input.tsx");
  const raw = readFileSync(sourcePath, "utf8");
  // Hard failure on empty read: a silent empty buffer would compare against
  // nothing and could green a broken checkout (AGENTS.md #1).
  if (raw.length === 0) {
    throw new Error(`chat-input-zero-diff: chat-input.tsx read empty (${sourcePath})`);
  }
  // Normalize the checkout's CRLF to LF before comparing (Windows autocrlf).
  return raw.replace(/\r\n/g, "\n");
}

describe("CR-2026-062 chat-input zero-diff (cmd-09)", () => {
  it("S1: ChatInputProps interface is verbatim from baseline 117fc6be", () => {
    expect(
      loadChatInputSource().includes(S1),
      "ChatInputProps region drifted from the baseline — zero-diff violation",
    ).toBe(true);
  });

  it("S2: ChatInput (global) function body is verbatim from baseline 117fc6be", () => {
    expect(
      loadChatInputSource().includes(S2),
      "ChatInput function body drifted from the baseline — zero-diff violation",
    ).toBe(true);
  });

  it("S3: ChatInputDraftAdapter interface is verbatim from baseline 117fc6be", () => {
    expect(
      loadChatInputSource().includes(S3),
      "ChatInputDraftAdapter region drifted from the baseline — zero-diff violation",
    ).toBe(true);
  });

  it("S4: ChatInputCoreProps interface is verbatim from baseline 117fc6be", () => {
    expect(
      loadChatInputSource().includes(S4),
      "ChatInputCoreProps region drifted from the baseline — zero-diff violation",
    ).toBe(true);
  });

  it("five symbol anchors each appear exactly once", () => {
    const src = loadChatInputSource();
    for (const anchor of ANCHORS) {
      const count = src.split(anchor).length - 1;
      expect(count, `anchor "${anchor}" must appear exactly once`).toBe(1);
    }
  });
});
