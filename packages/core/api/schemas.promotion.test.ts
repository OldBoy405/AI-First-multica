import { describe, expect, it } from "vitest";
import {
  IssueContextRefSchema,
  IssueSchema,
  ListIssuesResponseSchema,
  PromotionResultSchema,
} from "./schemas";

// CR-2026-061 TASK-04 (AC-7 client 面 / cmd-04): PromotionResultSchema and
// IssueSchema.context_refs — malformed-response behavior and old-backend
// fallback.

describe("PromotionResultSchema", () => {
  const valid = {
    issue_id: "11111111-1111-4111-8111-111111111111",
    issue_number: 7,
    session_id: "22222222-2222-4222-8222-222222222222",
    source_refs: {
      session_id: "22222222-2222-4222-8222-222222222222",
      message_ids: ["33333333-3333-4333-8333-333333333333"],
      attachment_ids: [],
    },
    created: true,
    upgrade_to_cr: true,
    run_id: "44444444-4444-4444-8444-444444444444",
  };

  it("parses a valid result with run_id", () => {
    expect(PromotionResultSchema.parse(valid).run_id).toBe("44444444-4444-4444-8444-444444444444");
  });

  it("accepts run_id null for plain promotions", () => {
    const parsed = PromotionResultSchema.parse({ ...valid, upgrade_to_cr: false, run_id: null });
    expect(parsed.run_id).toBeNull();
  });

  it("rejects a malformed issue_id", () => {
    expect(() => PromotionResultSchema.parse({ ...valid, issue_id: "nope" })).toThrow();
  });
});

describe("IssueSchema.context_refs", () => {
  const baseIssue = {
    id: "11111111-1111-4111-8111-111111111111",
    workspace_id: "22222222-2222-4222-8222-222222222222",
    number: 1,
    identifier: "MUL-1",
    title: "t",
    description: null,
    status: "todo",
    priority: "none",
    assignee_type: null,
    assignee_id: null,
    creator_type: "member",
    creator_id: "33333333-3333-4333-8333-333333333333",
    parent_issue_id: null,
    project_id: null,
    position: 0,
    start_date: null,
    due_date: null,
    metadata: {},
    properties: {},
    created_at: "2026-09-08T00:00:00Z",
    updated_at: "2026-09-08T00:00:00Z",
  };

  it("falls back to [] when the field is absent (old backend)", () => {
    const parsed = IssueSchema.parse(baseIssue);
    expect(parsed.context_refs).toEqual([]);
  });

  it("parses a promotion entry", () => {
    const parsed = IssueSchema.parse({
      ...baseIssue,
      context_refs: [
        {
          kind: "discussion_promotion",
          session_id: "22222222-2222-4222-8222-222222222222",
          message_ids: ["33333333-3333-4333-8333-333333333333"],
          attachment_ids: [],
          dedupe_key: "abc",
          promoted_by: "44444444-4444-4444-8444-444444444444",
          promoted_at: "2026-09-08T00:00:00Z",
          pipeline_run_id: "55555555-5555-4555-8555-555555555555",
        },
      ],
    });
    expect(parsed.context_refs?.[0]?.kind).toBe("discussion_promotion");
    expect(parsed.context_refs?.[0]?.pipeline_run_id).toBe("55555555-5555-4555-8555-555555555555");
  });

  it("never fails the whole issue over malformed context_refs", () => {
    const parsed = IssueSchema.parse({ ...baseIssue, context_refs: [{ kind: 42 }] });
    // The entry fails and the field degrades to [] — the issue survives.
    expect(parsed.id).toBe("11111111-1111-4111-8111-111111111111");
  });

  it("keeps the list usable when one issue carries malformed context_refs", () => {
    const parsed = ListIssuesResponseSchema.parse({
      issues: [
        baseIssue,
        { ...baseIssue, id: "99999999-9999-4999-8999-999999999999", context_refs: [{ kind: 42 }] },
      ],
    });
    expect(parsed.issues).toHaveLength(2);
    expect(parsed.issues[0]?.context_refs).toEqual([]);
    expect(parsed.issues[1]?.context_refs).toEqual([]);
  });
});

describe("IssueContextRefSchema", () => {
  it("accepts a minimal entry with every field omitted", () => {
    expect(IssueContextRefSchema.parse({})).toEqual({});
  });
});
