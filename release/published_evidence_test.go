package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const publishedManifestPath = "../benchmarks/releases/v0.1.0/manifest.json"

// The released manifest pins four documents that keep evolving on the main
// line by design: the plan and three narrative docs. They verify byte-for-byte
// at the released revision (tag v1.0.0), which is where a third party should
// run `release verify`. Everything else the release pins is evidence — traces,
// reports, fixtures, schemas, generated code — and evidence must never change
// after publication, on any revision.
var narrativeDocuments = map[string]bool{
	"PLAN.md":                               true,
	"docs/experiments.md":                   true,
	"docs/metrics.md":                       true,
	"docs/openai-realtime-compatibility.md": true,
}

func TestPublishedEvidenceIsUnchanged(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Clean(publishedManifestPath))
	if err != nil {
		t.Fatalf("read published manifest: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode published manifest: %v", err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("published manifest pins no files; the check is vacuous")
	}
	pinned := make(map[string]bool, len(manifest.Files))
	evidence := 0
	for _, file := range manifest.Files {
		pinned[file.Path] = true
		if narrativeDocuments[file.Path] {
			// Named as free to evolve; only its continued existence is checked.
			if _, err := os.Stat(filepath.Join("..", file.Path)); err != nil {
				t.Errorf("released document %s is gone: %v", file.Path, err)
			}
			continue
		}
		evidence++
		content, err := os.ReadFile(filepath.Join("..", filepath.Clean(file.Path)))
		if err != nil {
			t.Errorf("released evidence %s is unreadable: %v", file.Path, err)
			continue
		}
		if int64(len(content)) != file.Bytes {
			t.Errorf("released evidence %s changed size: %d bytes, published %d",
				file.Path, len(content), file.Bytes)
			continue
		}
		if digest := hex.EncodeToString(sha256Sum(content)); digest != file.SHA256 {
			t.Errorf("released evidence %s was modified after publication: %s, published %s",
				file.Path, digest, file.SHA256)
		}
	}
	if evidence == 0 {
		t.Error("no released evidence was checked; the exemption list swallowed the manifest")
	}
	// An exemption for a path the release does not pin is a stale exemption.
	for path := range narrativeDocuments {
		if !pinned[path] {
			t.Errorf("%s is exempted but the release does not pin it; remove the exemption", path)
		}
	}
}

func sha256Sum(content []byte) []byte {
	sum := sha256.Sum256(content)
	return sum[:]
}
