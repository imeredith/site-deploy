.PHONY: build server install-cli test vet check clean

# Release version from VERSION, plus the commit (and .dirty for uncommitted
# changes) as build metadata. Bump VERSION for every build installed on a
# server: deploy/upgrade.sh refuses to replace a daemon with the same or an
# older version unless forced.
VERSION := $(shell cat VERSION)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
DIRTY := $(shell git status --porcelain 2>/dev/null | grep -q . && echo .dirty)
BUILD := $(VERSION)$(if $(COMMIT),+$(COMMIT)$(DIRTY))
PREFIX ?= $(HOME)/.local
GOARCH ?= $(shell go env GOARCH)
LDFLAGS := -X site-deploy/internal/version.Version=$(BUILD)

# Static, so a binary built here also runs on the server's older glibc.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/site-deploy ./cmd/site-deploy

# A Linux binary for a server's architecture: make server GOARCH=arm64
server:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o bin/linux-$(GOARCH)/site-deploy ./cmd/site-deploy

install-cli: build
	install -D -m 0755 bin/site-deploy $(PREFIX)/bin/site-deploy
	install -D -m 0755 deploy/site-deploy-remote $(PREFIX)/bin/site-deploy-remote

test:
	go test -race ./...

vet:
	go vet ./...

check: test vet

clean:
	rm -rf bin
