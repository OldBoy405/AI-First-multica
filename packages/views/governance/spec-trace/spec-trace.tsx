"use client";

// AIFIRST: CR-2026-049 TASK-12 — spec trace timeline view (SDD §3.5 FR-6/FR-7).
// Rendering contract: baseline-imported history renders before event entries
// (document order), event entries follow ordered by (occurred_at, id);
// trace_snapshot_conflict renders a conflict badge; missing evidence renders
// an explicit "missing" marker (never falls back to trunk HEAD); one-hop
// commit links use only {repo,trunk,sha} from merge_commits.

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core";
import { specTraceOptions } from "@multica/core/trace/queries";
import type { TraceEventItem, TraceMilestoneView } from "@multica/core/types";
import { AlertTriangle } from "lucide-react";

const conflictLabel = "snapshot conflict";
const evidenceMissingLabel = "evidence: missing";
const evidencePresentLabel = "evidence: recorded";
const baselineImportedLabel = "baseline-imported";

function firstCommitLink(milestone: TraceMilestoneView): { repo: string; trunk: string; sha: string } | null {
  const merges = Array.isArray(milestone.mergeCommits) ? milestone.mergeCommits : [];
  for (const m of merges) {
    const row = m as { repo?: unknown; trunk?: unknown; sha?: unknown };
    if (typeof row.repo === "string" && typeof row.trunk === "string" && typeof row.sha === "string") {
      return { repo: row.repo, trunk: row.trunk, sha: row.sha };
    }
  }
  return null;
}

function commitHref(repo: string, _trunk: string, sha: string): string | null {
  const value = repo.trim().replace(/\.git$/, "").replace(/\/$/, "");
  if (/^https?:\/\/github\.com\/[^/]+\/[^/]+$/.test(value)) return `${value}/commit/${sha}`;
  if (/^[^/]+\/[^/]+$/.test(value)) return `https://github.com/${value}/commit/${sha}`;
  return null;
}

function evidenceLinks(evidence: unknown): Array<{ label: string; href: string }> {
  if (typeof evidence !== "object" || evidence === null || Array.isArray(evidence)) return [];
  return Object.entries(evidence as Record<string, unknown>)
    .filter(([, value]) => typeof value === "string" && /^https?:\/\//.test(value))
    .map(([label, value]) => ({ label, href: value as string }));
}

export function MilestoneRow({ milestone, event }: { milestone: TraceMilestoneView; event?: TraceEventItem }) {
  const link = firstCommitLink(milestone);
  return (
    <div data-testid="milestone-row" data-source={milestone.source}>
      <div className="flex items-center gap-2">
        <span className="font-medium">{milestone.cr}</span>
        <span className="text-muted-foreground">{milestone.milestone}</span>
        {milestone.traceSnapshotConflict && (
          <span data-testid="conflict-badge" className="inline-flex items-center gap-1 text-destructive">
            <AlertTriangle className="h-3 w-3" /> {conflictLabel}
          </span>
        )}
        {milestone.source === "baseline-imported" && <span className="text-caption text-muted-foreground">{baselineImportedLabel}</span>}
      </div>
      {Array.isArray(milestone.frs) && (
        <ul data-testid="frs" className="ml-4 list-disc text-body">
          {milestone.frs.map((fr, i) => (
            <li key={i}>{String((fr as { fr?: unknown })?.fr ?? "")}</li>
          ))}
        </ul>
      )}
      {milestone.evidence == null ? (
        <span data-testid="evidence-missing" className="text-caption text-muted-foreground">
          {evidenceMissingLabel}
        </span>
      ) : (
        <span data-testid="evidence-present" className="text-caption text-muted-foreground">
          {evidencePresentLabel}
          {evidenceLinks(milestone.evidence).map(({ label, href }) => (
            <a key={label} data-testid={`evidence-link-${label}`} href={href} className="ml-2 underline">
              {label}
            </a>
          ))}
        </span>
      )}
      {link && commitHref(link.repo, link.trunk, link.sha) && (
        <a data-testid="commit-link" href={commitHref(link.repo, link.trunk, link.sha)!} className="text-caption underline">
          {link.repo}@{link.sha.slice(0, 8)}
        </a>
      )}
      {event && event.state === "malformed" && (
        <span data-testid="malformed-badge" className="text-caption text-destructive">
          {event.errorCode ?? "trace_payload_invalid"}
        </span>
      )}
    </div>
  );
}

export function SpecTraceTimeline({ specId }: { specId: string }) {
  const wsId = useWorkspaceId();
  const timeline = useQuery(specTraceOptions(wsId, specId));
  const events = timeline.data?.events ?? [];
  if (timeline.isLoading) {
    const loadingLabel = "loading";
    return <div data-testid="loading">{loadingLabel}</div>;
  }
  return (
    <div data-testid="spec-trace-timeline">
      {events.map((event, i) =>
        event.milestone ? (
          <MilestoneRow key={`${event.crId}:${i}`} milestone={event.milestone} event={event} />
        ) : (
          <div key={`${event.eventId}:${i}`} data-testid="malformed-row">
            {event.crId} — {event.state} {event.errorCode ?? ""}
          </div>
        ),
      )}
    </div>
  );
}

export function SpecTracePage({ specId }: { specId: string }) {
  const title = `Spec ${specId}`;
  return (
    <div className="p-6">
      <h1 data-testid="spec-title">{title}</h1>
      <SpecTraceTimeline specId={specId} />
    </div>
  );
}
