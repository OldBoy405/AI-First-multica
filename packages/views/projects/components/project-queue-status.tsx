"use client";

import { useQuery } from "@tanstack/react-query";
import { projectQueueStatusOptions } from "@multica/core/projects/queries";
import { useT } from "../../i18n";

// Shared Team Agent queue indicator (CR-2026-004 FR-5). Shows the project's
// live queued+dispatched depth against its capacity limit; turns amber when
// full so members know new agent requests will be rejected. Kept live by the
// realtime task:* prefix invalidation — no polling.
//
// The disabled/rejected treatment for actual enqueue inputs lives at each
// enqueue surface (e.g. the quick-create modal branches on the 429
// project_queue_full response); this component is the always-visible signal.
export function ProjectQueueStatus({ wsId, projectId }: { wsId: string; projectId: string }) {
  const { t } = useT("projects");
  const { data } = useQuery(projectQueueStatusOptions(wsId, projectId));
  if (!data) return null;

  const full = data.queue_depth >= data.queue_limit;
  return (
    <div className="pl-2 flex items-center gap-2" data-testid="project-queue-status">
      <span
        className={`text-xs tabular-nums ${full ? "text-amber-600 font-medium" : "text-muted-foreground"}`}
      >
        {t(($) => $.detail.queue_depth, {
          depth: data.queue_depth,
          limit: data.queue_limit,
        })}
      </span>
      {full && (
        <span className="text-xs text-amber-600" data-testid="project-queue-full-hint">
          {t(($) => $.detail.queue_full_hint)}
        </span>
      )}
    </div>
  );
}
