package perception_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

// This provider deliberately returns a valid revision after observing the
// cancellation. The runtime owns the decision whether that result may publish.
type lateCancellationASR struct {
	entered chan struct{}
	flush   bool
	closed  atomic.Int32
}

func (*lateCancellationASR) Descriptor() v1.Descriptor { return cloneDescriptor(testASRDescriptor) }
func (provider *lateCancellationASR) PushFrame(ctx context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	if provider.flush {
		return nil, nil
	}
	close(provider.entered)
	<-ctx.Done()
	return []v1.PerceptionRevision{{RevisionID: 1, StableText: "withdrawn words"}}, nil
}
func (provider *lateCancellationASR) Finalize(ctx context.Context, _ uint64) (v1.PerceptionRevision, error) {
	close(provider.entered)
	<-ctx.Done()
	return v1.PerceptionRevision{RevisionID: 1, StableText: "withdrawn words", Final: true}, nil
}
func (provider *lateCancellationASR) Close() error { provider.closed.Add(1); return nil }

func asrCancellationAudio(item, session, stream string) element.Envelope {
	return element.Envelope{
		Type: element.Trigger(element.Named("audio.FrameBatch")), ItemID: item,
		SessionID: session, SourceID: stream, CancellationScope: stream,
		Payload: perceptionelements.AudioBatch{StreamID: stream, Frames: []coreperception.Frame{{
			Kind: coreperception.FrameAudio, Source: "microphone", PCM16LE: []byte{1, 0}, SampleRateHz: 16000,
		}}},
	}
}
func asrCancellationRequest(item, session, stream string) element.Envelope {
	return element.Envelope{
		Type: element.Interrupt(element.Named("audio.StreamID")), ItemID: item,
		SessionID: session, SourceID: stream, CancellationScope: stream,
		Payload: perceptionelements.Cancel{StreamID: stream, Reason: "utterance withdrawn"},
	}
}

func TestASRCanceledProviderResultCannotPublishObservation(t *testing.T) {
	for _, flushOperation := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush-%t", flushOperation), func(t *testing.T) {
			provider := &lateCancellationASR{entered: make(chan struct{}), flush: flushOperation}
			providers := perceptionelements.NewASRProviderRegistry()
			if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) { return provider, nil }); err != nil {
				t.Fatal(err)
			}
			mounted, done, cancel := mountASR(t, providers)
			defer stopASR(t, mounted, done, cancel)
			resolved, _ := mounted.Egress("resolved")
			_ = receive(t, resolved)
			observe, _ := mounted.Ingress("observe")
			flush, _ := mounted.Ingress("flush")
			interrupt, _ := mounted.Ingress("cancel")
			outcome, _ := mounted.Egress("outcome")
			observations, _ := mounted.Egress("observations")
			sendGate(t, observe, asrCancellationAudio("initial", "session-a", "withdrawn"))
			if flushOperation {
				_ = receive(t, outcome)
				sendGate(t, flush, element.Envelope{
					Type: element.Trigger(element.Named("audio.Flush")), ItemID: "flush", SessionID: "session-a", SourceID: "withdrawn",
					Payload: perceptionelements.Flush{StreamID: "withdrawn"},
				})
			}
			select {
			case <-provider.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not enter")
			}
			sendGate(t, interrupt, asrCancellationRequest("cancel", "session-a", "withdrawn"))
			terminal := receive(t, outcome).Payload.(perceptionelements.Outcome)
			if terminal.Kind != perceptionelements.OutcomeCanceled || terminal.ObservationCount != 0 {
				t.Fatalf("canceled provider result published observations: %+v", terminal)
			}
			assertNoGateEnvelope(t, observations)
			if provider.closed.Load() != 1 {
				t.Fatalf("provider close count = %d", provider.closed.Load())
			}
		})
	}
}

func TestASRCanceledStreamRejectsDelayedBatchesAndFlushes(t *testing.T) {
	var created atomic.Int32
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) { created.Add(1); return &scriptedASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountASR(t, providers)
	defer stopASR(t, mounted, done, cancel)
	resolved, _ := mounted.Egress("resolved")
	_ = receive(t, resolved)
	observe, _ := mounted.Ingress("observe")
	flush, _ := mounted.Ingress("flush")
	interrupt, _ := mounted.Ingress("cancel")
	outcome, _ := mounted.Egress("outcome")
	observations, _ := mounted.Egress("observations")
	sendGate(t, interrupt, asrCancellationRequest("cancel", "session-a", "withdrawn"))
	_ = receive(t, outcome)
	for index := 0; index < 2; index++ {
		sendGate(t, observe, asrCancellationAudio(fmt.Sprintf("late-%d", index), "session-a", "withdrawn"))
		if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled || terminal.ObservationCount != 0 {
			t.Fatalf("canceled stream reopened: %+v", terminal)
		}
	}
	sendGate(t, flush, element.Envelope{
		Type: element.Trigger(element.Named("audio.Flush")), ItemID: "late-flush", SessionID: "session-a", SourceID: "withdrawn",
		CausalParents: []string{"never-arriving-batch"}, Payload: perceptionelements.Flush{StreamID: "withdrawn", AfterItemID: "never-arriving-batch"},
	})
	if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
		t.Fatalf("canceled endpoint waited for a batch: %+v", terminal)
	}
	assertNoGateEnvelope(t, observations)
	for index, control := range []struct{ session, stream string }{{"session-b", "withdrawn"}, {"session-a", "replacement"}} {
		id := fmt.Sprintf("fresh-%d", index)
		sendGate(t, observe, asrCancellationAudio(id, control.session, control.stream))
		if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded {
			t.Fatalf("fresh stream refused: %+v", terminal)
		}
		if observation := receive(t, observations); observation.SessionID != control.session {
			t.Fatalf("observation crossed session: %+v", observation)
		}
		sendGate(t, flush, element.Envelope{
			Type: element.Trigger(element.Named("audio.Flush")), ItemID: id + "-flush", SessionID: control.session, SourceID: control.stream,
			Payload: perceptionelements.Flush{StreamID: control.stream},
		})
		if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded {
			t.Fatalf("fresh endpoint refused: %+v", terminal)
		}
		_ = receive(t, observations)
	}
	// The registry's initial attestation primes one provider, which can be
	// reused for the first fresh stream. The canceled stream must create none.
	if created.Load() != 2 {
		t.Fatalf("canceled stream recreated provider: %d instances", created.Load())
	}
}

type controlledCancellationASR struct {
	entered, release chan struct{}
	canceled         atomic.Bool
}

func (*controlledCancellationASR) Descriptor() v1.Descriptor {
	return cloneDescriptor(testASRDescriptor)
}
func (provider *controlledCancellationASR) PushFrame(ctx context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	close(provider.entered)
	select {
	case <-ctx.Done():
		provider.canceled.Store(true)
		return nil, ctx.Err()
	case <-provider.release:
		return []v1.PerceptionRevision{{RevisionID: 1, StableText: "current words"}}, nil
	}
}
func (*controlledCancellationASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, StableText: "current words", Final: true}, nil
}

func TestASRInactiveCancellationPreservesProviderAndRemembersTarget(t *testing.T) {
	for _, target := range []struct{ name, session, stream string }{
		{"another-session", "session-b", "current"},
		{"another-stream", "session-a", "future"},
		{"foreign-unaddressed", "session-b", ""},
	} {
		t.Run(target.name, func(t *testing.T) {
			provider := &controlledCancellationASR{entered: make(chan struct{}), release: make(chan struct{})}
			providers := perceptionelements.NewASRProviderRegistry()
			if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) { return provider, nil }); err != nil {
				t.Fatal(err)
			}
			mounted, done, cancel := mountASR(t, providers)
			defer stopASR(t, mounted, done, cancel)
			resolved, _ := mounted.Egress("resolved")
			_ = receive(t, resolved)
			observe, _ := mounted.Ingress("observe")
			interrupt, _ := mounted.Ingress("cancel")
			flush, _ := mounted.Ingress("flush")
			outcome, _ := mounted.Egress("outcome")
			observations, _ := mounted.Egress("observations")
			sendGate(t, observe, asrCancellationAudio("initial", "session-a", "current"))
			select {
			case <-provider.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not enter")
			}
			sendGate(t, interrupt, asrCancellationRequest("inactive-cancel", target.session, target.stream))
			receipt := receive(t, outcome).Payload.(perceptionelements.Outcome)
			if receipt.Operation != "cancel" {
				t.Fatalf("inactive cancellation stopped provider: %+v", receipt)
			}
			if target.stream != "" && receipt.Kind != perceptionelements.OutcomeCanceled {
				t.Fatalf("future cancellation not recorded: %+v", receipt)
			}
			if target.stream == "" && receipt.Kind != perceptionelements.OutcomeIgnored {
				t.Fatalf("unaddressed foreign cancellation = %+v", receipt)
			}
			close(provider.release)
			if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded || provider.canceled.Load() {
				t.Fatalf("current utterance canceled: %+v", terminal)
			}
			_ = receive(t, observations)
			sendGate(t, flush, element.Envelope{Type: element.Trigger(element.Named("audio.Flush")), ItemID: "current-flush", SessionID: "session-a", SourceID: "current", Payload: perceptionelements.Flush{StreamID: "current"}})
			_ = receive(t, outcome)
			_ = receive(t, observations)
			if target.stream != "" {
				sendGate(t, observe, asrCancellationAudio("target-arrives", target.session, target.stream))
				if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
					t.Fatalf("inactive cancellation was lost: %+v", terminal)
				}
				assertNoGateEnvelope(t, observations)
			}
		})
	}
}

func TestASRCancellationMemoryEvictsOldestStreamWithoutConsumingRetainedStreams(t *testing.T) {
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) { return &scriptedASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountASR(t, providers)
	defer stopASR(t, mounted, done, cancel)
	resolved, _ := mounted.Egress("resolved")
	_ = receive(t, resolved)
	interrupt, _ := mounted.Ingress("cancel")
	observe, _ := mounted.Ingress("observe")
	outcome, _ := mounted.Egress("outcome")
	observations, _ := mounted.Egress("observations")
	// Cancellation keeps the most recent 512 distinct session/stream addresses.
	for index := 0; index <= 512; index++ {
		stream := fmt.Sprintf("stream-%d", index)
		sendGate(t, interrupt, asrCancellationRequest("cancel-"+stream, "session-a", stream))
		_ = receive(t, outcome)
	}
	for index, stream := range []string{"stream-1", "stream-1", "stream-512"} {
		sendGate(t, observe, asrCancellationAudio(fmt.Sprintf("late-%d", index), "session-a", stream))
		if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
			t.Fatalf("retained cancellation lost: %+v", terminal)
		}
	}
	sendGate(t, observe, asrCancellationAudio("evicted-cancel", "session-a", "stream-0"))
	if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded {
		t.Fatalf("oldest cancellation did not evict: %+v", terminal)
	}
	_ = receive(t, observations)
	assertNoGateEnvelope(t, observations)
}

func TestASRCancellationClearsDeferredEndpoint(t *testing.T) {
	providers := perceptionelements.NewASRProviderRegistry()
	if err := providers.Register("primary", testASRDescriptor, func() (v1.PerceptionProvider, error) { return &scriptedASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountASR(t, providers)
	defer stopASR(t, mounted, done, cancel)
	resolved, _ := mounted.Egress("resolved")
	_ = receive(t, resolved)
	flush, _ := mounted.Ingress("flush")
	interrupt, _ := mounted.Ingress("cancel")
	observe, _ := mounted.Ingress("observe")
	outcome, _ := mounted.Egress("outcome")
	observations, _ := mounted.Egress("observations")
	for _, id := range []string{"first", "barrier"} {
		sendGate(t, flush, element.Envelope{Type: element.Trigger(element.Named("audio.Flush")), ItemID: id, SessionID: "session-a", SourceID: "withdrawn", CausalParents: []string{id + "-batch"}, Payload: perceptionelements.Flush{StreamID: "withdrawn", AfterItemID: id + "-batch"}})
	}
	if refusal := receive(t, outcome).Payload.(perceptionelements.Outcome); refusal.Code != "flush_barrier_pending" {
		t.Fatalf("endpoint did not become pending: %+v", refusal)
	}
	sendGate(t, interrupt, asrCancellationRequest("cancel", "session-a", "withdrawn"))
	_ = receive(t, outcome)
	sendGate(t, observe, asrCancellationAudio("first-batch", "session-a", "withdrawn"))
	if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeCanceled {
		t.Fatalf("late batch restarted endpoint: %+v", terminal)
	}
	assertNoGateEnvelope(t, observations)
	sendGate(t, observe, asrCancellationAudio("fresh-batch", "session-a", "fresh"))
	if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded {
		t.Fatalf("new utterance blocked by canceled endpoint: %+v", terminal)
	}
	_ = receive(t, observations)
	sendGate(t, flush, element.Envelope{Type: element.Trigger(element.Named("audio.Flush")), ItemID: "fresh-flush", SessionID: "session-a", SourceID: "fresh", Payload: perceptionelements.Flush{StreamID: "fresh"}})
	if terminal := receive(t, outcome).Payload.(perceptionelements.Outcome); terminal.Kind != perceptionelements.OutcomeSucceeded {
		t.Fatalf("new endpoint failed: %+v", terminal)
	}
	_ = receive(t, observations)
	assertNoGateEnvelope(t, outcome)
}
