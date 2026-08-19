"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Loader2, SendHorizontal } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@multica/core/api";
import {
  projectDiscussionOptions,
  projectGatesOptions,
  useSetProjectDiscussionCoordinator,
} from "@multica/core/projects";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { Attachment, TimelineEntry } from "@multica/core/types";
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
import { ReactionBar } from "@multica/ui/components/common/reaction-bar";
import { ActorAvatar } from "../../common/actor-avatar";
import { ContentEditor, type ContentEditorRef, useFileDropZone, FileDropOverlay, ReadonlyContent } from "../../editor";
import { AttachmentList } from "../../issues/components/comment-card";
import {
  PropertyPicker,
  PickerItem,
  PickerSection,
  PickerEmpty,
} from "../../issues/components/pickers/property-picker";
import { useIssueTimeline } from "../../issues/hooks/use-issue-timeline";
import { useT, useTimeAgo } from "../../i18n";

// ─── Container ───────────────────────────────────────────────────────────
//
// The Discussion tab of the project chat panel (CR-2026-009): pure human
// multi-member chat, no agent, no queue. Every message is a flat top-level
// comment on the project's hidden Discussion container issue (no reply
// threads — out of scope per the interaction design's §0.4 exclusion list)
// so the message stream is just the container's comment entries rendered in
// order, with a lean composer that omits the slash-command palette and the
// trigger-preview chip (CommentInput/ReplyInput both carry those, but a
// Discussion comment can never enqueue an agent — the CR-2026-009 red line
// lives server-side in computeCommentAgentTriggers — so a chip claiming
// "this will notify Agent X" would be actively wrong here, not just unused).
//
// CR-2026-012 adds two capabilities on top, both opt-in:
//  1. Discussion Coordinator binding (owner/admin): once set, @-mentioning
//     the coordinator in this stream activates it (controlled opening of the
//     red line — server-side filter in computeCommentAgentTriggers).
//  2. Multi-select + merge-forward: select N messages and forward them to
//     the project Team Agent as ONE merged message (DD-7/DD-8).

export function DiscussionPane({
  projectId,
  canConfigure,
}: {
  projectId: string;
  /** Owner/admin — may bind the Discussion Coordinator. */
  canConfigure: boolean;
}) {
  const wsId = useWorkspaceId();
  const userId = useAuthStore((s) => s.user?.id);
  const { t } = useT("projects");
  const { data: discussion, isLoading } = useQuery(
    projectDiscussionOptions(wsId, projectId),
  );

  if (isLoading || discussion === undefined || discussion.issue_id === "") {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.loading)}</p>
      </div>
    );
  }

  return (
    <DiscussionBody
      issueId={discussion.issue_id}
      projectId={projectId}
      wsId={wsId}
      userId={userId}
      canConfigure={canConfigure}
      coordinatorAgentId={discussion.coordinator_agent_id}
    />
  );
}

function DiscussionBody({
  issueId,
  projectId,
  wsId,
  userId,
  canConfigure,
  coordinatorAgentId,
}: {
  issueId: string;
  projectId: string;
  wsId: string;
  userId?: string;
  canConfigure: boolean;
  coordinatorAgentId: string;
}) {
  const { t } = useT("projects");
  // WS `comment:created` already writes new comments straight into this
  // cache (use-issue-timeline.ts), so no separate subscription is needed.
  const { timeline, toggleReaction, submitComment } = useIssueTimeline(issueId, userId);
  const comments = useMemo(
    () => timeline.filter((e) => e.type === "comment"),
    [timeline],
  );

  // CR-2026-012 §5.1: the multi-select state is single-component state by
  // design — no zustand, no persistence; leaving the tab drops it, which is
  // exactly the intended zero-side-effect exit.
  const [selectMode, setSelectMode] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [previewOpen, setPreviewOpen] = useState(false);

  const enterSelectMode = () => {
    setSelectMode(true);
    setSelected(new Set());
  };
  const exitSelectMode = () => {
    setSelectMode(false);
    setSelected(new Set());
    setPreviewOpen(false);
  };
  const toggleSelect = useCallback((commentId: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(commentId)) next.delete(commentId);
      else next.add(commentId);
      return next;
    });
  }, []);

  const selectedComments = useMemo(
    () => comments.filter((c) => selected.has(c.id)),
    [comments, selected],
  );

  const handleForwarded = () => {
    exitSelectMode();
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex shrink-0 items-center justify-end gap-2 border-b px-4 py-1.5">
        {coordinatorAgentId ? <CoordinatorBadge agentId={coordinatorAgentId} /> : null}
        {canConfigure && (
          <CoordinatorPicker wsId={wsId} projectId={projectId} />
        )}
        {!selectMode && comments.length > 0 && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            data-testid="discussion-select-entry"
            onClick={enterSelectMode}
          >
            {t(($) => $.chat.merged_forward.select_entry)}
          </Button>
        )}
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto" data-tab-scroll-root>
        <DiscussionStreamView
          comments={comments}
          currentUserId={userId}
          onToggleReaction={toggleReaction}
          selectMode={selectMode}
          selected={selected}
          onToggleSelect={toggleSelect}
        />
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
        onOpenChange={(open) => {
          // Closing the preview keeps the multi-select state (DD-6): the
          // user may just be re-reading the stream before retrying.
          setPreviewOpen(open);
        }}
        projectId={projectId}
        wsId={wsId}
        comments={selectedComments}
        onForwarded={handleForwarded}
      />
      <DiscussionComposer
        issueId={issueId}
        submitComment={async (content, attachmentIds) =>
          Boolean(await submitComment(content, attachmentIds))
        }
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

// Inline agent picker mirroring TeamAgentSetupPicker (project-chat-panel):
// selecting writes settings.discussion_coordinator_agent_id. Agent-only (no
// squads) — a Discussion has exactly one coordinator.
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

// ─── Merge-forward preview (DD-7/DD-8) ───────────────────────────────────
//
// The dialog renders the exact three-part structure the server persists:
// trigger message (earliest selected, quoted in full) + chronological
// history list + optional register-CR instruction. The persisted markdown is
// server-built (service buildMergedForwardContent); this preview is the
// user-facing confirmation copy, keyed off the same locale anchors.

function MergeForwardPreviewDialog({
  open,
  onOpenChange,
  projectId,
  wsId,
  comments,
  onForwarded,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  projectId: string;
  wsId: string;
  comments: TimelineEntry[];
  onForwarded: () => void;
}) {
  const { t } = useT("projects");
  const timeAgo = useTimeAgo();
  const { getActorName } = useActorName();
  const [registerCr, setRegisterCr] = useState(false);
  const [forwarding, setForwarding] = useState(false);

  // CR governance gates decide the register-CR default (REQ-SUG-002): no
  // in-flight gate → pre-check; any in-flight gate → leave unchecked.
  // Endpoint absent/erroring → default unchecked, never error the preview
  // (TSUG-003). Recomputed every time the preview opens.
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
    if (comments.length === 0 || forwarding) return;
    setForwarding(true);
    try {
      await api.mergeForwardDiscussion(
        projectId,
        comments.map((c) => c.id),
        registerCr,
      );
      toast.success(t(($) => $.chat.merged_forward.success));
      onForwarded();
    } catch (e) {
      // DD-6: 429/403 surface a structured notice and KEEP both the
      // multi-select state and the preview — nothing is cleared, the user
      // retries once the queue drains / the presenter releases.
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

  const trigger = comments[0];

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
                <span className="text-xs">
                  {getActorName(trigger.actor_type, trigger.actor_id)} · {timeAgo(trigger.created_at)}
                </span>
                <ReadonlyContent content={trigger.content ?? ""} />
              </blockquote>
            </section>
          )}
          <section>
            <h4 className="mb-1 text-xs font-medium text-muted-foreground">
              {t(($) => $.chat.merged_forward.chat_history)} ·{" "}
              {t(($) => $.chat.merged_forward.messages_in_conversation, {
                count: comments.length,
              })}
            </h4>
            <ul className="space-y-1" data-testid="merge-forward-history">
              {comments.map((c) => (
                <li key={c.id} className="text-xs text-muted-foreground">
                  <span className="font-medium text-foreground/80">
                    [{getActorName(c.actor_type, c.actor_id)} {timeAgo(c.created_at)}]
                  </span>{" "}
                  <span className="whitespace-pre-wrap">{(c.content ?? "").replace(/\s+/g, " ").trim()}</span>
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
            disabled={comments.length === 0 || forwarding}
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

// ─── Stream (presentational) ──────────────────────────────────────────────

function DiscussionStreamView({
  comments,
  currentUserId,
  onToggleReaction,
  selectMode,
  selected,
  onToggleSelect,
}: {
  comments: TimelineEntry[];
  currentUserId?: string;
  onToggleReaction: (commentId: string, emoji: string) => void;
  selectMode: boolean;
  selected: Set<string>;
  onToggleSelect: (commentId: string) => void;
}) {
  const { t } = useT("projects");

  if (comments.length === 0) {
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
      {comments.map((entry) => (
        <DiscussionMessage
          key={entry.id}
          entry={entry}
          currentUserId={currentUserId}
          onToggleReaction={onToggleReaction}
          selectMode={selectMode}
          checked={selected.has(entry.id)}
          onToggleSelect={onToggleSelect}
        />
      ))}
    </div>
  );
}

function DiscussionMessage({
  entry,
  currentUserId,
  onToggleReaction,
  selectMode,
  checked,
  onToggleSelect,
}: {
  entry: TimelineEntry;
  currentUserId?: string;
  onToggleReaction: (commentId: string, emoji: string) => void;
  selectMode: boolean;
  checked: boolean;
  onToggleSelect: (commentId: string) => void;
}) {
  const { getActorName } = useActorName();
  const timeAgo = useTimeAgo();
  const reactions = entry.reactions ?? [];

  return (
    <div className="py-1.5" data-testid="discussion-message">
      <div className="flex items-center gap-2.5 pb-1">
        {selectMode && (
          <Checkbox
            data-testid="discussion-select-checkbox"
            checked={checked}
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
      <AttachmentList attachments={entry.attachments} content={entry.content} className="mt-1.5 pl-[38px]" />
      <ReactionBar
        reactions={reactions}
        currentUserId={currentUserId}
        onToggle={(emoji) => onToggleReaction(entry.id, emoji)}
        getActorName={getActorName}
        className="mt-1 pl-[38px]"
      />
    </div>
  );
}

// ─── Composer ─────────────────────────────────────────────────────────────
//
// Deliberately not CommentInput/ReplyInput: both enable the slash-command
// palette and the @mention trigger-preview chip, neither of which applies to
// a surface that never drives an agent. Draft persistence reuses the exact
// same CommentDraftKey convention CommentInput uses (`new:${issueId}`) so it
// is naturally namespaced per project (each project has its own Discussion
// container issue) without a separate store.

function DiscussionComposer({
  issueId,
  submitComment,
}: {
  issueId: string;
  submitComment: (content: string, attachmentIds?: string[]) => Promise<boolean>;
}) {
  const { t } = useT("projects");
  const editorRef = useRef<ContentEditorRef>(null);
  const draftKey = `new:${issueId}` as const;
  const initialDraft = useCommentDraftStore.getState().getDraft(draftKey);
  const [isEmpty, setIsEmpty] = useState(() => !initialDraft?.trim());
  const [submitting, setSubmitting] = useState(false);
  const [pendingAttachments, setPendingAttachments] = useState<Attachment[]>([]);
  const { uploadWithToast } = useFileUpload(api);
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
    const result = await uploadWithToast(file, { issueId });
    if (result) setPendingAttachments((prev) => [...prev, result]);
    return result;
  }, [uploadWithToast, issueId]);

  const handleSubmit = async () => {
    const content = editorRef.current?.getMarkdown()?.replace(/(\n\s*)+$/, "").trim();
    if (!content || submitting) return;
    const activeIds = pendingAttachments
      .filter((a) => contentReferencesAttachment(content, a))
      .map((a) => a.id);
    setSubmitting(true);
    try {
      const ok = await submitComment(content, activeIds.length > 0 ? activeIds : undefined);
      if (ok) {
        editorRef.current?.clearContent();
        setIsEmpty(true);
        setPendingAttachments([]);
        clearDraft(draftKey);
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
            onSubmit={handleSubmit}
            onUploadFile={handleUpload}
            debounceMs={100}
            currentIssueId={issueId}
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
