/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiError, setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { CancelTaskResponse } from "../types";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import {
  getIssueSurfaceViewStore,
  pruneIssueSurfaceViewStates,
} from "../issues/stores/surface-view-store";
import {
  useCancelProjectQueueTask,
  useDeleteProject,
  useRequestPresenter,
  useApprovePresenter,
  useRejectPresenter,
  useTransferPresenter,
  useRevokePresenter,
  useReleasePresenter,
} from "./mutations";
import { projectKeys } from "./queries";

vi.mock("../hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

describe("useDeleteProject", () => {
  let qc: QueryClient;
  let deleteProject: ReturnType<typeof vi.fn<() => Promise<void>>>;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    deleteProject = vi.fn().mockResolvedValue(undefined);
    setApiInstance({ deleteProject } as unknown as ApiClient);
    setCurrentWorkspace("acme", "ws-1");
  });

  afterEach(() => {
    qc.clear();
    pruneIssueSurfaceViewStates([]);
    setCurrentWorkspace(null, null);
    vi.restoreAllMocks();
  });

  it("clears the deleted project's issue surface view state", async () => {
    const store = getIssueSurfaceViewStore("project:p1");
    store.getState().setViewMode("list");
    expect(store.getState().viewMode).toBe("list");

    const { result } = renderHook(() => useDeleteProject(), {
      wrapper: createWrapper(qc),
    });

    await act(async () => {
      await result.current.mutateAsync("p1");
    });

    expect(deleteProject).toHaveBeenCalledWith("p1");
    expect(store.getState().viewMode).toBe("board");
  });
});

describe("useCancelProjectQueueTask (CR-2026-007 TSUG-007)", () => {
  let qc: QueryClient;
  let cancelTaskById: ReturnType<typeof vi.fn<(taskId: string) => Promise<CancelTaskResponse>>>;

  function taskWithStatus(status: CancelTaskResponse["status"]): CancelTaskResponse {
    return {
      id: "t1",
      agent_id: "a1",
      runtime_id: "",
      issue_id: "i1",
      status,
      priority: 0,
      dispatched_at: null,
      started_at: null,
      completed_at: null,
      result: null,
      error: null,
      created_at: "2026-08-02T00:00:00Z",
    };
  }

  function renderCancelHook() {
    return renderHook(() => useCancelProjectQueueTask("ws-1", "p1"), {
      wrapper: createWrapper(qc),
    });
  }

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    cancelTaskById = vi.fn();
    setApiInstance({ cancelTaskById } as unknown as ApiClient);
  });

  afterEach(() => {
    qc.clear();
    vi.restoreAllMocks();
  });

  it("resolves with the cancelled task and invalidates the queue-status prefix", async () => {
    cancelTaskById.mockResolvedValue(taskWithStatus("cancelled"));
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderCancelHook();

    let res: CancelTaskResponse | undefined;
    await act(async () => {
      res = await result.current.mutateAsync("t1");
    });

    expect(cancelTaskById).toHaveBeenCalledWith("t1");
    // Branch 1: the caller sees status === "cancelled" and stays silent —
    // this is also the idempotent repeat-cancel (double click) response.
    expect(res?.status).toBe("cancelled");
    // The items key extends this prefix, so one invalidation covers both.
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: projectKeys.queueStatus("ws-1", "p1"),
    });
  });

  it("resolves with the real terminal status when the task already finished", async () => {
    cancelTaskById.mockResolvedValue(taskWithStatus("completed"));
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderCancelHook();

    let res: CancelTaskResponse | undefined;
    await act(async () => {
      res = await result.current.mutateAsync("t1");
    });

    // Branch 2: idempotent 200 with the original status — the caller shows
    // the "task already finished" toast off this value.
    expect(res?.status).toBe("completed");
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: projectKeys.queueStatus("ws-1", "p1"),
    });
  });

  it("rethrows an ApiError (403) and still invalidates on settle", async () => {
    cancelTaskById.mockRejectedValue(
      new ApiError("not allowed to cancel this task", 403, "Forbidden"),
    );
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    const { result } = renderCancelHook();

    // Branch 3: the caller toasts err.message from the thrown ApiError.
    await act(async () => {
      await expect(result.current.mutateAsync("t1")).rejects.toMatchObject({
        status: 403,
        message: "not allowed to cancel this task",
      });
    });

    expect(invalidate).toHaveBeenCalledWith({
      queryKey: projectKeys.queueStatus("ws-1", "p1"),
    });
  });
});

// CR-2026-010 TASK-05: presenter mutations. Not optimistic (cross-user
// permission state) — every mutation, success or failure, must invalidate
// both the presenter query and the chat query on settle (a transition can
// flip who is allowed to send).
describe("presenter mutations", () => {
  let qc: QueryClient;

  beforeEach(() => {
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  });

  afterEach(() => {
    qc.clear();
    vi.restoreAllMocks();
  });

  // Each hook's mutation variable type differs (string for the target-user
  // ones, void for the self-acting ones) — normalize to a single
  // `(arg?: string) => Promise<unknown>` shape per case so the data table
  // below can stay uniform and typecheck.
  const cases: Array<{
    name: string;
    useMutateAsync: (wsId: string, projectId: string) => (arg?: string) => Promise<unknown>;
    apiMethod: string;
    arg?: string;
    expectedCallArgs: unknown[];
  }> = [
    {
      name: "useRequestPresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useRequestPresenter(wsId, projectId);
        return () => mutateAsync();
      },
      apiMethod: "requestPresenter",
      expectedCallArgs: ["p1"],
    },
    {
      name: "useApprovePresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useApprovePresenter(wsId, projectId);
        return (arg) => mutateAsync(arg as string);
      },
      apiMethod: "approvePresenter",
      arg: "u-target",
      expectedCallArgs: ["p1", "u-target"],
    },
    {
      name: "useRejectPresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useRejectPresenter(wsId, projectId);
        return (arg) => mutateAsync(arg as string);
      },
      apiMethod: "rejectPresenter",
      arg: "u-target",
      expectedCallArgs: ["p1", "u-target"],
    },
    {
      name: "useTransferPresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useTransferPresenter(wsId, projectId);
        return (arg) => mutateAsync(arg as string);
      },
      apiMethod: "transferPresenter",
      arg: "u-target",
      expectedCallArgs: ["p1", "u-target"],
    },
    {
      name: "useRevokePresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useRevokePresenter(wsId, projectId);
        return () => mutateAsync();
      },
      apiMethod: "revokePresenter",
      expectedCallArgs: ["p1"],
    },
    {
      name: "useReleasePresenter",
      useMutateAsync: (wsId, projectId) => {
        const { mutateAsync } = useReleasePresenter(wsId, projectId);
        return () => mutateAsync();
      },
      apiMethod: "releasePresenter",
      expectedCallArgs: ["p1"],
    },
  ];

  for (const { name, useMutateAsync, apiMethod, arg, expectedCallArgs } of cases) {
    it(`${name}: success path calls the API and invalidates presenter + chat queries`, async () => {
      const fn = vi.fn().mockResolvedValue({ user_id: "u-1", status: "active", granted_by: "", created_at: "" });
      setApiInstance({ [apiMethod]: fn } as unknown as ApiClient);
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

      const { result } = renderHook(() => useMutateAsync("ws-1", "p1"), {
        wrapper: createWrapper(qc),
      });

      await act(async () => {
        await result.current(arg);
      });

      expect(fn).toHaveBeenCalledWith(...expectedCallArgs);
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: projectKeys.presenter("ws-1", "p1") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: projectKeys.chat("ws-1", "p1") });
    });

    it(`${name}: failure path still invalidates presenter + chat queries`, async () => {
      const fn = vi.fn().mockRejectedValue(new Error("boom"));
      setApiInstance({ [apiMethod]: fn } as unknown as ApiClient);
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

      const { result } = renderHook(() => useMutateAsync("ws-1", "p1"), {
        wrapper: createWrapper(qc),
      });

      await act(async () => {
        await expect(result.current(arg)).rejects.toThrow("boom");
      });

      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: projectKeys.presenter("ws-1", "p1") });
      expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: projectKeys.chat("ws-1", "p1") });
    });
  }
});
