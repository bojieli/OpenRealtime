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
// voice model (Gemini) run the production scenario graph against scripted
// stories whose transcripts arrive the way Deepgram delivers one - a partial
// every second growing a few words at a time, a final after the pause - with
// no audio, no recogniser, and no person needed to speak them.
//
// Each story sets a standing count and then tells it: animals at a zoo,
// language models tried in a week, fruit bought at a market with a question
// in the middle, a long safari, programming languages with animals as
// distractors, and cities until the person says to stop. It scores what was
// asked for: every item counted once, in order, on the partial that names
// it rather than after the sentence ends; partials of sentences with nothing
// to count listened to; a question answered on its final and the count
// continued after it; nothing counted once the rule is lifted; nothing but
// the numbers and the answers said; and the model never asked twice at
// once. It is opt-in because it spends real model calls:
//
//	OPENREALTIME_COUNTING_BENCH=1 go test ./graphs -run TestCountingBenchmark -v -count=1
//
// with a vLLM policy at OPENREALTIME_POLICY_URL (default http://127.0.0.1:8000/v1,
// model OPENREALTIME_POLICY_MODEL, default qwen-fast) and GEMINI_API_KEY set.
// Set OPENREALTIME_COUNTING_BENCH to a comma-separated list of story names
// to run a subset. Each run's timeline is written under
// OPENREALTIME_COUNTING_BENCH_DIR (default ../.runtime/counting-bench) whether
// it passes or not.
func TestCountingBenchmarkAgainstLiveModels(t *testing.T) {
	selection := strings.TrimSpace(os.Getenv("OPENREALTIME_COUNTING_BENCH"))
	if selection == "" {
		t.Skip("set OPENREALTIME_COUNTING_BENCH=1 to run the live counting benchmark")
	}
	geminiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if geminiKey == "" {
		t.Skip("GEMINI_API_KEY is not set")
	}
	wanted := map[string]bool{}
	if selection != "1" && selection != "all" {
		for _, name := range strings.Split(selection, ",") {
			wanted[strings.TrimSpace(name)] = true
		}
	}
	outDir := envOr("OPENREALTIME_COUNTING_BENCH_DIR", filepath.Join("..", ".runtime", "counting-bench"))
	stamp := time.Now().UTC().Format("20060102T150405Z")
	// The stories are independent sessions, so they play at the same time.
	var summaryMu sync.Mutex
	summary := map[string]bool{}
	t.Run("stories", func(t *testing.T) {
		for _, story := range countingBenchmarkStories() {
			if len(wanted) > 0 && !wanted[story.name] {
				continue
			}
			t.Run(story.name, func(t *testing.T) {
				t.Parallel()
				t.Cleanup(func() {
					summaryMu.Lock()
					summary[story.name] = !t.Failed()
					summaryMu.Unlock()
				})
				// Fresh providers per story: the policy element closes its
				// decider when the session ends, and a client shared across
				// stories would be closed after the first.
				report := runCountingStory(t, newCountingBenchmarkProviders(t, geminiKey), story)
				out := filepath.Join(outDir, stamp+"-"+story.name+".txt")
				if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
					_ = os.WriteFile(out, []byte(report.text+"\n\nTimeline:\n"+report.timeline), 0o644)
				}
				t.Logf("%s (also %s):\n%s", story.name, out, report.text)
				if len(report.failures) > 0 {
					t.Fatalf("%s failed:\n- %s", story.name, strings.Join(report.failures, "\n- "))
				}
			})
		}
	})
	var lines []string
	for _, story := range countingBenchmarkStories() {
		if passed, ran := summary[story.name]; ran {
			lines = append(lines, fmt.Sprintf("%-32s %s", story.name, map[bool]string{true: "PASS", false: "FAIL"}[passed]))
		}
	}
	t.Logf("counting benchmark:\n%s", strings.Join(lines, "\n"))
}

const (
	countingFramesPerWord        = 4
	countingPartialEveryFrames   = 10
	countingBenchmarkInstruction = "Every word you generate is spoken aloud. Answer the latest user request " +
		"directly in the language they are using unless they requested translation. Runtime notes, " +
		"observation labels, and playback annotations are context, never words to read aloud. " +
		graphs.ProductionContinuationInstruction
)

// countingSentence is one utterance of a story.
type countingSentence struct {
	text string
	// items are the words this sentence counts, in the order they are said.
	items []string
	// answer, when set, says the sentence is a question or request the agent
	// must answer on its final, and what the answer should look like;
	// optional lets the agent stay silent instead.
	answer   *regexp.Regexp
	optional bool
	// pause is how long to wait after the final before the next sentence.
	pause time.Duration
}

type countingStory struct {
	name      string
	sentences []countingSentence
	// mustNotCount are words that name what a lifted rule used to count, or
	// what the rule was never about; speaking for one of them on a partial is
	// the failure the story exists to test.
	mustNotCount []string
}

var countingNumberWords = []string{
	"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve",
}

// countingBenchmarkStories are the transcripts.
func countingBenchmarkStories() []countingStory {
	say := func(text string, items ...string) countingSentence { return countingSentence{text: text, items: items} }
	ask := func(text, pattern string) countingSentence {
		return countingSentence{text: text, answer: regexp.MustCompile(pattern), pause: 3 * time.Second}
	}
	rule := func(text string) countingSentence { return countingSentence{text: text, pause: 3 * time.Second} }
	// ready is how a person opens: the rule, then "Are you ready?", and the
	// agent answers that it is before the story starts. A room session went
	// unanswered here - the policy read the question as more of the set-up.
	ready := func() countingSentence {
		return countingSentence{text: "Are you ready?", answer: regexp.MustCompile(`(?i)\bready\b|\byes\b|go ahead|of course|sure`), pause: 3 * time.Second}
	}
	return []countingStory{
		{
			// Sentence 5 names two animals: one sentence counted twice, on two
			// partials, in lockstep.
			name: "animals-zoo",
			sentences: []countingSentence{
				rule("Count the animals out loud as I mention them and say nothing else."),
				ready(),
				say("Yesterday I went to the zoo with my little brother."),
				say("First we saw a capybara sleeping in the sun.", "capybara"),
				say("Then a zebra was running around the big field.", "zebra"),
				say("My brother wanted ice cream so we went to the shop."),
				say("Later a tiger walked past the glass and a monkey screamed at it.", "tiger", "monkey"),
				say("That was a great day and we went home."),
			},
		},
		{
			// Proper nouns rather than nouns: the model has to know what a
			// language model is called.
			name: "language-models",
			sentences: []countingSentence{
				rule("Count the language models I mention out loud, and say nothing else."),
				ready(),
				say("I have been comparing a few assistants this week for work."),
				say("First I tried GPT-4 for drafting emails to clients.", "gpt-4"),
				say("Then I switched to Claude for code review, which I liked a lot.", "claude"),
				say("My colleague swears by Gemini for anything with spreadsheets,", "gemini"),
				say("but honestly the open-source scene is catching up fast."),
				say("Llama runs on my laptop and Qwen is surprisingly good at Chinese.", "llama", "qwen"),
				say("Anyway, that is my week in a nutshell."),
			},
		},
		{
			// A question in the middle of the count: answered on its final,
			// and the count continues from where it was.
			name: "fruits-with-a-question",
			sentences: []countingSentence{
				rule("Count the fruits out loud as I name them."),
				ready(),
				say("At the market this morning I bought a bag of apples.", "apples"),
				say("and a couple of ripe oranges for the kids.", "oranges"),
				ask("By the way, what is seven times eight?", `(?i)fifty[- ]?six|\b56\b`),
				say("Then near the exit I found some mangoes on sale.", "mangoes"),
				say("and a small basket of cherries that smelled amazing.", "cherries"),
				say("That was everything I brought home."),
			},
		},
		{
			// Twelve sentences and eight animals, past the step history's
			// window, with two sentences that name two animals.
			name: "long-safari",
			sentences: []countingSentence{
				rule("Count the animals out loud as I mention them and say nothing else."),
				ready(),
				say("We drove out before sunrise and the guide was already waiting."),
				say("Just past the gate an elephant crossed the road in front of us.", "elephant"),
				say("The guide stopped so we could take photos for a while."),
				say("Then a giraffe appeared on the left, chewing the top of a tree.", "giraffe"),
				say("We had coffee from a flask while the sun came up."),
				say("Near the river a hippo surfaced and a crocodile slid off the bank.", "hippo", "crocodile"),
				say("It got hot so we put the roof up and drove on."),
				say("A lion was sleeping in the shade and never looked at us.", "lion"),
				say("Later a warthog ran across the track with its tail straight up.", "warthog"),
				say("On the way back an ostrich raced the car and a baboon sat on the sign.", "ostrich", "baboon"),
				say("We got back to camp in time for lunch."),
			},
		},
		{
			// Animals in the story that must not be counted: the rule is
			// about programming languages.
			name: "languages-with-distractors",
			sentences: []countingSentence{
				rule("Count the programming languages I mention out loud, and say nothing else."),
				ready(),
				say("My dog sleeps under the desk while I write Python all day.", "python"),
				say("The cat knocked my Rust book off the shelf again.", "rust"),
				say("On the way to the Go meetup we saw a horse in a field.", "go"),
				say("Java was on the whiteboard but nobody wanted to touch it.", "java"),
				say("Then we all went home and fed the animals."),
			},
			mustNotCount: []string{"dog", "cat", "horse", "animals"},
		},
		{
			// The rule is lifted halfway: nothing after it is counted.
			name: "cities-then-stop",
			sentences: []countingSentence{
				rule("Count the cities I mention out loud as I say them."),
				ready(),
				say("Last year I flew to Paris for my cousin's wedding.", "paris"),
				say("and took the train to Berlin the week after.", "berlin"),
				{text: "Okay, you can stop counting now.", pause: 3 * time.Second, optional: true,
					answer: regexp.MustCompile(`(?i)ok|sure|stop|understood|will do|got it|alright|no problem`)},
				say("The next month I was in Lisbon for a conference,"),
				say("and then in Rome for a holiday with friends."),
				say("It was a busy year all round."),
			},
			mustNotCount: []string{"lisbon", "rome"},
		},
	}
}

// spokenExpectation is one thing the agent should say, in order: a number
// word, or an answer matching a pattern (possibly over several utterances).
type spokenExpectation struct {
	number   string
	answer   *regexp.Regexp
	optional bool
}

func (story countingStory) expectedSpoken() []spokenExpectation {
	var expected []spokenExpectation
	count := 0
	for _, sentence := range story.sentences {
		for range sentence.items {
			if count < len(countingNumberWords) {
				expected = append(expected, spokenExpectation{number: countingNumberWords[count]})
			}
			count++
		}
		if sentence.answer != nil {
			expected = append(expected, spokenExpectation{answer: sentence.answer, optional: sentence.optional})
		}
	}
	return expected
}

func normalizeSpoken(text string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(text), " .!?,;:\"'"))
}

// matchSpoken walks what was said against what was expected. A number must
// be its own utterance; an answer may span several utterances until the
// pattern matches.
func matchSpoken(spoken []string, expected []spokenExpectation) []string {
	var failures []string
	index := 0
	for _, want := range expected {
		if want.number != "" {
			if index >= len(spoken) {
				failures = append(failures, fmt.Sprintf("the agent never said %q", want.number))
				continue
			}
			if got := normalizeSpoken(spoken[index]); got != want.number {
				failures = append(failures, fmt.Sprintf("the agent said %q where %q was due", spoken[index], want.number))
			}
			index++
			continue
		}
		joined := ""
		start := index
		for index < len(spoken) && !isNumberWord(spoken[index]) {
			joined += " " + spoken[index]
			index++
			if want.answer.MatchString(joined) {
				break
			}
		}
		if !want.answer.MatchString(joined) {
			if want.optional && index == start {
				continue
			}
			failures = append(failures, fmt.Sprintf("the answer %q does not match %s", strings.TrimSpace(joined), want.answer))
		}
	}
	if index < len(spoken) {
		failures = append(failures, fmt.Sprintf("the agent also said %q", spoken[index:]))
	}
	return failures
}

func isNumberWord(text string) bool {
	normalized := normalizeSpoken(text)
	for _, word := range countingNumberWords {
		if normalized == word {
			return true
		}
	}
	return false
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// countingBenchmarkProviders are the production policy client and voice
// adapter, shared across stories.
type countingBenchmarkProviders struct {
	policy           *policymodel.Client
	policyDescriptor policyelements.SemanticDeciderDescriptor
	voice            *gemini.Adapter
}

func newCountingBenchmarkProviders(t *testing.T, geminiKey string) countingBenchmarkProviders {
	t.Helper()
	policyModel := envOr("OPENREALTIME_POLICY_MODEL", "qwen-fast")
	policyClient, err := policymodel.New(policymodel.Config{
		BaseURL: envOr("OPENREALTIME_POLICY_URL", "http://127.0.0.1:8000/v1"), Model: policyModel,
		Timeout: 2 * time.Second, GuidedChoice: true, Reasoning: openaicompat.ReasoningControlTemplateKwargs,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = policyClient.Close() })
	temperature := 0.0
	voice, err := gemini.New(gemini.Config{
		APIKey: geminiKey, Model: envOr("OPENREALTIME_MODEL", "gemini-3.8-flash"),
		Endpoint: "https://generativelanguage.googleapis.com/v1beta",
		Phase:    trajectory.PhaseFast, Effort: continuation.Effort(envOr("OPENREALTIME_VOICE_EFFORT", "128")),
		ToolAuthority: continuation.ToolAuthorityPropose, SpeechAuthority: continuation.SpeechAuthorityVoice,
		Temperature: &temperature, RequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return countingBenchmarkProviders{
		policy: policyClient, voice: voice,
		policyDescriptor: policyelements.SemanticDeciderDescriptor{
			Provider: "vllm", Model: policyModel, Protocol: "openai-chat-completions",
			Revision: "policymodel-client-v2", ConfigurationDigest: "sha256:" + strings.Repeat("0", 64),
			Vision: true, StandingExtraction: true, DecisionTimeoutMS: 2000,
		},
	}
}

// runCountingStory speaks one story into a fresh session and scores it.
func runCountingStory(t *testing.T, providers countingBenchmarkProviders, story countingStory) countingReport {
	t.Helper()
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	config.Policy.Descriptor = providers.policyDescriptor
	config.Model.Descriptor = providers.voice.Descriptor()
	config.SemanticAdmission = scenarioconversation.SemanticAdmissionSelection{
		StandingExtraction: true, StandingMemory: 64, Rules: coreinteraction.ChoiceInstruction,
	}
	config.ContinuationInstruction = countingBenchmarkInstruction
	config.MaxOutputTokens = 1024
	config.Tools = nil
	texts := make([]string, 0, len(story.sentences))
	for _, sentence := range story.sentences {
		texts = append(texts, sentence.text)
	}
	asr := &countingBenchmarkASR{sentences: texts}
	config.ASR.Factory = func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
		asr.descriptor = config.ASR.Descriptor
		return asr, nil
	}
	config.Policy.Factory = func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		return &countingBenchmarkDecider{Client: providers.policy, descriptor: providers.policyDescriptor}, nil
	}
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		return providers.voice, nil
	}
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
	sink := &countingBenchmarkSink{scenarioAddressingSink: newScenarioAddressingSink()}
	settings := legacy.Settings{
		Instruction: "You are a realtime assistant in a conversation room. Follow the user's instructions.",
		Voice:       config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
	}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		SessionID: "counting-" + story.name, Sink: sink, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runtime.Close(ctx, errors.New("counting benchmark complete"))
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	// Speak the story at a person's pace: 100 ms frames, 2.5 words a second,
	// a partial every second, 800 ms of silence to end each sentence.
	var clock scenarioAddressingAudioClock
	for _, sentence := range story.sentences {
		frames := len(strings.Fields(sentence.text))*countingFramesPerWord + 3
		for i := 0; i < frames; i++ {
			sendScenarioCountAudio(t, runtime, &clock, false)
			time.Sleep(100 * time.Millisecond)
		}
		for i := 0; i < 8; i++ {
			sendScenarioCountAudio(t, runtime, &clock, true)
			time.Sleep(100 * time.Millisecond)
		}
		awaitCountingFinal(t, sink, sentence.text)
		pause := sentence.pause
		if pause == 0 {
			pause = 1500 * time.Millisecond
		}
		time.Sleep(pause)
	}
	time.Sleep(6 * time.Second)
	report := scoreCountingStory(story, recording, sink)
	report.timeline = sink.timelineText("counting-" + story.name)
	return report
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
	milliseconds := countingBenchmarkAudioMS(plan.Text)
	return emit(v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":bench-audio", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: scenarioEndpointPCM(24 * milliseconds), Final: true,
	})
}

// countingBenchmarkAudioMS is how long the fake synthesiser's audio for a
// text lasts: a fixed lead plus a quarter of a second a word.
func countingBenchmarkAudioMS(text string) int {
	return 300 + 250*len(strings.Fields(text))
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

func (sink *countingBenchmarkSink) timelineText(session string) string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	lines := make([]string, 0, len(sink.events))
	for _, entry := range sink.events {
		lines = append(lines, entry.event.Line(entry.at, session))
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
	timeline string
	failures []string
}

var countingHeardLine = regexp.MustCompile(`(?m)^heard from [^:]+ so far: "(.*)"$`)

// scoreCountingStory reads the run back and states what happened, item by
// item.
func scoreCountingStory(
	story countingStory, recording *scenarioAddressingGraphRecorder, sink *countingBenchmarkSink,
) countingReport {
	type decided struct {
		sentence int
		decision policyelements.SemanticDecision
		heard    string
		// added is what this step's words added to the previous step's on
		// the same sentence: the part the policy was deciding about.
		added string
	}
	var decisions []decided
	streams := map[string]int{}
	previous := map[int]string{}
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
		sentence := streams[decision.StreamID]
		added := strings.TrimPrefix(heard, previous[sentence])
		previous[sentence] = heard
		decisions = append(decisions, decided{sentence: sentence, decision: decision, heard: heard, added: added})
	}
	var report countingReport
	var lines []string
	add := func(format string, arguments ...any) { lines = append(lines, fmt.Sprintf(format, arguments...)) }
	fail := func(format string, arguments ...any) {
		report.failures = append(report.failures, fmt.Sprintf(format, arguments...))
	}

	add("Decisions (%d):", len(decisions))
	for _, entry := range decisions {
		add("  s%-2d %-7s %-11s conf %.2f  %-26s %q", entry.sentence, entry.decision.Event, entry.decision.Choice.Token(),
			entry.decision.Confidence, coreinteraction.DescribeAnswers(entry.decision.Questions), entry.heard)
	}
	add("Items:")
	used := map[int]bool{}
	count := 0
	for sentenceIndex, sentence := range story.sentences {
		for _, item := range sentence.items {
			count++
			found := false
			for index, entry := range decisions {
				if used[index] || entry.sentence != sentenceIndex || !entry.decision.Choice.Speak ||
					!strings.Contains(strings.ToLower(entry.heard), item) {
					continue
				}
				used[index] = true
				found = true
				add("  %-10s (%s) -> spoke on the %s (%q)", item, ordinal(count), entry.decision.Event, entry.heard)
				if entry.decision.Event != coreinteraction.TranscriptPartial {
					fail("%s was counted only on the final, not while the sentence was being spoken", item)
				}
				break
			}
			if !found {
				add("  %-10s (%s) -> never spoken for", item, ordinal(count))
				fail("%s was never counted", item)
			}
		}
		if sentence.answer != nil {
			answered := false
			for _, entry := range decisions {
				if entry.sentence == sentenceIndex && entry.decision.Event == coreinteraction.TranscriptFinal &&
					entry.decision.Choice.Speak {
					answered = true
				}
			}
			add("  question s%d -> %s", sentenceIndex,
				map[bool]string{true: "answered on the final", false: "not answered"}[answered])
			if !answered && !sentence.optional {
				fail("the question %q was not answered on its final", sentence.text)
			}
		}
	}
	extra := 0
	for index, entry := range decisions {
		if entry.sentence >= len(story.sentences) || used[index] || !entry.decision.Choice.Speak {
			continue
		}
		sentence := story.sentences[entry.sentence]
		if entry.decision.Event != coreinteraction.TranscriptPartial {
			// A final may be answered - the voice decides what, if anything,
			// to say, and the spoken sequence below catches an answer that
			// should not have been.
			continue
		}
		// A speak that credits no item is a generation the voice answers
		// with silence - reported, not failed, whether the sentence had
		// items or not: the spoken sequence below fails the story if the
		// voice said anything it should not have. What is failed here is a
		// speak for something the rule does not cover.
		if len(sentence.items) == 0 {
			extra++
			add("  extra speak on s%d (%q), which has nothing to count; the voice answers it with silence",
				entry.sentence, entry.added)
			continue
		}
		flagged := false
		for _, word := range story.mustNotCount {
			if strings.Contains(strings.ToLower(entry.added), word) {
				fail("%q must not be counted but the policy chose %s on %q (new words %q)", word,
					entry.decision.Choice.Token(), entry.heard, entry.added)
				flagged = true
			}
		}
		if !flagged {
			extra++
			add("  extra speak on s%d (%q); the voice answers it with silence", entry.sentence, entry.added)
		}
	}

	sink.mu.Lock()
	spoken := append([]string(nil), sink.spoken...)
	outcomes := append([]cognitionelements.Outcome(nil), sink.outcomes...)
	sink.mu.Unlock()
	add("Spoken: %q", spoken)
	for _, failure := range matchSpoken(spoken, story.expectedSpoken()) {
		fail("%s", failure)
	}

	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].StartedNS < outcomes[j].StartedNS })
	add("Generations: %d (%d extra speaks)", len(outcomes), extra)
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

func ordinal(count int) string {
	if count >= 1 && count <= len(countingNumberWords) {
		return countingNumberWords[count-1]
	}
	return fmt.Sprint(count)
}
