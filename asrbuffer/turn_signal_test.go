package asrbuffer

import (
	"context"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

type turnSignalProvider struct {
	endpointed, eager bool
}

func (*turnSignalProvider) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "turn-signal", Version: "1", Capabilities: v1.Capabilities{}}
}
func (*turnSignalProvider) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (*turnSignalProvider) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{Final: true}, nil
}
func (provider *turnSignalProvider) SpeechEndpointed() bool { return provider.endpointed }
func (provider *turnSignalProvider) EagerEndOfTurn() bool   { return provider.eager }

// The runtime reads a recogniser's turn signals from whatever it holds, and in
// the served room it holds this buffer.
func TestBufferForwardsTheRecognisersTurnSignals(t *testing.T) {
	provider := &turnSignalProvider{}
	buffer, err := New(Config{Provider: provider, MinimumChunk: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var held v1.PerceptionProvider = buffer
	endpointed, forwardsEndpoint := held.(interface{ SpeechEndpointed() bool })
	eager, forwardsEager := held.(interface{ EagerEndOfTurn() bool })
	if !forwardsEndpoint || !forwardsEager {
		t.Fatalf("buffer hides turn signals: endpoint %v eager %v", forwardsEndpoint, forwardsEager)
	}
	if endpointed.SpeechEndpointed() || eager.EagerEndOfTurn() {
		t.Fatal("buffer invented a turn signal the recogniser did not give")
	}
	provider.eager = true
	if !eager.EagerEndOfTurn() || endpointed.SpeechEndpointed() {
		t.Fatal("the recogniser's eager end of turn did not pass through")
	}
	provider.eager, provider.endpointed = false, true
	if !endpointed.SpeechEndpointed() || eager.EagerEndOfTurn() {
		t.Fatal("the recogniser's end of turn did not pass through")
	}

	// A recogniser without turn detection reports neither.
	plain, err := New(Config{Provider: plainProvider{}, MinimumChunk: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SpeechEndpointed() || plain.EagerEndOfTurn() {
		t.Fatal("a recogniser without turn detection reported a turn signal")
	}
}

type plainProvider struct{}

func (plainProvider) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "plain", Version: "1", Capabilities: v1.Capabilities{}}
}
func (plainProvider) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (plainProvider) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{Final: true}, nil
}
