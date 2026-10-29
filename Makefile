.PHONY: build install regen check-generated test test-cover lint tidy clean release-dry help

BINARY  := twoctl
PKG     := github.com/two-inc/twoctl-cli
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X $(PKG)/internal/httpx.Version=$(VERSION) -s -w

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the twoctl binary into ./twoctl.
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/twoctl

install: ## Install twoctl into $$GOBIN (or $$GOPATH/bin).
	go install -ldflags "$(LDFLAGS)" ./cmd/twoctl

regen: ## Refresh embedded OpenAPI specs from openapi/<api>-api.yaml.
	./scripts/codegen.sh

check-generated: ## Regenerate embedded specs and fail if committed output is stale.
	./scripts/check-generated.sh

test: ## Run all unit tests.
	go test ./...

test-cover: ## Run tests with coverage; fail if below COVERAGE_THRESHOLD (default 80).
	go test -coverprofile=coverage.out -covermode=atomic ./...
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ { sub(/%/, "", $$3); print $$3 }'); \
	echo "total coverage: $$total%"; \
	awk -v t=$$total -v thresh=$(COVERAGE_THRESHOLD) 'BEGIN { if (t+0 < thresh+0) { exit 1 } }' || \
	  { echo "::error::coverage $$total% below threshold $(COVERAGE_THRESHOLD)%"; exit 1; }
	@echo "html report: open coverage.html"
	go tool cover -html=coverage.out -o coverage.html

COVERAGE_THRESHOLD ?= 80

lint: ## Run the same lints that goreportcard.com runs.
	./scripts/lint.sh

tidy: ## Tidy go.mod / go.sum.
	go mod tidy

release-dry: ## Dry-run a goreleaser build for the current OS.
	goreleaser build --snapshot --clean --single-target

clean: ## Remove build artefacts.
	rm -f $(BINARY) coverage.out coverage.html
	rm -rf dist/ .build/
