package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareEmptyDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	newDirectory := filepath.Join(root, "new", "output")
	if err := prepareEmptyDirectory(newDirectory); err != nil {
		t.Fatal(err)
	}
	if err := prepareEmptyDirectory(newDirectory); err != nil {
		t.Fatalf("empty existing directory must be accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newDirectory, "old-report.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareEmptyDirectory(newDirectory); err == nil {
		t.Fatal("non-empty benchmark directory must be rejected")
	}
}
