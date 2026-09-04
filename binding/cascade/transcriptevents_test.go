package cascade_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

type serialTranslationDecider struct {
	standingSeen sync.Once
	standing     chan struct{}
	triggerSeen  sync.Once
	trigger      chan struct{}
}

func (*serialTranslationDecider) Name() string { return "serial-translation" }

func (decider *serialTranslationDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	evidence := decision.Evidence
	choice := interaction.ActStaySilent
	if strings.Contains(evidence, "transcript event: partial") &&
		strings.Contains(evidence, `heard from user so far: "hello"`) &&
		strings.Contains(evidence, "Standing instructions:") {
		decider.standingSeen.Do(func() { close(decider.standing) })
	}
	if strings.Contains(evidence, "transcript event: final") &&
		strings.Contains(evidence, `heard from user so far: "hello"`) {
		choice = interaction.ActAnswer
	}
	if strings.Contains(evidence, "transcript event: partial") &&
		strings.Contains(evidence, `heard from user so far: "tomorrow"`) {
		choice = interaction.ActSpeakThrough
		decider.triggerSeen.Do(func() { close(decider.trigger) })
	}
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

type translationSetupExtractor struct{}

func (*translationSetupExtractor) Name() string { return "translation-setup" }

func (*translationSetupExtractor) Extract(
	_ context.Context, _ []interaction.StandingInstruction, _ []string, utterance string,
) (interaction.Extraction, error) {
	if !strings.Contains(utterance, "translate") {
		return interaction.Extraction{}, nil
	}
	return interaction.Extraction{Pins: []interaction.StandingInstruction{{
		Text: "translate each fragment", Scope: interaction.ScopeConversation,
	}}}, nil
}

func (*translationSetupExtractor) HasArrived(
	context.Context, []interaction.StandingInstruction, string,
) bool {
	return true
}

type serialVoiceProvider struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	startedOnce  sync.Once
	mu           sync.Mutex
	requests     []continuation.Request
}

func (*serialVoiceProvider) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "serial-voice", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

func (provider *serialVoiceProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if index == 0 {
		provider.startedOnce.Do(func() { close(provider.firstStarted) })
		select {
		case <-provider.releaseFirst:
		case <-ctx.Done():
			return continuation.Completion{}, context.Cause(ctx)
		}
	}
	text := "Tomorrow."
	if index == 0 {
		text = "Hello."
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: text}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func (provider *serialVoiceProvider) invocations() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.requests)
}

func (provider *serialVoiceProvider) request(index int) continuation.Request {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.requests[index]
}

func (decider *eventChoiceDecider) Name() string { return "event-choice" }

func (decider *eventChoiceDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	decider.mu.Lock()
	decider.seen = append(decider.seen, decision)
	decider.mu.Unlock()
	choice := string(interaction.ActStaySilent)
	if strings.Contains(decision.Evidence, "transcript event: final") {
		choice = string(decider.final)
	}
	if slices.Contains(decision.Options, string(interaction.OverlapBackchannel)) {
		choice = string(interaction.OverlapAmbiguous)
		if strings.Contains(strings.ToLower(decision.Evidence), "mhm") {
			choice = string(interaction.OverlapBackchannel)
		}
	}
	if slices.Contains(decision.Options, "valid_backchannel") {
		choice = "valid_backchannel"
	}
	// A queued response is represented as speaking so the policy can preserve
	// it before its first audio frame. In that state listen/answer are not
	// meaningful and keep-speaking is the inertial event-aware act.
	if !slices.Contains(decision.Options, choice) &&
		slices.Contains(decision.Options, string(interaction.ActKeepSpeaking)) {
		choice = string(interaction.ActKeepSpeaking)
	}
	for index, option := range decision.Options {
		if option == choice {
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
	return eventASRWithText("please wait for the final words")
}

func eventASRWithText(text string) func() (v1.PerceptionProvider, error) {
	return func() (v1.PerceptionProvider, error) {
		return &scriptedASR{
			partials: []string{text},
			final:    text,
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

func TestLiveInterjectionContinuesAfterOrdinaryVoiceSafePoint(t *testing.T) {
	decider := &serialTranslationDecider{
		standing: make(chan struct{}), trigger: make(chan struct{}),
	}
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
	policies.Extraction = &translationSetupExtractor{}
	voice := &serialVoiceProvider{
		firstStarted: make(chan struct{}), releaseFirst: make(chan struct{}),
	}
	lines := []string{"translate each fragment", "hello", "tomorrow"}
	var next atomic.Int32
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			index := int(next.Add(1)) - 1
			if index >= len(lines) {
				index = len(lines) - 1
			}
			return &scriptedASR{partials: []string{lines[index]}, final: lines[index]}, nil
		},
		Fast: voice, Slow: newSlow(), Speech: toneSpeech{chunks: 1}, Policies: policies,
	}, binding.Settings{})

	// Establish the standing policy, then begin the next utterance without
	// endpointing it so the test can observe that policy before asking for an
	// ordinary answer.
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		return slices.ContainsFunc(runtime.Trajectory().Items, func(item trajectory.Item) bool {
			return item.Kind == trajectory.KindObservation && item.Content == "translate each fragment"
		})
	}, "the translation setup did not reach the canonical trajectory")
	pushAudio(t, runtime, tone(2400, 8000), 3)
	select {
	case <-decider.standing:
	case <-time.After(5 * time.Second):
		t.Fatal("the setup policy was not in force for the next utterance")
	}
	pushAudio(t, runtime, silence(2400), 8)
	select {
	case <-voice.firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the ordinary voice continuation did not start")
	}

	// A third utterance triggers speak-through while the ordinary voice is
	// still generating. The live continuation must wait: taking its snapshot
	// now would omit the answer about to commit and permit it to repeat it.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	select {
	case <-decider.trigger:
	case <-time.After(5 * time.Second):
		t.Fatal("the live speak-through trigger was not decided")
	}
	time.Sleep(25 * time.Millisecond)
	if got := voice.invocations(); got != 1 {
		t.Fatalf("live voice started before ordinary safe point: invocations=%d", got)
	}
	close(voice.releaseFirst)
	waitFor(t, func() bool { return voice.invocations() == 2 },
		"the live voice did not continue after the ordinary safe point")

	second := voice.request(1)
	if !slices.ContainsFunc(second.Trajectory.Items, func(item trajectory.Item) bool {
		return item.Kind == trajectory.KindAssistant && item.Content == "Hello."
	}) {
		t.Fatalf("live continuation omitted preceding ordinary answer: %+v", second.Trajectory.Items)
	}
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 2 },
		"the serialized voice turns were not both delivered")
	spoken := strings.Join(sink.spokenTexts(), " ")
	if strings.Count(spoken, "Hello.") != 1 || strings.Count(spoken, "Tomorrow.") != 1 {
		t.Fatalf("serialized voice repeated or lost content: %q", spoken)
	}
}

func TestEventPolicyCanKeepAQueuedResponseBeforeItsFirstAudioFrame(t *testing.T) {
	policies, decider := transcriptPolicies(t, interaction.ActAnswer)
	speech := &waitingSpeech{started: make(chan struct{}), release: make(chan struct{})}
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "Here is the detailed answer.",
	}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: eventASRWithText("mhm"), Fast: fast, Slow: newSlow(), Speech: speech,
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
	if !strings.Contains(evidence, "What the agent is saying now: Here is the detailed answer.") ||
		!strings.Contains(evidence, "What the overlapping person has said so far: mhm") {
		t.Fatalf("the overlap classifier did not see the queued voice and listener continuer:\n%s", evidence)
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
