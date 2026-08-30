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

// Executable hashes the bytes of the running executable under a stable
// deployment-controlled artifact ID.
func Executable(id string) (inspect.ArtifactIdentity, error) {
	path, err := os.Executable()
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("resolve running executable: %w", err)
	}
	return File(id, path)
}

// File hashes one exact regular-file payload. It is exported primarily for
// launchers that execute a separately staged implementation and for tests;
// callers remain responsible for opening that path without a mutable alias.
func File(id, path string) (inspect.ArtifactIdentity, error) {
	file, err := os.Open(path)
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("open runtime artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("stat runtime artifact: %w", err)
	}
	if !info.Mode().IsRegular() {
		return inspect.ArtifactIdentity{}, fmt.Errorf("runtime artifact %q is not a regular file", path)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("hash runtime artifact: %w", err)
	}
	identity := inspect.ArtifactIdentity{
		ID: id, Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
	}
	if err := identity.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("runtime artifact identity: %w", err)
	}
	return identity, nil
}
