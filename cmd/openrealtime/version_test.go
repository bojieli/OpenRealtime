package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The release version exists twice: as a constant this binary prints, and as
// the VERSION file a release process reads without building anything. Nothing
// checked that the two agree, so the constant could name a version the tree
// had already moved off - which is exactly what happened between the tag cut
// at 1.0.0 and the tree that kept going for another 1,450 commits while every
// binary built from it still said 1.0.0.
//
// The file is found by walking up rather than by a fixed number of parents, so
// moving this package does not silently start reading nothing.
func TestReleaseMatchesTheVERSIONFile(t *testing.T) {
	t.Parallel()
	path := repositoryFile(t, "VERSION")
	declared, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := strings.TrimSpace(string(declared)); got != release {
		t.Fatalf("VERSION says %q and the release constant says %q; they name the same release", got, release)
	}
}

// The version string a deployed binary prints has to carry the release, and
// under `go test` it also carries the toolchain. A binary that cannot say what
// it is cannot be traced back to a source tree.
func TestVersionReportsTheRelease(t *testing.T) {
	t.Parallel()
	printed := version()
	if !strings.HasPrefix(printed, "openrealtime "+release) {
		t.Fatalf("version() = %q, want it to start with the release", printed)
	}
}

// repositoryFile resolves a path relative to the repository root by walking up
// from the working directory until go.mod is found.
func repositoryFile(t *testing.T, name string) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return filepath.Join(directory, name)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no go.mod above %q, so the repository root could not be found", directory)
		}
		directory = parent
	}
}
