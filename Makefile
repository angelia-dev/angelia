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

GOLANGCI_LINT         ?= golangci-lint
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.9.0
FUZZTIME              ?= 30s
# package:FuzzTarget pairs run by `make fuzz`; go test -fuzz takes one target at a time.
FUZZ_TARGETS := \
	./internal/version:FuzzNatural \
	./internal/version/semver:FuzzParse \
	./internal/version/semver:FuzzCompare \
	./internal/version/semver:FuzzParseLoose \
	./internal/version/pep440:FuzzParse \
	./internal/version/pep440:FuzzCompare \
	./internal/version/calver:FuzzParse \
	./internal/version/calver:FuzzCompare \
	./internal/version/schemes:FuzzFallbackParse \
	./internal/version/schemes:FuzzFallbackTransitive

.PHONY: help build test fuzz lint lint-tools vuln generate bench-ordering bench-ordering-snapshot bench-scale bench-idle bench-github bench-github-repos bench-crash clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build bin/angelia (CGO disabled, version info injected)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/angelia

test: ## Run unit tests with the race detector (and the benchprof hook's)
	go test -race ./...
	go test -race -tags benchprof -run TestBenchProfile ./cmd/angelia

fuzz: ## Run each fuzz target for FUZZTIME (default 30s)
	@set -e; for t in $(FUZZ_TARGETS); do \
		echo "fuzz $$t for $(FUZZTIME)"; \
		go test -run '^$$' -fuzz "^$${t#*:}$$" -fuzztime $(FUZZTIME) "$${t%%:*}"; \
	done

# Same toolchain pin as vuln: a go1.26 build cannot lint go1.27 code.
lint-tools: ## Install the pinned golangci-lint version into GOBIN
	GOTOOLCHAIN=$$(go env GOVERSION) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

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

bench-ordering: ## Benchmark 6: version ordering vs the committed PyPI/npm snapshots (offline)
	go run ./bench/ordering

# Needs network (pypi.org, registry.npmjs.org), Python 3 with packaging and Node.js with npm.
bench-ordering-snapshot: ## Rebuild the benchmark 6 snapshots from the live registries
	python3 -I bench/ordering/snapshot/pypi.py bench/ordering/packages/pypi.txt bench/ordering/testdata/pypi.jsonl.gz
	node bench/ordering/snapshot/npm.mjs bench/ordering/packages/npm.txt bench/ordering/testdata/npm.jsonl.gz

# BENCH_N watches against the fake registry; Docker by default, BENCH_CONTAINER=0
# for a plain process. `make bench-scale N=100` works too. Other knobs:
# bench/load/README.md; the benchmark machine and runs: bench/README.md.
# Exit status 3 means incomplete (deliberately an error). bench-scale also
# writes pprof CPU and heap profiles (BENCH_PROFILE=0 turns that off).
BENCH_N ?= $(or $(N),10000)
bench-scale: ## Benchmark 1: BENCH_N watches (default 10000) against the fake registry, report in bin/bench
	BENCH_N=$(BENCH_N) MAKE="$(MAKE)" bench/load/bench-scale.sh

bench-idle: ## Benchmark 2: idle memory, binary and image size with 100 watches (N=…), report in bin/bench
	BENCH_KIND=idle BENCH_N=$(or $(N),100) MAKE="$(MAKE)" bench/load/bench-scale.sh

# Benchmark 3 needs ANGELIA_BENCH_GITHUB_TOKEN (bench/README.md); BENCH_GITHUB_FAKE=1
# is the dry run against the fake registry. 24 hours by default (BENCH_DURATION).
bench-github: ## Benchmark 3: 2,000 GitHub repos (N=…) on one token for 24 h, report in bin/bench
	BENCH_KIND=github BENCH_N=$(or $(N),2000) MAKE="$(MAKE)" bench/load/bench-scale.sh

# Benchmark 5 runs anywhere on Linux (process mode, no Docker), about 17 minutes.
# BENCH_KILLS (default 100), BENCH_ENV_NOTE (the machine, for the report) and
# BENCH_CRASH_FLAGS (more flags: go run ./bench/crash -h) change it.
bench-crash: build ## Benchmark 5: SIGKILL angelia 100 times during dispatch, report in bin/bench/crash
	go run ./bench/crash -angelia bin/$(BINARY) -kills $(or $(BENCH_KILLS),100) -out bin/bench/crash \
		-env-note "$(BENCH_ENV_NOTE)" $(BENCH_CRASH_FLAGS)

bench-github-repos: ## Rebuild bench/github/repos.txt from GitHub search (needs ANGELIA_BENCH_GITHUB_TOKEN)
	go run ./bench/github/genrepos -out bench/github/repos.txt

clean: ## Remove build output
	rm -rf bin
