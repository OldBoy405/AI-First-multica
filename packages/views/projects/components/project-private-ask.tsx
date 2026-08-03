"use client";

import { useCallback, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import {
  chatKeys,
  chatMessagesOptions,
  pendingChatTaskOptions,
} from "@multica/core/chat/queries";
import {
  projectChatDraftKey,
  projectChatOptions,
  projectPrivateChatOptions,
  useProjectChatStore,
} from "@multica/core/projects";
import { useAgentPresenceDetail } from "@multica/core/agents";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { agentListOptions } from "@multica/core/workspace/queries";
import type { Attachment } from "@multica/core/types";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@multica/ui/components/ui/tooltip";
import { ChatMessageList } from "../../chat/components/chat-message-list";
import {
  ChatInputCore,
  type ChatInputDraftAdapter,
} from "../../chat/components/chat-input";
import { ModelPicker } from "../../agents/components/inspector/model-picker";
import { useT } from "../../i18n";

// ─── Private Ask pane (CR-2026-008 TASK-04) ───────────────────────────────
//
// The project chat window's second tab: a per-member private Q&A sandbox
// bound to the project's Team Agent. Composes the store-free chat pieces
// (ChatMessageList renders messages, live tool timeline and the status pill
// from the pendingTask snapshot) around a session the pane resolves itself
// via get-or-create — deliberately NOT use-chat-controller / useChatStore,
// whose activeSessionId is a global singleton shared by the floating chat
// and the /chat page.
//
// composer rides ChatInputCore with a per-pane adapter (CR-2026-012 DD-9/
// FR-8): drafts + attachments live in the project chat store's private_ask
// slot, @-mentions stay member-only, and uploads bind to this pane's own
// chat session — never the global chat store's namespace.

export function ProjectPrivateAsk({
  projectId,
  wsId,
}: {
  projectId: string;
  wsId: string;
}) {
  const { t } = useT("projects");

  // Gate on the panel's chat context: only ask for (and lazily create) the
  // private session once a Team Agent is bound. Unconfigured projects show
  // a hint instead of surfacing a 409 from the get-or-create. isLoading on
  // THIS query is checked before reading `configured` — same order
  // TeamAgentPane uses for its identical projectChatOptions query — so a
  // fresh mount doesn't flash "needs Team Agent" before the config loads.
  const { data: chat, isLoading: chatLoading } = useQuery(projectChatOptions(wsId, projectId));

  const { data: session, isLoading: sessionLoading, isError } = useQuery({
    ...projectPrivateChatOptions(wsId, projectId),
    enabled: chat !== undefined && chat.team_agent_id !== "",
  });

  if (chatLoading || sessionLoading) {
    return (
      <Centered>
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.loading)}</p>
      </Centered>
    );
  }
  if (chat === undefined || chat.team_agent_id === "") {
    return (
      <Centered>
        <p
          data-testid="private-ask-unconfigured"
          className="text-xs text-muted-foreground"
        >
          {t(($) => $.chat.private.needs_team_agent)}
        </p>
      </Centered>
    );
  }
  if (isError || !session) {
    return (
      <Centered>
        <p className="text-xs text-muted-foreground">
          {t(($) => $.chat.private.load_failed)}
        </p>
      </Centered>
    );
  }

  return (
    <PrivateAskSession
      projectId={projectId}
      wsId={wsId}
      sessionId={session.id}
      agentId={session.agent_id}
    />
  );
}

function Centered({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
      {children}
    </div>
  );
}

function PrivateAskSession({
  projectId,
  wsId,
  sessionId,
  agentId,
}: {
  projectId: string;
  wsId: string;
  sessionId: string;
  agentId: string;
}) {
  const { t } = useT("projects");
  const { data: messages = [] } = useQuery(chatMessagesOptions(sessionId));
  const { data: pendingTask } = useQuery(pendingChatTaskOptions(sessionId));
  // CLAUDE.md pending-message pattern (mirrors TeamAgentComposer's
  // pendingMessage): render the just-sent text immediately with a visible
  // pending state, not silent optimism. Lifted here (rather than local to
  // the composer) so the empty-state greeting below also treats "I just
  // sent something" as non-empty. The real message lands via WS invalidate
  // + refetch of chatMessagesOptions; this local bubble only covers the gap.
  const [pendingMessage, setPendingMessage] = useState<string | null>(null);

  const presence = useAgentPresenceDetail(wsId, agentId);
  const availability = presence === "loading" ? undefined : presence.availability;

  return (
    <div
      className="flex h-full min-h-0 w-full flex-col"
      data-testid="private-ask-session"
    >
      <div className="min-h-0 flex-1">
        {messages.length === 0 && !pendingTask?.task_id && !pendingMessage ? (
          <Centered>
            <p className="text-sm font-medium">
              {t(($) => $.chat.greetings.private_ask)}
            </p>
            <p className="text-xs text-muted-foreground">
              {t(($) => $.chat.private.empty_hint)}
            </p>
          </Centered>
        ) : (
          <ChatMessageList
            messages={messages}
            pendingTask={pendingTask}
            availability={availability}
          />
        )}
      </div>
      <PrivateAskComposer
        projectId={projectId}
        wsId={wsId}
        sessionId={sessionId}
        agentId={agentId}
        pendingTaskId={pendingTask?.task_id ?? null}
        pendingMessage={pendingMessage}
        setPendingMessage={setPendingMessage}
      />
    </div>
  );
}

const EMPTY_DRAFT_ATTACHMENTS: Attachment[] = [];

// CR-2026-012 DD-9/DD-10: bridges ChatInputCore onto the project chat store's
// `${projectId}:private_ask` slot. Same shape as the Team Agent pane's
// adapter; the two panes only differ in the mode constant (SDD §5.3 — no
// shared wrapper, negative abstraction value).
function usePrivateAskDraftAdapter(projectId: string): ChatInputDraftAdapter {
  const draftKey = projectChatDraftKey(projectId, "private_ask");
  const draft = useProjectChatStore((s) => s.drafts[draftKey] ?? "");
  const attachments = useProjectChatStore(
    (s) => s.draftAttachments[draftKey] ?? EMPTY_DRAFT_ATTACHMENTS,
  );
  const setDraft = useProjectChatStore((s) => s.setDraft);
  const setDraftAttachments = useProjectChatStore((s) => s.setDraftAttachments);
  const addDraftAttachment = useProjectChatStore((s) => s.addDraftAttachment);
  return useMemo(
    () => ({
      draftKey,
      editorKey: "private_ask",
      draft,
      attachments,
      setDraft: (_key, content) => setDraft(projectId, "private_ask", content),
      setAttachments: (_key, atts) =>
        setDraftAttachments(projectId, "private_ask", atts),
      addAttachment: (_key, att) =>
        addDraftAttachment(projectId, "private_ask", att),
      clearDraft: (_key) => setDraft(projectId, "private_ask", ""),
    }),
    [projectId, draftKey, draft, attachments, setDraft, setDraftAttachments, addDraftAttachment],
  );
}

function PrivateAskComposer({
  projectId,
  wsId,
  sessionId,
  agentId,
  pendingTaskId,
  pendingMessage,
  setPendingMessage,
}: {
  projectId: string;
  wsId: string;
  sessionId: string;
  agentId: string;
  pendingTaskId: string | null;
  pendingMessage: string | null;
  setPendingMessage: (message: string | null) => void;
}) {
  const { t } = useT("projects");
  const qc = useQueryClient();
  const draftAdapter = usePrivateAskDraftAdapter(projectId);
  const running = pendingTaskId != null && pendingTaskId !== "";
  const { uploadWithToast } = useFileUpload(api, (err) => toast.error(err.message));

  const handleComposerUpload = useCallback(
    (file: File) => uploadWithToast(file, { chatSessionId: sessionId }),
    [uploadWithToast, sessionId],
  );

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: chatKeys.messages(sessionId) });
    void qc.invalidateQueries({ queryKey: chatKeys.pendingTask(sessionId) });
  };

  const handleSend = async (
    content: string,
    attachmentIds: string[] | undefined,
    commitInput: (options?: { extraDraftKeys?: string[]; clearEditor?: boolean }) => void,
  ): Promise<boolean> => {
    const trimmed = content.trim();
    if (!trimmed || running) return false;
    setPendingMessage(trimmed);
    try {
      await api.sendChatMessage(sessionId, trimmed, attachmentIds);
      commitInput(); // success → clear draft + attachment slot via adapter
      refresh();
      return true;
    } catch {
      // Draft is preserved — the user can just hit send again.
      toast.error(t(($) => $.chat.stream.send_failed));
      return false;
    } finally {
      setPendingMessage(null);
    }
  };

  // FR-9: the sender may stop their own generation; generated content stays
  // (cancelTaskById keeps the partial transcript, same semantics as /chat).
  const handleStop = async () => {
    if (!pendingTaskId) return;
    try {
      await api.cancelTaskById(pendingTaskId);
      refresh();
    } catch {
      toast.error(t(($) => $.chat.private.stop_failed));
    }
  };

  return (
    <div className="shrink-0 border-t px-4 py-3">
      {pendingMessage && (
        <div data-testid="private-ask-pending-message" className="mb-2 flex justify-end">
          <div className="flex max-w-[80%] items-center gap-1.5 rounded-2xl bg-muted/60 px-3.5 py-2 text-sm text-muted-foreground">
            <Loader2 className="h-3 w-3 shrink-0 animate-spin" />
            <span className="break-words">{pendingMessage}</span>
          </div>
        </div>
      )}
      {/* Read-only model badge (SDD-SUG-003): the model follows the Team
          Agent's configuration; Private Ask deliberately offers no editing
          entry point (a personal pane must not mutate the shared agent). */}
      <div
        data-testid="private-ask-model-row"
        className="mb-1.5 flex items-center gap-1.5 text-xs text-muted-foreground"
      >
        <span className="shrink-0">{t(($) => $.chat.stream.model_label)}</span>
        <Tooltip>
          <TooltipTrigger
            render={
              <span data-testid="private-ask-model-readonly">
                <PrivateAskModelBadge wsId={wsId} agentId={agentId} />
              </span>
            }
          />
          <TooltipContent side="top">
            {t(($) => $.chat.private.model_follows_team_agent)}
          </TooltipContent>
        </Tooltip>
      </div>
      {/* CR-2026-012 FR-8: rich composer (attachments + member-only @
          mentions). Stop button / model badge stay untouched above. */}
      <div data-testid="private-ask-composer">
        <ChatInputCore
          draftAdapter={draftAdapter}
          onSend={handleSend}
          onUploadFile={handleComposerUpload}
          onStop={handleStop}
          isRunning={running}
          disabled={running}
          mentionItemTypes={["member"]}
        />
      </div>
    </div>
  );
}

// Thin wrapper so the read-only ModelPicker resolves the agent's current
// model + runtime the same way the Team Agent composer does.
function PrivateAskModelBadge({
  wsId,
  agentId,
}: {
  wsId: string;
  agentId: string;
}) {
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const presence = useAgentPresenceDetail(wsId, agentId);
  const agent = agents.find((a) => a.id === agentId);
  if (!agent) return null;
  const runtimeOnline = presence !== "loading" && presence.availability === "online";
  return (
    <ModelPicker
      runtimeId={agent.runtime_id}
      runtimeOnline={runtimeOnline}
      value={agent.model ?? ""}
      canEdit={false}
      onChange={() => {}}
    />
  );
}
