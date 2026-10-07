// Turns site/.catalog/ (written by scripts/devenv/catalog-export from the
// signed catalog archive, which that program verifies) into the /apps list and
// one /apps/<id> page per template. The icons and screenshots under
// .catalog/static/ are served as static files (staticDirectories in
// docusaurus.config.ts). A missing or malformed .catalog fails the build.
const fs = require('node:fs');
const path = require('node:path');

const ID = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const FILE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

function fail(file, message) {
  throw new Error(`${file}: ${message}`);
}

function readCatalog(file) {
  if (!fs.existsSync(file)) {
    fail(file, 'is missing: run `make site-build` from the repository root, which exports the signed catalog first');
  }
  const catalog = JSON.parse(fs.readFileSync(file, 'utf8'));
  if (!Number.isInteger(catalog.serial) || catalog.serial <= 0) fail(file, 'carries no serial');
  if (!Array.isArray(catalog.templates)) fail(file, 'has no templates list');
  const seen = new Set();
  for (const t of catalog.templates) {
    if (typeof t.id !== 'string' || !ID.test(t.id) || seen.has(t.id)) fail(file, `lists the template id ${JSON.stringify(t.id)} twice or invalidly`);
    seen.add(t.id);
    if (typeof t.title !== 'string' || t.title === '') fail(file, `template ${t.id} has no title`);
    if (!Array.isArray(t.categories) || !Array.isArray(t.screenshots)) fail(file, `template ${t.id} lacks categories or screenshots`);
    for (const name of [...(t.icon ? [t.icon] : []), ...t.screenshots]) {
      if (!FILE.test(name) || !fs.existsSync(path.join(path.dirname(file), 'static', 'apps', t.id, name))) {
        fail(file, `template ${t.id} names the file ${JSON.stringify(name)}, which is not in static/apps/${t.id}/`);
      }
    }
  }
  return catalog;
}

module.exports = function catalogPlugin(context) {
  const file = path.join(context.siteDir, '.catalog', 'index.json');
  return {
    name: 'catalog',
    getPathsToWatch() {
      return [file];
    },
    async loadContent() {
      return readCatalog(file);
    },
    async contentLoaded({content, actions}) {
      const {createData, addRoute} = actions;
      const {serial, generatedAt, templates} = content;
      const listData = await createData('catalog.json', JSON.stringify({serial, generatedAt, templates}));
      addRoute({
        path: '/apps',
        exact: true,
        component: '@site/src/components/apps/AppsPage',
        modules: {catalog: listData},
      });
      for (const app of templates) {
        const appData = await createData(`app-${app.id}.json`, JSON.stringify({app}));
        addRoute({
          path: `/apps/${app.id}`,
          exact: true,
          component: '@site/src/components/apps/AppPage',
          modules: {data: appData},
        });
      }
    },
  };
};
