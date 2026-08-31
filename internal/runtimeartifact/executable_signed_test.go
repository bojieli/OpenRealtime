package runtimeartifact

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestSignedDarwinExecutableIdentityIsStableAndValidated(t *testing.T) {
	blob := darwinSigningBlobFixture(32)
	cdhash := make([]byte, darwinCodeDirectoryHashBytes)
	for index := range cdhash {
		cdhash[index] = byte(index + 1)
	}
	status := darwinCodeSigningValid | darwinCodeSigningSigned
	first, err := signedDarwinExecutableIdentity(
		"go://openrealtime/darwin-runtime", status, status, cdhash, cdhash, blob,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := signedDarwinExecutableIdentity(
		"go://openrealtime/darwin-runtime", status, status, cdhash, cdhash, blob,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("unstable Darwin runtime identity: first=%+v second=%+v", first, second)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}

	changedBlob := append([]byte(nil), blob...)
	changedBlob[len(changedBlob)-1] ^= 1
	changed, err := signedDarwinExecutableIdentity(
		"go://openrealtime/darwin-runtime", status, status, cdhash, cdhash, changedBlob,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Digest == first.Digest {
		t.Fatal("different signing material produced the same runtime identity")
	}
}

func TestSignedDarwinExecutableIdentityFailsClosed(t *testing.T) {
	blob := darwinSigningBlobFixture(24)
	cdhash := make([]byte, darwinCodeDirectoryHashBytes)
	cdhash[0] = 1
	valid := darwinCodeSigningValid | darwinCodeSigningSigned
	tests := []struct {
		name          string
		before, after uint32
		first, second []byte
		blob          []byte
		want          string
	}{
		{name: "unsigned", before: darwinCodeSigningValid, after: valid, first: cdhash, second: cdhash, blob: blob, want: "not code signed"},
		{name: "invalid", before: darwinCodeSigningSigned, after: valid, first: cdhash, second: cdhash, blob: blob, want: "not valid"},
		{name: "debugged", before: valid | darwinCodeSigningDebugged, after: valid, first: cdhash, second: cdhash, blob: blob, want: "debugged"},
		{name: "killed", before: valid | darwinCodeSigningKilled, after: valid, first: cdhash, second: cdhash, blob: blob, want: "killed"},
		{name: "changed status", before: valid, after: darwinCodeSigningSigned, first: cdhash, second: cdhash, blob: blob, want: "final code-signing status"},
		{name: "empty hash", before: valid, after: valid, first: make([]byte, darwinCodeDirectoryHashBytes), second: make([]byte, darwinCodeDirectoryHashBytes), blob: blob, want: "empty CodeDirectory"},
		{name: "changed hash", before: valid, after: valid, first: cdhash, second: append([]byte{2}, cdhash[1:]...), blob: blob, want: "CodeDirectory changed"},
		{name: "short hash", before: valid, after: valid, first: cdhash[:19], second: cdhash[:19], blob: blob, want: "hash length"},
		{name: "short blob", before: valid, after: valid, first: cdhash, second: cdhash, blob: blob[:8], want: "invalid code-signature size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := signedDarwinExecutableIdentity(
				"go://openrealtime/darwin-runtime", test.before, test.after,
				test.first, test.second, test.blob,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestDarwinCodeSignatureBlobCanonicalizesAllocationTail(t *testing.T) {
	blob := append(darwinSigningBlobFixture(16), make([]byte, 24)...)
	for index := 16; index < len(blob); index++ {
		blob[index] = byte(index + 1)
	}
	canonical, err := canonicalDarwinCodeSignatureBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) != 16 {
		t.Fatalf("canonical code-signature length = %d, want 16", len(canonical))
	}
	canonical[8] ^= 1
	if blob[8] != canonical[8] {
		t.Fatal("canonical code-signature payload is not the exact kernel buffer prefix")
	}
}

func TestDarwinCodeSignatureBlobValidationRejectsForgery(t *testing.T) {
	for _, test := range []struct {
		name string
		blob []byte
		want string
	}{
		{name: "magic", blob: func() []byte { value := darwinSigningBlobFixture(16); value[0] = 0; return value }(), want: "magic"},
		{name: "oversized length", blob: func() []byte {
			value := darwinSigningBlobFixture(16)
			binary.BigEndian.PutUint32(value[4:8], 17)
			return value
		}(), want: "length"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := canonicalDarwinCodeSignatureBlob(test.blob); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func darwinSigningBlobFixture(size int) []byte {
	value := make([]byte, size)
	binary.BigEndian.PutUint32(value[:4], darwinCodeSignatureMagic)
	binary.BigEndian.PutUint32(value[4:8], uint32(size))
	for index := 8; index < len(value); index++ {
		value[index] = byte(index)
	}
	return value
}
