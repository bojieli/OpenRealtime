package speech_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/spoken"
)

type receiptWordAligner struct{}

func (receiptWordAligner) Words(context.Context, spoken.Audio) ([]spoken.Word, error) {
	return []spoken.Word{
		{Text: "partly", StartMS: 0, EndMS: 1},
		{Text: "presented", StartMS: 1, EndMS: 2},
	}, nil
}

func TestPlaybackReceiptsAttestExactPostEffectOrder(t *testing.T) {
	sink := newReceiptSink("")
	fixture := mountSpeech(t,
		providerDescriptor, func() v1.SpeechProvider {
			return &scriptedProvider{chunks: testChunks("receipt", 1, 32)}
		},
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil,
	)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "receipt", Text: "audited output"})
	if outcome := receive(t, fixture.egress(t, "playback_outcome")).Payload.(speechelements.PlaybackOutcome); outcome.Kind != speechelements.OutcomeSucceeded {
		t.Fatalf("playback outcome = %+v", outcome)
	}

	wanted := []struct {
		boundary string
		kind     speechelements.PlaybackReceiptKind
	}{
		{"playback_reserved", speechelements.PlaybackReserved},
		{"playback_begun", speechelements.PlaybackBegun},
		{"playback_text_committed", speechelements.PlaybackTextCommitted},
		{"playback_audio_emitted", speechelements.PlaybackAudioEmitted},
		{"playback_ended", speechelements.PlaybackEnded},
		{"playback_released", speechelements.PlaybackReleased},
	}
	var previous string
	for index, expected := range wanted {
		envelope := receive(t, fixture.egress(t, expected.boundary))
		receipt, ok := envelope.Payload.(speechelements.PlaybackReceipt)
		if !ok || receipt.Kind != expected.kind || receipt.Sequence != uint64(index+1) ||
			receipt.Utterance.ID != "receipt" || receipt.Utterance.Text != "audited output" {
			t.Fatalf("%s receipt = %#v", expected.boundary, envelope.Payload)
		}
		if previous != "" && !slices.Contains(envelope.CausalParents, previous) {
			t.Fatalf("%s receipt parents %v omit prior receipt %q",
				expected.boundary, envelope.CausalParents, previous)
		}
		if expected.kind == speechelements.PlaybackAudioEmitted &&
			(len(receipt.Frame.PCM16LE) == 0 || receipt.Frame.SampleRateHz != 16_000) {
			t.Fatalf("audio receipt omitted exact frame: %+v", receipt.Frame)
		}
		if expected.kind == speechelements.PlaybackEnded && !receipt.Outcome.Completed {
			t.Fatalf("ended receipt omitted terminal outcome: %+v", receipt.Outcome)
		}
		previous = envelope.ItemID
	}
	if effects := sink.effectsSnapshot(); !slices.Equal(effects, []string{
		"reserve", "begin", "audio", "end",
	}) {
		t.Fatalf("sink effects = %v", effects)
	}
}

func TestPlaybackReceiptsExposeOnlySuccessfulEffectPrefixes(t *testing.T) {
	tests := []struct {
		name     string
		fail     string
		expected map[string]int
	}{
		{name: "reservation failed", fail: "reserve", expected: map[string]int{}},
		{name: "begin failed", fail: "begin", expected: map[string]int{
			"playback_reserved": 1, "playback_released": 1,
		}},
		{name: "audio failed", fail: "audio", expected: map[string]int{
			"playback_reserved": 1, "playback_begun": 1, "playback_text_committed": 1,
			"playback_ended": 1, "playback_released": 1,
		}},
		{name: "end failed", fail: "end", expected: map[string]int{
			"playback_reserved": 1, "playback_begun": 1, "playback_text_committed": 1,
			"playback_audio_emitted": 1,
		}},
	}
	boundaries := []string{
		"playback_reserved", "playback_begun", "playback_text_committed",
		"playback_audio_emitted", "playback_ended", "playback_released",
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sink := newReceiptSink(test.fail)
			fixture := mountSpeech(t,
				providerDescriptor, func() v1.SpeechProvider {
					return &scriptedProvider{chunks: testChunks("failed-receipt", 1, 32)}
				},
				sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil,
			)
			defer fixture.stop(t)
			fixture.receiveResolutions(t)
			fixture.sendText(t, speechelements.TextSegment{ID: "failed-receipt", Text: "failure prefix"})
			outcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "failed-receipt")
			if outcome.Kind != speechelements.OutcomeFailed {
				t.Fatalf("playback outcome = %+v", outcome)
			}
			for _, boundary := range boundaries {
				port := fixture.egress(t, boundary)
				if test.expected[boundary] == 1 {
					receipt := receive(t, port).Payload.(speechelements.PlaybackReceipt)
					if receipt.Kind == "" {
						t.Fatalf("%s emitted an empty receipt", boundary)
					}
				}
				assertNoPlaybackReceipt(t, port, boundary)
			}
		})
	}
}

func TestPlaybackReceiptsPreserveCancellationEffectPrefixes(t *testing.T) {
	t.Run("before first audio", func(t *testing.T) {
		provider := newBlockingProvider(false)
		sink := newReceiptSink("")
		fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
			sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
		defer fixture.stop(t)
		fixture.receiveResolutions(t)
		fixture.sendText(t, speechelements.TextSegment{ID: "cancel-before", Text: "not presented"})
		waitSignal(t, provider.entered, "provider did not begin")
		assertPlaybackReceipts(t, fixture, 1, "", []struct {
			boundary string
			kind     speechelements.PlaybackReceiptKind
		}{
			{"playback_reserved", speechelements.PlaybackReserved},
			{"playback_begun", speechelements.PlaybackBegun},
			{"playback_text_committed", speechelements.PlaybackTextCommitted},
		})
		fixture.sendCancel(t, "playback_cancel", "cancel-before", "barge-in")
		fixture.sendCancel(t, "tts_cancel", "cancel-before", "barge-in")
		outcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "cancel-before")
		if outcome.Kind != speechelements.OutcomeCancelled || outcome.CrossedBoundary {
			t.Fatalf("playback outcome = %+v", outcome)
		}
		assertPlaybackReceipts(t, fixture, 4, "barge-in", []struct {
			boundary string
			kind     speechelements.PlaybackReceiptKind
		}{
			{"playback_ended", speechelements.PlaybackEnded},
			{"playback_released", speechelements.PlaybackReleased},
		})
		assertNoReceiptBoundaries(t, fixture)
		if effects := sink.effectsSnapshot(); !slices.Equal(effects, []string{
			"reserve", "begin", "end",
		}) {
			t.Fatalf("sink effects = %v", effects)
		}
	})

	t.Run("after first audio", func(t *testing.T) {
		provider := newBlockingProvider(true)
		sink := newReceiptSink("")
		fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
			sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
		defer fixture.stop(t)
		fixture.receiveResolutions(t)
		fixture.sendText(t, speechelements.TextSegment{ID: "cancel-after", Text: "partly presented"})
		waitSignal(t, sink.audioDelivered, "first audio was not handed to sink")
		fixture.sendCancel(t, "playback_cancel", "cancel-after", "barge-in")
		fixture.sendCancel(t, "tts_cancel", "cancel-after", "barge-in")
		outcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "cancel-after")
		if outcome.Kind != speechelements.OutcomeCancelled || !outcome.CrossedBoundary {
			t.Fatalf("playback outcome = %+v", outcome)
		}

		assertPlaybackReceipts(t, fixture, 1, "barge-in", []struct {
			boundary string
			kind     speechelements.PlaybackReceiptKind
		}{
			{"playback_reserved", speechelements.PlaybackReserved},
			{"playback_begun", speechelements.PlaybackBegun},
			{"playback_text_committed", speechelements.PlaybackTextCommitted},
			{"playback_audio_emitted", speechelements.PlaybackAudioEmitted},
			{"playback_ended", speechelements.PlaybackEnded},
			{"playback_released", speechelements.PlaybackReleased},
		})
		assertNoReceiptBoundaries(t, fixture)
		if effects := sink.effectsSnapshot(); !slices.Equal(effects, []string{
			"reserve", "begin", "audio", "end",
		}) {
			t.Fatalf("sink effects = %v", effects)
		}
	})
}

func TestCancelledPlaybackReleaseCarriesMeasuredIncompleteWordBoundary(t *testing.T) {
	provider := newBlockingProvider(true)
	sink := newReceiptSink("")
	sink.timing = spoken.TrackerConfig{
		Aligner: receiptWordAligner{}, Interval: time.Nanosecond, Timeout: time.Second,
	}
	fixture := mountSpeech(t, providerDescriptor, func() v1.SpeechProvider { return provider },
		sinkDescriptor, func() speechelements.PlaybackSink { return sink }, nil)
	defer fixture.stop(t)
	fixture.receiveResolutions(t)
	fixture.sendText(t, speechelements.TextSegment{ID: "measured-cancel", Text: "partly presented"})
	waitSignal(t, sink.audioDelivered, "measured cancellation emitted no audio")
	fixture.sendCancel(t, "playback_cancel", "measured-cancel", "barge-in")
	fixture.sendCancel(t, "tts_cancel", "measured-cancel", "barge-in")
	if outcome := receiveMatchingPlayback(t, fixture.egress(t, "playback_outcome"), "measured-cancel"); outcome.Kind != speechelements.OutcomeCancelled {
		t.Fatalf("playback outcome = %+v", outcome)
	}
	for _, boundary := range []string{
		"playback_reserved", "playback_begun", "playback_text_committed",
		"playback_audio_emitted", "playback_ended", "playback_released",
	} {
		receipt := receive(t, fixture.egress(t, boundary)).Payload.(speechelements.PlaybackReceipt)
		if boundary != "playback_released" {
			continue
		}
		if receipt.Outcome.Completed || receipt.Outcome.Mark.Complete() ||
			!receipt.Outcome.Mark.Measured || receipt.Outcome.Mark.Pending == "" {
			t.Fatalf("released measured boundary = %+v", receipt.Outcome)
		}
	}
}

func assertPlaybackReceipts(
	t *testing.T, fixture *speechFixture, firstSequence uint64, terminalReason string,
	expected []struct {
		boundary string
		kind     speechelements.PlaybackReceiptKind
	},
) {
	t.Helper()
	for index, expected := range expected {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		envelope, err := fixture.egress(t, expected.boundary).Receive(ctx)
		cancel()
		if err != nil {
			t.Fatalf("receive %s cancellation receipt: %v", expected.boundary, err)
		}
		receipt := envelope.Payload.(speechelements.PlaybackReceipt)
		if receipt.Kind != expected.kind || receipt.Sequence != firstSequence+uint64(index) {
			t.Fatalf("%s receipt = %+v", expected.boundary, receipt)
		}
		if (expected.kind == speechelements.PlaybackEnded || expected.kind == speechelements.PlaybackReleased) &&
			(receipt.Outcome.Completed || receipt.Outcome.Reason != terminalReason) {
			t.Fatalf("%s cancellation receipt = %+v", expected.boundary, receipt)
		}
	}
}

func assertNoReceiptBoundaries(t *testing.T, fixture *speechFixture) {
	t.Helper()
	for _, boundary := range []string{
		"playback_reserved", "playback_begun", "playback_text_committed",
		"playback_audio_emitted", "playback_ended", "playback_released",
	} {
		assertNoPlaybackReceipt(t, fixture.egress(t, boundary), boundary)
	}
}

func assertNoPlaybackReceipt(t *testing.T, input element.InputPort, boundary string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("%s emitted unexpected receipt %+v", boundary, envelope.Payload)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inspect %s receipt: %v", boundary, err)
	}
}

type receiptSink struct {
	*recordingSink
	fail   string
	timing spoken.TrackerConfig
	mu     sync.Mutex
	seen   []string
}

func newReceiptSink(fail string) *receiptSink {
	return &receiptSink{recordingSink: newRecordingSink(), fail: fail}
}

func (sink *receiptSink) effect(name string) error {
	if sink.fail == name {
		return errors.New(name + " effect failed")
	}
	sink.mu.Lock()
	sink.seen = append(sink.seen, name)
	sink.mu.Unlock()
	return nil
}

func (sink *receiptSink) Reserve(action.Utterance) error { return sink.effect("reserve") }

func (sink *receiptSink) CancelReservation(action.Utterance) {
	sink.mu.Lock()
	sink.seen = append(sink.seen, "release")
	sink.mu.Unlock()
}

func (sink *receiptSink) Begin(_ context.Context, utterance action.Utterance) error {
	if err := sink.effect("begin"); err != nil {
		return err
	}
	return sink.recordingSink.Begin(context.Background(), utterance)
}

func (sink *receiptSink) Audio(
	_ context.Context, utterance action.Utterance, frame action.Frame,
) error {
	if err := sink.effect("audio"); err != nil {
		return err
	}
	return sink.recordingSink.Audio(context.Background(), utterance, frame)
}

func (sink *receiptSink) End(
	_ context.Context, utterance action.Utterance, outcome action.Outcome,
) error {
	if err := sink.effect("end"); err != nil {
		return err
	}
	return sink.recordingSink.End(context.Background(), utterance, outcome)
}

func (sink *receiptSink) effectsSnapshot() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return slices.Clone(sink.seen)
}

func (sink *receiptSink) PlaybackTiming() spoken.TrackerConfig { return sink.timing }

var _ speechelements.PlaybackSink = (*receiptSink)(nil)
var _ speechelements.PlaybackTimingSource = (*receiptSink)(nil)
var _ action.SpeechReservationSink = (*receiptSink)(nil)
