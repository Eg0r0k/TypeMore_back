# TypeMore Server

The TypeMore game server: a single static Go binary. It provides the realtime
plumbing (WebSocket `hello` + NTP clock exchange) and **authentication +
persistence**: email/password and GitHub/Google OAuth over PostgreSQL, with
opaque sessions, email verification, and password reset. Rooms/relay/match,
scoring, and Redis arrive in later stages (see `ARCHITECTURE.md` / `BACKEND.md`).

Contracts: the realtime wire format is in [`docs/PROTOCOL.md`](docs/PROTOCOL.md);
the auth/HTTP surface (endpoints, schema, env, security) is in
[`docs/AUTH.md`](docs/AUTH.md); run ingestion is in
[`docs/RUNS.md`](docs/RUNS.md); the replay worker that judges those runs is in
[`docs/REPLAY.md`](docs/REPLAY.md); the boards those verdicts feed are in
[`docs/LEADERBOARDS.md`](docs/LEADERBOARDS.md); the dictionary catalogue and its
caching contract are in [`docs/DICTIONARIES.md`](docs/DICTIONARIES.md).

## What's here

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness + build info (JSON) |
| `GET /readyz` | Readiness — database ping |
| `GET /ws` | WebSocket: `hello` handshake + `ntp_ping`/`ntp_pong` |
| `/api/v1/auth/*`, `GET /api/v1/me` | Auth + sessions — see [`docs/AUTH.md`](docs/AUTH.md) |
| `/api/v1/runs*` | Run ingestion, own-runs feed, public replay — see [`docs/RUNS.md`](docs/RUNS.md) |
| `/api/v1/leaderboards*` | Public bucketed score boards — see [`docs/LEADERBOARDS.md`](docs/LEADERBOARDS.md) |
| `GET /api/v1/dictionaries` | Public dictionary catalogue — see [`docs/DICTIONARIES.md`](docs/DICTIONARIES.md) |
| `GET /static/dictionaries/{dictHash}.json` | Dictionary body, content-addressed and `immutable` |

## Prerequisites

- **Go 1.26+** — https://go.dev/dl/.
- **Docker Desktop** — required for the full stack (Postgres + Mailpit) and for
  the integration tests (they spin up Postgres via testcontainers).
- Optional: **make** — on Windows, install via `choco install make` or use the
  raw `go` commands shown below. Run `make` targets from **git-bash** (bundled
  with Git for Windows), not `cmd.exe`.

## Quickstart (Windows, git-bash)

```bash
# 1. Start Postgres + Mailpit (dev SMTP sink) and apply migrations
docker compose up -d postgres mailpit
make migrate-up                       # or: go run ./cmd/migrate up

# 2. Run the server (listens on :8080)
go run ./cmd/server                   # or: make run
```

In a second git-bash window:

```bash
# 3. Liveness and readiness (readiness pings the DB)
curl http://localhost:8080/healthz
curl http://localhost:8080/readyz
```

Stop the server with `Ctrl+C` — it shuts down gracefully.

### Try the auth flow

With the stack up: register → verify → login. In dev without SMTP the
verification link is printed to the server logs; with the compose stack it lands
in Mailpit's inbox UI at http://localhost:8025.

```bash
O=http://localhost:5173   # must match TYPEMORE_FRONTEND_ORIGIN (CSRF)
curl -sX POST localhost:8080/api/v1/auth/register -H "Origin: $O" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@example.com","password":"password123","displayName":"Demo"}'
# grab the token from http://localhost:8025 , then:
curl -sX POST localhost:8080/api/v1/auth/verify -H "Origin: $O" \
  -H 'Content-Type: application/json' -d '{"token":"<TOKEN>"}'
curl -sX POST localhost:8080/api/v1/auth/login -c cj.txt -H "Origin: $O" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@example.com","password":"password123"}'
curl -s localhost:8080/api/v1/me -b cj.txt
```

### Run the tests

```bash
go test ./...      # or: make test
```

The suite covers a real end-to-end WebSocket test (`hello` + 5×NTP, offset math,
error paths) and full auth flows against a throwaway Postgres (testcontainers):
register → verify → login → `/me` → logout, password reset with session
revocation, OAuth create/link/collision against a fake provider,
anti-enumeration, and rate limiting. **Docker must be running.**

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

Full stack — server + Postgres + Mailpit (dev SMTP sink):

```bash
docker compose up -d postgres mailpit   # datastores
docker compose run --rm migrate up      # apply migrations
docker compose up --build app           # the server
```

- App: http://localhost:8080 (`/healthz`, `/readyz`)
- Mailpit inbox UI: http://localhost:8025 (sent verification/reset emails)

> On Windows, if host port 8080/5432 is already used by another service, remap
> the `ports:` in `docker-compose.yml`; the app↔db link uses the compose network
> and is unaffected.

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
| *DB / session / OAuth / SMTP / rate-limit vars* | see [`.env.example`](.env.example) | Documented in [`docs/AUTH.md`](docs/AUTH.md) |

## Project layout

```
cmd/
  server/              # composition root (main): wiring + lifecycle
  migrate/             # goose migration runner (embedded SQL)
  replayctl/           # operator tool: calibrate / revalidate the review policy
  leaderboardctl/      # operator tool: rebuild / show the boards
internal/
  platform/            # config, logging, HTTP lifecycle, health/build (no domain deps)
    db/                # pgx pool          migrate/  # goose runner
    mail/              # SMTP / log email senders (neutral Message type)
  protocol/            # realtime wire types (mirrors docs/PROTOCOL.md)
  ws/                  # WebSocket transport: hello/NTP
  auth/                # auth domain: service, handlers, sessions, oauth, argon2id
    authdb/            # sqlc-generated queries (committed)
    pgstore/           # Postgres impl of the auth Store/SessionStore interfaces
  runs/                # run ingestion, own-runs feed, public replay endpoint
    runsdb/            # sqlc-generated queries (committed)
    pgstore/           # Postgres impl of the runs Store interface
  replay/              # vendored TS core (goja): dictionary registry + replay worker
    corejs/            # core.bundle.js — checked-in build artifact (see its README)
    dicts/             # the ONLY copy of the word lists; embedded, served immutable
    replaydb/          # sqlc-generated queue queries (committed)
    pgstore/           # FOR UPDATE SKIP LOCKED queue adapter
    testdata/          # golden vectors + the generator that produces them
  leaderboard/         # bucket keys, board read model, projection from accepted runs
    leaderboarddb/     # sqlc-generated queries (committed)
    pgstore/           # projector (runs in the worker's tx) + reads + rebuild
db/migrations/         # goose SQL migrations (embedded)
docs/PROTOCOL.md       # realtime contract      docs/AUTH.md          # auth/HTTP contract
docs/RUNS.md           # run ingestion          docs/DICTIONARIES.md  # dictionary service
docs/REPLAY.md         # replay worker          docs/LEADERBOARDS.md  # score boards
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
