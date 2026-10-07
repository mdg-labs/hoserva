// The site loads no remote font, analytics or hosted-search service (Q3, Q90):
// fonts are self-hosted, there is no analytics plugin, and search is built
// locally at build time.
// usage: node scripts/check-external.mjs <site-dir> <build-dir>
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';

const [siteDir, buildDir] = process.argv.slice(2);
if (!siteDir || !buildDir) {
  console.error('usage: check-external.mjs <site-dir> <build-dir>');
  process.exit(2);
}
if (!fs.existsSync(buildDir)) {
  console.error(`check-external: ${buildDir} does not exist - build the site first`);
  process.exit(2);
}

// Source: any mention in what is committed. Skipped are installed packages,
// build output, the lockfile (it lists the transitive packages of the preset),
// the catalog export (signed content; what it adds to the build is checked in
// the build output below) and this file, which names the patterns.
const sourcePattern =
  /fonts\.(googleapis|gstatic)\.com|googletagmanager|google-analytics|gtag|plugin-google-|algolia|docsearch|plausible|posthog|matomo|umami|mixpanel|hotjar|segment\.(com|io)|cdn\.jsdelivr|unpkg\.com|cdnjs/i;
const skip = new Set(['node_modules', 'dist', '.docusaurus', '.openapi', '.catalog', 'package-lock.json', 'check-external.mjs']);
// The generated API reference carries base64 blobs that can contain any of the
// patterns by chance; what it renders is checked in the build output instead.
const generatedApi = /^(docs|versioned_docs[\\/][^\\/]+)[\\/]reference[\\/]api$/;

// Output: a request to one of these hosts. The local search library ships
// CSS class names that contain "algolia", which are not requests.
const hostPattern =
  /https?:\/\/([a-z0-9-]+\.)*(fonts\.googleapis\.com|fonts\.gstatic\.com|googletagmanager\.com|google-analytics\.com|algolia\.net|algolianet\.com|algolia\.io|jsdelivr\.net|unpkg\.com|cdnjs\.cloudflare\.com|plausible\.io|posthog\.com|matomo\.cloud)/i;

function files(dir, skipNames, skipPath = null, root = dir) {
  return fs.readdirSync(dir, {withFileTypes: true}).flatMap((entry) => {
    if (skipNames.has(entry.name)) return [];
    const full = path.join(dir, entry.name);
    if (skipPath?.test(path.relative(root, full))) return [];
    return entry.isDirectory() ? files(full, skipNames, skipPath, root) : [full];
  });
}

const failures = [];
for (const file of files(siteDir, skip, generatedApi)) {
  const match = sourcePattern.exec(fs.readFileSync(file, 'latin1'));
  if (match) failures.push(`${path.relative(siteDir, file)} mentions "${match[0]}"`);
}
for (const file of files(buildDir, new Set())) {
  const match = hostPattern.exec(fs.readFileSync(file, 'latin1'));
  if (match) failures.push(`build output ${path.relative(buildDir, file)} references ${match[0]}`);
}

if (failures.length > 0) {
  for (const message of failures) console.error(`check-external: ${message}`);
  process.exit(1);
}
console.log('check-external: ok');
