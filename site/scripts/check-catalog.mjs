// Checks that the build renders the catalog export: /apps lists every template
// of site/.catalog/index.json, each template has its own page, and every icon
// and screenshot a page shows is in the build. A catalog with no template shows
// the empty state instead of a list.
// usage: node scripts/check-catalog.mjs <site-dir> <build-dir>
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import {escapeText} from './html-text.mjs';

const [siteDir, buildDir] = process.argv.slice(2);
if (!siteDir || !buildDir) {
  console.error('usage: check-catalog.mjs <site-dir> <build-dir>');
  process.exit(2);
}

const failures = [];
const fail = (message) => failures.push(message);

const exportFile = path.join(siteDir, '.catalog', 'index.json');
if (!fs.existsSync(exportFile)) {
  console.error(`check-catalog: ${exportFile} is missing - run make site-catalog first`);
  process.exit(2);
}
const catalog = JSON.parse(fs.readFileSync(exportFile, 'utf8'));

function page(rel) {
  const file = path.join(buildDir, rel, 'index.html');
  if (!fs.existsSync(file)) {
    fail(`/${rel}/ is missing from the build`);
    return '';
  }
  return fs.readFileSync(file, 'utf8');
}

const list = page('apps');
const emptyState = 'The catalog has no apps yet';
if (catalog.templates.length === 0) {
  if (!list.includes(emptyState)) fail('the catalog has no templates, but /apps does not show the empty state');
} else if (list.includes(emptyState)) {
  fail('/apps shows the empty state although the catalog has templates');
}

let files = 0;
for (const app of catalog.templates) {
  const html = page(`apps/${app.id}`);
  if (html !== '' && !html.includes(escapeText(app.title))) fail(`/apps/${app.id}/ does not show the title ${app.title}`);
  if (!new RegExp(`href="/apps/${app.id}"`).test(list)) fail(`/apps does not link to /apps/${app.id}`);
  for (const file of [...(app.icon ? [app.icon] : []), ...app.screenshots]) {
    files += 1;
    const built = path.join(buildDir, 'apps', app.id, file);
    if (!fs.existsSync(built)) fail(`/apps/${app.id}/${file} is missing from the build`);
    if (!(app.icon === file ? list : html).includes(`src="/apps/${app.id}/${file}"`)) {
      fail(`${app.icon === file ? '/apps' : `/apps/${app.id}/`} does not show /apps/${app.id}/${file}`);
    }
  }
}

if (failures.length > 0) {
  for (const message of failures) console.error(`check-catalog: ${message}`);
  process.exit(1);
}
console.log(`check-catalog: ok (serial ${catalog.serial}; ${catalog.templates.length} templates, ${files} images)`);
