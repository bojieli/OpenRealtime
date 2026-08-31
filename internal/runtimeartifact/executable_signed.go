package runtimeartifact

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

const (
	darwinCodeSignatureMagic        = uint32(0xfade0cc0)
	darwinDetachedSignatureMagic    = uint32(0xfade0cc1)
	darwinCodeDirectoryHashBytes    = 20
	maximumDarwinCodeSignatureBytes = 64 << 20
	darwinExecutableDigestDomain    = "openrealtime/runtimeartifact/darwin-code-signature/v1\x00"
	darwinCodeSigningValid          = uint32(0x00000001)
	darwinCodeSigningKilled         = uint32(0x01000000)
	darwinCodeSigningDebugged       = uint32(0x10000000)
	darwinCodeSigningSigned         = uint32(0x20000000)
)

// signedDarwinExecutableIdentity validates two observations around the
// kernel-returned signing blob and then derives a SHA-256 artifact identity.
// The active cdhash selects the exact architecture-specific CodeDirectory;
// the signing blob seals the executable pages and metadata described by it.
func signedDarwinExecutableIdentity(
	id string,
	statusBefore, statusAfter uint32,
	cdhashBefore, cdhashAfter, signingBlob []byte,
) (inspect.ArtifactIdentity, error) {
	if err := validateDarwinCodeSigningStatus(statusBefore); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("initial code-signing status: %w", err)
	}
	if err := validateDarwinCodeSigningStatus(statusAfter); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("final code-signing status: %w", err)
	}
	if len(cdhashBefore) != darwinCodeDirectoryHashBytes ||
		len(cdhashAfter) != darwinCodeDirectoryHashBytes {
		return inspect.ArtifactIdentity{}, errors.New("kernel returned an invalid CodeDirectory hash length")
	}
	if allZeroBytes(cdhashBefore) || allZeroBytes(cdhashAfter) {
		return inspect.ArtifactIdentity{}, errors.New("kernel returned an empty CodeDirectory hash")
	}
	if !equalBytes(cdhashBefore, cdhashAfter) {
		return inspect.ArtifactIdentity{}, errors.New("running executable CodeDirectory changed while it was identified")
	}
	if err := validateDarwinCodeSignatureBlob(signingBlob); err != nil {
		return inspect.ArtifactIdentity{}, err
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte(darwinExecutableDigestDomain))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(signingBlob)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(signingBlob)
	_, _ = digest.Write(cdhashBefore)
	identity := inspect.ArtifactIdentity{
		ID: id, Digest: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
	}
	if err := identity.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("runtime artifact identity: %w", err)
	}
	return identity, nil
}

func validateDarwinCodeSigningStatus(status uint32) error {
	if status&darwinCodeSigningSigned == 0 {
		return errors.New("running executable is not code signed")
	}
	if status&darwinCodeSigningValid == 0 {
		return errors.New("running executable code signature is not valid")
	}
	if status&darwinCodeSigningKilled != 0 {
		return errors.New("running executable was killed for a code-signing violation")
	}
	if status&darwinCodeSigningDebugged != 0 {
		return errors.New("debugged executable cannot provide immutable runtime identity")
	}
	return nil
}

func validateDarwinCodeSignatureBlob(blob []byte) error {
	if len(blob) < 12 || len(blob) > maximumDarwinCodeSignatureBytes {
		return fmt.Errorf("kernel returned an invalid code-signature size %d", len(blob))
	}
	magic := binary.BigEndian.Uint32(blob[:4])
	if magic != darwinCodeSignatureMagic && magic != darwinDetachedSignatureMagic {
		return fmt.Errorf("kernel returned unsupported code-signature magic 0x%08x", magic)
	}
	if encoded := binary.BigEndian.Uint32(blob[4:8]); uint64(encoded) != uint64(len(blob)) {
		return fmt.Errorf(
			"kernel returned inconsistent code-signature length %d for %d bytes",
			encoded, len(blob),
		)
	}
	return nil
}

func allZeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
