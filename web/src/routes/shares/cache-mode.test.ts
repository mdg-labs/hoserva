import { describe, expect, it } from "vitest";

import { shareRelocationDirection } from "@/routes/shares/cache-mode";

describe("shareRelocationDirection", () => {
  it.each([
    ["cache-then-move", "array-only", "array"],
    ["cache-only", "array-only", "array"],
    ["array-only", "cache-then-move", "cache"],
    ["array-only", "cache-only", "cache"],
    ["cache-then-move", "cache-only", "cache"],
    ["cache-only", "cache-then-move", null],
    ["array-only", "array-only", null],
  ] as const)("%s → %s relocates to %s", (from, to, want) => {
    expect(shareRelocationDirection(from, to)).toBe(want);
  });
});
