package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/sidecar"
)

func testAudioWireFormat() sidecar.WireFormat {
	return sidecar.WireFormat{
		PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 1024, MaxBinaryBytes: 4800,
		Media: &sidecar.MediaFormat{
			Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
			SampleRateHz: 24_000, Channels: 1, MaxFrameDurationMS: 100,
		},
	}
}

func TestConfigNormalizesSettingsToTheV4OnWireEncoding(t *testing.T) {
	config, err := decodeConfig(json.RawMessage(`{
        "deployment": "deployment.test",
        "settings": { "voice": "test", "temperature": 0.7 }
    }`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(config.Settings), `{"voice":"test","temperature":0.7}`; got != want {
		t.Fatalf("wire settings = %s, want %s", got, want)
	}
	probe := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: func() *element.Descriptor {
			descriptor := StandardDescriptor()
			return &descriptor
		}(),
		ElementConfig: config.Settings,
		SelectedPorts: []sidecar.PortSelection{
			sidecar.JSONPortSelection("trigger", element.Input, GenerateType()),
		},
	}
	if err := probe.Validate(); err != nil {
		t.Fatalf("normalized settings are not a valid v4 hello: %v", err)
	}
}

func TestSelectedMediaRequiresExplicitFormatsBeforeDial(t *testing.T) {
	var dialed atomic.Int32
	_, err := mountConfigOnly(t, `graph selected_media {
    model.External :: model;
    input audio = model.audio;
}`, Config{Deployment: "deployment.test"}, &dialed)
	if err == nil || !strings.Contains(err.Error(), "without explicit wire formats") {
		t.Fatalf("missing selected-media format error = %v", err)
	}
	if got := dialed.Load(); got != 0 {
		t.Fatalf("invalid media configuration dialed %d session(s)", got)
	}
}

func TestFormatsForUnselectedPortsFailBeforeDial(t *testing.T) {
	var dialed atomic.Int32
	_, err := mountConfigOnly(t, `graph unselected_format {
    model.External :: model;
    input trigger = model.trigger;
    output outcome = model.outcome;
}`, Config{
		Deployment:  "deployment.test",
		PortFormats: map[string][]sidecar.WireFormat{"audio": {testAudioWireFormat()}},
	}, &dialed)
	if err == nil || !strings.Contains(err.Error(), "configure unselected port") {
		t.Fatalf("unselected-port format error = %v", err)
	}
	if got := dialed.Load(); got != 0 {
		t.Fatalf("invalid unselected-port configuration dialed %d session(s)", got)
	}
}

func TestKnownNativeOutputRequiresConcreteDecoderBeforeDial(t *testing.T) {
	var dialed atomic.Int32
	_, err := mountConfigOnly(t, `graph missing_native_decoder {
    model.External :: model;
    input trigger = model.trigger;
    output outcome = model.outcome;
}`, Config{Deployment: "deployment.test"}, &dialed)
	if err == nil || !strings.Contains(err.Error(), "no concrete payload decoder") {
		t.Fatalf("missing native decoder error = %v", err)
	}
	if got := dialed.Load(); got != 0 {
		t.Fatalf("missing native decoder dialed %d session(s)", got)
	}
}

func TestFactoryConfigValidationUsesThePackagedPortTypes(t *testing.T) {
	factory := Factory{}
	for name, test := range map[string]struct {
		config Config
		want   string
	}{
		"unknown port": {
			config: Config{Deployment: "deployment.test", PortFormats: map[string][]sidecar.WireFormat{
				"invented": {sidecar.JSONWireFormat()},
			}},
			want: "unknown port",
		},
		"media profile on trigger": {
			config: Config{Deployment: "deployment.test", PortFormats: map[string][]sidecar.WireFormat{
				"trigger": {testAudioWireFormat()},
			}},
			want: "non-media",
		},
		"video profile on audio": {
			config: Config{Deployment: "deployment.test", PortFormats: map[string][]sidecar.WireFormat{
				"audio": {{
					PayloadMode: sidecar.PayloadJSONBinary, MaxJSONBytes: 1024, MaxBinaryBytes: 1024,
					Media: &sidecar.MediaFormat{
						Kind: sidecar.MediaVideo, Encoding: "image/jpeg", MaxWidth: 640,
						MaxHeight: 480, MaxFrameRateMilliHz: 30_000,
					},
				}},
			}},
			want: "requires audio",
		},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(test.config)
			if err != nil {
				t.Fatal(err)
			}
			if err := factory.ValidateConfig(encoded); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("config validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func mountConfigOnly(
	t *testing.T, source string, config Config, dialed *atomic.Int32,
) (*graphruntime.Mounted, error) {
	t.Helper()
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptor(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("model-conformance.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployments := NewDeploymentRegistry()
	if err := deployments.Register(config.Deployment, func(context.Context, sidecar.Message) (Session, error) {
		dialed.Add(1)
		return nil, errors.New("unexpected dial")
	}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(DeploymentRegistryService, deployments); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set(PayloadCodecService, NewJSONCodec()); err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := RegisterFactory(registry); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"model": encoded},
	})
}

type binaryTrigger struct {
	Prompt string `json:"prompt"`
	Bytes  []byte `json:"-"`
}

func (trigger binaryTrigger) BinaryPayload() []byte { return trigger.Bytes }

type duplicateJSONPayload struct{}

func (duplicateJSONPayload) MarshalJSON() ([]byte, error) {
	return []byte(`{"value":1,"value":2}`), nil
}

type explicitMediaPayload struct {
	Metadata sidecar.MediaFrameMetadata `json:"-"`
	Bytes    []byte                     `json:"-"`
}

func (payload *explicitMediaPayload) BinaryPayload() []byte { return payload.Bytes }
func (payload *explicitMediaPayload) MediaFrameMetadata() *sidecar.MediaFrameMetadata {
	return &payload.Metadata
}
func (payload *explicitMediaPayload) SetBinaryPayload(source []byte) error {
	payload.Bytes = append([]byte(nil), source...)
	return nil
}
func (payload *explicitMediaPayload) SetMediaFrameMetadata(metadata sidecar.MediaFrameMetadata) error {
	payload.Metadata = metadata
	return nil
}

func TestWrongPayloadLaneFailsBeforeSessionSend(t *testing.T) {
	harness := mountModel(t, modelGraph, nil)
	trigger := ingress(t, harness.mounted, "trigger")
	if _, err := trigger.Broadcast(context.Background(), element.Envelope{
		Type: GenerateType(), ItemID: "trigger-binary", RunID: "run-binary",
		Payload: binaryTrigger{Prompt: "hello", Bytes: []byte{1, 2, 3, 4}},
	}); err != nil {
		harness.cancel()
		t.Fatal(err)
	}
	select {
	case err := <-harness.done:
		if err == nil || !strings.Contains(err.Error(), "JSON-only") {
			t.Fatalf("wrong payload-lane error = %v", err)
		}
	case <-time.After(time.Second):
		harness.cancel()
		t.Fatal("wrong payload lane did not stop the model node")
	}
	harness.cancel()
	if got := len(harness.session.sent); got != 0 {
		t.Fatalf("nonconformant frame reached Session.Send %d time(s)", got)
	}
	if got := harness.session.closes.Load(); got != 1 {
		t.Fatalf("session closes after local conformance failure = %d, want 1", got)
	}
}

type variadicTestInput struct {
	typeOf       element.Type
	envelope     element.Envelope
	delivered    atomic.Bool
	receiveCalls atomic.Int32
}

func (input *variadicTestInput) Name() string        { return "observations" }
func (input *variadicTestInput) Type() element.Type  { return input.typeOf.Clone() }
func (*variadicTestInput) Lanes() []element.Receiver { return nil }
func (input *variadicTestInput) Receive(context.Context) (element.Envelope, error) {
	input.receiveCalls.Add(1)
	return element.Envelope{}, errors.New("singular Receive used for a variadic input")
}
func (input *variadicTestInput) ReceiveAny(ctx context.Context) (element.Envelope, string, error) {
	if input.delivered.CompareAndSwap(false, true) {
		return input.envelope.Clone(), "observations/source-a", nil
	}
	<-ctx.Done()
	return element.Envelope{}, "", context.Cause(ctx)
}

func TestForwardInputSupportsVariadicDescriptorPorts(t *testing.T) {
	typeOf := element.Event(element.Named("test.Observation"))
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.VariadicExternal", Revision: 1,
		Ports: []element.Port{{
			Name: "observations", Direction: element.Input, Type: typeOf,
			Cardinality: element.Variadic,
		}},
	}
	helloDescriptor := descriptor.Clone()
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &helloDescriptor,
		SelectedPorts: []sidecar.PortSelection{
			sidecar.JSONPortSelection("observations", element.Input, typeOf),
		},
	}
	ready, err := (sidecar.ElementConformanceFixture{
		Runtime:  sidecar.ArtifactIdentity{ID: "runtime/variadic", Revision: "1"},
		Provider: sidecar.ArtifactIdentity{ID: "provider/variadic", Revision: "1"},
		Adapter:  sidecar.ArtifactIdentity{ID: "adapter/variadic", Revision: "4"},
	}).Ready(hello)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := sidecar.NegotiateElementSession(hello, ready)
	if err != nil {
		t.Fatal(err)
	}
	input := &variadicTestInput{
		typeOf: typeOf,
		envelope: element.Envelope{
			Type: typeOf, ItemID: "observation-1", SourceID: "source-a",
			Payload: map[string]string{"text": "hello"},
		},
	}
	session := &fakeSession{sent: make(chan sidecar.Message, 1)}
	runner := &runner{codec: NewJSONCodec()}
	ctx, cancel := context.WithCancel(context.Background())
	failures := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(1)
	go runner.forwardInput(ctx, "observations", input, session, contract,
		&sync.Mutex{}, failures, &wait)

	select {
	case sent := <-session.sent:
		if sent.Port != "observations" || sent.Envelope == nil || sent.Envelope.SourceID != "source-a" {
			t.Fatalf("variadic input frame = %+v", sent)
		}
	case err := <-failures:
		t.Fatalf("variadic input failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("variadic input did not reach the session")
	}
	cancel()
	wait.Wait()
	if input.receiveCalls.Load() != 0 {
		t.Fatalf("variadic input used singular Receive %d time(s)", input.receiveCalls.Load())
	}
}

type blockingSendSession struct {
	ready     sidecar.Message
	frames    chan sidecar.Message
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

func (session *blockingSendSession) Ready() sidecar.Message         { return session.ready.Clone() }
func (session *blockingSendSession) Frames() <-chan sidecar.Message { return session.frames }
func (*blockingSendSession) Err() error                             { return nil }
func (session *blockingSendSession) Send(sidecar.Message) error {
	session.startOnce.Do(func() { close(session.started) })
	<-session.release
	return errors.New("blocked send interrupted by session close")
}
func (session *blockingSendSession) Close() error {
	session.closeOnce.Do(func() {
		session.closes.Add(1)
		close(session.release)
		close(session.frames)
	})
	return nil
}

type acceptingResolutionReporter struct{}

func (acceptingResolutionReporter) Runtime(string, string, string) error { return nil }
func (acceptingResolutionReporter) Capabilities([]element.CapabilityResolution) error {
	return nil
}

func TestRunnerClosesSessionBeforeJoiningBlockedInputSenders(t *testing.T) {
	descriptor := StandardDescriptor()
	selections := []sidecar.PortSelection{
		sidecar.JSONPortSelection("trigger", element.Input, GenerateType()),
		sidecar.JSONPortSelection("outcome", element.Output, OutcomeType()),
	}
	input := &variadicTestInput{
		typeOf: GenerateType(),
		envelope: element.Envelope{
			Type: GenerateType(), ItemID: "blocked-trigger", RunID: "blocked-run",
			Payload: map[string]string{"prompt": "hello"},
		},
	}
	created := make(chan *blockingSendSession, 1)
	runner := &runner{
		instance: "blocked-model", descriptor: descriptor,
		config: Config{Deployment: "deployment.blocked", Settings: json.RawMessage(`{}`)},
		codec:  NewJSONCodec(), selections: selections,
		ports: boundPorts{
			inputs: map[string]element.InputPort{"trigger": input},
			outputs: map[string]element.OutputPort{
				"outcome": droppingOutput{typeOf: OutcomeType()},
			},
		},
		resolution: acceptingResolutionReporter{}, holder: &sessionHolder{},
	}
	runner.dial = func(_ context.Context, hello sidecar.Message) (Session, error) {
		ready, err := (sidecar.ElementConformanceFixture{
			Runtime:  sidecar.ArtifactIdentity{ID: "runtime/blocked-model", Revision: "1"},
			Provider: sidecar.ArtifactIdentity{ID: "provider/blocked-model", Revision: "1"},
			Adapter:  sidecar.ArtifactIdentity{ID: "adapter/blocked-model", Revision: "4"},
		}).Ready(hello)
		if err != nil {
			return nil, err
		}
		session := &blockingSendSession{
			ready: ready, frames: make(chan sidecar.Message),
			started: make(chan struct{}), release: make(chan struct{}),
		}
		created <- session
		return session, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	session := <-created
	select {
	case <-session.started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("external-model input did not reach the blocking send")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner waited on a blocked input sender before closing its session")
	}
	if got := session.closes.Load(); got != 1 {
		t.Fatalf("blocked session closes = %d, want 1", got)
	}
}

func TestJSONCodecRejectsDuplicateUnknownAndOversizedPayloads(t *testing.T) {
	codec := NewJSONCodec()
	if err := codec.Register(OutcomeType(), func() any { return &testOutcome{} }); err != nil {
		t.Fatal(err)
	}

	if _, err := codec.Encode(OutcomeType(), OpaquePayload{
		JSON: json.RawMessage(`{"kind":"one","kind":"two"}`),
	}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate opaque JSON encode error = %v", err)
	}
	if _, err := codec.Decode(OutcomeType(), EncodedPayload{
		JSON: json.RawMessage(`{"kind":"one","kind":"two"}`),
	}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate typed JSON decode error = %v", err)
	}
	if _, err := codec.Decode(OutcomeType(), EncodedPayload{
		JSON: json.RawMessage(`{"kind":"done","invented":true}`),
	}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown typed JSON field error = %v", err)
	}

	oversizedJSON := make(json.RawMessage, sidecar.MaxElementJSONBytes+1)
	if _, err := codec.Decode(OutcomeType(), EncodedPayload{JSON: oversizedJSON}); err == nil ||
		!strings.Contains(err.Error(), "limits") {
		t.Fatalf("oversized JSON decode error = %v", err)
	}
	oversizedBinary := make([]byte, sidecar.MaxElementBinaryBytes+1)
	if _, err := codec.Encode(OutcomeType(), OpaquePayload{
		JSON: json.RawMessage(`{}`), Binary: oversizedBinary,
	}); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Fatalf("oversized binary encode error = %v", err)
	}
	if _, err := codec.Decode(OutcomeType(), EncodedPayload{
		JSON: json.RawMessage(`{}`), Binary: oversizedBinary,
	}); err == nil ||
		!strings.Contains(err.Error(), "limits") {
		t.Fatalf("oversized binary decode error = %v", err)
	}
}

func TestJSONCodecDoesNotBypassRegisteredNativePayloadTypes(t *testing.T) {
	codec := NewJSONCodec()
	if err := codec.Register(OutcomeType(), func() any { return &testOutcome{} }); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Encode(OutcomeType(), map[string]any{"kind": "done"}); err == nil ||
		!strings.Contains(err.Error(), "want *model.testOutcome or model.testOutcome") {
		t.Fatalf("wrong native payload type error = %v", err)
	}
	if _, err := codec.Encode(OutcomeType(), OpaquePayload{JSON: json.RawMessage(`{"kind":"done"}`)}); err == nil ||
		!strings.Contains(err.Error(), "cannot bypass") {
		t.Fatalf("opaque registered payload error = %v", err)
	}
	if _, err := codec.Encode(OutcomeType(), testOutcome{Kind: "done"}); err != nil {
		t.Fatalf("registered native value payload: %v", err)
	}
	if _, err := codec.Encode(OutcomeType(), &testOutcome{Kind: "done"}); err != nil {
		t.Fatalf("registered native pointer payload: %v", err)
	}
}

func TestJSONCodecRejectsFactoryTypeDriftAfterRegistration(t *testing.T) {
	codec := NewJSONCodec()
	var calls atomic.Int32
	if err := codec.Register(OutcomeType(), func() any {
		if calls.Add(1) == 1 {
			return &testOutcome{}
		}
		return &testDelta{}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(OutcomeType(), EncodedPayload{
		JSON: json.RawMessage(`{"kind":"done"}`),
	}); err == nil || !strings.Contains(err.Error(), "changed type") {
		t.Fatalf("payload factory type drift error = %v", err)
	}
}

func TestJSONCodecPreservesBinaryOnlyOpaqueFramesAcrossBoundaries(t *testing.T) {
	codec := NewJSONCodec()
	source := []byte{1, 2, 3, 4}
	decoded, err := codec.Decode(OutcomeType(), EncodedPayload{Binary: source})
	if err != nil {
		t.Fatal(err)
	}
	opaque, ok := decoded.(OpaquePayload)
	if !ok {
		t.Fatalf("binary-only opaque decode type = %T", decoded)
	}
	source[0] = 9
	if len(opaque.JSON) != 0 || !bytes.Equal(opaque.Binary, []byte{1, 2, 3, 4}) {
		t.Fatalf("binary-only opaque decode = JSON %q binary %v", opaque.JSON, opaque.Binary)
	}

	encoded, err := codec.Encode(OutcomeType(), opaque)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded.JSON) != 0 || !bytes.Equal(encoded.Binary, opaque.Binary) {
		t.Fatalf("binary-only opaque re-encode = JSON %q binary %v", encoded.JSON, encoded.Binary)
	}
	encoded.Binary[0] = 8
	if opaque.Binary[0] != 1 {
		t.Fatal("opaque binary output aliases the retained payload")
	}

	if _, err := codec.Encode(OutcomeType(), duplicateJSONPayload{}); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("custom marshaler duplicate JSON error = %v", err)
	}
}

func TestMediaMetadataUsesExplicitTypedSourcesAndSurvivesOpaqueRelay(t *testing.T) {
	metadata := sidecar.MediaFrameMetadata{
		Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
		SampleRateHz: 24_000, Channels: 1, FrameDurationMS: 20,
	}
	typed := &explicitMediaPayload{Metadata: metadata, Bytes: []byte{1, 2, 3, 4}}
	extracted, err := mediaFrameMetadataOf(typed)
	if err != nil {
		t.Fatal(err)
	}
	if extracted == nil || *extracted != metadata {
		t.Fatalf("explicit media metadata = %+v", extracted)
	}
	extracted.SampleRateHz = 48_000
	if typed.Metadata.SampleRateHz != 24_000 {
		t.Fatal("media metadata source aliases the retained payload")
	}

	codec := NewJSONCodec()
	decoded, err := codec.Decode(AudioInputType(), EncodedPayload{
		JSON: json.RawMessage(`{"stream_id":"s1"}`), Binary: []byte{1, 2, 3, 4}, Media: &metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	opaque, ok := decoded.(OpaquePayload)
	if !ok || opaque.Media == nil || *opaque.Media != metadata {
		t.Fatalf("opaque media relay = %#v", decoded)
	}
	relayed, err := mediaFrameMetadataOf(opaque)
	if err != nil || relayed == nil || *relayed != metadata {
		t.Fatalf("relayed media metadata = %+v, error = %v", relayed, err)
	}
	relayed.FrameDurationMS = 40
	if opaque.Media.FrameDurationMS != 20 {
		t.Fatal("relayed metadata aliases opaque payload state")
	}

	receiver := &explicitMediaPayload{}
	attached, err := attachMediaFrameMetadata(receiver, &metadata)
	if err != nil || attached != receiver || receiver.Metadata != metadata {
		t.Fatalf("typed media receiver = %#v, error = %v", attached, err)
	}
	if _, err := attachMediaFrameMetadata(&testOutcome{}, &metadata); err == nil ||
		!strings.Contains(err.Error(), "cannot accept media frame metadata") {
		t.Fatalf("implicit media receiver error = %v", err)
	}

	invalid := metadata
	invalid.Channels = 0
	if _, err := mediaFrameMetadataOf(&explicitMediaPayload{Metadata: invalid}); err == nil {
		t.Fatal("invalid explicit media metadata was accepted")
	}
	var nilMedia *explicitMediaPayload
	if _, err := mediaFrameMetadataOf(nilMedia); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil media source error = %v", err)
	}
	if _, err := attachMediaFrameMetadata(nilMedia, &metadata); err == nil ||
		!strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil media receiver error = %v", err)
	}
}

func TestBootstrapRegistersAndInstallsWithoutReplacement(t *testing.T) {
	bootstrap := NewBootstrap()
	if bootstrap == nil || bootstrap.Deployments == nil || bootstrap.Codec == nil {
		t.Fatal("NewBootstrap returned an incomplete deployment surface")
	}
	for _, valueType := range []element.Type{
		AudioInputType(), ContextType(), GenerateType(), PreparedTextType(),
		PreparedAudioType(), ResultType(), ToolProposalType(), OutcomeType(),
	} {
		if !bootstrap.Codec.Supports(valueType) {
			t.Fatalf("bootstrap codec has no native mapping for %s", valueType.String())
		}
	}
	wantDialError := errors.New("dialer invoked")
	var calls atomic.Int32
	if err := bootstrap.RegisterDialer("remote.primary", func(context.Context, sidecar.Message) (Session, error) {
		calls.Add(1)
		return nil, wantDialError
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := bootstrap.Deployments.resolve("remote.primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.dial(context.Background(), sidecar.Message{}); !errors.Is(err, wantDialError) {
		t.Fatalf("registered dialer error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("registered dialer calls = %d, want 1", calls.Load())
	}
	if err := bootstrap.RegisterDialer("remote.primary", func(context.Context, sidecar.Message) (Session, error) {
		return nil, nil
	}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate deployment registration error = %v", err)
	}
	for name, config := range map[string]sidecar.Config{
		"missing transport": {},
		"ambiguous transport": {
			Command: []string{"sidecar"}, Address: "unix:/tmp/sidecar.sock",
		},
		"empty command": {Command: []string{" "}},
	} {
		if err := bootstrap.RegisterClient("invalid."+strings.ReplaceAll(name, " ", "-"), config); err == nil ||
			!strings.Contains(err.Error(), "external model client") {
			t.Fatalf("%s registration error = %v", name, err)
		}
	}

	clientConfig := sidecar.Config{Command: []string{"/missing/original-sidecar"}}
	if err := bootstrap.RegisterClient("local.sidecar", clientConfig); err != nil {
		t.Fatal(err)
	}
	clientConfig.Command[0] = "/missing/mutated-sidecar"
	clientEntry, err := bootstrap.Deployments.resolve("local.sidecar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientEntry.dial(context.Background(), sidecar.Message{}); err == nil ||
		!strings.Contains(err.Error(), "original-sidecar") || strings.Contains(err.Error(), "mutated-sidecar") {
		t.Fatalf("registered client config was not frozen: %v", err)
	}

	services := graphruntime.NewServiceSet()
	if err := bootstrap.Install(services); err != nil {
		t.Fatal(err)
	}
	deployments, _, found := services.Lookup(DeploymentRegistryService)
	if !found || deployments != bootstrap.Deployments {
		t.Fatalf("installed deployment service = %T %p", deployments, deployments)
	}
	codec, _, found := services.Lookup(PayloadCodecService)
	if !found || codec != bootstrap.Codec {
		t.Fatalf("installed codec service = %T %p", codec, codec)
	}
	replacement := NewBootstrap()
	if err := replacement.Install(services); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("replacement bootstrap error = %v", err)
	}
	if current, _, _ := services.Lookup(DeploymentRegistryService); current != bootstrap.Deployments {
		t.Fatal("refused bootstrap replaced the deployment registry")
	}
	if current, _, _ := services.Lookup(PayloadCodecService); current != bootstrap.Codec {
		t.Fatal("refused bootstrap replaced the payload codec")
	}
}

func TestConcurrentBootstrapInstallPublishesOneMatchingPair(t *testing.T) {
	services := graphruntime.NewServiceSet()
	const installers = 16
	bootstraps := make([]*Bootstrap, installers)
	start := make(chan struct{})
	type result struct {
		bootstrap *Bootstrap
		err       error
	}
	results := make(chan result, installers)
	var wait sync.WaitGroup
	for index := range bootstraps {
		bootstraps[index] = NewBootstrap()
		wait.Add(1)
		go func(bootstrap *Bootstrap) {
			defer wait.Done()
			<-start
			results <- result{bootstrap: bootstrap, err: bootstrap.Install(services)}
		}(bootstraps[index])
	}
	close(start)
	wait.Wait()
	close(results)

	var winner *Bootstrap
	for result := range results {
		if result.err == nil {
			if winner != nil {
				t.Fatal("more than one concurrent bootstrap reported success")
			}
			winner = result.bootstrap
			continue
		}
		if !strings.Contains(result.err.Error(), "already registered") {
			t.Fatalf("concurrent bootstrap error = %v", result.err)
		}
	}
	if winner == nil {
		t.Fatal("no concurrent bootstrap installed services")
	}
	deployments, _, found := services.Lookup(DeploymentRegistryService)
	if !found || deployments != winner.Deployments {
		t.Fatal("concurrent install published a deployment registry from another bootstrap")
	}
	codec, _, found := services.Lookup(PayloadCodecService)
	if !found || codec != winner.Codec {
		t.Fatal("concurrent install published a codec from another bootstrap")
	}
}

func TestBootstrapInstallCollisionDoesNotPartiallyPublish(t *testing.T) {
	for _, existingName := range []string{DeploymentRegistryService, PayloadCodecService} {
		t.Run(existingName, func(t *testing.T) {
			services := graphruntime.NewServiceSet()
			existing := &struct{ owner string }{owner: existingName}
			if _, err := services.Set(existingName, existing); err != nil {
				t.Fatal(err)
			}

			bootstrap := NewBootstrap()
			if err := bootstrap.Install(services); err == nil ||
				!strings.Contains(err.Error(), "already registered") {
				t.Fatalf("colliding bootstrap error = %v", err)
			}
			current, revision, found := services.Lookup(existingName)
			if !found || current != existing || revision != 1 {
				t.Fatalf("existing service changed: value=%#v revision=%d found=%v",
					current, revision, found)
			}
			otherName := DeploymentRegistryService
			if existingName == otherName {
				otherName = PayloadCodecService
			}
			if _, _, found := services.Lookup(otherName); found {
				t.Fatalf("bootstrap partially published non-colliding service %q", otherName)
			}
		})
	}
}
