"use client";

import { useQuery } from "@tanstack/react-query";
import { Bot, Lock, MessagesSquare, X } from "lucide-react";
import { projectChatOptions } from "@multica/core/projects/queries";
import {
  useProjectChatStore,
  projectChatDraftKey,
  type ProjectChatMode,
} from "@multica/core/projects";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
} from "@multica/ui/components/ui/tabs";
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@multica/ui/components/ui/tooltip";
import { Button } from "@multica/ui/components/ui/button";
import { ProjectTeamAgentChat } from "./project-team-agent-chat";
import { useT } from "../../i18n";

const MODES: readonly ProjectChatMode[] = [
  "team_agent",
  "private_ask",
  "discussion",
] as const;

const MODE_ICON = {
  team_agent: Bot,
  private_ask: Lock,
  discussion: MessagesSquare,
} as const;

/**
 * Project group-chat window (CR-2026-006 TASK-03). Skeleton + state layer only:
 * the Team Agent message stream is TASK-04, and Private Ask / Discussion are
 * empty-state placeholders for a later version.
 */
export function ProjectChatPanel({
  projectId,
  canConfigure,
}: {
  projectId: string;
  /** Owner/admin — may configure the Team Agent. */
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const activeMode =
    useProjectChatStore((s) => s.activeMode[projectId]) ?? "team_agent";
  const setActiveMode = useProjectChatStore((s) => s.setActiveMode);

  return (
    <Tabs
      value={activeMode}
      onValueChange={(v) => setActiveMode(projectId, v as ProjectChatMode)}
      className="flex-1 min-h-0"
    >
      <TabsList variant="line" className="mx-4 mt-2">
        {MODES.map((mode) => {
          const Icon = MODE_ICON[mode];
          return (
            <Tooltip key={mode}>
              <TooltipTrigger
                render={
                  <TabsTrigger value={mode} className="flex-none">
                    <Icon className="h-4 w-4" />
                    {t(($) => $.chat.tabs[mode])}
                  </TabsTrigger>
                }
              />
              <TooltipContent side="bottom">
                {t(($) => $.chat.tooltips[mode])}
              </TooltipContent>
            </Tooltip>
          );
        })}
      </TabsList>

      {MODES.map((mode) => (
        <TabsContent key={mode} value={mode} className="min-h-0">
          <ModePane
            projectId={projectId}
            mode={mode}
            canConfigure={canConfigure}
          />
        </TabsContent>
      ))}
    </Tabs>
  );
}

function ModePane({
  projectId,
  mode,
  canConfigure,
}: {
  projectId: string;
  mode: ProjectChatMode;
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const tutorialSeen = useProjectChatStore(
    (s) => s.tutorialSeen[projectChatDraftKey(projectId, mode)] === true,
  );
  const dismissTutorial = useProjectChatStore((s) => s.dismissTutorial);

  return (
    <div className="flex h-full flex-col p-4">
      {!tutorialSeen && (
        <div
          data-testid="project-chat-tutorial"
          className="mb-3 flex items-start gap-2 rounded-md border bg-accent/40 px-3 py-2 text-xs text-muted-foreground"
        >
          <span className="flex-1">{t(($) => $.chat.tutorials[mode])}</span>
          <button
            type="button"
            aria-label={t(($) => $.chat.tutorial_dismiss)}
            className="shrink-0 text-muted-foreground hover:text-foreground"
            onClick={() => dismissTutorial(projectId, mode)}
          >
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      )}

      {mode === "team_agent" ? (
        <TeamAgentPane projectId={projectId} canConfigure={canConfigure} />
      ) : (
        <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
          <p className="text-sm font-medium">{t(($) => $.chat.greetings[mode])}</p>
          <p className="text-xs text-muted-foreground">
            {t(($) => $.chat.coming_soon)}
          </p>
        </div>
      )}
    </div>
  );
}

function CenteredState({ children }: { children: React.ReactNode }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 text-center">
      {children}
    </div>
  );
}

function TeamAgentPane({
  projectId,
  canConfigure,
}: {
  projectId: string;
  canConfigure: boolean;
}) {
  const { t } = useT("projects");
  const wsId = useWorkspaceId();
  const { data: chat, isLoading } = useQuery(
    projectChatOptions(wsId, projectId),
  );

  if (isLoading) {
    return (
      <CenteredState>
        <p className="text-xs text-muted-foreground">{t(($) => $.chat.loading)}</p>
      </CenteredState>
    );
  }

  const configured = !!chat?.team_agent_id && !!chat.issue_id;

  if (!configured) {
    return canConfigure ? (
      <CenteredState>
        <div
          data-testid="project-chat-unconfigured-admin"
          className="flex flex-col items-center gap-2"
        >
          <p className="text-sm font-medium">
            {t(($) => $.chat.unconfigured_admin_title)}
          </p>
          {/* TODO(TASK-05): open the model/agent selector and PATCH
              settings.team_agent_id via PUT /api/projects/:id. */}
          <Button variant="outline" size="sm" disabled>
            {t(($) => $.chat.unconfigured_admin_cta)}
          </Button>
        </div>
      </CenteredState>
    ) : (
      <CenteredState>
        <p
          data-testid="project-chat-unconfigured-member"
          className="text-xs text-muted-foreground"
        >
          {t(($) => $.chat.unconfigured_member)}
        </p>
      </CenteredState>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <ProjectTeamAgentChat
        issueId={chat.issue_id}
        projectId={projectId}
        wsId={wsId}
        canConfigure={canConfigure}
      />
    </div>
  );
}
