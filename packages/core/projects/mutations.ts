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
    // CR-2026-012 FR-8: attachment ids ride along the same send (backend
    // binds them to the chat comment); absent/empty keeps prior behavior.
    // CR-2026-056 §3.1: session_id is REQUIRED — the send runs inside the
    // active session's transaction (Bind-in-tx).
    mutationFn: (vars: { sessionId: string; content: string; attachmentIds?: string[] }) =>
      api.sendProjectChatMessage(projectId, vars.sessionId, vars.content, vars.attachmentIds),
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        qc.invalidateQueries({ queryKey: projectKeys.chat(wsId, projectId) });
      }
    },
  });
}

// Cancel ("clear" / "stop") a task in the project's shared Team Agent queue
// (CR-2026-007 DD-2). Same pattern as useSendProjectChatMessage: the caller
// awaits mutateAsync and branches on the outcome. The server answers terminal
// tasks with an idempotent 200 carrying the task's real status, so the three
// UI branches (TSUG-007) are:
//   1. result.status === "cancelled" → silent success — this includes a
//      repeat cancel of an already-cancelled task (double click), no toast;
//   2. any other terminal status (completed/failed) → "task already finished,
//      cannot cancel" toast;
//   3. thrown ApiError (403 etc.) → toast err.message.
// This hook only owns the cross-cutting side effect: invalidating the
// queue-status prefix on settle, which also covers the items list because its
// key extends the same prefix (DD-3/TSUG-006).
export function useCancelProjectQueueTask(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (taskId: string) => api.cancelTaskById(taskId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: projectKeys.queueStatus(wsId, projectId) });
    },
  });
}

// Bind a Team Agent to the project's group chat (CR-2026-006 FR-4/DD-4). Not
// optimistic: the backend validates the agent exists in the workspace before
// accepting it, so the outcome isn't locally predictable (CLAUDE.md's
// optimistic-update rule). Invalidating projectKeys.chat is what flips the
// panel from the unconfigured guide to the live message stream.
// Presenter (single-writer control) mutations (CR-2026-010). Not optimistic:
// presenter grants are cross-user permission state, not a locally-predictable
// patch (CLAUDE.md's optimistic-update rule). onSettled invalidates both the
// presenter query and the chat query — a transition can flip who is allowed
// to send, so the send-guard state the chat panel reads must refresh too.
function usePresenterInvalidation(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return () => {
    qc.invalidateQueries({ queryKey: projectKeys.presenter(wsId, projectId) });
    qc.invalidateQueries({ queryKey: projectKeys.chat(wsId, projectId) });
  };
}

export function useRequestPresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: () => api.requestPresenter(projectId),
    onSettled: invalidate,
  });
}

export function useApprovePresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: (userId: string) => api.approvePresenter(projectId, userId),
    onSettled: invalidate,
  });
}

export function useRejectPresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: (userId: string) => api.rejectPresenter(projectId, userId),
    onSettled: invalidate,
  });
}

export function useTransferPresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: (userId: string) => api.transferPresenter(projectId, userId),
    onSettled: invalidate,
  });
}

export function useRevokePresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: () => api.revokePresenter(projectId),
    onSettled: invalidate,
  });
}

export function useReleasePresenter(wsId: string, projectId: string) {
  const invalidate = usePresenterInvalidation(wsId, projectId);
  return useMutation({
    mutationFn: () => api.releasePresenter(projectId),
    onSettled: invalidate,
  });
}

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

// Bind (or rebind) the project's Discussion Coordinator (CR-2026-012 DD-1):
// writes settings.discussion_coordinator_agent_id, mirrored on the Team
// Agent binding above. Invalidates the Discussion context query so the pane
// header flips to the configured state.
export function useSetProjectDiscussionCoordinator(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (agentId: string) =>
      api.updateProject(projectId, {
        settings: { discussion_coordinator_agent_id: agentId },
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: projectKeys.discussion(wsId, projectId) });
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
