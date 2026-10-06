// Makes a docs version: copies the API specification to
// versioned_api/openapi-X.Y.yaml, generates the API reference so the version's
// sidebar can be written, then runs `docusaurus docs:version X.Y`.
// usage: npm run docs:version -- X.Y   (from site/)
// HOSERVA_API_SPEC overrides the specification to copy, for a check on a throwaway copy.
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const siteDir = process.cwd();
const version = process.argv[2];
if (!/^\d+\.\d+$/.test(version ?? '')) {
  console.error('usage: npm run docs:version -- X.Y');
  process.exit(2);
}

const versionsFile = path.join(siteDir, 'versions.json');
const existing = fs.existsSync(versionsFile) ? JSON.parse(fs.readFileSync(versionsFile, 'utf8')) : [];
if (existing.includes(version)) {
  console.error(`version-docs: ${version} already exists in versions.json`);
  process.exit(1);
}

const source = path.resolve(siteDir, process.env.HOSERVA_API_SPEC ?? '../api/openapi.yaml');
if (!fs.existsSync(source)) {
  console.error(`version-docs: ${source} does not exist`);
  process.exit(1);
}
const target = path.join(siteDir, 'versioned_api', `openapi-${version}.yaml`);
fs.mkdirSync(path.dirname(target), {recursive: true});
fs.copyFileSync(source, target);

function run(...args) {
  const result = spawnSync(process.execPath, args, {cwd: siteDir, stdio: 'inherit'});
  if (result.status !== 0) {
    console.error(`version-docs: ${args.join(' ')} failed`);
    process.exit(1);
  }
}

run(path.join('scripts', 'gen-api-docs.mjs'));
run(path.join('node_modules', '@docusaurus', 'core', 'bin', 'docusaurus.mjs'), 'docs:version', version);

// The snapshot copied the generated pages of the current docs; the version's
// own are generated from its specification at every build.
fs.rmSync(path.join(siteDir, 'versioned_docs', `version-${version}`, 'reference', 'api'), {recursive: true, force: true});
console.log(`version-docs: ${version} done; commit versioned_docs/version-${version}/, versioned_sidebars/, versioned_api/openapi-${version}.yaml and versions.json`);
