// Package testgate separates a test that could not run here from a test that
// passed.
//
// A skipped test and a passing test both print ok, and a gate that inherits
// the package result prints ok too. That is fine for an ordinary run on a
// developer machine without a browser, and it is exactly what a release run
// may not do: the one thing a release gate is for is refusing to report a
// claim it did not check. scripts/check.sh already makes that distinction for
// the official client, the portable clients, and the Python sidecars; this
// package is the same rule for every Go test that needs a tool the host may
// lack, so the distinction is made once rather than reinvented, or forgotten,
// per package.
package testgate

import (
	"os"
	"testing"
)

// Variable is the environment variable that turns a run into a release gate.
// Any non-empty value counts, which is the rule scripts/check.sh applies.
const Variable = "OPENREALTIME_RELEASE_GATE"

// Release reports whether this run is a release gate.
func Release() bool { return os.Getenv(Variable) != "" }

// Missing marks a test that needs a tool this host does not have: node,
// chromium, ffmpeg, python3, git. Outside release mode it skips and names the
// tool; in release mode it fails, because the release gate installs those
// tools and a missing one is a broken gate, not an optional claim.
func Missing(t testing.TB, requirement string) {
	t.Helper()
	message := requirement + " is not installed"
	if Release() {
		t.Fatal(message + "; a release gate cannot skip what it was asked to verify")
	}
	t.Skip(message)
}

// Unprepared marks a test that needs a provisioned input no gate installs: a
// pinned dataset, a local model runtime. It always skips, because the claim
// belongs to the provisioned benchmark gate that supplies the input, but it
// says so in the same words every time so a recorded skip reads as an
// unverified claim rather than as a passing package.
func Unprepared(t testing.TB, resource string, err error) {
	t.Helper()
	if err != nil {
		t.Skipf("NOT VERIFIED: %s is unavailable (%v); a provisioned gate owns this claim", resource, err)
	}
	t.Skipf("NOT VERIFIED: %s is unavailable; a provisioned gate owns this claim", resource)
}
