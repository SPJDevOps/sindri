# Sindri build targets. Run `make help` for a list.

BINARY      := sindri
PKG         := github.com/spj/sindri
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

GOLANGCI_VERSION := v2.13.2
SQLC_VERSION     := v1.30.0
COMPOSE_FILE     := deploy/compose/docker-compose.yml

# `make tools` installs into GOBIN, which is often not on PATH. Prefer that
# pinned copy, fall back to PATH, and let the target fail if neither exists:
# a lint step that skips itself makes `make ci` green when CI is not.
TOOLS_BIN     := $(or $(shell go env GOBIN),$(shell go env GOPATH)/bin)
GOLANGCI_LINT := $(or $(wildcard $(TOOLS_BIN)/golangci-lint),$(shell command -v golangci-lint 2>/dev/null))
SQLC          := $(or $(wildcard $(TOOLS_BIN)/sqlc),$(shell command -v sqlc 2>/dev/null))

.DEFAULT_GOAL := help

## help: List available targets.
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | sort

## build: Compile the sindri binary into ./bin.
.PHONY: build
build:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) ./cmd/sindri

## test: Run unit tests with the race detector.
.PHONY: test
test:
	go test -race -count=1 ./...

## test-integration: Run tests tagged `integration` (needs Docker for testcontainers).
.PHONY: test-integration
test-integration:
	go test -race -count=1 -tags=integration -timeout=20m ./...

## cover: Run tests and write an HTML coverage report.
.PHONY: cover
cover:
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "report: coverage.html"

## lint: Run go vet and golangci-lint. Fails if golangci-lint is not installed.
.PHONY: lint
lint: fmt-check
	go vet ./...
	@test -n "$(GOLANGCI_LINT)" || { echo "golangci-lint not found in $(TOOLS_BIN) or PATH. Install it with: make tools"; exit 1; }
	$(GOLANGCI_LINT) run

## fmt: Format all Go source.
.PHONY: fmt
fmt:
	gofmt -s -w .

## fmt-check: Fail if any Go source is not gofmt-clean.
.PHONY: fmt-check
fmt-check:
	@out=$$(gofmt -s -l .); \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi

## tidy: Tidy and verify the module graph.
.PHONY: tidy
tidy:
	go mod tidy
	go mod verify

## generate: Regenerate sqlc typed queries under internal/db.
.PHONY: generate
generate:
	@test -n "$(SQLC)" || { echo "sqlc not found in $(TOOLS_BIN) or PATH. Install it with: make tools"; exit 1; }
	$(SQLC) generate

## tools: Install developer tooling into GOBIN.
.PHONY: tools
tools:
	GOTOOLCHAIN=go1.27.1 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

## up: Start local Postgres and MinIO for development.
.PHONY: up
up:
	docker compose -f $(COMPOSE_FILE) up -d

## down: Stop local dependencies and remove their volumes.
.PHONY: down
down:
	docker compose -f $(COMPOSE_FILE) down -v

## logs: Follow local dependency logs.
.PHONY: logs
logs:
	docker compose -f $(COMPOSE_FILE) logs -f

## clean: Remove build and coverage output.
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) dist coverage.out coverage.html

## ci: Everything CI runs, in order.
.PHONY: ci
ci: lint test build
