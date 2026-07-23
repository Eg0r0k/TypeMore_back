# TypeMore Server

The TypeMore game server: a single static Go binary. This is the **relay v0**
bootstrap — it proves the realtime plumbing (WebSocket connection handling, the
`hello` handshake, and NTP clock exchange) that multiplayer will grow on. It has
**no** rooms, relay, match logic, or database yet; those arrive in later stages
(see `ARCHITECTURE.md` / `BACKEND.md`).

The client↔server wire contract lives in [`docs/PROTOCOL.md`](docs/PROTOCOL.md).

## What's here

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness + build info (JSON) |
| `GET /ws` | WebSocket: `hello` handshake + `ntp_ping`/`ntp_pong` |

## Prerequisites

- **Go 1.26+** — https://go.dev/dl/ (this is all you need to run and test).
- Optional: **Docker Desktop** (for `docker compose`).
- Optional: **make** — on Windows, install via `choco install make` or use the
  raw `go` commands shown below. Run `make` targets from **git-bash** (bundled
  with Git for Windows), not `cmd.exe`.

## Quickstart (Windows, git-bash)

```bash
# 1. Fetch dependencies
go mod download

# 2. Run the server (listens on :8080)
go run ./cmd/server
#    ...or: make run
```

In a second git-bash window:

```bash
# 3. Check health + build info
curl http://localhost:8080/healthz
# {"status":"ok","build":{"version":"dev","commit":"none",...}}
```

Stop the server with `Ctrl+C` — it shuts down gracefully.

### Run the tests

```bash
go test ./...      # or: make test
```

The suite includes a real end-to-end WebSocket integration test (spins up a
server on a random port, connects a real client, runs the full `hello` + 5×NTP
exchange, and checks the offset math and error paths).

> **Race detector note:** `go test -race` (and `make test-race`, used by CI)
> needs cgo and a C compiler. A stock Windows box usually lacks one, so use plain
> `make test` locally; the race detector runs in CI on Linux.

### Lint

```bash
make tools    # one-time: installs golangci-lint into your Go bin
make lint     # or: golangci-lint run
```

Ensure your Go bin dir is on `PATH` (git-bash):
`export PATH="$PATH:$(go env GOPATH)/bin"`.

### Build a binary

```bash
make build            # -> bin/typemore-server(.exe) with version metadata
./bin/typemore-server.exe
```

## Docker

App-only compose (no Postgres/Redis this phase):

```bash
docker compose up --build
curl http://localhost:8080/healthz
```

## Configuration

All configuration is via `TYPEMORE_`-prefixed environment variables; every one
has a working default (see [`.env.example`](.env.example)). Copy it to `.env` to
override locally.

| Variable | Default | Meaning |
|---|---|---|
| `TYPEMORE_ADDR` | `:8080` | Listen address (`host:port`) |
| `TYPEMORE_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `TYPEMORE_LOG_FORMAT` | `json` | `json` or `text` |
| `TYPEMORE_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown budget |
| `TYPEMORE_READ_HEADER_TIMEOUT` | `5s` | Header read timeout |
| `TYPEMORE_ALLOWED_ORIGINS` | *(empty)* | WebSocket Origin allow-list; empty = allow any (dev) |

## Project layout

```
cmd/server/            # composition root (main): wiring + lifecycle
internal/
  platform/            # config, logging, HTTP lifecycle, health/build info (no domain deps)
  protocol/            # wire message types, version constant, JSON codec (mirrors docs/PROTOCOL.md)
  ws/                  # WebSocket transport: connection handling + hello/NTP
docs/PROTOCOL.md       # the client↔server contract (shared with the frontend)
```

The package layout follows `BACKEND.md §2`; only the packages this phase needs
exist yet. Layering rule: `platform` imports no domain package.

## The WebSocket concurrency model (for the next maintainer)

Every connection is served by three goroutines with strict ownership — this is
the template the relay phase extends:

- the HTTP handler goroutine owns the connection lifecycle (accept → wire up →
  final close);
- **one writer goroutine** is the *only* caller of `Conn.Write` (the library
  forbids concurrent writes) — all outbound frames funnel through a channel to it;
- **one reader goroutine** is the *only* caller of `Conn.Read`.

See the package doc in `internal/ws/handler.go` for the full rationale.
