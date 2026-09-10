"use client";

import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspacePaths } from "@multica/core/paths";
import { runtimeDisplayLabel, runtimeListOptions } from "@multica/core/runtimes";
import { memberListOptions } from "@multica/core/workspace/queries";
import {
  maturitySuggestionHistoryOptions,
  maturitySuggestionsOptions,
} from "@multica/core/maturity";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";

// AIFIRST: weekly report suggestions (CR-2026-047 TASK-09). Renders the
// latest report envelope (markdown) and the ISO-week history; the "follow up"
// button deep-links the existing Team Agent chat via ?session=chatSessionId
// (packages/views/chat/chat-page.tsx contract) — no new chat UI.

export function MaturitySuggestionsPanel({ wsId }: { wsId: string }) {
  const { t } = useT("usage");
  const wsPaths = useWorkspacePaths();
  const currentUser = useAuthStore((state) => state.user);
  const latest = useQuery(maturitySuggestionsOptions(wsId));
  const history = useQuery(maturitySuggestionHistoryOptions(wsId, 12));
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));
  const [runtimeId, setRuntimeId] = useState("");
  const onlineRuntimes = runtimes.filter((runtime) => runtime.status === "online");
  const selectedRuntimeId = runtimeId || onlineRuntimes[0]?.id || "";
  const canInitialize = members.some(
    (member) =>
      member.user_id === currentUser?.id &&
      (member.role === "owner" || member.role === "admin"),
  );
  const initialize = useMutation({
    mutationFn: () => api.ensureOrgAdminWorkspace(wsId, selectedRuntimeId),
    onSuccess: () => latest.refetch(),
  });

  if (latest.isLoading) {
    return <Skeleton className="h-24 w-full" data-testid="suggestions-loading" />;
  }
  const report = latest.data?.latest;

  return (
    <section className="space-y-3" data-testid="maturity-suggestions">
      <h2 className="text-title font-medium">{t(($) => $.maturity.suggestions_title)}</h2>
      {report ? (
        <div className="rounded-md border p-4">
          <div className="mb-2 flex items-center justify-between">
            <span className="font-medium">{t(($) => $.maturity.suggestions_week, { week: report.week })}</span>
            <AppLink
              className="text-body underline"
              data-testid="suggestions-follow-up"
              href={`${wsPaths.chat()}?session=${report.chatSessionId}`}
            >
              {t(($) => $.maturity.suggestions_follow_up_chat)}
            </AppLink>
          </div>
          <pre className="whitespace-pre-wrap text-body">{report.markdown}</pre>
        </div>
      ) : (
        <div className="space-y-3 rounded-md border p-4 text-muted-foreground" data-testid="suggestions-empty">
          <p>
            {latest.data?.dataStatus === "empty"
              ? "No weekly report yet — initialise Org Admin, then bind its project to a local directory."
              : "Report unavailable."}
          </p>
          {canInitialize ? (
            <div className="flex flex-wrap items-end gap-2">
              <label className="space-y-1 text-body" htmlFor="maturity-runtime">
                <span className="block text-label text-foreground">{t(($) => $.maturity.runtime_label)}</span>
                <select
                  id="maturity-runtime"
                  value={selectedRuntimeId}
                  onChange={(event) => setRuntimeId(event.target.value)}
                  className="h-9 rounded-md border bg-background px-3 text-foreground"
                >
                  {onlineRuntimes.map((runtime) => (
                    <option key={runtime.id} value={runtime.id}>
                      {runtimeDisplayLabel(runtime)}
                    </option>
                  ))}
                </select>
              </label>
              <Button
                type="button"
                disabled={!selectedRuntimeId || initialize.isPending}
                onClick={() => initialize.mutate()}
              >
                {initialize.isPending ? "Initialising…" : "Initialise Org Admin"}
              </Button>
            </div>
          ) : null}
          {canInitialize && onlineRuntimes.length === 0 ? (
            <p className="text-caption">{t(($) => $.maturity.suggestions_no_runtime)}</p>
          ) : null}
          {initialize.isError ? (
            <p className="text-caption text-destructive">{t(($) => $.maturity.suggestions_init_failed)}</p>
          ) : null}
        </div>
      )}

      {history.data && history.data.items.length > 0 && (
        <div className="space-y-2" data-testid="suggestions-history">
          <h3 className="text-body font-medium text-muted-foreground">{t(($) => $.maturity.suggestions_history)}</h3>
          {history.data.items.map((item) => (
            <div key={item.reportKey} className="rounded-md border p-3 text-body">
              <div className="flex items-center justify-between">
                <span>{t(($) => $.maturity.suggestions_week, { week: item.week })}</span>
                <AppLink
                  className="underline"
                  href={`${wsPaths.chat()}?session=${item.chatSessionId}`}
                >
                  {t(($) => $.maturity.suggestions_follow_up)}
                </AppLink>
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
