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
version is pinned by the frontend lockfile) with:

```
--bundle --format=iife --global-name=TypeMoreCore --target=es2017 --platform=browser
```

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
