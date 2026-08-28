package cascade_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type eventChoiceDecider struct {
	final interaction.Act
	mu    sync.Mutex
	seen  []interaction.Decision
}

func (decider *eventChoiceDecider) Name() string { return "event-choice" }

func (decider *eventChoiceDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	decider.mu.Lock()
	decider.seen = append(decider.seen, decision)
	decider.mu.Unlock()
	choice := interaction.ActStaySilent
	if strings.Contains(decision.Evidence, "transcript event: final") {
		choice = decider.final
	}
	// A queued response is represented as speaking so the policy can preserve
	// it before its first audio frame. In that state listen/answer are not
	// meaningful and keep-speaking is the inertial event-aware act.
	if !slices.Contains(decision.Options, string(choice)) &&
		slices.Contains(decision.Options, string(interaction.ActKeepSpeaking)) {
		choice = interaction.ActKeepSpeaking
	}
	for index, option := range decision.Options {
		if option == string(choice) {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{Option: decision.Options[0]}, nil
}

func (decider *eventChoiceDecider) evidence() []string {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	result := make([]string, len(decider.seen))
	for index, decision := range decider.seen {
		result[index] = decision.Evidence
	}
	return result
}

func transcriptPolicies(t *testing.T, final interaction.Act) (interaction.Policies, *eventChoiceDecider) {
	t.Helper()
	decider := &eventChoiceDecider{final: final}
	policy, err := interaction.NewTranscriptEventPolicy(decider, interaction.TranscriptEventOptions{
		Partial: interaction.TranscriptEventRules{
			Instruction: "partial rules",
			Acts: []interaction.Act{
				interaction.ActStaySilent, interaction.ActSpeakThrough,
				interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
			},
		},
		Final: interaction.TranscriptEventRules{
			Instruction: "final rules",
			Acts: []interaction.Act{
				interaction.ActStaySilent, interaction.ActAnswer,
				interaction.ActKeepSpeaking, interaction.ActStopSpeaking,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.TranscriptEvents = policy
	return policies, decider
}

func eventASR() func() (v1.PerceptionProvider, error) {
	return func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{"please wait for the final words"},
			final:    "please wait for the final words",
		}, nil
	}
}

// endpointingASR models the optional capability exposed by a streaming ASR
// that also owns VAD. It deliberately repeats the same partial text: an
// endpoint is control evidence and must not depend on a fresh transcript delta.
type endpointingASR struct {
	pushes        int
	endpointAt    int
	finalizations int
}

func (*endpointingASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "endpointing-asr", Version: "1"}
}

func (asr *endpointingASR) PushFrame(
	context.Context, v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	asr.pushes++
	return []v1.PerceptionRevision{{
		RevisionID: uint64(asr.pushes), StableText: "please wait", Final: false,
	}}, nil
}

func (asr *endpointingASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	asr.finalizations++
	return v1.PerceptionRevision{
		RevisionID: 99, StableText: "please wait", Final: true,
	}, nil
}

func (asr *endpointingASR) SpeechEndpointed() bool {
	return asr.pushes >= asr.endpointAt
}

func TestFinalTranscriptListenCommitsEvidenceWithoutStartingCognition(t *testing.T) {
	policies, decider := transcriptPolicies(t, interaction.ActStaySilent)
	fast, slow := newFast(), newSlow()
	runtime, _ := startSession(t, cascade.Config{
		Perception: eventASR(), Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.Content == "please wait for the final words" {
				return true
			}
		}
		return false
	}, "the final transcript was not committed")
	waitFor(t, func() bool { return len(decider.evidence()) >= 2 }, "partial and final were not both decided")
	// Give the coordinator a chance to expose an accidental rollout.
	time.Sleep(50 * time.Millisecond)
	if fast.invocations() != 0 || slow.invocations() != 0 {
		t.Fatalf("final listen started cognition: fast=%d slow=%d", fast.invocations(), slow.invocations())
	}
	evidence := strings.Join(decider.evidence(), "\n")
	if !strings.Contains(evidence, "transcript event: partial") ||
		!strings.Contains(evidence, "transcript event: final") {
		t.Fatalf("policy did not see both event kinds:\n%s", evidence)
	}
}

func TestFinalTranscriptAnswerPreservesTheOrdinaryCascadeRollout(t *testing.T) {
	policies, _ := transcriptPolicies(t, interaction.ActAnswer)
	fast, slow := newFast(), newSlow()
	runtime, _ := startSession(t, cascade.Config{
		Perception: eventASR(), Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		return fast.invocations() > 0 && slow.invocations() > 0
	}, "a final answer did not preserve the normal fast+slow rollout")
}

func TestEventPolicyCanKeepAQueuedResponseBeforeItsFirstAudioFrame(t *testing.T) {
	policies, decider := transcriptPolicies(t, interaction.ActAnswer)
	speech := &waitingSpeech{started: make(chan struct{}), release: make(chan struct{})}
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Here is the detailed answer.",
	}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: eventASR(), Fast: fast, Slow: newSlow(), Speech: speech,
		Policies: policies,
	}, binding.Settings{})

	// Finish one turn and hold synthesis before it emits anything.
	speak(t, runtime, 3)
	select {
	case <-speech.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the response never entered synthesis")
	}

	// Renewed speech must reach the event policy before anything cancels the
	// queued answer. Its partial is a listener continuer in this test policy,
	// so the pending utterance remains authorized.
	pushAudio(t, runtime, tone(2400, 8000), 4)
	close(speech.release)
	waitFor(t, func() bool { return len(sink.speechOutcomes()) > 0 },
		"the event-aware pending response never terminated")
	outcome := sink.speechOutcomes()[0].outcome
	if !outcome.Completed || outcome.PlayedMS == 0 {
		t.Fatalf("the event-aware onset canceled queued speech before classification: %+v", outcome)
	}

	evidence := strings.Join(decider.evidence(), "\n")
	if !strings.Contains(evidence, "transcript event: partial") ||
		!strings.Contains(evidence, "agent: speaking out loud right now") {
		t.Fatalf("the policy did not see queued speech as a keep-or-stop decision:\n%s", evidence)
	}
}

func TestRecognizerEndpointFinalizesBeforeTheLocalSilenceFallback(t *testing.T) {
	policies, _ := transcriptPolicies(t, interaction.ActStaySilent)
	asr := &endpointingASR{endpointAt: 1}
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return asr, nil },
		Fast:       newFast(), Slow: newSlow(), Policies: policies,
		// A long local threshold proves the endpoint below came from the
		// recogniser rather than from the existing acoustic fallback.
		EndpointSilenceMS: 1200,
	}, binding.Settings{})

	for index := 0; index < 2; index++ {
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: tone(2400, 8000),
		}); err != nil {
			t.Fatalf("audio block %d: %v", index, err)
		}
	}
	if asr.finalizations != 1 {
		t.Fatalf("recogniser endpoint finalized %d times, want one", asr.finalizations)
	}
	stopped := 0
	for _, event := range sink.activityEvents() {
		if event.Stopped {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("recogniser endpoint emitted %d stopped events, want one", stopped)
	}
}

func TestRecognizerEndpointCapabilityLeavesTheExistingPathUntouched(t *testing.T) {
	asr := &endpointingASR{endpointAt: 1}
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return asr, nil },
		Fast:       newFast(), Slow: newSlow(), Policies: interaction.Defaults(),
		EndpointSilenceMS: 1200,
	}, binding.Settings{})

	for index := 0; index < 2; index++ {
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: tone(2400, 8000),
		}); err != nil {
			t.Fatalf("audio block %d: %v", index, err)
		}
	}
	if asr.finalizations != 0 {
		t.Fatalf("the opt-out path consumed the recogniser endpoint %d times", asr.finalizations)
	}
	for _, event := range sink.activityEvents() {
		if event.Stopped {
			t.Fatal("the opt-out path ended before its existing acoustic threshold")
		}
	}
}

func TestRecognizerAndLocalEndpointOnOneFrameFinalizeOnlyOnce(t *testing.T) {
	policies, _ := transcriptPolicies(t, interaction.ActStaySilent)
	asr := &endpointingASR{endpointAt: 2}
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return asr, nil },
		Fast:       newFast(), Slow: newSlow(), Policies: policies,
		EndpointSilenceMS: 100,
	}, binding.Settings{})

	for index := 0; index < 2; index++ {
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: tone(2400, 8000),
		}); err != nil {
			t.Fatalf("audio block %d: %v", index, err)
		}
	}
	if err := runtime.Audio(context.Background(), perception.Frame{
		Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
		PCM16LE: silence(2400),
	}); err != nil {
		t.Fatalf("endpoint frame: %v", err)
	}
	if asr.finalizations != 1 {
		t.Fatalf("coincident endpoints finalized %d times, want one", asr.finalizations)
	}
	stopped := 0
	for _, event := range sink.activityEvents() {
		if event.Stopped {
			stopped++
		}
	}
	if stopped != 1 {
		t.Fatalf("coincident endpoints emitted %d stopped events, want one", stopped)
	}
}
