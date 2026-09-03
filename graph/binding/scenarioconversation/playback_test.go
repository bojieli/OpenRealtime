package scenarioconversation

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
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
	if client.turnEnded != 0 {
		client.mu.Unlock()
		t.Fatal("playback End exposed TurnEnd before the graph release barrier")
	}
	client.mu.Unlock()
	releaseSessionPlayback(t, sink, "run_1", utterance, action.Outcome{Completed: true})
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
	releaseSessionPlayback(t, sink, "run_text", utterance, action.Outcome{Completed: true})
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

func TestSessionPlaybackReleaseRequiresExactPendingEffectAndIsOnceOnly(t *testing.T) {
	client := &playbackClientSink{}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{
		ID: "utterance_exact", Text: "exact release", AssistantItemIDs: []string{"assistant_1"},
	}
	outcome := action.Outcome{Completed: true, PlayedMS: 25}
	missing := speechelements.PlaybackReceipt{
		Kind: speechelements.PlaybackReleased, Sequence: 4,
		Utterance: action.Utterance{ID: "utterance_missing", Text: "missing"},
		Outcome:   outcome,
	}
	if err := sink.Release(context.Background(), "run_exact", missing); err == nil ||
		!strings.Contains(err.Error(), "no pending sink outcome") {
		t.Fatalf("missing release error = %v", err)
	}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, outcome); err != nil {
		t.Fatal(err)
	}
	mismatched := speechelements.PlaybackReceipt{
		Kind: speechelements.PlaybackReleased, Sequence: 6,
		Utterance: clonePlaybackUtterance(utterance), Outcome: action.Outcome{Completed: true, PlayedMS: 26},
	}
	if err := sink.Release(context.Background(), "run_exact", mismatched); err == nil ||
		!strings.Contains(err.Error(), "changed its sink effect") {
		t.Fatalf("mismatched release error = %v", err)
	}
	receipt := mismatched
	receipt.Outcome = outcome
	if err := sink.Release(context.Background(), "run_exact", receipt); err != nil {
		t.Fatal(err)
	}
	if err := sink.Release(context.Background(), "run_exact", receipt); err == nil ||
		!strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("duplicate release error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.turnEnded != 1 {
		t.Fatalf("exact release TurnEnd calls = %d, want 1", client.turnEnded)
	}
}

func TestSessionPlaybackReleasePreservesIncompleteOutcome(t *testing.T) {
	client := &playbackClientSink{}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{ID: "utterance_cancelled", Text: "partially heard"}
	outcome := action.Outcome{PlayedMS: 15, Reason: "  user barge-in  "}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, outcome); err != nil {
		t.Fatal(err)
	}
	releaseSessionPlayback(t, sink, "run_cancelled", utterance, outcome)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.turnOutcomes) != 1 || !client.turnOutcomes[0].Incomplete ||
		client.turnOutcomes[0].Detail != "user barge-in" {
		t.Fatalf("incomplete turn outcome = %+v", client.turnOutcomes)
	}
}

func TestSessionPlaybackReleaseConsumesBeforeTurnEndError(t *testing.T) {
	turnErr := errors.New("turn end failed")
	client := &playbackClientSink{turnErr: turnErr}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{ID: "utterance_error", Text: "release error"}
	outcome := action.Outcome{Completed: true}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, outcome); err != nil {
		t.Fatal(err)
	}
	receipt := speechelements.PlaybackReceipt{
		Kind: speechelements.PlaybackReleased, Sequence: 6,
		Utterance: clonePlaybackUtterance(utterance), Outcome: outcome,
	}
	if err := sink.Release(context.Background(), "run_error", receipt); !errors.Is(err, turnErr) {
		t.Fatalf("TurnEnd release error = %v", err)
	}
	if err := sink.Release(context.Background(), "run_error", receipt); err == nil ||
		!strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("release retry after TurnEnd error = %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.turnEnded != 1 {
		t.Fatalf("failed TurnEnd calls = %d, want 1", client.turnEnded)
	}
}

func TestSessionPlaybackSinkCloseBalancesPendingRelease(t *testing.T) {
	client := &playbackClientSink{}
	sink := newSessionPlaybackSink(context.Background(), client,
		scenarioPlaybackDescriptor(), newPresentationState(legacy.Settings{}))
	utterance := action.Utterance{ID: "utterance_shutdown", Text: "finished locally"}
	if err := sink.Reserve(utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.Begin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.End(context.Background(), utterance, action.Outcome{Completed: true}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.ended != 1 || client.turnEnded != 1 || len(client.turnOutcomes) != 1 ||
		!client.turnOutcomes[0].Incomplete ||
		client.turnOutcomes[0].Detail != "playback release not observed before sink closed" {
		t.Fatalf("pending release shutdown = speech ends %d turn outcomes %+v",
			client.ended, client.turnOutcomes)
	}
}

func releaseSessionPlayback(
	t *testing.T, sink *sessionPlaybackSink, runID string,
	utterance action.Utterance, outcome action.Outcome,
) {
	t.Helper()
	err := sink.Release(context.Background(), runID, speechelements.PlaybackReceipt{
		Kind: speechelements.PlaybackReleased, Sequence: 6,
		Utterance: clonePlaybackUtterance(utterance), Outcome: outcome,
	})
	if err != nil {
		t.Fatal(err)
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
	turnOutcomes        []legacy.TurnOutcome
	turnErr             error
}

func (sink *playbackClientSink) TurnBegin(context.Context) error {
	sink.mu.Lock()
	sink.turnBegun++
	sink.mu.Unlock()
	return nil
}
func (sink *playbackClientSink) TurnEnd(_ context.Context, outcome legacy.TurnOutcome) error {
	sink.mu.Lock()
	sink.turnEnded++
	sink.turnOutcomes = append(sink.turnOutcomes, outcome)
	err := sink.turnErr
	sink.mu.Unlock()
	return err
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
