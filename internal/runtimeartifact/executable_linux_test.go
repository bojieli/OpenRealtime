//go:build linux

package runtimeartifact_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/runtimeartifact"
)

func TestExecutableHashesTheKernelMappedImage(t *testing.T) {
	identity, err := runtimeartifact.Executable("go://openrealtime/mapped-image-test")
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, mapped); err != nil {
		t.Fatal(err)
	}
	want := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if identity.Digest != want {
		t.Fatalf("mapped executable digest = %q, direct /proc/self/exe digest = %q", identity.Digest, want)
	}
}
