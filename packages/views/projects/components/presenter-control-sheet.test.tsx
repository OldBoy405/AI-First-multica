// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { MemberWithUser } from "@multica/core/types";
import enProjects from "../../locales/en/projects.json";
import enCommon from "../../locales/en/common.json";

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => <div data-testid="avatar" />,
}));

vi.mock("sonner", () => ({
  toast: { error: vi.fn(), success: vi.fn() },
}));

let authUserId = "u-owner";
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) =>
    selector({ user: { id: authUserId } }),
}));

let members: MemberWithUser[] = [];
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: (wsId: string) => ({
    queryKey: ["members", wsId],
    queryFn: async () => members,
  }),
}));

let presenterState: {
  presenter: { user_id: string; status: string; granted_by: string; created_at: string } | null;
  pending_requests: Array<{ user_id: string; status: string; granted_by: string; created_at: string }>;
  my_request: { user_id: string; status: string; granted_by: string; created_at: string } | null;
} = { presenter: null, pending_requests: [], my_request: null };

const mutationMocks = {
  request: { mutate: vi.fn(), isPending: false },
  approve: { mutate: vi.fn(), isPending: false },
  reject: { mutate: vi.fn(), isPending: false },
  revoke: { mutate: vi.fn(), isPending: false },
  transfer: { mutate: vi.fn(), isPending: false },
  release: { mutate: vi.fn(), isPending: false },
};

vi.mock("@multica/core/projects", () => ({
  projectPresenterOptions: (_wsId: string, id: string) => ({
    queryKey: ["presenter", id],
    queryFn: async () => presenterState,
  }),
  useRequestPresenter: () => mutationMocks.request,
  useApprovePresenter: () => mutationMocks.approve,
  useRejectPresenter: () => mutationMocks.reject,
  useRevokePresenter: () => mutationMocks.revoke,
  useTransferPresenter: () => mutationMocks.transfer,
  useReleasePresenter: () => mutationMocks.release,
}));

import { PresenterControlSheet } from "./presenter-control-sheet";

const RESOURCES = { en: { projects: enProjects, common: enCommon } };

function renderSheet() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={RESOURCES}>
        <PresenterControlSheet open onOpenChange={() => {}} wsId="ws-1" projectId="proj-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

function member(userId: string, role: MemberWithUser["role"], name: string): MemberWithUser {
  return {
    id: `m-${userId}`,
    workspace_id: "ws-1",
    user_id: userId,
    role,
    created_at: "2026-01-01T00:00:00Z",
    name,
    email: `${userId}@multica.ai`,
    avatar_url: null,
  };
}

beforeEach(() => {
  authUserId = "u-owner";
  members = [
    member("u-owner", "owner", "Owner"),
    member("u-member", "member", "Member"),
    member("u-other", "member", "Other Member"),
  ];
  presenterState = { presenter: null, pending_requests: [], my_request: null };
  for (const m of Object.values(mutationMocks)) {
    m.mutate.mockClear();
    m.isPending = false;
  }
});

afterEach(() => {
  cleanup();
});

describe("PresenterControlSheet role-based rendering (CR-2026-010 TASK-06 AC1)", () => {
  it("plain member with no pending request sees the request CTA", async () => {
    authUserId = "u-member";
    renderSheet();
    expect(
      await screen.findByRole("button", { name: enProjects.chat.control.request_cta }),
    ).not.toBeDisabled();
  });

  it("plain member with a pending request sees a disabled 'Requested' state", async () => {
    authUserId = "u-member";
    presenterState = {
      presenter: null,
      pending_requests: [{ user_id: "u-member", status: "pending", granted_by: "", created_at: "" }],
      my_request: { user_id: "u-member", status: "pending", granted_by: "", created_at: "" },
    };
    renderSheet();
    const btn = await screen.findByRole("button", { name: enProjects.chat.control.requested });
    expect(btn).toBeDisabled();
  });

  it("owner sees approve/reject on a pending request row", async () => {
    presenterState = {
      presenter: null,
      pending_requests: [{ user_id: "u-member", status: "pending", granted_by: "", created_at: "" }],
      my_request: null,
    };
    renderSheet();
    expect(await screen.findByRole("button", { name: enProjects.chat.control.approve })).toBeTruthy();
    expect(screen.getByRole("button", { name: enProjects.chat.control.reject })).toBeTruthy();
    // Owner is neither requesting nor presenting — no request CTA/release button.
    expect(screen.queryByRole("button", { name: enProjects.chat.control.request_cta })).toBeNull();
    expect(screen.queryByRole("button", { name: enProjects.chat.control.release })).toBeNull();
  });

  it("owner sees revoke on the active presenter's row", async () => {
    presenterState = {
      presenter: { user_id: "u-member", status: "active", granted_by: "u-owner", created_at: "" },
      pending_requests: [],
      my_request: null,
    };
    renderSheet();
    expect(await screen.findByRole("button", { name: enProjects.chat.control.revoke })).toBeTruthy();
  });

  it("the presenter (self) sees release and per-row transfer buttons, not owner-only actions", async () => {
    authUserId = "u-member";
    presenterState = {
      presenter: { user_id: "u-member", status: "active", granted_by: "u-owner", created_at: "" },
      pending_requests: [],
      my_request: null,
    };
    renderSheet();
    expect(await screen.findByRole("button", { name: enProjects.chat.control.release })).toBeTruthy();
    // Two transfer targets (owner, other member) — one button each, none on own row.
    expect(screen.getAllByRole("button", { name: enProjects.chat.control.transfer })).toHaveLength(2);
    expect(screen.queryByRole("button", { name: enProjects.chat.control.revoke })).toBeNull();
  });

  it("admin (not owner, not presenter) sees no action buttons", async () => {
    members = [...members, member("u-admin", "admin", "Admin")];
    authUserId = "u-admin";
    renderSheet();
    await screen.findAllByTestId("presenter-control-member-row");
    expect(screen.queryByRole("button", { name: enProjects.chat.control.request_cta })).toBeNull();
    expect(screen.queryByRole("button", { name: enProjects.chat.control.approve })).toBeNull();
    expect(screen.queryByRole("button", { name: enProjects.chat.control.release })).toBeNull();
  });

  it("clicking approve calls the mutation with the target user id", async () => {
    presenterState = {
      presenter: null,
      pending_requests: [{ user_id: "u-member", status: "pending", granted_by: "", created_at: "" }],
      my_request: null,
    };
    renderSheet();
    const approveBtn = await screen.findByRole("button", { name: enProjects.chat.control.approve });
    await act(async () => {
      approveBtn.click();
    });
    expect(mutationMocks.approve.mutate).toHaveBeenCalledWith("u-member", expect.anything());
  });
});
