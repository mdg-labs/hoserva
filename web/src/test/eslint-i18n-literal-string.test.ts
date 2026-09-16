// Proves the "every string comes from the i18n catalog" rule
// (i18next/no-literal-string, eslint.config.js) actually inspects a JSX
// attribute value, not only JSX text — a previous review found the
// plugin's default `jsx-text-only` mode silently skips every attribute
// regardless of `jsx-attributes` configuration, letting a raw aria-label
// ship unnoticed — and a template-literal spelling of either (a previous
// review found `should-validate-template` defaults to off, letting
// `aria-label={\`Section\`}` ship unnoticed too). Runs the real ESLint
// config against inline fixtures.
import { ESLint } from "eslint";
import path from "node:path";
import { describe, expect, it } from "vitest";

const configPath = path.resolve(import.meta.dirname, "../../eslint.config.js");

async function lint(code: string, filePath: string) {
  const eslint = new ESLint({ overrideConfigFile: configPath, cwd: path.resolve(import.meta.dirname, "../..") });
  const [result] = await eslint.lintText(code, { filePath });
  return result;
}

function fires(result: Awaited<ReturnType<typeof lint>>): boolean {
  return result.messages.some((m) => m.ruleId === "i18next/no-literal-string");
}

const ROUTE_FILE = () => path.resolve(import.meta.dirname, "../routes/deliberately-bad.tsx");

describe("every string comes from the i18n catalog (Q48)", () => {
  it("rejects a raw JSX text literal", async () => {
    const result = await lint("export function Bad() { return <div>Hello there</div>; }\n", ROUTE_FILE());
    expect(fires(result)).toBe(true);
  });

  it("rejects a raw aria-label attribute literal", async () => {
    const result = await lint(
      'export function Bad() { return <nav aria-label="Section" />; }\n',
      ROUTE_FILE(),
    );
    expect(fires(result)).toBe(true);
  });

  it("rejects a raw placeholder attribute literal", async () => {
    const result = await lint(
      'export function Bad() { return <input placeholder="Type here" />; }\n',
      ROUTE_FILE(),
    );
    expect(fires(result)).toBe(true);
  });

  it("rejects a template-literal aria-label attribute value", async () => {
    const result = await lint(
      "export function Bad() { return <nav aria-label={`Section`} />; }\n",
      ROUTE_FILE(),
    );
    expect(fires(result)).toBe(true);
  });

  it("rejects a template-literal JSX text child", async () => {
    const result = await lint(
      "export function Bad() { return <div>{`Hello there`}</div>; }\n",
      ROUTE_FILE(),
    );
    expect(fires(result)).toBe(true);
  });

  it("allows an excluded, non-user-visible attribute literal", async () => {
    const result = await lint(
      'export function Ok() { return <div className="flex" data-testid="thing" type="button" role="status" variant="ghost" size="xs" tone="error" aria-hidden="true" aria-current="page" href="/settings" /> ; }\n',
      ROUTE_FILE(),
    );
    expect(fires(result)).toBe(false);
  });

  it("does not check vendored coss primitives under src/components/ui", async () => {
    const result = await lint(
      'export function Bad() { return <div>Hello there</div>; }\n',
      path.resolve(import.meta.dirname, "../components/ui/deliberately-bad.tsx"),
    );
    expect(fires(result)).toBe(false);
  });
});
