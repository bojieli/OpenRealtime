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

func TestTraceRecorderCachesOnlySessionPseudonymizedCausalIdentities(t *testing.T) {
	const rawItem = "private-item"
	const rawParent = "private-parent"
	recorder := &traceRecorder{
		key:    bytes.Repeat([]byte{0x73}, traceSessionKeyBytes),
		active: make(map[string]traceCorrelation),
	}
	recorder.mu.Lock()
	flows := recorder.pseudonymizeFlowsLocked(map[string]inspect.FlowLive{
		"trace:private": {
			Correlation: "trace:private", Edges: []string{"edge"}, EdgeNS: []uint64{1},
			CausalStages: []inspect.CausalStageLive{{Item: rawItem, Parents: []string{rawParent}}},
			FirstNS:      1, LastNS: 1,
		},
	})
	recorder.mu.Unlock()
	if len(flows) != 1 || len(recorder.active) != 1 {
		t.Fatalf("pseudonymized flow/cache population = %+v / %+v", flows, recorder.active)
	}
	for _, flow := range flows {
		stage := flow.CausalStages[0]
		if stage.Item == rawItem || stage.Parents[0] == rawParent ||
			!strings.HasPrefix(stage.Item, "hmac-sha256:") ||
			!strings.HasPrefix(stage.Parents[0], "hmac-sha256:") {
			t.Fatalf("export candidate retained raw causal identities: %+v", stage)
		}
	}
	for _, cached := range recorder.active {
		stage := cached.causalStages[0]
		if stage.Item == rawItem || stage.Parents[0] == rawParent ||
			!strings.HasPrefix(stage.Item, "hmac-sha256:") ||
			!strings.HasPrefix(stage.Parents[0], "hmac-sha256:") {
			t.Fatalf("recorder cache retained raw causal identities: %+v", stage)
		}
	}
}
