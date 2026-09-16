# Every workflow goes through this Makefile (doc 12 §3) — if it isn't a
# target here, it doesn't exist. Only the targets this repository can
# actually satisfy today are defined; the rest (dev, db-migration, vm-*,
# deb, iso, ...) arrive with the issues that build what they need, so no
# target here pretends to work.

GO           ?= go
BIN_DIR      := bin
CMDS         := hoservad hoserva mockapi

# gen and api-check (issue #17, D18, Q63) need a Node toolchain for the
# TypeScript client and Spectral lint — installed by the CI job itself, or
# on PATH in a developer shell. Neither target reaches for a specific
# install method; `npm` is assumed present, exactly like `$(GO)` above.
NPM          ?= npm

# The loop-device lab (doc 06 §3, Q45). HOSERVA_LAB_ID namespaces the
# container name, the compose project (so two ids never share a network)
# and the bind-mounted image/mount root, so parallel labs never collide.
# Never started with a bare `docker run` — see docker-compose.dev.yml.
#
# HOSERVA_LAB_ID reaches directory names, a container name and an `rm -rf`
# argument below, so it is validated by lab-require-id — which shells out to
# scripts/devenv/lib.sh's own lab_id_valid, so there is exactly one
# implementation of the pattern, not two that can drift apart — before any
# target uses it. Every recipe below reads it as `$$HOSERVA_LAB_ID` (a
# literal `$` escaped for Make), never as Make's own `$(HOSERVA_LAB_ID)`
# substitution: Make performs its variable/function substitution on recipe
# text before the shell ever sees it, so a raw, not-yet-validated value
# containing shell metacharacters would otherwise get a chance to run
# *during* validation itself. `$$VAR` instead defers to a plain shell
# environment-variable read at run time, which never re-interprets its own
# value as code.
#
# A second, independent hazard: GNU Make auto-exports every
# command-line-supplied variable (`make target VAR=value`, as opposed to a
# plain environment variable) into the environment of any recipe that is
# about to run, and doing that requires Make to expand that variable's own
# stored text *as Make syntax* — `$(shell ...)` included — before the
# recipe's shell ever starts. That happens for any target, whether or not a
# rule below ever references the variable, and it happens before any
# recipe-level check (including the one above) gets to run. `unexport`
# below stops HOSERVA_LAB_ID from being auto-exported at all; reading it
# through `$(value ...)` (which returns a variable's literal text without
# expanding anything it contains — the one Make primitive built for exactly
# this) then lets us refuse a value containing a literal `$` — the one
# character that syntax needs — while it is still guaranteed inert, before
# Make ever gets a chance to export (and thereby expand) it. Only once that
# is confirmed do we `export` it: expanding a `$`-free string is a
# guaranteed no-op, so exporting it from here on is safe either way it was
# set (environment or command line).
#
# Trust boundary, established while hardening LAB_COMPOSE_EXTRA (issue
# #127) and confirmed empirically: this guard, and LAB_COMPOSE_EXTRA's
# below, assume their variable arrives as a plain command-line variable
# (`make target VAR=value`) or a plain environment variable — the two
# vectors the unexport/$(value ...)/export dance defends against. Neither
# defends against a value delivered through MAKEFLAGS. GNU Make treats
# `VAR=value` pairs found in the MAKEFLAGS environment variable as
# additional command-line arguments and force-expands them (running
# `$(shell ...)` if present) as part of Make's own startup, before this
# Makefile's first line runs — so by the time either guard executes, a
# `$(shell ...)` payload smuggled in through MAKEFLAGS has already run,
# and the variable already holds its (usually empty) result, which the
# guard then sees as clean. Confirmed:
#   env MAKEFLAGS='HOSERVA_LAB_ID=$(shell touch /tmp/x)' make -n lab-up
# creates /tmp/x and exits 0 — with output identical to the unset case.
# This is not new with LAB_COMPOSE_EXTRA: the same MAKEFLAGS payload against
# HOSERVA_LAB_ID's guard, unchanged since before issue #127, behaves
# identically. Nothing expressible in Makefile syntax can close this,
# because whoever controls MAKEFLAGS already has unconditional code
# execution independent of any variable this file inspects. Confirmed:
#   env MAKEFLAGS='--eval=$(shell touch /tmp/x)' make -n lab-up
# creates /tmp/x too, from Make's own command-line-flag parsing, before any
# makefile — this one or any other — is even read; no HOSERVA_LAB_ID or
# LAB_COMPOSE_EXTRA involved at all. So MAKEFLAGS, and the environment
# `make` itself runs in, are trusted input here, on the same basis CLAUDE.md
# already gives the shell that invokes `make`: setting MAKEFLAGS already
# implies control of make's environment. These two guards defend against a
# stray or mistyped ordinary value (`make lab-up LAB_COMPOSE_EXTRA=...`, or
# a plain exported variable); they are not, and cannot be, a defense
# against a MAKEFLAGS-level attacker.
unexport HOSERVA_LAB_ID
ifneq ($(findstring $$,$(value HOSERVA_LAB_ID)),)
$(error invalid HOSERVA_LAB_ID: must not contain '$$' — no Make or shell expansion syntax is accepted in a lab id; set a plain id, e.g. HOSERVA_LAB_ID=dev)
endif
export HOSERVA_LAB_ID
COMPOSE_DEV     := docker compose -f docker-compose.dev.yml
# Optional second compose file, appended only when set. Lets a lab-up/
# lab-destroy run vary one thing about the standing lab recipe through the
# real `make` targets instead of a hand-rolled replica of them (CLAUDE.md:
# "orchestrate, never reimplement") — e.g. S9/issue #10's
# LAB_COMPOSE_EXTRA=scripts/devenv/docker-compose.apparmor-default.yml.
# docker-compose.dev.yml itself is never edited for this. Unset by default,
# so the standing lab's behaviour is unchanged; set only from a trusted
# workflow or developer shell to a plain repo-relative compose file path.
#
# Hardened by issue #127: this text is spliced into COMPOSE_DEV below via
# Make's own `$(...)` substitution, not read by a recipe as a shell
# variable the way `$$HOSERVA_LAB_ID` is above — so an unvalidated value
# would reach the shell as literal, unquoted command-line text next to
# `-f`. It gets the same unexport/`$(value ...)`/export dance
# HOSERVA_LAB_ID gets above, against Make's command-line/environment
# auto-export hazard (exporting a variable requires Make to expand its
# stored text as Make syntax first, `$(shell ...)` included, before any
# check below runs), then a character whitelist mirroring LAB_ID_PATTERN,
# so every check after the whitelist can safely splice the now-known-inert
# value straight into a shell command of its own.
#
# This guard, like HOSERVA_LAB_ID's above, trusts MAKEFLAGS and the
# environment `make` itself runs in — see the trust-boundary note above
# HOSERVA_LAB_ID's `unexport` for why, and the commands that confirm it.
unexport LAB_COMPOSE_EXTRA
ifneq ($(findstring $$,$(value LAB_COMPOSE_EXTRA)),)
$(error invalid LAB_COMPOSE_EXTRA: must not contain '$$' — no Make or shell expansion syntax is accepted in a compose file path; set a plain repo-relative path, e.g. LAB_COMPOSE_EXTRA=scripts/devenv/docker-compose.apparmor-default.yml)
endif
export LAB_COMPOSE_EXTRA
ifneq ($(strip $(LAB_COMPOSE_EXTRA)),)
LAB_COMPOSE_EXTRA_PATTERN := ^[a-zA-Z0-9][a-zA-Z0-9_./-]*$$
# The whitelist check reads the value only via the shell's own
# `$$LAB_COMPOSE_EXTRA` (exported above), never by splicing
# `$(LAB_COMPOSE_EXTRA)`'s raw text into this command line — until this
# check passes, the value is not yet known to be free of shell
# metacharacters (quotes, `;`, backticks, spaces, newlines) that splicing
# it directly would let the shell reinterpret. `grep -z` (not plain `-E`)
# matters here: without it, `^`/`$` anchor per *line*, so a value with an
# embedded newline whose first line alone matches the pattern — e.g.
# "ok.yml\n;rm -rf ~" — would wrongly pass; `-z` anchors to the whole
# NUL-delimited input instead, so the embedded newline has to match too.
ifeq ($(shell printf '%s' "$$LAB_COMPOSE_EXTRA" | grep -zEq '$(LAB_COMPOSE_EXTRA_PATTERN)' && echo ok),)
$(error invalid LAB_COMPOSE_EXTRA '$(LAB_COMPOSE_EXTRA)': must match $(LAB_COMPOSE_EXTRA_PATTERN) — a repo-relative path (letters, digits, '_', '.', '/', '-' only, starting with a letter or digit — no leading '/', spaces, quotes, ';', backticks, newlines or other shell metacharacters))
endif
ifneq ($(findstring ..,$(LAB_COMPOSE_EXTRA)),)
$(error invalid LAB_COMPOSE_EXTRA '$(LAB_COMPOSE_EXTRA)': must not contain '..')
endif
# From here on the value is known to contain only the whitelisted
# characters, so splicing it into these checks' own shell command lines is
# safe the same way splicing it into COMPOSE_DEV below is.
ifeq ($(shell test -f '$(LAB_COMPOSE_EXTRA)' && echo ok),)
$(error invalid LAB_COMPOSE_EXTRA '$(LAB_COMPOSE_EXTRA)': not a repo-relative path to an existing file)
endif
# A shell `case`/`esac` test here would work logically but breaks Make's
# own $(shell ...) argument scan: Make finds where a $(shell ...) call's
# text ends by counting every '(' and ')' character in it, including ones
# meant purely for the shell, not just the ones that open a nested Make
# reference — a case pattern's bare, unmatched ')' reads to Make as the
# outer call's own closing paren, silently truncating everything after it.
# An earlier version of this check used $(filter $(CURDIR)/%,...) to avoid
# that hazard, but $(filter PATTERN,TEXT) itself splits both PATTERN and
# TEXT on whitespace before matching, so a CURDIR containing a space split
# the containment pattern into independent words — the word after the last
# space, with the appended '/%', became a standalone glob that an unrelated
# outside path could satisfy just by also containing a space in the right
# place. Confirmed exploitable: a repo checked out under
# ".../My Projects/hoserva", with LAB_COMPOSE_EXTRA a symlink resolving to
# ".../x Projects/hoserva/evil.yml" (no relation to the repo), passed the
# old check — "Projects/hoserva/evil.yml" matched the split-off pattern
# word "Projects/hoserva/%".
#
# A later version read $(CURDIR) into a shell double-quoted operand
# ("$(CURDIR)"/) of bash's own `#` prefix-strip. Double quotes stop word
# splitting and globbing, but not `$(...)` or backtick command
# substitution — so a checkout directory literally named
# "evil$(touch marker)dir" made that $(shell ...) call run an arbitrary
# command taken from the directory name, before this check's own
# pass/fail verdict was even reached (issue #127, third review round).
#
# This version never hands $(CURDIR) or LAB_COMPOSE_EXTRA to a shell at
# all: $(realpath ...) and $(subst ...) are both pure Make functions that
# work on literal text, with no re-parsing of that text as shell or Make
# syntax — nothing in either value (parens, `$(...)`, backticks, quotes,
# commas) is ever executed.
#
# The prefix compared against is the bare $(CURDIR), not
# $(realpath $(CURDIR)): Make sets CURDIR from a getcwd(3)-style call at
# startup, which on Linux already returns the kernel's canonical path —
# no symlink component survives in it — so re-resolving it buys nothing.
# Confirmed empirically: cd into a symlink pointing at this repo and run
# `make -p -n lab-up` — CURDIR already reports the symlink's real target.
# Re-resolving it would also be actively wrong: $(realpath ...), like
# $(wildcard ...) and $(abspath ...), treats its argument as a
# whitespace-separated *list* of names (so it can resolve several at
# once) — so a repo checked out under a path containing a space would
# have $(realpath $(CURDIR)) silently split it into two fragments,
# usually neither on disk on its own, and return empty; every real
# target would then fail the containment check below and get rejected as
# "outside the repository", even though nothing in CLAUDE.md restricts a
# space in a checkout path. Confirmed: a checkout under ".../evil dir"
# reproduced exactly this false rejection before this fix. LAB_COMPOSE_EXTRA
# itself carries no such hazard — its own whitelist above already forbids
# whitespace, so $(realpath $(LAB_COMPOSE_EXTRA)) always resolves exactly
# one name.
#
# Containment is then a literal string check: stripping one "$(CURDIR)/"
# prefix from the resolved target must both change the string (so the
# target really was under the repo, not just equal to reconstructing an
# unrelated string) and, glued back on, reproduce the exact original — so
# a target whose text happens to contain "$(CURDIR)/" twice (where subst
# would remove both occurrences) fails closed instead of passing.
LAB_COMPOSE_EXTRA_RESOLVED := $(realpath $(LAB_COMPOSE_EXTRA))
ifneq ($(CURDIR)/$(subst $(CURDIR)/,,$(LAB_COMPOSE_EXTRA_RESOLVED)),$(LAB_COMPOSE_EXTRA_RESOLVED))
$(error invalid LAB_COMPOSE_EXTRA '$(LAB_COMPOSE_EXTRA)': resolves outside the repository (a symlink pointing outside it?))
endif
COMPOSE_DEV := docker compose -f docker-compose.dev.yml -f $(LAB_COMPOSE_EXTRA)
endif
LAB_ID_PATTERN  := ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$$
LAB_SEED_PROFILE ?= mixed

# mock (issue #20, doc 06 §8) reads SCENARIO/MOCK_ADDR — `make mock
# SCENARIO=degraded` — the same way HOSERVA_LAB_ID is read above, so they
# get the same unexport/$(value ...)/export guard against GNU Make
# auto-exporting (and thereby Make-expanding, `$(shell ...)` included) a
# command-line-supplied variable before any recipe-level check runs. Once
# past this guard, cmd/mockapi itself — not this Makefile — validates the
# scenario name and refuses a non-loopback address, matching this file's
# existing LAB_SEED_PROFILE precedent of leaving value validation to the
# tool that actually uses the value.
SCENARIO ?= healthy
unexport SCENARIO
ifneq ($(findstring $$,$(value SCENARIO)),)
$(error invalid SCENARIO: must not contain '$$' — no Make or shell expansion syntax is accepted in a scenario name)
endif
export SCENARIO

MOCK_ADDR ?= 127.0.0.1:8090
unexport MOCK_ADDR
ifneq ($(findstring $$,$(value MOCK_ADDR)),)
$(error invalid MOCK_ADDR: must not contain '$$' — no Make or shell expansion syntax is accepted in a listen address)
endif
export MOCK_ADDR

.PHONY: build test test-unit lint clean mock lab-up lab-seed lab-destroy lab-verify-refusal lab-snapraid-check lab-require-id gen api-check web-build web-lint web-typecheck web-test

# web/ (issue #21, Q8): the Vite build has to run before the Go binaries so
# web/dist/ is real before cmd/hoservad's //go:embed (web/embed.go) reads
# it — a stale or placeholder dist/ would otherwise get baked into a
# release build silently.
web-build:
	@echo "web: npm ci"
	cd web && $(NPM) ci --no-audit --no-fund
	@echo "web: npm run build"
	cd web && $(NPM) run build
	@test -f web/dist/index.html || { echo "web-build: web/dist/index.html is missing after 'npm run build' — the embed (web/embed.go) would ship a placeholder, not the app" >&2; exit 1; }

build: web-build
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "building $$cmd"; \
		CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

web-lint:
	@echo "web: npm ci"
	cd web && $(NPM) ci --no-audit --no-fund
	@echo "web lint"
	cd web && $(NPM) run lint

web-typecheck:
	@echo "web: npm ci"
	cd web && $(NPM) ci --no-audit --no-fund
	@echo "web typecheck"
	cd web && $(NPM) run typecheck

web-test:
	@echo "web: npm ci"
	cd web && $(NPM) ci --no-audit --no-fund
	@echo "web test"
	cd web && $(NPM) run test

test: test-unit

# Go's own "./..." wildcard skips "vendor", "testdata" and dot/underscore
# directories, but not "node_modules" (`go help packages`) — once web/'s
# npm install populates web/node_modules/, a package that happens to ship a
# .go file (as flatted, an openapi-typescript dependency, does) is
# otherwise picked up as if it were this module's own code. Every `./...`
# invocation below filters it out explicitly rather than relying on that
# not to break the build.
GO_PACKAGES = $$($(GO) list ./... | grep -v /node_modules/)

test-unit:
	CGO_ENABLED=0 $(GO) test $(GO_PACKAGES)
	$(MAKE) web-test

lint:
	@echo "gofmt"
	@fmtout="$$(gofmt -l .)"; \
	if [ -n "$$fmtout" ]; then \
		echo "$$fmtout"; \
		echo "gofmt: files need formatting"; \
		exit 1; \
	fi
	@echo "go vet"
	@$(GO) vet $(GO_PACKAGES)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint"; \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping (gofmt and go vet above still ran)"; \
	fi
	$(MAKE) web-lint
	$(MAKE) web-typecheck

# api/openapi.yaml is the hand-written contract; everything under api/gen/
# is generated from it and committed (D18, Q63). Regenerates unconditionally
# — this is also how api-check below confirms the committed output isn't
# stale, and how CI catches a spec change that was pushed without
# regenerating (issue #17).
gen:
	@echo "ogen: Go server interfaces + client -> api/gen/go"
	@rm -rf api/gen/go
	@mkdir -p api/gen/go
	$(GO) run github.com/ogen-go/ogen/cmd/ogen --target api/gen/go --clean --package apiv1 --config api/ogen.yml api/openapi.yaml
	@echo "jschemagen: Event schema -> api/gen/go/events/event_gen.go"
	@# ogen's own generator drops streamEvents (client and server both, Q63)
	@# because its server side isn't implemented, and along with it every
	@# schema only that operation reaches — jschemagen generates the Event
	@# type on its own from the same spec (via api/event-schema-root.yaml)
	@# so a Go client still reads /api/v1/events through generated types
	@# (D18, doc 01 §5), not a hand-rolled struct.
	@mkdir -p api/gen/go/events
	cd api && $(GO) run github.com/ogen-go/ogen/cmd/jschemagen --package events --typename Event --target gen/go/events/event_gen.go event-schema-root.yaml
	@echo "writing api/gen/go/events/reader.go"
	@{ \
		printf '%s\n' '// Code generated by `make gen` (Q63) from api/openapi.yaml'"'"'s Event schema.'; \
		printf '%s\n' '// Do not edit this file directly. It is boilerplate on purpose: event_gen.go'; \
		printf '%s\n' '// (jschemagen, generated alongside this file) already carries all of the'; \
		printf '%s\n' '// schema'"'"'s content — the Event type and its JSON decoding — so this is just'; \
		printf '%s\n' '// the plain SSE-frame parsing loop around it. It exists so a Go client (the'; \
		printf '%s\n' '// CLI, once it consumes /api/v1/events) reads the stream only through'; \
		printf '%s\n' '// generated code, matching every other operation (D18, doc 01 §5), instead'; \
		printf '%s\n' '// of a hand-rolled parser.'; \
		printf '%s\n' 'package events'; \
		printf '%s\n' ''; \
		printf '%s\n' 'import ('; \
		printf '%s\n' '	"bufio"'; \
		printf '%s\n' '	"io"'; \
		printf '%s\n' '	"strings"'; \
		printf '%s\n' ')'; \
		printf '%s\n' ''; \
		printf '%s\n' '// Reader decodes a `text/event-stream` body into typed Event values, one per'; \
		printf '%s\n' '// SSE frame, using Event'"'"'s own generated JSON decoding for each frame'"'"'s'; \
		printf '%s\n' '// `data:` field.'; \
		printf '%s\n' 'type Reader struct {'; \
		printf '%s\n' '	scanner *bufio.Scanner'; \
		printf '%s\n' '}'; \
		printf '%s\n' ''; \
		printf '%s\n' '// NewReader wraps r, the body of a GET to /api/v1/events.'; \
		printf '%s\n' 'func NewReader(r io.Reader) *Reader {'; \
		printf '%s\n' '	return &Reader{scanner: bufio.NewScanner(r)}'; \
		printf '%s\n' '}'; \
		printf '%s\n' ''; \
		printf '%s\n' '// Next reads and decodes the next frame. It returns io.EOF once the stream'; \
		printf '%s\n' '// ends without a trailing blank line.'; \
		printf '%s\n' 'func (dec *Reader) Next() (Event, error) {'; \
		printf '%s\n' '	var data strings.Builder'; \
		printf '%s\n' '	sawData := false'; \
		printf '%s\n' '	for dec.scanner.Scan() {'; \
		printf '%s\n' '		line := dec.scanner.Text()'; \
		printf '%s\n' '		if line == "" {'; \
		printf '%s\n' '			if !sawData {'; \
		printf '%s\n' '				continue'; \
		printf '%s\n' '			}'; \
		printf '%s\n' '			var ev Event'; \
		printf '%s\n' '			err := ev.UnmarshalJSON([]byte(data.String()))'; \
		printf '%s\n' '			return ev, err'; \
		printf '%s\n' '		}'; \
		printf '%s\n' '		if rest, ok := strings.CutPrefix(line, "data:"); ok {'; \
		printf '%s\n' '			if sawData {'; \
		printf '%s\n' '				data.WriteByte('"'"'\n'"'"')'; \
		printf '%s\n' '			}'; \
		printf '%s\n' '			data.WriteString(strings.TrimPrefix(rest, " "))'; \
		printf '%s\n' '			sawData = true'; \
		printf '%s\n' '			continue'; \
		printf '%s\n' '		}'; \
		printf '%s\n' '		// Every other SSE field (`event:`, `id:`, `retry:`, comments) carries'; \
		printf '%s\n' '		// no independent value here: the payload'"'"'s own discriminator'; \
		printf '%s\n' '		// (openapi.yaml'"'"'s Event.event) already names the variant, and doc 01'; \
		printf '%s\n' '		// §5 does not ask a client to resume a stream from `id:`.'; \
		printf '%s\n' '	}'; \
		printf '%s\n' '	if err := dec.scanner.Err(); err != nil {'; \
		printf '%s\n' '		return Event{}, err'; \
		printf '%s\n' '	}'; \
		printf '%s\n' '	if sawData {'; \
		printf '%s\n' '		var ev Event'; \
		printf '%s\n' '		err := ev.UnmarshalJSON([]byte(data.String()))'; \
		printf '%s\n' '		return ev, err'; \
		printf '%s\n' '	}'; \
		printf '%s\n' '	return Event{}, io.EOF'; \
		printf '%s\n' '}'; \
	} > api/gen/go/events/reader.go
	@echo "openapi-typescript: TS schema -> api/gen/ts/schema.d.ts"
	@command -v $(NPM) >/dev/null 2>&1 || { echo "gen: '$(NPM)' not found on PATH — install Node (Q63) and retry" >&2; exit 1; }
	cd api && $(NPM) ci --no-audit --no-fund
	@mkdir -p api/gen/ts
	cd api && ./node_modules/.bin/openapi-typescript openapi.yaml -o gen/ts/schema.d.ts
	@echo "writing api/gen/ts/client.ts"
	@{ \
		echo '/**'; \
		echo ' * This file was generated by `make gen` (Q63) from api/openapi.yaml, via'; \
		echo ' * the Makefile'"'"'s gen target. It is boilerplate on purpose — openapi-fetch has'; \
		echo ' * no separate code generator; this wraps its `createClient` around the'; \
		echo ' * types openapi-typescript produced in ./schema.d.ts, which is where all of'; \
		echo ' * the spec'"'"'s actual content lives. Do not make direct changes to this file.'; \
		echo ' *'; \
		echo ' * The web UI (#21) imports { createHoservaClient } from here, and only from'; \
		echo ' * here — D18: the generated client is the only way it calls the API.'; \
		echo ' */'; \
		echo 'import createClient from "openapi-fetch";'; \
		echo 'import type { paths } from "./schema.d.ts";'; \
		echo ''; \
		echo 'export type { paths, components, operations } from "./schema.d.ts";'; \
		echo ''; \
		echo 'export function createHoservaClient(baseUrl: string) {'; \
		echo '  return createClient<paths>({ baseUrl });'; \
		echo '}'; \
	} > api/gen/ts/client.ts

# spec lint (operationId, x-hoserva-role — Q63), generated code freshness,
# and a breaking-change diff against the last release, once one exists
# (doc 12 §3). `gen` above is the freshness check's own regeneration step:
# if it changes anything under api/gen/, the committed output was stale.
api-check: gen
	@echo "checking api/gen/ is up to date with api/openapi.yaml"
	@# git diff alone misses a brand-new generated file that was never
	@# committed at all (untracked, so a plain diff has nothing to compare
	@# it against) — status --porcelain reports those too.
	@if [ -n "$$(git status --porcelain -- api/gen)" ]; then \
		git status --porcelain -- api/gen >&2; \
		echo "api-check: api/gen/ is stale — commit the output of 'make gen'" >&2; \
		exit 1; \
	fi
	@echo "spectral lint"
	cd api && ./node_modules/.bin/spectral lint openapi.yaml --ruleset .spectral.yaml
	@echo "oasdiff breaking-change check"
	@last_tag="$$(git tag --list 'v*' --sort=-v:refname | head -n1)"; \
	if [ -z "$$last_tag" ]; then \
		echo "api-check: no released api/openapi.yaml yet (no 'v*' tag) — skipping the breaking-change check (Q63)"; \
	else \
		echo "api-check: comparing $$last_tag's api/openapi.yaml with the working tree"; \
		tmp="$$(mktemp)"; \
		git show "$$last_tag:api/openapi.yaml" > "$$tmp" || { echo "api-check: $$last_tag has no api/openapi.yaml" >&2; rm -f "$$tmp"; exit 1; }; \
		$(GO) run github.com/oasdiff/oasdiff breaking "$$tmp" api/openapi.yaml; status=$$?; \
		rm -f "$$tmp"; \
		exit $$status; \
	fi

clean:
	rm -rf $(BIN_DIR)

# The mock API server (doc 06 §8, issue #20): serves api/openapi.yaml from
# web/fixtures/ scenarios, so frontend work never needs hoservad or the lab.
# web/ (the Vite dev server) doesn't exist yet (#21 adds it) — once
# web/package.json does, this starts both together and stops both on
# Ctrl-C, tracking the mock's own PID rather than a pattern kill.
mock:
	@if [ -f web/package.json ]; then \
		echo "mock: starting the mock API (scenario: $$SCENARIO) on http://$$MOCK_ADDR and the web dev server"; \
		$(GO) run ./cmd/mockapi --addr "$$MOCK_ADDR" --scenario "$$SCENARIO" & \
		mock_pid=$$!; \
		trap 'kill "$$mock_pid" 2>/dev/null' EXIT INT TERM; \
		(cd web && $(NPM) run dev); \
	else \
		echo "mock: web/ does not exist yet (#21) — starting the mock API server only"; \
		echo "mock: scenario $$SCENARIO on http://$$MOCK_ADDR/api/v1"; \
		$(GO) run ./cmd/mockapi --addr "$$MOCK_ADDR" --scenario "$$SCENARIO"; \
	fi

lab-require-id:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make lab-up)" >&2; exit 1; }
	@bash -euc '. scripts/devenv/lib.sh && lab_id_valid "$$HOSERVA_LAB_ID"' || { echo "invalid HOSERVA_LAB_ID '$$HOSERVA_LAB_ID': must match $(LAB_ID_PATTERN) (letters, digits, '_', '.', '-' only, starting with a letter or digit — no '/', '*', newlines or other shell/path metacharacters) and must not contain '..'" >&2; exit 1; }

lab-up: lab-require-id
	@mkdir -p -- ".lab/$$HOSERVA_LAB_ID"
	$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" up -d --build
	$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/create-array.sh

lab-seed: lab-require-id
	$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/seed-data.sh --profile "$(LAB_SEED_PROFILE)"

lab-verify-refusal: lab-require-id
	$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/test-host-refusal.sh

lab-snapraid-check: lab-require-id
	$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/snapraid-check.sh

# destroy-array.sh runs as root inside the container and fails loudly (exit
# non-zero) if a mount cannot be freed, rather than silently continuing to a
# half-finished `rm -rf` (see the script's own comments). Only when it either
# reports nothing running or exits clean do we tear the container down —
# never `down -v` while the one thing with permission on the root-owned
# files might still be needed to fix a stuck mount by hand. Not `-`-prefixed:
# a real failure here must stop this target, not be swallowed.
#
# Gated on the container *existing* (any state — running, stopped, exited),
# not on it already being `running`: a host reboot, a Docker daemon
# restart, an OOM kill, or a bare `docker compose down` outside `make` can
# all leave a container that exists but is stopped, and skipping teardown
# in that case — while still unconditionally removing the container and
# `rm -rf`-ing the bind mount below — would strand root-owned files and
# leak this lab's loop devices with nothing left able to clean them up
# (doc 08, S9). So: no container at all is the genuine no-op; a stopped
# container is started back up just long enough to run the real teardown.
lab-destroy: lab-require-id
	@cid="$$($(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" ps -aq lab 2>/dev/null)"; \
	if [ -n "$$cid" ]; then \
		running="$$($(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" ps --status running -q lab 2>/dev/null)"; \
		if [ -z "$$running" ]; then \
			echo "lab $$HOSERVA_LAB_ID: container exists but is not running, starting it to tear down cleanly"; \
			$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" start lab || exit 1; \
		fi; \
		$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/destroy-array.sh || exit 1; \
	else \
		echo "lab $$HOSERVA_LAB_ID: container does not exist, nothing to tear down"; \
	fi
	-$(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" down -v
	@# destroy-array.sh has already emptied $$LAB from inside the container,
	@# where root can. This step only removes the now-empty directory, with
	@# rmdir rather than `rm -rf`: if anything root-owned is still in there,
	@# rmdir fails harmlessly and says so, instead of a bare permission error
	@# from a recursive delete that was never going to succeed (issue #120).
	@if [ -d ".lab/$$HOSERVA_LAB_ID" ]; then \
		if ! rmdir -- ".lab/$$HOSERVA_LAB_ID" 2>/dev/null; then \
			echo "lab-destroy: .lab/$$HOSERVA_LAB_ID is not empty — the container tore down but left root-owned files behind:" >&2; \
			ls -la -- ".lab/$$HOSERVA_LAB_ID" >&2; \
			echo "lab-destroy: bring the lab back up and re-run destroy-array.sh inside it; do not try to delete these from the host." >&2; \
			exit 1; \
		fi; \
	fi
