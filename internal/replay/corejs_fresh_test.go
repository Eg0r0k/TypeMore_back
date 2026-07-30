package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rebuildHint is what a failure has to leave behind: the commands that fix it.
const rebuildHint = "run `pnpm --filter @typemore/core build` in the frontend, then `make core-bundle` here " +
	"(and READ THE DIFF — see internal/replay/corejs/README.md)"

// buildInfoTrailerPrefix marks the machine-readable last line the package build
// appends to dist/core.bundle.js. The same prefix is parsed by `make
// core-bundle` (provenance refusal) and by bundleBuildInfo in core.go.
const buildInfoTrailerPrefix = "//# typemore-core-build "

// TestVendoredBundleIsFresh diffs the vendored artifact against the frontend's
// built dist/core.bundle.js — the @typemore/core package's own build output,
// produced from the package's single entry by its deterministic build.
//
// The bundle is checked in because replay must not depend on a node toolchain
// at runtime, and that is exactly what makes it rot: nothing in this repo
// changes when the frontend's core does. The golden vectors catch a bundle
// whose ARITHMETIC moved, which is the loud case. This catches the quiet one —
// a vendored file that is simply not what the current source compiles to,
// which is how the bundle once spent a phase missing an entire module (B11).
//
// The build-info trailer is stripped from BOTH sides before comparing: it
// carries the frontend's git sha, so an unrelated frontend commit (new sha,
// byte-identical compiled core) must not read as a stale bundle. Provenance of
// the trailer itself (dirty tree, stale sha) is `make core-bundle`'s job at
// vendoring time, not this gate's.
//
// It is a gate, not a rebuild: the vendored file is never written here. Where
// the frontend (or its built dist) is absent it skips with the reason on
// stderr; `make bundle-gate` sets TYPEMORE_BUNDLE_GATE=required so an
// environment that EXPECTS to check freshness fails instead of skipping.
func TestVendoredBundleIsFresh(t *testing.T) {
	dist := distBundlePath(t)

	built, err := os.ReadFile(dist)
	require.NoError(t, err)
	require.NotEmpty(t, built, "frontend dist bundle is empty")

	fresh := stripBuildInfoTrailer(string(built))
	vendored := stripBuildInfoTrailer(coreBundle)

	if fresh == vendored {
		return
	}
	t.Errorf("the vendored core bundle is not what %s holds — %s\n\n%s",
		dist, rebuildHint, firstDifference(vendored, fresh))
}

// distBundlePath resolves the frontend's built bundle, matching the make
// target's FRONTEND default (a sibling of this repo; TYPEMORE_FRONTEND
// overrides), and skips when the checkout or its dist is not there — the CI
// case where only the Go module is checked out, or a checkout that simply has
// not run the package build.
func distBundlePath(t *testing.T) string {
	t.Helper()

	dir := os.Getenv("TYPEMORE_FRONTEND")
	if dir == "" {
		// The test runs in internal/replay; the repo root is two levels up, and
		// the make target defaults to ../TypeMore_front from there.
		root, err := filepath.Abs(filepath.Join("..", ".."))
		require.NoError(t, err)
		dir = filepath.Join(filepath.Dir(root), "TypeMore_front")
	}
	abs, err := filepath.Abs(dir)
	require.NoError(t, err)

	dist := filepath.Join(abs, "packages", "core", "dist", "core.bundle.js")
	if _, err := os.Stat(dist); err != nil {
		skipf(t, "no built frontend bundle at %s (set TYPEMORE_FRONTEND, or %s)", dist, rebuildHint)
	}
	return dist
}

// stripBuildInfoTrailer removes the trailing build-info line (and any final
// newline) so the comparison is about the compiled code, not about which
// commit compiled it.
func stripBuildInfoTrailer(bundle string) string {
	trimmed := strings.TrimRight(bundle, "\n")
	if i := strings.LastIndexByte(trimmed, '\n'); i >= 0 && strings.HasPrefix(trimmed[i+1:], buildInfoTrailerPrefix) {
		return trimmed[:i]
	}
	return trimmed
}

// gateRequiredEnv makes a skip a failure. A gate that cannot run is
// indistinguishable from a gate that passed once `go test` swallows the skip in
// a non-verbose run, so the environment that EXPECTS to check freshness says so
// and gets an error instead of silence. `make bundle-gate` sets it; a CI job
// that only checks out the Go module deliberately does not.
const gateRequiredEnv = "TYPEMORE_BUNDLE_GATE"

// skipf reports that the gate could not run. Under TYPEMORE_BUNDLE_GATE=required
// that is a failure; otherwise it is a skip whose reason is also written to
// stderr, so it is visible under -v, in `go test -json`, and in the log of
// whatever ran it.
func skipf(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "SKIP %s: bundle freshness gate did not run: %s\n", t.Name(), msg)
	if os.Getenv(gateRequiredEnv) == "required" {
		t.Fatalf("%s=required but the gate cannot run: %s", gateRequiredEnv, msg)
	}
	t.Skip(msg)
}

// firstDifference locates where two bundles part company and quotes both sides,
// so a failure says what moved instead of only that something did.
func firstDifference(vendored, rebuilt string) string {
	have, want := strings.Split(vendored, "\n"), strings.Split(rebuilt, "\n")
	for i := range max(len(have), len(want)) {
		h, w := lineAt(have, i), lineAt(want, i)
		if h == w {
			continue
		}
		return fmt.Sprintf("first difference at line %d:\n  vendored: %s\n  built:    %s\n"+
			"(vendored %d lines / %d bytes, built %d lines / %d bytes)",
			i+1, quote(h), quote(w), len(have), len(vendored), len(want), len(rebuilt))
	}
	return "the files differ only in trailing bytes"
}

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<end of file>"
}

// quote trims a bundle line to something readable: minified output has lines
// long enough to bury the rest of the failure.
func quote(s string) string {
	const limit = 160
	if len(s) > limit {
		return fmt.Sprintf("%q… (%d bytes)", s[:limit], len(s))
	}
	return fmt.Sprintf("%q", s)
}
