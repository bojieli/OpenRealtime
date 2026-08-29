package runtime

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestTraceRecorderCopiesAndErasesItsSessionKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x91}, traceSessionKeyBytes)
	configuration := inspect.ArtifactIdentity{
		ID: "values://internal", Revision: "config:1",
		Digest: "sha256:" + strings.Repeat("1", 64),
	}
	graph := ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "internal", Revision: 1,
		Fingerprint: "sha256:" + strings.Repeat("2", 64),
		Nodes:       []ir.Node{{ID: "node"}},
	}
	recorder, err := newTraceRecorder(
		graph, &configuration,
		InspectionConfig{MaxFlows: 1, MaxEdgesPerFlow: 1, MaxCorrelationBytes: 16},
		&TraceRecordingConfig{
			SessionCorrelationKey: key, MaxRetainedBytes: 4096,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	owned := recorder.key
	key[0] = 0
	if owned[0] != 0x91 {
		t.Fatal("recorder retained the caller's mutable key slice")
	}
	recorder.discard()
	for index, value := range owned {
		if value != 0 {
			t.Fatalf("owned session key byte %d was not erased", index)
		}
	}
	if !recorder.sealed || recorder.key != nil || recorder.active != nil {
		t.Fatalf("discarded recorder retained sensitive state: %+v", recorder)
	}
}
