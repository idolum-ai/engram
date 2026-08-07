PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY := engram
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/idolum-ai/engram/internal/version.Version=$(VERSION) -X github.com/idolum-ai/engram/internal/version.Commit=$(COMMIT) -X github.com/idolum-ai/engram/internal/version.Date=$(DATE)
GOCACHE ?= /tmp/engram-go-build
GOMODCACHE ?= /tmp/engram-go-mod
ENGRAM_ENV ?= $(HOME)/.engram/.env

.PHONY: build release-dist release-smoke install install-release uninstall install-service install-service-unit service-start service-stop service-restart service-status service-logs uninstall-service test test-race vet darwin-compile check architecture public-readiness secrets workflow-sanity stdlib-only docs-freshness smoke run

build:
	mkdir -p bin
	# Release identity comes from LDFLAGS; do not also embed checkout-specific VCS metadata.
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go build -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/engram

release-dist:
	@if [ "$(VERSION)" = "dev" ]; then echo "VERSION=vX.Y.Z is required" >&2; exit 2; fi
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) RELEASE_COMMIT=$(COMMIT) RELEASE_DATE=$(DATE) ./scripts/package-release.sh "$(VERSION)" dist

release-smoke:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./scripts/check-release.sh

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/$(BINARY) $(BINDIR)/$(BINARY)

install-release:
	@if [ "$(VERSION)" = "dev" ]; then ./scripts/install-release.sh; else ./scripts/install-release.sh "$(VERSION)"; fi

uninstall:
	rm -f $(BINDIR)/$(BINARY)

install-service: install
	@$(MAKE) --no-print-directory install-service-unit PREFIX="$(PREFIX)" BINDIR="$(BINDIR)"

install-service-unit:
	install -d -m 0700 $(HOME)/.engram
	@if [ ! -f "$(ENGRAM_ENV)" ]; then install -m 0600 .env.example "$(ENGRAM_ENV)"; fi
	bash scripts/user-service.sh install "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

service-start:
	bash scripts/user-service.sh start "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

service-stop:
	bash scripts/user-service.sh stop "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

service-restart:
	bash scripts/user-service.sh restart "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

service-status:
	bash scripts/user-service.sh status "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

service-logs:
	bash scripts/user-service.sh logs "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

uninstall-service:
	bash scripts/user-service.sh uninstall "$(BINDIR)/$(BINARY)" "$(ENGRAM_ENV)"

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -race ./...

vet:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go vet ./...

darwin-compile:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) GOOS=darwin GOARCH=amd64 go test -exec=true ./...
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) GOOS=darwin GOARCH=arm64 go test -exec=true ./...

check: test test-race vet darwin-compile build release-smoke architecture public-readiness secrets workflow-sanity stdlib-only docs-freshness smoke

architecture:
	bash scripts/check-architecture.sh

public-readiness:
	bash scripts/check-public-readiness.sh

secrets:
	bash scripts/check-secrets.sh

workflow-sanity:
	bash scripts/check-workflows.sh

stdlib-only:
	bash scripts/check-stdlib-only.sh

docs-freshness:
	bash scripts/check-docs-freshness.sh

smoke: build
	bash scripts/smoke.sh

run:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go run ./cmd/engram run --env "$(ENGRAM_ENV)"
