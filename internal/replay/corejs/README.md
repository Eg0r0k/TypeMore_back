# Vendored game core

`core.bundle.js` is a **checked-in build artifact**: the frontend's
`src/shared/core` (reducer, mulberry32, FNV-1a, metrics, score) compiled to a
single self-contained IIFE that assigns its exports to the global
`TypeMoreCore`.

It is vendored, not reimplemented, because anti-cheat and scoring stand on
server-side replay and a second Go implementation would drift (ARCHITECTURE.md
§4.2, BACKEND.md §1). Whatever this bundle computes *is* the answer — the server
never "checks" it against Go arithmetic.

## Rebuilding

```sh
make core-bundle                     # frontend checked out beside this repo
make core-bundle FRONTEND=/path/to/TypeMore_front
```

The target runs esbuild from the frontend's own `node_modules` (so the bundler
version is pinned by the frontend lockfile) with the arguments in
[`esbuild.args`](./esbuild.args) — one file, read by both the rebuild and the
freshness gate below, so the two cannot disagree about what a fresh bundle is:

```
--bundle --format=iife --global-name=TypeMoreCore --target=es2017 --platform=browser
```

Both run esbuild **with the frontend checkout as the working directory**. That is
not incidental: esbuild writes each module's path into the output as a comment,
so building the same source from a different directory produces different bytes.

- `iife` + `global-name`: goja has no module loader; the bundle must publish
  itself on the global object.
- `es2017`: goja implements ES5.1 plus most of ES2015+, and esbuild lowers
  anything newer. Raise this only after the package tests still pass.
- `platform=browser`: resolves `neverthrow` (the core's only runtime dependency)
  and inlines it. Nothing DOM-specific is pulled in — the core is framework-free
  by construction, enforced by the frontend's `core.purity.test.ts`.

## After rebuilding

Run `go test ./internal/replay/`. `TestPublishedHashesAreImmutable` is the
tripwire: a bundle change that moves a published `dict_hash` would invalidate
every stored run, and must be treated as a protocol break, not a refresh.

The bundle's SHA-256 lands on every decision as `bundle_sha`, so a rebuild leaves
already-judged runs behind the current code. `make revalidate` walks them
forward — see docs/REPLAY.md.

## Staying fresh

Nothing in this repo changes when the frontend's core does, which is what makes a
checked-in artifact rot. `make bundle-gate` (also `TestVendoredBundleIsFresh`,
which runs as part of `make test`) rebuilds into a **temporary** file and diffs
it against the vendored one; a difference fails with the command that fixes it.

The golden vectors already catch a bundle whose *arithmetic* moved. This catches
the quiet case they cannot: a vendored file that is simply not what the current
source compiles to. That is not hypothetical — the bundle spent a phase missing
`normalize.ts` and the `wordHistory` exports entirely, and stayed correct only
because nothing in the core imported them.

Where the frontend is not checked out, the test skips with the reason on stderr.
`make bundle-gate` sets `TYPEMORE_BUNDLE_GATE=required`, which turns any such
skip into a failure — use it in a CI job that HAS the frontend, so a missing
toolchain cannot read as a fresh bundle.
