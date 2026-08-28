"use client";

import { useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  ShieldAlert,
  XCircle,
} from "lucide-react";
import { ApiError } from "@multica/core/api";
import { useApproveCr } from "@multica/core/projects";
import type { GateNode, ProjectGateCR } from "@multica/core/api/schemas";
import { Button } from "@multica/ui/components/ui/button";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

// Gate-node stream items (CR-2026-011 TASK-06, SDD §5.2). One
// pipeline_node_run row = one card variant:
//   - human_approval, running -> ApprovalCard (batch/reject affordance)
//   - blocked (a review stage's failed round) -> BlockedCard (blocker list)
//   - everything else (passed / failed) -> HistoryRow (collapsed single line)
// Read-only for the CR's status itself — the only write path here is
// POST .../approve, which the server (not this component) turns into a
// status change via the signed-grant + crctl flow.

type StageKey = "requirement" | "tech-design" | "dev-start" | "code";
const KNOWN_STAGES = new Set<string>(["requirement", "tech-design", "dev-start", "code"]);
function asStageKey(stage: string): StageKey | null {
  return KNOWN_STAGES.has(stage) ? (stage as StageKey) : null;
}

function useStageLabel() {
  const { t } = useT("projects");
  return (stage: string) => {
    const key = asStageKey(stage);
    return key ? t(($) => $.governance.stage[key]) : stage;
  };
}

interface GateBlocker {
  id?: string;
  location?: string;
  issue?: string;
  suggestion?: string;
}

function parseBlockers(detail: unknown): GateBlocker[] {
  if (!detail || typeof detail !== "object") return [];
  const blockers = (detail as { blockers?: unknown }).blockers;
  return Array.isArray(blockers) ? (blockers as GateBlocker[]) : [];
}

export function CrGateCard({
  cr,
  node,
  wsId,
  projectId,
}: {
  cr: ProjectGateCR;
  node: GateNode;
  wsId: string;
  projectId: string;
}) {
  if (node.kind === "human_approval" && node.status === "running") {
    return <ApprovalCard cr={cr} wsId={wsId} projectId={projectId} />;
  }
  if (node.status === "blocked") {
    return <BlockedCard cr={cr} node={node} />;
  }
  return <HistoryRow cr={cr} node={node} />;
}

// ─── Approval card (pending human_approval gate) ──────────────────────────
// CR-2026-053 TASK-07 (FR-B6): exported so the stream view can render it
// directly whenever cr.pending_stage is non-empty — even when no
// human_approval/running gate node exists (the CR is at an approval gate but
// the node projection is missing). Consumes the same fields as before
// (cr.cr_id / cr.pending_stage / cr.can_approve / cr.evidence /
// cr.evidence_digest / cr.pending_advance); approve/reject still go through
// the existing API, no GateNode is fabricated.

export function ApprovalCard({
  cr,
  wsId,
  projectId,
}: {
  cr: ProjectGateCR;
  wsId: string;
  projectId: string;
}) {
  const { t } = useT("projects");
  const stageLabel = useStageLabel();
  const { mutateAsync, isPending } = useApproveCr(wsId, projectId);
  const [rejecting, setRejecting] = useState(false);
  const [reason, setReason] = useState("");
  const [errorMsg, setErrorMsg] = useState<string | null>(null);

  const stage = cr.pending_stage;

  const submit = async (decision: "approve" | "reject") => {
    setErrorMsg(null);
    try {
      await mutateAsync({
        crId: cr.cr_id,
        stage,
        decision,
        reject_reason: decision === "reject" ? reason : undefined,
        evidence_digest: cr.evidence_digest || undefined,
      });
      setRejecting(false);
      setReason("");
    } catch (e) {
      if (e instanceof ApiError) {
        const body = e.body as { error?: string; expected?: string; current?: string } | undefined;
        if (body?.error === "EVIDENCE_DRIFT") {
          setErrorMsg(t(($) => $.governance.evidence_drift));
          return;
        }
        if (e.status === 403) {
          setErrorMsg(t(($) => $.governance.forbidden_approver));
          return;
        }
      }
      setErrorMsg(t(($) => $.governance.submit_failed));
    }
  };

  return (
    <div
      className="rounded-lg border border-border bg-card px-4 py-3 text-sm"
      data-testid="cr-gate-approval-card"
    >
      <div className="mb-1.5 flex items-center gap-2">
        <ShieldAlert className="h-4 w-4 shrink-0 text-amber-600" />
        <span className="font-medium text-muted-foreground">{cr.cr_id}</span>
        <span className="text-muted-foreground/70">·</span>
        <span className="font-medium">{stageLabel(stage)}</span>
      </div>

      {cr.needs_reconcile && (
        <div className="mb-2 text-xs text-amber-600" data-testid="cr-gate-needs-reconcile">
          {t(($) => $.governance.needs_reconcile)}
        </div>
      )}

      {Object.keys(cr.evidence).length > 0 && (
        <div className="mb-2.5 text-xs text-muted-foreground">
          <div className="font-medium">{t(($) => $.governance.evidence_label)}</div>
          <ul className="mt-0.5 space-y-0.5">
            {Object.entries(cr.evidence).map(([path, digest]) => (
              <li key={path} className="truncate font-mono">
                {path} · {digest.replace(/^sha256:/, "").slice(0, 12)}
              </li>
            ))}
          </ul>
          {cr.evidence_digest && (
            <div className="mt-0.5 font-mono">
              digest: {cr.evidence_digest.slice(0, 12)}
            </div>
          )}
        </div>
      )}

      {errorMsg && (
        <div
          className="mb-2 rounded-md border border-destructive/30 bg-destructive/5 px-2.5 py-1.5 text-xs text-destructive"
          data-testid="cr-gate-error"
        >
          {errorMsg}
        </div>
      )}

      {cr.pending_advance ? (
        <div className="text-xs text-muted-foreground" data-testid="cr-gate-pending-advance">
          {t(($) => $.governance.pending_advance)}
        </div>
      ) : !cr.can_approve ? (
        <div className="text-xs text-muted-foreground" data-testid="cr-gate-readonly">
          {t(($) => $.governance.waiting_for_approval, { stage: stageLabel(stage) })}
        </div>
      ) : rejecting ? (
        <div className="space-y-2" data-testid="cr-gate-reject-form">
          <Textarea
            autoFocus
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder={t(($) => $.governance.reject_reason_placeholder)}
            className="min-h-16 text-xs"
          />
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              disabled={isPending}
              onClick={() => {
                setRejecting(false);
                setReason("");
              }}
            >
              {t(($) => $.governance.cancel)}
            </Button>
            <Button
              type="button"
              variant="destructive"
              size="sm"
              disabled={isPending || !reason.trim()}
              onClick={() => void submit("reject")}
            >
              {t(($) => $.governance.reject_confirm)}
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex justify-end gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={isPending}
            onClick={() => setRejecting(true)}
          >
            {t(($) => $.governance.reject)}
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={isPending}
            onClick={() => void submit("approve")}
          >
            {t(($) => $.governance.approve)}
          </Button>
        </div>
      )}
    </div>
  );
}

// ─── Blocked review card ───────────────────────────────────────────────────

function BlockedCard({ cr, node }: { cr: ProjectGateCR; node: GateNode }) {
  const { t } = useT("projects");
  const blockers = parseBlockers(node.detail);

  return (
    <div
      className="rounded-lg border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm"
      data-testid="cr-gate-blocked-card"
    >
      <div className="mb-1.5 flex items-center gap-2">
        <AlertTriangle className="h-4 w-4 shrink-0 text-destructive" />
        <span className="font-medium text-muted-foreground">{cr.cr_id}</span>
        <span className="text-muted-foreground/70">·</span>
        <span className="font-medium text-destructive">
          {t(($) => $.governance.review_blocked)}
        </span>
        <span className="ml-auto text-xs tabular-nums text-muted-foreground">
          {t(($) => $.governance.attempt_label, { attempt: node.attempt })}
        </span>
      </div>
      {blockers.length > 0 && (
        <ul className="space-y-1.5 text-xs text-muted-foreground">
          {blockers.map((b, i) => (
            <li key={b.id ?? i}>
              {b.location && <span className="font-medium">{b.location}: </span>}
              {b.issue}
              {b.suggestion && (
                <div className="text-muted-foreground/70">{b.suggestion}</div>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// ─── History row (passed / failed — collapsed single line) ────────────────

function HistoryRow({ cr, node }: { cr: ProjectGateCR; node: GateNode }) {
  const { t } = useT("projects");
  const stageLabel = useStageLabel();
  const [open, setOpen] = useState(false);
  const blockers = parseBlockers(node.detail);

  const passed = node.status === "passed";
  const nodeStageKey = asStageKey(node.stage);
  // A failed node (rejected/withdrawn path) always reads "Cancelled" — the
  // stage name only appears for a node that actually PASSED (checking kind
  // first without checking `passed` would mislabel a failed human_approval
  // node with just its stage name, indistinguishable from a passed one).
  const label = !passed
    ? t(($) => $.governance.node_cancelled)
    : node.kind === "human_approval" && nodeStageKey
      ? stageLabel(nodeStageKey)
      : t(($) => $.governance.review_passed);

  return (
    <div
      className="flex flex-col gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground"
      data-testid="cr-gate-history-row"
    >
      <button
        type="button"
        className="flex items-center gap-1.5 text-left"
        onClick={() => setOpen((v) => !v)}
        disabled={blockers.length === 0}
      >
        {blockers.length > 0 ? (
          open ? (
            <ChevronDown className="h-3 w-3 shrink-0" />
          ) : (
            <ChevronRight className="h-3 w-3 shrink-0" />
          )
        ) : (
          <span className="w-3 shrink-0" />
        )}
        {passed ? (
          <CheckCircle2 className="h-3 w-3 shrink-0 text-emerald-600" />
        ) : (
          <XCircle className="h-3 w-3 shrink-0 text-muted-foreground" />
        )}
        <span className="font-medium text-muted-foreground/90">{cr.cr_id}</span>
        <span>{label}</span>
        {node.attempt > 1 && (
          <span className={cn("tabular-nums")}>
            {t(($) => $.governance.attempt_label, { attempt: node.attempt })}
          </span>
        )}
      </button>
      {open && blockers.length > 0 && (
        <ul className="ml-4 space-y-1">
          {blockers.map((b, i) => (
            <li key={b.id ?? i}>
              {b.location && <span className="font-medium">{b.location}: </span>}
              {b.issue}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
