import type { components } from "@/lib/api/client";
import type { TemplateInput } from "@/routes/apps/install-form";

export type StackConfig = components["schemas"]["StackConfig"];
export type StackConfigInput = components["schemas"]["StackConfigInput"];
export type UpdateStackConfigRequest = components["schemas"]["UpdateStackConfigRequest"];

// A secret is never sent back to the browser, so the form can only keep it,
// replace it with a typed value or have the daemon make up a new one.
export type SecretMode = "keep" | "replace" | "generate";

export const KEEP: SecretMode = "keep";
export const REPLACE: SecretMode = "replace";
export const GENERATE: SecretMode = "generate";

export type ConfigEdits = {
  values: Record<string, string>;
  secrets: Record<string, SecretMode>;
};

export const NO_EDITS: ConfigEdits = { values: {}, secrets: {} };

export function secretMode(edits: ConfigEdits, name: string): SecretMode {
  return edits.secrets[name] ?? KEEP;
}

// The request that applies the edits, or null when nothing would change. A
// field is sent only if it differs from what the stack has now, so a value the
// user typed and then put back is not a change. An empty replacement secret is
// left out: the API reads an empty secret as "keep the stored value", which is
// what the form shows for it, and it must not count as a change.
export function buildConfigRequest(
  inputs: StackConfigInput[],
  edits: ConfigEdits,
): UpdateStackConfigRequest | null {
  const values: Record<string, string> = {};
  const generate: string[] = [];
  for (const input of inputs) {
    if (input.readOnly) {
      continue;
    }
    if (input.kind === "secret") {
      const mode = secretMode(edits, input.name);
      const typed = edits.values[input.name] ?? "";
      if (mode === GENERATE) {
        generate.push(input.name);
      } else if (mode === REPLACE && typed !== "") {
        values[input.name] = typed;
      }
      continue;
    }
    const typed = edits.values[input.name];
    if (typed !== undefined && typed !== (input.value ?? "")) {
      values[input.name] = typed;
    }
  }
  if (Object.keys(values).length === 0 && generate.length === 0) {
    return null;
  }
  const body: UpdateStackConfigRequest = {};
  if (Object.keys(values).length > 0) {
    body.values = values;
  }
  if (generate.length > 0) {
    body.generate = generate;
  }
  return body;
}

// The install form's field takes an install plan's input; a stack's input
// has the same label, kind and suggestions, never a requested port.
export function asTemplateInput(input: StackConfigInput): TemplateInput {
  return {
    name: input.name,
    kind: input.kind,
    role: input.role,
    label: input.label,
    description: input.description,
    value: input.value,
    generated: false,
    suggestions: input.suggestions,
  };
}
