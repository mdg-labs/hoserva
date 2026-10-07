# Hoserva documentation site

The public site at <https://hoserva.dev>, built with Docusaurus: a root page (`src/pages/index.tsx`) and the documentation under `/docs/`. The design docs for developers live in `docs/internal/`, not here (Q3). The page structure is doc 05 §7's; the versioning rules are Q90 in `docs/internal/13-open-questions.md`.

```
make site-build      # from the repository root: npm ci, build, and the layout checks
cd site && npm start # local dev server
```

`npm run build` writes to `site/dist/`, which `scripts/release/assemble-pages-site.sh` publishes as the root of hoserva.dev: the root page at `/` and the docs at `/docs/`. `make site-build` also runs three checks:

- `scripts/check-layout.mjs` — every internal link in the build resolves, the root page has no version banner or `noindex` and links to `/docs/`, and `/docs/`, `/docs/next/` and older versions carry the banners, `noindex` and version dropdown the rules call for.
- `scripts/check-api.mjs` — every operation, schema and Event type of the specification has a built reference page, and every operation page shows its required role and an `https://` example with the `Authorization` header.
- `scripts/check-versioning.mjs` — makes two throwaway snapshots in a temporary copy of the site, builds it, and runs the layout and API checks on that, so the "a version exists" layout, and a reference built from each version's own specification, are proven without committing a snapshot.
- `scripts/check-external.mjs` — no remote font, analytics or hosted-search reference in the source or the build.

## API reference

`/docs/reference/api` is generated from `api/openapi.yaml` by `docusaurus-plugin-openapi-docs` and its theme. `npm run build` and `npm start` run `scripts/gen-api-docs.mjs` first: it validates the specification (an invalid one fails the build), copies it to `.openapi/current.yaml`, generates the pages, and checks that each operation and schema got one. `scripts/api-pages.mjs` then adds each operation's required role and an example request, both read from the specification. The generated pages, `.openapi/` and the same pages inside `versioned_docs/` are build output: they are gitignored (`site/.gitignore`) and never committed. Run `npm run gen-api` to regenerate them without a full build.

Each docs version is built from its own copy of the specification, `versioned_api/openapi-X.Y.yaml`, so a snapshot keeps the API as it was released while the pages stay generated. The current docs read `api/openapi.yaml`. The build fails if `versions.json` lists a version that has no copy.

## Theme

The site takes the web UI's look (coss ui, D15): its palette in light and dark, radius, shadows and type, with `respectPrefersColorScheme` unchanged. The values are copied, never imported (`site/` does not import from `web/`).

- **Token source.** `src/css/coss.css` holds the tokens copied from `web/src/index.css` (`:root` for light; the web UI's `.dark` is `[data-theme='dark']` here). When the web UI's palette changes, copy it again.
- **Infima mapping.** `src/css/infima.css` maps those tokens onto Infima's variables (`--ifm-*`), the API reference theme's (`--openapi-*`) and the search plugin's (`--search-local-*`), and styles the stock navbar, sidebar, footer, admonitions, tables, tabs and code blocks. `src/lib/prism.ts` is the code-block theme: its token colours are CSS variables defined per colour mode in `infima.css`.
- **Tailwind v4** comes in through `plugins/tailwind.js`, which puts `@tailwindcss/postcss` first in Docusaurus's PostCSS chain. `src/css/custom.css` imports Tailwind's theme and utilities without its preflight, so Infima's layout on the docs pages is untouched. Utilities are `important` so Infima's element rules (`a:hover`) never override a coss component, and only `src/components`, `src/pages` and `src/theme` are scanned for classes.
- **coss components** in `src/components/ui/` (Button, Card, Badge, Input) are copied from `web/src/components/ui/`, each with its source in a header comment. Add another by copying it the same way. Pages import them from `@site/src/components/ui/`.
- **Swizzled components: none.** Everything is reached with CSS. If one is ever needed, list it here with the reason.
- **Footer.** The footer is styled, but none is configured: the site has no footer content yet.

## Writing pages

How to write a page (voice, structure, the elements to use, what a page must never contain) is in `.claude/skills/user-docs/SKILL.md`; read it before writing or editing one. The mechanics are here.

Pages are in `docs/`, one file per page of doc 05 §7's tree. `sidebars.ts` lists the tree. A page with `draft: true` in its front matter is left out of the production build, and the sidebar shows it as a "coming soon" label with no link. Remove `draft: true` and the entry becomes a real link; `sidebars.ts` needs no edit.

Any page that names Unraid renders `<TrademarkNotice />` (`src/components/TrademarkNotice`), which carries the trademark sentence from doc 00 §6.

## Versions

Before the first stable release there are no versions: the current docs are the whole docs site, at `/docs/`, with the "unreleased" banner.

**Release step.** For the first stable release of each minor (`vX.Y.0`), run this in the release-prep commit on `dev`, from `site/`:

```
npm run docs:version -- X.Y
```

It copies `../api/openapi.yaml` to `versioned_api/openapi-X.Y.yaml`, generates the API reference (the version's sidebar includes its operation list), runs `docusaurus docs:version X.Y`, and drops the generated reference pages from the snapshot. It writes `versioned_docs/version-X.Y/`, `versioned_sidebars/version-X.Y-sidebars.json`, `versioned_api/openapi-X.Y.yaml` and `versions.json`. Commit all four. They reach `main` in the normal `dev` to `main` pull request, before the tag. `release.yml` refuses a stable tag whose commit lacks `versioned_docs/version-X.Y/`, `versioned_sidebars/version-X.Y-sidebars.json` or `versioned_api/openapi-X.Y.yaml`, or whose `versions.json` does not list `X.Y`, before any build or signing step; beta tags are exempt. From then on `/docs/` serves the latest stable version, the current docs move to `/docs/next/` (unreleased banner, `noindex`), older versions show the "unmaintained" banner, and the navbar gets a version dropdown. A stable patch release (`vX.Y.Z`, Z > 0) gets no new snapshot of its own; it is checked against the `X.Y` one. Beta pre-release tags are never snapshotted.

**Fixing a version.** A docs fix lands in `docs/`, and also in the latest stable snapshot when it corrects something wrong there. To fix any snapshot, edit its files in `versioned_docs/version-X.Y/` and commit; the site rebuilds on the next push to `main`, with no Hoserva release. Older snapshots are not otherwise maintained.

If the build time or the published size nears the limit (the 900 MiB canary in `assemble-pages-site.sh`), drop the oldest version: remove it from `versions.json` and delete its `versioned_docs/version-<ver>/`, `versioned_sidebars/version-<ver>-sidebars.json` and `versioned_api/openapi-<ver>.yaml`. Each version's API reference adds about 50 MB of HTML to the build. It stays in git history. Don't use `onlyIncludeVersions`: `scripts/check-layout.mjs` requires every version in `versions.json` to be built.
