"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, RefreshCw, SendHorizontal } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@multica/core/api";
import {
  projectDiscussionOptions,
  projectGatesOptions,
  useSetProjectDiscussionCoordinator,
} from "@multica/core/projects";
import { chatKeys, chatMessagesPageOptions } from "@multica/core/chat/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
import { runtimeListOptions, runtimeModelsOptions } from "@multica/core/runtimes";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { Attachment, ChatMessage, TimelineEntry } from "@multica/core/types";
import { contentReferencesAttachment } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@multica/ui/components/ui/dialog";
import { FileUploadButton } from "@multica/ui/components/common/file-upload-button";
import { ActorAvatar } from "../../common/actor-avatar";
import { ContentEditor, type ContentEditorRef, useFileDropZone, FileDropOverlay, ReadonlyContent } from "../../editor";
import { AttachmentList } from "../../issues/components/comment-card";
import {
  PropertyPicker,
  PickerItem,
  PickerSection,
  PickerEmpty,
} from "../../issues/components/pickers/property-picker";
import { ModelPicker } from "../../agents/components/inspector/model-picker";
import { ThinkingPicker } from "../../agents/components/inspector/thinking-picker";
import { useIssueTimeline } from "../../issues/hooks/use-issue-timeline";
import { useT, useTimeAgo } from "../../i18n";

// ─── Container ───────────────────────────────────────────────────────────
//
// The Discussion tab of the project chat panel (CR-2026-009 → CR-2026-059):
// the pane's ONLY writable identity is the shared Discussion session
// (session_id). Messages/sends/attachments/config all flow through the
// shared-session API; the pre-CR container issue survives exclusively as a
// read-only replay stream behind legacy_issue_id — never sent to, never
// written. Hard degradation (AC-17/NFR-8): a response whose session_id is
// missing/empty/not-a-UUID degrades the WHOLE pane to read-only and offers a
// retry; no PATCH/send ever fires against an empty id.
//
// CR-2026-012 additions kept on top: the Discussion Coordinator binding and
// the merge-forward multi-select, now with two mutually exclusive arms —
// shared messages (message_ids + Idempotency-Key) and the legacy issue
// stream (comment_ids, header-less).

function isUUID(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

export function DiscussionPane({
  projectId,
  canConfigure,
}: {
  projectId: string;
  /** Owner/admin — may bind the Discussion Coordinator and edit config. */
  canConfigure: boolean;
}) {
  const wsId = useWorkspaceId();
  const userId = useAuthStore((s) => s.user?.id);
  const { t } = useT("projects");
  const { data: discussion, isLoading, refetch, isRefetching } = useQuery(
    projectDiscussionOptions(wsId, projectId),
  );

  if (isLoading || discussion === undefined) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.loading)}</p>
      </div>
    );
  }

  const sessionId = discussion.degraded || !isUUID(discussion.session_id) ? "" : discussion.session_id;
  if (sessionId === "") {
    // Hard degradation (AC-17): read-only, retry GET, never PATCH/send.
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
        <p className="text-sm font-medium">{t(($) => $.chat.discussion.unavailable_title)}</p>
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.discussion.unavailable_sub)}</p>
        <Button type="button" variant="outline" size="sm" data-testid="discussion-retry" onClick={() => void refetch()}>
          {isRefetching ? <Loader2 className="h-4 w-4 animate-spin" /> : <RefreshCw className="h-4 w-4" />}
          {t(($) => $.chat.discussion.retry)}
        </Button>
      </div>
    );
  }

  return (
    <DiscussionBody
      sessionId={sessionId}
      legacyIssueId={discussion.legacy_issue_id}
      projectId={projectId}
      wsId={wsId}
      userId={userId}
      canConfigure={canConfigure}
      coordinatorAgentId={discussion.coordinator_agent_id}
      model={discussion.model}
      thinkingLevel={discussion.thinking_level}
    />
  );
}

function DiscussionBody({
  sessionId,
  legacyIssueId,
  projectId,
  wsId,
  userId,
  canConfigure,
  coordinatorAgentId,
  model,
  thinkingLevel,
}: {
  sessionId: string;
  legacyIssueId: string | null;
  projectId: string;
  wsId: string;
  userId?: string;
  canConfigure: boolean;
  coordinatorAgentId: string;
  model: string;
  thinkingLevel: string;
}) {
  const { t } = useT("projects");
  const qc = useQueryClient();

  // Shared-session message stream (page object, invalidated by the realtime
  // layer on chat:message; shared events now reach every workspace member).
  const messagesQuery = useInfiniteQuery(chatMessagesPageOptions(sessionId));
  const messages = useMemo(
    () => (messagesQuery.data?.pages ?? []).flatMap((p) => p.messages),
    [messagesQuery.data],
  );

  // Legacy container replay (read-only) — the pre-CR issue stream.
  const { timeline } = useIssueTimeline(legacyIssueId ?? "", userId);
  const legacyComments = useMemo(
    () => (legacyIssueId ? timeline.filter((e) => e.type === "comment") : []),
    [timeline, legacyIssueId],
  );

  // Multi-select state (single-component by design — leaving the tab drops
  // it; the selection source is mutually exclusive by construction).
  const [selectMode, setSelectMode] = useState<"messages" | "comments" | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [previewOpen, setPreviewOpen] = useState(false);

  const enterSelectMode = (source: "messages" | "comments") => {
    setSelectMode(source);
    setSelected(new Set());
  };
  const exitSelectMode = () => {
    setSelectMode(null);
    setSelected(new Set());
    setPreviewOpen(false);
  };
  const toggleSelect = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const selectedMessages = useMemo(
    () => messages.filter((m) => selected.has(m.id)),
    [messages, selected],
  );
  const selectedComments = useMemo(
    () => legacyComments.filter((c) => selected.has(c.id)),
    [legacyComments, selected],
  );

  const refreshMessages = useCallback(() => {
    void qc.invalidateQueries({ queryKey: chatKeys.messagesPage(sessionId) });
    void qc.invalidateQueries({ queryKey: chatKeys.messages(sessionId) });
  }, [qc, sessionId]);

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center justify-end gap-2 border-b px-4 py-1.5">
        {coordinatorAgentId ? <CoordinatorBadge agentId={coordinatorAgentId} /> : null}
        {canConfigure && <CoordinatorPicker wsId={wsId} projectId={projectId} />}
        {canConfigure && (
          <DiscussionConfigControls
            sessionId={sessionId}
            wsId={wsId}
            coordinatorAgentId={coordinatorAgentId}
            model={model}
            thinkingLevel={thinkingLevel}
            onSaved={refreshMessages}
          />
        )}
        {!selectMode && messages.length > 0 && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            data-testid="discussion-select-entry"
            onClick={() => enterSelectMode("messages")}
          >
            {t(($) => $.chat.merged_forward.select_entry)}
          </Button>
        )}
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto" data-tab-scroll-root>
        <DiscussionMessageStream
          messages={messages}
          selectMode={selectMode === "messages"}
          selected={selected}
          onToggleSelect={toggleSelect}
        />
        {legacyIssueId ? (
          <LegacyDiscussionStream
            comments={legacyComments}
            selectMode={selectMode === "comments"}
            selected={selected}
            onToggleSelect={toggleSelect}
            onEnterSelect={() => enterSelectMode("comments")}
          />
        ) : null}
      </div>
      {selectMode && (
        <div
          data-testid="discussion-batch-bar"
          className="flex shrink-0 items-center gap-2 border-t bg-muted/40 px-4 py-2"
        >
          <span className="text-xs text-muted-foreground" data-testid="discussion-selected-count">
            {t(($) => $.chat.merged_forward.selected_count, { count: selected.size })}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <Button type="button" variant="outline" size="sm" onClick={exitSelectMode}>
              {t(($) => $.chat.merged_forward.cancel)}
            </Button>
            <Button
              type="button"
              size="sm"
              data-testid="discussion-merge-cta"
              disabled={selected.size === 0}
              onClick={() => setPreviewOpen(true)}
            >
              {t(($) => $.chat.merged_forward.merge_cta)}
            </Button>
          </div>
        </div>
      )}
      <MergeForwardPreviewDialog
        open={previewOpen}
        onOpenChange={(open) => setPreviewOpen(open)}
        projectId={projectId}
        wsId={wsId}
        selection={selectMode === "messages" ? { messages: selectedMessages } : { comments: selectedComments }}
        onForwarded={exitSelectMode}
      />
      <DiscussionComposer
        sessionId={sessionId}
        onSent={refreshMessages}
      />
    </div>
  );
}

// ─── Coordinator binding (owner/admin) ───────────────────────────────────

function CoordinatorBadge({ agentId }: { agentId: string }) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  return (
    <span
      data-testid="discussion-coordinator-badge"
      className="flex items-center gap-1.5 text-xs text-muted-foreground"
    >
      <ActorAvatar actorType="agent" actorId={agentId} size="sm" />
      <span className="truncate">
        {t(($) => $.chat.dc.coordinator_bound, {
          name: getActorName("agent", agentId),
        })}
      </span>
    </span>
  );
}

function CoordinatorPicker({ wsId, projectId }: { wsId: string; projectId: string }) {
  const { t } = useT("projects");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const setCoordinator = useSetProjectDiscussionCoordinator(wsId, projectId);

  const activeAgents = useMemo(() => agents.filter((a) => !a.archived_at), [agents]);
  const query = filter.trim().toLowerCase();
  const filteredAgents = activeAgents.filter((a) =>
    !query || a.name.toLowerCase().includes(query),
  );

  const handlePick = (agentId: string) => {
    setOpen(false);
    setCoordinator.mutate(agentId, {
      onError: () => toast.error(t(($) => $.chat.dc.coordinator_setup_failed)),
    });
  };

  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      searchable
      onSearchChange={setFilter}
      trigger={
        <Button
          variant="outline"
          size="sm"
          data-testid="discussion-coordinator-picker"
          disabled={setCoordinator.isPending}
        >
          {t(($) => $.chat.dc.coordinator_pick_cta)}
        </Button>
      }
    >
      {filteredAgents.length === 0 ? (
        <PickerEmpty />
      ) : (
        <PickerSection label={t(($) => $.chat.dc.coordinator_label)}>
          {filteredAgents.map((a) => (
            <PickerItem key={a.id} selected={false} onClick={() => handlePick(a.id)}>
              <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
              <span className="truncate">{a.name}</span>
            </PickerItem>
          ))}
        </PickerSection>
      )}
    </PropertyPicker>
  );
}

// ─── Session config controls (CR-2026-059 FR-19): PATCH the shared session's
// own config — never UpdateAgent. Choices come from the Coordinator's runtime
// catalog when one is bound; otherwise the current values render read-only
// (the L2 union has no client-side picker authority). ──────────────────────

function DiscussionConfigControls({
  sessionId,
  wsId,
  coordinatorAgentId,
  model,
  thinkingLevel,
  onSaved,
}: {
  sessionId: string;
  wsId: string;
  coordinatorAgentId: string;
  model: string;
  thinkingLevel: string;
  onSaved: () => void;
}) {
  const { t } = useT("projects");
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const agent = agents.find((a) => a.id === coordinatorAgentId) ?? null;
  const { data: runtimes = [] } = useQuery({
    ...runtimeListOptions(wsId),
    enabled: !!agent,
  });
  const runtime = agent?.runtime_id ? runtimes.find((r) => r.id === agent.runtime_id) ?? null : null;
  const runtimeOnline = runtime?.status === "online";
  const modelsQuery = useQuery(runtimeModelsOptions(runtimeOnline ? agent?.runtime_id ?? null : null));
  const thinkingLevels = useMemo(() => {
    const entry = (modelsQuery.data?.models ?? []).find((m) => m.id === model);
    return entry?.thinking?.supported_levels ?? [];
  }, [modelsQuery.data, model]);

  const persist = async (patch: { model?: string | null; thinking_level?: string | null }) => {
    try {
      await api.patchChatSessionConfig(sessionId, patch);
      onSaved();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        toast.error(t(($) => $.chat.discussion.config_unavailable));
        return;
      }
      toast.error(t(($) => $.chat.stream.model_update_failed));
    }
  };

  if (!agent) {
    return (
      <span data-testid="discussion-config-row" className="text-xs text-muted-foreground">
        {model ? `${t(($) => $.chat.stream.model_label)} ${model}` : t(($) => $.chat.stream.model_label)}
      </span>
    );
  }

  return (
    <div
      data-testid="discussion-config-row"
      className="flex items-center gap-1.5 text-xs text-muted-foreground"
    >
      <span className="shrink-0">{t(($) => $.chat.stream.model_label)}</span>
      <ModelPicker
        runtimeId={agent.runtime_id}
        runtimeOnline={!!runtimeOnline}
        value={model}
        canEdit
        onChange={(next) => void persist({ model: next })}
      />
      {thinkingLevels.length > 0 && (
        <span data-testid="discussion-thinking-picker" className="flex items-center gap-1">
          <span className="shrink-0">{t(($) => $.chat.stream.thinking_label)}</span>
          <ThinkingPicker
            value={thinkingLevel}
            levels={thinkingLevels}
            canEdit
            onChange={(next) => void persist({ thinking_level: next })}
          />
        </span>
      )}
    </div>
  );
}

// ─── Merge-forward preview (DD-7/DD-8 + CR-2026-059 §3.5 arms) ────────────

function MergeForwardPreviewDialog({
  open,
  onOpenChange,
  projectId,
  wsId,
  selection,
  onForwarded,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  projectId: string;
  wsId: string;
  selection: { messages: ChatMessage[] } | { comments: TimelineEntry[] };
  onForwarded: () => void;
}) {
  const { t } = useT("projects");
  const timeAgo = useTimeAgo();
  const [registerCr, setRegisterCr] = useState(false);
  const [forwarding, setForwarding] = useState(false);
  // One Idempotency-Key per preview for the message_ids arm: retries of the
  // SAME confirmation reuse it (replay semantics), a new preview gets a new
  // key (B-DP-07).
  const idempotencyKeyRef = useRef<string>("");

  const isMessagesArm = "messages" in selection;
  const items = useMemo(() => {
    if ("messages" in selection) {
      return [...selection.messages].sort(
        (a, b) => new Date(a.created_at).getTime() - new Date(b.created_at).getTime(),
      ).map((m) => ({
        id: m.id,
        author: m.author_type ? `${m.author_type}:${m.author_id ?? ""}` : null,
        content: m.content,
        created_at: m.created_at,
      }));
    }
    return [...selection.comments].map((c) => ({
      id: c.id,
      author: `${c.actor_type}:${c.actor_id ?? ""}`,
      content: c.content ?? "",
      created_at: c.created_at,
    }));
  }, [selection]);

  useEffect(() => {
    if (open && isMessagesArm) {
      idempotencyKeyRef.current = crypto.randomUUID();
    }
  }, [open, isMessagesArm]);

  const { data: gates, isError: gatesError } = useQuery({
    ...projectGatesOptions(wsId, projectId),
    enabled: open,
  });
  useEffect(() => {
    if (!open) return;
    if (gatesError) {
      setRegisterCr(false);
      return;
    }
    if (gates !== undefined) setRegisterCr(gates.length === 0);
  }, [open, gates, gatesError]);

  const handleConfirm = async () => {
    if (items.length === 0 || forwarding) return;
    setForwarding(true);
    try {
      if ("messages" in selection) {
        await api.mergeForwardDiscussion(
          projectId,
          { messageIds: selection.messages.map((m) => m.id) },
          registerCr,
          idempotencyKeyRef.current,
        );
      } else {
        await api.mergeForwardDiscussion(
          projectId,
          { commentIds: selection.comments.map((c) => c.id) },
          registerCr,
        );
      }
      toast.success(t(($) => $.chat.merged_forward.success));
      onForwarded();
    } catch (e) {
      // DD-6: 429/403 surface a structured notice and KEEP both the
      // multi-select state and the preview.
      if (e instanceof ApiError) {
        if (e.status === 429) {
          const body = e.body as { queue_depth?: number; queue_limit?: number } | undefined;
          toast.error(
            t(($) => $.chat.merged_forward.queue_full_error, {
              depth: body?.queue_depth ?? 0,
              limit: body?.queue_limit ?? 0,
            }),
          );
          return;
        }
        if (e.status === 403) {
          toast.error(t(($) => $.chat.merged_forward.presenter_locked_error));
          return;
        }
      }
      toast.error(t(($) => $.chat.merged_forward.send_failed));
    } finally {
      setForwarding(false);
    }
  };

  const trigger = items[0];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.chat.merged_forward.preview_title)}</DialogTitle>
        </DialogHeader>
        <div className="max-h-72 space-y-3 overflow-y-auto text-sm" data-testid="merge-forward-preview">
          {trigger && (
            <section>
              <h4 className="mb-1 text-xs font-medium text-muted-foreground">
                {t(($) => $.chat.merged_forward.trigger_message)}
              </h4>
              <blockquote
                data-testid="merge-forward-trigger"
                className="border-l-2 pl-2 text-muted-foreground"
              >
                <span className="text-xs">{timeAgo(trigger.created_at)}</span>
                <ReadonlyContent content={trigger.content} />
              </blockquote>
            </section>
          )}
          <section>
            <h4 className="mb-1 text-xs font-medium text-muted-foreground">
              {t(($) => $.chat.merged_forward.chat_history)} ·{" "}
              {t(($) => $.chat.merged_forward.messages_in_conversation, { count: items.length })}
            </h4>
            <ul className="space-y-1" data-testid="merge-forward-history">
              {items.map((c) => (
                <li key={c.id} className="text-xs text-muted-foreground">
                  <span className="font-medium text-foreground/80">[{timeAgo(c.created_at)}]</span>{" "}
                  <span className="whitespace-pre-wrap">{c.content.replace(/\s+/g, " ").trim()}</span>
                </li>
              ))}
            </ul>
          </section>
        </div>
        <label className="flex items-center gap-2 text-sm">
          <Checkbox
            data-testid="merge-forward-register-cr"
            checked={registerCr}
            onCheckedChange={(v) => setRegisterCr(v === true)}
          />
          {t(($) => $.chat.merged_forward.register_cr_label)}
        </label>
        <DialogFooter>
          <Button type="button" variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            {t(($) => $.chat.merged_forward.cancel)}
          </Button>
          <Button
            type="button"
            size="sm"
            data-testid="merge-forward-confirm"
            disabled={items.length === 0 || forwarding}
            onClick={() => void handleConfirm()}
          >
            {forwarding ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              t(($) => $.chat.merged_forward.confirm)
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ─── Shared message stream (presentational) ────────────────────────────────

function DiscussionMessageStream({
  messages,
  selectMode,
  selected,
  onToggleSelect,
}: {
  messages: ChatMessage[];
  selectMode: boolean;
  selected: Set<string>;
  onToggleSelect: (messageId: string) => void;
}) {
  const { t } = useT("projects");

  if (messages.length === 0) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-2 text-center">
        <p className="text-sm font-medium">{t(($) => $.chat.greetings.discussion)}</p>
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.discussion.sub)}</p>
      </div>
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-1 px-4 py-3">
      <div
        data-testid="discussion-no-earlier"
        className="pb-2 text-center text-xs text-muted-foreground/70"
      >
        {t(($) => $.chat.stream.no_earlier)}
      </div>
      {messages.map((message) => (
        <SharedDiscussionMessage
          key={message.id}
          message={message}
          selectMode={selectMode}
          checked={selected.has(message.id)}
          onToggleSelect={onToggleSelect}
        />
      ))}
    </div>
  );
}

function SharedDiscussionMessage({
  message,
  selectMode,
  checked,
  onToggleSelect,
}: {
  message: ChatMessage;
  selectMode: boolean;
  checked: boolean;
  onToggleSelect: (messageId: string) => void;
}) {
  const { getActorName } = useActorName();
  const { t } = useT("projects");
  const timeAgo = useTimeAgo();

  // Author resolution (SDD §3.3 fallback table): member/agent via the actor
  // cache; NULL or degraded fields keep the baseline rendering (no author
  // label). Assistant bubbles keep the plain reply rendering.
  const authorType = message.author_type ?? null;
  const authorId = message.author_id ?? null;
  const showAuthor = message.role === "user" && authorType != null && authorId != null;
  const authorName = showAuthor
    ? getActorName(authorType as "member" | "agent", authorId as string)
    : null;

  return (
    <div className="py-1.5" data-testid="discussion-message">
      <div className="flex items-center gap-2.5 pb-1">
        {selectMode && (
          <Checkbox
            data-testid="discussion-select-checkbox"
            checked={checked}
            onCheckedChange={() => onToggleSelect(message.id)}
            aria-label={message.id}
          />
        )}
        {showAuthor && authorType && authorId ? (
          <ActorAvatar actorType={authorType as "member" | "agent"} actorId={authorId} size="md" enableHoverCard showStatusDot />
        ) : message.role === "user" ? (
          <span className="flex h-6 w-6 items-center justify-center rounded-full bg-muted text-xs">
            {t(($) => $.chat.discussion.unknown_member)}
          </span>
        ) : (
          <span className="flex h-6 w-6 items-center justify-center rounded-full bg-brand/10 text-xs" data-testid="discussion-assistant-marker">
            {t(($) => $.chat.discussion.assistant_label)}
          </span>
        )}
        {showAuthor && (
          <span className="text-sm font-medium" data-testid="discussion-author">
            {authorName}
          </span>
        )}
        <span className="text-xs text-muted-foreground">{timeAgo(message.created_at)}</span>
      </div>
      <div className="pl-[38px] text-sm leading-relaxed text-foreground/85">
        <ReadonlyContent content={message.content} attachments={message.attachments} />
      </div>
      <AttachmentList attachments={message.attachments} content={message.content} className="mt-1.5 pl-[38px]" />
    </div>
  );
}

// ─── Legacy issue stream (read-only replay, CR-2026-059 FR-16) ────────────

function LegacyDiscussionStream({
  comments,
  selectMode,
  selected,
  onToggleSelect,
  onEnterSelect,
}: {
  comments: TimelineEntry[];
  selectMode: boolean;
  selected: Set<string>;
  onToggleSelect: (commentId: string) => void;
  onEnterSelect: () => void;
}) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const timeAgo = useTimeAgo();

  if (comments.length === 0) return null;

  return (
    <section className="mx-auto w-full max-w-3xl border-t px-4 py-3" data-testid="discussion-legacy-stream">
      <div className="flex items-center justify-between pb-2">
        <h3 className="text-xs font-medium text-muted-foreground">
          {t(($) => $.chat.discussion.legacy_section_title)}
        </h3>
        {!selectMode && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            data-testid="discussion-legacy-select-entry"
            onClick={onEnterSelect}
          >
            {t(($) => $.chat.merged_forward.select_entry)}
          </Button>
        )}
      </div>
      {comments.map((entry) => (
        <div key={entry.id} className="py-1.5 opacity-80" data-testid="discussion-legacy-message">
          <div className="flex items-center gap-2.5 pb-1">
            {selectMode && (
              <Checkbox
                data-testid="discussion-select-checkbox"
                checked={selected.has(entry.id)}
                onCheckedChange={() => onToggleSelect(entry.id)}
                aria-label={entry.id}
              />
            )}
            <ActorAvatar actorType={entry.actor_type} actorId={entry.actor_id} size="md" enableHoverCard showStatusDot />
            <span className="text-sm font-medium">{getActorName(entry.actor_type, entry.actor_id)}</span>
            <span className="text-xs text-muted-foreground">{timeAgo(entry.created_at)}</span>
          </div>
          <div className="pl-[38px] text-sm leading-relaxed text-foreground/85">
            <ReadonlyContent content={entry.content ?? ""} attachments={entry.attachments} />
          </div>
        </div>
      ))}
    </section>
  );
}

// ─── Composer (shared-session send) ──────────────────────────────────────
//
// Draft persistence reuses the CommentDraftKey convention namespaced by the
// session id. Every send carries a fresh Idempotency-Key; on failure the
// draft survives for retry (FR-15: drafts stay unbound server-side).

function DiscussionComposer({
  sessionId,
  onSent,
}: {
  sessionId: string;
  onSent: () => void;
}) {
  const { t } = useT("projects");
  const editorRef = useRef<ContentEditorRef>(null);
  // Session ids and issue ids are disjoint UUID spaces, so the `new:`
  // CommentDraftKey namespace is safe to share (session-scoped draft).
  const draftKey = `new:${sessionId}` as const;
  const initialDraft = useCommentDraftStore.getState().getDraft(draftKey);
  const [isEmpty, setIsEmpty] = useState(() => !initialDraft?.trim());
  const [submitting, setSubmitting] = useState(false);
  const [pendingAttachments, setPendingAttachments] = useState<Attachment[]>([]);
  const { uploadWithToast } = useFileUpload(api, (err) => toast.error(err.message));
  const { isDragOver, dropZoneProps } = useFileDropZone({
    onDrop: (files) => files.forEach((f) => editorRef.current?.uploadFile(f)),
  });
  const setDraft = useCommentDraftStore((s) => s.setDraft);
  const clearDraft = useCommentDraftStore((s) => s.clearDraft);

  useEffect(() => {
    const flush = () => {
      const md = editorRef.current?.getMarkdown();
      if (md && md.trim().length > 0) setDraft(draftKey, md);
    };
    const onVis = () => { if (document.visibilityState === "hidden") flush(); };
    document.addEventListener("visibilitychange", onVis);
    window.addEventListener("pagehide", flush);
    return () => {
      document.removeEventListener("visibilitychange", onVis);
      window.removeEventListener("pagehide", flush);
    };
  }, [draftKey, setDraft]);

  const handleUpload = useCallback(async (file: File) => {
    // Drafts bind to the shared session — never an issue.
    const result = await uploadWithToast(file, { chatSessionId: sessionId });
    if (result) setPendingAttachments((prev) => [...prev, result]);
    return result;
  }, [uploadWithToast, sessionId]);

  const handleSubmit = async () => {
    const content = editorRef.current?.getMarkdown()?.replace(/(\n\s*)+$/, "").trim();
    if (!content || submitting) return;
    const activeIds = pendingAttachments
      .filter((a) => contentReferencesAttachment(content, a))
      .map((a) => a.id);
    setSubmitting(true);
    try {
      await api.sendChatMessage(sessionId, content, activeIds.length > 0 ? activeIds : undefined, {
        idempotencyKey: crypto.randomUUID(),
      });
      editorRef.current?.clearContent();
      setIsEmpty(true);
      setPendingAttachments([]);
      clearDraft(draftKey);
      onSent();
    } catch (e) {
      // 409 attachment_already_bound keeps the composer content + attachments
      // for retry (FR-15: the server left the drafts unbound).
      if (e instanceof ApiError && e.status === 409) {
        toast.error(t(($) => $.chat.discussion.send_conflict));
      } else {
        toast.error(t(($) => $.chat.discussion.send_failed));
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="shrink-0 border-t px-4 py-3">
      <div
        {...dropZoneProps}
        className={cn(
          "relative flex items-end gap-2 rounded-lg border bg-card px-3 py-2 transition-colors focus-within:border-brand",
          submitting && "pointer-events-none opacity-60",
        )}
        aria-busy={submitting || undefined}
      >
        <div className="min-h-[24px] max-h-32 flex-1 overflow-y-auto text-sm">
          <ContentEditor
            ref={editorRef}
            defaultValue={initialDraft}
            placeholder={t(($) => $.chat.discussion.composer_placeholder)}
            onUpdate={(md) => {
              setIsEmpty(!md.trim());
              if (md.trim().length > 0) setDraft(draftKey, md);
              else clearDraft(draftKey);
            }}
            onSubmit={() => void handleSubmit()}
            onUploadFile={handleUpload}
            debounceMs={100}
            attachments={pendingAttachments}
          />
        </div>
        <FileUploadButton size="sm" multiple onSelect={(file) => editorRef.current?.uploadFile(file)} />
        <Button
          type="button"
          size="icon-sm"
          data-testid="discussion-send"
          disabled={isEmpty || submitting}
          onClick={() => void handleSubmit()}
          aria-label={t(($) => $.chat.stream.send)}
        >
          {submitting ? <Loader2 className="h-4 w-4 animate-spin" /> : <SendHorizontal className="h-4 w-4" />}
        </Button>
        {isDragOver && <FileDropOverlay />}
      </div>
    </div>
  );
}
