# Hoserva documentation site

The public site at <https://hoserva.dev>, built with Docusaurus: a root page (`src/pages/index.tsx`) and the documentation under `/docs/`. The design docs for developers live in `docs/internal/`, not here (Q3). The page structure is doc 05 §7's; the versioning rules are Q90 in `docs/internal/13-open-questions.md`.

```
make site-build      # from the repository root: npm ci, build, and the layout checks
cd site && npm start # local dev server
```

`npm run build` writes to `site/dist/`, which `scripts/release/assemble-pages-site.sh` publishes as the root of hoserva.dev: the root page at `/` and the docs at `/docs/`. `make site-build` also runs three checks:

- `scripts/check-layout.mjs` — every internal link in the build resolves, the root page has no version banner or `noindex` and links to `/docs/`, and `/docs/`, `/docs/next/` and older versions carry the banners, `noindex` and version dropdown the rules call for.
- `scripts/check-versioning.mjs` — makes two throwaway snapshots in a temporary copy of the site, builds it, and runs the layout check on that, so the "a version exists" layout is proven without committing a snapshot.
- `scripts/check-external.mjs` — no remote font, analytics or hosted-search reference in the source or the build.

## Writing pages

How to write a page (voice, structure, the elements to use, what a page must never contain) is in `.claude/skills/user-docs/SKILL.md`; read it before writing or editing one. The mechanics are here.

Pages are in `docs/`, one file per page of doc 05 §7's tree. `sidebars.ts` lists the tree. A page with `draft: true` in its front matter is left out of the production build, and the sidebar shows it as a "coming soon" label with no link. Remove `draft: true` and the entry becomes a real link; `sidebars.ts` needs no edit.

Any page that names Unraid renders `<TrademarkNotice />` (`src/components/TrademarkNotice`), which carries the trademark sentence from doc 00 §6.

## Versions

Before the first stable release there are no versions: the current docs are the whole docs site, at `/docs/`, with the "unreleased" banner.

**Release step.** For the first stable release of each minor (`vX.Y.0`), run this in the release-prep commit on `dev`, from `site/`:

```
npx docusaurus docs:version X.Y
```

It writes `versioned_docs/version-X.Y/`, `versioned_sidebars/version-X.Y-sidebars.json` and `versions.json`. Commit all three. They reach `main` in the normal `dev` to `main` pull request, before the tag. `release.yml` refuses a stable tag whose commit lacks `versioned_docs/version-X.Y/` or `versioned_sidebars/version-X.Y-sidebars.json`, or whose `versions.json` does not list `X.Y`, before any build or signing step; beta tags are exempt. From then on `/docs/` serves the latest stable version, the current docs move to `/docs/next/` (unreleased banner, `noindex`), older versions show the "unmaintained" banner, and the navbar gets a version dropdown. A stable patch release (`vX.Y.Z`, Z > 0) gets no new snapshot of its own; it is checked against the `X.Y` one. Beta pre-release tags are never snapshotted.

**Fixing a version.** A docs fix lands in `docs/`, and also in the latest stable snapshot when it corrects something wrong there. To fix any snapshot, edit its files in `versioned_docs/version-X.Y/` and commit; the site rebuilds on the next push to `main`, with no Hoserva release. Older snapshots are not otherwise maintained.

If the build time or the published size nears the limit (the 900 MiB canary in `assemble-pages-site.sh`), drop the oldest version: remove it from `versions.json` and delete its `versioned_docs/version-<ver>/` and `versioned_sidebars/version-<ver>-sidebars.json`. It stays in git history. Don't use `onlyIncludeVersions`: `scripts/check-layout.mjs` requires every version in `versions.json` to be built.
