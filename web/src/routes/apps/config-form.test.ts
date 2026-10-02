import { describe, expect, it } from "vitest";

import { buildConfigRequest, NO_EDITS, type StackConfigInput } from "@/routes/apps/config-form";

const inputs: StackConfigInput[] = [
  { name: "DB_PASSWORD", kind: "secret", set: true, readOnly: false },
  { name: "GPU", kind: "device", value: "/dev/dri/renderD128", readOnly: true },
  { name: "SITE_NAME", kind: "string", value: "Notes", readOnly: false },
  { name: "WEBUI_PORT", kind: "port", value: "3000", readOnly: false },
];

describe("buildConfigRequest", () => {
  it("is null when nothing was edited", () => {
    expect(buildConfigRequest(inputs, NO_EDITS)).toBeNull();
  });

  it("sends only the fields whose value differs from what the stack has", () => {
    const body = buildConfigRequest(inputs, { values: { SITE_NAME: "Notes", WEBUI_PORT: "3100" }, secrets: {} });
    expect(body).toEqual({ values: { WEBUI_PORT: "3100" } });
  });

  it("is null when a value was typed and put back", () => {
    expect(buildConfigRequest(inputs, { values: { WEBUI_PORT: "3000" }, secrets: {} })).toBeNull();
  });

  it("sends a cleared text field as an empty value, which the API reads as the default", () => {
    expect(buildConfigRequest(inputs, { values: { SITE_NAME: "" }, secrets: {} })).toEqual({ values: { SITE_NAME: "" } });
  });

  it("never sends a secret that is kept, whatever was typed before", () => {
    expect(buildConfigRequest(inputs, { values: { DB_PASSWORD: "typed" }, secrets: { DB_PASSWORD: "keep" } })).toBeNull();
    expect(buildConfigRequest(inputs, { values: { DB_PASSWORD: "typed" }, secrets: {} })).toBeNull();
  });

  it("leaves out an empty replacement secret so the stored value is kept", () => {
    expect(buildConfigRequest(inputs, { values: {}, secrets: { DB_PASSWORD: "replace" } })).toBeNull();
    expect(buildConfigRequest(inputs, { values: { DB_PASSWORD: "" }, secrets: { DB_PASSWORD: "replace" } })).toBeNull();
  });

  it("sends a typed replacement, and a generate request without a value", () => {
    expect(buildConfigRequest(inputs, { values: { DB_PASSWORD: "new" }, secrets: { DB_PASSWORD: "replace" } })).toEqual({
      values: { DB_PASSWORD: "new" },
    });
    expect(buildConfigRequest(inputs, { values: { DB_PASSWORD: "stale" }, secrets: { DB_PASSWORD: "generate" } })).toEqual({
      generate: ["DB_PASSWORD"],
    });
  });

  it("never sends a read-only input", () => {
    expect(buildConfigRequest(inputs, { values: { GPU: "/dev/dri/renderD129" }, secrets: {} })).toBeNull();
  });
});
