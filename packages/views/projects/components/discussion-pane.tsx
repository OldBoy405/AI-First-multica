"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Loader2, SendHorizontal } from "lucide-react";
import { projectDiscussionOptions } from "@multica/core/projects";
import { useWorkspaceId } from "@multica/core/hooks";
import { useAuthStore } from "@multica/core/auth";
import { useActorName } from "@multica/core/workspace/hooks";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { api } from "@multica/core/api";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { Attachment, TimelineEntry } from "@multica/core/types";
import { contentReferencesAttachment } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { FileUploadButton } from "@multica/ui/components/common/file-upload-button";
import { ReactionBar } from "@multica/ui/components/common/reaction-bar";
import { ActorAvatar } from "../../common/actor-avatar";
import { ContentEditor, type ContentEditorRef, useFileDropZone, FileDropOverlay, ReadonlyContent } from "../../editor";
import { AttachmentList } from "../../issues/components/comment-card";
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

export function DiscussionPane({ projectId }: { projectId: string }) {
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
    <div className="flex min-h-0 flex-1 flex-col">
      <DiscussionThread issueId={discussion.issue_id} userId={userId} />
      <DiscussionComposer issueId={discussion.issue_id} userId={userId} />
    </div>
  );
}

function DiscussionThread({ issueId, userId }: { issueId: string; userId?: string }) {
  // WS `comment:created` already writes new comments straight into this
  // cache (use-issue-timeline.ts), so no separate subscription is needed.
  const { timeline, toggleReaction } = useIssueTimeline(issueId, userId);
  const comments = useMemo(
    () => timeline.filter((e) => e.type === "comment"),
    [timeline],
  );

  return (
    <div className="flex-1 min-h-0 overflow-y-auto" data-tab-scroll-root>
      <DiscussionStreamView
        comments={comments}
        currentUserId={userId}
        onToggleReaction={toggleReaction}
      />
    </div>
  );
}

// ─── Stream (presentational) ──────────────────────────────────────────────

function DiscussionStreamView({
  comments,
  currentUserId,
  onToggleReaction,
}: {
  comments: TimelineEntry[];
  currentUserId?: string;
  onToggleReaction: (commentId: string, emoji: string) => void;
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
        />
      ))}
    </div>
  );
}

function DiscussionMessage({
  entry,
  currentUserId,
  onToggleReaction,
}: {
  entry: TimelineEntry;
  currentUserId?: string;
  onToggleReaction: (commentId: string, emoji: string) => void;
}) {
  const { getActorName } = useActorName();
  const timeAgo = useTimeAgo();
  const reactions = entry.reactions ?? [];

  return (
    <div className="py-1.5" data-testid="discussion-message">
      <div className="flex items-center gap-2.5 pb-1">
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

function DiscussionComposer({ issueId, userId }: { issueId: string; userId?: string }) {
  const { t } = useT("projects");
  const { submitComment } = useIssueTimeline(issueId, userId);
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
