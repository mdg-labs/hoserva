# Every workflow goes through this Makefile (doc 12 §3) — if it isn't a
# target here, it doesn't exist. Only the targets this repository can
# actually satisfy today are defined; the rest (lab-up, dev, mock, gen,
# db-migration, vm-*, deb, iso, ...) arrive with the issues that build what
# they need, so no target here pretends to work.

GO           ?= go
BIN_DIR      := bin
CMDS         := hoservad hoserva mockapi

.PHONY: build test test-unit lint clean

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
