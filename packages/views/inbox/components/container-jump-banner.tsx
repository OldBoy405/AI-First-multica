"use client";

import { ArrowRight, MessagesSquare } from "lucide-react";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";

/**
 * A Team Agent chat / Discussion notification's `issue_id` points at a
 * hidden system container issue (CR-2026-006/CR-2026-009) — the inbox can
 * still resolve and preview it by id (containers are only excluded from
 * *listing* surfaces, not direct-id fetches), but rendering IssueDetail for
 * one would show a bare, meaningless "Issue" page for something that was
 * never meant to be seen as one. This banner replaces that preview with a
 * jump link to the actual project chat panel tab the message lives in.
 */
export function ContainerJumpBanner({
  projectHref,
  mode,
}: {
  projectHref: string;
  mode: "team_agent" | "discussion";
}) {
  const { t } = useT("inbox");
  return (
    <div className="flex h-full flex-col items-center justify-center gap-3 p-6 text-center">
      <MessagesSquare className="h-8 w-8 text-muted-foreground/50" />
      <p className="text-sm text-muted-foreground">
        {t(($) => $.container_jump[mode])}
      </p>
      <AppLink
        href={projectHref}
        className="inline-flex items-center gap-1.5 text-sm font-medium text-brand hover:underline"
      >
        {t(($) => $.container_jump.cta)}
        <ArrowRight className="h-3.5 w-3.5" />
      </AppLink>
    </div>
  );
}
