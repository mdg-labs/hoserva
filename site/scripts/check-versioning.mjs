// Proves the "a version exists" layout (docs/internal/13-open-questions.md Q90)
// on a throwaway copy of the site: it makes two snapshots with
// `docusaurus docs:version` and builds, so no versioned_docs/ lands in the
// repository. The real site's versions.json, if any, is dropped from the copy,
// so the result does not depend on which versions have been released.
// usage: node scripts/check-versioning.mjs   (run from site/, after npm ci)
import {spawnSync} from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';

const siteDir = process.cwd();
const skip = new Set(['node_modules', 'dist', '.docusaurus', 'versions.json', 'versioned_docs', 'versioned_sidebars']);
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'hoserva-site-versioning-'));

try {
  const copy = path.join(work, 'site');
  fs.cpSync(siteDir, copy, {
    recursive: true,
    filter: (src) => !skip.has(path.basename(src)) || path.dirname(src) !== siteDir,
  });
  // The copy links the installed packages but keeps its own bundler cache, so
  // it never reads or overwrites the real site's.
  const modules = path.join(siteDir, 'node_modules');
  fs.mkdirSync(path.join(copy, 'node_modules'));
  for (const entry of fs.readdirSync(modules)) {
    if (entry !== '.cache') fs.symlinkSync(path.join(modules, entry), path.join(copy, 'node_modules', entry));
  }

  const docusaurus = (...args) => {
    const result = spawnSync(process.execPath, [path.join('node_modules', '@docusaurus', 'core', 'bin', 'docusaurus.mjs'), ...args], {
      cwd: copy,
      stdio: ['ignore', 'inherit', 'inherit'],
    });
    if (result.status !== 0) {
      throw new Error(`docusaurus ${args.join(' ')} failed in the throwaway copy`);
    }
  };

  docusaurus('docs:version', '99.1');
  docusaurus('docs:version', '99.2');
  docusaurus('build', '--out-dir', 'dist');

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
