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

# JUnit reporting (doc 06 §7). With TEST_REPORT_DIR set, every test target
# below also writes JUnit XML under it — go test through gotestsum, each
# shell suite script through scripts/devenv/junit-step.sh, vitest through
# its junit reporter — and with it unset, every target behaves exactly as
# without any of this. The path is made absolute because vitest runs from
# web/. Use an empty or fresh directory: a case is appended to its suite's
# file on every run.
ifneq ($(strip $(TEST_REPORT_DIR)),)
override TEST_REPORT_DIR := $(abspath $(TEST_REPORT_DIR))
export TEST_REPORT_DIR
endif
JUNIT        := scripts/devenv/junit-step.sh

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

# vm-snapshot/vm-restore (NAME=) and vm-deploy (TAG=, DEB=) — the same
# unexport/$(value ...)/export guard as every other command-line-supplied
# variable above, against GNU Make auto-exporting (and thereby
# Make-expanding, `$(shell ...)` included) one before any recipe-level
# check runs. Once past this guard, scripts/vm/*.sh's own validation (a
# character whitelist for NAME, a real file check for DEB) is what these
# values are actually checked against — this guard only proves them
# inert to Make/shell expansion, matching every existing variable here.
NAME ?=
unexport NAME
ifneq ($(findstring $$,$(value NAME)),)
$(error invalid NAME: must not contain '$$' — no Make or shell expansion syntax is accepted in a snapshot name)
endif
export NAME

TAG ?=
unexport TAG
ifneq ($(findstring $$,$(value TAG)),)
$(error invalid TAG: must not contain '$$' — no Make or shell expansion syntax is accepted in a release tag)
endif
export TAG

DEB ?=
unexport DEB
ifneq ($(findstring $$,$(value DEB)),)
$(error invalid DEB: must not contain '$$' — no Make or shell expansion syntax is accepted in a file path)
endif
export DEB

# L3_STEPS (issue #391) — `make vm-suite L3_STEPS=storage-target,disk-yank`
# — the same guard as every other command-line-supplied variable above.
# Once past it, scripts/vm/run-l3-suite.sh's own l3_resolve_steps is what
# actually validates the ids, refusing an unknown one before any VM
# operation. Empty (the default) means every step, exactly as before this
# issue existed — always what the nightly workflow's schedule trigger
# passes.
L3_STEPS ?=
unexport L3_STEPS
ifneq ($(findstring $$,$(value L3_STEPS)),)
$(error invalid L3_STEPS: must not contain '$$' — no Make or shell expansion syntax is accepted in a step list)
endif
export L3_STEPS

# VARIANT and LAYOUT — `make vm-migration-suite VARIANT=unraid-7x-xfs-single-parity
# LAYOUT=shared-nvme` — the same guard as every other command-line-supplied
# variable above (a `$(shell ...)` in a value would otherwise run when Make
# exports it). scripts/vm/run-migration-suite.sh compares both with its own list
# of runs and refuses any other value before a VM exists. An empty LAYOUT (the
# default) is the separate cache-disk layout.
VARIANT ?=
unexport VARIANT
ifneq ($(findstring $$,$(value VARIANT)),)
$(error invalid VARIANT: must not contain '$$' — no Make or shell expansion syntax is accepted in a fixture variant name)
endif
export VARIANT

LAYOUT ?=
unexport LAYOUT
ifneq ($(findstring $$,$(value LAYOUT)),)
$(error invalid LAYOUT: must not contain '$$' — no Make or shell expansion syntax is accepted in a layout name)
endif
export LAYOUT

.PHONY: build test test-unit test-go test-corpus test-devenv test-pages-site test-integration test-lab packaging-test test-unraid-tools lint lint-go lint-sh clean mock lab-up lab-seed lab-destroy lab-verify-refusal lab-snapraid-check lab-unraid-fixture lab-unraid-verify lab-require-id gen api-check web-build site-catalog site-build web-check-outbound web-scan-outbound web-outbound-test catalog-snapshot catalog-snapshot-test web-lint web-typecheck web-test db-migration db-check vm-up vm-snapshot vm-restore vm-unraid-fixture vm-unraid-capture vm-deploy vm-reinstall-os vm-destroy vm-suite vm-suite-plan vm-soak vm-migration-suite hooks-install

# One-time local setup (CONTRIBUTING.md, doc 13 Q2): every commit needs a
# DCO Signed-off-by trailer. This points git at the repo-tracked hook
# instead of copying it into .git/hooks, so `git pull` keeps it current.
hooks-install:
	git config core.hooksPath scripts/devenv/hooks
	@echo "hooks-install: core.hooksPath -> scripts/devenv/hooks (commits are now signed off automatically)"

# web/ (issue #21, Q8): the Vite build has to run before the Go binaries so
# web/dist/ is real before cmd/hoservad's //go:embed (web/embed.go) reads
# it — a stale or placeholder dist/ would otherwise get baked into a
# release build silently.
web-build:
	@echo "web: npm ci"
	cd web && $(NPM) ci --ignore-scripts --no-audit --no-fund
	@echo "web: npm run build"
	cd web && $(NPM) run build
	@test -f web/dist/index.html || { echo "web-build: web/dist/index.html is missing after 'npm run build' — the embed (web/embed.go) would ship a placeholder, not the app" >&2; exit 1; }

# The catalog behind hoserva.dev/apps: scripts/devenv/catalog-export verifies
# the signed archive with the code and key hoservad uses and writes the plain
# site/.catalog/ the site plugin reads. The source is the pinned snapshot, so
# a local or CI build needs no live fetch; when CATALOG_LIVE_URL is set (the
# Pages deploy sets it) the live catalog is used instead if it verifies and is
# not older than the pin, and the pinned snapshot otherwise. The directory is
# removed first, so a build never renders an export left by an earlier run.
site-catalog: catalog-snapshot
	rm -rf site/.catalog
	$(GO) run ./scripts/devenv/catalog-export -pin scripts/devenv/catalog.pin -snapshot internal/template/snapshot -out site/.catalog

# site/ (issue #82, Q3, Q90): the Docusaurus docs site. The build is written
# to site/dist, which scripts/release/assemble-pages-site.sh publishes at the
# root of hoserva.dev (running this target itself). dist is removed first, so
# the index.html check below cannot pass on a build left by an earlier run.
site-build: site-catalog
	@echo "site: npm ci"
	cd site && $(NPM) ci --no-audit --no-fund
	@echo "site: typecheck"
	cd site && $(NPM) run typecheck
	rm -rf site/dist
	@echo "site: npm run build"
	cd site && $(NPM) run build
	@test -f site/dist/index.html || { echo "site-build: site/dist/index.html is missing after 'npm run build' — scripts/release/assemble-pages-site.sh would publish no docs" >&2; exit 1; }
	@echo "site: layout, external-reference and versioning checks"
	cd site && $(NPM) run check

# Q49: the built app embeds no outbound request. web-scan-outbound scans an
# existing web/dist only — CI's web job, which has just built it, runs that.
web-check-outbound: web-build
	$(JUNIT) web-outbound check-web-outbound.sh -- scripts/devenv/check-web-outbound.sh web/dist

web-scan-outbound:
	$(JUNIT) web-outbound check-web-outbound.sh -- scripts/devenv/check-web-outbound.sh web/dist

# The curated catalog archive pinned in scripts/devenv/catalog.pin is fetched
# from its immutable catalog release, checked against the pin and the
# compiled-in catalog key, and placed where internal/template embeds it
# (Q65, doc 04 §7). It fails — leaving nothing in internal/template/snapshot/
# — on a download failure, a pin mismatch or a bad signature, so `build`
# never ships the placeholder. Every Go build or test that ships or runs the
# daemon depends on it.
catalog-snapshot:
	scripts/devenv/catalog-snapshot.sh

# Fixture test for catalog-snapshot.sh's refusals (a failed download, a pin
# mismatch, a bad signature); it never reaches the network.
catalog-snapshot-test: catalog-snapshot
	$(JUNIT) devenv test-catalog-snapshot.sh -- scripts/devenv/test-catalog-snapshot.sh

build: web-build catalog-snapshot
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

test: test-unit test-devenv packaging-test test-unraid-tools test-gh web-outbound-test

# Go's own "./..." wildcard skips "vendor", "testdata" and dot/underscore
# directories, but not "node_modules" (`go help packages`) — once web/'s
# npm install populates web/node_modules/, a package that happens to ship a
# .go file (as flatted, an openapi-typescript dependency, does) is
# otherwise picked up as if it were this module's own code. Every `./...`
# invocation below filters it out explicitly rather than relying on that
# not to break the build.
GO_PACKAGES = $$($(GO) list ./... | grep -v /node_modules/)

# Go-only L1: CI's lint-and-unit job calls this so it does not also run
# the web job's lint/test. Local `make test` / `make test-unit` still
# include web-test (doc 06 §10).
test-go: catalog-snapshot-test
	@if [ -n "$$TEST_REPORT_DIR" ]; then \
		mkdir -p -- "$$TEST_REPORT_DIR" && \
		CGO_ENABLED=0 $(GO) tool gotestsum --format standard-quiet --junitfile "$$TEST_REPORT_DIR/go-unit.xml" -- $(GO_PACKAGES); \
	else \
		CGO_ENABLED=0 $(GO) test $(GO_PACKAGES); \
	fi

test-unit: test-go
	$(MAKE) web-test

# The Unraid template converter's release metric (Q36, doc 06 §2): converts
# every project-authored template in testdata/unraid-templates/, prints the
# clean-conversion rate and fails when it is below the rate recorded in that
# directory's manifest.txt. It needs neither the catalog snapshot nor a
# built web UI, so it has no prerequisites; `make test-go` runs the same
# test as part of ./... .
test-corpus:
	@if [ -n "$$TEST_REPORT_DIR" ]; then \
		mkdir -p -- "$$TEST_REPORT_DIR" && \
		CGO_ENABLED=0 $(GO) tool gotestsum --format standard-verbose --junitfile "$$TEST_REPORT_DIR/go-corpus.xml" -- -count=1 -v -run '^(TestUnraidCorpus|TestCorpus)' ./internal/template; \
	else \
		CGO_ENABLED=0 $(GO) test -count=1 -v -run '^(TestUnraidCorpus|TestCorpus)' ./internal/template; \
	fi

# The .deb's own safety-critical regression tests (issue #42): each script
# runs the real maintainer script (postinst/postrm) or lib.sh function
# against a throwaway HOSERVA_TEST_ROOT, never a real root filesystem —
# no debhelper or dpkg-buildpackage needed, so this runs anywhere `make
# test` does.
packaging-test:
	$(JUNIT) packaging-test scripts/release/test-lib.sh -- scripts/release/test-lib.sh
	$(JUNIT) packaging-test scripts/release/test-postinst.sh -- scripts/release/test-postinst.sh
	$(JUNIT) packaging-test scripts/release/test-postrm-purge.sh -- scripts/release/test-postrm-purge.sh
	$(JUNIT) packaging-test scripts/release/test-build-deb.sh -- scripts/release/test-build-deb.sh
	$(JUNIT) packaging-test scripts/release/test-stamp-prepare-script.sh -- scripts/release/test-stamp-prepare-script.sh
	$(JUNIT) packaging-test scripts/release/test-publish-release.sh -- scripts/release/test-publish-release.sh
	$(JUNIT) packaging-test scripts/release/test-stage-release-artifacts.sh -- scripts/release/test-stage-release-artifacts.sh
	$(JUNIT) packaging-test scripts/release/test-release-workflow.sh -- scripts/release/test-release-workflow.sh
	$(JUNIT) packaging-test packaging/test-control-depends.sh -- packaging/test-control-depends.sh
	$(JUNIT) packaging-test packaging/test-unattended-upgrades.sh -- packaging/test-unattended-upgrades.sh
	$(JUNIT) packaging-test packaging/test-preinst-smartd-dropin.sh -- packaging/test-preinst-smartd-dropin.sh
	$(JUNIT) packaging-test packaging/test-postinst-smartd-mask.sh -- packaging/test-postinst-smartd-mask.sh
	$(JUNIT) packaging-test packaging/test-postinst-smartd-survives-upgrade.sh -- packaging/test-postinst-smartd-survives-upgrade.sh
	$(JUNIT) packaging-test packaging/test-postinst-hoserva-apps.sh -- packaging/test-postinst-hoserva-apps.sh
	$(JUNIT) packaging-test packaging/test-udev-storage-rule-ordering.sh -- packaging/test-udev-storage-rule-ordering.sh

# The Unraid prepare script (issue #556, doc 05 §4 Phase A step 0): run
# against hand-written fixture roots under tools/unraid/testdata/ with
# stand-ins for docker, findmnt, lsblk, df and zip, never an Unraid server
# (D20). Needs jq, python3 and unzip.
test-unraid-tools:
	$(JUNIT) unraid-tools tools/unraid/test-prepare-migration.sh -- tools/unraid/test-prepare-migration.sh
	$(JUNIT) unraid-tools scripts/vm/unraid-sizing-check.sh -- scripts/vm/unraid-sizing-check.sh
	$(JUNIT) unraid-tools scripts/vm/migration-suite-coverage-check.sh -- scripts/vm/migration-suite-coverage-check.sh
	$(JUNIT) unraid-tools scripts/vm/migration-suite-check.sh -- scripts/vm/migration-suite-check.sh

# The agent workflow's GitHub client (issue #410): scripts/gh-rest.sh's own
# contract tests against a fake `gh`, plus the fake-gh tests for the
# scripts that read GitHub through it — never the live API.
test-gh:
	$(JUNIT) gh scripts/test-gh-rest.sh -- scripts/test-gh-rest.sh
	$(JUNIT) gh scripts/test-issue-status.sh -- scripts/test-issue-status.sh
	$(JUNIT) gh scripts/test-epic-status.sh -- scripts/test-epic-status.sh
	$(JUNIT) gh scripts/test-issue-readiness.sh -- scripts/test-issue-readiness.sh
	$(JUNIT) gh scripts/test-check-gh-rest.sh -- scripts/test-check-gh-rest.sh
	$(JUNIT) gh scripts/test-security-history.sh -- scripts/test-security-history.sh
	$(JUNIT) gh scripts/test-audit-report.sh -- scripts/test-audit-report.sh

# Fixture test for the Q49 outbound-request check's allowlist (issue #461);
# it scans throwaway directories, not the web build.
web-outbound-test:
	$(JUNIT) web-outbound test-check-web-outbound.sh -- scripts/devenv/test-check-web-outbound.sh

# Fixture tests for the JUnit wrapper, the L3 suite's JUnit writer (no VM) and
# the PR comment script (a stub gh); the renderer's own tests are Go tests under
# scripts/devenv/testreport and run with test-go.
test-devenv:
	$(JUNIT) devenv test-junit-step.sh -- scripts/devenv/test-junit-step.sh
	$(JUNIT) devenv test-l3-junit.sh -- scripts/vm/test-l3-junit.sh
	$(JUNIT) devenv test-pr-comment.sh -- scripts/devenv/test-pr-comment.sh

# The Pages site assembly script's own test (Q66).
test-pages-site:
	$(JUNIT) pages-site test-pages-site.sh -- scripts/release/test-pages-site.sh

# Go-only lint: CI's lint-and-unit job calls this so it does not also
# run the web job's lint/typecheck. Local `make lint` still includes
# web-lint and web-typecheck (doc 06 §10). In CI, missing golangci-lint
# is a failure (issue #130).
lint-go:
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
	elif [ -n "$$CI" ]; then \
		echo "golangci-lint: not installed, and CI is set — CI must install a pinned version (issue #130)" >&2; \
		exit 1; \
	else \
		echo "golangci-lint not installed, skipping (gofmt and go vet above still ran)"; \
	fi

# Guards the agent workflow's move off GraphQL onto repository-scoped REST
# (issue #410): fails on a GraphQL-backed `gh` subcommand or `--paginate`
# anywhere under .claude/ or scripts/ — either one works on the
# maintainer's own machine and then breaks silently in a Claude Code cloud
# session, whose egress proxy refuses both. CI's lint-and-unit job calls
# this directly.
lint-gh:
	scripts/check-gh-rest.sh

# shellcheck over the user-run Unraid script and its tests, and over the
# release helpers that publish it. In CI a missing shellcheck is a failure,
# as for golangci-lint above.
SHELL_LINT_FILES = scripts/devenv/unraid-fixture.sh scripts/devenv/test-unraid-fixture.sh scripts/devenv/junit-step.sh scripts/devenv/test-junit-step.sh scripts/vm/l3-junit.sh scripts/vm/test-l3-junit.sh scripts/devenv/pr-comment.sh scripts/devenv/test-pr-comment.sh \
	scripts/vm/unraid-fixture.sh scripts/vm/unraid-capture.sh scripts/vm/unraid-capture-guest.sh scripts/vm/unraid-lib.sh \
	scripts/vm/create-vm.sh scripts/vm/unraid-sizing-check.sh \
	scripts/vm/run-migration-suite.sh scripts/vm/migration-suite-guest.sh scripts/vm/usb-image.sh scripts/vm/migration-suite-coverage-check.sh scripts/vm/migration-suite-check.sh \
	tools/unraid/prepare-migration.sh tools/unraid/test-prepare-migration.sh \
	scripts/release/stamp-prepare-script.sh scripts/release/test-stamp-prepare-script.sh \
	scripts/release/test-publish-release.sh \
	tools/unraid/testdata/stubs/docker tools/unraid/testdata/stubs/findmnt tools/unraid/testdata/stubs/lsblk \
	tools/unraid/testdata/stubs/df tools/unraid/testdata/stubs/emcmd tools/unraid/testdata/stubs/mdcmd \
	tools/unraid/testdata/stubs/efibootmgr tools/unraid/testdata/stubs/mover

lint-sh:
	@if command -v shellcheck >/dev/null 2>&1; then \
		echo "shellcheck"; \
		shellcheck -x $(SHELL_LINT_FILES); \
	elif [ -n "$$CI" ]; then \
		echo "shellcheck: not installed, and CI is set" >&2; \
		exit 1; \
	else \
		echo "shellcheck not installed, skipping"; \
	fi

lint: lint-go lint-gh lint-sh
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
	@echo "sqlc: internal/store/schema/schema.sql -> internal/store/db/ (D16, Q60)"
	@rm -rf internal/store/db
	@mkdir -p internal/store/db
	$(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc generate --file internal/store/sqlc.yaml

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

# Diffs internal/store/schema/schema.sql against the schema
# internal/store/migrations/ produces and writes the next, immutable,
# timestamped file (D16, Q60) via sqlite-migrate generate. NAME is required
# so every migration's filename says what it does, not "<timestamp>_migration".
# A schema.sql edit that drops a table/column needs
# NAME=... ARGS=--allow-destructive, and one sqlite-migrate can't tell is a
# rename from a genuine drop-and-add needs ARGS=--assume-renames or
# ARGS=--assume-no-renames (never left to its own non-interactive default —
# see the sqlite-migrate skill).
db-migration:
	@test -n "$(NAME)" || { echo "set NAME (e.g. make db-migration NAME=add_job_table)" >&2; exit 1; }
	$(GO) run github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate generate -schema internal/store/schema/schema.sql -dir internal/store/migrations -m "$(NAME)" $(ARGS)

# Migration checksums (an edited "immutable" file) and schema drift
# (replaying every migration must produce exactly schema.sql) — Q60's
# sqlite-migrate check, connection-free and safe to run in CI.
db-check:
	$(JUNIT) db-check sqlite-migrate-check -- $(GO) run github.com/mdg-labs/sqlite-migrate/cmd/sqlite-migrate check -schema internal/store/schema/schema.sql -dir internal/store/migrations

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
	$(JUNIT) lab lab-verify-refusal -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/test-host-refusal.sh

lab-snapraid-check: lab-require-id
	$(JUNIT) lab lab-snapraid-check -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/snapraid-check.sh

# The synthetic Unraid source (doc 06 §5, issue #74), built inside the lab on
# loop devices backed by this lab's own images. VARIANT names a directory under
# testdata/unraid-fixtures/ and is read from the environment, never spliced
# into the recipe. A variant with options (unraid-with-vms) also takes OPTION,
# passed the same way as HOSERVA_FIXTURE_OPTION; unset builds its default.
# lab-unraid-verify re-reads the build through read-only norecovery mounts and
# diffs it against its manifest, then checks that the builder refuses every
# device that is not this lab's own. A variant that needs ZFS or device-mapper
# (unraid-encrypted, unraid-zfs-disk, unraid-internal-boot,
# unraid-internal-boot-shared) is L3 only: this target refuses it before writing
# anything and names make vm-unraid-fixture.
lab-unraid-fixture: lab-require-id
	@test -n "$$VARIANT" || { echo "set VARIANT (e.g. make lab-unraid-fixture VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	$(JUNIT) lab lab-unraid-fixture -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T -e HOSERVA_FIXTURE_OPTION="$$OPTION" lab bash /src/scripts/devenv/unraid-fixture.sh --tier l2 "$$VARIANT"

lab-unraid-verify: lab-require-id
	@test -n "$$VARIANT" || { echo "set VARIANT (e.g. make lab-unraid-verify VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	$(JUNIT) lab test-unraid-fixture.sh -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/test-unraid-fixture.sh
	$(JUNIT) lab lab-unraid-verify -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T -e HOSERVA_FIXTURE_OPTION="$$OPTION" lab bash /src/scripts/devenv/unraid-fixture.sh --tier l2 --verify "$$VARIANT"

# doc 12 §3's planned integration target (issue #219): a Hoserva-generated
# smb.conf, exercised against a real smbd, inside the lab. gen-smb-conf runs
# on the host, into this lab's own bind-mounted directory, because the lab
# image carries no Go toolchain (unlike snapraid/mergerfs, which are native
# packages) — smb-check.sh, run inside the container next, does everything
# Samba-related itself. test-lab (issue #328) then compiles every
# //go:build lab package on the host and runs each binary in this lab;
# smb-check runs first so a destroy+create reset inside test-lab cannot
# wipe smb.conf.rendered mid-check. check-mnt-user-clean.sh runs after it,
# confirming smb-check's own /mnt/user symlink (issue #363) is gone before
# test-lab's own lab tests, any of which may mount pool.CatchAllPath fresh
# at that same path, run.
test-integration: lab-require-id
	$(GO) run ./scripts/devenv/gen-smb-conf > ".lab/$$HOSERVA_LAB_ID/smb.conf.rendered"
	$(JUNIT) lab smb-check.sh -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/smb-check.sh
	$(JUNIT) lab check-mnt-user-clean.sh -- $(COMPOSE_DEV) -p "hoserva-lab-$$HOSERVA_LAB_ID" exec -T lab bash /src/scripts/devenv/check-mnt-user-clean.sh
	$(MAKE) test-lab

# Every //go:build lab Go test (doc 06 §3, issue #328): compile on the
# host with `go test -tags lab -c`, run each binary inside this lab's
# container. Includes internal/parity/guard_lab_test.go — the threshold
# guard's real-snapraid proof. A failing lab test fails this target.
test-lab: lab-require-id
	scripts/devenv/run-lab-tests.sh

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

# The L3 VM harness (doc 06 §4, Q42, Q79, D20). Every target below shells
# out to scripts/vm/*.sh, which own the actual safety guards (own-domain
# checks, HOSERVA_LAB_ID namespacing, qemu:///session only) — this
# Makefile never duplicates that logic, matching lab-require-id's own
# precedent above of leaving validation to the script that acts on the
# value. HOSERVA_LAB_ID itself is already validated by the top-of-file
# unexport/$(value ...)/export guard before any recipe below runs.
#
# vm-up VARIANT=<variant> creates each array disk the variant's spec targets
# at that spec's l3size=, else size= (parity1, disk1..disk5, cache; a disk no spec line
# targets keeps its HOSERVA_VM_*_SIZE default), so the L3 build of an Unraid
# fixture lays out the same partition scheme as its L2 build. Without VARIANT the
# disks are sized by HOSERVA_VM_*_SIZE alone.
vm-up:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-up)" >&2; exit 1; }
	scripts/vm/create-vm.sh

vm-snapshot:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-snapshot NAME=clean)" >&2; exit 1; }
	scripts/vm/snapshot-vm.sh

vm-restore:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-restore NAME=clean)" >&2; exit 1; }
	scripts/vm/restore-vm.sh

# Builds a synthetic Unraid source (doc 06 §5, issue #74) on this lab's
# guest's own array disks before any .deb is deployed, verifies it, and takes
# the snapshot named like the variant, so `make vm-restore NAME=<variant>`
# returns to it. The guest must come from `make vm-up VARIANT=<variant>`: each
# target disk must be exactly its spec l3size= (else size=), and the build refuses any other
# before it writes a disk, so the layout (MBR up to 2000G, GPT from 2T) is the
# one the L2 build gives. A variant with options takes OPTION=<name> too; a
# non-default one is built, copied and snapshotted as <variant>-<option>. A
# variant with a LUKS or ZFS disk or an internal boot device also gets cryptsetup
# or OpenZFS installed in the guest (never on the host).
vm-unraid-fixture:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-unraid-fixture VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	@test -n "$$VARIANT" || { echo "set VARIANT (e.g. make vm-unraid-fixture VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	scripts/vm/unraid-fixture.sh

# Regenerates the committed capture of a variant (containers.json,
# networks.json, autostart, var.ini, smart/, capture.json, report.txt): runs the
# fixture's containers in this lab's guest, runs tools/unraid/prepare-migration.sh
# against the fixture's flash root there, and copies the result back into
# testdata/unraid-fixtures/<variant>/flash/config/hoserva/. Like
# vm-unraid-fixture, it needs a guest from `make vm-up VARIANT=<variant>`, so
# the capture's disk sizes and fit figures are the spec's. With OPTION=<name>
# for a non-default option it regenerates only that option's capture.json and
# report.txt, under options/<name>/flash/config/hoserva/. A variant whose spec
# says capture=none has no capture, and this target refuses it.
vm-unraid-capture:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-unraid-capture VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	@test -n "$$VARIANT" || { echo "set VARIANT (e.g. make vm-unraid-capture VARIANT=unraid-6.12-xfs-single-parity)" >&2; exit 1; }
	scripts/vm/unraid-capture.sh

vm-deploy:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-deploy)" >&2; exit 1; }
	scripts/vm/deploy.sh

# Replaces only this lab's VM's OS disk with a fresh image and installs
# Hoserva on it (a bare-metal reinstall); every array disk is kept. DEB=
# and TAG= are vm-deploy's.
vm-reinstall-os:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-reinstall-os)" >&2; exit 1; }
	scripts/vm/reinstall-os.sh

vm-destroy:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-destroy)" >&2; exit 1; }
	scripts/vm/destroy-vm.sh

# The nightly/pre-release L3 suite (doc 06 §4, §7, Q79): install,
# onboarding, array setup, disk yank and reconstruction, `virsh destroy`
# mid-sync recovery, reboot persistence, config backup/restore, and the
# Playwright journeys — itemized honestly (not silently skipped) against
# what the product actually exposes today, in scripts/vm/run-l3-suite.sh.
# L3_STEPS (issue #391) runs only the named steps, after the always-run
# setup; empty (the default) runs every step, unchanged from before.
vm-suite:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-suite)" >&2; exit 1; }
	scripts/vm/run-l3-suite.sh

# vm-suite-plan (issue #391) prints which steps an L3_STEPS selection would
# run, in what order, with prerequisites pulled in or an unknown id
# refused — without creating a VM, so a selection can be checked in well
# under a second.
vm-suite-plan:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-suite-plan)" >&2; exit 1; }
	L3_PLAN=1 scripts/vm/run-l3-suite.sh

# The migration test procedure of doc 06 §5 (issue #80) against the synthetic
# Unraid fixtures, in this lab's own L3 VM: restore the fixture snapshot (built
# first when this lab has none), install the .deb, scan, import, verify, the point
# of no return, appdata, containers, scrub and fix, each step recorded PASS or
# FAIL; a refusal variant asserts the refusal and that no source disk changed.
# VARIANT=<variant> runs one, VARIANT=all every one of them (and the shared-NVMe
# layout of unraid-7x-xfs-single-parity); LAYOUT=shared-nvme with that variant
# runs the shared-NVMe layout alone. Requires HOSERVA_LAB_ID and tears the VM down
# on exit. nightly-migration.yml runs it as a matrix over the variants.
vm-migration-suite:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-migration-suite VARIANT=unraid-7x-xfs-single-parity)" >&2; exit 1; }
	@test -n "$$VARIANT" || { echo "set VARIANT (e.g. make vm-migration-suite VARIANT=unraid-7x-xfs-single-parity, or VARIANT=all)" >&2; exit 1; }
	scripts/vm/run-migration-suite.sh "$$VARIANT"

# Phase 1's L3 soak (doc 06 §6, Q16): 30 nightly chains back to back over
# seeded churn, with injected failures. Time-compressed; does not wait
# calendar nights. Requires HOSERVA_LAB_ID. Tears the VM down on exit.
vm-soak:
	@test -n "$$HOSERVA_LAB_ID" || { echo "set HOSERVA_LAB_ID (e.g. HOSERVA_LAB_ID=dev make vm-soak)" >&2; exit 1; }
	scripts/vm/run-l3-soak.sh
