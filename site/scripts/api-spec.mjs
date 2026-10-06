// Reads and validates an OpenAPI specification for the API reference.
import {bundle, createConfig, lint} from '@redocly/openapi-core';

const METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];

// api/openapi.yaml is OpenAPI 3.1 but marks four nullable values with the 3.0
// keyword `nullable`, which the 3.1 schema does not know. The reference
// renders them correctly, so only this one finding is tolerated.
const tolerated = (problem) => problem.ruleId === 'struct' && problem.message === 'Property `nullable` is not expected here.';

// Returns the parsed document, or throws with every validation error.
export async function loadSpec(file) {
  const config = await createConfig({extends: ['minimal']});
  const problems = (await lint({ref: file, config})).filter((p) => p.severity === 'error' && !tolerated(p));
  if (problems.length > 0) {
    const shown = problems.slice(0, 10).map((p) => `  ${p.location?.[0]?.pointer ?? ''}: ${p.message}`);
    throw new Error(`${file} is not a valid OpenAPI document (${problems.length} errors):\n${shown.join('\n')}`);
  }
  const {bundle: result} = await bundle({ref: file, config});
  return result.parsed;
}

// Every operation of the spec, by "METHOD /route".
export function operationsByRoute(spec) {
  const found = new Map();
  for (const [route, item] of Object.entries(spec.paths ?? {})) {
    for (const method of METHODS) {
      if (item[method] !== undefined) found.set(`${method.toUpperCase()} ${route}`, item[method]);
    }
  }
  return found;
}

// What the reference has to contain: one page per operation, one per schema,
// and one per member of the Event schema.
export function expectations(spec) {
  const operations = [...operationsByRoute(spec).keys()];
  const schemas = Object.keys(spec.components?.schemas ?? {});
  const event = spec.components?.schemas?.Event ?? {};
  const members = (event.oneOf ?? event.anyOf ?? []).map((member) => member.$ref?.split('/').pop());
  if (operations.length === 0) throw new Error('the specification has no operations');
  if (!schemas.includes('Event') || members.length === 0 || members.some((m) => !schemas.includes(m))) {
    throw new Error('the specification has no Event schema with its member types');
  }
  return {operations, schemas, events: ['Event', ...members]};
}
