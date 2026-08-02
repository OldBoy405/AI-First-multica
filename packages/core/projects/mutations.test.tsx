/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import {
  getIssueSurfaceViewStore,
  pruneIssueSurfaceViewStates,
} from "../issues/stores/surface-view-store";
import {
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
