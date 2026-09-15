# Every workflow goes through this Makefile (doc 12 §3) — if it isn't a
# target here, it doesn't exist. Only the targets this repository can
# actually satisfy today are defined; the rest (dev, mock, gen,
# db-migration, vm-*, deb, iso, ...) arrive with the issues that build what
# they need, so no target here pretends to work.

GO           ?= go
BIN_DIR      := bin
CMDS         := hoservad hoserva mockapi

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
LAB_ID_PATTERN  := ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$$
LAB_SEED_PROFILE ?= mixed

.PHONY: build test test-unit lint clean lab-up lab-seed lab-destroy lab-verify-refusal lab-require-id

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
	rm -rf -- ".lab/$$HOSERVA_LAB_ID"
