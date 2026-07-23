# TypeMore server — developer tasks.
#
# Designed to run under git-bash on Windows as well as Linux/macOS. It uses only
# portable shell features (git-bash provides `git`, `date`, `sh`), and appends
# `.exe` to the built binary on Windows automatically.
#
# Common targets:
#   make run         start the server locally
#   make test        run the test suite (portable; no cgo required)
#   make test-race   run tests under the race detector (needs a C compiler)
#   make lint        run golangci-lint (see `make tools`)
#   make build       build the binary into ./bin with version metadata
#   make tools       install golangci-lint into your Go bin

BINARY := typemore-server
PKG    := github.com/typemore/typemore-server
CMD    := ./cmd/server

# Windows produces .exe binaries; keep the output runnable on both OSes.
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif

# Build metadata, injected via -ldflags. Overridable: `make build VERSION=1.2.3`.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/platform.Version=$(VERSION) \
	-X $(PKG)/internal/platform.Commit=$(COMMIT) \
	-X $(PKG)/internal/platform.BuildDate=$(DATE)

.PHONY: run test test-race lint build tidy tools help

## run: start the server locally
run:
	go run $(CMD)

## test: run all tests (portable; used by the README quickstart)
test:
	go test ./...

## test-race: run tests under the race detector (requires CGO + a C compiler)
test-race:
	go test -race ./...

## lint: run golangci-lint
lint:
	golangci-lint run

## build: compile the binary into ./bin with version metadata
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(EXE) $(CMD)

## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

## tools: install pinned dev tooling
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
