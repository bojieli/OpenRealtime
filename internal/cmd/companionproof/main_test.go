package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestValidateBindsTwoRedactedSnapshotsToOneStableGraph(t *testing.T) {
	browser := testSnapshot(1, time.Unix(1_700_000_000, 1).UTC())
	native := testSnapshot(2, time.Unix(1_700_000_001, 2).UTC())
	browserPath := writeSnapshot(t, "browser.json", browser)
	nativePath := writeSnapshot(t, "native.json", native)

	receipt, err := validate(options{
		browserPath: browserPath, browserSession: "sess_browser",
		nativePath: nativePath, nativeSession: "sess_native",
		invocationNonce:      strings.Repeat("f", 64),
		serverIdentityDigest: "sha256:" + strings.Repeat("e", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Schema != "openrealtime/companion/management-receipt/v1" ||
		receipt.InvocationNonce != strings.Repeat("f", 64) ||
		receipt.ServerIdentityDigest != "sha256:"+strings.Repeat("e", 64) ||
		receipt.Browser.Order != 1 || receipt.Browser.Client != "browser" ||
		receipt.Browser.Transport != "webrtc" || receipt.Native.Order != 2 ||
		receipt.Native.Client != "macos_native" || receipt.Native.Transport != "websocket" ||
		receipt.Browser.SessionID != "sess_browser" || receipt.Native.SessionID != "sess_native" ||
		receipt.Browser.ResponseURL != "http://127.0.0.1:18767/client/v1/management/sessions/sess_browser/live" ||
		receipt.Native.ResponseURL != "http://127.0.0.1:18767/client/v1/management/sessions/sess_native/live" ||
		receipt.Browser.Sequence != 1 || receipt.Native.Sequence != 2 ||
		receipt.Browser.PayloadDigest == receipt.Native.PayloadDigest ||
		receipt.Browser.StableIdentityDigest != receipt.Native.StableIdentityDigest ||
		receipt.Graph.StableIdentityDigest != receipt.Browser.StableIdentityDigest ||
		receipt.Graph.Fingerprint != browser.Fingerprint {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestReadSnapshotRejectsUnknownUnredactedAndNonPrivateArtifacts(t *testing.T) {
	value := testSnapshot(1, time.Unix(1_700_000_000, 0).UTC())
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	unknown := bytes.Replace(payload, []byte(`{"format_version":1,`),
		[]byte(`{"format_version":1,"unknown":true,`), 1)
	unknownPath := filepath.Join(privateTempDir(t), "unknown.json")
	if err := os.WriteFile(unknownPath, unknown, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown-field error = %v", err)
	}

	node := value.Nodes["node"]
	node.LastOutcome = "private response"
	value.Nodes["node"] = node
	unredactedPath := writeSnapshot(t, "unredacted.json", value)
	if _, _, err := readSnapshot(unredactedPath); err == nil || !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("unredacted error = %v", err)
	}

	private := testSnapshot(1, time.Unix(1_700_000_000, 0).UTC())
	modePath := writeSnapshot(t, "mode.json", private)
	if err := os.Chmod(modePath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSnapshot(modePath); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("mode error = %v", err)
	}
}

func TestReadSnapshotRequiresRunningGraphState(t *testing.T) {
	value := testSnapshot(1, time.Unix(1_700_000_000, 0).UTC())
	value.State = "active"
	path := writeSnapshot(t, "active.json", value)
	if _, _, err := readSnapshot(path); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("non-running state error = %v", err)
	}
}

func TestValidateRejectsDistinctRuntimeGraphsAndSessionReuse(t *testing.T) {
	browser := testSnapshot(1, time.Unix(1_700_000_000, 0).UTC())
	native := testSnapshot(2, time.Unix(1_700_000_001, 0).UTC())
	native.Nodes["node"].Resolution.Runtime.Revision = "2"
	browserPath := writeSnapshot(t, "browser.json", browser)
	nativePath := writeSnapshot(t, "native.json", native)

	_, err := validate(options{
		browserPath: browserPath, browserSession: "sess_browser",
		nativePath: nativePath, nativeSession: "sess_native",
		invocationNonce:      strings.Repeat("f", 64),
		serverIdentityDigest: "sha256:" + strings.Repeat("e", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "unchanged runtime graph") {
		t.Fatalf("runtime mismatch error = %v", err)
	}
	_, err = validate(options{
		browserPath: browserPath, browserSession: "sess_reused",
		nativePath: nativePath, nativeSession: "sess_reused",
		invocationNonce:      strings.Repeat("f", 64),
		serverIdentityDigest: "sha256:" + strings.Repeat("e", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("session reuse error = %v", err)
	}
}

func testSnapshot(sequence uint64, observedAt time.Time) inspect.Live {
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       "companion",
		GraphRevision: 1,
		Fingerprint:   "sha256:" + strings.Repeat("a", 64),
		Configuration: &inspect.ArtifactIdentity{
			ID: "values://companion", Revision: "1", Digest: "sha256:" + strings.Repeat("b", 64),
		},
		Sequence: sequence, ObservedAt: observedAt, State: "running",
		Nodes: map[string]inspect.NodeLive{
			"node": {
				State: "mounted",
				Resolution: &inspect.NodeResolution{
					Element: element.Identity{
						Name: "test.Companion", Revision: 1,
						Digest: "sha256:" + strings.Repeat("c", 64),
					},
					Implementation: "test.companion.runtime",
					Runtime: inspect.ArtifactIdentity{
						ID: "runtime://companion", Revision: "1",
						Digest: "sha256:" + strings.Repeat("d", 64),
					},
					RuntimeEvidence: inspect.EvidenceLive,
				},
			},
		},
	}
}

func writeSnapshot(t *testing.T, name string, value inspect.Live) string {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(privateTempDir(t), name)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
