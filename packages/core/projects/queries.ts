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
