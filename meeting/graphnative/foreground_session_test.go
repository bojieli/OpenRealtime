package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type foregroundTestBinding struct {
	name         string
	ownership    legacy.Ownership
	capabilities legacy.Capabilities
	runtime      *foregroundTestRuntime
	starts       atomic.Int32
}

func (binding *foregroundTestBinding) Name() string { return binding.name }

func (binding *foregroundTestBinding) Ownership() legacy.Ownership { return binding.ownership }

func (binding *foregroundTestBinding) Capabilities() legacy.Capabilities {
	result := binding.capabilities
	result.Observers = slices.Clone(binding.capabilities.Observers)
	return result
}

func (binding *foregroundTestBinding) Start(
	_ context.Context, options legacy.Options,
) (legacy.Runtime, error) {
	binding.starts.Add(1)
	binding.runtime.mu.Lock()
	binding.runtime.sink = options.Sink
	binding.runtime.mu.Unlock()
	return binding.runtime, nil
}

type foregroundTestRuntime struct {
	mu              sync.Mutex
	sink            legacy.Sink
	settings        legacy.Settings
	texts           []legacy.TextInput
	events          []string
	createResponses int
	closes          atomic.Int32
}

func (runtime *foregroundTestRuntime) Update(_ context.Context, settings legacy.Settings) error {
	runtime.mu.Lock()
	runtime.settings = legacy.CloneSettings(settings)
	runtime.events = append(runtime.events, "tools")
	runtime.mu.Unlock()
	return nil
}

func (runtime *foregroundTestRuntime) Audio(context.Context, perception.Frame) error {
	runtime.mu.Lock()
	runtime.events = append(runtime.events, "audio")
	runtime.mu.Unlock()
	return nil
}

func (*foregroundTestRuntime) Video(context.Context, perception.Frame) error { return nil }

func (runtime *foregroundTestRuntime) Text(_ context.Context, input legacy.TextInput) error {
	runtime.mu.Lock()
	input.Images = slices.Clone(input.Images)
	runtime.texts = append(runtime.texts, input)
	runtime.mu.Unlock()
	return nil
}

func (*foregroundTestRuntime) ToolResult(context.Context, trajectory.ToolResult) error { return nil }

func (*foregroundTestRuntime) CommitAudio(context.Context) error { return nil }

func (runtime *foregroundTestRuntime) CreateResponse(context.Context) error {
	runtime.mu.Lock()
	runtime.createResponses++
	runtime.mu.Unlock()
	return nil
}

func (*foregroundTestRuntime) Cancel(context.Context, string) error { return nil }

func (*foregroundTestRuntime) Truncate(context.Context, legacy.Truncation) error { return nil }

func (*foregroundTestRuntime) Trajectory() trajectory.Snapshot { return trajectory.Snapshot{} }

func (*foregroundTestRuntime) Status() legacy.Status { return legacy.Status{} }

func (runtime *foregroundTestRuntime) Close(context.Context, error) error {
	runtime.closes.Add(1)
	return nil
}

func (runtime *foregroundTestRuntime) snapshot() (legacy.Sink, int, []legacy.TextInput) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.sink, runtime.createResponses, slices.Clone(runtime.texts)
}

func (runtime *foregroundTestRuntime) eventSnapshot() []string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return slices.Clone(runtime.events)
}

type foregroundTestClientSink struct{}

func (foregroundTestClientSink) TurnBegin(context.Context) error                          { return nil }
func (foregroundTestClientSink) TurnEnd(context.Context, legacy.TurnOutcome) error        { return nil }
func (foregroundTestClientSink) Activity(context.Context, legacy.ActivityEvent) error     { return nil }
func (foregroundTestClientSink) Transcript(context.Context, legacy.TranscriptEvent) error { return nil }
func (foregroundTestClientSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (foregroundTestClientSink) SpeechBegin(context.Context, action.Utterance) error { return nil }
func (foregroundTestClientSink) SpeechText(context.Context, action.Utterance, string) error {
	return nil
}
func (foregroundTestClientSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (foregroundTestClientSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (foregroundTestClientSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (foregroundTestClientSink) Failed(context.Context, legacy.ErrorEvent)             {}

func foregroundTestArtifacts() map[string]inspect.ArtifactIdentity {
	return map[string]inspect.ArtifactIdentity{
		"adapter":             {ID: "plugin://openrealtime/meeting/session-adapter", Revision: "implementation:1"},
		"foreground":          {ID: "plugin://openrealtime/meeting/foreground", Revision: "implementation:1"},
		"foreground-provider": {ID: "model://openrealtime/meeting/foreground", Revision: "2026-08-30"},
		"foreground-runtime":  {ID: "runtime://openrealtime/meeting/foreground", Revision: "implementation:1"},
		"foreground-wire":     {ID: "adapter://openrealtime/meeting/foreground-wire", Revision: "implementation:1"},
		"visual":              {ID: "model://openrealtime/meeting/visual", Revision: "2026-08-30"},
		"background":          {ID: "model://openrealtime/meeting/background", Revision: "2026-08-30"},
	}
}

func foregroundTestOwnership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerModel, FastCognition: legacy.OwnerModel,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerModel,
		Interaction: legacy.OwnerModel, Floor: legacy.OwnerModel,
	}
}

func foregroundTestCapabilities() legacy.Capabilities {
	return legacy.Capabilities{
		Video: true, Observations: true, ManualTurns: true, MaxOutputTokens: 512,
		Observers: []string{"meeting.foreground.asr"},
		Voice:     legacy.VoiceControl{InForce: "meeting-voice"},
		Stack: legacy.StackCapabilities{
			AudioInput: true, AudioOutput: true, VisualInput: true,
			Transcription: true, TurnGeneration: true, ConcurrentIO: true,
			TextInjection: true,
		},
	}
}

func foregroundTestDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "meeting-fixture", Model: "foreground-fixture", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

func foregroundTestPlugin(t testing.TB) (*SessionPlugin, *foregroundTestRuntime, *foregroundTestBinding) {
	t.Helper()
	artifacts := foregroundTestArtifacts()
	runtime := &foregroundTestRuntime{}
	binding := &foregroundTestBinding{
		name: "meeting-fixture", ownership: foregroundTestOwnership(),
		capabilities: foregroundTestCapabilities(), runtime: runtime,
	}
	plugin, err := NewSessionPlugin(SessionPluginConfig{
		AdapterArtifact: artifacts["adapter"],
		Foreground: ForegroundPlugin{
			Artifact: artifacts["foreground"], ProviderArtifact: artifacts["foreground-provider"],
			RuntimeArtifact:     artifacts["foreground-runtime"],
			WireAdapterArtifact: artifacts["foreground-wire"], BindingName: binding.name,
			Ownership: binding.ownership, Capabilities: binding.capabilities,
			Descriptor: foregroundTestDescriptor(),
			Factory: func(context.Context, legacy.Options) (legacy.Binding, error) {
				return binding, nil
			},
		},
		Visual: VisualPlugin{
			Artifact: artifacts["visual"],
			Descriptor: perceptionelements.VisualProviderDescriptor{
				Name: "meeting-fixture-visual", Revision: "2026-08-30",
			},
			Factory: func(context.Context, legacy.Options) (perceptionelements.VisualProvider, error) {
				return nil, errors.New("visual fixture must stay lazy")
			},
		},
		Background: BackgroundPlugin{
			Artifact: artifacts["background"], Descriptor: continuation.Descriptor{
				Provider: "meeting-fixture", Model: "background-fixture", Phase: trajectory.PhaseSlow,
				Effort: continuation.EffortMinimal, Streaming: true,
				ToolAuthority:   continuation.ToolAuthorityPropose,
				SpeechAuthority: continuation.SpeechAuthoritySilent,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				return nil, errors.New("background fixture must stay lazy")
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plugin, runtime, binding
}

func foregroundTestHello(t testing.TB) sidecar.Message {
	t.Helper()
	descriptor := modelelements.StandardDescriptor()
	selected := make([]sidecar.PortSelection, 0, len(expectedForegroundPorts))
	for _, port := range descriptor.Ports {
		if _, found := expectedForegroundPorts[port.Name]; !found {
			continue
		}
		selection := sidecar.JSONPortSelection(port.Name, port.Direction, port.Type)
		mediaKind, media, err := sidecar.MediaKindForType(port.Type)
		if err != nil {
			t.Fatal(err)
		}
		if media {
			switch mediaKind {
			case sidecar.MediaAudio:
				selection.Formats = []sidecar.WireFormat{{
					PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 64 << 10,
					MaxBinaryBytes: 96 << 10, Media: &sidecar.MediaFormat{
						Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
						SampleRateHz: 24_000, Channels: 1, MaxFrameDurationMS: 2_000,
					},
				}}
			case sidecar.MediaVideo:
				selection.Formats = []sidecar.WireFormat{{
					PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 64 << 10,
					MaxBinaryBytes: 16 << 20, Media: &sidecar.MediaFormat{
						Kind: sidecar.MediaVideo, Encoding: "image/jpeg",
						MaxWidth: 4096, MaxHeight: 4096, MaxFrameRateMilliHz: 30_000,
					},
				}}
			}
		}
		selected = append(selected, selection)
	}
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &descriptor, ElementConfig: json.RawMessage(`{"profile":"meeting-fixture"}`),
		SelectedPorts: selected,
		RequiredCapabilities: []sidecar.CapabilityRequirement{
			{Name: "model.concurrent-io"},
			{Name: ForegroundCausalOrderingCapability, Contract: ForegroundCausalOrderingContract},
		},
	}
	if err := hello.Validate(); err != nil {
		t.Fatalf("foreground Hello fixture: %v", err)
	}
	return hello
}

func foregroundTestSession(t testing.TB) (*foregroundSession, *foregroundTestRuntime, *foregroundTestBinding) {
	t.Helper()
	plugin, runtime, binding := foregroundTestPlugin(t)
	created, err := plugin.newForegroundSession(context.Background(), foregroundTestHello(t), legacy.Options{
		SessionID: "meeting-session-1", Sink: foregroundTestClientSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, ok := created.(*foregroundSession)
	if !ok {
		t.Fatalf("foreground session type = %T", created)
	}
	configuration := foregroundTestInputMessage(t, foregroundTestHello(t), "tools",
		foregroundSessionSettings{Settings: legacy.Settings{}},
		element.Envelope{ItemID: "meeting-engine-tools", Sequence: 1})
	if err := session.Send(configuration); err != nil {
		t.Fatalf("configure foreground test session: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close foreground session: %v", err)
		}
	})
	return session, runtime, binding
}

func foregroundTestInputMessage(
	t testing.TB, hello sidecar.Message, port string, payload any, envelope element.Envelope,
) sidecar.Message {
	t.Helper()
	var valueType element.Type
	for _, selected := range hello.SelectedPorts {
		if selected.Name == port {
			valueType = selected.Type.Clone()
			break
		}
	}
	if valueType.Name == "" {
		t.Fatalf("Hello has no selected port %q", port)
	}
	encoded, err := modelelements.NewStandardJSONCodec().Encode(valueType, payload)
	if err != nil {
		t.Fatalf("encode %s: %v", port, err)
	}
	envelope.Type = valueType
	if envelope.ItemID == "" {
		envelope.ItemID = "meeting-engine-" + port
	}
	if envelope.SessionID == "" {
		envelope.SessionID = "meeting-session-1"
	}
	wire := sidecar.FromEnvelope(envelope, encoded.JSON)
	wire.Media = encoded.Media
	message := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: port, Envelope: &wire,
		Payload: encoded.Binary, PayloadBytes: len(encoded.Binary),
	}
	return message
}

func foregroundTestReadFrame(t testing.TB, session *foregroundSession) sidecar.Message {
	t.Helper()
	select {
	case frame := <-session.Frames():
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for foreground frame")
		return sidecar.Message{}
	}
}

func foregroundTestDecode(t testing.TB, frame sidecar.Message) any {
	t.Helper()
	decoded, err := modelelements.NewStandardJSONCodec().Decode(frame.Envelope.Type,
		modelelements.EncodedPayload{JSON: frame.Envelope.JSON, Binary: frame.Payload, Media: frame.Envelope.Media})
	if err != nil {
		t.Fatalf("decode %s: %v", frame.Port, err)
	}
	return decoded
}

func TestForegroundSessionNegotiatesExactLiveIdentitiesAndRejectsDriftBeforeFactory(t *testing.T) {
	plugin, _, binding := foregroundTestPlugin(t)
	hello := foregroundTestHello(t)
	session, err := plugin.newForegroundSession(context.Background(), hello, legacy.Options{
		SessionID: "meeting-session-1", Sink: foregroundTestClientSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready := session.Ready()
	if got, want := ready.RuntimeArtifact, sidecarArtifact(plugin.config.Foreground.RuntimeArtifact); got != want {
		t.Fatalf("runtime artifact = %+v, want %+v", got, want)
	}
	if _, err := sidecar.NegotiateElementSession(hello, ready); err != nil {
		t.Fatalf("ready does not bind exact Hello: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if got := binding.starts.Load(); got != 1 {
		t.Fatalf("binding starts = %d, want 1", got)
	}

	drift := hello.Clone()
	drift.SelectedPorts = drift.SelectedPorts[:len(drift.SelectedPorts)-1]
	if _, err := plugin.newForegroundSession(context.Background(), drift, legacy.Options{
		SessionID: "meeting-session-2", Sink: foregroundTestClientSink{},
	}); err == nil {
		t.Fatal("incomplete selected-port set was accepted")
	}
	if got := binding.starts.Load(); got != 1 {
		t.Fatalf("invalid Hello reached binding factory/start: %d", got)
	}
}

func TestForegroundSessionCausallyOrdersBackgroundInjectionBeforeGenerate(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	hello := foregroundTestHello(t)
	parent := "background-run/meeting-background-injection"
	trigger := foregroundTestInputMessage(t, hello, "trigger", cognitionelements.Generate{
		Invocation: continuation.Invocation{Instruction: "continue", MaxOutputTokens: 32},
	}, element.Envelope{
		ItemID: "trigger-1", RunID: "foreground-run-1", CausalParents: []string{parent},
	})
	if err := session.Send(trigger); err != nil {
		t.Fatal(err)
	}
	if _, calls, _ := runtime.snapshot(); calls != 0 {
		t.Fatalf("Generate crossed unresolved causal parent: %d call(s)", calls)
	}
	injection := foregroundTestInputMessage(t, hello, "text", ContextInjection{
		Role: "background", Source: "meeting.background", RunID: "background-run", Text: "quiet context",
	}, element.Envelope{ItemID: parent, RunID: "background-run"})
	if err := session.Send(injection); err != nil {
		t.Fatal(err)
	}
	_, calls, texts := runtime.snapshot()
	if calls != 1 {
		t.Fatalf("Generate calls after causal injection = %d, want 1", calls)
	}
	if len(texts) != 1 || texts[0].Role != "system" || texts[0].Text != "quiet context" {
		t.Fatalf("foreground text inputs = %+v", texts)
	}
}

func TestForegroundSessionAppliesInitialConfigurationBeforeOvertakingAudio(t *testing.T) {
	plugin, runtime, _ := foregroundTestPlugin(t)
	hello := foregroundTestHello(t)
	created, err := plugin.newForegroundSession(context.Background(), hello, legacy.Options{
		SessionID: "meeting-session-1", Sink: foregroundTestClientSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := created.(*foregroundSession)
	t.Cleanup(func() { _ = session.Close() })
	audio := foregroundTestInputMessage(t, hello, "audio", acousticelements.InputFrame{
		StreamID: "microphone", Frame: perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", CapturedNS: 2,
			PCM16LE: []byte{1, 0}, SampleRateHz: 24_000,
		},
	}, element.Envelope{ItemID: "audio-2", Sequence: 2, CaptureNS: 2})
	if err := session.Send(audio); err != nil {
		t.Fatal(err)
	}
	if events := runtime.eventSnapshot(); len(events) != 0 {
		t.Fatalf("audio overtook initial settings: %v", events)
	}
	settings := foregroundTestInputMessage(t, hello, "tools",
		foregroundSessionSettings{Settings: legacy.Settings{Instruction: "configured"}},
		element.Envelope{ItemID: "tools-1", Sequence: 1})
	if err := session.Send(settings); err != nil {
		t.Fatal(err)
	}
	if events := runtime.eventSnapshot(); !slices.Equal(events, []string{"tools", "audio"}) {
		t.Fatalf("initial input order = %v, want tools then audio", events)
	}
}

func TestForegroundSessionGeneratedRunIdentityBindsOrderedSpeechResultAndOutcome(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	hello := foregroundTestHello(t)
	trigger := foregroundTestInputMessage(t, hello, "trigger", cognitionelements.Generate{
		Invocation: continuation.Invocation{Instruction: "answer", MaxOutputTokens: 32},
	}, element.Envelope{ItemID: "trigger-with-generated-run"})
	if err := session.Send(trigger); err != nil {
		t.Fatal(err)
	}
	sink, calls, _ := runtime.snapshot()
	if calls != 1 || sink == nil {
		t.Fatalf("runtime state: calls=%d sink=%T", calls, sink)
	}
	utterance := action.Utterance{ID: "utterance-1", Text: "Hello."}
	reservation, ok := sink.(legacy.SpeechReservationSink)
	if !ok {
		t.Fatalf("foreground sink lacks speech reservations: %T", sink)
	}
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reservation.SpeechReserved(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechBegin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechText(context.Background(), utterance, "Hello."); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechAudio(context.Background(), utterance, action.Frame{
		PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 24_000, Final: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechEnd(context.Background(), utterance, action.Outcome{Completed: true}); err != nil {
		t.Fatal(err)
	}
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}

	wantPorts := []string{"audio_out", "text_out", "text_out", "audio_out", "text_out", "audio_out", "result", "outcome"}
	var runID string
	responseSequence := uint64(0)
	for index, wantPort := range wantPorts {
		frame := foregroundTestReadFrame(t, session)
		if frame.Port != wantPort {
			t.Fatalf("frame %d port = %q, want %q", index, frame.Port, wantPort)
		}
		if frame.Envelope.RunID == "" {
			t.Fatalf("frame %d has no generated run ID", index)
		}
		if runID == "" {
			runID = frame.Envelope.RunID
		} else if frame.Envelope.RunID != runID {
			t.Fatalf("frame %d run = %q, want %q", index, frame.Envelope.RunID, runID)
		}
		if wantPort != "result" {
			responseSequence++
			if frame.Envelope.Sequence != responseSequence {
				t.Fatalf("response frame %d sequence = %d, want gap-free %d",
					index, frame.Envelope.Sequence, responseSequence)
			}
		}
		if frame.Port == "result" {
			decoded := foregroundTestDecode(t, frame)
			result, ok := decoded.(cognitionelements.Result)
			if pointer, pointerOK := decoded.(*cognitionelements.Result); pointerOK && pointer != nil {
				result, ok = *pointer, true
			}
			if !ok || result.RunID != runID || result.AssistantText != "Hello." ||
				result.Invocation.Instruction != "answer" || result.Interrupted {
				t.Fatalf("foreground result = %#v", decoded)
			}
		}
		if frame.Port == "outcome" {
			decoded := foregroundTestDecode(t, frame)
			outcome, ok := decoded.(cognitionelements.Outcome)
			if pointer, pointerOK := decoded.(*cognitionelements.Outcome); pointerOK && pointer != nil {
				outcome, ok = *pointer, true
			}
			if !ok || outcome.RunID != runID || outcome.Kind != cognitionelements.OutcomeSucceeded {
				t.Fatalf("foreground outcome = %#v", decoded)
			}
		}
	}
}

func TestForegroundSessionResponseSequenceCommitsOnlyAfterSuccessfulPublication(t *testing.T) {
	session, _, _ := foregroundTestSession(t)
	run := &foregroundRun{id: "transactional-response-run"}
	outcome := cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: run.id,
		ProviderReference: ForegroundDeploymentReference,
		StartedNS:         1, FinishedNS: 2, DurationNS: 1,
	}
	if err := session.emitResponse(context.Background(), run, "not-an-output-port",
		modelelements.OutcomeType(), nil, outcome); err == nil {
		t.Fatal("invalid response publication succeeded")
	}
	if session.responseSequence != 0 {
		t.Fatalf("failed validation consumed response sequence %d", session.responseSequence)
	}

	// Reach the actual send boundary with a live context, then cancel while the
	// unbuffered output has no receiver. A subsequent healthy publication must
	// reuse sequence one rather than leaving the adapter waiting on a burned gap.
	session.frames = make(chan sidecar.Message)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := session.emitResponse(ctx, run, "outcome", modelelements.OutcomeType(), nil, outcome)
	cancel()
	if err == nil {
		t.Fatal("canceled response send succeeded")
	}
	if session.responseSequence != 0 || context.Cause(session.ctx) != nil {
		t.Fatalf("canceled send sequence=%d session_cause=%v", session.responseSequence, context.Cause(session.ctx))
	}

	session.frames = make(chan sidecar.Message, 1)
	if err := session.emitResponse(context.Background(), run, "outcome",
		modelelements.OutcomeType(), nil, outcome); err != nil {
		t.Fatal(err)
	}
	frame := <-session.frames
	if frame.Envelope == nil || frame.Envelope.Sequence != 1 || session.responseSequence != 1 {
		t.Fatalf("healthy response envelope=%+v committed=%d, want sequence 1",
			frame.Envelope, session.responseSequence)
	}
}

func TestForegroundSessionPoisonsPartialSpeechTerminalPublication(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	reservation := sink.(legacy.SpeechReservationSink)
	utterance := action.Utterance{ID: "partial-terminal-speech", Text: "Transactional close."}
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reservation.SpeechReserved(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechBegin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	_ = foregroundTestReadFrame(t, session)
	_ = foregroundTestReadFrame(t, session)

	session.mu.Lock()
	run := session.utteranceRuns[utterance.ID]
	session.mu.Unlock()
	session.frames = make(chan sidecar.Message, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := sink.SpeechEnd(ctx, utterance, action.Outcome{Completed: true})
	cancel()
	if err == nil || context.Cause(session.ctx) == nil {
		t.Fatalf("partial speech terminal error=%v session_cause=%v", err, context.Cause(session.ctx))
	}
	frame := <-session.frames
	if frame.Port != "text_out" || frame.Envelope == nil || frame.Envelope.Sequence != 3 ||
		session.responseSequence != 3 {
		t.Fatalf("partial speech terminal frame=%+v committed=%d", frame, session.responseSequence)
	}
	session.mu.Lock()
	done := run.utterances[utterance.ID].done
	session.mu.Unlock()
	if done {
		t.Fatal("failed partial speech terminal was committed as closed")
	}
	if err := session.emitResponse(context.Background(), run, "outcome", modelelements.OutcomeType(), nil,
		cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: run.id,
			ProviderReference: ForegroundDeploymentReference,
		}); err == nil {
		t.Fatal("poisoned partial speech session emitted a later response")
	}
}

func TestForegroundSessionPoisonsPartialToolProposalBatch(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	session.frames = make(chan sidecar.Message, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := sink.ToolCalls(ctx, legacy.ToolCallEvent{Calls: []trajectory.ToolCall{
		{CallID: "partial-tool-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":1,"y":1}`)},
		{CallID: "partial-tool-2", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":2,"y":2}`)},
	}})
	cancel()
	if err == nil || context.Cause(session.ctx) == nil {
		t.Fatalf("partial tool batch error=%v session_cause=%v", err, context.Cause(session.ctx))
	}
	frame := <-session.frames
	if frame.Port != "tool_proposal" || frame.Envelope == nil || frame.Envelope.Sequence != 1 ||
		session.responseSequence != 1 {
		t.Fatalf("partial tool batch frame=%+v committed=%d", frame, session.responseSequence)
	}
	session.mu.Lock()
	run := session.active
	outputs, proposals := len(run.outputs), len(run.proposals)
	session.mu.Unlock()
	if outputs != 0 || proposals != 0 {
		t.Fatalf("partial tool batch committed outputs=%d proposals=%d", outputs, proposals)
	}
}

func TestForegroundSessionSerializesOverlappingProviderTurns(t *testing.T) {
	_, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(entered)
		done <- sink.TurnBegin(context.Background())
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("overlapping TurnBegin returned before the active run ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serialized TurnBegin = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serialized TurnBegin did not resume after the active run ended")
	}
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundSessionStartsNextRunWhilePriorSpeechDrains(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	hello := foregroundTestHello(t)
	runOneContext := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "context-run-1", Kind: trajectory.KindObservation,
		Content: "context visible to run one", SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
	}}}
	if err := session.updateContext(runOneContext); err != nil {
		t.Fatal(err)
	}
	for index, runID := range []string{"foreground-run-1", "foreground-run-2"} {
		trigger := foregroundTestInputMessage(t, hello, "trigger", cognitionelements.Generate{
			Invocation: continuation.Invocation{
				Instruction: "meeting turn", MaxOutputTokens: 32,
			},
		}, element.Envelope{
			ItemID: "trigger-" + runID, RunID: runID, Sequence: uint64(index + 2),
		})
		if err := session.Send(trigger); err != nil {
			t.Fatal(err)
		}
	}
	sink, calls, _ := runtime.snapshot()
	if calls != 2 || sink == nil {
		t.Fatalf("runtime state: calls=%d sink=%T", calls, sink)
	}
	reservation := sink.(legacy.SpeechReservationSink)
	nextResponseSequence := uint64(0)
	assertResponseSequence := func(frame sidecar.Message) {
		t.Helper()
		nextResponseSequence++
		if frame.Envelope == nil || frame.Envelope.Sequence != nextResponseSequence {
			t.Fatalf("cross-run response sequence = %+v, want %d", frame.Envelope, nextResponseSequence)
		}
	}
	utterance := action.Utterance{ID: "draining-utterance", Text: "Still speaking."}
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reservation.SpeechReserved(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	// Cascade returns from cognition after reserving the action-plane speech.
	// Its actual Begin/Text/Audio/End callbacks may arrive later.
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}

	started := make(chan error, 1)
	go func() { started <- sink.TurnBegin(context.Background()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("next run while speech drains = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("next run was serialized behind prior paced speech")
	}
	runTwoContext := cloneForegroundSnapshot(runOneContext)
	runTwoContext.Version = 2
	runTwoContext.Items = append(runTwoContext.Items, trajectory.Item{
		ID: "context-run-2", Kind: trajectory.KindObservation,
		Content: "context visible only to run two", SourceRevision: 2,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver},
	})
	if err := session.updateContext(runTwoContext); err != nil {
		t.Fatal(err)
	}
	// The prior action-plane stream continues while the next cognition run is
	// active. Every speech frame must retain the prior graph run identity.
	if err := sink.SpeechBegin(context.Background(), utterance); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechText(context.Background(), utterance, utterance.Text); err != nil {
		t.Fatal(err)
	}
	if err := sink.SpeechAudio(context.Background(), utterance, action.Frame{
		PCM16LE: []byte{1, 0, 2, 0}, SampleRateHz: 24_000, Final: true,
	}); err != nil {
		t.Fatal(err)
	}
	for index, port := range []string{"audio_out", "text_out", "text_out", "audio_out"} {
		frame := foregroundTestReadFrame(t, session)
		if frame.Port != port || frame.Envelope.RunID != "foreground-run-1" {
			t.Fatalf("overlapping speech frame %d = port %q run %q", index, frame.Port, frame.Envelope.RunID)
		}
		assertResponseSequence(frame)
	}
	if err := sink.ToolCalls(context.Background(), legacy.ToolCallEvent{
		InvocationID: "visual-reflex-invocation",
		Calls: []trajectory.ToolCall{{
			CallID: "visual-call-1", Name: "computer.click_normalized",
			Arguments: json.RawMessage(`{"x":500,"y":500}`),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}

	// The second, silent action run completes before the first turn's paced
	// audio. Its graph identity must remain independent.
	for index, port := range []string{"tool_proposal", "result", "outcome"} {
		frame := foregroundTestReadFrame(t, session)
		if frame.Port != port || frame.Envelope.RunID != "foreground-run-2" {
			t.Fatalf("second run frame %d = port %q run %q", index, frame.Port, frame.Envelope.RunID)
		}
		if port != "result" {
			assertResponseSequence(frame)
		}
		switch port {
		case "result":
			result := foregroundTestCognitionResult(t, frame)
			if result.ContextVersion != 2 || result.ContextTailID != "context-run-2" {
				t.Fatalf("second run context = version %d tail %q, want 2/context-run-2",
					result.ContextVersion, result.ContextTailID)
			}
		case "outcome":
			outcome := foregroundTestCognitionOutcome(t, frame)
			if outcome.ContextVersion != 2 {
				t.Fatalf("second run outcome context = %d, want 2", outcome.ContextVersion)
			}
		}
	}

	if err := sink.SpeechEnd(context.Background(), utterance, action.Outcome{Completed: true}); err != nil {
		t.Fatal(err)
	}
	for index, port := range []string{
		"text_out", "audio_out", "result", "outcome",
	} {
		frame := foregroundTestReadFrame(t, session)
		if frame.Port != port || frame.Envelope.RunID != "foreground-run-1" {
			t.Fatalf("draining run frame %d = port %q run %q", index, frame.Port, frame.Envelope.RunID)
		}
		if port != "result" {
			assertResponseSequence(frame)
		}
		switch port {
		case "result":
			result := foregroundTestCognitionResult(t, frame)
			if result.ContextVersion != 1 || result.ContextTailID != "context-run-1" {
				t.Fatalf("draining run context was stamped with hindsight: version %d tail %q",
					result.ContextVersion, result.ContextTailID)
			}
		case "outcome":
			outcome := foregroundTestCognitionOutcome(t, frame)
			if outcome.ContextVersion != 1 {
				t.Fatalf("draining run outcome was stamped with context %d, want 1",
					outcome.ContextVersion)
			}
		}
	}
	if len(session.draining) != 0 || len(session.utteranceRuns) != 0 {
		t.Fatalf("completed speech retained run=%d utterance=%d", len(session.draining), len(session.utteranceRuns))
	}
}

func TestForegroundSessionFreezesAtExactDelayedTranscriptRevision(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	if err := sink.Transcript(context.Background(), legacy.TranscriptEvent{
		ItemID: "microphone-turn", Text: "show the latest conversion rate", Final: true,
	}); err != nil {
		t.Fatal(err)
	}
	transcript := foregroundTestReadFrame(t, session)
	if transcript.Port != "transcript" {
		t.Fatalf("transcript frame port = %q", transcript.Port)
	}
	transcriptEventID := transcript.Envelope.ItemID
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- sink.TurnEnd(context.Background(), legacy.TurnOutcome{}) }()
	select {
	case err := <-ended:
		t.Fatalf("turn did not wait for its exact committed transcript: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	// The shared observation commit mux assigns canonical source revisions across
	// both screen and transcript streams. A prior visual commit therefore makes
	// the transcript's canonical revision differ from its ASR-local revision.
	// Neither a same-text collision on that local revision nor a forged copy of
	// the event identity from the wrong observer/source may release the run.
	wrong := []trajectory.Item{
		{
			ID: "wrong-local-revision", Kind: trajectory.KindObservation,
			Content: "show the latest conversion rate", SourceRevision: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Event: &trajectory.EventMetadata{
				EventID: "unrelated-same-text-event", Type: "meeting.foreground.asr.endpoint",
				Source: "meeting.foreground.asr", Channel: "microphone", OccurredNS: 1,
			},
		},
		{
			ID: "wrong-observer", Kind: trajectory.KindObservation,
			Content: "show the latest conversion rate", SourceRevision: 2,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Event: &trajectory.EventMetadata{
				EventID: transcriptEventID, Type: "meeting.visual.endpoint",
				Source: "meeting.visual", Channel: "screen", OccurredNS: 2,
			},
		},
	}
	if err := session.updateContext(trajectory.Snapshot{Version: 2, Items: wrong}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		t.Fatalf("turn accepted a same-text or wrong-source transcript collision: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	// The exact transcript event follows the two interleaved canonical commits.
	// A later visual item is already visible in the mutable snapshot, but only
	// the prefix through the exact transcript event belongs to this ending run.
	committed := append(slices.Clone(wrong),
		trajectory.Item{
			ID: "transcript-canonical-revision-3", Kind: trajectory.KindObservation,
			Content: "show the latest conversion rate", SourceRevision: 3,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Event: &trajectory.EventMetadata{
				EventID: transcriptEventID, Type: "meeting.foreground.asr.endpoint",
				Source: "meeting.foreground.asr", Channel: "microphone", OccurredNS: 3,
			},
		},
		trajectory.Item{
			ID: "later-observation", Kind: trajectory.KindObservation,
			Content: "later screen state", SourceRevision: 4,
			Producer: trajectory.Producer{Phase: trajectory.PhaseObserver},
		},
	)
	if err := session.updateContext(trajectory.Snapshot{Version: 4, Items: committed}); err != nil {
		t.Fatal(err)
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
	resultFrame := foregroundTestReadFrame(t, session)
	outcomeFrame := foregroundTestReadFrame(t, session)
	if resultFrame.Port != "result" || outcomeFrame.Port != "outcome" {
		t.Fatalf("terminal ports = %q/%q", resultFrame.Port, outcomeFrame.Port)
	}
	result := foregroundTestCognitionResult(t, resultFrame)
	if result.ContextVersion != 3 || result.ContextTailID != "transcript-canonical-revision-3" {
		t.Fatalf("delayed transcript context = version %d tail %q, want exact prefix 3",
			result.ContextVersion, result.ContextTailID)
	}
	if outcome := foregroundTestCognitionOutcome(t, outcomeFrame); outcome.ContextVersion != 3 {
		t.Fatalf("delayed transcript outcome context = %d, want 3", outcome.ContextVersion)
	}
}

func TestForegroundSessionRejectsReusedGraphIdentitiesAndBoundsDraining(t *testing.T) {
	t.Run("run ID", func(t *testing.T) {
		session, runtime, _ := foregroundTestSession(t)
		hello := foregroundTestHello(t)
		for index := 0; index < 2; index++ {
			if err := session.Send(foregroundTestInputMessage(t, hello, "trigger",
				cognitionelements.Generate{Invocation: continuation.Invocation{
					Instruction: "answer", MaxOutputTokens: 32,
				}}, element.Envelope{
					ItemID: "duplicate-trigger-" + string(rune('a'+index)),
					RunID:  "duplicate-foreground-run", Sequence: uint64(index + 1),
				})); err != nil {
				t.Fatal(err)
			}
		}
		sink, _, _ := runtime.snapshot()
		if err := sink.TurnBegin(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
			t.Fatal(err)
		}
		_ = foregroundTestReadFrame(t, session)
		_ = foregroundTestReadFrame(t, session)
		if err := sink.TurnBegin(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "duplicate run ID") {
			t.Fatalf("reused run ID = %v", err)
		}
	})

	t.Run("utterance ID", func(t *testing.T) {
		session, runtime, _ := foregroundTestSession(t)
		sink, _, _ := runtime.snapshot()
		reservation := sink.(legacy.SpeechReservationSink)
		utterance := action.Utterance{ID: "never-reuse-this-utterance", Text: "hello"}
		if err := sink.TurnBegin(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := reservation.SpeechReserved(context.Background(), utterance); err != nil {
			t.Fatal(err)
		}
		reservation.SpeechReservationCancelled(context.Background(), utterance)
		if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
			t.Fatal(err)
		}
		_ = foregroundTestReadFrame(t, session)
		_ = foregroundTestReadFrame(t, session)
		if err := sink.TurnBegin(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := reservation.SpeechReserved(context.Background(), utterance); err == nil ||
			!strings.Contains(err.Error(), "reused utterance ID") {
			t.Fatalf("reused utterance ID = %v", err)
		}
		if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
			t.Fatal(err)
		}
		_ = foregroundTestReadFrame(t, session)
		_ = foregroundTestReadFrame(t, session)
	})

	t.Run("draining bound", func(t *testing.T) {
		session, runtime, _ := foregroundTestSession(t)
		session.mu.Lock()
		for index := 0; index < maximumForegroundDrainingRuns; index++ {
			runID := fmt.Sprintf("retained-run-%d", index)
			session.draining[runID] = &foregroundRun{id: runID}
		}
		session.mu.Unlock()
		sink, _, _ := runtime.snapshot()
		if err := sink.TurnBegin(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "draining run limit") {
			t.Fatalf("draining run admission = %v", err)
		}
	})
}

func foregroundTestCognitionResult(t testing.TB, frame sidecar.Message) cognitionelements.Result {
	t.Helper()
	decoded := foregroundTestDecode(t, frame)
	if pointer, ok := decoded.(*cognitionelements.Result); ok && pointer != nil {
		return *pointer
	}
	if result, ok := decoded.(cognitionelements.Result); ok {
		return result
	}
	t.Fatalf("foreground result payload = %T", decoded)
	return cognitionelements.Result{}
}

func foregroundTestCognitionOutcome(t testing.TB, frame sidecar.Message) cognitionelements.Outcome {
	t.Helper()
	decoded := foregroundTestDecode(t, frame)
	if pointer, ok := decoded.(*cognitionelements.Outcome); ok && pointer != nil {
		return *pointer
	}
	if outcome, ok := decoded.(cognitionelements.Outcome); ok {
		return outcome
	}
	t.Fatalf("foreground outcome payload = %T", decoded)
	return cognitionelements.Outcome{}
}

func TestForegroundSessionCanceledOverlappingTurnDoesNotStrandAdmission(t *testing.T) {
	_, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.TurnBegin(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled overlapping TurnBegin = %v", err)
	}
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}
	if err := sink.TurnBegin(context.Background()); err != nil {
		t.Fatalf("TurnBegin after canceled waiter = %v", err)
	}
	if err := sink.TurnEnd(context.Background(), legacy.TurnOutcome{}); err != nil {
		t.Fatal(err)
	}
}

func TestForegroundTranscriptSupersessionUsesStableStreamAndIndependentFrames(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	for _, event := range []legacy.TranscriptEvent{
		{ItemID: "microphone-item-1", Text: "hel", Final: false},
		{ItemID: "microphone-item-1", Text: "hello", Final: true},
	} {
		if err := sink.Transcript(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	first := foregroundTestReadFrame(t, session)
	second := foregroundTestReadFrame(t, session)
	if first.Port != "transcript" || second.Port != "transcript" ||
		first.Envelope.ItemID == second.Envelope.ItemID ||
		first.Envelope.OpportunityID != "microphone-item-1" ||
		second.Envelope.OpportunityID != "microphone-item-1" ||
		first.Envelope.SourceID != "microphone" || second.Envelope.SourceID != "microphone" {
		t.Fatalf("transcript frame identities drifted: first=%+v second=%+v", first.Envelope, second.Envelope)
	}
	firstDecoded := foregroundTestDecode(t, first)
	firstObservation, ok := firstDecoded.(perception.Observation)
	if pointer, pointerOK := firstDecoded.(*perception.Observation); pointerOK && pointer != nil {
		firstObservation, ok = *pointer, true
	}
	if !ok {
		t.Fatalf("first transcript payload = %T", firstDecoded)
	}
	secondDecoded := foregroundTestDecode(t, second)
	secondObservation, ok := secondDecoded.(perception.Observation)
	if pointer, pointerOK := secondDecoded.(*perception.Observation); pointerOK && pointer != nil {
		secondObservation, ok = *pointer, true
	}
	if !ok || secondObservation.Supersedes != firstObservation.Revision || !secondObservation.Final {
		t.Fatalf("transcript revisions: first=%+v second=%+v", firstObservation, secondObservation)
	}
}

func TestForegroundCloseDuringEmitDoesNotCloseProducerChannel(t *testing.T) {
	session, runtime, _ := foregroundTestSession(t)
	sink, _, _ := runtime.snapshot()
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		done <- sink.Activity(context.Background(), legacy.ActivityEvent{Started: true, ItemID: "activity-1"})
	}()
	close(start)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) && err.Error() != "meeting foreground session closed" {
			t.Fatalf("emit after close error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit raced with close and did not return")
	}
	if got := runtime.closes.Load(); got != 1 {
		t.Fatalf("runtime closes = %d, want 1", got)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runtime.closes.Load(); got != 1 {
		t.Fatalf("idempotent runtime closes = %d, want 1", got)
	}
}

func TestForegroundContextNormalizesEmptySnapshot(t *testing.T) {
	session, _, _ := foregroundTestSession(t)
	if err := session.updateContext(trajectory.Snapshot{Items: []trajectory.Item{}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(session.contextSnapshot, trajectory.Snapshot{}) {
		t.Fatalf("empty context snapshot = %#v", session.contextSnapshot)
	}
}
