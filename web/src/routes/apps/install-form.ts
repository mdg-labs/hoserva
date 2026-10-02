import type { components } from "@/lib/api/client";

export type TemplateInput = components["schemas"]["TemplateInput"];
export type TemplateInstallPlan = components["schemas"]["TemplateInstallPlan"];
export type TemplateInstallRequest = components["schemas"]["TemplateInstallRequest"];

// An entry the user cleared is left out, because the API reads an empty value
// as "take the default". A secret's value never goes to a preview: the plan
// does not depend on it, and a preview is sent on every pause in typing.
export function buildInstallRequest(
  name: string,
  values: Record<string, string>,
  omit: ReadonlySet<string> = new Set(),
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
  return body;
}

export function secretNames(inputs: TemplateInput[]): Set<string> {
  return new Set(inputs.filter((input) => input.kind === "secret").map((input) => input.name));
}

// The API words a refused input as `…: <NAME> <what is wrong>`, so the input
// the message starts with is the one to mark. A message about anything else
// (Docker not reachable, a GPU the host cannot give) belongs to no field.
export function fieldOfError(message: string, inputNames: string[]): string | null {
  const parts = message.split(": ");
  for (const input of inputNames) {
    if (parts.some((part) => part.startsWith(`${input} `) || part.startsWith(`${input}:`))) {
      return input;
    }
  }
  return null;
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
