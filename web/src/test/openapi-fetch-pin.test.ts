// web/package.json and api/package.json each pin openapi-fetch
// independently (api/'s copy only drives openapi-typescript's code
// generation; web/'s is what the built app actually ships, aliased over
// api/gen/ts/client.ts's own import — vite.config.ts) — nothing else
// catches the two drifting apart if one is bumped without the other.
import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

function readPin(pkgPath: string): string {
  const pkg = JSON.parse(readFileSync(pkgPath, "utf-8")) as { dependencies?: Record<string, string> };
  const pin = pkg.dependencies?.["openapi-fetch"];
  if (!pin) {
    throw new Error(`${pkgPath} does not depend on openapi-fetch`);
  }
  return pin;
}

describe("openapi-fetch pin", () => {
  it("matches api/package.json's pin", () => {
    const webPin = readPin(path.resolve(import.meta.dirname, "../../package.json"));
    const apiPin = readPin(path.resolve(import.meta.dirname, "../../../api/package.json"));
    expect(webPin).toBe(apiPin);
  });
});
