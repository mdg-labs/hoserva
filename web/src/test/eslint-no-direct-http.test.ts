// Proves the "no direct fetch/XMLHttpRequest/WebSocket/axios outside
// src/lib/api" rule (D18, eslint.config.js) actually fires, rather than
// trusting the config by inspection. Runs the real ESLint config against
// inline fixtures — no throwaway file is left behind — covering every
// bypass a previous review found working against this same config: bare
// globals, their window/globalThis/self-qualified spellings in both dot and
// bracket notation, navigator.sendBeacon in both notations, a static
// HTTP-library import, a dynamic one, and a require() of one.
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
  return result.messages.some((m) => m.ruleId?.startsWith("no-restricted-"));
}

describe("no direct HTTP outside src/lib/api (D18)", () => {
  it.each([
    ["a bare fetch() call", 'export function bad() { return fetch("/api/v1/jobs"); }\n'],
    ["a bare XMLHttpRequest", "export function bad() { return new XMLHttpRequest(); }\n"],
    ["a bare EventSource", 'export function bad() { return new EventSource("/api/v1/events"); }\n'],
    ["a bare WebSocket", 'export function bad() { return new WebSocket("wss://x"); }\n'],
    ["window.fetch", 'export function bad() { return window.fetch("/api/v1/jobs"); }\n'],
    ["globalThis.fetch", 'export function bad() { return globalThis.fetch("/api/v1/jobs"); }\n'],
    ["self.fetch", 'export function bad() { return self.fetch("/api/v1/jobs"); }\n'],
    ["window[\"fetch\"]", 'export function bad() { return window["fetch"]("/api/v1/jobs"); }\n'],
    ["globalThis[\"fetch\"]", 'export function bad() { return globalThis["fetch"]("/api/v1/jobs"); }\n'],
    ["window.WebSocket", 'export function bad() { return new window.WebSocket("wss://x"); }\n'],
    ["window[\"WebSocket\"]", 'export function bad() { return window["WebSocket"]("wss://x"); }\n'],
    ["new window[\"WebSocket\"](...)", 'export function bad() { return new window["WebSocket"]("wss://x"); }\n'],
    ["navigator.sendBeacon", 'export function bad() { return navigator.sendBeacon("/api/v1/jobs", "{}"); }\n'],
    [
      "navigator[\"sendBeacon\"]",
      'export function bad() { return navigator["sendBeacon"]("/api/v1/jobs", "{}"); }\n',
    ],
  ])("rejects %s in a route", async (_name, code) => {
    const result = await lint(code, path.resolve(import.meta.dirname, "../routes/deliberately-bad.ts"));
    expect(fires(result)).toBe(true);
  });

  it.each([
    ["a static axios import", 'import axios from "axios";\nexport const client = axios;\n'],
    ["a static ky import", 'import ky from "ky";\nexport const client = ky;\n'],
    ["a static openapi-fetch import", 'import createClient from "openapi-fetch";\nexport const client = createClient;\n'],
    ["a dynamic axios import", 'export async function bad() { return import("axios"); }\n'],
    ["a dynamic openapi-fetch import", 'export async function bad() { return import("openapi-fetch"); }\n'],
    ["a require() of axios", 'export const client = require("axios");\n'],
    ["a require() of openapi-fetch", 'export const client = require("openapi-fetch");\n'],
  ])("rejects %s in a pattern component", async (_name, code) => {
    const result = await lint(code, path.resolve(import.meta.dirname, "../components/patterns/deliberately-bad.ts"));
    expect(fires(result)).toBe(true);
  });

  it("allows fetch inside src/lib/api", async () => {
    const result = await lint(
      "export function ok() { return fetch(\"/api/v1/jobs\"); }\n",
      path.resolve(import.meta.dirname, "../lib/api/deliberately-fine.ts"),
    );
    expect(fires(result)).toBe(false);
  });

  it("allows openapi-fetch inside src/lib/api", async () => {
    const result = await lint(
      'import createClient from "openapi-fetch";\nexport const client = createClient;\n',
      path.resolve(import.meta.dirname, "../lib/api/deliberately-fine.ts"),
    );
    expect(fires(result)).toBe(false);
  });
});

// Proves the "hoservaClient is only ever called from src/lib/api" rule
// (D18, #271): a route or component must go through the shared
// useApiQuery/useApiMutation hooks (or an src/lib/api/operations.ts
// wrapper), never hoservaClient directly.
describe("no direct hoservaClient outside src/lib/api (D18, #271)", () => {
  it("rejects a direct hoservaClient call in a route", async () => {
    const result = await lint(
      'import { hoservaClient } from "@/lib/api/client";\nexport function bad() { return hoservaClient.GET("/jobs"); }\n',
      path.resolve(import.meta.dirname, "../routes/deliberately-bad.ts"),
    );
    expect(fires(result)).toBe(true);
  });

  it("rejects a direct hoservaClient call in a pattern component", async () => {
    const result = await lint(
      'import { hoservaClient } from "@/lib/api/client";\nexport function bad() { return hoservaClient.POST("/mover/run"); }\n',
      path.resolve(import.meta.dirname, "../components/patterns/deliberately-bad.ts"),
    );
    expect(fires(result)).toBe(true);
  });

  it("rejects importing hoservaClient in a route even before it is called", async () => {
    const result = await lint(
      'import { hoservaClient } from "@/lib/api/client";\nexport const client = hoservaClient;\n',
      path.resolve(import.meta.dirname, "../routes/deliberately-bad.ts"),
    );
    expect(fires(result)).toBe(true);
  });

  it("allows the shared useApiQuery hook in a route", async () => {
    const result = await lint(
      [
        'import { getJobs } from "@/lib/api/operations";',
        'import { useApiQuery } from "@/lib/api/use-api-query";',
        "export function useJobs() {",
        '  return useApiQuery({ queryKey: "jobs", queryFn: (signal) => getJobs(undefined, signal) });',
        "}",
        "",
      ].join("\n"),
      path.resolve(import.meta.dirname, "../routes/deliberately-fine.ts"),
    );
    expect(fires(result)).toBe(false);
  });

  it("allows hoservaClient inside src/lib/api", async () => {
    const result = await lint(
      'import { hoservaClient } from "@/lib/api/client";\nexport function getJobs() { return hoservaClient.GET("/jobs"); }\n',
      path.resolve(import.meta.dirname, "../lib/api/deliberately-fine.ts"),
    );
    expect(fires(result)).toBe(false);
  });
});
