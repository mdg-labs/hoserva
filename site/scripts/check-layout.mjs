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

// The site root is the landing page, outside the versioned docs: it carries
// no version banner, is not marked noindex, and leads to the docs.
const home = readPage('index.html');
if (home.includes(UNRELEASED) || home.includes(UNMAINTAINED)) fail('the site root shows a version banner');
if (NOINDEX.test(home)) fail('the site root is marked noindex');
// Only the page body counts: the navbar of a versioned build also links into the docs.
const homeBody = /<main[\s\S]*<\/main>/.exec(home)?.[0] ?? '';
if (home !== '' && !/\bhref="\/docs\/"/.test(homeBody)) fail('the site root body does not link to /docs/');

if (versions.length === 0) {
  const docs = readPage('docs/index.html');
  if (!docs.includes(UNRELEASED)) fail('no versions: /docs/ does not show the unreleased banner');
  if (NOINDEX.test(docs)) fail('no versions: /docs/ is marked noindex');
  if (docs.includes(DROPDOWN)) fail('no versions: the navbar has a version dropdown');
  if (fs.existsSync(path.join(buildDir, 'docs', 'next'))) fail('no versions: /docs/next/ exists');
} else {
  const docs = readPage('docs/index.html');
  if (docs.includes(UNRELEASED) || docs.includes(UNMAINTAINED)) fail(`/docs/ (${versions[0]}) shows a version banner`);
  if (NOINDEX.test(docs)) fail(`/docs/ (${versions[0]}) is marked noindex`);
  if (!docs.includes(DROPDOWN)) fail('the navbar has no version dropdown');
  const next = readPage('docs/next/index.html');
  if (!next.includes(UNRELEASED)) fail('/docs/next/ does not show the unreleased banner');
  if (!NOINDEX.test(next)) fail('/docs/next/ is not marked noindex');
  for (const older of versions.slice(1)) {
    const page = readPage(`docs/${older}/index.html`);
    if (!page.includes(UNMAINTAINED)) fail(`/docs/${older}/ does not show the unmaintained banner`);
    if (NOINDEX.test(page)) fail(`/docs/${older}/ is marked noindex`);
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
