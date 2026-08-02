import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import { projectKeys } from "./queries";
import { useWorkspaceId } from "../hooks";
import { useRecentContextStore } from "../chat/recent-context-store";
import { clearIssueSurfaceViewState } from "../issues/stores/surface-view-store";
import { issueScopeKey } from "../issues/surface/scope";
import type { Project, CreateProjectRequest, UpdateProjectRequest, ListProjectsResponse } from "../types";
import type { ApproveCrRequest } from "../api/schemas";

export function useCreateProject() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (data: CreateProjectRequest) => api.createProject(data),
    onSuccess: (newProject) => {
      qc.setQueryData<ListProjectsResponse>(projectKeys.list(wsId), (old) =>
        old && !old.projects.some((p) => p.id === newProject.id)
          ? { ...old, projects: [...old.projects, newProject], total: old.total + 1 }
          : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
    },
  });
}

export function useUpdateProject() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & UpdateProjectRequest) =>
      api.updateProject(id, data),
    onMutate: ({ id, ...data }) => {
      qc.cancelQueries({ queryKey: projectKeys.list(wsId) });
      const prevList = qc.getQueryData<ListProjectsResponse>(projectKeys.list(wsId));
      const prevDetail = qc.getQueryData<Project>(projectKeys.detail(wsId, id));
      qc.setQueryData<ListProjectsResponse>(projectKeys.list(wsId), (old) =>
        old ? { ...old, projects: old.projects.map((p) => (p.id === id ? { ...p, ...data } : p)) } : old,
      );
      qc.setQueryData<Project>(projectKeys.detail(wsId, id), (old) =>
        old ? { ...old, ...data } : old,
      );
      return { prevList, prevDetail, id };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.prevList) qc.setQueryData(projectKeys.list(wsId), ctx.prevList);
      if (ctx?.prevDetail) qc.setQueryData(projectKeys.detail(wsId, ctx.id), ctx.prevDetail);
    },
    onSettled: (_data, _err, vars) => {
      qc.invalidateQueries({ queryKey: projectKeys.detail(wsId, vars.id) });
      qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
    },
  });
}

// Send a Team Agent chat message (CR-2026-006 TASK-04). The composer awaits
// mutateAsync and branches on the thrown ApiError for the 429/502/409 paths;
// this hook only handles the cross-cutting side effect: a 409
// team_agent_not_configured means config drifted since the panel loaded, so
// refresh the chat context to flip the panel back to its unconfigured guide.
export function useSendProjectChatMessage(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (content: string) => api.sendProjectChatMessage(projectId, content),
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        qc.invalidateQueries({ queryKey: projectKeys.chat(wsId, projectId) });
      }
    },
  });
}

// Bind a Team Agent to the project's group chat (CR-2026-006 FR-4/DD-4). Not
// optimistic: the backend validates the agent exists in the workspace before
// accepting it, so the outcome isn't locally predictable (CLAUDE.md's
// optimistic-update rule). Invalidating projectKeys.chat is what flips the
// panel from the unconfigured guide to the live message stream.
export function useSetProjectTeamAgent(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (agentId: string) =>
      api.updateProject(projectId, { settings: { team_agent_id: agentId } }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.chat(wsId, projectId) });
    },
  });
}

// Approve/reject a CR's pending gate (CR-2026-011 TASK-05). No optimistic
// update — the outcome isn't locally predictable (EVIDENCE_DRIFT/
// FORBIDDEN_APPROVER can reject it, and pending_advance is server-derived),
// so the card renders its own pending state and invalidates on settle
// (CLAUDE.md's optimistic-update rule). cr:updated (WS) also invalidates the
// same key once crctl actually advances the CR — this invalidate covers the
// gap until that arrives.
export function useApproveCr(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ crId, ...body }: { crId: string } & ApproveCrRequest) =>
      api.approveCr(wsId, crId, body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.gates(wsId, projectId) });
    },
  });
}

export function useDeleteProject() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  return useMutation({
    mutationFn: (id: string) => api.deleteProject(id),
    onMutate: async (id) => {
      await qc.cancelQueries({ queryKey: projectKeys.list(wsId) });
      const prevList = qc.getQueryData<ListProjectsResponse>(projectKeys.list(wsId));
      qc.setQueryData<ListProjectsResponse>(projectKeys.list(wsId), (old) =>
        old ? { ...old, projects: old.projects.filter((p) => p.id !== id), total: old.total - 1 } : old,
      );
      qc.removeQueries({ queryKey: projectKeys.detail(wsId, id) });
      return { prevList };
    },
    onError: (_err, _id, ctx) => {
      if (ctx?.prevList) qc.setQueryData(projectKeys.list(wsId), ctx.prevList);
    },
    onSuccess: (_data, id) => {
      useRecentContextStore.getState().forgetContext(wsId, { type: "project", id });
      clearIssueSurfaceViewState(issueScopeKey({ type: "project", projectId: id }));
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: projectKeys.list(wsId) });
    },
  });
}
