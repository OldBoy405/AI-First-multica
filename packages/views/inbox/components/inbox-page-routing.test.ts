import { describe, it, expect } from "vitest";
import type { InboxItemType } from "@multica/core/types";
import { resolveInboxItemHref } from "./inbox-page";

// CR-2026-010 TSUG-002 / TASK-06 AC5: the 5 presenter inbox types must deep
// link to the project's Chat tab via details.project_id, never the default
// issue_id path (which points at a hidden, filtered-out container issue).
describe("resolveInboxItemHref", () => {
  const projectDetailPath = (id: string) => `/acme/projects/${id}`;

  const PRESENTER_TYPES: InboxItemType[] = [
    "presenter_requested",
    "presenter_approved",
    "presenter_rejected",
    "presenter_transferred",
    "presenter_revoked",
  ];

  it.each(PRESENTER_TYPES)("routes %s to the project's Chat tab via details.project_id", (type) => {
    const href = resolveInboxItemHref(
      { type, details: { project_id: "proj-1" } },
      projectDetailPath,
    );
    expect(href).toBe("/acme/projects/proj-1?tab=chat");
  });

  it("falls back to the default (null) when details.project_id is missing", () => {
    const href = resolveInboxItemHref(
      { type: "presenter_requested", details: null },
      projectDetailPath,
    );
    expect(href).toBeNull();
  });

  it("returns null for every non-presenter type (keeps the default embedded-issue selection)", () => {
    const nonPresenterTypes: InboxItemType[] = [
      "issue_assigned",
      "new_comment",
      "mentioned",
      "quick_create_done",
    ];
    for (const type of nonPresenterTypes) {
      expect(
        resolveInboxItemHref({ type, details: { project_id: "proj-1" } }, projectDetailPath),
      ).toBeNull();
    }
  });

  it("returns null for an unrelated/unknown type (release is not an inbox item — no directed recipient)", () => {
    expect(
      resolveInboxItemHref(
        { type: "reaction_added", details: { project_id: "proj-1" } },
        projectDetailPath,
      ),
    ).toBeNull();
  });
});
