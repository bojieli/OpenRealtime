package trajectory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const prefixIdentityDomain = "openrealtime.trajectory/prefix/v1"

// PrefixIdentity names one immutable prefix without carrying its items. The
// digest is a domain-separated chain over encoding/json's deterministic
// encoding of each exact Item value through Version, including opaque JSON
// bytes. It is an OpenRealtime identity format, not an RFC canonical-JSON
// claim.
type PrefixIdentity struct {
	Version uint64 `json:"version"`
	Digest  string `json:"digest"`
}

// IdentifyPrefix derives the identity of snapshot.Items[:version]. Snapshot
// must itself be a complete, internally consistent prefix.
func IdentifyPrefix(snapshot Snapshot, version uint64) (PrefixIdentity, error) {
	if err := validateSnapshotShape(snapshot); err != nil {
		return PrefixIdentity{}, err
	}
	if version > snapshot.Version {
		return PrefixIdentity{}, fmt.Errorf(
			"trajectory prefix %d exceeds snapshot version %d", version, snapshot.Version,
		)
	}
	digest := prefixSeed()
	for index := uint64(0); index < version; index++ {
		var err error
		digest, err = prefixItem(digest, index+1, snapshot.Items[index])
		if err != nil {
			return PrefixIdentity{}, fmt.Errorf("encode trajectory item %d: %w", index, err)
		}
	}
	return prefixIdentity(version, digest), nil
}

// VerifyPrefix proves that identity names the exact prefix embedded in
// snapshot. A later snapshot is accepted only when its first identity.Version
// items still produce the same digest.
func VerifyPrefix(snapshot Snapshot, identity PrefixIdentity) error {
	if err := validatePrefixIdentity(identity); err != nil {
		return err
	}
	actual, err := IdentifyPrefix(snapshot, identity.Version)
	if err != nil {
		return err
	}
	if actual.Digest != identity.Digest {
		return fmt.Errorf(
			"trajectory prefix %d digest mismatch: got %s, want %s",
			identity.Version, actual.Digest, identity.Digest,
		)
	}
	return nil
}

// Prefix verifies identity and returns a defensive copy of exactly the named
// immutable prefix.
func Prefix(snapshot Snapshot, identity PrefixIdentity) (Snapshot, error) {
	if err := VerifyPrefix(snapshot, identity); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Version: identity.Version,
		Items:   cloneItems(snapshot.Items[:identity.Version]),
	}, nil
}

func validateSnapshotShape(snapshot Snapshot) error {
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return fmt.Errorf(
			"trajectory snapshot version %d does not equal item count %d",
			snapshot.Version, len(snapshot.Items),
		)
	}
	return nil
}

func validatePrefixIdentity(identity PrefixIdentity) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(identity.Digest, prefix) ||
		len(identity.Digest) != len(prefix)+sha256.Size*2 ||
		identity.Digest != strings.ToLower(identity.Digest) {
		return errors.New("trajectory prefix digest is not canonical SHA-256")
	}
	if _, err := hex.DecodeString(identity.Digest[len(prefix):]); err != nil {
		return errors.New("trajectory prefix digest is not canonical SHA-256")
	}
	return nil
}

func prefixSeed() [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefixIdentityDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte("empty"))
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func prefixIdentity(version uint64, digest [sha256.Size]byte) PrefixIdentity {
	return PrefixIdentity{
		Version: version,
		Digest:  "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func prefixItem(
	previous [sha256.Size]byte, version uint64, item Item,
) ([sha256.Size]byte, error) {
	wire, err := json.Marshal(item)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return prefixLink(previous, version, wire), nil
}

func prefixLink(previous [sha256.Size]byte, version uint64, wire []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefixIdentityDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte("item"))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(previous[:])
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], version)
	_, _ = hash.Write(encoded[:])
	binary.BigEndian.PutUint64(encoded[:], uint64(len(wire)))
	_, _ = hash.Write(encoded[:])
	_, _ = hash.Write(wire)
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}
