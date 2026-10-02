import type { components } from "@/lib/api/client";

export type TemplateInput = components["schemas"]["TemplateInput"];
export type TemplateInstallPlan = components["schemas"]["TemplateInstallPlan"];
export type TemplateInstallRequest = components["schemas"]["TemplateInstallRequest"];
export type ConversionWarning = components["schemas"]["ConversionWarning"];
export type DockerNetwork = components["schemas"]["DockerNetwork"];
export type RestartPolicy = NonNullable<TemplateInstallRequest["restart"]>;

// What the Advanced tab holds. Every entry is text so a half-typed number is
// not lost; an empty one means the setting is left as the template has it.
export type AdvancedSettings = {
  networkMode: string;
  restart: string;
  cpus: string;
  memoryMiB: string;
  extraParams: string;
};

export const NO_ADVANCED: AdvancedSettings = { networkMode: "", restart: "", cpus: "", memoryMiB: "", extraParams: "" };

export const RESTART_POLICIES: RestartPolicy[] = ["no", "always", "unless-stopped", "on-failure"];
export const NETWORK_BRIDGE = "bridge";
export const NETWORK_HOST = "host";

// The settings the API names in an error's details.input beside the template's
// own inputs.
export const ADVANCED_FIELDS = ["networkMode", "restart", "cpus", "memoryMiB", "extraParams"] as const;
export type AdvancedField = (typeof ADVANCED_FIELDS)[number];

export function isAdvancedField(name: string): name is AdvancedField {
  return (ADVANCED_FIELDS as readonly string[]).includes(name);
}

// A number the user typed, or null for nothing typed or text that is no
// number: the form marks the second as wrong instead of leaving it out.
export function parseLimit(text: string): number | null {
  const trimmed = text.trim();
  if (trimmed === "") {
    return null;
  }
  const value = Number(trimmed);
  return Number.isFinite(value) ? value : Number.NaN;
}

// An entry the user cleared is left out, because the API reads an empty value
// as "take the default". A secret's value never goes to a preview: the plan
// does not depend on it, and a preview is sent on every pause in typing. A
// limit that is not a number is left out too; the form marks it, and the
// install is held until it is fixed.
export function buildInstallRequest(
  name: string,
  values: Record<string, string>,
  omit: ReadonlySet<string> = new Set(),
  advanced: AdvancedSettings = NO_ADVANCED,
): TemplateInstallRequest {
  const body: TemplateInstallRequest = {};
  const stackName = name.trim();
  if (stackName !== "") {
    body.name = stackName;
  }
  const entries = Object.entries(values).filter(([input, value]) => value !== "" && !omit.has(input));
  if (entries.length > 0) {
    body.values = Object.fromEntries(entries);
  }
  if (advanced.networkMode.trim() !== "") {
    body.networkMode = advanced.networkMode.trim();
  }
  if (RESTART_POLICIES.includes(advanced.restart as RestartPolicy)) {
    body.restart = advanced.restart as RestartPolicy;
  }
  const cpus = parseLimit(advanced.cpus);
  if (cpus !== null && !Number.isNaN(cpus)) {
    body.cpus = cpus;
  }
  const memory = parseLimit(advanced.memoryMiB);
  if (memory !== null && Number.isInteger(memory)) {
    body.memoryMiB = memory;
  }
  if (advanced.extraParams.trim() !== "") {
    body.extraParams = advanced.extraParams;
  }
  return body;
}

// The limits that are text but not a number the API could take: a CPU limit
// that is not a number and a memory limit that is not a whole number.
export function malformedLimits(advanced: AdvancedSettings): Array<"cpus" | "memoryMiB"> {
  const out: Array<"cpus" | "memoryMiB"> = [];
  const cpus = parseLimit(advanced.cpus);
  if (cpus !== null && Number.isNaN(cpus)) {
    out.push("cpus");
  }
  const memory = parseLimit(advanced.memoryMiB);
  if (memory !== null && !Number.isInteger(memory)) {
    out.push("memoryMiB");
  }
  return out;
}

export function secretNames(inputs: TemplateInput[]): Set<string> {
  return new Set(inputs.filter((input) => input.kind === "secret").map((input) => input.name));
}

// The input or setting an API refusal is about, from the error's structured
// details.input; null for a refusal about nothing in particular (Docker not
// reachable, a GPU the host cannot give).
export function inputOf(error: { details?: { [key: string]: unknown } } | undefined): string | null {
  const input = error?.details?.input;
  return typeof input === "string" && input !== "" ? input : null;
}

// The networks the user created: the built-in bridge, host and none are
// offered on their own.
const BUILT_IN_NETWORKS = new Set(["bridge", "host", "none"]);

export function customNetworks(networks: DockerNetwork[]): DockerNetwork[] {
  return networks.filter((network) => !BUILT_IN_NETWORKS.has(network.name));
}

export function missingNetwork(plan: TemplateInstallPlan): ConversionWarning | null {
  return plan.warnings.find((warning) => warning.class === "missing_network") ?? null;
}

// A port the template asked for that something else holds: the API resolved
// it to the next free port and said which one was asked for.
export function portConflict(input: TemplateInput): { requested: string; suggested: string } | null {
  if (input.kind !== "port" || input.requestedValue === undefined || input.value === undefined) {
    return null;
  }
  return { requested: input.requestedValue, suggested: input.value };
}

export function hasPortConflict(plan: TemplateInstallPlan): boolean {
  return plan.inputs.some((input) => portConflict(input) !== null);
}

// An input the API lists as needing a value it does not have yet.
export function hasInputError(plan: TemplateInstallPlan): boolean {
  return plan.inputs.some((input) => (input.error ?? "") !== "");
}
