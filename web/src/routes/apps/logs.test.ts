import { describe, expect, it } from "vitest";

import { appendLog } from "@/routes/apps/logs";

describe("appendLog", () => {
  it("joins chunks while the text is small", () => {
    expect(appendLog("a\nb", "\nc\n")).toBe("a\nb\nc\n");
  });

  it("drops the oldest whole lines once the text passes its size limit", () => {
    const line = `${"x".repeat(99)}\n`;
    const full = line.repeat(20_000);
    const next = appendLog(full, "last line\n");
    expect(next.length).toBeLessThanOrEqual(2_000_000);
    expect(next.endsWith("last line\n")).toBe(true);
    expect(next.startsWith("x".repeat(99))).toBe(true);
  });
});
