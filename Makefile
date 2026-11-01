# Developer entry points. They mirror the commands in AGENTS.md.
# Run `make` or `make help` to list the targets.

.DEFAULT_GOAL := help

BINARY    := angelia
BUILDINFO := github.com/angelia-dev/angelia/internal/buildinfo

# := keeps same-named environment variables from leaking in; command-line
# overrides such as `make VERSION=v1.0.0` still work.
VERSION := $(shell git describe --tags --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
# Commit time of HEAD in UTC, so rebuilding the same commit gives the same date.
DATE    := $(shell TZ=UTC git log -1 --date=format-local:%Y-%m-%dT%H:%M:%SZ --format=%cd 2>/dev/null || echo unknown)
LDFLAGS := -X $(BUILDINFO).version=$(VERSION) -X $(BUILDINFO).commit=$(COMMIT) -X $(BUILDINFO).date=$(DATE)

GOLANGCI_LINT       ?= golangci-lint
GOVULNCHECK_VERSION := v1.9.0

.PHONY: help build test lint vuln generate clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/angelia (CGO disabled, version info injected)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/angelia

test: ## Run unit tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint and the gofumpt format check
	$(GOLANGCI_LINT) run
	$(GOLANGCI_LINT) fmt --diff

# govulncheck's go.mod asks for Go 1.26, so `go run` would build it with go1.26
# and it could not load this module (go 1.27). Build it with the toolchain this
# module uses instead.
vuln: ## Check for known vulnerabilities (needs access to vuln.go.dev)
	GOTOOLCHAIN=$$(go env GOVERSION) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

generate: ## Run go generate (sqlc, oapi-codegen); commit the output
	go generate ./...

clean: ## Remove build output
	rm -rf bin
