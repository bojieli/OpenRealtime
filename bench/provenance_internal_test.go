package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGPUIdentityDoesNotNeedTheVendorControlPlane(t *testing.T) {
	root := t.TempDir()
	information := filepath.Join(root, "0000:01:00.0", "information")
	if err := os.MkdirAll(filepath.Dir(information), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(information, []byte(
		"Model: \t\t NVIDIA RTX PRO 6000 Blackwell Workstation Edition\n"+
			"GPU UUID: \t GPU-example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := nvidiaGPUFromProc(root); got != "NVIDIA RTX PRO 6000 Blackwell Workstation Edition" {
		t.Fatalf("GPU model = %q", got)
	}
}

func TestAnUnavailableGPUIdentityIsNotAnError(t *testing.T) {
	if got := nvidiaGPUFromProc(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("missing GPU model = %q", got)
	}
}
