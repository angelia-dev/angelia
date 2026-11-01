# Developer entry points. They mirror the commands in AGENTS.md.

BINARY    := angelia
BUILDINFO := github.com/angelia-dev/angelia/internal/buildinfo
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

GOVULNCHECK_VERSION := v1.7.0

.PHONY: build test lint vuln generate clean

build: ## Build bin/angelia with CGO disabled and version info injected
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/angelia

test: ## Run unit tests with the race detector
	go test -race ./...

lint: ## Run golangci-lint and check gofumpt formatting (config in .golangci.yml)
	golangci-lint run
	golangci-lint fmt --diff

vuln: ## Check dependencies and code for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

generate: ## Run go generate (sqlc, oapi-codegen); commit the result
	go generate ./...

clean: ## Remove build output
	rm -rf bin
