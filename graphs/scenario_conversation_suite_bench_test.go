package graphs_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/timeline"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The twelve-scenario benchmark: the canonical scripted conversations in
// bench/scenario, played into the production scenario graph in-process with
// the real interaction policy (vLLM) and the real voice (Gemini), a scripted
// recogniser that delivers each line the way Deepgram would, a fake
// synthesiser whose audio lasts as long as the words would take to say, and
// the graph's own real-time playback. Every line lands on the timeline where
// the script puts it, pictures are shown when the script shows them, and a
// phone menu answers the keys the agent presses.
//
// Each run is scored twice. The scenario package's own checks are applied to
// the transcript, the paced playout, and what the loudspeaker carried in each
// window - the same rule the live diagnostic applies. Then Gemini reads the
// whole exchange as a judge and says whether the agent did the right thing at
// the right moment with no duplicate or erroneous actions; its verdict and
// reasons are part of the report and a fail is a fail.
//
// It is opt-in because it spends real model calls and real time (the scripts
// run at a person's pace):
//
//	OPENREALTIME_SCENARIO_BENCH=1 go test ./graphs -run TestTwelveScenariosAgainstLiveModels -v -count=1 -timeout 40m
//
// with a vLLM policy at OPENREALTIME_POLICY_URL and GEMINI_API_KEY set. Set
// OPENREALTIME_SCENARIO_BENCH to a comma-separated list of scenario names to
// run a subset, OPENREALTIME_SCENARIO_JUDGE=0 to skip the judge. Reports go
// under OPENREALTIME_SCENARIO_BENCH_DIR (default ../.runtime/scenario-bench).
func TestTwelveScenariosAgainstLiveModels(t *testing.T) {
	selection := strings.TrimSpace(os.Getenv("OPENREALTIME_SCENARIO_BENCH"))
	if selection == "" {
		t.Skip("set OPENREALTIME_SCENARIO_BENCH=1 to run the live twelve-scenario benchmark")
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
	judge := strings.TrimSpace(os.Getenv("OPENREALTIME_SCENARIO_JUDGE")) != "0"
	outDir := envOr("OPENREALTIME_SCENARIO_BENCH_DIR", filepath.Join("..", ".runtime", "scenario-bench"))
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	var summary []string
	for _, item := range scenario.Suite() {
		if len(wanted) > 0 && !wanted[item.Name] {
			continue
		}
		passed := t.Run(item.Name, func(t *testing.T) {
			run := playScenarioInProcess(t, newCountingBenchmarkProviders(t, geminiKey), item, root)
			result := scenario.ScorePlayed(item, run.timeline, run.transcript, run.menu, run.capture, run.listen)
			var lines []string
			lines = append(lines, "Scenario: "+item.Name, "Note: "+item.Note, "", "Events:", run.eventLog(), "", "Decisions:", run.decisionLog())
			if len(result.Failures) > 0 {
				lines = append(lines, "", "Checks: FAIL", "- "+strings.Join(result.Failures, "\n- "))
			} else {
				lines = append(lines, "", "Checks: PASS")
			}
			failures := append([]string(nil), result.Failures...)
			if judge {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				verdict, err := judgeScenarioRun(ctx, geminiKey, item, run, result)
				cancel()
				switch {
				case err != nil:
					lines = append(lines, "", "Judge: NOT VERIFIED: "+err.Error())
					failures = append(failures, "the judge could not review the run: "+err.Error())
				case verdict.Pass:
					lines = append(lines, "", "Judge: PASS", verdict.describe())
				default:
					lines = append(lines, "", "Judge: FAIL", verdict.describe())
					failures = append(failures, "the judge failed the run: "+strings.Join(verdict.Reasons, "; "))
				}
			}
			out := filepath.Join(outDir, stamp+"-"+scenarioFileStem(item.Name)+".txt")
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
				_ = os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n\nTimeline:\n"+run.timelineText+"\n"), 0o644)
			}
			t.Logf("%s (also %s):\n%s", item.Name, out, strings.Join(lines, "\n"))
			if len(failures) > 0 {
				t.Fatalf("%s failed:\n- %s", item.Name, strings.Join(failures, "\n- "))
			}
		})
		summary = append(summary, fmt.Sprintf("%-40s %s", item.Name, map[bool]string{true: "PASS", false: "FAIL"}[passed]))
	}
	t.Logf("twelve-scenario benchmark:\n%s", strings.Join(summary, "\n"))
}

func scenarioFileStem(name string) string {
	return strings.TrimSuffix(bench.TranscriptFileName(name), ".json")
}

// scenarioBenchLine is where one line of the script landed on the harness
// clock, and the words the recogniser reveals for it.
type scenarioBenchLine struct {
	// index is this line's position; script is the scripted line it came
	// from, since a scripted line is said one sentence at a time.
	index   int
	script  int
	speaker string
	text    string
	words   []string
	startMS int
	endMS   int
}

// scenarioBenchRun is everything one in-process play produced.
type scenarioBenchRun struct {
	lines      []scenarioBenchLine
	timeline   scenario.Timeline
	transcript bench.Transcript
	capture    bench.SessionAudioCapture
	menu       *scenario.Menu
	listen     scenario.Listener
	utterances []scenarioBenchUtterance
	decisions  []policyelements.SemanticDecision
	// events is the human-readable log the judge reads.
	events       []string
	timelineText string
}

func (run *scenarioBenchRun) eventLog() string { return strings.Join(run.events, "\n") }
func (run *scenarioBenchRun) decisionLog() string {
	lines := make([]string, 0, len(run.decisions))
	for _, decision := range run.decisions {
		lines = append(lines, fmt.Sprintf("  %-9s %-7s %-11s conf %.2f  pins %d->%d  %s %s", decision.Operation, decision.Event,
			decision.Choice.Token(), decision.Confidence, decision.StandingBefore, decision.StandingAfter,
			decision.DecisionStage, coreinteraction.DescribeAnswers(decision.Questions)))
	}
	return strings.Join(lines, "\n")
}

// scenarioBenchUtterance is one thing the agent said, where its audio landed,
// and where it was cut off if it was.
type scenarioBenchUtterance struct {
	id        string
	text      string
	startMS   float64
	audioMS   float64
	playedMS  float64
	completed bool
	ended     bool
}

const scenarioBenchFrameMS = 100

// scenarioBenchFramesPerWord paces the scripted speech at about three words
// a second, which is what the synthesiser the live diagnostic uses takes for
// these lines; the scripts' moments were written against that pace.
const scenarioBenchFramesPerWord = 3

// scenarioBenchSegments are the word boundaries of the suite's lines that
// carry no spaces. A recogniser reveals Mandarin a word at a time too, and
// revealing "办公室" (office) a character at a time had the voice translate
// "办" as "off-" and the rest as "ice" - the harness's cut, not the agent's.
var scenarioBenchSegments = map[string][]string{
	"你好，很高兴见到你。":      {"你好，", "很", "高兴", "见到", "你。"},
	"我们明天下午三点在办公室见面。": {"我们", "明天", "下午", "三点", "在", "办公室", "见面。"},
}

// layoutScenario places every line on the harness clock the way Compose
// places synthesised speech: at its scripted moment, or after a breath once
// the previous line has finished, lasting as long as its words take to say.
//
// A scripted line is laid out one sentence at a time, with the breath a
// voice takes between sentences, because that is what the room's recogniser
// hears: it ends an utterance at the pause, so "Count out loud from one to
// forty for me. Slowly, one number at a time." reaches the policy as two
// finals, the first of them a complete request on its own. Laid out as one
// final, the harness passed a scenario the live room did not.
func layoutScenario(item scenario.Scenario) ([]scenarioBenchLine, int) {
	lines := make([]scenarioBenchLine, 0, len(item.Script))
	finished, spokenYet := 0, false
	total := item.TrailingMS
	for _, sight := range item.Sees {
		total = max(total, sight.AtMS)
	}
	for script, line := range item.Script {
		for offset, sentence := range scenarioBenchSentences(line.Text) {
			words := scenarioBenchWords(sentence)
			start := line.AtMS
			if offset > 0 || (spokenYet && finished+600 > start) {
				start = finished + 600
			}
			duration := (len(words)*scenarioBenchFramesPerWord + 3) * scenarioBenchFrameMS
			entry := scenarioBenchLine{index: len(lines), script: script, speaker: line.Speaker,
				text: sentence, words: words, startMS: start, endMS: start + duration}
			lines = append(lines, entry)
			finished, spokenYet = entry.endMS, true
			total = max(total, entry.endMS)
		}
	}
	if item.TrailingMS > 0 && len(item.Script) > 0 {
		total = max(total, finished+item.TrailingMS)
	}
	return lines, total
}

// scenarioBenchWords splits a line into the units the recogniser reveals one
// at a time: words where there are spaces, pairs of characters where there
// are none.
// scenarioBenchSentences splits a scripted line at the sentence ends a voice
// pauses on: a full stop, question or exclamation mark before a space or the
// end, and their full-width forms anywhere. A line with none is one sentence.
func scenarioBenchSentences(text string) []string {
	var sentences []string
	runes := []rune(strings.TrimSpace(text))
	start := 0
	for index, r := range runes {
		ends := false
		switch r {
		case '。', '！', '？':
			ends = true
		case '.', '!', '?':
			ends = index+1 == len(runes) || unicode.IsSpace(runes[index+1])
		}
		if !ends || index+1 == len(runes) {
			continue
		}
		if sentence := strings.TrimSpace(string(runes[start : index+1])); sentence != "" {
			sentences = append(sentences, sentence)
		}
		start = index + 1
	}
	if rest := strings.TrimSpace(string(runes[start:])); rest != "" {
		sentences = append(sentences, rest)
	}
	if len(sentences) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return sentences
}

func scenarioBenchWords(text string) []string {
	if segments, known := scenarioBenchSegments[strings.TrimSpace(text)]; known {
		return segments
	}
	if fields := strings.Fields(text); len(fields) > 1 || len(text) < 4 {
		return fields
	}
	var words []string
	runes := []rune(text)
	for index := 0; index < len(runes); index += 2 {
		end := min(index+2, len(runes))
		words = append(words, string(runes[index:end]))
	}
	return words
}

func playScenarioInProcess(
	t *testing.T, providers countingBenchmarkProviders, item scenario.Scenario, root string,
) *scenarioBenchRun {
	t.Helper()
	lines, totalMS := layoutScenario(item)
	run := &scenarioBenchRun{lines: lines}
	if item.Menu != nil {
		run.menu = item.Menu()
	}
	// The room composes the same instruction and freezes it under this
	// limit; a harness that let it grow past the limit would measure a
	// voice the room could not launch.
	if len(countingBenchmarkInstruction) > 4096 {
		t.Fatalf("the voice instruction is %d bytes, over the room's 4096-byte limit", len(countingBenchmarkInstruction))
	}
	base := newScenarioProfileFixture(t)
	config := base.pluginConfig()
	// The room's architecture: the policy sees the frames it decides about.
	architecture, err := projectarch.Default().Resolve("cascade.composed-policy-direct-visual@1")
	if err != nil {
		t.Fatal(err)
	}
	config.Architecture = architecture
	config.Policy.Descriptor = providers.policyDescriptor
	config.Model.Descriptor = providers.voice.Descriptor()
	config.SemanticAdmission = scenarioconversation.SemanticAdmissionSelection{
		StandingExtraction: true, StandingMemory: 64, Rules: coreinteraction.ChoiceInstruction,
	}
	config.ContinuationInstruction = countingBenchmarkInstruction
	config.MaxOutputTokens = 1024
	config.Tools = nil
	var specs []legacyaction.ToolSpec
	for _, tool := range item.Tools {
		declaration, err := tool.FunctionDeclaration()
		if err != nil {
			t.Fatal(err)
		}
		config.Tools = append(config.Tools, scenarioconversation.ToolDeclaration{
			Name: tool.Name, Description: tool.Description, Parameters: declaration.Parameters,
			Confirm: legacyaction.ConfirmNever,
		})
		specs = append(specs, legacyaction.ToolSpec{
			Name: tool.Name, Description: tool.Description, Parameters: declaration.Parameters,
			Confirm: legacyaction.ConfirmNever,
		})
	}
	asr := &scenarioBenchASR{}
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
	sink := newScenarioBenchSink(run)
	settings := legacy.Settings{
		Instruction: item.Instructions, Tools: specs,
		Voice: config.TTS.Voice, Modalities: []string{"audio"}, Gate: config.Gate,
	}
	runtime, err := launched.Binding.Start(context.Background(), legacy.Options{
		SessionID: "scenario-" + scenarioFileStem(item.Name), Sink: sink, Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	sink.runtime = runtime
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = runtime.Close(ctx, errors.New("scenario benchmark complete"))
	})
	if err := runtime.Update(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	// Pictures, decoded once so the frame carries real dimensions.
	type sightFrame struct {
		atMS          int
		name          string
		image         []byte
		width, height int
	}
	var sights []sightFrame
	for _, sight := range item.Sees {
		path := sight.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		image, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		shape, err := png.DecodeConfig(bytes.NewReader(image))
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		sights = append(sights, sightFrame{atMS: sight.AtMS, name: filepath.Base(path), image: image,
			width: shape.Width, height: shape.Height})
	}

	// The room's clock: 100 ms frames at a person's pace, tone while a line
	// is being said and silence otherwise, pictures at their moment.
	frames := totalMS/scenarioBenchFrameMS + 1
	room := make([]int16, 0, frames*2_400)
	var clock scenarioAddressingAudioClock
	started := time.Now()
	sink.start(started)
	for frame := 0; frame < frames; frame++ {
		atMS := frame * scenarioBenchFrameMS
		time.Sleep(time.Until(started.Add(time.Duration(atMS) * time.Millisecond)))
		for _, sight := range sights {
			if sight.atMS >= atMS && sight.atMS < atMS+scenarioBenchFrameMS {
				// Showing a frame returns once the policy has decided about
				// it, and that decision waits for a generation in flight; a
				// slow voice made this take longer than five seconds.
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := runtime.Video(ctx, perception.Frame{
					Kind: perception.FrameImage, Source: "screen", CapturedNS: uint64(time.Since(started)),
					Image: sight.image, MIMEType: "image/png", Width: sight.width, Height: sight.height,
				})
				cancel()
				if err != nil {
					t.Fatalf("show the frame for %dms: %v", sight.atMS, err)
				}
				sink.note(fmt.Sprintf("%6dms  the agent is shown a picture (%s)", atMS, sight.name))
			}
		}
		active := -1
		for _, line := range lines {
			if atMS >= line.startMS && atMS < line.endMS {
				active = line.index
			}
		}
		asr.setLine(active, lines)
		sendScenarioCountAudio(t, runtime, &clock, active < 0)
		if active < 0 {
			room = append(room, make([]int16, 2_400)...)
		} else {
			room = append(room, scenarioEndpointSamples(2_400)...)
		}
	}
	// The live client keeps the session open until the agent has finished
	// what it was saying, and so does this: the checks that read the whole
	// conversation are about what the agent said, not about what it had
	// managed to say by the time the recording ran out.
	sink.awaitQuiet(3*time.Second, 90*time.Second)

	run.transcript, run.capture, run.utterances = sink.snapshot()
	run.capture.RoomPCM16 = room
	run.capture.SampleRateHz = 24_000
	run.transcript.PlaybackMS = float64(totalMS)
	run.timeline = scenario.Timeline{Samples: room, TotalMS: totalMS}
	// A span per scripted line, covering every sentence it was said in.
	for _, line := range lines {
		if line.script < len(run.timeline.Spans) {
			run.timeline.Spans[line.script].EndMS = line.endMS
			continue
		}
		run.timeline.Spans = append(run.timeline.Spans, scenario.Span{StartMS: line.startMS, EndMS: line.endMS})
	}
	for _, sight := range item.Sees {
		run.timeline.Sights = append(run.timeline.Sights, sight.AtMS)
	}
	run.listen = scenarioBenchListener(run.utterances)
	for _, envelope := range recording.records("semantic_admission.decision") {
		if decision, ok := envelope.Payload.(policyelements.SemanticDecision); ok {
			run.decisions = append(run.decisions, decision)
		}
	}
	sink.scenarioAddressingSink.mu.Lock()
	for _, failure := range sink.scenarioAddressingSink.failures {
		sink.events = append(sink.events, fmt.Sprintf("%6dms  runtime failure %s: %s", 0, failure.Code, failure.Message))
	}
	sink.scenarioAddressingSink.mu.Unlock()
	run.events = sink.eventLines()
	run.timelineText = sink.timelineText("scenario-" + scenarioFileStem(item.Name))
	return run
}

func scenarioEndpointSamples(count int) []int16 {
	pcm := scenarioEndpointPCM(count)
	samples := make([]int16, count)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(pcm[index*2:]))
	}
	return samples
}

// scenarioBenchListener answers what the loudspeaker carried in a window
// from the synthesiser's own pacing: each utterance's words are spread evenly
// over the audio the synthesiser makes for it, and a word counts as heard
// once most of it has played - an utterance cut off mid-way carries only the
// words before the cut, the way a transcriber of the recording would hear it.
func scenarioBenchListener(utterances []scenarioBenchUtterance) scenario.Listener {
	return func(fromMS, toMS int) (string, error) {
		var heard []string
		for _, utterance := range utterances {
			words := strings.Fields(utterance.text)
			if len(words) == 0 || utterance.startMS < 0 {
				continue
			}
			perWord := float64(countingBenchmarkAudioMS(utterance.text)) / float64(len(words))
			for index, word := range words {
				if perWord*(float64(index)+0.8) > utterance.playedMS {
					break
				}
				middle := utterance.startMS + perWord*(float64(index)+0.5)
				if middle >= float64(fromMS) && middle <= float64(toMS) {
					heard = append(heard, word)
				}
			}
		}
		return strings.Join(heard, " "), nil
	}
}

// scenarioBenchASR reveals the words of whichever line the harness says is
// being spoken, a few at a time at a person's pace, with a partial every
// second and the whole line as the final when the gate endpoints it.
type scenarioBenchASR struct {
	mu         sync.Mutex
	descriptor v1.Descriptor
	line       int
	words      []string
	text       string
	frames     int
	revealed   int
	revision   uint64
	// pending is the line whose final has not been delivered yet.
	pending string
}

func (provider *scenarioBenchASR) Descriptor() v1.Descriptor { return provider.descriptor }

func (provider *scenarioBenchASR) setLine(index int, lines []scenarioBenchLine) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if index < 0 {
		provider.line = -1
		return
	}
	if provider.line != index || provider.text != lines[index].text {
		provider.line = index
		provider.words = lines[index].words
		provider.text = lines[index].text
		provider.frames, provider.revealed = 0, 0
		provider.pending = provider.text
	}
}

func (provider *scenarioBenchASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.line < 0 || provider.pending == "" {
		return nil, nil
	}
	provider.frames++
	reveal := min(len(provider.words), provider.frames/scenarioBenchFramesPerWord)
	if reveal <= provider.revealed || (provider.frames%countingPartialEveryFrames != 0 && reveal < len(provider.words)) {
		return nil, nil
	}
	provider.revealed = reveal
	provider.revision++
	return []v1.PerceptionRevision{{
		RevisionID: provider.revision, SourceSample: frame.SampleOffset,
		StableText: strings.Join(provider.words[:reveal], scenarioBenchJoiner(provider.words)),
	}}, nil
}

func scenarioBenchJoiner(words []string) string {
	for _, word := range words {
		for _, r := range word {
			if unicode.Is(unicode.Han, r) {
				return ""
			}
		}
	}
	return " "
}

func (provider *scenarioBenchASR) Finalize(_ context.Context, sample uint64) (v1.PerceptionRevision, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	text := provider.pending
	provider.pending = ""
	provider.frames, provider.revealed = 0, 0
	provider.revision++
	return v1.PerceptionRevision{
		RevisionID: provider.revision, SourceSample: sample, StableText: text, Final: true,
	}, nil
}

// scenarioBenchSink records what the person would have heard, on the
// harness clock, in the shape the scenario scorer reads.
type scenarioBenchSink struct {
	*scenarioAddressingSink
	run     *scenarioBenchRun
	runtime legacy.Runtime

	mu         sync.Mutex
	started    time.Time
	playoutMS  float64
	lastEvent  time.Time
	inFlight   int
	moments    []bench.Moment
	agent      []bench.TimedAudioChunk
	utterances map[string]*scenarioBenchUtterance
	order      []string
	events     []string
	timeline   []countingTimelineEvent
}

// awaitQuiet returns once the agent has had nothing to say for idle, or
// after limit however busy it is.
func (sink *scenarioBenchSink) awaitQuiet(idle, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		open := 0
		for _, utterance := range sink.utterances {
			if !utterance.ended {
				open++
			}
		}
		last, inFlight := sink.lastEvent, sink.inFlight
		sink.mu.Unlock()
		// A generation still running is the agent about to say something;
		// a slow voice at the end of the script was cut off by the idle
		// rule and scored as never having answered.
		if open == 0 && inFlight == 0 && time.Since(last) >= idle {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func newScenarioBenchSink(run *scenarioBenchRun) *scenarioBenchSink {
	return &scenarioBenchSink{scenarioAddressingSink: newScenarioAddressingSink(), run: run,
		utterances: map[string]*scenarioBenchUtterance{}}
}

func (sink *scenarioBenchSink) start(at time.Time) {
	sink.mu.Lock()
	sink.started = at
	sink.mu.Unlock()
}

func (sink *scenarioBenchSink) nowMS() float64 {
	if sink.started.IsZero() {
		return 0
	}
	return float64(time.Since(sink.started)) / float64(time.Millisecond)
}

func (sink *scenarioBenchSink) note(line string) {
	sink.mu.Lock()
	sink.events = append(sink.events, line)
	sink.mu.Unlock()
}

func (sink *scenarioBenchSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }

func (sink *scenarioBenchSink) Transcript(_ context.Context, event legacy.TranscriptEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	now := sink.nowMS()
	sink.recordLocked(legacy.DebugEvent{
		Category: "asr", Name: "asr.transcript", Attributes: map[string]any{"final": event.Final},
		Payload: map[string]any{"text": event.Text},
	})
	if event.Final {
		sink.moments = append(sink.moments, bench.Moment{AtMS: now, Kind: bench.MomentTranscript, Text: event.Text})
		sink.events = append(sink.events, fmt.Sprintf("%6.0fms  heard (final): %q", now, event.Text))
	}
	return nil
}

func (sink *scenarioBenchSink) SpeechBegin(_ context.Context, utterance legacyaction.Utterance) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if _, known := sink.utterances[utterance.ID]; !known {
		sink.utterances[utterance.ID] = &scenarioBenchUtterance{id: utterance.ID, text: utterance.Text, startMS: -1}
		sink.order = append(sink.order, utterance.ID)
	}
	return nil
}

func (sink *scenarioBenchSink) SpeechText(_ context.Context, utterance legacyaction.Utterance, text string) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	now := sink.nowMS()
	record := sink.utteranceLocked(utterance)
	record.text = strings.TrimSpace(text)
	sink.moments = append(sink.moments, bench.Moment{AtMS: now, Kind: bench.MomentAgentText, Text: text})
	sink.events = append(sink.events, fmt.Sprintf("%6.0fms  agent starts saying: %q", now, strings.TrimSpace(text)))
	return nil
}

func (sink *scenarioBenchSink) utteranceLocked(utterance legacyaction.Utterance) *scenarioBenchUtterance {
	record, known := sink.utterances[utterance.ID]
	if !known {
		record = &scenarioBenchUtterance{id: utterance.ID, text: utterance.Text, startMS: -1}
		sink.utterances[utterance.ID] = record
		sink.order = append(sink.order, utterance.ID)
	}
	return record
}

func (sink *scenarioBenchSink) SpeechAudio(_ context.Context, utterance legacyaction.Utterance, frame legacyaction.Frame) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	now := sink.nowMS()
	record := sink.utteranceLocked(utterance)
	samples := make([]int16, len(frame.PCM16LE)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(frame.PCM16LE[index*2:]))
	}
	durationMS := float64(len(samples)) * 1000 / float64(max(frame.SampleRateHz, 1))
	// The playback element paces frames in real time, so arrival is playout
	// - serialised onto one clock, as a loudspeaker would play them, because
	// a frame can arrive a few milliseconds before the previous one has
	// finished sounding. A long frame is split into pieces so a window sees
	// the part inside it.
	if now < sink.playoutMS {
		now = sink.playoutMS
	}
	if record.startMS < 0 {
		record.startMS = now
	}
	sink.playoutMS = now + durationMS
	for offset := 0.0; offset < durationMS; offset += scenarioBenchFrameMS {
		piece := min(float64(scenarioBenchFrameMS), durationMS-offset)
		sink.moments = append(sink.moments, bench.Moment{AtMS: now + offset, Kind: bench.MomentAgentAudio,
			AudioMS: piece, PlayoutAtMS: now + offset, CallID: utterance.ID, ResponseID: utterance.ID})
	}
	sink.agent = append(sink.agent, bench.TimedAudioChunk{AtMS: now, PCM16: samples})
	record.audioMS += durationMS
	record.playedMS = record.audioMS
	return nil
}

func (sink *scenarioBenchSink) SpeechEnd(_ context.Context, utterance legacyaction.Utterance, outcome legacyaction.Outcome) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	now := sink.nowMS()
	record := sink.utteranceLocked(utterance)
	record.ended, record.completed = true, outcome.Completed
	if !outcome.Completed {
		record.playedMS = min(record.audioMS, float64(outcome.PlayedMS))
		// What was never played was never heard: drop the audio past the cut.
		kept := sink.moments[:0]
		for _, moment := range sink.moments {
			if moment.Kind == bench.MomentAgentAudio && moment.CallID == utterance.ID &&
				moment.AtMS-record.startMS >= record.playedMS {
				continue
			}
			kept = append(kept, moment)
		}
		sink.moments = kept
		sink.trimAgentAudioLocked(record)
		sink.events = append(sink.events, fmt.Sprintf("%6.0fms  agent is cut off after %dms of %q (%s)", now,
			outcome.PlayedMS, record.text, outcome.Reason))
	} else {
		sink.events = append(sink.events, fmt.Sprintf("%6.0fms  agent finishes saying %q", now, record.text))
	}
	status := "completed"
	if !outcome.Completed {
		status = "cancelled"
	}
	sink.moments = append(sink.moments, bench.Moment{AtMS: now, Kind: bench.MomentResponseDone,
		ResponseID: utterance.ID, ResponseStatus: status, ResponseStatusReason: outcome.Reason})
	return nil
}

// trimAgentAudioLocked cuts the captured playout of an utterance at the
// point playback stopped.
func (sink *scenarioBenchSink) trimAgentAudioLocked(record *scenarioBenchUtterance) {
	cut := record.startMS + record.playedMS
	kept := sink.agent[:0]
	for _, chunk := range sink.agent {
		endMS := chunk.AtMS + float64(len(chunk.PCM16))*1000/24_000
		if chunk.AtMS >= record.startMS && chunk.AtMS < record.startMS+record.audioMS+1 && endMS > cut {
			keep := int((cut - chunk.AtMS) * 24)
			if keep <= 0 {
				continue
			}
			if keep < len(chunk.PCM16) {
				chunk.PCM16 = chunk.PCM16[:keep]
			}
		}
		kept = append(kept, chunk)
	}
	sink.agent = kept
}

func (sink *scenarioBenchSink) ToolCalls(_ context.Context, event legacy.ToolCallEvent) error {
	sink.mu.Lock()
	now := sink.nowMS()
	for _, call := range event.Calls {
		sink.moments = append(sink.moments, bench.Moment{AtMS: now, Kind: bench.MomentToolCall,
			Name: call.Name, CallID: call.CallID, Arguments: string(call.Arguments)})
		sink.events = append(sink.events, fmt.Sprintf("%6.0fms  agent calls %s(%s)", now, call.Name, string(call.Arguments)))
	}
	sink.mu.Unlock()
	// The world answers on its own goroutine: the runtime delivers this call
	// from inside the graph, and the result goes back in through the front.
	go func() {
		for _, call := range event.Calls {
			var output json.RawMessage = json.RawMessage(`{"ok":true}`)
			if sink.run.menu != nil {
				answer, err := sink.run.menu.Respond(call.Name, call.Arguments)
				if err == nil {
					output = answer
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := sink.runtime.ToolResult(ctx, trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Output: output})
			cancel()
			sink.mu.Lock()
			at := sink.nowMS()
			sink.moments = append(sink.moments, bench.Moment{AtMS: at, Kind: bench.MomentToolResult, Name: call.Name, CallID: call.CallID, Text: string(output)})
			if err != nil {
				sink.events = append(sink.events, fmt.Sprintf("%6.0fms  the tool result for %s was refused: %v", at, call.Name, err))
			} else {
				sink.events = append(sink.events, fmt.Sprintf("%6.0fms  the world answers %s: %s", at, call.Name, string(output)))
			}
			sink.mu.Unlock()
		}
	}()
	return nil
}

func (sink *scenarioBenchSink) Debug(_ context.Context, event legacy.DebugEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.recordLocked(event)
	return nil
}

func (sink *scenarioBenchSink) recordLocked(event legacy.DebugEvent) {
	sink.lastEvent = time.Now()
	for _, projected := range timeline.Project(event) {
		sink.timeline = append(sink.timeline, countingTimelineEvent{at: time.Now(), event: projected})
		if projected.Lane == timeline.LaneModel && projected.Kind == "request" && projected.Phase == "start" {
			sink.inFlight++
		}
		if projected.Lane == timeline.LaneModel && projected.Phase == "end" && sink.inFlight > 0 {
			sink.inFlight--
		}
	}
}

func (sink *scenarioBenchSink) timelineText(session string) string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	lines := make([]string, 0, len(sink.timeline))
	for _, entry := range sink.timeline {
		lines = append(lines, entry.event.Line(entry.at, session))
	}
	return strings.Join(lines, "\n")
}

func (sink *scenarioBenchSink) eventLines() []string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	lines := append([]string(nil), sink.events...)
	sort.SliceStable(lines, func(i, j int) bool {
		return scenarioEventMS(lines[i]) < scenarioEventMS(lines[j])
	})
	return lines
}

func scenarioEventMS(line string) float64 {
	var ms float64
	_, _ = fmt.Sscanf(strings.TrimSpace(line), "%fms", &ms)
	return ms
}

func (sink *scenarioBenchSink) snapshot() (bench.Transcript, bench.SessionAudioCapture, []scenarioBenchUtterance) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	moments := append([]bench.Moment(nil), sink.moments...)
	sort.SliceStable(moments, func(i, j int) bool { return moments[i].AtMS < moments[j].AtMS })
	agent := make([]bench.TimedAudioChunk, len(sink.agent))
	copy(agent, sink.agent)
	utterances := make([]scenarioBenchUtterance, 0, len(sink.order))
	for _, id := range sink.order {
		utterances = append(utterances, *sink.utterances[id])
	}
	return bench.Transcript{Moments: moments}, bench.SessionAudioCapture{SampleRateHz: 24_000, Agent: agent}, utterances
}

// The judge.

type scenarioJudgeVerdict struct {
	Pass             bool     `json:"pass"`
	Reasons          []string `json:"reasons"`
	ErroneousActions []string `json:"erroneous_actions"`
	DuplicateActions []string `json:"duplicate_actions"`
}

func (verdict scenarioJudgeVerdict) describe() string {
	var lines []string
	for _, reason := range verdict.Reasons {
		lines = append(lines, "  reason: "+reason)
	}
	for _, action := range verdict.ErroneousActions {
		lines = append(lines, "  erroneous: "+action)
	}
	for _, action := range verdict.DuplicateActions {
		lines = append(lines, "  duplicate: "+action)
	}
	return strings.Join(lines, "\n")
}

// judgeScenarioRun asks Gemini to read the exchange and say whether the agent
// acted rightly, at the right moment, once.
func judgeScenarioRun(
	ctx context.Context, key string, item scenario.Scenario, run *scenarioBenchRun, result scenario.Result,
) (scenarioJudgeVerdict, error) {
	var prompt strings.Builder
	prompt.WriteString("You are reviewing a recorded conversation between a person (and sometimes a third party) and a voice agent. ")
	prompt.WriteString("The agent hears everything through one microphone. Times are milliseconds from the start of the recording.\n\n")
	fmt.Fprintf(&prompt, "Scenario: %s\nWhat it tests: %s\n", item.Name, item.Note)
	fmt.Fprintf(&prompt, "The agent's instructions: %q\n", item.Instructions)
	for _, tool := range item.Tools {
		fmt.Fprintf(&prompt, "The agent has a tool %s(%s): %s\n", tool.Name, strings.Join(tool.Parameters, ", "), tool.Description)
	}
	prompt.WriteString("\nThe script, with when each line was actually spoken:\n")
	for _, line := range run.lines {
		fmt.Fprintf(&prompt, "  %6d-%6dms  %s says: %q\n", line.startMS, line.endMS, line.speaker, line.text)
	}
	for index, sight := range item.Sees {
		fmt.Fprintf(&prompt, "  %6dms  the agent is shown picture %d (%s): %s\n", sight.AtMS, index+1, filepath.Base(sight.Path), sight.Note)
	}
	fmt.Fprintf(&prompt, "The recording ends at %dms.\n", run.timeline.TotalMS)
	prompt.WriteString("\nWhat the scenario requires (each check, in the author's words):\n")
	for _, check := range item.Checks {
		fmt.Fprintf(&prompt, "  - [%s] %s\n", check.Kind, check.Note)
	}
	prompt.WriteString("\nWhat the agent did, in order:\n")
	if len(run.events) == 0 {
		prompt.WriteString("  (the agent did nothing at all)\n")
	}
	for _, event := range run.events {
		prompt.WriteString("  " + event + "\n")
	}
	prompt.WriteString("\nThe automatic checks reported: ")
	if len(result.Failures) == 0 {
		prompt.WriteString("all passed.\n")
	} else {
		prompt.WriteString("\n")
		for _, failure := range result.Failures {
			prompt.WriteString("  - " + failure + "\n")
		}
	}
	prompt.WriteString("\nJudge the run. Did the agent take the right action at the right moment, and only once? ")
	prompt.WriteString("Treat as erroneous: speaking when the scenario asked for silence, answering a question that was not for it, ")
	prompt.WriteString("acting before the thing it should react to had happened, saying something wrong, or pressing the wrong key. ")
	prompt.WriteString("Treat as duplicate: the same count, correction, translation, key press or answer given twice for one occurrence. ")
	prompt.WriteString("A brief acknowledgement right after the person sets a rule (such as \"I will\") is acceptable and is not an error. ")
	prompt.WriteString("An agent that says nothing when the scenario wanted speech has not acted rightly. ")
	prompt.WriteString("Answer as JSON with fields: pass (boolean), reasons (list of short strings explaining the verdict), ")
	prompt.WriteString("erroneous_actions (list of strings, empty if none), duplicate_actions (list of strings, empty if none).\n")

	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": prompt.String()}}}},
		"generationConfig": map[string]any{
			"responseMimeType": "application/json", "temperature": 0,
			"responseSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pass":              map[string]any{"type": "boolean"},
					"reasons":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"erroneous_actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"duplicate_actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"pass", "reasons", "erroneous_actions", "duplicate_actions"},
			},
		},
	})
	if err != nil {
		return scenarioJudgeVerdict{}, err
	}
	model := envOr("OPENREALTIME_SCENARIO_JUDGE_MODEL", "gemini-3.7-flash")
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return scenarioJudgeVerdict{}, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-goog-api-key", key)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("judge returned %s: %s", response.Status, truncateJudge(payload))
			if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
				return scenarioJudgeVerdict{}, lastErr
			}
			time.Sleep(3 * time.Second)
			continue
		}
		var reply struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(payload, &reply); err != nil {
			return scenarioJudgeVerdict{}, fmt.Errorf("decode judge reply: %w (%s)", err, truncateJudge(payload))
		}
		var text strings.Builder
		for _, candidate := range reply.Candidates {
			for _, part := range candidate.Content.Parts {
				text.WriteString(part.Text)
			}
		}
		var verdict scenarioJudgeVerdict
		if err := json.Unmarshal([]byte(strings.TrimSpace(text.String())), &verdict); err != nil {
			return scenarioJudgeVerdict{}, fmt.Errorf("judge did not answer in JSON: %w (%s)", err, truncateJudge([]byte(text.String())))
		}
		return verdict, nil
	}
	return scenarioJudgeVerdict{}, lastErr
}

func truncateJudge(payload []byte) string {
	text := strings.Join(strings.Fields(string(payload)), " ")
	if len(text) > 300 {
		return text[:300] + "…"
	}
	return text
}

func TestScenarioBenchSentencesSplitWhereAVoicePauses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		text string
		want []string
	}{
		{"Count out loud from one to forty for me, slowly, one number at a time, and don't say anything else.",
			[]string{"Count out loud from one to forty for me, slowly, one number at a time, and don't say anything else."}},
		{"Did you get the milk on the way in? I looked in the fridge and there wasn't any.",
			[]string{"Did you get the milk on the way in?", "I looked in the fridge and there wasn't any."}},
		{"It costs 3.5 dollars. Fine!", []string{"It costs 3.5 dollars.", "Fine!"}},
		{"你好，很高兴见到你。", []string{"你好，很高兴见到你。"}},
		{"好的。我们明天见面。", []string{"好的。", "我们明天见面。"}},
		{"Right, carry on", []string{"Right, carry on"}},
	} {
		if got := scenarioBenchSentences(test.text); !slices.Equal(got, test.want) {
			t.Errorf("sentences(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}
