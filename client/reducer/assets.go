package reducer

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// portableAssets are the exact language-neutral artifacts consumed by the
// browser bundle and external conformance runners. Keeping the canonical
// module here lets presentation profiles assemble a platform adapter without
// maintaining a second reducer implementation.
//
//go:embed javascript/reducer.mjs testdata/reducer_vectors.json swift/Package.swift swift/StrictJSON.swift swift/ClientReducer.swift swift/NativeClientCore.swift
var portableAssets embed.FS

var swiftCoreFiles = []string{
	"swift/Package.swift",
	"swift/StrictJSON.swift",
	"swift/ClientReducer.swift",
	"swift/NativeClientCore.swift",
}

func JavaScriptSource() ([]byte, error) {
	payload, err := portableAssets.ReadFile("javascript/reducer.mjs")
	if err != nil {
		return nil, fmt.Errorf("read embedded JavaScript reducer: %w", err)
	}
	return append([]byte(nil), payload...), nil
}

func CorpusSource() ([]byte, error) {
	payload, err := portableAssets.ReadFile("testdata/reducer_vectors.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded reducer corpus: %w", err)
	}
	return append([]byte(nil), payload...), nil
}

// SwiftSourceDigest binds a native implementation selection to the exact
// portable reducer/package sources that are compiled into it.
func SwiftSourceDigest() (string, error) {
	digest := sha256.New()
	for _, name := range swiftCoreFiles {
		payload, err := portableAssets.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("read embedded Swift reducer source %s: %w", name, err)
		}
		writeDigestField(digest, name, payload)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func writeDigestField(digest hash.Hash, name string, payload []byte) {
	_, _ = io.WriteString(digest, fmt.Sprintf("%d:", len(name)))
	_, _ = io.WriteString(digest, name)
	_, _ = io.WriteString(digest, fmt.Sprintf("%d:", len(payload)))
	_, _ = digest.Write(payload)
}
