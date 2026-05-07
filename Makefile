.PHONY: all test test-integration lint fmt vet tidy clean

GO ?= go
GOLANGCI_LINT ?= golangci-lint

all: lint test

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
