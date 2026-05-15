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

# test depends on bridge so the embedded dylib is present on
# darwin/arm64 (go:embed fails the build if the file is missing). On
# other platforms bridge is a no-op so this dependency is free.
test: bridge
	$(GO) test -race -count=1 ./...

test-integration: bridge
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

# bridge builds libACBridge.dylib via SwiftPM and copies it into
# runtime/applecontainer/embed/ where go:embed picks it up. Required
# before any Go build that imports runtime/applecontainer on
# darwin/arm64. On other platforms it's a no-op so this target can be
# unconditionally listed as a dependency by `test` / `test-integration`
# without burdening Linux CI.
bridge:
	@if [ "$$(uname -s)" = "Darwin" ] && [ "$$(uname -m)" = "arm64" ]; then \
		cd applecontainer-bridge && swift build -c release && \
		mkdir -p ../runtime/applecontainer/embed && \
		cp .build/arm64-apple-macosx/release/libACBridge.dylib \
		   ../runtime/applecontainer/embed/libACBridge.dylib; \
	else \
		echo "bridge: skipped (requires darwin/arm64)"; \
	fi

bridge-clean:
	@if [ "$$(uname -s)" = "Darwin" ] && [ "$$(uname -m)" = "arm64" ]; then \
		(cd applecontainer-bridge && swift package clean && rm -rf .build) && \
		rm -f runtime/applecontainer/embed/libACBridge.dylib; \
	else \
		echo "bridge-clean: skipped (requires darwin/arm64)"; \
	fi
