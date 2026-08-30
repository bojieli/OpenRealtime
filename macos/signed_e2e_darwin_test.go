//go:build darwin && signed_macos_e2e

package macos

import (
	"os"
	"os/exec"
	"testing"
)

func TestSignedNativeRunnerReleaseGate(t *testing.T) {
	command := exec.Command("./verify-signed-e2e.sh")
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("signed native release gate failed closed: %v\n%s", err, output)
	}
}
