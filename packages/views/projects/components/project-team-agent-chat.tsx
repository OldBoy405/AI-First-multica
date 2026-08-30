"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, CheckCircle2, Copy, Loader2, Mic, XCircle } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@multica/core/api";
import type { ProjectChat } from "@multica/core/api/schemas";
import { issueKeys } from "@multica/core/issues/queries";
import { taskMessagesOptions } from "@multica/core/chat/queries";
import {
  projectChatDraftKey,
  projectChatOptions,
  projectGatesOptions,
  projectPresenterOptions,
  projectQueueStatusOptions,
  useCancelProjectQueueTask,
  useProjectChatStore,
  useRequestPresenter,
  useSendProjectChatMessage,
} from "@multica/core/projects";
import type { ProjectGateCR } from "@multica/core/api/schemas";
import { useAuthStore } from "@multica/core/auth";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
import { runtimeListOptions, runtimeModelsOptions } from "@multica/core/runtimes";
import type { AgentTask, Attachment, TimelineEntry } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Switch } from "@multica/ui/components/ui/switch";
import { ActorAvatar } from "../../common/actor-avatar";
import { ReadonlyContent } from "../../editor";
import { buildTimeline } from "../../common/task-transcript";
import { TimelineView } from "../../chat/components/chat-message-list";
import {
  ChatInputCore,
  type ChatInputDraftAdapter,
} from "../../chat/components/chat-input";
import { ApprovalCard, CrGateCard } from "./cr-gate-card";
import { ModelPicker } from "../../agents/components/inspector/model-picker";
import { ThinkingPicker } from "../../agents/components/inspector/thinking-picker";
import { useIssueTimeline } from "../../issues/hooks/use-issue-timeline";
import { ProjectQueueBar } from "./project-queue-bar";
import { useTimeAgo } from "../../i18n/use-time-ago";
import { useT } from "../../i18n";

// CR-2026-010: the 5 presenter activity actions rendered as notice cards in
// the message stream (release included — it has no inbox recipient but still
// gets a card, same as the other five).
const PRESENTER_NOTICE_ACTIONS = new Set([
  "presenter_requested",
  "presenter_approved",
  "presenter_rejected",
  "presenter_transferred",
  "presenter_revoked",
  "presenter_released",
]);

// ─── Container ───────────────────────────────────────────────────────────
//
// The configured Team Agent tab of the project chat panel (CR-2026-006
// TASK-04). User messages ride the issue comment stream (bubbles); agent
// runs ride the issue task-run stream (execution cards). The two are
// coalesced by time and replayed in full — the timeline endpoint has no
// pagination (hard cap 2000), so a "no earlier messages" divider is pinned
// to the top instead of a load-more affordance.

export function ProjectTeamAgentChat({
  issueId,
  sessionId,
  projectId,
  wsId,
  teamAgentId,
  canConfigure,
}: {
  /** The chat container issue id — "" until the first send binds one (AC-11). */
  issueId: string;
  /** The active session id — anchors PATCH/send (CR-2026-056 §3.1). */
  sessionId: string;
  projectId: string;
  wsId: string;
  /** The configured Team Agent's agent id — drives the model selector. */
  teamAgentId: string;
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const userId = useAuthStore((s) => s.user?.id);
  // AC-11: before the first send there is no container issue — the timeline
  // and task list stay empty (no container-scoped fetches) while the
  // composer stays fully usable.
  const hasContainer = issueId !== "";
  // WS `comment:created` already writes new comments straight into this cache.
  const { timeline } = useIssueTimeline(issueId, userId);
  // Same cache key + WS `task:*` invalidation as ExecutionLogSection, so agent
  // runs stay live without a local subscription.
  const { data: tasks = [] } = useQuery({
    queryKey: issueKeys.tasks(issueId),
    queryFn: () => api.listTasksByIssue(issueId),
    staleTime: 30_000,
    enabled: hasContainer,
  });

  const comments = useMemo(
    () => timeline.filter((e) => e.type === "comment" && e.actor_type === "member"),
    [timeline],
  );
  // CR-2026-010: presenter transitions ride the same activity_log + WS
  // activity:created path as any other issue activity (see project_presenter.go's
  // recordPresenterActivity), so useIssueTimeline already has them — this is
  // purely a display-side filter, not a new data source.
  const presenterNotices = useMemo(
    () =>
      timeline.filter(
        (e) => e.type === "activity" && !!e.action && PRESENTER_NOTICE_ACTIONS.has(e.action),
      ),
    [timeline],
  );

  // CR governance gates (CR-2026-011 TASK-06): the same query CrStatusBadge
  // reads, kept live by cr:updated (WS) — no separate subscription needed.
  const { data: crs = [] } = useQuery(projectGatesOptions(wsId, projectId));

  // "Agent requests only" filter (CR-2026-007 DD-4). Read/write happens here
  // so the switch below is the single owner of the store round-trip; the
  // stream view underneath just receives the resolved boolean and stays a
  // pure render branch.
  const agentRequestFilter = useProjectChatStore(
    (s) => s.agentRequestFilter[projectId] === true,
  );
  const setAgentRequestFilter = useProjectChatStore((s) => s.setAgentRequestFilter);

  return (
    <div className="flex h-full min-h-0 w-full flex-col">
      <div className="flex shrink-0 justify-end border-b px-4 py-1.5">
        <label
          data-testid="project-chat-agent-filter"
          className="flex cursor-pointer items-center gap-2 text-xs text-muted-foreground"
        >
          <span>{t(($) => $.chat.stream.agent_requests_only)}</span>
          <Switch
            size="sm"
            data-testid="project-chat-agent-filter-switch"
            checked={agentRequestFilter}
            onCheckedChange={(checked: boolean) =>
              setAgentRequestFilter(projectId, checked)
            }
          />
        </label>
      </div>
      <div className="flex-1 min-h-0 overflow-y-auto" data-tab-scroll-root>
        <TeamAgentStreamView
          comments={comments}
          tasks={tasks}
          crs={crs}
          presenterNotices={presenterNotices}
          currentUserId={userId}
          wsId={wsId}
          projectId={projectId}
          canConfigure={canConfigure}
          filterOn={agentRequestFilter}
        />
      </div>
      <ProjectQueueBar
        wsId={wsId}
        projectId={projectId}
        currentUserId={userId}
        canConfigure={canConfigure}
      />
      <TeamAgentComposer
        projectId={projectId}
        wsId={wsId}
        issueId={issueId}
        sessionId={sessionId}
        teamAgentId={teamAgentId}
        canConfigure={canConfigure}
      />
    </div>
  );
}

// ─── Stream (presentational; coalesces comments + task cards by time) ─────

export function TeamAgentStreamView({
  comments,
  tasks,
  crs = [],
  presenterNotices = [],
  wsId,
  projectId,
  currentUserId,
  canConfigure,
  filterOn,
}: {
  comments: TimelineEntry[];
  tasks: AgentTask[];
  /** CR governance gates (CR-2026-011 TASK-06) — each gate_node becomes a
   *  third kind of stream item, interleaved with comments and task cards. */
  crs?: ProjectGateCR[];
  presenterNotices?: TimelineEntry[];
  wsId: string;
  projectId: string;
  currentUserId?: string;
  /** Workspace owner/admin — can stop any running task, not just their own (DD-1). */
  canConfigure: boolean;
  /** "Agent requests only" filter (DD-4): switches task cards to summary form. */
  filterOn: boolean;
}) {
  const { t } = useT("projects");

  // "Withdrawn" join (DD-5): a cancelled task's trigger_comment_id maps back
  // to the comment bubble that spawned it. Pure lookup over data already in
  // the react-query cache (the tasks list) — no new request.
  const withdrawnCommentIds = useMemo(() => {
    const ids = new Set<string>();
    for (const task of tasks) {
      if (task.status === "cancelled" && task.trigger_comment_id) {
        ids.add(task.trigger_comment_id);
      }
    }
    return ids;
  }, [tasks]);

  const items = useMemo(() => {
    const merged: { key: string; at: number; node: React.ReactNode }[] = [];
    for (const c of comments) {
      merged.push({
        key: `c:${c.id}`,
        at: new Date(c.created_at).getTime(),
        node: (
          <UserBubble
            key={`c:${c.id}`}
            entry={c}
            currentUserId={currentUserId}
            withdrawn={withdrawnCommentIds.has(c.id)}
          />
        ),
      });
    }
    for (const task of tasks) {
      merged.push({
        key: `t:${task.id}`,
        at: new Date(task.created_at).getTime(),
        node: (
          <TaskExecutionCard
            key={`t:${task.id}`}
            task={task}
            currentUserId={currentUserId}
            canConfigure={canConfigure}
            filterOn={filterOn}
            wsId={wsId}
            projectId={projectId}
          />
        ),
      });
    }
    for (const cr of crs) {
      // CR-2026-053 TASK-07 (FR-B6): pending_stage non-empty renders the ONE
      // current ApprovalCard directly — even when gate_nodes lacks a
      // human_approval/running node (the CR is at an approval gate but the
      // node projection is missing/empty). pending_stage empty → no current
      // card; blocked/history nodes below keep rendering.
      if (cr.pending_stage) {
        merged.push({
          key: `g:${cr.cr_id}:current`,
          at: new Date(cr.updated_at).getTime(),
          node: <ApprovalCard cr={cr} wsId={wsId} projectId={projectId} />,
        });
      }
      for (const node of cr.gate_nodes) {
        // Skip the current human_approval/running node: the pending_stage
        // branch above renders exactly one current approval card; without
        // this skip, both paths would double-render. Blocked cards and
        // completed/failed history keep showing (AC-C2/AC-C3).
        if (node.kind === "human_approval" && node.status === "running") {
          continue;
        }
        const key = `g:${cr.cr_id}:${node.node_id}:${node.attempt}`;
        merged.push({
          key,
          at: node.started_at ? new Date(node.started_at).getTime() : new Date(cr.updated_at).getTime(),
          node: (
            <CrGateCard key={key} cr={cr} node={node} wsId={wsId} projectId={projectId} />
          ),
        });
      }
    }
    for (const notice of presenterNotices) {
      merged.push({
        key: `p:${notice.id}`,
        at: new Date(notice.created_at).getTime(),
        node: <PresenterNoticeCard key={`p:${notice.id}`} entry={notice} />,
      });
    }
    // Stable order: by timestamp, then key so equal timestamps don't jitter.
    merged.sort((a, b) => a.at - b.at || a.key.localeCompare(b.key));
    return merged;
  }, [
    comments,
    tasks,
    crs,
    presenterNotices,
    currentUserId,
    withdrawnCommentIds,
    canConfigure,
    filterOn,
    wsId,
    projectId,
  ]);

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 px-4 py-3">
      <div
        data-testid="project-chat-no-earlier"
        className="text-center text-xs text-muted-foreground/70"
      >
        {t(($) => $.chat.stream.no_earlier)}
      </div>
      {items.map((item) => item.node)}
    </div>
  );
}

function UserBubble({
  entry,
  currentUserId,
  withdrawn,
}: {
  entry: TimelineEntry;
  currentUserId?: string;
  /** "Withdrawn" badge (DD-5): a cancelled task's trigger comment. */
  withdrawn: boolean;
}) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const isOwn = entry.actor_type === "member" && entry.actor_id === currentUserId;

  const handleCopy = () => {
    void navigator.clipboard.writeText(entry.content ?? "").then(() => {
      toast.success(t(($) => $.chat.stream.copied));
    });
  };

  const copyButton = (
    <Button
      type="button"
      size="icon-xs"
      variant="ghost"
      className="shrink-0 opacity-0 transition-opacity group-hover:opacity-100"
      data-testid="project-chat-copy"
      aria-label={t(($) => $.chat.stream.copy)}
      onClick={handleCopy}
    >
      <Copy className="h-3 w-3" />
    </Button>
  );

  const withdrawnBadge = withdrawn && (
    <Badge variant="secondary" data-testid="project-chat-withdrawn-badge">
      {t(($) => $.chat.stream.withdrawn)}
    </Badge>
  );

  if (isOwn) {
    return (
      <div className="group flex items-center justify-end gap-1.5" data-testid="project-chat-user-bubble">
        {withdrawnBadge}
        {copyButton}
        <div className="max-w-[80%] break-words rounded-2xl bg-muted px-3.5 py-2 text-sm">
          <ReadonlyContent content={entry.content ?? ""} attachments={entry.attachments} />
        </div>
      </div>
    );
  }

  return (
    <div className="group flex items-start gap-2.5" data-testid="project-chat-user-bubble">
      <ActorAvatar
        actorType={entry.actor_type}
        actorId={entry.actor_id}
        size="sm"
        className="mt-0.5 shrink-0"
      />
      <div className="min-w-0 flex-1">
        <div className="mb-0.5 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
          <span>{getActorName(entry.actor_type, entry.actor_id)}</span>
          {withdrawnBadge}
        </div>
        <div className="flex items-start gap-1.5">
          <div className="min-w-0 flex-1 text-sm leading-relaxed text-foreground/85">
            <ReadonlyContent content={entry.content ?? ""} attachments={entry.attachments} />
          </div>
          {copyButton}
        </div>
      </div>
    </div>
  );
}

// ─── Presenter notice card (CR-2026-010) ──────────────────────────────────
// A narrow centered system-status bar, not a message bubble — presenter
// transitions are audit events, not something anyone "said". Deliberately
// distinct from UserBubble/TaskExecutionCard's containers.

function PresenterNoticeCard({ entry }: { entry: TimelineEntry }) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const timeAgo = useTimeAgo();
  const details = (entry.details ?? {}) as Record<string, string>;

  const toName = details.to_user_id ? getActorName("member", details.to_user_id) : "";
  const fromName = details.from_user_id ? getActorName("member", details.from_user_id) : "";
  const approverName = details.by_user_id ? getActorName("member", details.by_user_id) : "";
  const params = {
    user: toName || fromName,
    approver: approverName,
    to: toName,
    from: fromName,
  };

  const action = entry.action ?? "";
  const text = PRESENTER_NOTICE_ACTIONS.has(action)
    ? t(($) => $.chat.notices[action as keyof typeof $.chat.notices], params)
    : action;

  return (
    <div
      data-testid="project-chat-presenter-notice"
      data-action={action}
      className="flex items-center justify-center gap-1.5 py-1 text-center text-xs text-muted-foreground"
    >
      <Mic className="h-3 w-3 shrink-0" />
      <span>{text}</span>
      <span className="text-muted-foreground/60">·</span>
      <span className="tabular-nums text-muted-foreground/70">{timeAgo(entry.created_at)}</span>
    </div>
  );
}

// ─── Task execution card (agent side) ────────────────────────────────────

type TaskStatusKind = "running" | "done" | "error" | "interrupted";

function taskStatusKind(status: AgentTask["status"]): TaskStatusKind {
  switch (status) {
    case "completed":
      return "done";
    case "failed":
      return "error";
    case "cancelled":
      return "interrupted";
    default:
      // queued / dispatched / waiting_local_directory / running
      return "running";
  }
}

function formatDurationMs(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

// `result` is `unknown` on the wire ({ output, pr_url, tool_calls, … } —
// daemon.go:2470-2509). The summary form (DD-4) only ever surfaces the
// `output` string; rendering the whole object would dump raw JSON in the UI.
function taskResultOutput(result: unknown): string {
  if (result && typeof result === "object" && "output" in result) {
    const output = (result as { output?: unknown }).output;
    if (typeof output === "string") return output;
  }
  return "";
}

function TaskExecutionCard({
  task,
  currentUserId,
  canConfigure,
  filterOn,
  wsId,
  projectId,
}: {
  task: AgentTask;
  currentUserId?: string;
  /** Workspace owner/admin — can stop any running task, not just their own (DD-1). */
  canConfigure: boolean;
  /** "Agent requests only" filter (DD-4): summary form, no TimelineView. */
  filterOn: boolean;
  wsId: string;
  projectId: string;
}) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  // AC-4: the filter must not cause a network request for the timeline it no
  // longer renders.
  const { data: messages } = useQuery({ ...taskMessagesOptions(task.id), enabled: !filterOn });
  const items = useMemo(() => buildTimeline(messages ?? []), [messages]);
  const cancelTask = useCancelProjectQueueTask(wsId, projectId);

  const kind = taskStatusKind(task.status);
  const running = kind === "running";
  // DD-1: visible only on the literal `running` status (queued/dispatched
  // are the queue-bar's "clear" affordance, not this card's), and only for
  // the task's originator or a workspace owner/admin. `originator_user_id`
  // is the canonical check (TSUG-002) — NOT trigger_comment_id → timeline
  // authorship, which drifts under comment coalescing.
  const isOriginator =
    task.originator_user_id != null && task.originator_user_id === currentUserId;
  const canStop = task.status === "running" && (isOriginator || canConfigure);

  const handleStop = async () => {
    try {
      const res = await cancelTask.mutateAsync(task.id);
      // TSUG-007: repeat-cancel of an already-cancelled task is a silent
      // success (idempotent 200) — only a *different* terminal status means
      // the stop request came too late.
      if (res.status !== "cancelled") {
        toast.error(t(($) => $.chat.stream.cancel_already_finished));
      }
    } catch (e) {
      toast.error(e instanceof ApiError ? e.message : t(($) => $.chat.stream.send_failed));
    }
  };
  // ponytail: elapsed is computed once at render, so a running card shows an
  // approximate (non-ticking) duration. A live timer would need a per-card
  // interval — not worth it until product asks for second-by-second feedback.
  const duration =
    task.started_at != null
      ? formatDurationMs(
          new Date(task.completed_at ?? new Date().toISOString()).getTime() -
            new Date(task.started_at).getTime(),
        )
      : null;

  const badge: Record<TaskStatusKind, { icon: React.ReactNode; label: string; tone: string }> = {
    running: {
      icon: <Loader2 className="h-3 w-3 animate-spin" />,
      label: t(($) => $.chat.stream.status_running),
      tone: "text-info",
    },
    done: {
      icon: <CheckCircle2 className="h-3 w-3" />,
      label: t(($) => $.chat.stream.status_done),
      tone: "text-success",
    },
    error: {
      icon: <XCircle className="h-3 w-3" />,
      label: t(($) => $.chat.stream.status_error),
      tone: "text-destructive",
    },
    interrupted: {
      icon: <Ban className="h-3 w-3" />,
      label: t(($) => $.chat.stream.status_interrupted),
      tone: "text-muted-foreground",
    },
  };
  const b = badge[kind];

  return (
    <div className="flex items-start gap-2.5" data-testid="project-chat-task-card">
      {task.agent_id ? (
        <ActorAvatar actorType="agent" actorId={task.agent_id} size="sm" className="mt-0.5 shrink-0" />
      ) : (
        <span className="mt-0.5 inline-block h-5 w-5 shrink-0 rounded-full bg-muted" />
      )}
      <div className="min-w-0 flex-1 space-y-1.5">
        <div className="flex items-center gap-2 text-xs">
          {task.agent_id && (
            <span className="font-medium text-muted-foreground">
              {getActorName("agent", task.agent_id)}
            </span>
          )}
          <span className={cn("inline-flex items-center gap-1", b.tone)}>
            {b.icon}
            {b.label}
          </span>
          {duration && !running && (
            <span className="tabular-nums text-muted-foreground/70">{duration}</span>
          )}
          {canStop && (
            <Button
              type="button"
              size="xs"
              variant="ghost"
              className="ml-auto"
              data-testid="project-chat-task-stop"
              disabled={cancelTask.isPending}
              onClick={() => void handleStop()}
            >
              {t(($) => $.chat.stream.stop)}
            </Button>
          )}
        </div>
        {filterOn ? (
          kind === "done" && (
            <div
              className="text-sm leading-relaxed text-foreground/85"
              data-testid="project-chat-task-summary-output"
            >
              {taskResultOutput(task.result) || t(($) => $.chat.stream.no_text_reply)}
            </div>
          )
        ) : (
          items.length > 0 && <TimelineView items={items} isStreaming={running} />
        )}
        {kind === "error" && task.error && (
          <div className="text-xs text-destructive/90">{task.error}</div>
        )}
      </div>
    </div>
  );
}

// ─── Composer ─────────────────────────────────────────────────────────────

const EMPTY_DRAFT_ATTACHMENTS: Attachment[] = [];

// CR-2026-012 DD-9/DD-10: bridges ChatInputCore onto the project chat store's
// `${projectId}:team_agent` slot. Editor identity is constant ("team_agent")
// — the project pane has no per-agent remount semantics (tech-debt doc §4.3),
// and draft/attachments persist across tab switches via the store.
function useTeamAgentDraftAdapter(projectId: string): ChatInputDraftAdapter {
  const draftKey = projectChatDraftKey(projectId, "team_agent");
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
      editorKey: "team_agent",
      draft,
      attachments,
      // The adapter contract passes the storage key explicitly; this pane's
      // slot is fixed, so the writers pin projectId/mode and ignore it.
      setDraft: (_key, content) => setDraft(projectId, "team_agent", content),
      setAttachments: (_key, atts) =>
        setDraftAttachments(projectId, "team_agent", atts),
      addAttachment: (_key, att) =>
        addDraftAttachment(projectId, "team_agent", att),
      clearDraft: (_key) => setDraft(projectId, "team_agent", ""),
    }),
    [projectId, draftKey, draft, attachments, setDraft, setDraftAttachments, addDraftAttachment],
  );
}

export function TeamAgentComposer({
  projectId,
  wsId,
  issueId,
  sessionId,
  teamAgentId,
  canConfigure,
}: {
  projectId: string;
  wsId: string;
  /** The chat container issue id — "" until the first send binds one. */
  issueId: string;
  /** The active session id — anchors PATCH/send (CR-2026-056 §3.1). */
  sessionId: string;
  /** The configured Team Agent's agent id, when the panel has one. */
  teamAgentId?: string;
  /** Owner/admin — the session-config PATCH gate (AC-6); also exempts them
   *  from the full-queue lock. */
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const qc = useQueryClient();
  const draftAdapter = useTeamAgentDraftAdapter(projectId);
  const { mutateAsync, isPending } = useSendProjectChatMessage(wsId, projectId);
  const { uploadWithToast } = useFileUpload(api, (err) => toast.error(err.message));

  // CR-2026-056 §4.11 (AC-1/AC-2): uploads are drafts until the send
  // transaction binds them — the composer stops sending issueId (TASK-09
  // backend tolerance), so an unbound container never blocks an upload.
  const handleComposerUpload = useCallback(
    (file: File) => uploadWithToast(file, issueId !== "" ? { issueId } : {}),
    [uploadWithToast, issueId],
  );

  // ─── Session-config state (CR-2026-056 §3.1/§4.11) ─────────────────────
  // The effective model/thinking level now lives on the ACTIVE SESSION, not
  // on the agent row: the pane reads the chat context (already cached by the
  // panel) and persists through PATCH /chat/config — `api.updateAgent` must
  // never run from the chat path (AC-1). The hard-degradation sentinel
  // (session_id "") can't reach here: the panel gates mounting on it, and
  // every write below refuses an empty session_id anyway.
  const { data: chat } = useQuery(projectChatOptions(wsId, projectId));
  const chatModel = chat?.model ?? "";
  const chatThinking = chat?.thinking_level ?? "";

  const { data: agents = [] } = useQuery({
    ...agentListOptions(wsId),
    enabled: !!teamAgentId,
  });
  const agent = teamAgentId
    ? agents.find((a) => a.id === teamAgentId) ?? null
    : null;

  const { data: runtimes = [] } = useQuery({
    ...runtimeListOptions(wsId),
    enabled: !!agent,
  });
  const runtime = agent?.runtime_id
    ? runtimes.find((r) => r.id === agent.runtime_id) ?? null
    : null;
  const runtimeOnline = runtime?.status === "online";
  const modelsQuery = useQuery(
    runtimeModelsOptions(runtimeOnline ? agent?.runtime_id ?? null : null),
  );
  // Runtime is "ready" when online and the daemon has (or is still) reporting
  // models. A resolved-empty list or an offline runtime → the guide state.
  const runtimeReady =
    runtimeOnline &&
    (modelsQuery.isLoading || (modelsQuery.data?.models.length ?? 0) > 0);
  // Only gate send on runtime once we actually have an agent to check.
  const runtimeBlocked = agent != null && !runtimeReady;

  // Thinking levels for the CURRENT effective model (AC-2): same catalog
  // shape the agent inspector consumes; an empty set renders nothing.
  const thinkingLevels = useMemo(() => {
    const entry = (modelsQuery.data?.models ?? []).find((m) => m.id === chatModel);
    return entry?.thinking?.supported_levels ?? [];
  }, [modelsQuery.data, chatModel]);

  const chatKey = projectChatOptions(wsId, projectId).queryKey;

  const persistModel = async (model: string) => {
    if (!sessionId) return;
    const prev = chatModel;
    qc.setQueryData<ProjectChat>(chatKey, (old) => (old ? { ...old, model } : old));
    try {
      await api.patchProjectChatConfig(projectId, sessionId, { model });
      qc.invalidateQueries({ queryKey: chatKey });
    } catch {
      qc.setQueryData<ProjectChat>(chatKey, (old) => (old ? { ...old, model: prev } : old));
      toast.error(t(($) => $.chat.stream.model_update_failed));
    }
  };

  const persistThinking = async (level: string) => {
    if (!sessionId) return;
    const prev = chatThinking;
    qc.setQueryData<ProjectChat>(chatKey, (old) => (old ? { ...old, thinking_level: level } : old));
    try {
      await api.patchProjectChatConfig(projectId, sessionId, { thinking_level: level });
      qc.invalidateQueries({ queryKey: chatKey });
    } catch {
      qc.setQueryData<ProjectChat>(chatKey, (old) => (old ? { ...old, thinking_level: prev } : old));
      toast.error(t(($) => $.chat.stream.model_update_failed));
    }
  };

  // Live shared-queue depth (CR-2026-004). Drives recovery out of the
  // full-queue lock: WS `task:*` invalidation refetches it, and once depth
  // drops below the limit the local 429 latch clears.
  const { data: queue } = useQuery(projectQueueStatusOptions(wsId, projectId));
  const [sendQueueFull, setSendQueueFull] = useState<{ depth: number; limit: number } | null>(null);

  useEffect(() => {
    if (queue && queue.queue_limit > 0 && queue.queue_depth < queue.queue_limit) {
      setSendQueueFull(null);
    }
  }, [queue]);

  const liveFull =
    queue && queue.queue_limit > 0 && queue.queue_depth >= queue.queue_limit
      ? { depth: queue.queue_depth, limit: queue.queue_limit }
      : null;
  const queueFull = canConfigure ? null : sendQueueFull ?? liveFull;

  // CR-2026-010: presenter (single-writer control) rejection. Latched the
  // same way as sendQueueFull — the backend rejection is the source of
  // truth, WS project:presenter_changed invalidates this query so a
  // transition that resolves the block clears it without a manual refresh.
  //
  // The only real "unblocked" transition for a plain member is *becoming*
  // the presenter — presenter turning back to null does NOT unblock them
  // (SDD §4.3: presenter=null's default state is itself owner/admin-only,
  // so a plain member stays rejected either way). Clearing on presenter ===
  // null here would be wrong: it's the permanent blocked default for this
  // caller, not a resolution.
  const userId = useAuthStore((s) => s.user?.id);
  const { data: presenterState } = useQuery(projectPresenterOptions(wsId, projectId));
  const [presenterRequired, setPresenterRequired] = useState<{ presenterUserId: string } | null>(null);
  const requestPresenter = useRequestPresenter(wsId, projectId);

  useEffect(() => {
    if (presenterState?.presenter != null && presenterState.presenter.user_id === userId) {
      setPresenterRequired(null);
    }
  }, [presenterState, userId]);

  // Independent lock reasons (SDD §4.3): capacity and presenter control are
  // separate dimensions of "why can't I send right now" — combined with OR,
  // never merged into one branch.
  const locked = queueFull != null || presenterRequired != null;

  // Pending-message pattern (CLAUDE.md: render immediately with a visible
  // pending state and retry on failure, not silent optimism). The real
  // comment lands in the timeline via WS `comment:created`; this local bubble
  // only covers the gap between click and that event / the error toast.
  const [pendingMessage, setPendingMessage] = useState<string | null>(null);

  const handleSend = async (
    content: string,
    attachmentIds: string[] | undefined,
    commitInput: (options?: { extraDraftKeys?: string[]; clearEditor?: boolean }) => void,
  ): Promise<boolean> => {
    const trimmed = content.trim();
    if (!trimmed || locked || runtimeBlocked || !sessionId) return false;
    setPendingMessage(trimmed);
    try {
      await mutateAsync({ sessionId, content: trimmed, attachmentIds });
      // Success → clear draft + attachment slot through the adapter.
      commitInput();
      return true;
    } catch (e) {
      if (e instanceof ApiError && e.body && typeof e.body === "object") {
        const body = e.body as {
          code?: string;
          queue_depth?: number;
          queue_limit?: number;
          presenter_user_id?: string;
        };
        if (body.code === "presenter_required") {
          // Active presenter holds single-writer control and this caller
          // isn't it — distinct rejection reason from queue_full, must not
          // be conflated into the same banner/copy.
          setPresenterRequired({ presenterUserId: body.presenter_user_id ?? "" });
          return false; // keep the draft so the user can resend once unblocked
        }
        if (body.code === "project_queue_full") {
          // Full shared queue: latch the disabled state with the live pair.
          // Owner/admin are exempt, so only latch for non-privileged users.
          if (!canConfigure) {
            setSendQueueFull({ depth: body.queue_depth ?? 0, limit: body.queue_limit ?? 0 });
          }
          return false; // keep the draft so the user can resend on recovery
        }
        if (body.code === "team_agent_not_configured") {
          // Config drifted — the mutation's onError refreshes the chat context,
          // flipping the panel back to its unconfigured guide. Keep the draft.
          return false;
        }
        // enqueue_failed (502) and everything else: transient — keep the draft.
      }
      toast.error(t(($) => $.chat.stream.send_failed));
      return false;
    } finally {
      // Cleared on both success and failure: on success the real comment
      // takes over via WS; on failure the draft is preserved so the user can
      // just hit send again, and a lingering "sending…" bubble would lie.
      setPendingMessage(null);
    }
  };

  return (
    <div className="shrink-0 border-t px-4 py-3">
      {pendingMessage && (
        <div data-testid="project-chat-pending-message" className="mb-2 flex justify-end">
          <div className="flex max-w-[80%] items-center gap-1.5 rounded-2xl bg-muted/60 px-3.5 py-2 text-sm text-muted-foreground">
            <Loader2 className="h-3 w-3 shrink-0 animate-spin" />
            <span className="break-words">{pendingMessage}</span>
          </div>
        </div>
      )}
      {presenterRequired && (
        <div
          data-testid="project-chat-presenter-required"
          className="mb-2 flex items-center justify-between gap-2 rounded-md border border-warning/30 bg-warning/5 px-3 py-2 text-xs text-muted-foreground"
        >
          <span className="font-medium text-foreground">
            {presenterRequired.presenterUserId
              ? t(($) => $.chat.presenter.locked_title, {
                  name: getActorName("member", presenterRequired.presenterUserId),
                })
              : t(($) => $.chat.presenter.locked_title_default)}
          </span>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={requestPresenter.isPending || !!presenterState?.my_request}
            onClick={() => requestPresenter.mutate()}
          >
            {presenterState?.my_request
              ? t(($) => $.chat.presenter.requested)
              : t(($) => $.chat.presenter.request_cta)}
          </Button>
        </div>
      )}
      {queueFull && (
        <div
          data-testid="project-chat-queue-full"
          className="mb-2 rounded-md border border-warning/30 bg-warning/5 px-3 py-2 text-xs text-muted-foreground"
        >
          <div className="font-medium text-foreground">
            {t(($) => $.chat.stream.queue_full_title)}
          </div>
          <div className="mt-0.5 tabular-nums">
            {t(($) => $.chat.stream.queue_full_depth, {
              depth: queueFull.depth,
              limit: queueFull.limit,
            })}
          </div>
        </div>
      )}
      {agent && (
        <div
          data-testid="project-chat-model-row"
          className="mb-1.5 flex items-center gap-1.5 text-xs text-muted-foreground"
        >
          <span className="shrink-0">{t(($) => $.chat.stream.model_label)}</span>
          {/* CR-2026-056 §4.11 (AC-6): the session-config PATCH gate is
              owner/admin — a plain member always gets the read-only badge;
              runtime availability then decides the editor's content. The
              value shown is the SESSION's effective model, not agent.model. */}
          {!canConfigure ? (
            <span data-testid="project-chat-model-readonly">
              <ModelPicker
                runtimeId={agent.runtime_id}
                runtimeOnline={!!runtimeOnline}
                value={chatModel}
                canEdit={false}
                onChange={() => {}}
              />
            </span>
          ) : runtimeReady ? (
            <span data-testid="project-chat-model-picker">
              <ModelPicker
                runtimeId={agent.runtime_id}
                runtimeOnline={!!runtimeOnline}
                value={chatModel}
                canEdit
                onChange={persistModel}
              />
            </span>
          ) : (
            <span data-testid="project-chat-model-runtime-guide">
              {t(($) => $.chat.stream.runtime_guide)}
            </span>
          )}
          {thinkingLevels.length > 0 && (
            <span
              data-testid="project-chat-thinking-picker"
              className="flex items-center gap-1"
            >
              <span className="shrink-0">{t(($) => $.chat.stream.thinking_label)}</span>
              <ThinkingPicker
                value={chatThinking}
                levels={thinkingLevels}
                canEdit={canConfigure}
                onChange={persistThinking}
              />
            </span>
          )}
        </div>
      )}
      {/* CR-2026-012 FR-8: rich composer (attachments + member-only @
          mentions) on top of the same send/lock/pending machinery above. */}
      <div data-testid="project-chat-composer">
        <ChatInputCore
          draftAdapter={draftAdapter}
          onSend={handleSend}
          onUploadFile={handleComposerUpload}
          disabled={locked || runtimeBlocked}
          isRunning={isPending}
          mentionItemTypes={["member"]}
        />
      </div>
    </div>
  );
}
