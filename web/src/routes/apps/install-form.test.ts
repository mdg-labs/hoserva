import { describe, expect, it } from "vitest";

import { buildInstallRequest, fieldOfError, hasPortConflict, portConflict } from "@/routes/apps/install-form";

describe("buildInstallRequest", () => {
  it("sends nothing the user did not choose", () => {
    expect(buildInstallRequest("", {})).toEqual({});
  });

  it("drops a cleared entry so the API takes the default, and trims the name", () => {
    expect(buildInstallRequest(" notes ", { A: "", B: "x" })).toEqual({ name: "notes", values: { B: "x" } });
  });

  it("leaves the omitted names out", () => {
    expect(buildInstallRequest("", { SECRET: "s", B: "x" }, new Set(["SECRET"]))).toEqual({ values: { B: "x" } });
  });
});

describe("fieldOfError", () => {
  const names = ["PORT", "PORT_WEB", "APPDATA"];

  it("finds the input a refusal starts with", () => {
    expect(fieldOfError("template: invalid install input: APPDATA needs a path", names)).toBe("APPDATA");
    expect(fieldOfError("template: invalid install input: PORT_WEB must be a port from 1 to 65535", names)).toBe("PORT_WEB");
  });

  it("does not take one input's name for another that begins with it", () => {
    expect(fieldOfError("PORT_WEB needs a port", ["PORT", "PORT_WEB"])).toBe("PORT_WEB");
  });

  it("finds none for a message about something else", () => {
    expect(fieldOfError("Docker is not reachable", names)).toBeNull();
    expect(fieldOfError("the APPDATA folder is on a stopped disk", names)).toBeNull();
  });
});

describe("portConflict", () => {
  const port = { name: "P", kind: "port" as const, generated: false, value: "8097" };

  it("reads a moved port from the API's requestedValue", () => {
    expect(portConflict({ ...port, requestedValue: "8096" })).toEqual({ requested: "8096", suggested: "8097" });
    expect(portConflict(port)).toBeNull();
  });

  it("is only about ports", () => {
    expect(portConflict({ ...port, kind: "string", requestedValue: "8096" })).toBeNull();
  });

  it("is true for a plan with one moved port", () => {
    const plan = { template: { source: "", id: "", revision: "1" }, title: "", name: "", privileges: [], compose: "" };
    expect(hasPortConflict({ ...plan, inputs: [port, { ...port, requestedValue: "8096" }] })).toBe(true);
    expect(hasPortConflict({ ...plan, inputs: [port] })).toBe(false);
  });
});
