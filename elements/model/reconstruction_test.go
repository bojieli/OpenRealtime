package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/speech"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/sidecar"
)

func TestOmniDuplexAndUpstreamAreSelectionsOverOneTypedElement(t *testing.T) {
	cases := []struct {
		directory string
		inputs    []string
		outputs   []string
		required  []string
	}{
		{
			directory: "omni-external-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context", "context_injection", "interaction",
				"tick", "tool_result", "tools", "trigger", "truncate", "video",
			},
			outputs: []string{
				"activity", "native_state", "outcome", "prepared_audio", "prepared_text",
				"result", "tool_proposal", "transcript",
			},
			required: []string{},
		},
		{
			directory: "duplex-native-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context", "context_injection", "tool_result",
				"tools", "truncate", "video",
			},
			outputs: []string{
				"activity", "interaction_act", "native_state", "outcome", "prepared_audio",
				"prepared_text", "result", "tool_proposal", "transcript",
			},
			required: []string{"model.concurrent-io", "model.native-interaction"},
		},
		{
			directory: "upstream-native-interaction",
			inputs: []string{
				"audio", "cancel", "commit", "context_injection", "tool_result", "tools",
				"trigger", "truncate",
			},
			outputs: []string{
				"activity", "interaction_act", "native_state", "outcome", "prepared_audio",
				"prepared_text", "result", "tool_proposal", "transcript",
			},
			required: []string{"model.native-interaction", "transport.upstream-realtime"},
		},
	}
	for _, test := range cases {
		t.Run(test.directory, func(t *testing.T) {
			graph, config := compileReference(t, test.directory)
			if len(graph.Nodes) != 1 || graph.Nodes[0].Element.Name != "model.External" {
				t.Fatalf("reference reconstructed a binding/runtime species: %+v", graph.Nodes)
			}
			inputs, outputs := boundaryNames(graph)
			if !reflect.DeepEqual(inputs, test.inputs) || !reflect.DeepEqual(outputs, test.outputs) {
				t.Fatalf("boundaries = inputs %v outputs %v", inputs, outputs)
			}
			required := make([]string, 0, len(config.RequiredCapabilities))
			for _, capability := range config.RequiredCapabilities {
				required = append(required, capability.Name)
			}
			sort.Strings(required)
			if !reflect.DeepEqual(required, test.required) {
				t.Fatalf("required live capabilities = %v, want %v", required, test.required)
			}
		})
	}
}

func TestStandardDescriptorExposesContinuousAndResumingInputsAsTriggers(t *testing.T) {
	descriptor := StandardDescriptor()
	if descriptor.Revision != 2 {
		t.Fatalf("descriptor revision = %d, want 2 after reaction-contract change", descriptor.Revision)
	}
	want := []string{
		"audio", "video", "tool_result", "trigger", "commit", "interaction", "tick", "truncate",
	}
	if !reflect.DeepEqual(descriptor.Reaction.Triggers, want) {
		t.Fatalf("reaction triggers = %v, want %v", descriptor.Reaction.Triggers, want)
	}
}

func TestExternalModelPreparedOutputsUseNativeCompositionContracts(t *testing.T) {
	for name, pair := range map[string][2]string{
		"prepared text":  {PreparedTextType().String(), cognitionelements.PreparedTextType().String()},
		"prepared audio": {PreparedAudioType().String(), speech.AudioType().String()},
		"result":         {ResultType().String(), cognitionelements.ResultType().String()},
		"tool proposal":  {ToolProposalType().String(), cognitionelements.ToolProposalType().String()},
		"outcome":        {OutcomeType().String(), cognitionelements.OutcomeType().String()},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s contract = %s, native element uses %s", name, pair[0], pair[1])
		}
	}
}

func TestLockedExternalModelComponentsMountAndNegotiateExactV4Sessions(t *testing.T) {
	for _, directory := range []string{
		"omni-external-interaction",
		"duplex-native-interaction",
		"upstream-native-interaction",
	} {
		t.Run(directory, func(t *testing.T) {
			reference := loadReference(t, directory)
			deployments := NewDeploymentRegistry()
			type dialedSession struct {
				hello   sidecar.Message
				session *fakeSession
			}
			dialed := make(chan dialedSession, 1)
			if err := deployments.Register(reference.config.Deployment,
				func(_ context.Context, hello sidecar.Message) (Session, error) {
					// A deployment adapter receives an owned, fully validated v4
					// offer. Build the strict proof from that exact offer rather
					// than reconstructing graph topology out of band.
					if err := hello.Validate(); err != nil {
						return nil, err
					}
					session := newFakeSession(hello)
					if _, err := sidecar.NegotiateElementSession(hello, session.ready); err != nil {
						return nil, err
					}
					dialed <- dialedSession{hello: hello.Clone(), session: session}
					return session, nil
				}); err != nil {
				t.Fatal(err)
			}

			services := graphruntime.NewServiceSet()
			if _, err := services.Set(DeploymentRegistryService, deployments); err != nil {
				t.Fatal(err)
			}
			if _, err := services.Set(PayloadCodecService, NewStandardJSONCodec()); err != nil {
				t.Fatal(err)
			}
			registry := graphruntime.NewRegistry()
			if err := registry.RegisterArtifact("", inspect.ArtifactIdentity{
				ID: "builtin/model-external", Revision: "locked-component-test",
			}, Factory{}); err != nil {
				t.Fatal(err)
			}
			mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: reference.graph, Registry: registry, Services: services,
				Values: reference.values,
			})
			if err != nil {
				t.Fatalf("mount locked component: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- mounted.Run(ctx) }()

			var observed dialedSession
			select {
			case observed = <-dialed:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("locked component did not dial its v4 deployment")
			}
			assertLockedComponentHello(t, reference, observed.hello)
			contract, err := sidecar.NegotiateElementSession(observed.hello, observed.session.ready)
			if err != nil {
				cancel()
				t.Fatalf("negotiate locked component readiness: %v", err)
			}
			for _, selection := range observed.hello.SelectedPorts {
				negotiated, found := contract.NegotiatedPort(selection.Direction, selection.Name)
				if !found || len(selection.Formats) == 0 || !negotiated.Format.Equal(selection.Formats[0]) {
					cancel()
					t.Fatalf("port %s negotiated %+v, found=%v; first offer %+v",
						selection.Name, negotiated.Format, found, selection.Formats)
				}
			}

			eventually(t, func() bool {
				live := mounted.Live().Nodes["foreground"].Resolution
				return live != nil && live.RuntimeEvidence == inspect.EvidenceLive &&
					live.Runtime.ID == "runtime/fake-sidecar" &&
					live.Runtime.Revision == "4.1.0" &&
					live.CapabilitiesEvidence == inspect.EvidenceLive
			})
			live := mounted.Live().Nodes["foreground"].Resolution
			if live == nil || len(live.Capabilities) !=
				len(observed.hello.SelectedPorts)+len(observed.hello.RequiredCapabilities) {
				cancel()
				t.Fatalf("live capability proof = %+v", live)
			}
			for _, selection := range observed.hello.SelectedPorts {
				name := sidecar.PortCapabilityName(selection.Direction, selection.Name)
				if !hasLiveCapability(live.Capabilities, name, selection.Type.String(), true) {
					cancel()
					t.Fatalf("live resolution does not prove selected port %s (%s): %+v",
						name, selection.Type.String(), live.Capabilities)
				}
			}
			for _, required := range observed.hello.RequiredCapabilities {
				if !hasLiveCapability(live.Capabilities, required.Name, required.Contract, false) {
					cancel()
					t.Fatalf("live resolution does not prove required capability %+v: %+v",
						required, live.Capabilities)
				}
			}
			proveLockedComponentDataPlane(t, mounted, observed.session, contract)

			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("locked component shutdown: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("locked component did not stop")
			}
			if got := observed.session.closes.Load(); got != 1 {
				t.Fatalf("locked component session closes = %d, want 1", got)
			}
		})
	}
}

func proveLockedComponentDataPlane(
	t *testing.T, mounted *graphruntime.Mounted, session *fakeSession,
	contract *sidecar.ElementSessionContract,
) {
	t.Helper()
	pcmInput := make([]byte, 960)
	for index := range pcmInput {
		pcmInput[index] = byte(index % 251)
	}
	audioInput, err := mounted.Ingress("audio")
	if err != nil {
		t.Fatal(err)
	}
	inputFrame := acoustic.InputFrame{
		StreamID: "microphone-1",
		Frame: coreperception.Frame{
			Kind: coreperception.FrameAudio, Source: "microphone", CapturedNS: 100,
			Index: 7, PCM16LE: pcmInput, SampleRateHz: 24_000, SampleOffset: 480,
		},
	}
	inputEnvelope := element.Envelope{
		Type: AudioInputType(), ItemID: "audio-input-7", SessionID: "session-locked",
		SourceID: "microphone", Sequence: 7, CaptureNS: 100, ReceiveNS: 110,
		TraceID: "trace-locked", Payload: inputFrame,
	}
	if result, err := audioInput.Broadcast(context.Background(), inputEnvelope); err != nil || result.Delivered != 1 {
		t.Fatalf("broadcast locked audio input = %+v, %v", result, err)
	}
	sent := receiveSent(t, session)
	if err := contract.ValidateEngineFrame(sent); err != nil {
		t.Fatalf("locked audio frame failed its negotiated contract: %v", err)
	}
	if sent.Port != "audio" || sent.Envelope == nil ||
		!sent.Envelope.Type.Equal(AudioInputType()) || sent.Envelope.ItemID != inputEnvelope.ItemID ||
		!bytes.Equal(sent.Payload, pcmInput) {
		t.Fatalf("locked audio frame lost its native envelope or PCM: %+v", sent)
	}
	var wireInput acoustic.InputFrame
	if err := json.Unmarshal(sent.Envelope.JSON, &wireInput); err != nil {
		t.Fatal(err)
	}
	if wireInput.StreamID != inputFrame.StreamID || wireInput.Frame.Source != inputFrame.Frame.Source ||
		wireInput.Frame.SampleRateHz != inputFrame.Frame.SampleRateHz ||
		len(wireInput.Frame.PCM16LE) != 0 {
		t.Fatalf("locked audio JSON metadata = %+v", wireInput)
	}
	wantInputMedia := sidecar.MediaFrameMetadata{
		Kind: sidecar.MediaAudio, Encoding: "audio/pcm", SampleFormat: "s16le",
		SampleRateHz: 24_000, Channels: 1, FrameDurationMS: 20,
	}
	if sent.Envelope.Media == nil || *sent.Envelope.Media != wantInputMedia {
		t.Fatalf("locked audio media evidence = %+v, want %+v", sent.Envelope.Media, wantInputMedia)
	}

	preparedOutput, err := mounted.Egress("prepared_audio")
	if err != nil {
		t.Fatal(err)
	}
	outcomeOutput, err := mounted.Egress("outcome")
	if err != nil {
		t.Fatal(err)
	}
	pcmOutput := make([]byte, 960)
	for index := range pcmOutput {
		pcmOutput[index] = byte(255 - index%251)
	}
	prepared := speech.AudioFrame{
		Kind: speech.AudioChunk, UtteranceID: "utterance-locked",
		Chunk: v1.SpeechChunk{
			ChunkID: "utterance-locked-1", CandidateID: "utterance-locked",
			SampleOffset: 0, SampleRateHz: 24_000, PCM16LE: pcmOutput, Final: true,
		},
	}
	codec := NewStandardJSONCodec()
	encodedAudio, err := codec.Encode(PreparedAudioType(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	audioEnvelope := element.Envelope{
		Type: PreparedAudioType(), ItemID: "prepared-audio-1", SessionID: "session-locked",
		RunID: "run-locked", Sequence: 1, TraceID: "trace-locked", Payload: prepared,
	}
	wireAudio := sidecar.FromEnvelope(audioEnvelope, encodedAudio.JSON)
	wireAudio.Media = encodedAudio.Media
	audioMessage := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "audio_out", Envelope: &wireAudio,
		Payload: encodedAudio.Binary, PayloadBytes: len(encodedAudio.Binary),
	}
	if err := contract.ValidateSidecarFrame(audioMessage); err != nil {
		t.Fatalf("prepared-audio fixture is not negotiated data: %v", err)
	}
	session.frames <- audioMessage

	terminal := cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeSucceeded, Operation: "generate",
		RunID: "run-locked", ProviderReference: "provider/test-model",
		StartedNS: 200, FinishedNS: 300, DurationNS: 100,
	}
	encodedOutcome, err := codec.Encode(OutcomeType(), terminal)
	if err != nil {
		t.Fatal(err)
	}
	terminalEnvelope := element.Envelope{
		Type: OutcomeType(), ItemID: "outcome-locked", SessionID: "session-locked",
		RunID: "run-locked", Sequence: 1, TraceID: "trace-locked", Payload: terminal,
	}
	wireOutcome := sidecar.FromEnvelope(terminalEnvelope, encodedOutcome.JSON)
	outcomeMessage := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "outcome", Envelope: &wireOutcome,
		PayloadBytes: len(encodedOutcome.Binary), Payload: encodedOutcome.Binary,
	}
	if err := contract.ValidateSidecarFrame(outcomeMessage); err != nil {
		t.Fatalf("outcome fixture is not negotiated data: %v", err)
	}
	session.frames <- outcomeMessage

	preparedEnvelope := receiveEnvelope(t, preparedOutput)
	preparedNative, ok := preparedEnvelope.Payload.(*speech.AudioFrame)
	if !ok {
		t.Fatalf("prepared audio reconstructed as %T, want *speech.AudioFrame", preparedEnvelope.Payload)
	}
	if preparedNative.Kind != speech.AudioChunk || preparedNative.UtteranceID != prepared.UtteranceID ||
		!bytes.Equal(preparedNative.Chunk.PCM16LE, pcmOutput) ||
		preparedNative.Chunk.SampleRateHz != 24_000 {
		t.Fatalf("reconstructed prepared audio = %+v", preparedNative)
	}
	pcmOutput[0] ^= 0xff
	if preparedNative.Chunk.PCM16LE[0] == pcmOutput[0] {
		t.Fatal("reconstructed prepared audio aliases sidecar-owned binary memory")
	}

	terminalResult := receiveEnvelope(t, outcomeOutput)
	terminalNative, ok := terminalResult.Payload.(*cognitionelements.Outcome)
	if !ok {
		t.Fatalf("outcome reconstructed as %T, want *cognition.Outcome", terminalResult.Payload)
	}
	if terminalNative.Kind != terminal.Kind || terminalNative.Operation != terminal.Operation ||
		terminalNative.RunID != terminal.RunID || terminalNative.DurationNS != terminal.DurationNS {
		t.Fatalf("reconstructed cognition outcome = %+v", terminalNative)
	}
}

type lockedReference struct {
	graph  ir.Graph
	values map[string]json.RawMessage
	config Config
}

func assertLockedComponentHello(t *testing.T, reference lockedReference, hello sidecar.Message) {
	t.Helper()
	if hello.Type != sidecar.TypeHello || hello.Version != sidecar.VersionElementGraph ||
		hello.ElementDescriptor == nil {
		t.Fatalf("deployment hello = %+v", hello)
	}
	identity, err := hello.ElementDescriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if len(reference.graph.Nodes) != 1 || identity != reference.graph.Nodes[0].Element {
		t.Fatalf("hello descriptor identity = %+v, locked graph node = %+v",
			identity, reference.graph.Nodes)
	}
	if !reflect.DeepEqual([]byte(hello.ElementConfig), []byte(reference.config.Settings)) {
		t.Fatalf("hello settings = %s, bound settings = %s",
			hello.ElementConfig, reference.config.Settings)
	}
	if !reflect.DeepEqual(hello.RequiredCapabilities, reference.config.RequiredCapabilities) {
		t.Fatalf("hello requirements = %+v, bound requirements = %+v",
			hello.RequiredCapabilities, reference.config.RequiredCapabilities)
	}

	configured := make(map[string]struct{}, len(reference.config.PortFormats))
	for _, selection := range hello.SelectedPorts {
		want, explicit := reference.config.PortFormats[selection.Name]
		if explicit {
			configured[selection.Name] = struct{}{}
		} else {
			want = []sidecar.WireFormat{sidecar.JSONWireFormat()}
		}
		if !reflect.DeepEqual(selection.Formats, want) {
			t.Fatalf("selected port %s formats = %+v, want %+v",
				selection.Name, selection.Formats, want)
		}
		_, media, err := sidecar.MediaKindForType(selection.Type)
		if err != nil {
			t.Fatal(err)
		}
		if media != explicit {
			t.Fatalf("selected port %s media=%v but explicit format=%v", selection.Name, media, explicit)
		}
	}
	if len(configured) != len(reference.config.PortFormats) {
		t.Fatalf("only %d/%d configured media ports reached the v4 offer",
			len(configured), len(reference.config.PortFormats))
	}
}

func hasLiveCapability(
	capabilities []inspect.CapabilityIdentity, name, contract string, adapterRequired bool,
) bool {
	for _, capability := range capabilities {
		if capability.Name == name && capability.Contract == contract &&
			capability.Provider.ID == "provider/test-model" &&
			capability.Provider.Revision == "2026-08-29" {
			return !adapterRequired || capability.Adapter != nil &&
				capability.Adapter.ID == "adapter/element-wire" &&
				capability.Adapter.Revision == "4"
		}
	}
	return false
}

func compileReference(t *testing.T, directory string) (ir.Graph, Config) {
	t.Helper()
	reference := loadReference(t, directory)
	return reference.graph, reference.config
}

func loadReference(t *testing.T, directory string) lockedReference {
	t.Helper()
	root := filepath.Join("..", "..", "graphs", "components", directory)
	topology, err := os.ReadFile(filepath.Join(root, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptor(catalog); err != nil {
		t.Fatal(err)
	}
	lockPayload, err := os.ReadFile(filepath.Join(root, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockPayload)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesSource, err := os.ReadFile(filepath.Join(root, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML("agent.values.yaml", valuesSource)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeConfig(bound.Values["foreground"])
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]json.RawMessage, len(bound.Values))
	for name, value := range bound.Values {
		values[name] = append(json.RawMessage(nil), value...)
	}
	return lockedReference{graph: bound.Graph, values: values, config: config}
}

func boundaryNames(graph ir.Graph) ([]string, []string) {
	var inputs, outputs []string
	for _, boundary := range graph.Boundaries {
		switch boundary.Direction {
		case ir.InputBoundary:
			inputs = append(inputs, boundary.Name)
		case ir.OutputBoundary:
			outputs = append(outputs, boundary.Name)
		}
	}
	sort.Strings(inputs)
	sort.Strings(outputs)
	return inputs, outputs
}
