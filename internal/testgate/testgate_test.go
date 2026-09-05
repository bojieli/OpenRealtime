package testgate_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/testgate"
)

// The helper is re-run as a subprocess so that the assertion is about what
// `go test` actually prints - a SKIP outside release mode and a FAIL inside it
// - rather than about an internal flag. That is the difference the gate reads.
func TestMissingSkipsOutsideAReleaseGateAndFailsInsideOne(t *testing.T) {
	if os.Getenv("OPENREALTIME_TESTGATE_HELPER") == "1" {
		testgate.Missing(t, "a-tool-nobody-has")
		return
	}
	run := func(release string) string {
		command := exec.Command(os.Args[0], "-test.v",
			"-test.run=^TestMissingSkipsOutsideAReleaseGateAndFailsInsideOne$")
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"OPENREALTIME_TESTGATE_HELPER=1",
			testgate.Variable + "=" + release,
		}
		output, _ := command.CombinedOutput()
		return string(output)
	}
	ordinary := run("")
	if !strings.Contains(ordinary, "--- SKIP") || strings.Contains(ordinary, "--- FAIL") {
		t.Fatalf("an ordinary run must skip a missing tool:\n%s", ordinary)
	}
	if !strings.Contains(ordinary, "a-tool-nobody-has is not installed") {
		t.Fatalf("the skip must name the tool:\n%s", ordinary)
	}
	release := run("1")
	if !strings.Contains(release, "--- FAIL") || strings.Contains(release, "--- SKIP") {
		t.Fatalf("a release run must fail rather than skip:\n%s", release)
	}
	// Any non-empty value is a release run, matching scripts/check.sh's -n test;
	// a helper that only honoured "1" would let a gate set to "true" skip.
	if other := run("true"); !strings.Contains(other, "--- FAIL") {
		t.Fatalf("a non-empty gate value other than 1 must still be a release run:\n%s", other)
	}
}

func TestUnpreparedAlwaysSkipsAndSaysWhatWasNotVerified(t *testing.T) {
	if os.Getenv("OPENREALTIME_TESTGATE_HELPER") == "2" {
		testgate.Unprepared(t, "the pinned example dataset", os.ErrNotExist)
		return
	}
	command := exec.Command(os.Args[0], "-test.v",
		"-test.run=^TestUnpreparedAlwaysSkipsAndSaysWhatWasNotVerified$")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"OPENREALTIME_TESTGATE_HELPER=2",
		testgate.Variable + "=1",
	}
	output, _ := command.CombinedOutput()
	text := string(output)
	if !strings.Contains(text, "--- SKIP") || strings.Contains(text, "--- FAIL") {
		t.Fatalf("a provisioned input must skip even in release mode:\n%s", text)
	}
	if !strings.Contains(text, "NOT VERIFIED: the pinned example dataset is unavailable") {
		t.Fatalf("the skip must say what was not verified:\n%s", text)
	}
}
