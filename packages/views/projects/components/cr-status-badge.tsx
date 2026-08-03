"use client";

import { useQuery } from "@tanstack/react-query";
import { projectGatesOptions } from "@multica/core/projects/queries";
import { Badge } from "@multica/ui/components/ui/badge";
import {
  Popover,
  PopoverTrigger,
  PopoverContent,
} from "@multica/ui/components/ui/popover";
import { useT } from "../../i18n";

// 16-state CR badge for the project chat window (CR-2026-011 TASK-05, SDD
// DD-7). Read-only — the "no reverse" board rule (P0 §4.1: dragging a CR
// shell issue on the kanban must not change CR status) applies here too, so
// this component exposes no status-change affordance at all.

// The 7-bucket board grouping (P0 §4.1) reused for the badge's color weight,
// keyed by the exact 15 status strings (governance.KnownStatuses on the Go
// side — the platform docs' "16 态" phrasing predates a state that no longer
// exists; the state machine has 15).
type StatusBucket = "todo" | "in_progress" | "in_review" | "done" | "cancelled";

// `satisfies` (not a `: Record<string, StatusBucket>` annotation) keeps the
// object's literal keys intact for StatusKey below — an explicit Record
// annotation would widen them all to `string` and break the t() indexing.
const STATUS_BUCKET = {
  "drafting": "todo",
  "requirement-reviewing": "todo",
  "requirement-approved": "in_progress",
  "tech-designing": "in_progress",
  "tech-design-review-pending": "in_progress",
  "tech-design-reviewed": "in_progress",
  "task-breakdown": "in_progress",
  "developing": "in_progress",
  "code-reviewing": "in_review",
  "code-approved": "in_review",
  "merging": "in_review",
  "writing-back": "in_review",
  "archived": "done",
  "rejected": "cancelled",
  "withdrawn": "cancelled",
} satisfies Record<string, StatusBucket>;

const BUCKET_CLASS: Record<StatusBucket, string> = {
  todo: "bg-muted text-muted-foreground",
  in_progress: "bg-blue-500/10 text-blue-600 dark:text-blue-400",
  in_review: "bg-amber-500/10 text-amber-600 dark:text-amber-400",
  done: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400",
  cancelled: "bg-destructive/10 text-destructive",
};

// governance.status keys (projects.json) — a narrow union so both the
// bucket lookup and the useT selector below can index safely. asStatusKey()
// is the untyped-wire-string -> union boundary (an unrecognized status falls
// back to the raw string / the "todo" bucket, CLAUDE.md's "default
// server-driven fields defensively" rule).
type StatusKey = keyof typeof STATUS_BUCKET;
const KNOWN_STATUSES = new Set<string>(Object.keys(STATUS_BUCKET));
function asStatusKey(status: string): StatusKey | null {
  return KNOWN_STATUSES.has(status) ? (status as StatusKey) : null;
}

function bucketFor(status: string): StatusBucket {
  const key = asStatusKey(status);
  return key ? STATUS_BUCKET[key] : "todo";
}

function useStatusLabel() {
  const { t } = useT("projects");
  return (status: string) => {
    const key = asStatusKey(status);
    if (!key) return status;
    return t(($) => $.governance.status[key]);
  };
}

function StatusBadge({ status }: { status: string }) {
  const statusLabel = useStatusLabel();
  const bucket = bucketFor(status);
  return (
    <Badge
      variant="outline"
      className={`border-transparent ${BUCKET_CLASS[bucket]}`}
    >
      {statusLabel(status)}
    </Badge>
  );
}

export function CrStatusBadge({
  wsId,
  projectId,
}: {
  wsId: string;
  projectId: string;
}) {
  const { t } = useT("projects");
  const { data: crs } = useQuery(projectGatesOptions(wsId, projectId));

  if (!crs || crs.length === 0) return null;

  // DD-7: the most recently updated in-flight CR drives the badge; a
  // multi-CR popover lists the rest (does not swap the primary badge on
  // hover — clicking is the only way to see the others).
  const sorted = [...crs].sort((a, b) => b.updated_at.localeCompare(a.updated_at));
  const primary = sorted[0]!;

  const trigger = (
    <span
      className="inline-flex items-center gap-1.5 cursor-default"
      data-testid="cr-status-badge"
    >
      <span className="text-xs font-medium text-muted-foreground">
        {primary.cr_id}
      </span>
      <StatusBadge status={primary.status} />
      {primary.needs_reconcile && (
        <span
          className="text-xs text-amber-600"
          data-testid="cr-needs-reconcile"
        >
          {t(($) => $.governance.needs_reconcile)}
        </span>
      )}
    </span>
  );

  if (sorted.length === 1) return trigger;

  return (
    <Popover>
      <PopoverTrigger className="cursor-pointer" data-testid="cr-status-badge-trigger">
        {trigger}
      </PopoverTrigger>
      <PopoverContent align="end" className="w-64 p-2">
        <div className="mb-1.5 px-1 text-xs text-muted-foreground">
          {t(($) => $.governance.multiple_active, { count: sorted.length })}
        </div>
        <div className="flex flex-col gap-1">
          {sorted.map((cr) => (
            <div
              key={cr.cr_id}
              className="flex items-center justify-between gap-2 rounded px-1 py-1 text-sm"
            >
              <span className="truncate text-muted-foreground">{cr.cr_id}</span>
              <StatusBadge status={cr.status} />
            </div>
          ))}
        </div>
      </PopoverContent>
    </Popover>
  );
}
