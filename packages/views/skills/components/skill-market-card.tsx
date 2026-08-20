"use client";

// AIFIRST: CR-2026-048 TASK-09: org-publish card on the skill detail page.
// Publish = grant: the confirm dialog states it. Blocked publishes surface the
// gate findings with per-item appeal actions; owners may approve/reject.

import { useState } from "react";
import { Globe, Lock, ShieldAlert } from "lucide-react";
import type {
  Skill,
  SkillGateFinding,
  SkillPublishBlockedBody,
} from "@multica/core/types";
import { api, ApiError } from "@multica/core/api";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Badge } from "@multica/ui/components/ui/badge";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@multica/ui/components/ui/dialog";
import { parseRequirements } from "../lib/skill-metadata";
import { useT } from "../../i18n";

export function SkillMarketCard({
  skill,
  canEdit,
  isAdmin,
}: {
  skill: Skill;
  canEdit: boolean;
  isAdmin: boolean;
}) {
  const { t } = useT("skills");
  const [publishing, setPublishing] = useState(false);
  const [blocked, setBlocked] = useState<SkillPublishBlockedBody | null>(null);
  const [open, setOpen] = useState(false);
  const isOrg = skill.visibility === "org";

  const publish = async () => {
    setPublishing(true);
    try {
      await api.updateSkill(skill.id, { visibility: "org" });
      toast.success(t(($) => $.market.publish_ok));
      setBlocked(null);
      setOpen(false);
    } catch (err) {
      if (err instanceof ApiError && err.status === 422 && err.body) {
        setBlocked(err.body as SkillPublishBlockedBody);
      } else {
        toast.error(err instanceof Error ? err.message : String(err));
      }
    } finally {
      setPublishing(false);
    }
  };

  const appeal = async (f: SkillGateFinding) => {
    try {
      await api.submitSkillAppeal(skill.id, {
        appeal_id: f.appeal_id,
        file: f.file,
        line: f.line,
        pattern_id: f.pattern_id,
      });
      toast.success(t(($) => $.market.appeal_submitted));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  const decide = async (f: SkillGateFinding, approve: boolean) => {
    try {
      await api.decideSkillAppeal(skill.id, { appeal_id: f.appeal_id, approve });
      toast.success(t(($) => $.market.decided));
      await publish();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="space-y-2">
    <div className="flex flex-wrap items-center gap-2">
      {isOrg ? (
        <Badge variant="secondary" className="gap-1">
          <Globe className="h-3 w-3" />
          {t(($) => $.market.visibility_org)}
        </Badge>
      ) : (
        <Badge variant="outline" className="gap-1">
          <Lock className="h-3 w-3" />
          {t(($) => $.market.visibility_private)}
        </Badge>
      )}
      {skill.version && (
        <span className="text-caption text-muted-foreground">
          {t(($) => $.market.version_label)} {skill.version}
        </span>
      )}
      {canEdit && (
        <Dialog open={open} onOpenChange={setOpen}>
          <DialogTrigger
            render={
              <Button variant="outline" size="xs" disabled={isOrg} className="gap-1">
                <Globe className="h-3 w-3" />
                {t(($) => $.market.publish)}
              </Button>
            }
          />
          <DialogContent>
            <DialogHeader>
              <DialogTitle>{t(($) => $.market.publish_confirm_title)}</DialogTitle>
            </DialogHeader>
            <p className="text-body text-muted-foreground">
              {t(($) => $.market.publish_confirm_body)}
            </p>
            {blocked && (
              <div className="space-y-2 rounded-md border border-destructive/40 bg-destructive/5 p-3">
                <div className="flex items-center gap-2 text-destructive">
                  <ShieldAlert className="h-4 w-4" />
                  <span className="text-caption font-medium">
                    {t(($) => $.market.publish_blocked_title)}
                  </span>
                </div>
                <ul className="list-disc pl-5 text-caption">
                  {(blocked.reasons ?? []).map((r) => (
                    <li key={r}>{r}</li>
                  ))}
                  {(blocked.findings ?? []).map((f) => (
                    <li key={f.appeal_id}>
                      <span className="font-mono">
                        {f.file}:{f.line} {f.pattern_id}
                      </span>
                      <span className="block text-muted-foreground">{f.excerpt}</span>
                      <span className="flex gap-2 pt-1">
                        <Button variant="outline" size="xs" onClick={() => appeal(f)}>
                          {t(($) => $.market.appeal)}
                        </Button>
                        {isAdmin && (
                          <>
                            <Button variant="outline" size="xs" onClick={() => decide(f, true)}>
                              {t(($) => $.market.approve)}
                            </Button>
                            <Button variant="ghost" size="xs" onClick={() => decide(f, false)}>
                              {t(($) => $.market.reject)}
                            </Button>
                          </>
                        )}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
            <DialogFooter>
              <Button onClick={publish} disabled={publishing}>
                {t(($) => $.market.publish_confirm)}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </div>
    {isOrg && <SkillMetadataCard metadata={skill.metadata} />}
    </div>
  );
}

// AIFIRST: CR-2026-048 TASK-09 (FR-20/FR-21): the metadata card and runtime
// requirement tags for an org-visible skill. Values come from the frontmatter
// parsed server-side (`skill.metadata`); unknown or missing fields are simply
// not rendered, never an error.
const CARD_FIELDS = [
  ["applicable-scenarios", "applicable_scenarios"],
  ["context-dependencies", "context_dependencies"],
  ["permission-declaration", "permission_declaration"],
  ["failure-handling", "failure_handling"],
] as const;

export function SkillMetadataCard({ metadata }: { metadata?: Record<string, string> }) {
  const { t } = useT("skills");
  if (!metadata) return null;
  const rows = CARD_FIELDS.filter(([key]) => metadata[key]);
  const requirements = parseRequirements(metadata.requirements);
  if (rows.length === 0 && requirements.length === 0) return null;
  return (
    <div className="rounded-md border p-3">
      <div className="mb-2 text-caption font-medium">{t(($) => $.market.card_title)}</div>
      <dl className="space-y-1">
        {rows.map(([key, label]) => (
          <div key={key} className="flex gap-2 text-caption">
            <dt className="shrink-0 text-muted-foreground">{t(($) => $.market[label])}</dt>
            <dd className="min-w-0 break-words">{metadata[key]}</dd>
          </div>
        ))}
      </dl>
      {requirements.length > 0 && (
        <div className="mt-2 flex flex-wrap items-center gap-1">
          <span className="text-caption text-muted-foreground">
            {t(($) => $.market.requirements)}
          </span>
          {requirements.map((r) => (
            <Badge key={r} variant="outline">
              {r}
            </Badge>
          ))}
        </div>
      )}
    </div>
  );
}
