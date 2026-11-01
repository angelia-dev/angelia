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
FUZZTIME            ?= 30s
# package:FuzzTarget pairs run by `make fuzz`; go test -fuzz takes one target at a time.
FUZZ_TARGETS := \
	./internal/version:FuzzNatural \
	./internal/version/semver:FuzzParse \
	./internal/version/semver:FuzzCompare \
	./internal/version/pep440:FuzzParse \
	./internal/version/pep440:FuzzCompare \
	./internal/version/schemes:FuzzFallbackParse
GOVULNCHECK_VERSION := v1.9.0

.PHONY: help build test fuzz lint vuln generate clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/angelia (CGO disabled, version info injected)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/angelia

test: ## Run unit tests with the race detector
	go test -race ./...

fuzz: ## Run each fuzz target for FUZZTIME (default 30s)
	@set -e; for t in $(FUZZ_TARGETS); do \
		echo "fuzz $$t for $(FUZZTIME)"; \
		go test -run '^$$' -fuzz "^$${t#*:}$$" -fuzztime $(FUZZTIME) "$${t%%:*}"; \
	done

lint: ## Run golangci-lint and the gofumpt format check
	$(GOLANGCI_LINT) run
	$(GOLANGCI_LINT) fmt --diff

vuln: ## Check for known vulnerabilities (needs access to vuln.go.dev)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

generate: ## Run go generate (sqlc, oapi-codegen); commit the output
	go generate ./...

clean: ## Remove build output
	rm -rf bin
