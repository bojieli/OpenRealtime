package sidecarbinding_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/duplex"
	"github.com/bojieli/OpenRealtime/binding/omni"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type scriptedSlow struct {
	mu    sync.Mutex
	turns [][]continuation.Event
	calls int
}

func (provider *scriptedSlow) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh,
		ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func (provider *scriptedSlow) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	var events []continuation.Event
	if index < len(provider.turns) {
		events = provider.turns[index]
	}
	provider.mu.Unlock()
	for _, event := range events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

type collectingSink struct {
	mu          sync.Mutex
	transcripts []binding.TranscriptEvent
	audioFrames int
	spoken      []string
	toolCalls   []binding.ToolCallEvent
	activity    []binding.ActivityEvent
	failures    []binding.ErrorEvent
}

func (sink *collectingSink) TurnBegin(context.Context) error                    { return nil }
func (sink *collectingSink) TurnEnd(context.Context, binding.TurnOutcome) error { return nil }

func (sink *collectingSink) Activity(_ context.Context, event binding.ActivityEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.activity = append(sink.activity, event)
	return nil
}
func (sink *collectingSink) Transcript(_ context.Context, event binding.TranscriptEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.transcripts = append(sink.transcripts, event)
	return nil
}
func (sink *collectingSink) Observation(context.Context, perception.Observation) error { return nil }
func (sink *collectingSink) SpeechBegin(context.Context, action.Utterance) error       { return nil }
func (sink *collectingSink) SpeechText(_ context.Context, _ action.Utterance, delta string) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.spoken = append(sink.spoken, delta)
	return nil
}

func (sink *collectingSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.audioFrames++
	return nil
}
func (sink *collectingSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (sink *collectingSink) ToolCalls(_ context.Context, event binding.ToolCallEvent) error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.toolCalls = append(sink.toolCalls, event)
	return nil
}
func (sink *collectingSink) Failed(_ context.Context, event binding.ErrorEvent) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	sink.failures = append(sink.failures, event)
}

// buildFakeSidecar compiles a sidecar that speaks the protocol and records
// what it was sent, so the binding can be tested without a model.
func buildFakeSidecar(t *testing.T) (binary, transcript string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte(fakeSidecarSource), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"),
		[]byte("module fakesidecar\n\ngo 1.25.0\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	binary = filepath.Join(directory, "fakesidecar")
	build := exec.Command(goBinary(t), "build", "-o", binary, ".")
	build.Dir = directory
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return binary, filepath.Join(directory, "received.jsonl")
}

func goBinary(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/usr/local/go/bin/go", "go"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no Go toolchain available")
	return ""
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		if condition() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(message)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func sidecarReceived(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var messages []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(line), &decoded) == nil {
			messages = append(messages, decoded)
		}
	}
	return messages
}

func TestOmniKeepsTheFloorInTheEngine(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	bind, err := omni.New(omni.Config{
		Sidecar: sidecar.Config{
			Command: []string{binary}, Environment: []string{"FAKE_SIDECAR_LOG=" + received},
		},
		Slow: slow,
	})
	if err != nil {
		t.Fatalf("new omni: %v", err)
	}
	ownership := bind.Ownership()
	if ownership.Floor != binding.OwnerEngine {
		t.Fatalf("the engine keeps the floor for omni: %+v", ownership)
	}
	if ownership.FastCognition != binding.OwnerModel || ownership.SlowCognition != binding.OwnerEngine {
		t.Fatalf("unexpected ownership %+v", ownership)
	}

	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{Sink: sink, SessionID: "test"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runtime.Close(context.Background(), nil)

	// Speech, then silence: the engine's own gate decides the turn ended and
	// tells the model to respond.
	loud := make([]byte, 4800)
	for index := 0; index < len(loud); index += 2 {
		loud[index+1] = 0x40
	}
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: loud,
		}); err != nil {
			t.Fatalf("audio: %v", err)
		}
	}
	quiet := make([]byte, 4800)
	for index := 0; index < 8; index++ {
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: quiet,
		}); err != nil {
			t.Fatalf("silence: %v", err)
		}
	}
	waitFor(t, func() bool {
		for _, message := range sidecarReceived(t, received) {
			if message["type"] == "respond" {
				return true
			}
		}
		return false
	}, "the engine's floor must decide when the model takes its turn")

	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.audioFrames > 0 && len(sink.transcripts) > 0
	}, "the model's turn must reach the client")

	// The reasoner works over the same conversation and hands its answer back.
	waitFor(t, func() bool {
		for _, message := range sidecarReceived(t, received) {
			if message["type"] == "text" && strings.Contains(message["text"].(string), "$40.00") {
				return true
			}
		}
		return false
	}, "the background reasoner's answer must be handed to the model")
}

func TestDuplexGivesTheModelItsFloor(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	// The reasoner has to actually produce an answer for this test to mean
	// anything. With nothing to hand over there is nothing that could have
	// demanded a turn, and the assertion below would hold no matter what the
	// hand-off did.
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."},
	}}}
	bind, err := duplex.New(duplex.Config{
		Sidecar: sidecar.Config{
			Command:     []string{binary},
			Environment: []string{"FAKE_SIDECAR_LOG=" + received, "FAKE_SIDECAR_DUPLEX=1"},
		},
		Slow: slow,
	})
	if err != nil {
		t.Fatalf("new duplex: %v", err)
	}
	if bind.Ownership().Floor != binding.OwnerModel {
		t.Fatal("a full-duplex model owns its floor")
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{Sink: sink, SessionID: "test"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runtime.Close(context.Background(), nil)

	loud := make([]byte, 4800)
	for index := 0; index < len(loud); index += 2 {
		loud[index+1] = 0x40
	}
	quiet := make([]byte, 4800)
	ctx := context.Background()
	for index := 0; index < 12; index++ {
		payload := loud
		if index >= 3 {
			payload = quiet
		}
		if err := runtime.Audio(ctx, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000, PCM16LE: payload,
		}); err != nil {
			t.Fatalf("audio: %v", err)
		}
	}
	// The answer reaches the model: a duplex model has no background reasoner
	// of its own, and borrowing one is the whole of what this binding adds.
	waitFor(t, func() bool {
		for _, message := range sidecarReceived(t, received) {
			text, isText := message["text"].(string)
			if message["type"] == "text" && isText && strings.Contains(text, "$40.00") {
				return true
			}
		}
		return false
	}, "the background reasoner's answer must be handed to the model")

	// And it arrives as context rather than as an instruction to speak. The
	// model decides for itself when to say what it now knows; demanding a turn
	// would take back the floor it owns, which is the one thing this binding
	// must never do.
	time.Sleep(200 * time.Millisecond)
	for _, message := range sidecarReceived(t, received) {
		if message["type"] == "respond" {
			t.Fatal("the engine must not demand turns from a model that owns its floor")
		}
	}
}

// The fast provider cannot call tools. That does not stop being true because
// the provider happens to live in another process.
func TestModelToolCallsAreRefusedWithAReason(t *testing.T) {
	binary, received := buildFakeSidecar(t)
	bind, err := omni.New(omni.Config{
		Sidecar: sidecar.Config{
			Command: []string{binary},
			Environment: []string{
				"FAKE_SIDECAR_LOG=" + received, "FAKE_SIDECAR_TOOL_CALL=1",
			},
		},
		Slow: &scriptedSlow{},
	})
	if err != nil {
		t.Fatalf("new omni: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{
		Sink: sink, SessionID: "test",
		Settings: binding.Settings{Tools: []action.ToolSpec{{
			Name: "get_balance", Description: "read a balance",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}}},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer runtime.Close(context.Background(), nil)

	if err := runtime.CreateResponse(context.Background()); err != nil {
		t.Fatalf("respond: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range sidecarReceived(t, received) {
			if message["type"] != "tool_result" {
				continue
			}
			failure, _ := message["error"].(string)
			if strings.Contains(failure, "execution authority") {
				return true
			}
		}
		return false
	}, "the model's call must be refused, with a reason it can see")
	sink.mu.Lock()
	toolCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if toolCalls != 0 {
		t.Fatal("the model's proposal must not reach the client as an executable call")
	}
}

func TestABindingWithoutAReasonerIsRefused(t *testing.T) {
	if _, err := omni.New(omni.Config{
		Sidecar: sidecar.Config{Command: []string{"true"}},
	}); err == nil {
		t.Fatal("the background reasoner is what this binding adds and must be required")
	}
	if _, err := omni.New(omni.Config{Slow: &scriptedSlow{}}); err == nil {
		t.Fatal("a binding with no sidecar has no model")
	}
}

const fakeSidecarSource = `package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
)

type message struct {
	Type         string          ` + "`json:\"type\"`" + `
	PayloadBytes int             ` + "`json:\"payload_bytes,omitempty\"`" + `
	Version      int             ` + "`json:\"version,omitempty\"`" + `
	Model        string          ` + "`json:\"model,omitempty\"`" + `
	SampleRate   int             ` + "`json:\"sample_rate,omitempty\"`" + `
	OutputRate   int             ` + "`json:\"output_rate,omitempty\"`" + `
	Capabilities []string        ` + "`json:\"capabilities,omitempty\"`" + `
	Text         string          ` + "`json:\"text,omitempty\"`" + `
	Final        bool            ` + "`json:\"final,omitempty\"`" + `
	CallID       string          ` + "`json:\"call_id,omitempty\"`" + `
	Name         string          ` + "`json:\"name,omitempty\"`" + `
	Arguments    json.RawMessage ` + "`json:\"arguments,omitempty\"`" + `
	Error        string          ` + "`json:\"error,omitempty\"`" + `
	Role         string          ` + "`json:\"role,omitempty\"`" + `
}

func main() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	var log *os.File
	if path := os.Getenv("FAKE_SIDECAR_LOG"); path != "" {
		log, _ = os.Create(path)
		defer log.Close()
	}
	heard := 0
	send := func(header message, payload []byte) {
		header.PayloadBytes = len(payload)
		encoded, _ := json.Marshal(header)
		writer.Write(append(encoded, '\n'))
		if len(payload) > 0 {
			writer.Write(payload)
		}
		writer.Flush()
	}

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var incoming message
		if json.Unmarshal(line, &incoming) != nil {
			continue
		}
		if incoming.PayloadBytes > 0 {
			if _, err := io.ReadFull(reader, make([]byte, incoming.PayloadBytes)); err != nil {
				return
			}
		}
		if log != nil && incoming.Type != "audio" {
			log.Write(line)
			log.Sync()
		}
		// A full-duplex model owns its floor, so it reports what it heard on
		// its own initiative rather than when the engine asks. Nothing else in
		// this fake would ever produce a transcript without a respond, and a
		// duplex binding never sends one - so without this the reasoner would
		// have no user speech to work over and every duplex assertion below
		// would hold vacuously.
		if incoming.Type == "audio" && os.Getenv("FAKE_SIDECAR_DUPLEX") != "" {
			heard++
			if heard == 3 {
				send(message{Type: "speech_started"}, nil)
			}
			if heard == 6 {
				send(message{Type: "transcript", Text: "what is my balance", Final: true}, nil)
				send(message{Type: "speech_stopped"}, nil)
			}
			continue
		}
		switch incoming.Type {
		case "hello":
			send(message{
				Type: "ready", Version: 1, Model: "fake-omni", OutputRate: 24000,
				Capabilities: []string{"transcript", "text_injection", "tools"},
			}, nil)
		case "respond":
			if os.Getenv("FAKE_SIDECAR_TOOL_CALL") != "" {
				send(message{
					Type: "tool_call", CallID: "c1", Name: "get_balance",
					Arguments: json.RawMessage("{}"),
				}, nil)
				continue
			}
			send(message{Type: "transcript", Text: "what is my balance", Final: true}, nil)
			send(message{Type: "text_delta", Text: "Let me check."}, nil)
			send(message{Type: "text_done", Text: "Let me check."}, nil)
			send(message{Type: "output_audio"}, make([]byte, 4800))
			send(message{Type: "turn_done"}, nil)
		case "bye":
			return
		}
	}
}
`

// The cascade shipped a step that recited another provider's text and withdrew
// it, because a phase told to say what someone else wrote performs it rather
// than speaking - and when it loses the referent it reads its own last turn
// back instead. Three of the four bindings inherited the design and not the
// lesson.
func TestTheHandOffStatesTheResultRatherThanDictatingIt(t *testing.T) {
	handed := sidecarbinding.HandOffText("The order ABC123 is out for delivery and arrives tomorrow.")
	lowered := strings.ToLower(handed)
	for _, dictation := range []string{"say this", "read this", "repeat this", "add nothing"} {
		if strings.Contains(lowered, dictation) {
			t.Errorf("the hand-off dictates with %q: %s", dictation, handed)
		}
	}
	if !strings.Contains(lowered, "your own words") {
		t.Error("the model has to be told to speak rather than perform")
	}
	if !strings.Contains(handed, "ABC123") {
		t.Error("the result itself has to be present, or there is no referent to speak from")
	}
	if !strings.Contains(lowered, "exactly") {
		t.Error("identifiers still have to survive the retelling")
	}
}
