// Generates the API reference pages (gitignored build output) from the
// OpenAPI specification: validates it, copies it to .openapi/current.yaml, and
// runs the plugin for the current docs and for every docs version. The build
// has no write access to api/.
// usage: node scripts/gen-api-docs.mjs   (run from site/, after npm ci)
// HOSERVA_API_SPEC overrides the spec, for a check on a throwaway copy.
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import {finishPages, generatedPages} from './api-pages.mjs';
import {expectations, loadSpec} from './api-spec.mjs';

const siteDir = process.cwd();
const source = path.resolve(siteDir, process.env.HOSERVA_API_SPEC ?? '../api/openapi.yaml');
const versionsFile = path.join(siteDir, 'versions.json');
const versions = fs.existsSync(versionsFile) ? JSON.parse(fs.readFileSync(versionsFile, 'utf8')) : [];

const outputs = [
  {spec: path.join(siteDir, '.openapi', 'current.yaml'), dir: path.join(siteDir, 'docs', 'reference', 'api')},
  ...versions.map((version) => ({
    spec: path.join(siteDir, 'versioned_api', `openapi-${version}.yaml`),
    dir: path.join(siteDir, 'versioned_docs', `version-${version}`, 'reference', 'api'),
  })),
];

function fail(message) {
  console.error(`gen-api-docs: ${message}`);
  process.exit(1);
}

if (!fs.existsSync(source)) fail(`${source} does not exist`);
fs.mkdirSync(path.dirname(outputs[0].spec), {recursive: true});
fs.copyFileSync(source, outputs[0].spec);

const parsed = new Map();
for (const {spec} of outputs) {
  try {
    parsed.set(spec, await loadSpec(spec));
  } catch (err) {
    fail(err.message);
  }
}

for (const {dir} of outputs) fs.rmSync(dir, {recursive: true, force: true});

const docusaurus = path.join('node_modules', '@docusaurus', 'core', 'bin', 'docusaurus.mjs');
const run = spawnSync(process.execPath, [docusaurus, 'gen-api-docs', 'all'], {cwd: siteDir, stdio: ['ignore', 'ignore', 'inherit']});
if (run.status !== 0) fail('docusaurus gen-api-docs failed');

for (const {spec, dir} of outputs) {
  try {
    finishPages(dir, parsed.get(spec));
  } catch (err) {
    fail(err.message);
  }
  const expected = expectations(parsed.get(spec));
  const generated = generatedPages(dir);
  const label = path.relative(siteDir, dir);
  for (const operation of expected.operations) {
    if (!generated.operations.has(operation)) fail(`${label} has no page for ${operation}`);
  }
  for (const schema of expected.schemas) {
    if (!generated.schemas.has(schema)) fail(`${label} has no page for the schema ${schema}`);
  }
  console.log(`gen-api-docs: ${label}: ${expected.operations.length} operations, ${expected.schemas.length} schemas`);
}
