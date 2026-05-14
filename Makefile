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
# No-op on other platforms; runtime/applecontainer's stub file builds
# without the dylib.
bridge:
	cd applecontainer-bridge && swift build -c release

bridge-clean:
	cd applecontainer-bridge && swift package clean && rm -rf .build
