package graphs_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
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

type canceledReferenceASR struct {
	referenceASR
	entered chan struct{}
	flush   bool
}

func (provider *canceledReferenceASR) PushFrame(ctx context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if provider.flush {
		return nil, nil
	}
	close(provider.entered)
	<-ctx.Done()
	return []v1.PerceptionRevision{{RevisionID: 1, StableText: "withdrawn words"}}, nil
}
func (provider *canceledReferenceASR) Finalize(ctx context.Context, _ uint64) (v1.PerceptionRevision, error) {
	close(provider.entered)
	<-ctx.Done()
	return v1.PerceptionRevision{RevisionID: 1, StableText: "withdrawn words", Final: true}, nil
}

func TestASRTrajectoryCanceledProviderCannotCommitLateWords(t *testing.T) {
	for _, flushOperation := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush-%t", flushOperation), func(t *testing.T) {
			provider := &canceledReferenceASR{entered: make(chan struct{}), flush: flushOperation}
			providers := perceptionelements.NewASRProviderRegistry()
			var instances atomic.Int32
			if err := providers.Register("deployment.asr", referenceASRDescriptor, func() (v1.PerceptionProvider, error) {
				if instances.Add(1) == 1 {
					return provider, nil
				}
				return &referenceASR{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			store := trajectory.NewStore()
			mounted := mountASRCancellationReference(t, providers, store)
			_ = receive(t, egress(t, mounted, "provider_resolution"))
			_ = receive(t, egress(t, mounted, "snapshot"))
			outcomes := egress(t, mounted, "perception_outcome")
			send := func(port string, envelope element.Envelope) {
				t.Helper()
				result, err := ingress(t, mounted, port).Broadcast(t.Context(), envelope)
				if err != nil || result.Delivered != 1 {
					t.Fatalf("send %s: %+v, %v", port, result, err)
				}
			}
			audio := func(id, stream string) {
				t.Helper()
				send("observe", element.Envelope{
					Type: element.Trigger(element.Named("audio.FrameBatch")), ItemID: id, SessionID: "asr-cancel-session", SourceID: stream, CancellationScope: stream,
					Payload: perceptionelements.AudioBatch{StreamID: stream, Frames: []coreperception.Frame{{Kind: coreperception.FrameAudio, Source: "microphone", PCM16LE: []byte{1, 0}, SampleRateHz: 16000}}},
				})
			}
			flush := func(id, stream string) {
				t.Helper()
				send("flush", element.Envelope{Type: element.Trigger(element.Named("audio.Flush")), ItemID: id, SessionID: "asr-cancel-session", SourceID: stream, Payload: perceptionelements.Flush{StreamID: stream}})
			}
			audio("initial", "withdrawn")
			if flushOperation {
				_ = receive(t, outcomes)
				flush("initial-flush", "withdrawn")
			}
			select {
			case <-provider.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not enter")
			}
			send("cancel", element.Envelope{Type: element.Interrupt(element.Named("audio.StreamID")), ItemID: "cancel", SessionID: "asr-cancel-session", SourceID: "withdrawn", Payload: perceptionelements.Cancel{StreamID: "withdrawn", Reason: "withdrawn"}})
			if terminal := receive(t, outcomes).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
				t.Fatalf("cancel outcome = %+v", terminal)
			}
			// A fresh complete utterance supplies a downstream ordering barrier:
			// only its two revisions may enter canonical history.
			audio("fresh", "replacement")
			_ = receive(t, outcomes)
			flush("fresh-flush", "replacement")
			_ = receive(t, outcomes)
			for index, text := range []string{"hello", "hello world"} {
				snapshot := receive(t, egress(t, mounted, "snapshot")).Payload.(trajectory.Snapshot)
				if snapshot.Version != uint64(index+1) || snapshot.Items[index].Content != text {
					t.Fatalf("canceled provider text entered canonical history: %+v", snapshot)
				}
				if committed := receive(t, egress(t, mounted, "commit_outcome")).Payload.(stateelements.ObservationCommitOutcome); committed.Kind != stateelements.ObservationCommitted || committed.StreamID != "replacement" {
					t.Fatalf("unexpected canonical commit: %+v", committed)
				}
			}
			audio("late-batch", "withdrawn")
			if terminal := receive(t, outcomes).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
				t.Fatalf("late batch reopened stream: %+v", terminal)
			}
			flush("late-flush", "withdrawn")
			if terminal := receive(t, outcomes).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
				t.Fatalf("late flush reopened stream: %+v", terminal)
			}
			if snapshot := store.Snapshot(); snapshot.Version != 2 {
				t.Fatalf("unexpected canonical history: %+v", snapshot)
			}
			for _, name := range []string{"snapshot", "commit_outcome"} {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
				envelope, err := egress(t, mounted, name).Receive(ctx)
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("unexpected %s: %+v, %v", name, envelope, err)
				}
			}
		})
	}
}

func mountASRCancellationReference(t *testing.T, providers *perceptionelements.ASRProviderRegistry, store *trajectory.Store) *graphruntime.Mounted {
	t.Helper()
	directory := filepath.Join("components", "asr-trajectory")
	parsed, err := syntax.Parse("agent.ortg", readFile(t, filepath.Join(directory, "agent.ortg")))
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
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked})
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("agent.values.yaml", readFile(t, filepath.Join(directory, "agent.values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	for name, value := range map[string]any{perceptionelements.ASRProviderRegistryService: providers, stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{Store: store, SessionID: "asr-cancel-session"}} {
		if _, err := services.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(t.Context(), graphruntime.Config{Graph: bound.Graph, Values: bound.Values, Registry: registry, Services: services, Now: func() uint64 { return 42 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("ASR trajectory shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("ASR trajectory did not stop")
		}
	})
	return mounted
}
