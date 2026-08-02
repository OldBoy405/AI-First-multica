"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronUp } from "lucide-react";
import { toast } from "sonner";
import { ApiError } from "@multica/core/api";
import { projectQueueItemsOptions, useCancelProjectQueueTask } from "@multica/core/projects";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import type { QueueItem } from "@multica/core/api/schemas";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import { Button } from "@multica/ui/components/ui/button";
import { useFormatRelativeDate } from "./labels";
import { useT } from "../../i18n";

// Live queue detail strip (CR-2026-007 DD-3/DD-2). Sits between the message
// stream and the composer (TASK-04 mount point). Rides the same
// `projectQueueItemsOptions` cache as the composer's depth/limit banner, so
// the realtime `task:*` prefix invalidation (use-realtime-sync.ts) refreshes
// both with zero extra wiring.
export function ProjectQueueBar({
  wsId,
  projectId,
  currentUserId,
  canConfigure,
}: {
  wsId: string;
  projectId: string;
  currentUserId?: string;
  /** Owner/admin — may clear anyone's queued/dispatched item (SDD DD-2). */
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const { data } = useQuery(projectQueueItemsOptions(wsId, projectId));
  const [expanded, setExpanded] = useState(false);
  const cancelMutation = useCancelProjectQueueTask(wsId, projectId);
  const formatRelativeDate = useFormatRelativeDate();

  const count = data?.queue_depth ?? 0;
  // count === 0 collapses to nothing (SDD: "不占注意力形态", implementation
  // takes the no-render option over a minimized chip).
  if (count === 0) return null;

  const handleCancel = async (taskId: string) => {
    try {
      const res = await cancelMutation.mutateAsync(taskId);
      // Branch 1 (TSUG-007): cancelled — including the idempotent repeat of an
      // already-cancelled item — stays silent.
      if (res.status === "cancelled") return;
      // Branch 2: any other terminal status means the server raced us to
      // completion; the task can't be cancelled anymore.
      toast.error(t(($) => $.chat.queue_bar.cancel_already_finished));
    } catch (e) {
      // Branch 3: thrown ApiError (e.g. 403) — surface the server message.
      if (e instanceof ApiError) toast.error(e.message);
    }
  };

  return (
    <div className="shrink-0 border-t px-4 py-2" data-testid="project-queue-bar">
      <button
        type="button"
        data-testid="project-queue-bar-toggle"
        className="flex w-full items-center justify-between text-xs text-muted-foreground"
        onClick={() => setExpanded((v) => !v)}
        aria-expanded={expanded}
      >
        <span>{t(($) => $.chat.queue_bar.count, { count })}</span>
        {expanded ? <ChevronUp className="h-3.5 w-3.5" /> : <ChevronDown className="h-3.5 w-3.5" />}
      </button>
      {expanded && (
        <ul className="mt-2 flex flex-col gap-2.5" data-testid="project-queue-bar-list">
          {(data?.items ?? []).map((item) => (
            <QueueBarItem
              key={item.task_id}
              item={item}
              currentUserId={currentUserId}
              canConfigure={canConfigure}
              formatRelativeDate={formatRelativeDate}
              onCancel={() => void handleCancel(item.task_id)}
              isCancelling={
                cancelMutation.isPending && cancelMutation.variables === item.task_id
              }
            />
          ))}
        </ul>
      )}
    </div>
  );
}

type ProjectsT = ReturnType<typeof useT<"projects">>["t"];

function statusLabel(t: ProjectsT, status: string): string {
  switch (status) {
    case "queued":
      return t(($) => $.chat.queue_bar.status_queued);
    case "dispatched":
      return t(($) => $.chat.queue_bar.status_dispatched);
    default:
      // Items are server-filtered to queued/dispatched (DD-3); fall back to
      // the raw value rather than hiding an unexpected status.
      return status;
  }
}

function QueueBarItem({
  item,
  currentUserId,
  canConfigure,
  formatRelativeDate,
  onCancel,
  isCancelling,
}: {
  item: QueueItem;
  currentUserId?: string;
  canConfigure: boolean;
  formatRelativeDate: (date: string) => string;
  onCancel: () => void;
  isCancelling: boolean;
}) {
  const { t } = useT("projects");
  const originator = item.originator;
  const name = originator?.name || t(($) => $.chat.queue_bar.system_task);
  const summary = item.summary || t(($) => $.chat.queue_bar.no_summary);
  const initials = (originator?.name ?? "")
    .split(" ")
    .filter(Boolean)
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);
  const canCancel =
    canConfigure || (currentUserId != null && originator?.id === currentUserId);

  return (
    <li className="flex items-start gap-2.5" data-testid="project-queue-bar-item">
      <ActorAvatarBase
        name={name}
        initials={initials}
        avatarUrl={resolvePublicFileUrl(originator?.avatar_url ?? null)}
        isSystem={originator === null}
        size="sm"
        className="mt-0.5 shrink-0"
      />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5 text-xs">
          <span className="truncate font-medium text-muted-foreground">{name}</span>
          <span className="shrink-0 text-muted-foreground/70">
            {statusLabel(t, item.status)}
          </span>
          <span className="shrink-0 tabular-nums text-muted-foreground/70">
            {formatRelativeDate(item.created_at)}
          </span>
        </div>
        <div className="truncate text-sm text-foreground/85">{summary}</div>
      </div>
      {canCancel && (
        <Button
          type="button"
          variant="ghost"
          size="xs"
          data-testid="project-queue-bar-cancel"
          disabled={isCancelling}
          onClick={onCancel}
        >
          {t(($) => $.chat.queue_bar.cancel)}
        </Button>
      )}
    </li>
  );
}
