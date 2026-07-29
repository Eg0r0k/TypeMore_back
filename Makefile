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
#   make build       build the binary into ./bin (NO review policy — docs/SELF_HOST.md)
#   make build-anticheat  the same binary WITH the review policy compiled in
#   make test-anticheat   run the suite with the review policy built in
#   make core-bundle re-vendor the TS core bundle from the frontend checkout
#   make bundle-gate fail if the vendored bundle is stale against that checkout
#   make contract    regenerate the cross-repo match-timing contract artifact
#   make vectors     regenerate the replay golden vectors (read the diff!)
#   make tools       install golangci-lint into your Go bin
#   make calibrate   dry-run the replay review policy over stored runs
#   make revalidate  re-judge runs behind the current policy OR core bundle
#   make rebuild-leaderboards  recompute the boards from accepted runs
#   make leaderboards          print the board index (bucket=KEY for one board)
#   make import-quotes         publish the vendored quote corpora into Postgres
#   make ban / unban / bans / ban-show   account restrictions (docs/MODERATION.md)
#   make load        run the performance & load suite (docs/PERFORMANCE.md)
#   make bench       run the Go benchmarks

BINARY := typemore-server
PKG    := github.com/typemore/typemore-server
CMD    := ./cmd/server

# Database URL for the goose CLI targets (migrate-create). The migrate-up/down
# targets go through cmd/migrate, which reads TYPEMORE_DATABASE_URL itself.
DATABASE_URL ?= postgres://typemore:typemore@localhost:5432/typemore?sslmode=disable

# Frontend checkout used by `make core-bundle`. Defaults to a sibling directory.
# TestVendoredBundleIsFresh resolves the same path (TYPEMORE_FRONTEND overrides
# it there), so the gate checks the checkout the rebuild would read.
FRONTEND ?= ../TypeMore_front

# The esbuild argument list for `make core-bundle`, read from the file the
# freshness gate reads. One argument per line, #-comments and blanks stripped.
ESBUILD_ARGS := $(shell sed -e 's/#.*//' internal/replay/corejs/esbuild.args)

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

.PHONY: run test test-race test-anticheat lint build build-anticheat tidy sqlc core-bundle bundle-gate contract vectors calibrate revalidate rebuild-leaderboards leaderboards import-quotes ban unban bans ban-show load bench load-plans migrate-up migrate-down migrate-status migrate-create tools help

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
# WITHOUT the review policy. The binary boots, replays every run, recomputes
# every number and refuses everything the hard rules refuse — it just judges
# nothing. It says so at startup and on /healthz. See docs/SELF_HOST.md.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(EXE) $(CMD)

## build-anticheat: compile the binary WITH the review policy built in
# The weights, the review threshold and the combination rules are compiled in
# only here — a plain `make build` does not contain them, which is checked by
# TestBinaryWithoutTheTagCarriesNoPolicy rather than assumed.
build-anticheat:
	go build -trimpath -tags anticheat -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(EXE) $(CMD)

## test-anticheat: run the suite with the review policy built in
# `make test` covers the open build; this covers the closed one. Both matter:
# the policy's own tests only compile under the tag, and the proof that hard
# verdicts do NOT depend on a policy only means anything in the build without one.
test-anticheat:
	go test -tags anticheat ./...

## core-bundle: re-vendor internal/replay/corejs/core.bundle.js from $(FRONTEND)
# The bundler comes from the frontend's own node_modules, so its version is
# pinned by that lockfile rather than by whatever is on this machine. The flags
# live in corejs/esbuild.args, which the freshness gate reads too — see
# internal/replay/corejs/README.md before changing them.
core-bundle:
	cd $(FRONTEND) && pnpm exec esbuild $(ESBUILD_ARGS) \
		--outfile="$(CURDIR)/internal/replay/corejs/core.bundle.js"
	go test ./internal/replay/

## bundle-gate: fail if the vendored core bundle is not what $(FRONTEND) compiles to
# Rebuilds into a temp file and diffs — never writes the vendored artifact. The
# strict flag turns "the toolchain is missing" into a failure, so a CI job that
# has the frontend cannot pass this by accident; a job WITHOUT the frontend runs
# the test through `make test` instead, where it skips with a stated reason.
bundle-gate:
	TYPEMORE_BUNDLE_GATE=required go test ./internal/replay/ -run TestVendoredBundleIsFresh -count=1 -v

## contract: regenerate contract/match-timings.json from the Go constants
# The frontend READS this file to check its own AFK thresholds sit inside the
# server's. Regenerating it is therefore a cross-repo change: the test that
# asserts the values (internal/ws/contract_test.go) is what should have stopped
# you first, and it is the one to argue with.
contract:
	TYPEMORE_UPDATE_CONTRACT=1 go test ./internal/ws/ -run TestMatchTimingContractSnapshotIsCurrent -count=1
	go test ./internal/ws/ -run TestMatchTiming -count=1

## vectors: regenerate the replay golden vectors (ONLY after a deliberate bundle change)
# The vectors pin the scoring contract: `make core-bundle` fails the test suite
# if they move. Regenerate only once you have decided the change is intended,
# and READ THE DIFF — see internal/replay/testdata/README.md and docs/REPLAY.md.
vectors:
	node internal/replay/testdata/generate.mjs
	go test ./internal/replay/

## calibrate: dry-run the review policy over stored runs — writes NOTHING
# Prints per-flag firing rates, a suspicion histogram, the worst offenders and
# the status changes the current policy would make. Run this BEFORE changing a
# weight or the threshold. Reads TYPEMORE_DATABASE_URL (and any
# TYPEMORE_REPLAY_* overrides) exactly as the server does.
calibrate:
	go run ./cmd/replayctl calibrate

## revalidate: re-judge runs behind the current policy OR the current bundle
# Keys on BOTH: policy_version < CurrentPolicyVersion (the rules moved) and
# bundle_sha <> the vendored bundle's digest (the code that produced the numbers
# moved). Bounded and idempotent: applying a decision writes both columns, so a
# second pass finds nothing. Run it after bumping CurrentPolicyVersion or after
# `make core-bundle`.
revalidate:
	go run ./cmd/replayctl revalidate

## rebuild-leaderboards: recompute the whole board from accepted runs
# The projection is maintained incrementally inside the replay worker's
# transaction, so this should report "unchanged" — being able to run it, and it
# changing nothing, is what proves the board is derived from Postgres alone.
# Run it after flipping TYPEMORE_LEADERBOARD_REQUIRE_VERIFIED_EMAIL or changing
# the eligible-runs view. See docs/LEADERBOARDS.md.
rebuild-leaderboards:
	go run ./cmd/leaderboardctl rebuild

## leaderboards: print the board index (or one bucket with bucket=KEY)
leaderboards:
	go run ./cmd/leaderboardctl show $(if $(bucket),-bucket $(bucket),)

## ban: put an account under restriction (see docs/MODERATION.md)
##      make ban user=ada reason="cheating" until=72h
ban:
	go run ./cmd/banctl ban $(user) --reason "$(reason)" $(if $(until),--until $(until),)

## unban: revoke an account's active ban
##        make unban user=ada
unban:
	go run ./cmd/banctl unban $(user)

## bans: list bans (all=1 to include revoked and expired)
bans:
	go run ./cmd/banctl list $(if $(all),--all,--active) $(if $(user),,)

## ban-show: every ban an account has ever had
##           make ban-show user=ada
ban-show:
	go run ./cmd/banctl show $(user)

## import-quotes: publish the vendored quote corpora into Postgres (lang=ID for one)
# Driven by internal/quote/quotes/MANIFEST.json, never by a directory listing:
# a corpus with no manifest row is not imported. Idempotent — running it twice
# reports every quote unchanged, which is the proof that a re-import cannot
# quietly rewrite published text. When upstream DOES change a text, the new
# bytes land as a new row and the old one is retired but stays resolvable by
# id, because runs played on it must replay forever. See docs/QUOTES.md.
import-quotes:
	go run ./cmd/quotectl import $(if $(lang),-lang $(lang),)

## load: run the performance & load suite (heavy; needs Docker)
# Everything behind the `load` build tag: realistic-volume seeds, latency and
# memory budgets, and the SQL plan assertions. Minutes, not seconds — the normal
# `make test` deliberately skips all of it. Budgets and verdicts are in
# docs/PERFORMANCE.md; a BUDGET MISSED line in the output is a failing test.
load:
	go test -tags=load -timeout=45m -v -run '^TestLoad' ./... 2>&1

## bench: run the Go benchmarks (hot paths, allocations)
bench:
	go test -tags=load -timeout=45m -run '^$$' -bench=. -benchmem ./...

## load-plans: only the query-plan assertions (fast; still needs Docker)
# A migration that drops an index makes these red immediately, without waiting
# for a latency budget to notice.
load-plans:
	go test -tags=load -timeout=15m -v -run '^TestLoadPlan' ./...


## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

## sqlc: regenerate type-safe DB code from internal/auth/queries.sql
sqlc:
	sqlc generate

## migrate-up: apply all pending migrations (uses TYPEMORE_DATABASE_URL)
migrate-up:
	go run ./cmd/migrate up

## migrate-down: roll back the most recent migration
migrate-down:
	go run ./cmd/migrate down

## migrate-status: show migration status
migrate-status:
	go run ./cmd/migrate status

## migrate-create: scaffold a new migration file (name=... ), needs goose CLI
migrate-create:
	goose -dir db/migrations create $(name) sql

## tools: install pinned dev tooling (golangci-lint, sqlc, goose)
tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install github.com/pressly/goose/v3/cmd/goose@latest

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
