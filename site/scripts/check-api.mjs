// Checks that a built site has the whole API reference for every docs
// version: a page for each operation, each schema and each Event member,
// every operation page with its required role and an https example that
// carries the Authorization header. Each version is checked against its own
// specification.
// usage: node scripts/check-api.mjs <site-dir> <build-dir>
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import {generatedPages} from './api-pages.mjs';
import {expectations, loadSpec} from './api-spec.mjs';

const [siteDir, buildDir] = process.argv.slice(2);
if (!siteDir || !buildDir) {
  console.error('usage: check-api.mjs <site-dir> <build-dir>');
  process.exit(2);
}

const versionsFile = path.join(siteDir, 'versions.json');
const versions = fs.existsSync(versionsFile) ? JSON.parse(fs.readFileSync(versionsFile, 'utf8')) : [];

const targets = [
  {
    label: versions.length === 0 ? 'the docs' : 'next',
    route: versions.length === 0 ? 'docs' : 'docs/next',
    spec: path.join(siteDir, '.openapi', 'current.yaml'),
    generated: path.join(siteDir, 'docs', 'reference', 'api'),
  },
  ...versions.map((version, index) => ({
    label: `version ${version}`,
    route: index === 0 ? 'docs' : `docs/${version}`,
    spec: path.join(siteDir, 'versioned_api', `openapi-${version}.yaml`),
    generated: path.join(siteDir, 'versioned_docs', `version-${version}`, 'reference', 'api'),
  })),
];

const failures = [];
const fail = (message) => failures.push(message);

const text = (file) => fs.readFileSync(file, 'utf8').replace(/<[^>]+>/g, ' ').replace(/&quot;/g, '"').replace(/\s+/g, ' ');

for (const target of targets) {
  if (!fs.existsSync(target.spec)) {
    fail(`${target.label}: ${path.relative(siteDir, target.spec)} is missing`);
    continue;
  }
  let expected;
  try {
    expected = expectations(await loadSpec(target.spec));
  } catch (err) {
    fail(`${target.label}: ${err.message}`);
    continue;
  }
  const generated = generatedPages(target.generated);
  const page = (id) => path.join(buildDir, target.route, 'reference', 'api', ...id.split('/'), 'index.html');

  for (const operation of expected.operations) {
    const id = generated.operations.get(operation);
    if (id === undefined || !fs.existsSync(page(id))) {
      fail(`${target.label}: no built page for ${operation}`);
      continue;
    }
    const body = text(page(id));
    if (!body.includes('Required role:')) fail(`${target.label}: ${operation} shows no required role`);
    const isPublic = body.includes('Required role: none');
    if (!/https:\/\/\$HOSERVA_HOST:\d+\/api\//.test(body)) fail(`${target.label}: ${operation} has no https example request`);
    if (!isPublic && !body.includes('Authorization: Bearer $HOSERVA_TOKEN')) {
      fail(`${target.label}: ${operation} has no Authorization header in its example request`);
    }
  }
  for (const schema of expected.schemas) {
    const id = generated.schemas.get(schema);
    if (id === undefined || !fs.existsSync(page(id))) fail(`${target.label}: no built page for the schema ${schema}`);
  }
  for (const event of expected.events) {
    const id = generated.schemas.get(event);
    if (id === undefined || !fs.existsSync(page(id))) fail(`${target.label}: no built page for the event type ${event}`);
  }
  console.log(`check-api: ${target.label} (/${target.route}/): ${expected.operations.length} operations, ${expected.schemas.length} schemas`);
}

if (failures.length > 0) {
  for (const message of failures.slice(0, 20)) console.error(`check-api: ${message}`);
  if (failures.length > 20) console.error(`check-api: ... and ${failures.length - 20} more`);
  process.exit(1);
}
console.log('check-api: ok');
