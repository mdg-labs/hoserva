import { describe, expect, it } from "vitest";

import { apiErrorMessage, parseClientResult } from "@/lib/api/request";

describe("parseClientResult", () => {
  it("treats a non-OK response with an empty error as a failure", () => {
    const parsed = parseClientResult(
      { error: undefined, response: { ok: false } },
      "could not stop the array",
    );
    expect(parsed.data).toBeUndefined();
    expect(parsed.error).toBe("could not stop the array");
  });

  it("returns data when the response is ok and there is no error", () => {
    const parsed = parseClientResult({ data: { value: 1 }, response: { ok: true } });
    expect(parsed.error).toBeNull();
    expect(parsed.data).toEqual({ value: 1 });
  });
});

describe("apiErrorMessage", () => {
  it("uses the catalog when the response has no message", () => {
    expect(apiErrorMessage(undefined)).toBe("Request failed");
  });
});
