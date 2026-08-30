package scenarioconversation

import (
	"context"
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestSessionPlaybackSinkFreezesAudioProjectionForReservedUtterance(t *testing.T) {
	presentation := newPresentationState(legacy.Settings{Modalities: []string{"audio"}})
	client := &playbackClientSink{}
	descriptor := scenarioPlaybackDescriptor()
	sink := newSessionPlaybackSink(context.Background(), client, descriptor, presentation)
	utterance := action.Utterance{ID: "utterance_1", Text: "hello"}
	frame := action.Frame{PCM16LE: []byte{1, 0}, SampleRateHz: 24_000}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Audio(context.Background(), utterance, frame); err != nil {
		t.Fatal(err)
	}
	presentation.Update(legacy.Settings{Modalities: []string{"text"}})
	if err := sink.Audio(context.Background(), utterance, frame); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, action.Outcome{Completed: true}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.reserved != 1 || client.begun != 1 || client.text != "hello" ||
		client.audio != 2 || client.ended != 1 || client.turnBegun != 1 || client.turnEnded != 1 {
		t.Fatalf("playback projection = reserved %d begun %d text %q audio %d ended %d",
			client.reserved, client.begun, client.text, client.audio, client.ended)
	}
	actual := sink.Descriptor()
	actual.Capabilities["mutated"] = true
	if maps.Equal(actual.Capabilities, sink.Descriptor().Capabilities) {
		t.Fatal("playback descriptor exposed mutable capability state")
	}
}

func TestSessionPlaybackSinkFreezesTextOnlyProjectionForReservedUtterance(t *testing.T) {
	presentation := newPresentationState(legacy.Settings{Modalities: []string{"text"}})
	client := &playbackClientSink{}
	sink := newSessionPlaybackSink(context.Background(), client, scenarioPlaybackDescriptor(), presentation)
	utterance := action.Utterance{ID: "utterance_text", Text: "hello"}
	frame := action.Frame{PCM16LE: []byte{1, 0}, SampleRateHz: 24_000}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	presentation.Update(legacy.Settings{Modalities: []string{"audio"}})
	if err := sink.Audio(context.Background(), utterance, frame); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, action.Outcome{Completed: true}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.audio != 0 || client.begun != 1 || client.ended != 1 ||
		client.turnBegun != 1 || client.turnEnded != 1 {
		t.Fatalf("text-only projection = audio %d speech %d/%d turns %d/%d",
			client.audio, client.begun, client.ended, client.turnBegun, client.turnEnded)
	}
}

func TestSessionPlaybackSinkBalancesBeginWhenTextProjectionFails(t *testing.T) {
	client := &playbackClientSink{textErr: errors.New("text failed")}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	err := sink.Begin(context.Background(), action.Utterance{ID: "utterance_1", Text: "hello"})
	if err == nil || !errors.Is(err, client.textErr) {
		t.Fatalf("begin error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.begun != 1 || client.ended != 1 {
		t.Fatalf("unbalanced failed begin = begun %d ended %d", client.begun, client.ended)
	}
}

func TestSessionPlaybackSinkForwardsReservationCancellation(t *testing.T) {
	client := &playbackClientSink{}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{ID: "utterance_1", Text: "hello"}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	sink.CancelReservation(utterance)
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.reserved != 1 || client.reservationCanceled != 1 {
		t.Fatalf("reservation lifecycle = %d/%d", client.reserved, client.reservationCanceled)
	}
}

type playbackClientSink struct {
	mu                  sync.Mutex
	reserved            int
	reservationCanceled int
	begun               int
	text                string
	audio               int
	ended               int
	textErr             error
	turnBegun           int
	turnEnded           int
}

func (sink *playbackClientSink) TurnBegin(context.Context) error {
	sink.mu.Lock()
	sink.turnBegun++
	sink.mu.Unlock()
	return nil
}
func (sink *playbackClientSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	sink.mu.Lock()
	sink.turnEnded++
	sink.mu.Unlock()
	return nil
}
func (*playbackClientSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (*playbackClientSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (*playbackClientSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *playbackClientSink) SpeechBegin(_ context.Context, _ action.Utterance) error {
	sink.mu.Lock()
	sink.begun++
	sink.mu.Unlock()
	return nil
}
func (sink *playbackClientSink) SpeechText(_ context.Context, _ action.Utterance, text string) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.text += text
	return sink.textErr
}
func (sink *playbackClientSink) SpeechAudio(_ context.Context, _ action.Utterance, _ action.Frame) error {
	sink.mu.Lock()
	sink.audio++
	sink.mu.Unlock()
	return nil
}
func (sink *playbackClientSink) SpeechEnd(_ context.Context, _ action.Utterance, _ action.Outcome) error {
	sink.mu.Lock()
	sink.ended++
	sink.mu.Unlock()
	return nil
}
func (*playbackClientSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (*playbackClientSink) Failed(context.Context, legacy.ErrorEvent)             {}
func (sink *playbackClientSink) SpeechReserved(_ context.Context, _ action.Utterance) error {
	sink.mu.Lock()
	sink.reserved++
	sink.mu.Unlock()
	return nil
}
func (sink *playbackClientSink) SpeechReservationCancelled(_ context.Context, _ action.Utterance) {
	sink.mu.Lock()
	sink.reservationCanceled++
	sink.mu.Unlock()
}

var _ legacy.Sink = (*playbackClientSink)(nil)
var _ legacy.SpeechReservationSink = (*playbackClientSink)(nil)
