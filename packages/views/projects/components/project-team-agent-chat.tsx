"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Ban, CheckCircle2, Loader2, SendHorizontal, XCircle } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@multica/core/api";
import { issueKeys } from "@multica/core/issues/queries";
import { taskMessagesOptions } from "@multica/core/chat/queries";
import {
  projectChatDraftKey,
  projectGatesOptions,
  projectQueueStatusOptions,
  useProjectChatStore,
  useSendProjectChatMessage,
} from "@multica/core/projects";
import type { ProjectGateCR } from "@multica/core/api/schemas";
import { useAuthStore } from "@multica/core/auth";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
import { runtimeListOptions, runtimeModelsOptions } from "@multica/core/runtimes";
import { useAgentPermissions } from "@multica/core/permissions";
import type { Agent, AgentTask, TimelineEntry } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { Button } from "@multica/ui/components/ui/button";
import { ActorAvatar } from "../../common/actor-avatar";
import { ReadonlyContent } from "../../editor";
import { buildTimeline } from "../../common/task-transcript";
import { TimelineView } from "../../chat/components/chat-message-list";
import { CrGateCard } from "./cr-gate-card";
import { ModelPicker } from "../../agents/components/inspector/model-picker";
import { useIssueTimeline } from "../../issues/hooks/use-issue-timeline";
import { useT } from "../../i18n";

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
  projectId,
  wsId,
  teamAgentId,
  canConfigure,
}: {
  issueId: string;
  projectId: string;
  wsId: string;
  /** The configured Team Agent's agent id — drives the model selector. */
  teamAgentId: string;
  canConfigure: boolean;
}) {
  const userId = useAuthStore((s) => s.user?.id);
  // WS `comment:created` already writes new comments straight into this cache.
  const { timeline } = useIssueTimeline(issueId, userId);
  // Same cache key + WS `task:*` invalidation as ExecutionLogSection, so agent
  // runs stay live without a local subscription.
  const { data: tasks = [] } = useQuery({
    queryKey: issueKeys.tasks(issueId),
    queryFn: () => api.listTasksByIssue(issueId),
    staleTime: 30_000,
  });

  const comments = useMemo(
    () => timeline.filter((e) => e.type === "comment" && e.actor_type === "member"),
    [timeline],
  );

  // CR governance gates (CR-2026-011 TASK-06): the same query CrStatusBadge
  // reads, kept live by cr:updated (WS) — no separate subscription needed.
  const { data: crs = [] } = useQuery(projectGatesOptions(wsId, projectId));

  return (
    <div className="flex h-full min-h-0 w-full flex-col">
      <div className="flex-1 min-h-0 overflow-y-auto" data-tab-scroll-root>
        <TeamAgentStreamView
          comments={comments}
          tasks={tasks}
          crs={crs}
          wsId={wsId}
          projectId={projectId}
          currentUserId={userId}
        />
      </div>
      <TeamAgentComposer
        projectId={projectId}
        wsId={wsId}
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
  wsId,
  projectId,
  currentUserId,
}: {
  comments: TimelineEntry[];
  tasks: AgentTask[];
  /** CR governance gates (CR-2026-011 TASK-06) — each gate_node becomes a
   *  third kind of stream item, interleaved with comments and task cards. */
  crs?: ProjectGateCR[];
  /** Required together with projectId when crs is non-empty (the approval
   *  card's mutation needs both); optional otherwise so existing callers
   *  (e.g. tests) that don't pass gate data don't need to thread them. */
  wsId?: string;
  projectId?: string;
  currentUserId?: string;
}) {
  const { t } = useT("projects");

  const items = useMemo(() => {
    const merged: { key: string; at: number; node: React.ReactNode }[] = [];
    for (const c of comments) {
      merged.push({
        key: `c:${c.id}`,
        at: new Date(c.created_at).getTime(),
        node: <UserBubble key={`c:${c.id}`} entry={c} currentUserId={currentUserId} />,
      });
    }
    for (const task of tasks) {
      merged.push({
        key: `t:${task.id}`,
        at: new Date(task.created_at).getTime(),
        node: <TaskExecutionCard key={`t:${task.id}`} task={task} />,
      });
    }
    if (wsId && projectId) {
      for (const cr of crs) {
        for (const node of cr.gate_nodes) {
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
    }
    // Stable order: by timestamp, then key so equal timestamps don't jitter.
    merged.sort((a, b) => a.at - b.at || a.key.localeCompare(b.key));
    return merged;
  }, [comments, tasks, crs, wsId, projectId, currentUserId]);

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
}: {
  entry: TimelineEntry;
  currentUserId?: string;
}) {
  const { getActorName } = useActorName();
  const isOwn = entry.actor_type === "member" && entry.actor_id === currentUserId;

  if (isOwn) {
    return (
      <div className="flex justify-end" data-testid="project-chat-user-bubble">
        <div className="max-w-[80%] break-words rounded-2xl bg-muted px-3.5 py-2 text-sm">
          <ReadonlyContent content={entry.content ?? ""} attachments={entry.attachments} />
        </div>
      </div>
    );
  }

  return (
    <div className="flex items-start gap-2.5" data-testid="project-chat-user-bubble">
      <ActorAvatar
        actorType={entry.actor_type}
        actorId={entry.actor_id}
        size="sm"
        className="mt-0.5 shrink-0"
      />
      <div className="min-w-0 flex-1">
        <div className="mb-0.5 text-xs font-medium text-muted-foreground">
          {getActorName(entry.actor_type, entry.actor_id)}
        </div>
        <div className="text-sm leading-relaxed text-foreground/85">
          <ReadonlyContent content={entry.content ?? ""} attachments={entry.attachments} />
        </div>
      </div>
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

function TaskExecutionCard({ task }: { task: AgentTask }) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const { data: messages } = useQuery(taskMessagesOptions(task.id));
  const items = useMemo(() => buildTimeline(messages ?? []), [messages]);

  const kind = taskStatusKind(task.status);
  const running = kind === "running";
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
        </div>
        {items.length > 0 && <TimelineView items={items} isStreaming={running} />}
        {kind === "error" && task.error && (
          <div className="text-xs text-destructive/90">{task.error}</div>
        )}
      </div>
    </div>
  );
}

// ─── Composer ─────────────────────────────────────────────────────────────

export function TeamAgentComposer({
  projectId,
  wsId,
  teamAgentId,
  canConfigure,
}: {
  projectId: string;
  wsId: string;
  /** The configured Team Agent's agent id, when the panel has one. */
  teamAgentId?: string;
  /** Owner/admin — backend exempts them from the full-queue lock, so the
   *  composer never enters the disabled state for them. */
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const qc = useQueryClient();
  const draftKey = projectChatDraftKey(projectId, "team_agent");
  const draft = useProjectChatStore((s) => s.drafts[draftKey] ?? "");
  const setDraft = useProjectChatStore((s) => s.setDraft);
  const { mutateAsync, isPending } = useSendProjectChatMessage(wsId, projectId);

  // ─── Model selector state (CR-2026-006 TASK-05, TSUG-003) ───────────────
  // The selector binds the Team Agent AGENT's `model` field, not a per-message
  // override (the daemon reads agent.Model at claim time and ignores task-level
  // overrides). Two disabled states are kept distinct per TSUG-003, decided in
  // order: permission first (can this user edit the agent at all?), then
  // runtime availability (did the daemon report any models?).
  const { data: agents = [] } = useQuery({
    ...agentListOptions(wsId),
    enabled: !!teamAgentId,
  });
  const agent = teamAgentId
    ? agents.find((a) => a.id === teamAgentId) ?? null
    : null;
  // Reuse the exact agent edit-permission rule the inspector uses — being a
  // project owner/admin (`canConfigure`) does NOT imply edit rights on an
  // agent someone else owns.
  const { canEdit } = useAgentPermissions(agent, wsId);

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

  const persistModel = async (model: string) => {
    if (!agent) return;
    const key = agentListOptions(wsId).queryKey;
    const prev = agent.model ?? "";
    // Optimistic field patch (CLAUDE.md: predictable, same screen, trivial
    // rollback) so the chip flips immediately, matching agent-detail-page.
    qc.setQueryData<Agent[]>(key, (old) =>
      old?.map((a) => (a.id === agent.id ? { ...a, model } : a)),
    );
    try {
      await api.updateAgent(agent.id, { model });
      qc.invalidateQueries({ queryKey: key });
    } catch {
      qc.setQueryData<Agent[]>(key, (old) =>
        old?.map((a) => (a.id === agent.id ? { ...a, model: prev } : a)),
      );
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
  const locked = queueFull != null;

  // Pending-message pattern (CLAUDE.md: render immediately with a visible
  // pending state and retry on failure, not silent optimism). The real
  // comment lands in the timeline via WS `comment:created`; this local bubble
  // only covers the gap between click and that event / the error toast.
  const [pendingMessage, setPendingMessage] = useState<string | null>(null);

  const handleSend = async () => {
    const content = draft.trim();
    if (!content || locked || runtimeBlocked || isPending) return;
    setPendingMessage(content);
    try {
      await mutateAsync(content);
      setDraft(projectId, "team_agent", ""); // success → clear draft
    } catch (e) {
      if (e instanceof ApiError && e.body && typeof e.body === "object") {
        const body = e.body as {
          code?: string;
          queue_depth?: number;
          queue_limit?: number;
        };
        if (body.code === "project_queue_full") {
          // Full shared queue: latch the disabled state with the live pair.
          // Owner/admin are exempt, so only latch for non-privileged users.
          if (!canConfigure) {
            setSendQueueFull({ depth: body.queue_depth ?? 0, limit: body.queue_limit ?? 0 });
          }
          return; // keep the draft so the user can resend on recovery
        }
        if (body.code === "team_agent_not_configured") {
          // Config drifted — the mutation's onError refreshes the chat context,
          // flipping the panel back to its unconfigured guide. Keep the draft.
          return;
        }
        // enqueue_failed (502) and everything else: transient — keep the draft.
      }
      toast.error(t(($) => $.chat.stream.send_failed));
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
          {/* Order (TSUG-003): permission gates interactivity; a non-editor
              always gets the read-only badge — never the runtime guide — so
              "you can't change this" is never misread as "the environment is
              broken". Runtime availability then decides the editor's content. */}
          {!canEdit.allowed ? (
            <span data-testid="project-chat-model-readonly">
              <ModelPicker
                runtimeId={agent.runtime_id}
                runtimeOnline={!!runtimeOnline}
                value={agent.model ?? ""}
                canEdit={false}
                onChange={() => {}}
              />
            </span>
          ) : runtimeReady ? (
            <span data-testid="project-chat-model-picker">
              <ModelPicker
                runtimeId={agent.runtime_id}
                runtimeOnline={!!runtimeOnline}
                value={agent.model ?? ""}
                canEdit
                onChange={persistModel}
              />
            </span>
          ) : (
            <span data-testid="project-chat-model-runtime-guide">
              {t(($) => $.chat.stream.runtime_guide)}
            </span>
          )}
        </div>
      )}
      <div className="relative flex items-end gap-2 rounded-lg border bg-card px-3 py-2 transition-colors focus-within:border-brand">
        <textarea
          data-testid="project-chat-composer-input"
          value={draft}
          disabled={locked || runtimeBlocked}
          rows={1}
          placeholder={t(($) => $.chat.stream.composer_placeholder)}
          onChange={(e) => setDraft(projectId, "team_agent", e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              void handleSend();
            }
          }}
          className="max-h-32 min-h-[24px] flex-1 resize-none bg-transparent text-sm outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-60"
        />
        <Button
          type="button"
          size="icon-sm"
          data-testid="project-chat-send"
          disabled={locked || runtimeBlocked || isPending || !draft.trim()}
          onClick={() => void handleSend()}
          aria-label={t(($) => $.chat.stream.send)}
        >
          {isPending ? (
            <Loader2 className="h-4 w-4 animate-spin" />
          ) : (
            <SendHorizontal className="h-4 w-4" />
          )}
        </Button>
      </div>
    </div>
  );
}
