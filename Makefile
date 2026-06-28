# ============================================================
#  Moodist — Makefile
#  Produces a single self-contained binary via `go embed`.
# ============================================================

BINARY     ?= moodist
DIST_DIR   := cmd/moodist/dist
WEB_DIR    := web
GOOS       ?= $(shell go env GOOS)
GOARCH     ?= $(shell go env GOARCH)
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -s -w -X main.version=$(VERSION)

.PHONY: all build build-web build-go clean run dev help

## all: build everything (default)
all: build

## build: build the frontend then compile the Go binary
build: build-web build-go

## build-web: install npm deps and compile the Astro frontend to cmd/moodist/dist/
build-web:
	@echo "→ installing frontend dependencies…"
	cd $(WEB_DIR) && pnpm install --frozen-lockfile
	@echo "→ building frontend…"
	cd $(WEB_DIR) && pnpm build
	@echo "→ copying build output to embed path…"
	rm -rf $(DIST_DIR)
	cp -r $(WEB_DIR)/dist $(DIST_DIR)
	@echo "✓ frontend built at $(DIST_DIR)"

## build-go: compile the Go binary (requires $(DIST_DIR) to exist)
build-go:
	@test -d $(DIST_DIR) || (echo "ERROR: $(DIST_DIR) missing — run 'make build-web' first" && exit 1)
	@echo "→ compiling Go binary…"
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BINARY) ./cmd/moodist
	@echo "✓ binary: ./$(BINARY)"

## run: build then run the binary
run: build
	./$(BINARY)

## dev: run the Go server against a pre-built dist (no Node rebuild)
dev:
	@test -d $(DIST_DIR) || (echo "ERROR: run 'make build-web' first" && exit 1)
	go run ./cmd/moodist -addr :8080 -log-level debug

## clean: remove build artefacts
clean:
	rm -f $(BINARY)
	rm -rf $(DIST_DIR)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
