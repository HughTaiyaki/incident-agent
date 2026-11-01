GO ?= go
# Public dependencies and toolchains must be checksum-verified for project checks.
export GOSUMDB := sum.golang.org
BINARY_NAME := incident-api
GOLANGCI_VERSION := v2.15.0
GOVULNCHECK_VERSION := v1.9.0
LINT := GOSUMDB=sum.golang.org $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
VULNCHECK := GOSUMDB=sum.golang.org $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

.PHONY: build fmt help lint run test test-race test-integration vet verify

## build: Build the API, query, offline investigation, model and worker binaries.
build:
	$(GO) build -o bin/$(BINARY_NAME) ./cmd/incident-api
	$(GO) build -o bin/incident-query ./cmd/incident-query
	$(GO) build -o bin/incident-investigate ./cmd/incident-investigate
	$(GO) build -o bin/incident-model ./cmd/incident-model
	$(GO) build -o bin/incident-worker ./cmd/incident-worker

## fmt: Format all Go packages.
fmt:
	$(GO) fmt ./...

## vet: Run the Go vet analyzer.
vet:
	$(GO) vet ./...

## test: Run unit tests.
test:
	$(GO) test ./...

## test-race: Run unit tests with the race detector.
test-race:
	$(GO) test -race ./...

## test-integration: Query running telemetry backends using INCIDENT_INTEGRATION_QUERIES.
test-integration:
	$(GO) test -tags=integration ./internal/adapter/outbound/telemetryhttp -run TestLiveContract -count=1

## lint: Run configured linters.
lint:
	$(LINT) run ./...

## verify: Run formatting, vet, race tests, and lint.
verify: fmt vet test-race lint

## run: Run the local incident API.
run:
	$(GO) run ./cmd/incident-api

## help: List available targets.
help:
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':'

.PHONY: test-redis
## test-redis: Test persistence scripts on temporary isolated redis-server processes.
test-redis:
	$(GO) test -race -tags=redisintegration ./internal/adapter/outbound/redisstate -count=1

.PHONY: check-fmt lint-redis security release-check
## check-fmt: Reject unformatted files without modifying them.
check-fmt:
	$(GO) run ./internal/checks/format

## lint-redis: Lint Redis and worker integration tests as well as the application.
lint-redis:
	$(LINT) run --build-tags=redisintegration ./...

## security: Fail on reachable vulnerabilities in application and tagged recovery paths.
security:
	$(VULNCHECK) ./...
	$(VULNCHECK) -tags=redisintegration ./...

## release-check: Run required local publication checks; never deploy or publish.
release-check:
	$(MAKE) check-fmt
	$(GO) mod verify
	$(MAKE) vet
	$(MAKE) test-race
	$(MAKE) lint-redis
	$(MAKE) test-redis
	$(MAKE) build
	$(MAKE) security
