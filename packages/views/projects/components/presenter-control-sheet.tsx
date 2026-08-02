"use client";

import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  projectPresenterOptions,
  useApprovePresenter,
  useRejectPresenter,
  useRequestPresenter,
  useRevokePresenter,
  useTransferPresenter,
  useReleasePresenter,
} from "@multica/core/projects";
import { memberListOptions } from "@multica/core/workspace/queries";
import { useAuthStore } from "@multica/core/auth";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from "@multica/ui/components/ui/sheet";
import { Button } from "@multica/ui/components/ui/button";
import { Badge } from "@multica/ui/components/ui/badge";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT } from "../../i18n";

/**
 * Presenter (single-writer control) panel (CR-2026-010 SDD §5.3): request
 * access (plain member), approve/reject pending + revoke active (owner),
 * transfer/release (the current presenter). Member list = every workspace
 * member — there is no project-level membership concept to filter by.
 *
 * Server-authoritative (NFR-1): every button here can still 403/404/409 on a
 * race (someone else acted first); each call surfaces a toast on failure and
 * relies on the mutation's onSettled to invalidate + re-render the correct
 * state rather than trusting the click.
 */
export function PresenterControlSheet({
  open,
  onOpenChange,
  wsId,
  projectId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  wsId: string;
  projectId: string;
}) {
  const { t } = useT("projects");
  const userId = useAuthStore((s) => s.user?.id);
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: presenterState } = useQuery(projectPresenterOptions(wsId, projectId));

  const me = members.find((m) => m.user_id === userId);
  const isOwner = me?.role === "owner";
  const isAdmin = me?.role === "admin";
  const presenterUserId = presenterState?.presenter?.user_id;
  const isPresenter = presenterUserId != null && presenterUserId === userId;
  const pendingUserIds = new Set((presenterState?.pending_requests ?? []).map((g) => g.user_id));
  const hasOwnPendingRequest = !!presenterState?.my_request;

  const requestPresenter = useRequestPresenter(wsId, projectId);
  const approvePresenter = useApprovePresenter(wsId, projectId);
  const rejectPresenter = useRejectPresenter(wsId, projectId);
  const revokePresenter = useRevokePresenter(wsId, projectId);
  const transferPresenter = useTransferPresenter(wsId, projectId);
  const releasePresenter = useReleasePresenter(wsId, projectId);

  const onActionError = () => toast.error(t(($) => $.chat.control.action_failed));

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="flex w-[360px] flex-col overflow-y-auto p-0">
        <SheetHeader className="border-b px-4 py-3">
          <SheetTitle>{t(($) => $.chat.control.title)}</SheetTitle>
        </SheetHeader>

        <div className="flex-1 overflow-y-auto">
          {members.map((member) => {
            const isThisPresenter = member.user_id === presenterUserId;
            const isThisPending = pendingUserIds.has(member.user_id);
            const canTransferToThisMember = isPresenter && !isThisPresenter;

            return (
              <div
                key={member.user_id}
                data-testid="presenter-control-member-row"
                className="flex items-center gap-3 border-b px-4 py-3 last:border-b-0"
              >
                <ActorAvatar actorType="member" actorId={member.user_id} size="sm" />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-medium">{member.name}</div>
                  {isThisPresenter && (
                    <Badge variant="secondary" className="mt-0.5">
                      {t(($) => $.chat.control.presenter_badge)}
                    </Badge>
                  )}
                  {isThisPending && !isThisPresenter && (
                    <Badge variant="outline" className="mt-0.5">
                      {t(($) => $.chat.control.pending_badge)}
                    </Badge>
                  )}
                </div>
                <div className="flex shrink-0 items-center gap-1">
                  {isOwner && isThisPending && (
                    <>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={approvePresenter.isPending}
                        onClick={() =>
                          approvePresenter.mutate(member.user_id, { onError: onActionError })
                        }
                      >
                        {t(($) => $.chat.control.approve)}
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={rejectPresenter.isPending}
                        onClick={() =>
                          rejectPresenter.mutate(member.user_id, { onError: onActionError })
                        }
                      >
                        {t(($) => $.chat.control.reject)}
                      </Button>
                    </>
                  )}
                  {isOwner && isThisPresenter && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={revokePresenter.isPending}
                      onClick={() => revokePresenter.mutate(undefined, { onError: onActionError })}
                    >
                      {t(($) => $.chat.control.revoke)}
                    </Button>
                  )}
                  {canTransferToThisMember && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={transferPresenter.isPending}
                      onClick={() =>
                        transferPresenter.mutate(member.user_id, { onError: onActionError })
                      }
                    >
                      {t(($) => $.chat.control.transfer)}
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
        </div>

        <div className="border-t px-4 py-3">
          {!isOwner && !isAdmin && !isPresenter && (
            <Button
              className="w-full"
              disabled={requestPresenter.isPending || hasOwnPendingRequest}
              onClick={() => requestPresenter.mutate(undefined, { onError: onActionError })}
            >
              {hasOwnPendingRequest
                ? t(($) => $.chat.control.requested)
                : t(($) => $.chat.control.request_cta)}
            </Button>
          )}
          {isPresenter && (
            <Button
              className="w-full"
              variant="outline"
              disabled={releasePresenter.isPending}
              onClick={() => releasePresenter.mutate(undefined, { onError: onActionError })}
            >
              {t(($) => $.chat.control.release)}
            </Button>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
