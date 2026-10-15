.PHONY: build server install-cli upgrade test vet check clean

# Release version from VERSION, plus the commit (and .dirty for uncommitted
# changes) as build metadata. Bump VERSION for every build installed on a
# server: site-deploy upgrade refuses to replace a daemon with the same or an
# older version unless forced.
VERSION := $(shell cat VERSION)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
DIRTY := $(shell git status --porcelain 2>/dev/null | grep -q . && echo .dirty)
BUILD := $(VERSION)$(if $(COMMIT),+$(COMMIT)$(DIRTY))
PREFIX ?= $(HOME)/.local
GOARCH ?= $(shell go env GOARCH)
# Server builds that install-cli puts beside the CLI, for site-deploy upgrade.
SERVER_ARCHES := amd64 arm64
LDFLAGS := -X site-deploy/internal/version.Version=$(BUILD)

# Static, so a binary built here also runs on the server's older glibc.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/site-deploy ./cmd/site-deploy

# A Linux binary for a server's architecture: make server GOARCH=arm64
server:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linux-$(GOARCH)/site-deploy ./cmd/site-deploy

# The CLI, plus a Linux build per server architecture in lib/site-deploy/,
# which site-deploy upgrade installs on servers whose CPU differs from this one.
install-cli: build
	for arch in $(SERVER_ARCHES); do $(MAKE) --no-print-directory server GOARCH=$$arch || exit 1; \
		install -D -m 0755 bin/linux-$$arch/site-deploy $(PREFIX)/lib/site-deploy/linux-$$arch/site-deploy || exit 1; done
	install -D -m 0755 bin/site-deploy $(PREFIX)/bin/site-deploy
	install -D -m 0755 deploy/site-deploy-remote $(PREFIX)/bin/site-deploy-remote

# Installs this checkout as the local CLI, then as HOST's daemon:
# make upgrade HOST=prod [FORCE=1]
upgrade: install-cli
	$(PREFIX)/bin/site-deploy upgrade $(HOST) $(if $(FORCE),--force)

test:
	go test -race ./...

vet:
	go vet ./...

check: test vet

clean:
	rm -rf bin
