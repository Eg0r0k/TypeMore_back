package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A runtime switch would not have been enough. An env-var-driven policy leaves
// its weights, its threshold and its rule names in the binary whether the switch
// is on or off, and `strings` finds them in ten seconds — so the switch is a
// build tag, and this is the test that says so out loud.
//
// It compiles the server twice and greps the bytes. Slow, and worth it: every
// other test in this repo asserts about behaviour, and behaviour cannot tell you
// what a stripped binary is still carrying.

// policySecrets are strings that must not appear in a binary built WITHOUT the
// tag. Rule ids first — they are the giveaway, because they NAME the shapes the
// server looks for — then the identifiers that would lead a reader straight to
// the table.
//
// What is deliberately NOT on this list: the flag codes themselves
// (min-interval, zero-variance, and the rest). They are in the binary, inside
// the vendored core bundle, and they belong there — the detectors run in the
// browser and hiding their names would be pretending. What is hidden is what the
// server DOES about what they report.
//
// "reviewPolicy" is not on the list either, and the first run of this test is
// why: it matched, on the /healthz field that reports whether a policy is
// running. That field is the opposite of a leak.
var policySecrets = []string{
	"bot_cadence",
	"sustained_superhuman",
	"defaultFlagWeights",
	"defaultReviewThreshold",
	"defaultSustainedBurstSec",
	"applyWeightOverrides",
}

func TestBinaryWithoutTheTagCarriesNoPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the server binary twice")
	}
	plain := buildServer(t, nil)

	var found []string
	for _, secret := range policySecrets {
		if bytes.Contains(plain, []byte(secret)) {
			found = append(found, secret)
		}
	}
	require.Empty(t, found,
		"the untagged server binary carries %v — the review policy is not actually absent, "+
			"only unreachable, and `strings` reads it in ten seconds", found)
	requireNoTestDouble(t, plain)
}

// The deterministic fake exists so the open repo's tests do not depend on a
// policy that may not be built — which makes it a judge that ships with the
// source and must never ship in a binary. A server accidentally wired to it
// would judge every run against invented weights and look entirely healthy doing
// it.
func requireNoTestDouble(t *testing.T, binary []byte) {
	t.Helper()
	for _, marker := range []string{"fake_shape", "fakeWeights", "policytest"} {
		require.False(t, bytes.Contains(binary, []byte(marker)),
			"the server binary contains %q: the test double is linked into it", marker)
	}
}

// The control. Without it the test above would pass just as happily against a
// binary that had renamed its rules, or against a grep that never matched
// anything — so the tagged build must contain what the untagged one must not.
func TestTheTaggedBinaryDoesCarryThePolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the server binary twice")
	}
	tagged := buildServer(t, []string{"-tags", "anticheat"})

	for _, secret := range []string{"bot_cadence", "sustained_superhuman"} {
		require.True(t, bytes.Contains(tagged, []byte(secret)),
			"the tagged binary does NOT contain %q: either the tag no longer builds the "+
				"policy in, or this test is greping for something that has been renamed — "+
				"and in either case the untagged assertion above proves nothing", secret)
	}
	requireNoTestDouble(t, tagged)
}

// buildServer compiles this command with the given extra flags and returns the
// binary's bytes.
func buildServer(t *testing.T, extra []string) []byte {
	t.Helper()

	out := filepath.Join(t.TempDir(), "server")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	// -trimpath so the build directory's own path cannot make a match, and
	// nothing else: the production build adds -ldflags "-s -w", which would only
	// strip MORE, so testing the un-stripped binary is the stricter check.
	args := append([]string{"build", "-trimpath", "-o", out}, extra...)
	args = append(args, ".")

	cmd := exec.Command(goTool(t), args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "go %s: %s", strings.Join(args, " "), stderr.String())

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	return raw
}

// goTool locates the go binary this test builds with.
//
// PATH only. `runtime.GOROOT()` was the obvious answer and is deprecated as of
// Go 1.24 for a reason that applies here: it reports the GOROOT the TEST BINARY
// was built with, which says nothing about the machine now running it. A test
// invoked by `go test` was started by a go on PATH, so PATH is both the honest
// question and the one with an answer.
func goTool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	require.NoError(t, err, "no go toolchain to build with")
	return path
}
