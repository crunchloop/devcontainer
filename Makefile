.PHONY: all test test-integration lint fmt vet tidy clean tools bridge bridge-clean

GO ?= go
GOLANGCI_LINT ?= golangci-lint
# Keep in sync with .github/workflows/ci.yml (lint job)
GOLANGCI_LINT_VERSION ?= v2.5.0

all: lint test

# tools installs developer tooling under $GOPATH/bin (or $GOBIN). Re-run
# to refresh after bumping GOLANGCI_LINT_VERSION.
tools:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

test:
	$(GO) test -race -count=1 ./...

test-integration:
	$(GO) test -race -count=1 -tags=integration -timeout=10m ./test/integration/...

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

clean:
	$(GO) clean -testcache

# bridge builds libACBridge.dylib via SwiftPM. Required before any Go
# code that imports runtime/applecontainer can link on darwin/arm64.
# darwin/arm64 only; on other platforms runtime/applecontainer's stub
# file builds without the dylib so this target should not be invoked.
# Guarded with a uname check rather than left unconditional: invoking
# `make bridge` on linux/amd64 prints a clear skip message instead of
# failing with "swift: command not found".
bridge:
	@if [ "$$(uname -s)" = "Darwin" ] && [ "$$(uname -m)" = "arm64" ]; then \
		cd applecontainer-bridge && swift build -c release; \
	else \
		echo "bridge: skipped (requires darwin/arm64)"; \
	fi

bridge-clean:
	@if [ "$$(uname -s)" = "Darwin" ] && [ "$$(uname -m)" = "arm64" ]; then \
		cd applecontainer-bridge && swift package clean && rm -rf .build; \
	else \
		echo "bridge-clean: skipped (requires darwin/arm64)"; \
	fi
