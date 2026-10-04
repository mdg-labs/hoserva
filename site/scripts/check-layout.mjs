// Checks a built site against the versioning rules (docs/internal/13-open-questions.md Q90).
// usage: node scripts/check-layout.mjs <site-dir> <build-dir>
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const [siteDir, buildDir] = process.argv.slice(2);
if (!siteDir || !buildDir) {
  console.error('usage: check-layout.mjs <site-dir> <build-dir>');
  process.exit(2);
}

const failures = [];
const fail = (message) => failures.push(message);

function readPage(rel) {
  const file = path.join(buildDir, rel);
  if (!fs.existsSync(file)) {
    fail(`${rel} is missing from the build`);
    return '';
  }
  return fs.readFileSync(file, 'utf8');
}

function htmlFiles(dir) {
  return fs.readdirSync(dir, {withFileTypes: true}).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return htmlFiles(full);
    return entry.name.endsWith('.html') ? [full] : [];
  });
}

const UNRELEASED = 'unreleased documentation';
const UNMAINTAINED = 'no longer actively maintained';
const NOINDEX = /name="robots" content="noindex/;
const DROPDOWN = 'navbar__item dropdown';

const versionsFile = path.join(siteDir, 'versions.json');
const versions = fs.existsSync(versionsFile) ? JSON.parse(fs.readFileSync(versionsFile, 'utf8')) : [];

if (!fs.existsSync(path.join(buildDir, 'index.html'))) {
  fail('index.html is missing from the build');
}

if (versions.length === 0) {
  const root = readPage('index.html');
  if (!root.includes(UNRELEASED)) fail('no versions: the root does not show the unreleased banner');
  if (NOINDEX.test(root)) fail('no versions: the root is marked noindex');
  if (root.includes(DROPDOWN)) fail('no versions: the navbar has a version dropdown');
  if (fs.existsSync(path.join(buildDir, 'next'))) fail('no versions: /next/ exists');
} else {
  const root = readPage('index.html');
  if (root.includes(UNRELEASED) || root.includes(UNMAINTAINED)) fail(`the root (${versions[0]}) shows a version banner`);
  if (NOINDEX.test(root)) fail(`the root (${versions[0]}) is marked noindex`);
  if (!root.includes(DROPDOWN)) fail('the navbar has no version dropdown');
  const next = readPage('next/index.html');
  if (!next.includes(UNRELEASED)) fail('/next/ does not show the unreleased banner');
  if (!NOINDEX.test(next)) fail('/next/ is not marked noindex');
  for (const older of versions.slice(1)) {
    const page = readPage(`${older}/index.html`);
    if (!page.includes(UNMAINTAINED)) fail(`/${older}/ does not show the unmaintained banner`);
    if (NOINDEX.test(page)) fail(`/${older}/ is marked noindex`);
  }
}

// Every internal link on every page, which includes the sidebar and navbar,
// has to resolve to a file in the build.
const hrefs = /\bhref="(\/[^"/][^"]*|\/)"/g;
const checked = new Set();
for (const file of htmlFiles(buildDir)) {
  for (const [, href] of fs.readFileSync(file, 'utf8').matchAll(hrefs)) {
    const target = decodeURI(href.split(/[?#]/)[0]);
    if (checked.has(target)) continue;
    checked.add(target);
    const candidates = [target, path.join(target, 'index.html'), `${target}.html`];
    if (!candidates.some((c) => fs.existsSync(path.join(buildDir, c)) && fs.statSync(path.join(buildDir, c)).isFile())) {
      fail(`${path.relative(buildDir, file)} links to ${href}, which is not in the build`);
    }
  }
}

if (failures.length > 0) {
  for (const message of failures) console.error(`check-layout: ${message}`);
  process.exit(1);
}
console.log(`check-layout: ok (${versions.length === 0 ? 'no versions' : `versions ${versions.join(', ')}`}; ${checked.size} internal links resolve)`);
