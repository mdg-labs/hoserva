# Every workflow goes through this Makefile (doc 12 §3) — if it isn't a
# target here, it doesn't exist. Only the targets this repository can
# actually satisfy today are defined; the rest (dev, mock, db-migration,
# vm-*, deb, iso, ...) arrive with the issues that build what they need, so
# no target here pretends to work.

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
# workflow or developer shell to a plain repo-relative compose file path
# (this text is spliced into COMPOSE_DEV below via Make's own `$(...)`
# substitution, not a shell variable — never set it from template or user
# input).
ifneq ($(strip $(LAB_COMPOSE_EXTRA)),)
COMPOSE_DEV := docker compose -f docker-compose.dev.yml -f $(LAB_COMPOSE_EXTRA)
endif
LAB_ID_PATTERN  := ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$$
LAB_SEED_PROFILE ?= mixed

.PHONY: build test test-unit lint clean lab-up lab-seed lab-destroy lab-verify-refusal lab-snapraid-check lab-require-id gen api-check

build:
	@mkdir -p $(BIN_DIR)
	@for cmd in $(CMDS); do \
		echo "building $$cmd"; \
		CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/$$cmd ./cmd/$$cmd || exit 1; \
	done

test: test-unit

test-unit:
	CGO_ENABLED=0 $(GO) test ./...

lint:
	@echo "gofmt"
	@fmtout="$$(gofmt -l .)"; \
	if [ -n "$$fmtout" ]; then \
		echo "$$fmtout"; \
		echo "gofmt: files need formatting"; \
		exit 1; \
	fi
	@echo "go vet"
	@$(GO) vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint"; \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed, skipping (gofmt and go vet above still ran)"; \
	fi

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
