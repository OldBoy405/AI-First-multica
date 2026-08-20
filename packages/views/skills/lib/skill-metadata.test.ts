// @vitest-environment node
// AIFIRST: CR-2026-048 TASK-09 (FR-21).
import { describe, expect, it } from "vitest";
import { parseRequirements } from "./skill-metadata";

describe("parseRequirements", () => {
  it("reads JSON-encoded sequences and plain scalar lists", () => {
    expect(parseRequirements('["git","node"]')).toEqual(["git", "node"]);
    expect(parseRequirements("git, node")).toEqual(["git", "node"]);
    expect(parseRequirements("git")).toEqual(["git"]);
  });

  it("degrades to no tags instead of throwing", () => {
    expect(parseRequirements(undefined)).toEqual([]);
    expect(parseRequirements("")).toEqual([]);
    expect(parseRequirements("[broken")).toEqual(["[broken"]);
    expect(parseRequirements('{"a":1}')).toEqual(['{"a":1}']);
  });
});
