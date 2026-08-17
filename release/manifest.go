// Package release builds and verifies deterministic benchmark release manifests.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

const (
	SchemaVersion = "0.1.0"
	ReleaseID     = "openrealtime-benchmarks-v0.1.0"
)

type ProtocolSource struct {
	URL         string `json:"url"`
	Revision    string `json:"revision"`
	SHA256      string `json:"sha256"`
	RetrievedAt string `json:"retrieved_at"`
}

type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion  string         `json:"schema_version"`
	ReleaseID      string         `json:"release_id"`
	ReleaseDate    string         `json:"release_date"`
	Runtime        string         `json:"runtime"`
	EvidenceMode   string         `json:"evidence_mode"`
	ProtocolSource ProtocolSource `json:"openai_protocol_source"`
	Files          []File         `json:"files"`
}

var defaultInputs = []string{
	"PLAN.md",
	"benchmarks/m1/reference",
	"benchmarks/m2/reference",
	"benchmarks/m3/reference",
	"benchmarks/m4/reference",
	"benchmarks/m5/reference",
	"benchmarks/releases/v0.1.0/README.md",
	"benchmarks/releases/v0.1.0/preregistration.json",
	"benchmarks/releases/v0.1.0/study.json",
	"docs/experiments.md",
	"docs/metrics.md",
	"docs/openai-realtime-compatibility.md",
	"docs/research/human-study-protocol-v0.1.md",
	"docs/research/technical-report-v0.1.md",
	"protocol/openai/events_gen.go",
	"protocol/openai/openai-realtime-events.schema.json",
	"schemas/trace-record-v0.2.schema.json",
	"tests/fixtures",
}

func Build(root string) (Manifest, error) {
	if root == "" {
		return Manifest{}, errors.New("release root must not be empty")
	}
	source, err := protocolSource()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{
		SchemaVersion: SchemaVersion, ReleaseID: ReleaseID, ReleaseDate: "2026-08-17",
		Runtime:      "Go 1.25+; no Python runtime or build dependency",
		EvidenceMode: "deterministic_project_authored_reference", ProtocolSource: source,
	}
	seen := make(map[string]struct{})
	for _, relative := range defaultInputs {
		if err := collect(root, relative, seen, &manifest.Files); err != nil {
			return Manifest{}, err
		}
	}
	slices.SortFunc(manifest.Files, func(left, right File) int {
		return strings.Compare(left.Path, right.Path)
	})
	if len(manifest.Files) == 0 {
		return Manifest{}, errors.New("release manifest cannot be empty")
	}
	return manifest, nil
}

func Load(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	const maximumBytes = int64(4 << 20)
	var manifest Manifest
	decoder := json.NewDecoder(io.LimitReader(file, maximumBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("release manifest must contain exactly one JSON value")
		}
		return Manifest{}, fmt.Errorf("decode trailing release manifest data: %w", err)
	}
	return manifest, nil
}

func Verify(root string, manifest Manifest) error {
	if root == "" {
		return errors.New("release root must not be empty")
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.ReleaseID != ReleaseID || len(manifest.Files) == 0 {
		return errors.New("unsupported or empty release manifest")
	}
	source, err := protocolSource()
	if err != nil {
		return err
	}
	if manifest.ProtocolSource != source {
		return errors.New("release protocol provenance does not match embedded schema")
	}
	previous := ""
	seen := make(map[string]struct{}, len(manifest.Files))
	for _, expected := range manifest.Files {
		if err := validateRelative(expected.Path); err != nil {
			return err
		}
		if expected.Path <= previous {
			return errors.New("release files must be unique and strictly sorted")
		}
		previous = expected.Path
		if _, exists := seen[expected.Path]; exists {
			return fmt.Errorf("duplicate release file %q", expected.Path)
		}
		seen[expected.Path] = struct{}{}
		actual, err := inspectFile(root, expected.Path)
		if err != nil {
			return err
		}
		if actual.Bytes != expected.Bytes || actual.SHA256 != expected.SHA256 {
			return fmt.Errorf("release file %s does not match size/hash manifest", expected.Path)
		}
	}
	return nil
}

func collect(root, relative string, seen map[string]struct{}, files *[]File) error {
	if err := validateRelative(relative); err != nil {
		return err
	}
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	metadata, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect release input %s: %w", relative, err)
	}
	if metadata.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("release input %s must not be a symlink", relative)
	}
	if metadata.Mode().IsRegular() {
		return appendFile(root, relative, seen, files)
	}
	if !metadata.IsDir() {
		return fmt.Errorf("release input %s is not a regular file or directory", relative)
	}
	return filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release input contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("release input contains non-regular file %s", path)
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		return appendFile(root, filepath.ToSlash(relativePath), seen, files)
	})
}

func appendFile(root, relative string, seen map[string]struct{}, files *[]File) error {
	if _, exists := seen[relative]; exists {
		return fmt.Errorf("duplicate release input %q", relative)
	}
	seen[relative] = struct{}{}
	file, err := inspectFile(root, relative)
	if err != nil {
		return err
	}
	*files = append(*files, file)
	return nil
}

func inspectFile(root, relative string) (File, error) {
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	metadata, err := os.Lstat(absolute)
	if err != nil {
		return File{}, fmt.Errorf("inspect release file %s: %w", relative, err)
	}
	if !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 {
		return File{}, fmt.Errorf("release file %s must be a nonsymlink regular file", relative)
	}
	input, err := os.Open(absolute)
	if err != nil {
		return File{}, err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return File{}, err
	}
	return File{Path: relative, Bytes: metadata.Size(), SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func protocolSource() (ProtocolSource, error) {
	var bundle struct {
		Source ProtocolSource `json:"x-openrealtime-source"`
	}
	if err := json.Unmarshal(openaiwire.SchemaBundle, &bundle); err != nil {
		return ProtocolSource{}, fmt.Errorf("decode embedded protocol provenance: %w", err)
	}
	if bundle.Source.URL == "" || bundle.Source.Revision == "" || bundle.Source.SHA256 == "" || bundle.Source.RetrievedAt == "" {
		return ProtocolSource{}, errors.New("embedded protocol provenance is incomplete")
	}
	return bundle.Source, nil
}

func validateRelative(path string) error {
	if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || path == "." || strings.HasPrefix(path, "../") {
		return fmt.Errorf("unsafe release path %q", path)
	}
	return nil
}
