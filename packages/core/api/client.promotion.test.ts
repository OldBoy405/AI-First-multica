import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { EMPTY_PROMOTION_RESULT } from "./schemas";

// CR-2026-061 TASK-04 (AC-7 client 面 / cmd-04): promoteDiscussion contract —
// REQUIRED Idempotency-Key header (client-enforced), request shape, structured
// parse, and the malformed-response fallback.

afterEach(() => {
  vi.unstubAllGlobals();
});

function promotionResponse(over: Record<string, unknown> = {}) {
  return new Response(
    JSON.stringify({
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
      ...over,
    }),
    { status: 201, headers: { "Content-Type": "application/json" } },
  );
}

describe("promoteDiscussion", () => {
  it("throws without an Idempotency-Key and sends nothing", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    await expect(
      client.promoteDiscussion(
        "proj-1",
        { session_id: "22222222-2222-4222-8222-222222222222", message_ids: ["33333333-3333-4333-8333-333333333333"] },
        "",
      ),
    ).rejects.toThrow(/Idempotency-Key is required/);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("POSTs the promote endpoint with the key header and parses the result", async () => {
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(promotionResponse()));
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    const result = await client.promoteDiscussion(
      "proj-1",
      {
        session_id: "22222222-2222-4222-8222-222222222222",
        message_ids: ["33333333-3333-4333-8333-333333333333"],
        upgrade_to_cr: true,
      },
      "key-1",
    );

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("https://api.example.test/api/projects/proj-1/discussion/promote");
    expect(init.method).toBe("POST");
    expect((init.headers as Record<string, string>)["Idempotency-Key"]).toBe("key-1");
    expect(JSON.parse(String(init.body))).toEqual({
      session_id: "22222222-2222-4222-8222-222222222222",
      message_ids: ["33333333-3333-4333-8333-333333333333"],
      upgrade_to_cr: true,
    });
    expect(result.issue_id).toBe("11111111-1111-4111-8111-111111111111");
    expect(result.issue_number).toBe(7);
    expect(result.run_id).toBe("44444444-4444-4444-8444-444444444444");
    expect(result.created).toBe(true);
  });

  it("parses upgrade_to_cr=false responses with run_id null", async () => {
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(promotionResponse({ upgrade_to_cr: false, run_id: null })),
    );
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    const result = await client.promoteDiscussion(
      "proj-1",
      { session_id: "22222222-2222-4222-8222-222222222222", message_ids: ["33333333-3333-4333-8333-333333333333"] },
      "key-2",
    );
    expect(result.upgrade_to_cr).toBe(false);
    expect(result.run_id).toBeNull();
  });

  it("degrades a malformed success body to the empty fallback", async () => {
    const fetchMock = vi.fn().mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify({ issue_id: "not-a-uuid" }), {
          status: 201,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const client = new ApiClient("https://api.example.test");

    const result = await client.promoteDiscussion(
      "proj-1",
      { session_id: "22222222-2222-4222-8222-222222222222", message_ids: ["33333333-3333-4333-8333-333333333333"] },
      "key-3",
    );
    expect(result).toEqual(EMPTY_PROMOTION_RESULT);
  });
});
