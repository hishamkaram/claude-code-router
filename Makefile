BIN_DIR := ./bin
CCR := $(BIN_DIR)/ccr
GO := go
GOFLAGS := -race
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILT_BY ?= make
LDFLAGS := -s -w -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Version=$(VERSION) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Commit=$(COMMIT) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Date=$(DATE) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.BuiltBy=$(BUILT_BY)

.PHONY: build test test-race test-live test-live-fixture test-live-real test-live-matrix test-live-switch test-live-subagents test-live-workflows vet lint coverage govulncheck check check-live check-live-fixture check-live-real clean help

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags="$(LDFLAGS)" -o $(CCR) ./cmd/ccr

test:
	$(GO) test -count=1 ./...

test-race:
	$(GO) test $(GOFLAGS) -count=1 -p 4 ./...

test-live:
	$(GO) test -tags=live -count=1 -p 1 ./...

test-live-fixture:
	$(GO) test -tags=live -count=1 -p 1 -run '^(TestLiveFixtureMatrix|TestLiveClaudeConformanceMatrix|TestLiveFixtureMalformedResponses|TestLiveLaunchOpenAIProviderStreamsAgentToolInput|TestLiveLaunchOpenAIProviderRunsDynamicWorkflow|TestLiveLaunchAnthropicCompatibleProviderAutoModePluginResearchAgent)$$' ./internal/cli

test-live-real:
	@test "$$CCR_LIVE_REAL_MATRIX" = "1" || (echo "CCR_LIVE_REAL_MATRIX=1 is required" >&2; exit 1)
	CCR_LIVE_CONFIGURED_PROVIDER=1 $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^(TestLiveRealProviderMatrix|TestLiveConfiguredProviderAutoModeAgentWebFetch|TestLiveConfiguredProviderAutoModeWorkflow)$$' ./internal/cli

test-live-matrix: test-live-fixture test-live-real

test-live-switch:
	$(GO) test -tags=live -count=1 -p 1 -run TestLiveModelSwitch ./...

test-live-subagents:
	$(GO) test -tags=live -count=1 -p 1 -run TestLiveSubagents ./...

test-live-workflows:
	$(GO) test -tags=live -count=1 -p 1 -run TestLiveWorkflows ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

coverage:
	$(GO) test $(GOFLAGS) -count=1 -p 4 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

govulncheck:
	govulncheck ./...

check: vet lint test-race govulncheck

check-live: check test-live

check-live-fixture: check test-live-fixture

check-live-real: check test-live-real

clean:
	rm -rf $(BIN_DIR)
	rm -f coverage.out coverage.html
	go clean -cache -testcache

help:
	@sed -n 's/^\([a-zA-Z0-9_-]*\):.*/\1/p' $(MAKEFILE_LIST) | sort
