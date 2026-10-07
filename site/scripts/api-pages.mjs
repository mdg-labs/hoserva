// Post-processes the pages the plugin generated: adds each operation's
// required role and a runnable example request, both read from the
// operation in the specification, and keeps descriptions from being parsed
// as MDX module statements.
import fs from 'node:fs';
import path from 'node:path';
import {operationsByRoute} from './api-spec.mjs';

const ROLE_TEXT = {
  admin: 'Only an account or token with the admin role can call this operation.',
  viewer: 'An account or token with the viewer or admin role can call this operation.',
  public: 'This operation needs no credential.',
};

// A description that starts a line with the word "import" or "export" is read
// by MDX as a module statement and stops the build. The plugin's own import
// lines are left alone, and so is everything inside a code fence.
export function escapeModuleWords(markdown) {
  let fenced = false;
  return markdown
    .split('\n')
    .map((line) => {
      if (/^\s*(```|~~~)/.test(line)) fenced = !fenced;
      if (fenced || /^import \w+ from "[^"]+";$/.test(line)) return line;
      return line.replace(/^i(mport\s)|^e(xport\s)/, (_, rest, other) => (rest ? `&#105;${rest}` : `&#101;${other}`));
    })
    .join('\n');
}

function example(spec, operation, method, route) {
  const role = operation['x-hoserva-role'];
  if (!(role in ROLE_TEXT)) {
    throw new Error(`${method.toUpperCase()} ${route} has no x-hoserva-role of admin, viewer or public`);
  }
  const server = (spec.servers ?? []).find((candidate) => candidate.url.startsWith('https://'));
  if (server === undefined) throw new Error('the specification declares no https:// server');
  const base = server.url.replace(/\{(\w+)\}/g, (_, name) => `$HOSERVA_${name.toUpperCase()}`);

  const lines = [`curl --request ${method.toUpperCase()}`];
  if (role !== 'public') lines.push('--header "Authorization: Bearer $HOSERVA_TOKEN"');
  if (operation.requestBody !== undefined) lines.push('--header "Content-Type: application/json"', '--data @body.json');
  lines.push(`"${base}${route}"`);

  return [
    '',
    `**Required role:** ${role === 'public' ? 'none' : role}. ${ROLE_TEXT[role]}`,
    '',
    '```bash title="Example request"',
    lines.join(' \\\n  '),
    '```',
    '',
  ].join('\n');
}

// Rewrites every generated page under dir in place.
export function finishPages(dir, spec) {
  const operations = operationsByRoute(spec);
  const marker = '</MethodEndpoint>\n';
  const files = fs.readdirSync(dir, {recursive: true}).filter((name) => name.endsWith('.mdx'));
  for (const name of files) {
    const file = path.join(dir, name);
    let text = fs.readFileSync(file, 'utf8');
    if (name.endsWith('.api.mdx')) {
      const method = /method=\{"(\w+)"\}/.exec(text)?.[1];
      const route = /path=\{"([^"]*)"\}/.exec(text)?.[1];
      const operation = operations.get(`${method?.toUpperCase()} ${route}`);
      const at = text.indexOf(marker);
      if (operation === undefined || at < 0) {
        throw new Error(`${name}: cannot tell which operation this page documents`);
      }
      text = text.slice(0, at + marker.length) + example(spec, operation, method, route) + text.slice(at + marker.length);
    }
    fs.writeFileSync(file, escapeModuleWords(text));
  }
}

// The pages the plugin generated in dir: operations by "METHOD /route" and
// schemas by name, each with the doc id its page is served under.
export function generatedPages(dir) {
  const operations = new Map();
  const schemas = new Map();
  if (!fs.existsSync(dir)) return {operations, schemas};
  const id = (text) => /^id: (\S+)$/m.exec(text)?.[1];
  for (const name of fs.readdirSync(dir)) {
    if (!name.endsWith('.api.mdx')) continue;
    const text = fs.readFileSync(path.join(dir, name), 'utf8');
    const method = /method=\{"(\w+)"\}/.exec(text)?.[1];
    const route = /path=\{"([^"]*)"\}/.exec(text)?.[1];
    if (method !== undefined && route !== undefined) operations.set(`${method.toUpperCase()} ${route}`, id(text));
  }
  const schemaDir = path.join(dir, 'schemas');
  if (fs.existsSync(schemaDir)) {
    for (const name of fs.readdirSync(schemaDir)) {
      if (!name.endsWith('.schema.mdx')) continue;
      const text = fs.readFileSync(path.join(schemaDir, name), 'utf8');
      const title = /^title: "(.*)"$/m.exec(text)?.[1];
      if (title !== undefined) schemas.set(title, `schemas/${id(text)}`);
    }
  }
  return {operations, schemas};
}
