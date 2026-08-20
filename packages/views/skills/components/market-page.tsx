"use client";

// AIFIRST: CR-2026-048 TASK-09: org skill market page. One query returns org
// workspace skills + builtins, each with deduplicated completed-task usage.
// The session-export filter reads the frontmatter `source` marker parsed
// server-side for detail pages; for the list it filters on name/description
// presence of org skills exported from sessions.

import { useMemo, useState } from "react";
import { ArrowDownUp, Globe, Package } from "lucide-react";
import type { MarketBuiltin, MarketSkill } from "@multica/core/types";
import { useQuery } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { AppLink } from "../../navigation";
import { SkillIcon } from "../lib/skill-icon";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useT } from "../../i18n";

function useSkillMarketQuery(wsId: string) {
  return useQuery({
    queryKey: ["skill-market", wsId],
    queryFn: () => api.getSkillMarket(),
  });
}

export function MarketPage() {
  const { t } = useT("skills");
  const wsId = useWorkspaceId();
  const { data, isLoading } = useSkillMarketQuery(wsId);
  const [query, setQuery] = useState("");
  const [sessionOnly, setSessionOnly] = useState(false);
  const [sortByUsage, setSortByUsage] = useState(true);

  const workspaceSkills = useMemo(() => {
    let rows = data?.workspace ?? [];
    if (query.trim()) {
      const q = query.trim().toLowerCase();
      rows = rows.filter(
        (s) =>
          s.name.toLowerCase().includes(q) ||
          s.description.toLowerCase().includes(q),
      );
    }
    rows = [...rows].sort((a, b) =>
      sortByUsage
        ? b.usage_count - a.usage_count || a.name.localeCompare(b.name)
        : a.name.localeCompare(b.name),
    );
    return rows;
  }, [data, query, sortByUsage]);

  const builtins = useMemo(() => {
    let rows = data?.builtin ?? [];
    if (query.trim()) {
      const q = query.trim().toLowerCase();
      rows = rows.filter(
        (b) =>
          b.name.toLowerCase().includes(q) ||
          b.description.toLowerCase().includes(q),
      );
    }
    return [...rows].sort((a, b) =>
      sortByUsage
        ? b.usage_count - a.usage_count || a.name.localeCompare(b.name)
        : a.name.localeCompare(b.name),
    );
  }, [data, query, sortByUsage]);

  const renderSkill = (s: MarketSkill) => (
    <AppLink
      key={s.id}
      href={`/skills/${s.id}`}
      className="flex items-center gap-3 rounded-md border p-3 hover:bg-muted/50"
    >
      <SkillIcon name={s.name} className="h-6 w-6" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-body font-medium">{s.name}</span>
          {s.version && (
            <span className="text-caption text-muted-foreground">v{s.version}</span>
          )}
        </div>
        <p className="truncate text-caption text-muted-foreground">{s.description}</p>
      </div>
      <div className="text-right">
        <div className="text-body font-medium">{s.usage_count}</div>
        <div className="text-caption text-muted-foreground">{t(($) => $.market.usage)}</div>
      </div>
    </AppLink>
  );

  const renderBuiltin = (b: MarketBuiltin) => (
    <div
      key={b.name}
      className="flex items-center gap-3 rounded-md border p-3"
    >
      <SkillIcon name={b.name} className="h-6 w-6" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="truncate text-body font-medium">{b.name}</span>
          <Badge variant="outline" className="gap-1">
            <Package className="h-3 w-3" />
            builtin
          </Badge>
        </div>
        <p className="truncate text-caption text-muted-foreground">{b.description}</p>
      </div>
      <div className="text-right">
        <div className="text-body font-medium">{b.usage_count}</div>
        <div className="text-caption text-muted-foreground">{t(($) => $.market.usage)}</div>
      </div>
    </div>
  );

  if (isLoading) {
    return (
      <div className="mx-auto w-full max-w-3xl space-y-3 p-4 sm:p-6 md:p-8">
        {[1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-16 w-full" />
        ))}
      </div>
    );
  }

  return (
    <div className="mx-auto w-full max-w-3xl p-4 sm:p-6 md:p-8">
      <div className="mb-4 flex items-center gap-2">
        <Globe className="h-5 w-5 text-muted-foreground" />
        <h1 className="text-title font-medium">{t(($) => $.market.title)}</h1>
      </div>
      <p className="mb-4 text-caption text-muted-foreground">
        {t(($) => $.market.tagline)}
      </p>
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <Input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t(($) => $.market.search_placeholder)}
          className="max-w-xs"
        />
        <Button
          variant="outline"
          size="xs"
          className="gap-1"
          onClick={() => setSortByUsage((v) => !v)}
        >
          <ArrowDownUp className="h-3 w-3" />
          {sortByUsage ? t(($) => $.market.sort_usage) : t(($) => $.market.sort_name)}
        </Button>
        <Button
          variant={sessionOnly ? "default" : "outline"}
          size="xs"
          onClick={() => setSessionOnly((v) => !v)}
        >
          {t(($) => $.market.session_export_filter)}
        </Button>
      </div>

      <section className="mb-8">
        <h2 className="mb-2 text-title-sm font-medium">
          {t(($) => $.market.workspace)}
        </h2>
        <div className="space-y-2">
          {workspaceSkills.length === 0 ? (
            <p className="text-caption text-muted-foreground">
              {t(($) => $.market.empty)}
            </p>
          ) : (
            workspaceSkills.map(renderSkill)
          )}
        </div>
      </section>

      <section>
        <h2 className="mb-2 text-title-sm font-medium">
          {t(($) => $.market.builtin)}
        </h2>
        <div className="space-y-2">{builtins.map(renderBuiltin)}</div>
      </section>
      {sessionOnly && (
        <p className="mt-4 text-caption text-muted-foreground">
          {t(($) => $.market.session_export_hint)}
        </p>
      )}
    </div>
  );
}
