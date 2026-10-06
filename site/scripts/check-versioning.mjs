// Proves the "a version exists" layout (docs/internal/13-open-questions.md Q90)
// on a throwaway copy of the site: it makes two snapshots with
// `npm run docs:version` and builds, so no versioned_docs/ lands in the
// repository. The real site's versions.json, if any, is dropped from the copy,
// so the result does not depend on which versions have been released.
// The first snapshot is made from a specification with one operation renamed,
// so the build has to show each version's API reference from its own copy.
// usage: node scripts/check-versioning.mjs   (run from site/, after npm ci)
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';

const siteDir = process.cwd();
const skip = new Set(['node_modules', 'dist', '.docusaurus', '.openapi', 'versions.json', 'versioned_docs', 'versioned_sidebars', 'versioned_api']);
const generatedApi = path.join(siteDir, 'docs', 'reference', 'api');
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'hoserva-site-versioning-'));

try {
  const copy = path.join(work, 'site');
  fs.cpSync(siteDir, copy, {
    recursive: true,
    filter: (src) => src !== generatedApi && (!skip.has(path.basename(src)) || path.dirname(src) !== siteDir),
  });
  // The copy links the installed packages but keeps its own bundler cache, so
  // it never reads or overwrites the real site's.
  const modules = path.join(siteDir, 'node_modules');
  fs.mkdirSync(path.join(copy, 'node_modules'));
  for (const entry of fs.readdirSync(modules)) {
    if (entry !== '.cache') fs.symlinkSync(path.join(modules, entry), path.join(copy, 'node_modules', entry));
  }

  const node = (cwdScript, args, env) => {
    const result = spawnSync(process.execPath, [cwdScript, ...args], {
      cwd: copy,
      env: {...process.env, ...env},
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    if (result.status !== 0) {
      throw new Error(`${cwdScript} ${args.join(' ')} failed in the throwaway copy`);
    }
  };
  const docusaurus = path.join('node_modules', '@docusaurus', 'core', 'bin', 'docusaurus.mjs');

  const spec = path.resolve(siteDir, process.env.HOSERVA_API_SPEC ?? '../api/openapi.yaml');
  const marker = 'List jobs (snapshot marker)';
  const original = fs.readFileSync(spec, 'utf8');
  const renamed = original.replace('summary: List jobs\n', `summary: ${marker}\n`);
  if (renamed === original) throw new Error('the specification has no "summary: List jobs" operation to rename');
  const renamedSpec = path.join(work, 'renamed.yaml');
  fs.writeFileSync(renamedSpec, renamed);

  node(path.join('scripts', 'version-docs.mjs'), ['99.1'], {HOSERVA_API_SPEC: renamedSpec});
  node(path.join('scripts', 'version-docs.mjs'), ['99.2'], {HOSERVA_API_SPEC: spec});
  node(path.join('scripts', 'gen-api-docs.mjs'), [], {HOSERVA_API_SPEC: spec});
  node(docusaurus, ['build', '--out-dir', 'dist'], {HOSERVA_API_SPEC: spec});

  // The page of the renamed operation is the one whose text carries the marker.
  const referenceMentions = (route, text) => {
    const dir = path.join(copy, 'dist', route, 'reference', 'api');
    return fs.readdirSync(dir, {withFileTypes: true}).some(
      (entry) => entry.isDirectory() && fs.existsSync(path.join(dir, entry.name, 'index.html')) &&
        fs.readFileSync(path.join(dir, entry.name, 'index.html'), 'utf8').includes(text),
    );
  };
  const expectMarker = (route, present) => {
    if (referenceMentions(route, marker) !== present) {
      throw new Error(`the /${route}/ API reference ${present ? 'does not show' : 'shows'} the operation renamed in the 99.1 specification`);
    }
  };
  expectMarker('docs/99.1', true);
  expectMarker('docs', false);
  expectMarker('docs/next', false);

  const api = spawnSync(process.execPath, [path.join(siteDir, 'scripts', 'check-api.mjs'), copy, path.join(copy, 'dist')], {
    stdio: 'inherit',
  });
  if (api.status !== 0) {
    throw new Error('the versioned API reference check failed');
  }

  const layout = spawnSync(process.execPath, [path.join(siteDir, 'scripts', 'check-layout.mjs'), copy, path.join(copy, 'dist')], {
    stdio: 'inherit',
  });
  if (layout.status !== 0) {
    throw new Error('the versioned layout check failed');
  }
} catch (err) {
  console.error(`check-versioning: ${err.message}`);
  process.exitCode = 1;
} finally {
  fs.rmSync(work, {recursive: true, force: true});
}
