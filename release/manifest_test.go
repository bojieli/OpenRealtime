package release

import (
	"path/filepath"
	"testing"
)

func TestManifestBuildAndVerify(t *testing.T) {
	t.Parallel()
	manifest, err := Build("..")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) < 20 {
		t.Fatalf("release manifest is unexpectedly small: %d", len(manifest.Files))
	}
	if err := Verify("..", manifest); err != nil {
		t.Fatal(err)
	}
	mutated := manifest
	mutated.Files = append([]File(nil), manifest.Files...)
	mutated.Files[0].SHA256 = "00"
	if err := Verify("..", mutated); err == nil {
		t.Fatal("hash mutation was accepted")
	}
}

func TestManifestRejectsTraversal(t *testing.T) {
	t.Parallel()
	manifest := Manifest{SchemaVersion: SchemaVersion, ReleaseID: ReleaseID, Files: []File{{Path: filepath.ToSlash("../escape")}}}
	if err := Verify("..", manifest); err == nil {
		t.Fatal("path traversal was accepted")
	}
}
