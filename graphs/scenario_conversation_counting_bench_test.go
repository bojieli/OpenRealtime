package graphs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/policymodel"
	"github.com/bojieli/OpenRealtime/timeline"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The counting benchmark: the real interaction policy (vLLM) and the real
// voice model (Gemini) run the production scenario graph against a scripted
// story whose transcript arrives the way Deepgram delivers one - a partial
// every second growing a few words at a time, a final after the pause - with
// no audio, no recogniser, and no person needed to speak it.
//
// It measures what the person asked for: every animal is counted, once, in
// order, on the partial that names it rather than after the sentence ends;
// partials of sentences without an animal are listened to; nothing but the
// numbers is said; and the model is never asked twice at once. It is opt-in
// because it spends real model calls:
//
//	OPENREALTIME_COUNTING_BENCH=1 go test ./graphs -run TestCountingBenchmark -v -count=1
//
// with a vLLM policy at OPENREALTIME_POLICY_URL (default http://127.0.0.1:8000/v1,
// model OPENREALTIME_POLICY_MODEL, default qwen-fast) and GEMINI_API_KEY set.
// The full turn timeline of the run is written to OPENREALTIME_COUNTING_BENCH_OUT
// (default ../.runtime/counting-bench/<time>.txt) whether it passes or not.
func TestCountingBenchmarkAgainstLiveModels(t *testing.T) {
	if os.Getenv("OPENREALTIME_COUNTING_BENCH") == "" {
		t.Skip("set OPENREALTIME_COUNTING_BENCH=1 to run the live counting benchmark")
	}
	geminiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if geminiKey == "" {
		t.Skip("GEMINI_API_KEY is not set")
	}
	policyURL := envOr("OPENREALTIME_POLICY_URL", "http://127.0.0.1:8000/v1")
	policyModel := envOr("OPENREALTIME_POLICY_MODEL", "qwen-fast")
	story := countingBenchmarkStory()

	policyClient, err := policymodel.New(policymodel.Config{
		BaseURL: policyURL, Model: policyModel, Timeout: 2 * time.Second,
		GuidedChoice: true, Reasoning: openaicompat.ReasoningControlTemplateKwargs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policyClient.Close() })
	policyDescriptor := policyelements.SemanticDeciderDescriptor{
		Provider: "vllm", Model: policyModel, Protocol: "openai-chat-completions",
		Revision: "policymodel-client-v2", ConfigurationDigest: "sha256:" + strings.Repeat("0", 64),
		Vision: true, StandingExtraction: true, DecisionTimeoutMS: 2000,
	}
	temperature := 0.0
	voice, err := gemini.New(gemini.Config{
		APIKey: geminiKey, Model: envOr("OPENREALTIME_MODEL", "gemini-3.7-flash"),
		Endpoint: "https://generativelanguage.googleapis.com/v1beta",
		Phase:    trajectory.PhaseFast, Effort: continuation.Effort("128"),
		ToolAuthority: continuation.ToolAuthorityPropose, SpeechAuthority: continuation.SpeechAuthorityVoice,
		Temperature: &temperature, RequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	config.Policy.Descriptor = policyDescriptor
	config.Model.Descriptor = voice.Descriptor()
	config.SemanticAdmission = scenarioconversation.SemanticAdmissionSelection{
		StandingExtraction: true, StandingMemory: 64, Rules: coreinteraction.ChoiceInstruction,
	}
	config.ContinuationInstruction = countingBenchmarkInstruction
	config.MaxOutputTokens = 1024
	config.Tools = nil
	asr := &countingBenchmarkASR{sentences: story.sentences}
	config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
		asr.descriptor = config.ASR.Descriptor
		return asr, nil
	}
	config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		return &countingBenchmarkDecider{Client: policyClient, descriptor: policyDescriptor}, nil
	}
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) { return voice, nil }
	config.TTS.Factory = func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
		return &countingBenchmarkTTS{descriptor: config.TTS.Descriptor}, nil
	}
	launchConfig, err := graphs.ScenarioConversationLaunchConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	recording := newScenarioAddressingGraphRecorder()
	instrumentScenarioAddressingFactory(t, &launchConfig, "policy.SemanticAdmission", recording, "decision", "outcome")
	launched, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}
	sink := &countingBenchmarkSink{scenarioAddressingSink: newScenarioAddressingSink(), started: time.Now()}
	settings := legacy.Settings{
		Instruction: "You are a realtime assistant in a conversation room. Follow the user's instructions.",
		Voice:       config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
	}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		SessionID: "counting-bench", Sink: sink, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := runtime.Close(ctx, errors.New("counting benchmark complete")); err != nil {
			t.Error(err)
		}
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	// Speak the story at a person's pace: 100 ms frames, 2.5 words a second,
	// a partial every second, 800 ms of silence to end each sentence.
	var clock scenarioAddressingAudioClock
	for index, sentence := range story.sentences {
		frames := len(strings.Fields(sentence))*countingFramesPerWord + 3
		for i := 0; i < frames; i++ {
			sendScenarioCountAudio(t, runtime, &clock, false)
			time.Sleep(100 * time.Millisecond)
		}
		for i := 0; i < 8; i++ {
			sendScenarioCountAudio(t, runtime, &clock, true)
			time.Sleep(100 * time.Millisecond)
		}
		awaitCountingFinal(t, sink, sentence)
		time.Sleep(story.pauseAfter[index])
	}
	time.Sleep(6 * time.Second)

	report := scoreCountingBenchmark(story, recording, sink)
	out := envOr("OPENREALTIME_COUNTING_BENCH_OUT",
		filepath.Join("..", ".runtime", "counting-bench", time.Now().UTC().Format("20060102T150405Z")+".txt"))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
		_ = os.WriteFile(out, []byte(report.text+"\n\nTimeline:\n"+sink.timelineText()), 0o644)
	}
	t.Logf("counting benchmark report (also %s):\n%s", out, report.text)
	if len(report.failures) > 0 {
		t.Fatalf("counting benchmark failed:\n- %s", strings.Join(report.failures, "\n- "))
	}
}

const (
	countingFramesPerWord        = 4
	countingPartialEveryFrames   = 10
	countingBenchmarkInstruction = "Every word you generate is spoken aloud. Answer the latest user request " +
		"directly in the language they are using unless they requested translation. Runtime notes, " +
		"observation labels, and playback annotations are context, never words to read aloud. " +
		graphs.ProductionContinuationInstruction
)

type countingAnimal struct {
	sentence int
	word     string
	number   string
}

type countingStory struct {
	sentences  []string
	pauseAfter []time.Duration
	animals    []countingAnimal
}

// countingBenchmarkStory is the transcript. Sentence 5 names two animals so
// one sentence must be counted twice, on two partials, in lockstep.
func countingBenchmarkStory() countingStory {
	return countingStory{
		sentences: []string{
			"Count the animals out loud as I mention them and say nothing else.",
			"Yesterday I went to the zoo with my little brother.",
			"First we saw a capybara sleeping in the sun.",
			"Then a zebra was running around the big field.",
			"My brother wanted ice cream so we went to the shop.",
			"Later a tiger walked past the glass and a monkey screamed at it.",
			"That was a great day and we went home.",
		},
		pauseAfter: []time.Duration{
			3 * time.Second, 1200 * time.Millisecond, 1500 * time.Millisecond, 1500 * time.Millisecond,
			1200 * time.Millisecond, 2 * time.Second, 1200 * time.Millisecond,
		},
		animals: []countingAnimal{
			{2, "capybara", "one"}, {3, "zebra", "two"}, {5, "tiger", "three"}, {5, "monkey", "four"},
		},
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// countingBenchmarkASR delivers a scripted transcript the way a streaming
// recogniser does: words become audible at a person's pace, a partial with
// everything heard so far every second, and the whole sentence as the final
// when the endpoint fires.
type countingBenchmarkASR struct {
	mu         sync.Mutex
	descriptor v1.Descriptor
	sentences  []string
	index      int
	frames     int
	revealed   int
	revision   uint64
}

func (provider *countingBenchmarkASR) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *countingBenchmarkASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.index >= len(provider.sentences) {
		return nil, nil
	}
	provider.frames++
	words := strings.Fields(provider.sentences[provider.index])
	reveal := min(len(words), provider.frames/countingFramesPerWord)
	if reveal <= provider.revealed || (provider.frames%countingPartialEveryFrames != 0 && reveal < len(words)) {
		return nil, nil
	}
	provider.revealed = reveal
	provider.revision++
	return []v1.PerceptionRevision{{
		RevisionID: provider.revision, SourceSample: frame.SampleOffset,
		StableText: strings.Join(words[:reveal], " "),
	}}, nil
}

func (provider *countingBenchmarkASR) Finalize(_ context.Context, sample uint64) (v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.index >= len(provider.sentences) {
		return v1.PerceptionRevision{}, errors.New("counting benchmark recogniser finalized after its story ended")
	}
	text := provider.sentences[provider.index]
	provider.index++
	provider.frames, provider.revealed = 0, 0
	provider.revision++
	return v1.PerceptionRevision{
		RevisionID: provider.revision, SourceSample: sample, StableText: text, Final: true,
	}, nil
}

// countingBenchmarkDecider is the production policy client under the
// descriptor the graph was configured with.
type countingBenchmarkDecider struct {
	*policymodel.Client
	descriptor policyelements.SemanticDeciderDescriptor
}

func (decider *countingBenchmarkDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

// countingBenchmarkTTS returns audio as long as the words would take to say,
// so the agent is audibly speaking for a realistic stretch after each count.
type countingBenchmarkTTS struct{ descriptor v1.Descriptor }

func (provider *countingBenchmarkTTS) Descriptor() v1.Descriptor { return provider.descriptor }
func (provider *countingBenchmarkTTS) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var result []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		result = append(result, chunk)
		return nil
	})
	return result, err
}
func (provider *countingBenchmarkTTS) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	milliseconds := 300 + 250*len(strings.Fields(plan.Text))
	return emit(v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":bench-audio", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: scenarioEndpointPCM(24 * milliseconds), Final: true,
	})
}

type countingTimelineEvent struct {
	at    time.Time
	event timeline.Event
}

// countingBenchmarkSink records what the person would have heard and the
// turn story the runtime reports, projected onto the same timeline lanes the
// room draws.
type countingBenchmarkSink struct {
	*scenarioAddressingSink
	started  time.Time
	mu       sync.Mutex
	spoken   []string
	events   []countingTimelineEvent
	outcomes []cognitionelements.Outcome
}

func (sink *countingBenchmarkSink) SpeechText(ctx context.Context, utterance legacyaction.Utterance, text string) error {
	sink.mu.Lock()
	sink.spoken = append(sink.spoken, strings.TrimSpace(text))
	sink.mu.Unlock()
	return sink.scenarioAddressingSink.SpeechText(ctx, utterance, text)
}

func (sink *countingBenchmarkSink) Transcript(ctx context.Context, event legacy.TranscriptEvent) error {
	sink.record(legacy.DebugEvent{
		Category: "asr", Name: "asr.transcript", Attributes: map[string]any{"final": event.Final},
		Payload: map[string]any{"text": event.Text},
	})
	return sink.scenarioAddressingSink.Transcript(ctx, event)
}

func (sink *countingBenchmarkSink) Debug(_ context.Context, event legacy.DebugEvent) error {
	sink.record(event)
	return nil
}

func (sink *countingBenchmarkSink) record(event legacy.DebugEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if event.Name == "model_outcome" {
		if outcome, ok := event.Payload["value"].(cognitionelements.Outcome); ok {
			sink.outcomes = append(sink.outcomes, outcome)
		}
	}
	for _, projected := range timeline.Project(event) {
		sink.events = append(sink.events, countingTimelineEvent{at: time.Now(), event: projected})
	}
}

func (sink *countingBenchmarkSink) timelineText() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	lines := make([]string, 0, len(sink.events))
	for _, entry := range sink.events {
		lines = append(lines, entry.event.Line(entry.at, "counting-bench"))
	}
	return strings.Join(lines, "\n")
}

func awaitCountingFinal(t *testing.T, sink *countingBenchmarkSink, sentence string) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-sink.transcripts:
			if event.Final {
				if event.Text != sentence {
					t.Fatalf("final transcript = %q, want %q", event.Text, sentence)
				}
				return
			}
		case <-deadline.C:
			t.Fatalf("no final transcript for %q", sentence)
		}
	}
}

type countingReport struct {
	text     string
	failures []string
}

var countingHeardLine = regexp.MustCompile(`(?m)^heard from [^:]+ so far: "(.*)"$`)

// scoreCountingBenchmark reads the run back and states what happened,
// animal by animal.
func scoreCountingBenchmark(
	story countingStory, recording *scenarioAddressingGraphRecorder, sink *countingBenchmarkSink,
) countingReport {
	type decided struct {
		sentence int
		decision policyelements.SemanticDecision
		heard    string
	}
	var decisions []decided
	streams := map[string]int{}
	for _, envelope := range recording.records("semantic_admission.decision") {
		decision, ok := envelope.Payload.(policyelements.SemanticDecision)
		if !ok || decision.StreamID == "" {
			continue
		}
		if _, seen := streams[decision.StreamID]; !seen {
			streams[decision.StreamID] = len(streams)
		}
		heard := ""
		if match := countingHeardLine.FindStringSubmatch(decision.Evidence); match != nil {
			heard = match[1]
		}
		decisions = append(decisions, decided{sentence: streams[decision.StreamID], decision: decision, heard: heard})
	}
	var report countingReport
	var lines []string
	add := func(format string, arguments ...any) { lines = append(lines, fmt.Sprintf(format, arguments...)) }
	fail := func(format string, arguments ...any) {
		report.failures = append(report.failures, fmt.Sprintf(format, arguments...))
	}

	add("Decisions (%d):", len(decisions))
	for _, entry := range decisions {
		add("  s%d %-7s %-11s conf %.2f  %q", entry.sentence, entry.decision.Event, entry.decision.Choice.Token(),
			entry.decision.Confidence, entry.heard)
	}
	animalSentences := map[int]bool{}
	for _, animal := range story.animals {
		animalSentences[animal.sentence] = true
	}
	add("Animals:")
	for _, animal := range story.animals {
		found := false
		for _, entry := range decisions {
			if entry.sentence != animal.sentence || !entry.decision.Choice.Speak ||
				!strings.Contains(strings.ToLower(entry.heard), animal.word) {
				continue
			}
			found = true
			add("  %-9s -> spoke on the %s (%q)", animal.word, entry.decision.Event, entry.heard)
			if entry.decision.Event != coreinteraction.TranscriptPartial {
				fail("%s was counted only on the final, not while the sentence was being spoken", animal.word)
			}
			break
		}
		if !found {
			add("  %-9s -> never spoken for", animal.word)
			fail("%s was never counted", animal.word)
		}
	}
	for _, entry := range decisions {
		// A final may be answered - the voice decides what, if anything, to
		// say and the spoken sequence below catches an answer that should
		// not have been - but a partial of a sentence without an animal is
		// nothing to speak for.
		if entry.decision.Choice.Speak && !animalSentences[entry.sentence] &&
			entry.decision.Event == coreinteraction.TranscriptPartial {
			fail("sentence %d has no animal but the policy chose %s on the partial %q", entry.sentence,
				entry.decision.Choice.Token(), entry.heard)
		}
	}

	sink.mu.Lock()
	spoken := append([]string(nil), sink.spoken...)
	outcomes := append([]cognitionelements.Outcome(nil), sink.outcomes...)
	sink.mu.Unlock()
	heardNumbers := make([]string, 0, len(spoken))
	for _, text := range spoken {
		heardNumbers = append(heardNumbers, strings.ToLower(strings.Trim(text, " .!?,")))
	}
	add("Spoken: %q", spoken)
	wantNumbers := make([]string, 0, len(story.animals))
	for _, animal := range story.animals {
		wantNumbers = append(wantNumbers, animal.number)
	}
	if strings.Join(heardNumbers, " ") != strings.Join(wantNumbers, " ") {
		fail("the agent said %q, want %q", heardNumbers, wantNumbers)
	}

	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].StartedNS < outcomes[j].StartedNS })
	add("Generations: %d", len(outcomes))
	for index := 1; index < len(outcomes); index++ {
		previous := outcomes[index-1]
		// stop+speak cancels a run and admits the next in one decision; the
		// cancelled run's outcome can land after its replacement started.
		if outcomes[index].StartedNS < previous.FinishedNS && previous.Kind == cognitionelements.OutcomeSucceeded {
			fail("generation %s started while %s was still running", outcomes[index].RunID, previous.RunID)
		}
	}
	if len(report.failures) == 0 {
		add("Result: PASS")
	} else {
		add("Result: FAIL (%d)", len(report.failures))
	}
	report.text = strings.Join(lines, "\n")
	return report
}
