import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";

import { expect, test as setup } from "@playwright/test";

import { pageSessionSecret, SESSION_SECRET_FILE } from "./session";

const storageState = "e2e/.auth/user.json";

setup("authenticate as the L3 admin", async ({ page }) => {
  mkdirSync(dirname(storageState), { recursive: true });

  const username = process.env.HOSERVA_E2E_USERNAME ?? "hoserva-l3";
  const password = process.env.HOSERVA_E2E_PASSWORD ?? "hoserva-l3-suite-password";

  await page.goto("/");
  const signIn = page.getByRole("button", { name: /sign in/i });
  const jobs = page.getByRole("link", { name: /jobs/i });
  await expect(signIn.or(jobs)).toBeVisible();

  if (await signIn.isVisible()) {
    await page.getByLabel("Username").fill(username);
    await page.getByLabel("Password").fill(password);
    await signIn.click();
  }

  await expect(jobs).toBeVisible();
  await expect(page).not.toHaveURL(/\/login(?:\/|$|\?)/);
  await expect(page).not.toHaveURL(/\/welcome(?:\/|$|\?)/);
  const secret = await pageSessionSecret(page);
  expect(secret, "signing in must leave the session's second secret in sessionStorage").toBeTruthy();
  writeFileSync(SESSION_SECRET_FILE, secret ?? "", { mode: 0o600 });
  await page.context().storageState({ path: storageState });
});
