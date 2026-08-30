package fileidentity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRequireSingleLinkAcceptsExclusiveFileAndRejectsHardLink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "artifact")
	if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := RequireSingleLink(file); err != nil {
		t.Fatalf("exclusive file rejected: %v", err)
	}
	if err := os.Link(path, filepath.Join(directory, "external-alias")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := RequireSingleLink(file); err == nil {
		t.Fatal("file with an external hard link was accepted")
	}
}
