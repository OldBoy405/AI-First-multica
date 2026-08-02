import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const projectKeys = {
  all: (wsId: string) => ["projects", wsId] as const,
  list: (wsId: string) => [...projectKeys.all(wsId), "list"] as const,
  detail: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "detail", id] as const,
  queueStatus: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "queue-status", id] as const,
  queueStatusAll: (wsId: string) =>
    [...projectKeys.all(wsId), "queue-status"] as const,
  chat: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "chat", id] as const,
  gatesAll: (wsId: string) =>
    [...projectKeys.all(wsId), "gates"] as const,
  gates: (wsId: string, id: string) =>
    [...projectKeys.gatesAll(wsId), id] as const,
};

export function projectListOptions(wsId: string) {
  return queryOptions({
    queryKey: projectKeys.list(wsId),
    queryFn: () => api.listProjects(),
    select: (data) => data.projects,
  });
}

export function projectDetailOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.detail(wsId, id),
    queryFn: () => api.getProject(id),
  });
}

// Live shared Team Agent queue depth + limit (CR-2026-004). Refetched via
// the realtime task:* prefix invalidation, so no polling interval is needed.
export function projectQueueStatusOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.queueStatus(wsId, id),
    queryFn: () => api.getProjectQueueStatus(id),
  });
}

// Project group-chat context (CR-2026-006): backing issue id + configured
// Team Agent id. Powers the project chat panel's Team Agent tab.
export function projectChatOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.chat(wsId, id),
    queryFn: () => api.getProjectChat(id),
  });
}

// CR governance gates (CR-2026-011 TASK-05): 16-state CR badge + pending
// approval cards + gate-node history for the project chat window. An empty
// `crs` array (feature off server-side, or genuinely no in-flight CRs) is
// the natural "nothing to render" state — no separate enabled/disabled flag
// needed (api.getProjectGates already folds the feature-off 404 into this
// same empty shape).
export function projectGatesOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.gates(wsId, id),
    queryFn: () => api.getProjectGates(id),
    select: (data) => data.crs,
  });
}
