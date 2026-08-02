"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, SendHorizontal, Square } from "lucide-react";
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
import { agentListOptions } from "@multica/core/workspace/queries";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@multica/ui/components/ui/tooltip";
import { Button } from "@multica/ui/components/ui/button";
import { ChatMessageList } from "../../chat/components/chat-message-list";
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
// ponytail: composer is the same plain textarea as the Team Agent pane —
// ChatInput reads useChatStore internally (chat-input.tsx draft keys), so
// reusing it here would leak this pane's drafts into the global chat's
// draft namespace. Attachments / @mentions ride on a later ChatInput
// decoupling; add them when that lands.

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
  // a hint instead of surfacing a 409 from the get-or-create.
  const { data: chat } = useQuery(projectChatOptions(wsId, projectId));
  const configured = chat !== undefined && chat.team_agent_id !== "";

  const { data: session, isLoading, isError } = useQuery({
    ...projectPrivateChatOptions(wsId, projectId),
    enabled: configured,
  });

  if (!configured) {
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
  if (isLoading) {
    return (
      <Centered>
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.loading)}</p>
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

  const presence = useAgentPresenceDetail(wsId, agentId);
  const availability = presence === "loading" ? undefined : presence.availability;

  return (
    <div
      className="flex h-full min-h-0 w-full flex-col"
      data-testid="private-ask-session"
    >
      <div className="min-h-0 flex-1">
        {messages.length === 0 && !pendingTask?.task_id ? (
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
      />
    </div>
  );
}

function PrivateAskComposer({
  projectId,
  wsId,
  sessionId,
  agentId,
  pendingTaskId,
}: {
  projectId: string;
  wsId: string;
  sessionId: string;
  agentId: string;
  pendingTaskId: string | null;
}) {
  const { t } = useT("projects");
  const qc = useQueryClient();
  const draftKey = projectChatDraftKey(projectId, "private_ask");
  const draft = useProjectChatStore((s) => s.drafts[draftKey] ?? "");
  const setDraft = useProjectChatStore((s) => s.setDraft);
  const [sending, setSending] = useState(false);
  const running = pendingTaskId != null && pendingTaskId !== "";

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: chatKeys.messages(sessionId) });
    void qc.invalidateQueries({ queryKey: chatKeys.pendingTask(sessionId) });
  };

  const handleSend = async () => {
    const content = draft.trim();
    if (!content || sending || running) return;
    setSending(true);
    try {
      await api.sendChatMessage(sessionId, content);
      setDraft(projectId, "private_ask", "");
      refresh();
    } catch {
      // Draft is preserved — the user can just hit send again.
      toast.error(t(($) => $.chat.stream.send_failed));
    } finally {
      setSending(false);
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
      <div className="relative flex items-end gap-2 rounded-lg border bg-card px-3 py-2 transition-colors focus-within:border-brand">
        <textarea
          data-testid="private-ask-composer-input"
          value={draft}
          rows={1}
          placeholder={t(($) => $.chat.private.composer_placeholder)}
          onChange={(e) => setDraft(projectId, "private_ask", e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              void handleSend();
            }
          }}
          className="max-h-32 min-h-[24px] flex-1 resize-none bg-transparent text-sm outline-none placeholder:text-muted-foreground"
        />
        {running ? (
          <Button
            type="button"
            size="icon-sm"
            variant="outline"
            data-testid="private-ask-stop"
            onClick={() => void handleStop()}
            aria-label={t(($) => $.chat.private.stop)}
          >
            <Square className="h-3.5 w-3.5" />
          </Button>
        ) : (
          <Button
            type="button"
            size="icon-sm"
            data-testid="private-ask-send"
            disabled={sending || !draft.trim()}
            onClick={() => void handleSend()}
            aria-label={t(($) => $.chat.stream.send)}
          >
            {sending ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <SendHorizontal className="h-4 w-4" />
            )}
          </Button>
        )}
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
