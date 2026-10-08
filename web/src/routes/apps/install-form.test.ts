import { describe, expect, it } from "vitest";

import {
  buildInstallRequest,
  customNetworks,
  hasInputError,
  hasPortConflict,
  inputOf,
  malformedLimits,
  missingNetwork,
  NO_ADVANCED,
  portConflict,
  type TemplateInstallPlan,
} from "@/routes/apps/install-form";

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

  it("sends each advanced setting that was chosen and none that was not", () => {
    expect(buildInstallRequest("", {}, new Set(), NO_ADVANCED)).toEqual({});
    expect(
      buildInstallRequest("", {}, new Set(), {
        networkMode: " lan ",
        restart: "on-failure",
        cpus: "1.5",
        memoryMiB: "512",
        extraParams: "--cap-add NET_ADMIN",
      }),
    ).toEqual({ networkMode: "lan", restart: "on-failure", cpus: 1.5, memoryMiB: 512, extraParams: "--cap-add NET_ADMIN" });
  });

  it("leaves out a restart rule the API does not know and a limit that is no number", () => {
    const body = buildInstallRequest("", {}, new Set(), { ...NO_ADVANCED, restart: "sometimes", cpus: "lots", memoryMiB: "1.5" });
    expect(body).toEqual({});
  });

  it("sends a zero limit, which the API refuses, instead of reading it as no limit", () => {
    expect(buildInstallRequest("", {}, new Set(), { ...NO_ADVANCED, cpus: "0", memoryMiB: "0" })).toEqual({ cpus: 0, memoryMiB: 0 });
  });
});

describe("malformedLimits", () => {
  it("names the limits that are text but not a number the API could take", () => {
    expect(malformedLimits(NO_ADVANCED)).toEqual([]);
    expect(malformedLimits({ ...NO_ADVANCED, cpus: "1.5", memoryMiB: "512" })).toEqual([]);
    expect(malformedLimits({ ...NO_ADVANCED, cpus: "lots", memoryMiB: "1.5" })).toEqual(["cpus", "memoryMiB"]);
  });
});

describe("inputOf", () => {
  it("reads the input the API's error is about from its details", () => {
    expect(inputOf({ details: { input: "APPDATA" } })).toBe("APPDATA");
    expect(inputOf({ details: { input: "networkMode" } })).toBe("networkMode");
  });

  it("finds none for an error that names no input, whatever its text says", () => {
    expect(inputOf(undefined)).toBeNull();
    expect(inputOf({})).toBeNull();
    expect(inputOf({ details: { input: 3 } })).toBeNull();
    expect(inputOf({ details: { input: "" } })).toBeNull();
  });
});

describe("customNetworks", () => {
  it("leaves out the built-in networks, which are offered on their own", () => {
    const networks = [
      { name: "bridge", driver: "bridge" },
      { name: "host", driver: "host" },
      { name: "lan", driver: "macvlan" },
      { name: "none", driver: "null" },
    ];
    expect(customNetworks(networks)).toEqual([{ name: "lan", driver: "macvlan" }]);
  });
});

const port = { name: "P", kind: "port" as const, generated: false, required: true, value: "8097" };
const plan = {
  template: { source: "", id: "", revision: "1" },
  title: "",
  name: "",
  privileges: [],
  warnings: [],
  advancedAvailable: true,
  compose: "",
  digest: "",
};

describe("portConflict", () => {
  it("reads a moved port from the API's requestedValue", () => {
    expect(portConflict({ ...port, requestedValue: "8096" })).toEqual({ requested: "8096", suggested: "8097" });
    expect(portConflict(port)).toBeNull();
  });

  it("is only about ports", () => {
    expect(portConflict({ ...port, kind: "string", requestedValue: "8096" })).toBeNull();
  });

  it("is true for a plan with one moved port", () => {
    expect(hasPortConflict({ ...plan, inputs: [port, { ...port, requestedValue: "8096" }] })).toBe(true);
    expect(hasPortConflict({ ...plan, inputs: [port] })).toBe(false);
  });
});

describe("hasInputError and missingNetwork", () => {
  it("is true when the API lists an input that still needs a value", () => {
    expect(hasInputError({ ...plan, inputs: [port, { ...port, error: "NAME needs a value" }] })).toBe(true);
    expect(hasInputError({ ...plan, inputs: [port, { ...port, error: "" }] })).toBe(false);
  });

  it("finds the missing-network warning among the plan's warnings", () => {
    const missing = { class: "missing_network" as const, message: "m", command: "docker network create iot" };
    const withWarnings: TemplateInstallPlan = { ...plan, inputs: [], warnings: [{ class: "note", message: "n" }, missing] };
    expect(missingNetwork(withWarnings)).toEqual(missing);
    expect(missingNetwork({ ...plan, inputs: [] })).toBeNull();
  });
});
