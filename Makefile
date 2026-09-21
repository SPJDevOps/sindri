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
COMPOSE_FILE     := deploy/compose/docker-compose.yml

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

## lint: Run go vet, plus golangci-lint when it is installed.
.PHONY: lint
lint: fmt-check
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not found, ran go vet only. Install it with: make tools"; \
	fi

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

## tools: Install developer tooling into GOBIN.
.PHONY: tools
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

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
