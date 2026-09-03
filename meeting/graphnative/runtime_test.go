package graphnative

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	videograph "github.com/bojieli/OpenRealtime/elements/video"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestScreenForkPublishesDirectAndAdaptiveEvidenceFromOneFrame(t *testing.T) {
	input := newTestInput("video", modelelements.VideoInputType())
	outputs := map[string]element.OutputPort{
		"foreground": newTestOutput("foreground", modelelements.VideoInputType()),
		"source":     newTestOutput("source", videograph.SourceStartType()),
		"frame":      newTestOutput("frame", videograph.InlineFrameType()),
		"tick":       newTestOutput("tick", videograph.TimingTickType()),
		"end":        newTestOutput("end", videograph.SourceEndType()),
	}
	reporter := &testResolution{}
	runner := &screenForkRunner{
		config: ScreenForkConfig{Source: "screen", Kind: videograph.SourceScreen, MaxFrameBytes: 1024},
		input:  input, outputs: outputs, resolution: reporter,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	frame := modelelements.VideoInputFrame{
		StreamID: "screen-stream", FrameRateMilliHz: 5_000,
		Frame: perception.Frame{
			Kind: perception.FrameImage, Source: "screen", CapturedNS: 1_000,
			Index: 1, Image: []byte{1, 2, 3}, MIMEType: "image/jpeg", Width: 2, Height: 2,
		},
	}
	input.send(t, element.Envelope{
		Type: modelelements.VideoInputType(), ItemID: "screen-1", SessionID: "meeting-session",
		CaptureNS: frame.Frame.CapturedNS, Payload: frame,
	})

	source := outputEnvelope(t, outputs["source"])
	started, ok := source.Payload.(videograph.SourceStart)
	if !ok || started.Source != "screen" || started.StreamID != frame.StreamID ||
		started.SourceRevision != 1 || started.OpenedNS != frame.Frame.CapturedNS {
		t.Fatalf("screen source = %#v", source.Payload)
	}
	foreground := outputEnvelope(t, outputs["foreground"])
	direct, ok := foreground.Payload.(modelelements.VideoInputFrame)
	if !ok || direct.StreamID != frame.StreamID || len(direct.Frame.Image) != 3 {
		t.Fatalf("direct frame = %#v", foreground.Payload)
	}
	observed := outputEnvelope(t, outputs["frame"])
	inline, ok := observed.Payload.(videograph.InlineFrame)
	if !ok || inline.SourceRevision != 1 || inline.Frame.Index != 1 {
		t.Fatalf("adaptive frame = %#v", observed.Payload)
	}
	frame.Frame.Image[0] = 9
	if direct.Frame.Image[0] != 1 || inline.Frame.Image[0] != 1 {
		t.Fatal("screen fork retained mutable caller-owned image storage")
	}
	tick := outputEnvelope(t, outputs["tick"])
	clock, ok := tick.Payload.(videograph.TimingTick)
	if !ok || clock.NowNS != frame.Frame.CapturedNS || clock.SourceRevision != 1 {
		t.Fatalf("adaptive tick = %#v", tick.Payload)
	}
	if reporter.runtimeID != screenForkRuntimeID || reporter.runtimeRevision != runtimeRevision ||
		reporter.capabilityReports != 1 {
		t.Fatalf("screen live resolution = %+v", reporter)
	}

	cancel()
	if err := awaitRunner(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestScreenForkRejectsInvalidCaptureClockBeforeAnyEffect(t *testing.T) {
	outputs := map[string]element.OutputPort{
		"foreground": newTestOutput("foreground", modelelements.VideoInputType()),
		"source":     newTestOutput("source", videograph.SourceStartType()),
		"frame":      newTestOutput("frame", videograph.InlineFrameType()),
		"tick":       newTestOutput("tick", videograph.TimingTickType()),
		"end":        newTestOutput("end", videograph.SourceEndType()),
	}
	runner := &screenForkRunner{
		config:  ScreenForkConfig{Source: "screen", Kind: videograph.SourceScreen, MaxFrameBytes: 1024},
		outputs: outputs,
	}
	err := runner.accept(context.Background(), element.Envelope{
		Type: modelelements.VideoInputType(), ItemID: "clock-mismatch", SessionID: "meeting-session",
		CaptureNS: 99,
		Payload: modelelements.VideoInputFrame{
			StreamID: "screen-stream", FrameRateMilliHz: 5_000,
			Frame: perception.Frame{
				Kind: perception.FrameImage, Source: "screen", CapturedNS: 100,
				Index: 1, Image: []byte{1}, MIMEType: "image/jpeg", Width: 1, Height: 1,
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "externally clocked image") {
		t.Fatalf("capture mismatch error = %v", err)
	}
	for name, output := range outputs {
		if got := len(output.(*testOutput).values); got != 0 {
			t.Fatalf("invalid frame reached %s (%d envelopes)", name, got)
		}
	}
}

func TestScreenForkClosesReplacedStreamWithExactRevision(t *testing.T) {
	input := newTestInput("video", modelelements.VideoInputType())
	outputs := map[string]element.OutputPort{
		"foreground": newTestOutput("foreground", modelelements.VideoInputType()),
		"source":     newTestOutput("source", videograph.SourceStartType()),
		"frame":      newTestOutput("frame", videograph.InlineFrameType()),
		"tick":       newTestOutput("tick", videograph.TimingTickType()),
		"end":        newTestOutput("end", videograph.SourceEndType()),
	}
	runner := &screenForkRunner{
		config: ScreenForkConfig{Source: "screen", Kind: videograph.SourceScreen, MaxFrameBytes: 1024},
		input:  input, outputs: outputs, resolution: &testResolution{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	sendScreenFrame(t, input, "stream-one", 1, 1)
	for _, name := range []string{"source", "foreground", "frame", "tick"} {
		_ = outputEnvelope(t, outputs[name])
	}
	replacement := element.Envelope{
		Type: modelelements.VideoInputType(), ItemID: "stream-two", SessionID: "meeting-session", CaptureNS: 2,
		Payload: modelelements.VideoInputFrame{
			StreamID: "replacement-stream", FrameRateMilliHz: 5_000,
			Frame: perception.Frame{
				Kind: perception.FrameImage, Source: "screen", CapturedNS: 2,
				Index: 0, Image: []byte{2}, MIMEType: "image/jpeg", Width: 1, Height: 1,
			},
		},
	}
	input.send(t, replacement)
	ended := outputEnvelope(t, outputs["end"]).Payload.(videograph.SourceEnd)
	started := outputEnvelope(t, outputs["source"]).Payload.(videograph.SourceStart)
	if ended.StreamID != "screen-stream" || ended.SourceRevision != 1 ||
		ended.Reason != "stream-replaced" || started.StreamID != "replacement-stream" ||
		started.SourceRevision != 2 {
		t.Fatalf("replacement end/start = %+v / %+v", ended, started)
	}
	for _, name := range []string{"foreground", "frame", "tick"} {
		_ = outputEnvelope(t, outputs[name])
	}
	cancel()
	if err := awaitRunner(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestScreenForkRejectsClockRegressionBeforePublishingAnotherFrame(t *testing.T) {
	input := newTestInput("video", modelelements.VideoInputType())
	outputs := map[string]element.OutputPort{
		"foreground": newTestOutput("foreground", modelelements.VideoInputType()),
		"source":     newTestOutput("source", videograph.SourceStartType()),
		"frame":      newTestOutput("frame", videograph.InlineFrameType()),
		"tick":       newTestOutput("tick", videograph.TimingTickType()),
		"end":        newTestOutput("end", videograph.SourceEndType()),
	}
	runner := &screenForkRunner{
		config: ScreenForkConfig{Source: "screen", Kind: videograph.SourceScreen, MaxFrameBytes: 1024},
		input:  input, outputs: outputs, resolution: &testResolution{},
	}
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background()) }()
	sendScreenFrame(t, input, "first", 2, 2)
	for _, name := range []string{"source", "foreground", "frame", "tick"} {
		_ = outputEnvelope(t, outputs[name])
	}
	sendScreenFrame(t, input, "regressed", 1, 3)
	err := awaitRunner(t, done)
	if err == nil || !strings.Contains(err.Error(), "regresses stream time/index") {
		t.Fatalf("screen regression error = %v", err)
	}
	for _, name := range []string{"foreground", "frame", "tick"} {
		if got := len(outputs[name].(*testOutput).values); got != 0 {
			t.Fatalf("regressed frame reached %s (%d envelopes)", name, got)
		}
	}
}

func TestBackgroundInjectionPublishesOnlyCompleteBoundedText(t *testing.T) {
	input := newTestInput("text", interactionelements.SafePreparedTextType())
	outputs := map[string]element.OutputPort{
		"injection": newTestOutput("injection", modelelements.TextInputType()),
		"trigger":   newTestOutput("trigger", modelelements.GenerateType()),
		"outcome":   newTestOutput("outcome", BackgroundOutcomeType()),
	}
	runner := &backgroundInjectionRunner{
		config: BackgroundInjectionConfig{
			Role: "background", Instruction: "Use the grounded background result.",
			MaxOutputTokens: 128, MaxRuns: 2, MaxTextBytes: 32, TerminalMemory: 4,
		},
		input: input, outputs: outputs, resolution: &testResolution{},
		active: make(map[string]*backgroundText), terminal: newBoundedIdentifiers(4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	for index, delta := range []cognitionelements.PreparedTextDelta{
		{Boundary: cognitionelements.TextBegin, Index: 0},
		{Boundary: cognitionelements.TextChunk, Index: 1},
		{Boundary: cognitionelements.TextChunk, Index: 2, Text: "grounded "},
		{Boundary: cognitionelements.TextEnd, Index: 3, Text: "result"},
	} {
		input.send(t, preparedEnvelope("slow-run", index, delta))
	}
	injected := outputEnvelope(t, outputs["injection"])
	payload, ok := injected.Payload.(ContextInjection)
	if !ok || payload.Text != "grounded result" || payload.Role != "background" ||
		payload.Source != "meeting.background" {
		t.Fatalf("background injection = %#v", injected.Payload)
	}
	trigger := outputEnvelope(t, outputs["trigger"])
	generate, ok := trigger.Payload.(cognitionelements.Generate)
	if !ok || generate.Invocation.Instruction != runner.config.Instruction ||
		generate.Invocation.MaxOutputTokens != 128 {
		t.Fatalf("background trigger = %#v", trigger.Payload)
	}
	if !slices.Contains(trigger.CausalParents, injected.ItemID) ||
		!slices.Contains(injected.CausalParents, "slow-run-0") {
		t.Fatalf("background causal chain injection=%v trigger=%v",
			injected.CausalParents, trigger.CausalParents)
	}
	outcome := outputEnvelope(t, outputs["outcome"])
	audit, ok := outcome.Payload.(BackgroundInjectionOutcome)
	if !ok || audit.Kind != BackgroundInjected || audit.Bytes != len("grounded result") {
		t.Fatalf("background outcome = %#v", outcome.Payload)
	}

	// A replay is terminal evidence only; it never repeats the injection or
	// response trigger.
	input.send(t, preparedEnvelope("slow-run", 3,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextEnd, Index: 2}))
	replay := outputEnvelope(t, outputs["outcome"])
	if replay.Payload.(BackgroundInjectionOutcome).Kind != BackgroundIgnored {
		t.Fatalf("background replay = %#v", replay.Payload)
	}
	if len(outputs["injection"].(*testOutput).values) != 0 ||
		len(outputs["trigger"].(*testOutput).values) != 0 {
		t.Fatal("terminal background replay crossed an effect boundary")
	}

	cancel()
	if err := awaitRunner(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundInjectionWithholdsInterruptedAndOversizedStreams(t *testing.T) {
	for _, test := range []struct {
		name   string
		frames []cognitionelements.PreparedTextDelta
		kind   BackgroundOutcomeKind
	}{
		{
			name: "interrupted",
			frames: []cognitionelements.PreparedTextDelta{
				{Boundary: cognitionelements.TextBegin, Index: 0},
				{Boundary: cognitionelements.TextChunk, Index: 1, Text: "partial"},
				{Boundary: cognitionelements.TextEnd, Index: 2, Interrupted: true},
			},
			kind: BackgroundInterrupted,
		},
		{
			name: "oversized",
			frames: []cognitionelements.PreparedTextDelta{
				{Boundary: cognitionelements.TextBegin, Index: 0},
				{Boundary: cognitionelements.TextChunk, Index: 1, Text: "too-large"},
			},
			kind: BackgroundRejected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := newTestInput("text", interactionelements.SafePreparedTextType())
			outputs := map[string]element.OutputPort{
				"injection": newTestOutput("injection", modelelements.TextInputType()),
				"trigger":   newTestOutput("trigger", modelelements.GenerateType()),
				"outcome":   newTestOutput("outcome", BackgroundOutcomeType()),
			}
			maxTextBytes := 4
			if test.name == "interrupted" {
				maxTextBytes = 16
			}
			runner := &backgroundInjectionRunner{
				config: BackgroundInjectionConfig{
					Role: "background", Instruction: "Use it.", MaxOutputTokens: 16,
					MaxRuns: 1, MaxTextBytes: maxTextBytes, TerminalMemory: 2,
				},
				input: input, outputs: outputs, resolution: &testResolution{},
				active: make(map[string]*backgroundText), terminal: newBoundedIdentifiers(2),
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			for index, delta := range test.frames {
				input.send(t, preparedEnvelope("bounded", index, delta))
			}
			outcome := outputEnvelope(t, outputs["outcome"])
			if got := outcome.Payload.(BackgroundInjectionOutcome).Kind; got != test.kind {
				t.Fatalf("outcome kind = %s, want %s", got, test.kind)
			}
			if len(outputs["injection"].(*testOutput).values) != 0 ||
				len(outputs["trigger"].(*testOutput).values) != 0 {
				t.Fatal("unsafe background stream crossed an effect boundary")
			}
			cancel()
			if err := awaitRunner(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackgroundInjectionDoesNotReflectInvalidRunIDsOrMalformedText(t *testing.T) {
	input := newTestInput("text", interactionelements.SafePreparedTextType())
	outputs := map[string]element.OutputPort{
		"injection": newTestOutput("injection", modelelements.TextInputType()),
		"trigger":   newTestOutput("trigger", modelelements.GenerateType()),
		"outcome":   newTestOutput("outcome", BackgroundOutcomeType()),
	}
	runner := &backgroundInjectionRunner{
		config: BackgroundInjectionConfig{
			Role: "background", Instruction: "Use it.", MaxOutputTokens: 16,
			MaxRuns: 2, MaxTextBytes: 32, TerminalMemory: 4,
		},
		input: input, outputs: outputs, resolution: &testResolution{},
		active: make(map[string]*backgroundText), terminal: newBoundedIdentifiers(4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	untrusted := strings.Repeat("x", 4096)
	invalidRun := preparedEnvelope(untrusted, 0,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0})
	invalidRun.ItemID = "invalid-run-frame"
	input.send(t, invalidRun)
	invalidEnvelope := outputEnvelope(t, outputs["outcome"])
	invalidID := invalidEnvelope.Payload.(BackgroundInjectionOutcome)
	if invalidID.Code != "invalid_run_id" || invalidID.RunID != "" ||
		invalidEnvelope.RunID != "" || strings.Contains(invalidID.Message, untrusted) {
		t.Fatalf("invalid run ID outcome leaked untrusted identity: %+v", invalidID)
	}

	input.send(t, preparedEnvelope("malformed", 0,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
	input.send(t, preparedEnvelope("malformed", 1,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextChunk, Index: 1, Text: "bad\x00text"}))
	malformed := outputEnvelope(t, outputs["outcome"]).Payload.(BackgroundInjectionOutcome)
	if malformed.Kind != BackgroundRejected || malformed.Code != "invalid_delta" {
		t.Fatalf("malformed text outcome = %+v", malformed)
	}
	if len(outputs["injection"].(*testOutput).values) != 0 ||
		len(outputs["trigger"].(*testOutput).values) != 0 {
		t.Fatal("malformed background input crossed an effect boundary")
	}

	cancel()
	if err := awaitRunner(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundInjectionTerminalizesMalformedPayloadForAnActiveRun(t *testing.T) {
	outputs := map[string]element.OutputPort{
		"injection": newTestOutput("injection", modelelements.TextInputType()),
		"trigger":   newTestOutput("trigger", modelelements.GenerateType()),
		"outcome":   newTestOutput("outcome", BackgroundOutcomeType()),
	}
	runner := &backgroundInjectionRunner{
		config: BackgroundInjectionConfig{
			Role: "background", Instruction: "Use it.", MaxOutputTokens: 16,
			MaxRuns: 2, MaxTextBytes: 32, TerminalMemory: 4,
		},
		outputs: outputs, active: make(map[string]*backgroundText),
		terminal: newBoundedIdentifiers(4),
	}
	ctx := context.Background()
	if err := runner.accept(ctx, preparedEnvelope("malformed-payload", 0,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0})); err != nil {
		t.Fatal(err)
	}
	malformed := preparedEnvelope("malformed-payload", 1,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextChunk, Index: 1, Text: "unused"})
	malformed.Payload = struct{ Value string }{Value: "not a prepared-text delta"}
	if err := runner.accept(ctx, malformed); err != nil {
		t.Fatal(err)
	}
	outcome := outputEnvelope(t, outputs["outcome"]).Payload.(BackgroundInjectionOutcome)
	if outcome.Kind != BackgroundRejected || outcome.Code != "invalid_payload" {
		t.Fatalf("malformed payload outcome = %+v", outcome)
	}
	if _, active := runner.active["malformed-payload"]; active ||
		!runner.terminal.contains("malformed-payload") {
		t.Fatal("malformed payload did not terminalize its active background run")
	}

	if err := runner.accept(ctx, preparedEnvelope("malformed-payload", 2,
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextEnd, Index: 1})); err != nil {
		t.Fatal(err)
	}
	replay := outputEnvelope(t, outputs["outcome"]).Payload.(BackgroundInjectionOutcome)
	if replay.Kind != BackgroundIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("post-malformation replay outcome = %+v", replay)
	}
	if len(outputs["injection"].(*testOutput).values) != 0 ||
		len(outputs["trigger"].(*testOutput).values) != 0 {
		t.Fatal("malformed active run crossed an injection or trigger boundary")
	}
}

func sendScreenFrame(t testing.TB, input *testInput, item string, captured, index uint64) {
	t.Helper()
	input.send(t, element.Envelope{
		Type: modelelements.VideoInputType(), ItemID: item, SessionID: "meeting-session",
		CaptureNS: captured,
		Payload: modelelements.VideoInputFrame{
			StreamID: "screen-stream", FrameRateMilliHz: 5_000,
			Frame: perception.Frame{
				Kind: perception.FrameImage, Source: "screen", CapturedNS: captured,
				Index: index, Image: []byte{1}, MIMEType: "image/jpeg", Width: 1, Height: 1,
			},
		},
	})
}

func preparedEnvelope(
	runID string, sequence int, delta cognitionelements.PreparedTextDelta,
) element.Envelope {
	return element.Envelope{
		Type: interactionelements.SafePreparedTextType(), ItemID: fmt.Sprintf("%s-%d", runID, sequence),
		SessionID: "meeting-session", RunID: runID, Sequence: uint64(sequence + 1), Payload: delta,
	}
}

type testInput struct {
	name   string
	typeOf element.Type
	values chan element.Envelope
}

func newTestInput(name string, valueType element.Type) *testInput {
	return &testInput{name: name, typeOf: valueType, values: make(chan element.Envelope, 32)}
}

func (input *testInput) Name() string              { return input.name }
func (input *testInput) Type() element.Type        { return input.typeOf.Clone() }
func (input *testInput) Lanes() []element.Receiver { return nil }
func (input *testInput) Receive(ctx context.Context) (element.Envelope, error) {
	select {
	case value := <-input.values:
		return value, nil
	case <-ctx.Done():
		return element.Envelope{}, ctx.Err()
	}
}
func (input *testInput) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
	value, err := input.Receive(ctx)
	return value, input.name, err
}
func (input *testInput) send(t testing.TB, value element.Envelope) {
	t.Helper()
	if err := value.ValidateFor(input.typeOf); err != nil {
		t.Fatal(err)
	}
	select {
	case input.values <- value:
	case <-time.After(time.Second):
		t.Fatal("test input blocked")
	}
}

type testOutput struct {
	name   string
	typeOf element.Type
	values chan element.Envelope
}

func newTestOutput(name string, valueType element.Type) *testOutput {
	return &testOutput{name: name, typeOf: valueType, values: make(chan element.Envelope, 32)}
}

func (output *testOutput) Name() string            { return output.name }
func (output *testOutput) Type() element.Type      { return output.typeOf.Clone() }
func (output *testOutput) Lanes() []element.Sender { return nil }
func (output *testOutput) Broadcast(
	ctx context.Context, value element.Envelope,
) (element.SendResult, error) {
	if err := value.ValidateFor(output.typeOf); err != nil {
		return element.SendResult{}, err
	}
	select {
	case output.values <- value:
		return element.SendResult{Delivered: 1}, nil
	case <-ctx.Done():
		return element.SendResult{}, ctx.Err()
	}
}

func outputEnvelope(t testing.TB, port element.OutputPort) element.Envelope {
	t.Helper()
	output := port.(*testOutput)
	select {
	case value := <-output.values:
		return value
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", output.name)
		return element.Envelope{}
	}
}

type testResolution struct {
	mu                   sync.Mutex
	runtimeID            string
	runtimeRevision      string
	runtimeDigest        string
	capabilityReports    int
	capabilityResolution []element.CapabilityResolution
}

func (resolution *testResolution) Runtime(id, revision, digest string) error {
	resolution.mu.Lock()
	defer resolution.mu.Unlock()
	if resolution.runtimeID != "" {
		return errors.New("runtime reported twice")
	}
	resolution.runtimeID, resolution.runtimeRevision, resolution.runtimeDigest = id, revision, digest
	return nil
}
func (resolution *testResolution) Capabilities(values []element.CapabilityResolution) error {
	resolution.mu.Lock()
	defer resolution.mu.Unlock()
	resolution.capabilityReports++
	resolution.capabilityResolution = append([]element.CapabilityResolution(nil), values...)
	return nil
}

func awaitRunner(t testing.TB, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop")
		return nil
	}
}
