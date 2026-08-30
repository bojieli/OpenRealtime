// Package runtimeartifact identifies executable bytes used by in-process
// plugin registrations. It deliberately does not infer a source revision:
// the SHA-256 is the exact code identity, while source provenance remains a
// separate benchmark concern.
package runtimeartifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

// File hashes one exact regular-file payload. It is exported primarily for
// launchers that execute a separately staged implementation and for tests;
// callers remain responsible for opening that path without a mutable alias.
func File(id, path string) (inspect.ArtifactIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("open runtime artifact: %w", err)
	}
	defer file.Close()
	return openedFile(id, file, path)
}

// openedFile hashes an already-opened regular file and verifies that its
// identity and size did not change while it was read. Executable deliberately
// passes the mapped image descriptor here; it must never turn that descriptor
// back into a pathname.
func openedFile(id string, file *os.File, source string) (inspect.ArtifactIdentity, error) {
	before, err := file.Stat()
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("stat runtime artifact: %w", err)
	}
	if !before.Mode().IsRegular() {
		return inspect.ArtifactIdentity{}, fmt.Errorf("runtime artifact %q is not a regular file", source)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("hash runtime artifact: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("restat runtime artifact: %w", err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() ||
		!before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
		return inspect.ArtifactIdentity{}, fmt.Errorf(
			"runtime artifact %q changed while it was hashed", source,
		)
	}
	identity := inspect.ArtifactIdentity{
		ID: id, Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
	}
	if err := identity.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("runtime artifact identity: %w", err)
	}
	return identity, nil
}
