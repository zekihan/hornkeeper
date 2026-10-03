GO ?= go
HELM ?= helm
PYTHON ?= python3
BINARY := dist/hornkeeper
VERSION := $(shell cat VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build fmt tidy lint test test/cover audit test/integration container-test release-check chart-test chart-package
build:
	mkdir -p dist
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/hornkeeper
fmt:
	gofmt -w cmd internal

tidy:
	$(GO) mod tidy
lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)
	golangci-lint run --build-tags=integration

test:
	$(GO) test -race -count=1 ./...

test/cover:
	mkdir -p tmp
	$(GO) test -race -coverprofile=tmp/coverage.out ./...

audit: lint test
	$(GO) mod verify

test/integration:
	./scripts/integration.sh

container-test:
	./scripts/container_smoke.sh

release-check:
	goreleaser check

chart-test:
	$(HELM) lint --strict charts/hornkeeper
	HELM=$(HELM) $(PYTHON) tests/helm/test_chart.py

chart-package: chart-test
	mkdir -p dist/charts
	$(HELM) package charts/hornkeeper --destination dist/charts --version "$(VERSION)" --app-version "$(VERSION)"
