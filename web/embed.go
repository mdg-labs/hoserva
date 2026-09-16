// Package web embeds the built single-page app (Q8) so hoservad can serve
// it (doc 01 §1) — wiring an embedded FS into cmd/hoservad's HTTP server is
// #22's job; this package only exposes it.
//
// dist/ is `make build`'s web-build step's output (`npm run build`) and is
// git-ignored, since it is derived from src/ — but a committed
// dist/.gitkeep placeholder (.gitignore) keeps //go:embed satisfied on a
// clean checkout that has never run the web build, so `go build ./...` and
// `go vet ./...` never fail for its absence. Vite empties outDir before
// writing (its default emptyOutDir behaviour) and a real build replaces the
// placeholder with the actual app; vite.config.ts's own closeBundle plugin
// then rewrites dist/.gitkeep after every build so a later clean checkout
// still has it, and `make build`/`make web-build` fail outright if
// dist/index.html — the real app, not the placeholder — is missing
// afterwards.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
