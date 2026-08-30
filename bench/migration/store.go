package migration

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	MaxImportedArtifactBytes int64 = 512 << 20
	MaxListedArtifacts             = 65_536
)

const (
	EvidenceKindManifest      = "migration-manifest"
	EvidenceKindCensus        = "suite-census"
	EvidenceKindImportProfile = "migration-import-profile"
	EvidenceKindResult        = "benchmark-result"
	EvidenceKindExecution     = "execution-attestation"
)

// LocalStore is the create-only evidence store used by migration integration.
// References are always relative to Root, content-addressed, and verified on
// every read. It is deliberately smaller than a general object-store client;
// remote storage can implement the same preregistration/import contract after
// it provides equivalent create-only and digest guarantees.
type LocalStore struct {
	Root string
}

func (store LocalStore) validate() (string, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return "", errors.New("migration evidence store needs a root")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve migration evidence store: %w", err)
	}
	return filepath.Clean(absolute), nil
}

// Archive copies an existing artifact into its content-addressed, create-only
// location. Repeating the operation for identical bytes is idempotent; bytes
// at an existing content address are always reverified before reuse.
func (store LocalStore) Archive(kind, sourcePath string) (EvidenceRef, error) {
	if !validArtifactKind(kind) {
		return EvidenceRef{}, fmt.Errorf("invalid migration evidence kind %q", kind)
	}
	payload, err := readBoundedFile(sourcePath, MaxImportedArtifactBytes)
	if err != nil {
		return EvidenceRef{}, err
	}
	extension := strings.ToLower(filepath.Ext(sourcePath))
	return store.ArchiveBytes(kind, extension, payload)
}

// ArchiveBytes retains generated compatibility artifacts without first
// writing an overwriteable staging path.
func (store LocalStore) ArchiveBytes(kind, extension string, payload []byte) (EvidenceRef, error) {
	if !validArtifactKind(kind) {
		return EvidenceRef{}, fmt.Errorf("invalid migration evidence kind %q", kind)
	}
	if int64(len(payload)) > MaxImportedArtifactBytes {
		return EvidenceRef{}, fmt.Errorf("migration evidence exceeds %d bytes", MaxImportedArtifactBytes)
	}
	extension = strings.ToLower(strings.TrimSpace(extension))
	if extension != "" && (!strings.HasPrefix(extension, ".") || len(extension) > 12 ||
		strings.ContainsAny(extension, `/\\`) || strings.Contains(extension, "..")) {
		return EvidenceRef{}, fmt.Errorf("invalid migration evidence extension %q", extension)
	}
	digest := sha256.Sum256(payload)
	encoded := hex.EncodeToString(digest[:])
	location := filepath.ToSlash(filepath.Join("artifacts", kind, encoded+extension))
	if err := store.writeCreateOnly(location, payload); err != nil {
		return EvidenceRef{}, err
	}
	return EvidenceRef{Kind: kind, Location: location, SHA256: encoded}, nil
}

// Put archives caller-produced canonical bytes under an explicit logical
// location. It never replaces a prior campaign artifact.
func (store LocalStore) Put(kind, location string, payload []byte) (EvidenceRef, error) {
	if !validArtifactKind(kind) {
		return EvidenceRef{}, fmt.Errorf("invalid migration evidence kind %q", kind)
	}
	if int64(len(payload)) > MaxImportedArtifactBytes {
		return EvidenceRef{}, fmt.Errorf("migration evidence exceeds %d bytes", MaxImportedArtifactBytes)
	}
	if err := store.writeCreateOnly(location, payload); err != nil {
		return EvidenceRef{}, err
	}
	digest := sha256.Sum256(payload)
	return EvidenceRef{Kind: kind, Location: filepath.ToSlash(filepath.Clean(location)),
		SHA256: hex.EncodeToString(digest[:])}, nil
}

func (store LocalStore) writeCreateOnly(location string, payload []byte) error {
	clean, err := cleanStoreLocation(location)
	if err != nil {
		return err
	}
	root, err := store.openRoot(true)
	if err != nil {
		return err
	}
	defer root.Close()
	directory := filepath.Dir(clean)
	if err := root.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create migration evidence directory: %w", err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("name migration evidence staging file: %w", err)
	}
	temporary := filepath.Join(directory, ".migration-evidence-"+hex.EncodeToString(random))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create migration evidence staging file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		_ = root.Remove(temporary)
	}()
	written, err := file.Write(payload)
	if err != nil {
		return fmt.Errorf("write migration evidence staging file: %w", err)
	}
	if written != len(payload) {
		return fmt.Errorf("write migration evidence staging file: %w", io.ErrShortWrite)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync migration evidence staging file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close migration evidence staging file: %w", err)
	}
	closed = true
	if err := root.Link(temporary, clean); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("archive migration evidence: %w", err)
		}
		existing, readErr := readBoundedRootFile(root, clean, MaxImportedArtifactBytes)
		if readErr != nil {
			return fmt.Errorf("verify existing migration evidence: %w", readErr)
		}
		if !bytes.Equal(existing, payload) {
			return errors.New("migration evidence location already contains different bytes")
		}
	}
	directoryHandle, err := root.Open(directory)
	if err != nil {
		return fmt.Errorf("open migration evidence directory for sync: %w", err)
	}
	if err := directoryHandle.Sync(); err != nil {
		_ = directoryHandle.Close()
		return fmt.Errorf("sync migration evidence directory: %w", err)
	}
	if err := directoryHandle.Close(); err != nil {
		return fmt.Errorf("close migration evidence directory: %w", err)
	}
	return nil
}

// Resolve reads and verifies an immutable evidence reference. A JSON fragment
// after '#' identifies a row inside the artifact but is not part of its file
// location or digest.
func (store LocalStore) Resolve(reference EvidenceRef) ([]byte, error) {
	if strings.TrimSpace(reference.Kind) == "" || !validSHA256(reference.SHA256) {
		return nil, errors.New("migration evidence reference needs kind and a lowercase SHA-256")
	}
	location := reference.Location
	if before, _, found := strings.Cut(location, "#"); found {
		location = before
	}
	clean, err := cleanStoreLocation(location)
	if err != nil {
		return nil, err
	}
	root, err := store.openRoot(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	payload, err := readBoundedRootFile(root, clean, MaxImportedArtifactBytes)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != reference.SHA256 {
		return nil, fmt.Errorf("migration evidence %q digest does not match", reference.Location)
	}
	return payload, nil
}

// List returns every content-addressed artifact of one kind. The SHA-256 is
// recovered from the content-addressed filename; callers still use Resolve to
// read and verify the bytes. Listing is capped so a hostile store cannot force
// an unbounded directory allocation. Campaign import
// uses it to discover all launch intents/outcomes for a registration so a
// caller cannot omit an unfavorable attempt merely by leaving its reference
// off the comparison command line.
func (store LocalStore) List(kind string) ([]EvidenceRef, error) {
	if !validArtifactKind(kind) {
		return nil, fmt.Errorf("invalid migration evidence kind %q", kind)
	}
	root, err := store.openRoot(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory := filepath.Join("artifacts", kind)
	handle, err := root.Open(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []EvidenceRef{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list migration evidence kind %q: %w", kind, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = handle.Close()
		}
	}()
	result := make([]EvidenceRef, 0)
	for {
		entries, readErr := handle.ReadDir(256)
		for _, entry := range entries {
			if len(result) == MaxListedArtifacts {
				return nil, fmt.Errorf("migration evidence kind %q exceeds listing limit %d",
					kind, MaxListedArtifacts)
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return nil, fmt.Errorf("inspect migration evidence kind %q entry %q: %w",
					kind, entry.Name(), infoErr)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("migration evidence kind %q contains non-regular entry %q",
					kind, entry.Name())
			}
			digest, nameErr := artifactFilenameDigest(entry.Name())
			if nameErr != nil {
				return nil, fmt.Errorf("migration evidence kind %q: %w", kind, nameErr)
			}
			location := filepath.Join(directory, entry.Name())
			result = append(result, EvidenceRef{
				Kind: kind, Location: filepath.ToSlash(location), SHA256: digest,
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("list migration evidence kind %q: %w", kind, readErr)
		}
	}
	if err := handle.Close(); err != nil {
		return nil, fmt.Errorf("close migration evidence directory: %w", err)
	}
	closed = true
	sort.Slice(result, func(left, right int) bool { return result[left].Location < result[right].Location })
	return result, nil
}

func artifactFilenameDigest(name string) (string, error) {
	if len(name) < sha256.Size*2 {
		return "", fmt.Errorf("migration evidence entry %q is not content-addressed", name)
	}
	digest := name[:sha256.Size*2]
	if !validSHA256(digest) {
		return "", fmt.Errorf("migration evidence entry %q has an invalid content digest", name)
	}
	extension := name[sha256.Size*2:]
	if extension != "" && (!strings.HasPrefix(extension, ".") || len(extension) > 12 ||
		strings.ContainsAny(extension, `/\\`) || strings.Contains(extension, "..")) {
		return "", fmt.Errorf("migration evidence entry %q has an invalid extension", name)
	}
	return digest, nil
}

func (store LocalStore) openRoot(create bool) (*os.Root, error) {
	root, err := store.validate()
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("create migration evidence store: %w", err)
		}
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open migration evidence store: %w", err)
	}
	return handle, nil
}

func cleanStoreLocation(location string) (string, error) {
	if strings.Contains(location, "#") {
		return "", errors.New("migration evidence storage location cannot contain a fragment")
	}
	trimmed := strings.TrimSpace(location)
	if trimmed == "" || filepath.IsAbs(trimmed) {
		return "", errors.New("migration evidence location must be a non-empty relative path")
	}
	clean := filepath.Clean(filepath.FromSlash(trimmed))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("migration evidence location escapes the store root")
	}
	return clean, nil
}

func readBoundedRootFile(root *os.Root, path string, maximum int64) ([]byte, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read migration evidence %s: %w", path, err)
	}
	defer file.Close()
	return readBoundedOpenFile(file, path, maximum)
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("migration evidence needs a path")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read migration evidence %s: %w", path, err)
	}
	defer file.Close()
	return readBoundedOpenFile(file, path, maximum)
}

func readBoundedOpenFile(file *os.File, path string, maximum int64) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat migration evidence %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("migration evidence %s is not a regular file", path)
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("migration evidence %s exceeds %d bytes", path, maximum)
	}
	limited := &io.LimitedReader{R: file, N: maximum + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read migration evidence %s: %w", path, err)
	}
	if int64(len(payload)) > maximum {
		return nil, fmt.Errorf("migration evidence %s exceeds %d bytes", path, maximum)
	}
	return payload, nil
}

func validArtifactKind(kind string) bool {
	if kind == "" || len(kind) > 128 {
		return false
	}
	for index := range len(kind) {
		character := kind[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}
