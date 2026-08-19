"use client";

import { useQuery } from "@tanstack/react-query";
import { useWorkspacePaths } from "@multica/core/paths";
import {
  maturitySuggestionHistoryOptions,
  maturitySuggestionsOptions,
} from "@multica/core/maturity";
import { Skeleton } from "@multica/ui/components/ui/skeleton";

// AIFIRST: weekly report suggestions (CR-2026-047 TASK-09). Renders the
// latest report envelope (markdown) and the ISO-week history; the "follow up"
// button deep-links the existing Team Agent chat via ?session=chat_session_id
// (packages/views/chat/chat-page.tsx contract) — no new chat UI.

export function MaturitySuggestionsPanel({ wsId }: { wsId: string }) {
  const wsPaths = useWorkspacePaths();
  const latest = useQuery(maturitySuggestionsOptions(wsId));
  const history = useQuery(maturitySuggestionHistoryOptions(wsId, 12));

  if (latest.isLoading) {
    return <Skeleton className="h-24 w-full" data-testid="suggestions-loading" />;
  }
  const report = latest.data?.latest;

  return (
    <section className="space-y-3" data-testid="maturity-suggestions">
      <h2 className="text-lg font-medium">AI-native org suggestions</h2>
      {report ? (
        <div className="rounded-md border p-4">
          <div className="mb-2 flex items-center justify-between">
            <span className="font-medium">Week {report.week}</span>
            <a
              className="text-sm underline"
              data-testid="suggestions-follow-up"
              href={`${wsPaths.chat()}?session=${report.chat_session_id}`}
            >
              Follow up in chat
            </a>
          </div>
          <pre className="whitespace-pre-wrap text-sm">{report.markdown}</pre>
        </div>
      ) : (
        <div className="rounded-md border p-4 text-muted-foreground" data-testid="suggestions-empty">
          {latest.data?.data_status === "empty"
            ? "No weekly report yet — Org Admin workspace must be initialised and bound to a local directory."
            : "Report unavailable."}
        </div>
      )}

      {history.data && history.data.items.length > 0 && (
        <div className="space-y-2" data-testid="suggestions-history">
          <h3 className="text-sm font-medium text-muted-foreground">History</h3>
          {history.data.items.map((item) => (
            <div key={item.report_key} className="rounded-md border p-3 text-sm">
              <div className="flex items-center justify-between">
                <span>Week {item.week}</span>
                <a
                  className="underline"
                  href={`${wsPaths.chat()}?session=${item.chat_session_id}`}
                >
                  Follow up
                </a>
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
