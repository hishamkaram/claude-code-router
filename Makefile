BIN_DIR := ./bin
CCR := $(BIN_DIR)/ccr
CUA_MACOS_HELPER := $(BIN_DIR)/ccr-cua-macos
GO := go
GOFLAGS := -race
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --verify HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILT_BY ?= make
LDFLAGS := -s -w -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Version=$(VERSION) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Commit=$(COMMIT) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.Date=$(DATE) -X github.com/hishamkaram/claude-code-router/internal/buildinfo.BuiltBy=$(BUILT_BY)

.PHONY: build build-cua-macos test test-race test-cua-browser-entrypoint test-cua-docker-fixture test-cua-macos-fixture test-live test-live-fixture test-live-real test-live-real-routing test-live-real-vision test-live-real-anthropic-cua test-live-real-openai-responses-cua test-live-real-cua-executors test-live-real-full test-live-matrix test-live-matrix-full test-live-switch test-live-subagents test-live-workflows vet lint coverage govulncheck maintainability check check-live check-live-fixture check-live-real check-live-real-full clean help

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags="$(LDFLAGS)" -o $(CCR) ./cmd/ccr

build-cua-macos:
	@test "$$(uname -s)" = Darwin || (echo "build-cua-macos requires macOS" >&2; exit 1)
	@command -v swiftc >/dev/null || (echo "build-cua-macos requires swiftc" >&2; exit 1)
	@mkdir -p $(BIN_DIR)
	swiftc -framework Foundation -framework AppKit -framework CoreGraphics -framework ScreenCaptureKit \
		cmd/ccr-cua-macos/Sources/*.swift -o $(CUA_MACOS_HELPER)

test:
	$(GO) test -count=1 ./...

test-race:
	$(GO) test $(GOFLAGS) -count=1 -p 4 ./...

test-cua-browser-entrypoint:
	@command -v node >/dev/null || (echo "test-cua-browser-entrypoint requires node" >&2; exit 1)
	node --test docs/release/cua-browser-entrypoint.test.js

test-cua-docker-fixture: test-cua-browser-entrypoint
	@set -e; \
	trap 'docker image rm ccr-cua-browser-fixture:local >/dev/null 2>&1 || true' EXIT; \
	docker build --file docs/release/browser-image.Dockerfile --tag ccr-cua-browser-fixture:local .; \
	CCR_CUA_DOCKER_SMOKE=1 CCR_CUA_DOCKER_IMAGE=ccr-cua-browser-fixture:local $(GO) test -count=1 -timeout 2m -run '^TestDockerBrowserSmoke$$' ./internal/cua/executor

test-cua-macos-fixture:
	@command -v python3 >/dev/null || (echo "test-cua-macos-fixture requires python3" >&2; exit 1)
	python3 -m unittest discover -s cmd/ccr-cua-macos/tests -v

test-live:
	$(GO) test -tags=live -count=1 -p 1 ./...

test-live-fixture:
	$(GO) test -tags=live -count=1 -p 1 -run '^(TestLiveFixture.*|TestLiveClaudeConformanceMatrix|TestLiveLaunchOpenAIProviderStreamsAgentToolInput|TestLiveLaunchOpenAIProviderRunsDynamicWorkflow|TestLiveLaunchAnthropicCompatibleProviderAutoModePluginResearchAgent)$$' ./internal/cli

test-live-real-routing:
	@test "$$CCR_LIVE_REAL_MATRIX" = "1" || (echo "CCR_LIVE_REAL_MATRIX=1 is required" >&2; exit 1)
	CCR_LIVE_CONFIGURED_PROVIDER=1 $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^(TestLiveRealProviderMatrix|TestLiveConfiguredProviderAutoModeAgentWebFetch|TestLiveConfiguredProviderAutoModeWorkflow)$$' ./internal/cli

test-live-real-vision:
	CCR_LIVE_CONFIGURED_PROVIDER=1 CCR_LIVE_REAL_VISION=1 $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^TestLiveLocalRealConfiguredVision$$' ./internal/cli

test-live-real-anthropic-cua:
	CCR_LIVE_REAL_ANTHROPIC_CUA=1 $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^TestLiveLocalRealAnthropicCUA$$' ./internal/cli

test-live-real-openai-responses-cua:
	CCR_LIVE_CONFIGURED_PROVIDER=1 CCR_LIVE_REAL_OPENAI_RESPONSES_CUA=1 $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^TestLiveLocalRealOpenAIResponsesCUA$$' ./internal/cli

test-live-real-cua-executors:
	@set -e; \
	cleanup_docker_image=; \
	if command -v docker >/dev/null; then \
		docker build --file docs/release/browser-image.Dockerfile --tag ccr-cua-browser-fixture:local .; \
		cleanup_docker_image=1; \
	fi; \
	trap 'if [ -n "$$cleanup_docker_image" ]; then docker image rm ccr-cua-browser-fixture:local >/dev/null 2>&1 || true; fi' EXIT; \
	CCR_LIVE_REAL_CUA_EXECUTORS=1 CCR_CUA_DOCKER_IMAGE=ccr-cua-browser-fixture:local $(GO) test -tags=live -count=1 -p 1 -timeout 30m -run '^TestLiveLocalRealCUAExecutors$$' ./internal/cli

test-live-real: test-live-real-routing

test-live-real-full: test-live-real-routing test-live-real-vision test-live-real-anthropic-cua test-live-real-openai-responses-cua test-live-real-cua-executors

test-live-matrix: test-live-fixture test-live-real

test-live-matrix-full: test-live-fixture test-live-real-full

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

maintainability:
	$(GO) run ./tools/maintainability

check: maintainability vet lint test-race govulncheck

check-live: check test-live

check-live-fixture: check test-live-fixture

check-live-real: check test-live-real

check-live-real-full: check test-live-real-full

clean:
	rm -rf $(BIN_DIR)
	rm -f coverage.out coverage.html
	go clean -cache -testcache

help:
	@sed -n 's/^\([a-zA-Z0-9_-]*\):.*/\1/p' $(MAKEFILE_LIST) | sort
