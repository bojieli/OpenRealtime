package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
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
	createResponses int
	closes          atomic.Int32
}

func (runtime *foregroundTestRuntime) Update(_ context.Context, settings legacy.Settings) error {
	runtime.mu.Lock()
	runtime.settings = legacy.CloneSettings(settings)
	runtime.mu.Unlock()
	return nil
}

func (*foregroundTestRuntime) Audio(context.Context, perception.Frame) error { return nil }

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
