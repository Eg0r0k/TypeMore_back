# Vendored game core

`core.bundle.js` is a **checked-in build artifact**: the frontend's
`@typemore/core` package (reducer, mulberry32, FNV-1a, metrics, score)
compiled to a single self-contained IIFE that assigns its exports to the
global `TypeMoreCore`.

It is vendored, not reimplemented, because anti-cheat and scoring stand on
server-side replay and a second Go implementation would drift (ARCHITECTURE.md
§4.2, BACKEND.md §1). Whatever this bundle computes *is* the answer — the server
never "checks" it against Go arithmetic.

## Provenance

The artifact is built by the package itself — `pnpm --filter @typemore/core
build` in the frontend checkout — from the package's ONE entry
(`packages/core/src/index.ts`), the same file its ESM library is built from.
This repo runs no bundler: there is no second entry and no second flag set to
rot. The package pins esbuild exactly and its build is deterministic
(byte-identical across runs on the same tree; the package's own
`bundle-determinism` and `export-parity` tests hold both properties).

The bundle's last line is a machine-readable trailer:

```
//# typemore-core-build {"version":...,"eventLogVersion":...,"telemetryLogVersion":...,"gitSha":...,"gitDirty":...}
```

At startup the server logs the bundle's exported `CORE_PACKAGE_VERSION` and
`EVENT_LOG_VERSION*` constants together with `BundleSHA` and the trailer's
git provenance, so "which core judged this run" is answerable from any boot
log.

## Rebuilding / re-vendoring

```sh
# in the frontend checkout (must be committed — see below)
pnpm --filter @typemore/core build

# in this repo
make core-bundle                     # frontend checked out beside this repo
make core-bundle FRONTEND=/path/to/TypeMore_front
```

`make core-bundle` copies `packages/core/dist/core.bundle.js` and **refuses**
to vendor when the dist is missing, when its trailer says `gitDirty: true`, or
when its `gitSha` is not the frontend checkout's current HEAD. Every verdict
records `bundle_sha`; the refusal rules keep that hash traceable to exactly one
reproducible frontend commit.

## After re-vendoring

Run `go test ./internal/replay/` (the make target does).
`TestPublishedHashesAreImmutable` is the tripwire: a bundle change that moves a
published `dict_hash` would invalidate every stored run, and must be treated as
a protocol break, not a refresh.

The bundle's SHA-256 lands on every decision as `bundle_sha`, so a rebuild
leaves already-judged runs behind the current code. `make revalidate` walks
them forward — see docs/REPLAY.md.

## Staying fresh

Nothing in this repo changes when the frontend's core does, which is what makes
a checked-in artifact rot. `make bundle-gate` (also `TestVendoredBundleIsFresh`,
part of `make test`) diffs the vendored file against the frontend's built dist,
**after stripping the trailer from both sides** — the trailer carries the
frontend git sha, and an unrelated frontend commit over identical core source
must not read as a stale bundle. Compiled-code drift still fails byte-for-byte,
with the fixing command in the message.

The golden vectors already catch a bundle whose *arithmetic* moved. The gate
catches the quiet case they cannot: a vendored file that is simply not what the
current source compiles to. That is not hypothetical — the bundle once spent a
phase missing `normalize.ts` entirely (B11), and stayed correct only because
nothing in the core imported it. The frontend's `export-parity` test now closes
that class at the source; this gate closes it at the artifact.

Where the frontend (or its built dist) is not present, the test skips with the
reason on stderr. `make bundle-gate` sets `TYPEMORE_BUNDLE_GATE=required`,
which turns any such skip into a failure — use it in a CI job that HAS the
frontend, so a missing build cannot read as a fresh bundle.
