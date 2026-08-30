"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Bot, Lock, MessagesSquare, Users, X } from "lucide-react";
import { toast } from "sonner";
import { projectChatOptions, projectKeys, projectPresenterOptions } from "@multica/core/projects/queries";
import {
  useProjectChatStore,
  useSetProjectTeamAgent,
  projectChatDraftKey,
  type ProjectChatMode,
} from "@multica/core/projects";
import { useWorkspaceId } from "@multica/core/hooks";
import { useActorName } from "@multica/core/workspace/hooks";
import { agentListOptions } from "@multica/core/workspace/queries";
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
import { ActorAvatar } from "../../common/actor-avatar";
import {
  PropertyPicker,
  PickerItem,
  PickerSection,
  PickerEmpty,
} from "../../issues/components/pickers/property-picker";
import { useNavigation } from "../../navigation";
import { ProjectTeamAgentChat } from "./project-team-agent-chat";
import { CrStatusBadge } from "./cr-status-badge";
import { ProjectPrivateAsk } from "./project-private-ask";
import { DiscussionPane } from "./discussion-pane";
import { PresenterControlSheet } from "./presenter-control-sheet";
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
  const wsId = useWorkspaceId();
  const router = useNavigation();
  const activeMode =
    useProjectChatStore((s) => s.activeMode[projectId]) ?? "team_agent";
  const setActiveMode = useProjectChatStore((s) => s.setActiveMode);

  // ?mode= deep link (CR-2026-009): read once per project so an inbox jump
  // link (`?tab=chat&mode=discussion`) lands on the right tab. Only re-runs
  // when projectId changes, not on every render, so a later manual tab
  // switch isn't clobbered by a stale query string.
  useEffect(() => {
    const raw = router.searchParams.get("mode");
    if (raw === "team_agent" || raw === "private_ask" || raw === "discussion") {
      setActiveMode(projectId, raw);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId]);

  return (
    <Tabs
      value={activeMode}
      onValueChange={(v) => setActiveMode(projectId, v as ProjectChatMode)}
      className="flex-1 min-h-0"
    >
      <div className="mx-4 mt-2 flex items-center justify-between gap-2">
        <TabsList variant="line">
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
        <div className="flex items-center gap-2">
          {activeMode === "team_agent" && (
            <PresenterHeader wsId={wsId} projectId={projectId} />
          )}
          <CrStatusBadge wsId={wsId} projectId={projectId} />
        </div>
      </div>

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

// Presenter (single-writer control) display for the Team Agent tab
// (CR-2026-010 SDD §5.2/§5.3). Shows who currently holds control — or the
// Owner/Admin default when no presenter is active — plus a trigger button
// that opens the control panel Sheet.
function PresenterHeader({ wsId, projectId }: { wsId: string; projectId: string }) {
  const { t } = useT("projects");
  const { getActorName } = useActorName();
  const { data } = useQuery(projectPresenterOptions(wsId, projectId));
  const [panelOpen, setPanelOpen] = useState(false);
  const presenter = data?.presenter ?? null;

  return (
    <div className="ml-auto flex items-center gap-1.5 pr-1 text-xs text-muted-foreground">
      <span className="truncate">
        {presenter
          ? t(($) => $.chat.presenter.current, {
              name: getActorName("member", presenter.user_id),
            })
          : t(($) => $.chat.presenter.default)}
      </span>
      <Tooltip>
        <TooltipTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              className="h-6 w-6"
              onClick={() => setPanelOpen(true)}
            >
              <Users className="h-3.5 w-3.5" />
            </Button>
          }
        />
        <TooltipContent side="bottom">
          {t(($) => $.chat.presenter.open_panel)}
        </TooltipContent>
      </Tooltip>
      <PresenterControlSheet
        open={panelOpen}
        onOpenChange={setPanelOpen}
        wsId={wsId}
        projectId={projectId}
      />
    </div>
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
      ) : mode === "private_ask" ? (
        <PrivateAskPane projectId={projectId} />
      ) : (
        <DiscussionPane projectId={projectId} canConfigure={canConfigure} />
      )}
    </div>
  );
}

function PrivateAskPane({ projectId }: { projectId: string }) {
  const wsId = useWorkspaceId();
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <ProjectPrivateAsk projectId={projectId} wsId={wsId} />
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

  // Explicit non-empty-string checks rather than truthy/falsy: both fields
  // default to "" (never undefined) once `chat` itself has loaded, and "" is
  // the actual "not configured yet" sentinel, not just a falsy placeholder.
  // CR-2026-056 (AC-11/§3.1): issue_id may be null before the first send —
  // the session itself anchors "configured", not the container. A hard
  // degradation (empty session_id) lands in the same branch, read-only.
  const configured =
    chat !== undefined && chat.team_agent_id !== "" && chat.session_id !== "";

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
          <TeamAgentSetupPicker projectId={projectId} wsId={wsId} />
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
        <RetryChatConfigButton wsId={wsId} projectId={projectId} />
      </CenteredState>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <ProjectTeamAgentChat
        issueId={chat.issue_id ?? ""}
        sessionId={chat.session_id}
        projectId={projectId}
        wsId={wsId}
        teamAgentId={chat.team_agent_id}
        canConfigure={canConfigure}
      />
    </div>
  );
}

// Retry affordance for a hard-degraded chat context (AC-27): the GET schema
// fallback wipes session_id, so the member state doubles as the
// "config unavailable" state — one explicit retry refetches the GET.
function RetryChatConfigButton({
  wsId,
  projectId,
}: {
  wsId: string;
  projectId: string;
}) {
  const { t } = useT("projects");
  const qc = useQueryClient();
  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      data-testid="project-chat-config-retry"
      onClick={() => {
        void qc.invalidateQueries({ queryKey: projectKeys.chat(wsId, projectId) });
      }}
    >
      {t(($) => $.chat.config_retry)}
    </Button>
  );
}

// Inline agent picker for the unconfigured-admin guide (SDD §5.2: "owner/admin
// see an inline agent selector; selecting writes settings.team_agent_id").
// Agent-only (no squads) — a group chat has exactly one Team Agent, so the
// squad branch other agent pickers carry doesn't apply here.
function TeamAgentSetupPicker({
  projectId,
  wsId,
}: {
  projectId: string;
  wsId: string;
}) {
  const { t } = useT("projects");
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const setTeamAgent = useSetProjectTeamAgent(wsId, projectId);

  const activeAgents = useMemo(() => agents.filter((a) => !a.archived_at), [agents]);
  const query = filter.trim().toLowerCase();
  const filteredAgents = activeAgents.filter((a) =>
    !query || a.name.toLowerCase().includes(query),
  );

  const handlePick = (agentId: string) => {
    setOpen(false);
    setTeamAgent.mutate(agentId, {
      onError: () => toast.error(t(($) => $.chat.team_agent_setup_failed)),
    });
  };

  return (
    <PropertyPicker
      open={open}
      onOpenChange={setOpen}
      width="w-56"
      searchable
      onSearchChange={setFilter}
      trigger={
        <Button variant="outline" size="sm" disabled={setTeamAgent.isPending}>
          {t(($) => $.chat.unconfigured_admin_cta)}
        </Button>
      }
    >
      {filteredAgents.length === 0 ? (
        <PickerEmpty />
      ) : (
        <PickerSection label={t(($) => $.chat.tabs.team_agent)}>
          {filteredAgents.map((a) => (
            <PickerItem key={a.id} selected={false} onClick={() => handlePick(a.id)}>
              <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
              <span className="truncate">{a.name}</span>
            </PickerItem>
          ))}
        </PickerSection>
      )}
    </PropertyPicker>
  );
}
