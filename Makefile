PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
BINARY := engram
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/idolum-ai/engram/internal/version.Version=$(VERSION) -X github.com/idolum-ai/engram/internal/version.Commit=$(COMMIT) -X github.com/idolum-ai/engram/internal/version.Date=$(DATE)
GOCACHE ?= /tmp/engram-go-build
GOMODCACHE ?= /tmp/engram-go-mod

.PHONY: build install uninstall install-service uninstall-service test test-race check architecture public-readiness secrets workflow-sanity smoke run

build:
	mkdir -p bin
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/engram

install: build
	mkdir -p $(BINDIR)
	install -m 0755 bin/$(BINARY) $(BINDIR)/$(BINARY)

uninstall:
	rm -f $(BINDIR)/$(BINARY)

install-service: install
	mkdir -p $(HOME)/.config/systemd/user $(HOME)/.engram
	@if [ ! -f "$(HOME)/.engram/.env" ]; then cp .env.example "$(HOME)/.engram/.env"; fi
	printf '%s\n' \
		'[Unit]' \
		'Description=Engram Telegram tmux client' \
		'After=default.target' \
		'' \
		'[Service]' \
		'Type=simple' \
		'ExecStart=$(BINDIR)/$(BINARY) run --env %h/.engram/.env' \
		'Restart=on-failure' \
		'RestartSec=5' \
		'' \
		'[Install]' \
		'WantedBy=default.target' \
		> $(HOME)/.config/systemd/user/engram.service
	systemctl --user daemon-reload
	systemctl --user enable --now engram.service

uninstall-service:
	-systemctl --user disable --now engram.service
	rm -f $(HOME)/.config/systemd/user/engram.service
	systemctl --user daemon-reload

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-race:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test -race ./internal/state ./internal/lockfile ./internal/telegram ./internal/tmux

check: test build architecture public-readiness secrets workflow-sanity smoke

architecture:
	bash scripts/check-architecture.sh

public-readiness:
	bash scripts/check-public-readiness.sh

secrets:
	bash scripts/check-secrets.sh

workflow-sanity:
	bash scripts/check-workflows.sh

smoke: build
	bash scripts/smoke.sh

run:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go run ./cmd/engram run --env .env
