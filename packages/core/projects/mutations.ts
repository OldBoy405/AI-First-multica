import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import { projectKeys } from "./queries";
import { useWorkspaceId } from "../hooks";
import { useRecentContextStore } from "../chat/recent-context-store";
import { clearIssueSurfaceViewState } from "../issues/stores/surface-view-store";
import { issueScopeKey } from "../issues/surface/scope";
import type { Project, CreateProjectRequest, UpdateProjectRequest, ListProjectsResponse } from "../types";

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
