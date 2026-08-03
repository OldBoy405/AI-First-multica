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
  // Extends queueStatus so the realtime task:* handler's queueStatusAll
  // prefix invalidation (use-realtime-sync.ts) covers the items list with
  // zero changes there (CR-2026-007 TSUG-006).
  queueItems: (wsId: string, id: string) =>
    [...projectKeys.queueStatus(wsId, id), "items"] as const,
  chat: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "chat", id] as const,
  gatesAll: (wsId: string) =>
    [...projectKeys.all(wsId), "gates"] as const,
  gates: (wsId: string, id: string) =>
    [...projectKeys.gatesAll(wsId), id] as const,
  privateChat: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "private-chat", id] as const,
  discussion: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "discussion", id] as const,
  presenter: (wsId: string, id: string) =>
    [...projectKeys.all(wsId), "presenter", id] as const,
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

// Expanded queue detail (CR-2026-007 DD-3): the pending rows behind the
// depth number. Rides the same realtime prefix invalidation as
// projectQueueStatusOptions, so no polling either.
export function projectQueueItemsOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.queueItems(wsId, id),
    queryFn: () => api.getProjectQueueItems(id),
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

// The caller's Private Ask session for a project (CR-2026-008): get-or-create,
// so the queryFn is only enabled once the panel knows a Team Agent is bound
// (callers pass `enabled`). staleTime Infinity — the session id is stable for
// the pane's lifetime; message/pending caches are what stay live.
export function projectPrivateChatOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.privateChat(wsId, id),
    queryFn: () => api.getProjectPrivateChat(id),
    staleTime: Infinity,
  });
}

// Project Discussion context (CR-2026-009): backing container issue id.
// Powers the project chat panel's Discussion tab.
export function projectDiscussionOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.discussion(wsId, id),
    queryFn: () => api.getProjectDiscussion(id),
  });
}

// Presenter (single-writer control) grant state for a project's Team Agent
// chat (CR-2026-010). WS updates arrive via project:presenter_changed, which
// the realtime layer's "project:" prefix invalidation already covers — no
// dedicated WS handler needed here.
export function projectPresenterOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: projectKeys.presenter(wsId, id),
    queryFn: () => api.getProjectPresenter(id),
  });
}
