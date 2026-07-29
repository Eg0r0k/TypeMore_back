package replay

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// esbuildArgs is the argument list `make core-bundle` builds the vendored bundle
// with — the same file, so the gate and the rebuild cannot disagree about what
// "fresh" means.
//
//go:embed corejs/esbuild.args
var esbuildArgs string

// rebuildHint is what a failure has to leave behind: the command that fixes it.
const rebuildHint = "run `make core-bundle` (and READ THE DIFF — see internal/replay/corejs/README.md)"

// TestVendoredBundleIsFresh rebuilds the core bundle from the frontend checkout
// into a TEMPORARY file and diffs it against the vendored artifact.
//
// The bundle is checked in because replay must not depend on a node toolchain at
// runtime, and that is exactly what makes it rot: nothing in this repo changes
// when the frontend's core does. The golden vectors catch a bundle whose
// ARITHMETIC moved, which is the loud case. This catches the quiet one — a
// vendored file that is simply not what the current source compiles to, which is
// how the bundle spent a phase missing an entire module by accident rather than
// by decision.
//
// It is a gate, not a rebuild: the vendored file is never written here. When it
// fails, the fix is a human running `make core-bundle` and reading the diff.
//
// In CI it runs either way. Where the frontend is checked out beside this repo,
// `make bundle-gate` sets TYPEMORE_BUNDLE_GATE=required so a missing toolchain
// fails instead of skipping. Where only the Go module is checked out, it skips
// with the reason on stderr — an explicit "did not run", never a quiet pass.
func TestVendoredBundleIsFresh(t *testing.T) {
	frontend := frontendCheckout(t)
	pnpm := pnpmBinary(t)

	fresh := filepath.Join(t.TempDir(), "core.fresh.js")
	args := append(append([]string{"exec", "esbuild"}, parseEsbuildArgs(esbuildArgs)...), "--outfile="+fresh)

	// Run from the frontend root, like the make target: esbuild writes each
	// module's path into the bundle as a comment, so the working directory is
	// part of the output bytes.
	cmd := exec.Command(pnpm, args...)
	cmd.Dir = frontend
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out

	start := time.Now()
	if err := cmd.Run(); err != nil {
		// A toolchain that cannot run is not a stale bundle. Skipping loudly is
		// the honest outcome; passing quietly is the one thing this must not do.
		skipf(t, "esbuild failed in %s: %v\n%s", frontend, err, out.String())
	}
	t.Logf("rebuilt in %s: %s", time.Since(start).Round(time.Millisecond), strings.TrimSpace(out.String()))

	rebuilt, err := os.ReadFile(fresh)
	require.NoError(t, err)
	require.NotEmpty(t, rebuilt, "esbuild produced an empty bundle")

	if string(rebuilt) == coreBundle {
		return
	}
	t.Errorf("the vendored core bundle is not what %s compiles to — %s\n\n%s",
		filepath.Join(frontend, "src", "shared", "core"), rebuildHint,
		firstDifference(coreBundle, string(rebuilt)))
}

// frontendCheckout resolves the frontend the bundle is vendored from, matching
// the make target's FRONTEND default (a sibling of this repo), and skips when it
// is not there — the CI case, where only the Go module is checked out.
func frontendCheckout(t *testing.T) string {
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

	// The entry point, not just the directory: a checkout that exists but has no
	// core is a broken setup, and it must not read as "fresh".
	entry := filepath.Join(abs, filepath.FromSlash(parseEsbuildArgs(esbuildArgs)[0]))
	if _, err := os.Stat(entry); err != nil {
		skipf(t, "no frontend core source at %s (set TYPEMORE_FRONTEND, or see %s)", entry, rebuildHint)
	}
	return abs
}

// pnpmBinary finds the package manager the make target shells out to. esbuild is
// deliberately taken from the frontend's node_modules rather than from PATH: the
// bundler version is pinned by that lockfile, and a different one would produce
// different bytes for identical source.
func pnpmBinary(t *testing.T) string {
	t.Helper()
	name := "pnpm"
	if runtime.GOOS == "windows" {
		name = "pnpm.cmd"
	}
	path, err := exec.LookPath(name)
	if errors.Is(err, exec.ErrNotFound) && runtime.GOOS == "windows" {
		path, err = exec.LookPath("pnpm")
	}
	if err != nil {
		skipf(t, "pnpm is not on PATH: cannot rebuild the bundle to compare against")
	}
	return path
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

// parseEsbuildArgs turns the shared argument file into a list: one argument per
// line, #-comments and blank lines dropped. Same stripping the Makefile does
// with sed.
func parseEsbuildArgs(raw string) []string {
	var out []string
	for line := range strings.SplitSeq(raw, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
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
		return fmt.Sprintf("first difference at line %d:\n  vendored: %s\n  rebuilt:  %s\n"+
			"(vendored %d lines / %d bytes, rebuilt %d lines / %d bytes)",
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
