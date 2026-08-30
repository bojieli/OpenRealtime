package runtimeartifact_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/runtimeartifact"
)

func TestFileProducesExactValidatedSHA256Identity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "implementation.bin")
	payload := []byte("exact implementation bytes\n")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := runtimeartifact.File("go://openrealtime/test-implementation", path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	if identity.ID != "go://openrealtime/test-implementation" ||
		identity.Digest != "sha256:"+hex.EncodeToString(want[:]) || identity.Revision != "" {
		t.Fatalf("runtime identity = %+v", identity)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFileRejectsMutableIdentityAndNonRegularPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "implementation.bin")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeartifact.File("implementation:latest", path); err == nil ||
		!strings.Contains(err.Error(), "mutable or placeholder") {
		t.Fatalf("mutable artifact ID error = %v", err)
	}
	if _, err := runtimeartifact.File("go://openrealtime/directory", filepath.Dir(path)); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory artifact error = %v", err)
	}
}

func TestExecutableIdentifiesThisTestBinary(t *testing.T) {
	identity, err := runtimeartifact.Executable("go://openrealtime/runtimeartifact-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}
