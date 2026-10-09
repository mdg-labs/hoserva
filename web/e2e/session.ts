import { readFileSync } from "node:fs";

import { expect, test as base, type Page } from "@playwright/test";

// A browser sends the session cookie on its own; the API also wants the
// session's second secret as a header (doc 15 T15). The web client keeps it
// in sessionStorage, which Playwright's storageState does not carry, so
// auth.setup.ts saves it beside the state and every spec restores it.
export const SESSION_SECRET_HEADER = "X-Hoserva-Session-Secret";
const SESSION_SECRET_KEY = "hoserva.sessionSecret";
export const SESSION_SECRET_FILE = "e2e/.auth/session-secret.txt";

export async function pageSessionSecret(page: Page): Promise<string | null> {
  return page.evaluate((key) => sessionStorage.getItem(key), SESSION_SECRET_KEY);
}

export async function sessionHeaders(page: Page): Promise<Record<string, string>> {
  const secret = await pageSessionSecret(page);
  return secret === null ? {} : { [SESSION_SECRET_HEADER]: secret };
}

export const test = base.extend<{ sessionSecret: string }>({
  sessionSecret: [
    async ({ context }, use) => {
      const secret = readFileSync(SESSION_SECRET_FILE, "utf-8").trim();
      await context.addInitScript(
        ([key, value]) => {
          try {
            sessionStorage.setItem(key, value);
          } catch {
            // A document with an opaque origin has no sessionStorage.
          }
        },
        [SESSION_SECRET_KEY, secret],
      );
      await use(secret);
    },
    { auto: true },
  ],
});

export { expect };
