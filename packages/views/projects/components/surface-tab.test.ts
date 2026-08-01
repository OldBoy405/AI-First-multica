import { describe, it, expect } from "vitest";
import { projectSurfaceTab, projectSurfaceHref } from "./surface-tab";

describe("project surface ?tab= mapping (CR-2026-006)", () => {
  it("defaults to issues when ?tab= is absent or unknown", () => {
    expect(projectSurfaceTab(new URLSearchParams(""))).toBe("issues");
    expect(projectSurfaceTab(new URLSearchParams("tab=bogus"))).toBe("issues");
  });

  it("selects chat only for ?tab=chat", () => {
    expect(projectSurfaceTab(new URLSearchParams("tab=chat"))).toBe("chat");
  });

  it("adds ?tab=chat when switching to chat, preserving other params", () => {
    const href = projectSurfaceHref(
      "/acme/projects/p1",
      new URLSearchParams("foo=bar"),
      "chat",
    );
    expect(href).toBe("/acme/projects/p1?foo=bar&tab=chat");
  });

  it("drops ?tab= entirely when switching back to issues", () => {
    expect(
      projectSurfaceHref("/acme/projects/p1", new URLSearchParams("tab=chat"), "issues"),
    ).toBe("/acme/projects/p1");
    // Other params survive the drop.
    expect(
      projectSurfaceHref(
        "/acme/projects/p1",
        new URLSearchParams("tab=chat&foo=bar"),
        "issues",
      ),
    ).toBe("/acme/projects/p1?foo=bar");
  });
});
