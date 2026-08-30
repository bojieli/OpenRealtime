package graphs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var referenceASRDescriptor = v1.Descriptor{
	Name: "reference-asr", Version: "1",
	Capabilities: v1.Capabilities{
		v1.CapabilityStreamingInput: true,
		v1.CapabilityRevisions:      true,
		v1.CapabilityCancellation:   true,
	},
}

func TestASRTrajectoryReferenceArtifactRunsLockedAcrossUtterances(t *testing.T) {
	directory := filepath.Join("components", "asr-trajectory")
	topology := readFile(t, filepath.Join(directory, "agent.ortg"))
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(readFile(t, filepath.Join(directory, "openrealtime.lock")))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesDocument, err := graphvalues.ParseYAML("agent.values.yaml",
		readFile(t, filepath.Join(directory, "agent.values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, valuesDocument)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Graph.Fingerprint == compiled.Graph.Fingerprint {
		t.Fatal("binding element values did not change executable graph identity")
	}

	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("deployment.asr", referenceASRDescriptor,
		func() (v1.PerceptionProvider, error) { return &referenceASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(perceptionelements.ASRProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Services: services, Values: bound.Values,
		Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	defer func() {
		cancelRun()
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("reference graph shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("reference graph did not stop")
		}
	}()

	resolved := egress(t, mounted, "provider_resolution")
	resolution := receive(t, resolved).Payload.(perceptionelements.ProviderResolution)
	if resolution.Reference != "deployment.asr" ||
		!reflect.DeepEqual(resolution.Descriptor, referenceASRDescriptor) {
		t.Fatalf("live resolution = %+v", resolution)
	}
	snapshots := egress(t, mounted, "snapshot")
	if seed := receive(t, snapshots).Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("trajectory seed = %+v", seed)
	}
	perceptionOutcomes := egress(t, mounted, "perception_outcome")
	commitOutcomes := egress(t, mounted, "commit_outcome")
	observe := ingress(t, mounted, "observe")
	flush := ingress(t, mounted, "flush")

	for utterance := 1; utterance <= 2; utterance++ {
		streamID := "utterance-" + strconv.Itoa(utterance)
		const sessionID = "session-asr-reference"
		frame := coreperception.Frame{
			Kind: coreperception.FrameAudio, Source: "microphone", CapturedNS: uint64(utterance * 100),
			PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 16_000,
		}
		if _, err := observe.Broadcast(context.Background(), element.Envelope{
			Type:   element.Trigger(element.Named("audio.FrameBatch")),
			ItemID: "observe-" + streamID, SessionID: sessionID,
			SourceID: streamID, CancellationScope: streamID,
			Payload: perceptionelements.AudioBatch{StreamID: streamID, Frames: []coreperception.Frame{frame}},
		}); err != nil {
			t.Fatal(err)
		}
		partialSnapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
		if got := receive(t, perceptionOutcomes).Payload.(perceptionelements.Outcome); got.Kind != perceptionelements.OutcomeSucceeded {
			t.Fatalf("partial ASR outcome = %+v", got)
		}
		if got := receive(t, commitOutcomes).Payload.(stateelements.ObservationCommitOutcome); got.Kind != stateelements.ObservationCommitted {
			t.Fatalf("partial commit outcome = %+v", got)
		}
		if _, err := flush.Broadcast(context.Background(), element.Envelope{
			Type: element.Trigger(element.Named("audio.Flush")), ItemID: "flush-" + streamID,
			SessionID: sessionID, SourceID: streamID, CancellationScope: streamID,
			Payload: perceptionelements.Flush{StreamID: streamID},
		}); err != nil {
			t.Fatal(err)
		}
		finalSnapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
		if finalSnapshot.Version != partialSnapshot.Version+1 {
			t.Fatalf("utterance %d versions = %d -> %d", utterance, partialSnapshot.Version, finalSnapshot.Version)
		}
		if got := receive(t, perceptionOutcomes).Payload.(perceptionelements.Outcome); got.Kind != perceptionelements.OutcomeSucceeded {
			t.Fatalf("final ASR outcome = %+v", got)
		}
		if got := receive(t, commitOutcomes).Payload.(stateelements.ObservationCommitOutcome); got.Kind != stateelements.ObservationCommitted {
			t.Fatalf("final commit outcome = %+v", got)
		}
	}
	final := receiveSnapshotVersion(t, mounted, 4)
	_ = final
}

// receiveSnapshotVersion verifies the final version through the mounted graph
// identity without consuming another state event. It intentionally consults
// live queue evidence only; all four state events were already observed above.
func receiveSnapshotVersion(t *testing.T, mounted *graphruntime.Mounted, want uint64) uint64 {
	t.Helper()
	live := mounted.Live()
	edge, found := live.Edges["boundary:snapshot"]
	if !found || edge.Dequeued != want+1 { // seeded version zero plus each commit
		t.Fatalf("snapshot queue evidence = %+v, want %d dequeues", edge, want+1)
	}
	return want
}

type referenceASR struct{}

func (*referenceASR) Descriptor() v1.Descriptor {
	descriptor := referenceASRDescriptor
	descriptor.Capabilities = map[v1.Capability]bool{}
	for capability, enabled := range referenceASRDescriptor.Capabilities {
		descriptor.Capabilities[capability] = enabled
	}
	return descriptor
}
func (*referenceASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return []v1.PerceptionRevision{{RevisionID: 1, StableText: "hello"}}, nil
}
func (*referenceASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, StableText: "hello world", Final: true}, nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func ingress(t *testing.T, mounted *graphruntime.Mounted, name string) element.OutputPort {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func egress(t *testing.T, mounted *graphruntime.Mounted, name string) element.InputPort {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func receive(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
