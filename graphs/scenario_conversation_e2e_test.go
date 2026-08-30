package graphs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

const (
	scenarioEndpointModelName = "scenario-conversation-endpoint-test"
	scenarioEndpointVoice     = "scenario-endpoint-voice"
	scenarioEndpointTool      = "lookup.weather"
	scenarioEndpointCallID    = "call_weather_endpoint_1"
	scenarioEndpointPrompt    = "Follow the exact endpoint scenario instructions."
)

var errScenarioEndpointProvider = errors.New("scripted endpoint provider failure")

func TestScenarioConversationGraphRoundTripsUnchangedRealtimeEndpoint(t *testing.T) {
	fixture := newScenarioEndpointFixture(t)
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.Cases) != 11 {
		t.Fatalf("graph-native scenario contract has %d cases, want 11", len(contract.Cases))
	}
	delegate, err := graphs.ScenarioConversationApplicationRegistration(fixture.registration)
	if err != nil {
		t.Fatal(err)
	}
	application, err := graphnative.NewApplicationPlugin(graphnative.ApplicationPluginConfig{
		Reference: "application.openrealtime.scenario-conversation.graph-native-e2e.v1",
		Artifact:  fixture.artifact("scenario-contract", "1"),
		Delegate:  delegate,
	})
	if err != nil {
		t.Fatal(err)
	}
	delegateConfiguration, err := json.Marshal(fixture.application)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, delegate, delegateConfiguration,
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := graphnative.FreezeLaunchProfile(context.Background(), graphnative.LaunchProfileConfig{
		Name: "openrealtime.scenario-conversation.graph-native-endpoint-test", Revision: 1,
		Contract: contract, Application: application, Configuration: configuration,
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.scenario-conversation-endpoint-test", ProfileRevision: 1,
			ProviderArtifact: fixture.registration.ProviderArtifact,
			GatewayArtifact:  fixture.gatewayArtifact,
			Model:            scenarioEndpointModelName, TranscriptionModel: "scenario-endpoint-asr",
			ValidateWire: true, InspectionTokenTTLMS: 30_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{application})
	if err != nil {
		t.Fatal(err)
	}
	fixture.assertFactories(t, 0)

	bundle, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: fixture.gatewayArtifact,
		})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.GraphPlan.Identity() != profile.Plan {
		t.Fatal("profiled scenario endpoint changed its exact graph plan")
	}
	fixture.assertFactories(t, 0)
	realm, err := bundle.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := realm.Close(ctx); closeErr != nil {
			t.Errorf("close scenario endpoint realm: %v", closeErr)
		}
	})
	fixture.assertFactories(t, 0)

	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	connection, _, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+
			"/v1/realtime?model="+scenarioEndpointModelName, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})
	client := &scenarioEndpointWireClient{
		t: t, connection: connection, validator: protocol.NewValidator(),
	}
	client.awaitType(5*time.Second, "session.created")
	fixture.assertFactories(t, 1)

	client.send(scenarioEndpointSessionUpdate())
	updated := client.awaitType(5*time.Second, "session.updated")
	assertScenarioEndpointSession(t, updated)

	// The automatic endpoint path is the real graph-owned EnergyAdmission ->
	// ASR -> FinalObservationGate -> SessionInvocation path. No manual commit
	// or adapter-local response shortcut is used.
	for index := 0; index < 3; index++ {
		client.send(map[string]any{
			"type": "input_audio_buffer.append", "audio": scenarioEndpointTone(2_400),
		})
	}
	for index := 0; index < 5; index++ {
		client.send(map[string]any{
			"type": "input_audio_buffer.append", "audio": scenarioEndpointSilence(2_400),
		})
	}
	client.awaitType(5*time.Second, "input_audio_buffer.speech_started")
	client.awaitType(5*time.Second, "input_audio_buffer.speech_stopped")
	transcript := client.awaitType(5*time.Second,
		"conversation.item.input_audio_transcription.completed")
	if transcript["transcript"] != "weather in Paris" {
		t.Fatalf("scenario endpoint transcript = %+v", transcript)
	}
	call := client.awaitType(10*time.Second, "response.function_call_arguments.done")
	if call["call_id"] != scenarioEndpointCallID || call["name"] != scenarioEndpointTool ||
		call["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("scenario endpoint function call = %+v", call)
	}
	callResponseID, _ := call["response_id"].(string)
	client.awaitResponseDone(10*time.Second, callResponseID, "completed")
	if fixture.asr.finalizes.Load() != 1 || fixture.asr.pushes.Load() == 0 {
		t.Fatalf("scenario endpoint ASR pushes=%d finalizes=%d",
			fixture.asr.pushes.Load(), fixture.asr.finalizes.Load())
	}

	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "evt_tool_result",
		"item": map[string]any{
			"type": "function_call_output", "call_id": scenarioEndpointCallID,
			"output": `{"ok":true,"temperature_c":21}`,
		},
	})
	client.await(5*time.Second, false, func(message map[string]any) bool {
		if message["type"] != "conversation.item.created" {
			return false
		}
		item, _ := message["item"].(map[string]any)
		return item["type"] == "function_call_output" && item["call_id"] == scenarioEndpointCallID
	})
	client.send(map[string]any{"type": "response.create", "event_id": "evt_tool_resume"})
	var resultEvidence trajectory.ToolResult
	select {
	case resultEvidence = <-fixture.model.toolResults:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for canonical tool result; model invocations=%d",
			fixture.model.invocations.Load())
	}
	if resultEvidence.CallID != scenarioEndpointCallID || resultEvidence.Name != scenarioEndpointTool ||
		string(resultEvidence.Output) != `{"ok":true,"temperature_c":21}` {
		t.Fatalf("scenario endpoint model tool result = %+v", resultEvidence)
	}
	firstSpeech := client.awaitType(10*time.Second, "response.output_audio.delta")
	assertScenarioEndpointAudio(t, firstSpeech)
	firstSpeechResponse, _ := firstSpeech["response_id"].(string)
	client.awaitResponseDone(10*time.Second, firstSpeechResponse, "completed")

	imageBytes, imageURL := scenarioEndpointJPEG(t, 64, 48)
	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "evt_image",
		"item": map[string]any{
			"id": "visual_item_1", "type": "message", "role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "Inspect the submitted still image."},
				{"type": "input_image", "image_url": imageURL},
			},
		},
	})
	client.awaitCreatedItem(5*time.Second, "visual_item_1")
	client.send(map[string]any{"type": "response.create", "event_id": "evt_image_create"})
	mediaEvidence := receiveScenarioEndpoint(t, fixture.model.media, "resolved retained image")
	if mediaEvidence.Handle == "" || mediaEvidence.MIMEType != "image/jpeg" ||
		mediaEvidence.Width != 64 || mediaEvidence.Height != 48 ||
		!bytes.Equal(mediaEvidence.Bytes, imageBytes) {
		t.Fatalf("scenario endpoint retained image evidence = %+v", mediaEvidence)
	}
	imageSpeech := client.awaitType(10*time.Second, "response.output_audio.delta")
	assertScenarioEndpointAudio(t, imageSpeech)
	imageResponse, _ := imageSpeech["response_id"].(string)
	client.awaitResponseDone(10*time.Second, imageResponse, "completed")

	client.send(scenarioEndpointTextMessage("cancel_item_1", "Start a response that will be canceled."))
	client.awaitCreatedItem(5*time.Second, "cancel_item_1")
	client.send(map[string]any{"type": "response.create", "event_id": "evt_cancel_create"})
	receiveScenarioEndpoint(t, fixture.model.cancelStarted, "cancelable model invocation")
	receiveScenarioEndpoint(t, fixture.tts.cancelStarted, "cancelable TTS invocation")
	cancelAudio := client.awaitType(10*time.Second, "response.output_audio.delta")
	assertScenarioEndpointAudio(t, cancelAudio)
	cancelResponse, _ := cancelAudio["response_id"].(string)
	client.send(map[string]any{"type": "response.cancel", "event_id": "evt_cancel"})
	receiveScenarioEndpoint(t, fixture.model.cancelObserved, "model cancellation")
	receiveScenarioEndpoint(t, fixture.tts.cancelObserved, "TTS cancellation")
	client.awaitResponseDone(10*time.Second, cancelResponse, "cancelled")

	client.send(scenarioEndpointTextMessage("failure_item_1", "Trigger the typed provider failure."))
	client.awaitCreatedItem(5*time.Second, "failure_item_1")
	client.send(map[string]any{"type": "response.create", "event_id": "evt_failure_create"})
	failure := client.awaitTypeAllowError(10*time.Second, "error")
	detail, _ := failure["error"].(map[string]any)
	if detail["type"] != "invalid_request_error" || detail["code"] != "provider_error" ||
		!strings.Contains(fmt.Sprint(detail["message"]), errScenarioEndpointProvider.Error()) {
		t.Fatalf("scenario endpoint typed provider failure = %+v", failure)
	}
	failedDone := client.await(10*time.Second, true, func(message map[string]any) bool {
		if message["type"] != "response.done" {
			return false
		}
		response, _ := message["response"].(map[string]any)
		return response["status"] == "incomplete"
	})
	failedResponse, _ := failedDone["response"].(map[string]any)
	failedDetails, _ := failedResponse["status_details"].(map[string]any)
	if failedDetails["type"] != "incomplete" {
		t.Fatalf("scenario endpoint failed response = %+v", failedResponse)
	}

	// The typed provider error is recoverable at the stable session boundary.
	client.send(scenarioEndpointTextMessage("recovery_item_1", "Prove the session is still usable."))
	client.awaitCreatedItem(5*time.Second, "recovery_item_1")
	client.send(map[string]any{"type": "response.create", "event_id": "evt_recovery_create"})
	recoveryAudio := client.awaitType(10*time.Second, "response.output_audio.delta")
	assertScenarioEndpointAudio(t, recoveryAudio)
	recoveryResponse, _ := recoveryAudio["response_id"].(string)
	client.awaitResponseDone(10*time.Second, recoveryResponse, "completed")
	barrier := scenarioEndpointSessionUpdate()
	barrier["event_id"] = "evt_final_session_barrier"
	client.send(barrier)
	client.awaitType(5*time.Second, "session.updated")
	client.assertNoResponseOutputAfterDone(t, cancelResponse)

	if fixture.model.invocations.Load() != 6 {
		t.Fatalf("scenario endpoint model invocations = %d, want 6", fixture.model.invocations.Load())
	}
	fixture.assertFactories(t, 1)
	client.assertBalancedResponseLifecycle(t)
}

type scenarioEndpointFixture struct {
	application     scenarioconversation.ApplicationConfig
	registration    scenarioconversation.ApplicationRegistrationConfig
	gatewayArtifact inspect.ArtifactIdentity
	asr             *scenarioEndpointASR
	model           *scenarioEndpointModel
	tts             *scenarioEndpointTTS
	asrFactories    atomic.Int32
	modelFactories  atomic.Int32
	ttsFactories    atomic.Int32
}

func newScenarioEndpointFixture(t testing.TB) *scenarioEndpointFixture {
	t.Helper()
	fixture := &scenarioEndpointFixture{}
	fixture.asr = &scenarioEndpointASR{}
	fixture.model = newScenarioEndpointModel()
	fixture.tts = newScenarioEndpointTTS()
	fixture.gatewayArtifact = fixture.artifact("gateway", "1")
	asrSelection := scenarioconversation.ApplicationASRSelection{
		Reference: "plugin.test.scenario-endpoint.asr.v1", Artifact: fixture.artifact("asr", "1"),
		Descriptor: scenarioEndpointASRDescriptor(),
	}
	modelSelection := scenarioconversation.ApplicationModelSelection{
		Reference: "plugin.test.scenario-endpoint.model.v1", Artifact: fixture.artifact("model", "1"),
		Descriptor: scenarioEndpointModelDescriptor(),
	}
	ttsSelection := scenarioconversation.ApplicationTTSSelection{
		Reference: "plugin.test.scenario-endpoint.tts.v1", Artifact: fixture.artifact("tts", "1"),
		Descriptor: scenarioEndpointTTSDescriptor(), Voice: scenarioEndpointVoice,
	}
	gate := perception.DefaultGateConfig()
	architecture, err := projectarch.Default().Resolve("cascade.controlled@3")
	if err != nil {
		t.Fatal(err)
	}
	fixture.application = scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		Architecture:  architecture.Identity(),
		ASR:           asrSelection, Model: modelSelection, TTS: ttsSelection,
		Tools: []scenarioconversation.ToolDeclaration{{
			Name: scenarioEndpointTool, Description: "Look up exact weather data.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			Confirm:    legacyaction.ConfirmNever,
		}},
		Target: computeruse.Target{
			Name: "scenario-client", Sources: []string{scenarioconversation.SourceMessage},
			Width: 64, Height: 48,
		},
		Gate: scenarioconversation.ApplicationGateSelection{
			Threshold: gate.Threshold, PrefixPaddingMS: gate.PrefixPaddingMS,
			SilenceDurationMS: gate.SilenceDurationMS, SpeechDurationMS: gate.SpeechDurationMS,
		},
		Media: scenarioconversation.MediaLimits{
			MaxItems: 8, MaxBytes: 4 << 20, MaxItemBytes: 2 << 20,
			MaxPending: 8, MaxActiveLeases: 16,
		},
		MaxOutputTokens: 4096,
	}
	fixture.registration = scenarioconversation.ApplicationRegistrationConfig{
		ApplicationArtifact: fixture.artifact("application", "1"),
		ProviderArtifact:    fixture.artifact("session-provider", "1"),
		RuntimeArtifact:     fixture.artifact("session-adapter", "2"),
		DependencyArtifact:  fixture.artifact("session-dependencies", "1"),
		ASR: []scenarioconversation.ASRFactoryRegistration{{
			ApplicationASRSelection: asrSelection,
			Factory: func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				fixture.asrFactories.Add(1)
				return fixture.asr, nil
			},
		}},
		Models: []scenarioconversation.ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				fixture.modelFactories.Add(1)
				return fixture.model, nil
			},
		}},
		TTS: []scenarioconversation.TTSFactoryRegistration{{
			ApplicationTTSSelection: ttsSelection,
			Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				fixture.ttsFactories.Add(1)
				return fixture.tts, nil
			},
		}},
	}
	return fixture
}

func (fixture *scenarioEndpointFixture) artifact(name, revision string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "plugin://test/scenario-conversation-endpoint/" + name, Revision: "build:" + revision,
	}
}

func (fixture *scenarioEndpointFixture) assertFactories(t testing.TB, wanted int32) {
	t.Helper()
	if fixture.asrFactories.Load() != wanted || fixture.modelFactories.Load() != wanted ||
		fixture.ttsFactories.Load() != wanted {
		t.Fatalf("scenario endpoint factories ASR=%d model=%d TTS=%d, want %d each",
			fixture.asrFactories.Load(), fixture.modelFactories.Load(),
			fixture.ttsFactories.Load(), wanted)
	}
}

type scenarioEndpointASR struct {
	pushes    atomic.Int32
	finalizes atomic.Int32
}

func (*scenarioEndpointASR) Descriptor() v1.Descriptor { return scenarioEndpointASRDescriptor() }

func (provider *scenarioEndpointASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	sequence := provider.pushes.Add(1)
	return []v1.PerceptionRevision{{
		RevisionID: uint64(sequence), SourceSample: frame.SampleOffset,
		StableText: "weather", UnstableText: " in Paris",
	}}, nil
}

func (provider *scenarioEndpointASR) Finalize(
	_ context.Context, sample uint64,
) (v1.PerceptionRevision, error) {
	sequence := provider.finalizes.Add(1)
	return v1.PerceptionRevision{
		RevisionID: uint64(provider.pushes.Load()) + uint64(sequence), SourceSample: sample,
		StableText: "weather in Paris", Final: true,
	}, nil
}

func scenarioEndpointASRDescriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "scenario-endpoint-asr", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true, v1.CapabilityRevisions: true,
			v1.CapabilityCancellation: true,
		},
	}
}

type scenarioEndpointMediaEvidence struct {
	Handle   string
	MIMEType string
	Width    int
	Height   int
	Bytes    []byte
}

type scenarioEndpointModel struct {
	invocations    atomic.Int32
	toolResults    chan trajectory.ToolResult
	media          chan scenarioEndpointMediaEvidence
	cancelStarted  chan struct{}
	cancelObserved chan struct{}
	cancelStart    sync.Once
	cancelDone     sync.Once
}

func newScenarioEndpointModel() *scenarioEndpointModel {
	return &scenarioEndpointModel{
		toolResults:   make(chan trajectory.ToolResult, 1),
		media:         make(chan scenarioEndpointMediaEvidence, 1),
		cancelStarted: make(chan struct{}), cancelObserved: make(chan struct{}),
	}
}

func (*scenarioEndpointModel) Descriptor() continuation.Descriptor {
	return scenarioEndpointModelDescriptor()
}

func (provider *scenarioEndpointModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	switch invocation := provider.invocations.Add(1); invocation {
	case 1:
		if request.Invocation.Instruction != scenarioEndpointPrompt ||
			len(request.Invocation.Tools) != 1 || request.Invocation.Tools[0].Name != scenarioEndpointTool {
			return continuation.Completion{}, errors.New("endpoint invocation settings drifted")
		}
		if !scenarioEndpointHasFinalAudio(request.Trajectory, "weather in Paris") {
			return continuation.Completion{}, errors.New("endpoint final audio observation is missing")
		}
		call := trajectory.ToolCall{
			CallID: scenarioEndpointCallID, Name: scenarioEndpointTool,
			Arguments: json.RawMessage(`{"city":"Paris"}`),
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
			return continuation.Completion{}, err
		}
		return continuation.Completion{StopReason: "tool_call"}, nil
	case 2:
		for _, item := range request.Trajectory.Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
				item.InvocationID != "" && item.ToolResult.CallID == scenarioEndpointCallID {
				provider.toolResults <- *item.ToolResult
				return scenarioEndpointSpeak(emit, "Weather result accepted.")
			}
		}
		return continuation.Completion{}, errors.New("canonical tool result is missing")
	case 3:
		if request.Media == nil {
			return continuation.Completion{}, errors.New("retained-media resolver is missing")
		}
		for _, item := range request.Trajectory.Items {
			if item.Kind != trajectory.KindObservation || item.Observation == nil {
				continue
			}
			for _, reference := range item.Observation.Media {
				resolved, err := request.Media(reference.Handle)
				if err != nil {
					return continuation.Completion{}, err
				}
				provider.media <- scenarioEndpointMediaEvidence{
					Handle: reference.Handle, MIMEType: resolved.MIMEType,
					Width: reference.Width, Height: reference.Height,
					Bytes: slices.Clone(resolved.Bytes),
				}
				return scenarioEndpointSpeak(emit, "Image bytes verified.")
			}
		}
		return continuation.Completion{}, errors.New("retained image handle is missing")
	case 4:
		if err := emit(continuation.Event{
			Kind: continuation.EventAssistantDelta,
			Text: "Cancellation audio starts. Pending response remains",
		}); err != nil {
			return continuation.Completion{}, err
		}
		provider.cancelStart.Do(func() { close(provider.cancelStarted) })
		<-ctx.Done()
		provider.cancelDone.Do(func() { close(provider.cancelObserved) })
		return continuation.Completion{}, context.Cause(ctx)
	case 5:
		return continuation.Completion{}, errScenarioEndpointProvider
	case 6:
		return scenarioEndpointSpeak(emit, "Session recovered.")
	default:
		return continuation.Completion{}, fmt.Errorf("unexpected endpoint model invocation %d", invocation)
	}
}

func scenarioEndpointModelDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: scenarioEndpointModelName, Phase: trajectory.PhaseFast,
		Effort: continuation.EffortLow, Streaming: true, Vision: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

func scenarioEndpointSpeak(
	emit continuation.Emit, text string,
) (continuation.Completion, error) {
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: text}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func scenarioEndpointHasFinalAudio(snapshot trajectory.Snapshot, text string) bool {
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindObservation && item.Content == text &&
			item.Producer.Phase == trajectory.PhaseUser && item.Observation == nil &&
			item.Event != nil && item.Event.Channel == scenarioconversation.SourceMicrophone &&
			strings.HasSuffix(item.Event.Type, ".endpoint") {
			return true
		}
	}
	return false
}

type scenarioEndpointTTS struct {
	plans          atomic.Int32
	cancelStarted  chan struct{}
	cancelObserved chan struct{}
	cancelStart    sync.Once
	cancelDone     sync.Once
}

func newScenarioEndpointTTS() *scenarioEndpointTTS {
	return &scenarioEndpointTTS{
		cancelStarted: make(chan struct{}), cancelObserved: make(chan struct{}),
	}
}

func (*scenarioEndpointTTS) Descriptor() v1.Descriptor { return scenarioEndpointTTSDescriptor() }

func (provider *scenarioEndpointTTS) Synthesize(
	ctx context.Context, plan v1.SpeechPlan,
) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

func (provider *scenarioEndpointTTS) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	provider.plans.Add(1)
	chunk := v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":audio:1", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: scenarioEndpointPCM(2_400), Final: true,
	}
	if strings.Contains(plan.Text, "Cancellation audio starts") {
		chunk.Final = false
		if err := emit(chunk); err != nil {
			return err
		}
		provider.cancelStart.Do(func() { close(provider.cancelStarted) })
		<-ctx.Done()
		provider.cancelDone.Do(func() { close(provider.cancelObserved) })
		return context.Cause(ctx)
	}
	return emit(chunk)
}

func scenarioEndpointTTSDescriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "scenario-endpoint-tts", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
			v1.CapabilityCancellation: true,
		},
	}
}

type scenarioEndpointWireClient struct {
	t          *testing.T
	connection *websocket.Conn
	validator  *protocol.Validator
	received   []map[string]any
	pending    []map[string]any
}

func (client *scenarioEndpointWireClient) send(value map[string]any) {
	client.t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		client.t.Fatal(err)
	}
	message, err := protocol.Decode(payload)
	if err != nil {
		client.t.Fatalf("decode scenario endpoint client event: %v", err)
	}
	if err := client.validator.Validate(protocol.ProfileRealtime, protocol.DirectionClient, message); err != nil {
		client.t.Fatalf("validate scenario endpoint client event %q: %v", value["type"], err)
	}
	if err := client.connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		client.t.Fatal(err)
	}
}

func (client *scenarioEndpointWireClient) awaitType(
	timeout time.Duration, eventType string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, false, func(message map[string]any) bool {
		return message["type"] == eventType
	})
}

func (client *scenarioEndpointWireClient) awaitTypeAllowError(
	timeout time.Duration, eventType string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, true, func(message map[string]any) bool {
		return message["type"] == eventType
	})
}

func (client *scenarioEndpointWireClient) await(
	timeout time.Duration, allowError bool, match func(map[string]any) bool,
) map[string]any {
	client.t.Helper()
	for index, message := range client.pending {
		if match(message) {
			client.pending = append(client.pending[:index], client.pending[index+1:]...)
			return message
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, payload, err := client.connection.Read(ctx)
		if err != nil {
			client.t.Fatalf("read scenario endpoint: %v (events %s)", err, client.eventTypes())
		}
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			client.t.Fatalf("decode scenario endpoint server event: %v", err)
		}
		client.received = append(client.received, message)
		decoded, err := protocol.Decode(payload)
		if err != nil {
			client.t.Fatalf("decode pinned scenario endpoint event %q: %v", message["type"], err)
		}
		if err := client.validator.Validate(protocol.ProfileRealtime, protocol.DirectionServer, decoded); err != nil {
			client.t.Fatalf("validate pinned scenario endpoint event %q: %v", message["type"], err)
		}
		if message["type"] == "error" && !allowError {
			client.t.Fatalf("unexpected scenario endpoint error: %+v", message["error"])
		}
		if match(message) {
			return message
		}
		client.pending = append(client.pending, message)
	}
}

func (client *scenarioEndpointWireClient) awaitCreatedItem(timeout time.Duration, itemID string) {
	client.t.Helper()
	client.await(timeout, false, func(message map[string]any) bool {
		if message["type"] != "conversation.item.created" {
			return false
		}
		item, _ := message["item"].(map[string]any)
		return item["id"] == itemID
	})
}

func (client *scenarioEndpointWireClient) awaitResponseDone(
	timeout time.Duration, responseID, status string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, false, func(message map[string]any) bool {
		if message["type"] != "response.done" {
			return false
		}
		response, _ := message["response"].(map[string]any)
		return response["id"] == responseID && response["status"] == status
	})
}

func (client *scenarioEndpointWireClient) assertNoResponseOutputAfterDone(
	t testing.TB, responseID string,
) {
	t.Helper()
	done := -1
	for index, message := range client.received {
		if message["type"] == "response.done" {
			response, _ := message["response"].(map[string]any)
			if response["id"] == responseID {
				done = index
				continue
			}
		}
		if done >= 0 && message["response_id"] == responseID &&
			strings.HasPrefix(fmt.Sprint(message["type"]), "response.output_") {
			t.Fatalf("late output after canceled response %q: %+v", responseID, message)
		}
	}
	if done < 0 {
		t.Fatalf("canceled response %q has no terminal response.done", responseID)
	}
}

func (client *scenarioEndpointWireClient) assertBalancedResponseLifecycle(t testing.TB) {
	t.Helper()
	created := make(map[string]int)
	done := make(map[string]int)
	for _, message := range client.received {
		switch message["type"] {
		case "response.created":
			response, _ := message["response"].(map[string]any)
			created[fmt.Sprint(response["id"])]++
		case "response.done":
			response, _ := message["response"].(map[string]any)
			done[fmt.Sprint(response["id"])]++
		}
	}
	if len(created) != len(done) {
		t.Fatalf("scenario endpoint response lifecycle created=%v done=%v", created, done)
	}
	for id, count := range created {
		if id == "" || count != 1 || done[id] != 1 {
			t.Fatalf("scenario endpoint response lifecycle created=%v done=%v", created, done)
		}
	}
}

func (client *scenarioEndpointWireClient) eventTypes() string {
	types := make([]string, 0, len(client.received))
	for _, message := range client.received {
		types = append(types, fmt.Sprint(message["type"]))
	}
	return strings.Join(types, ",")
}

func scenarioEndpointSessionUpdate() map[string]any {
	return map[string]any{
		"type": "session.update", "event_id": "evt_session_update",
		"session": map[string]any{
			"type": "realtime", "instructions": scenarioEndpointPrompt,
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"input": map[string]any{
					"format":        map[string]any{"type": "audio/pcm", "rate": 24_000},
					"transcription": map[string]any{"model": "scenario-endpoint-asr"},
					"turn_detection": map[string]any{
						"type": "server_vad", "threshold": 0.5,
						"prefix_padding_ms": 300, "silence_duration_ms": 500,
					},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24_000},
					"voice":  scenarioEndpointVoice,
				},
			},
			"tools": []map[string]any{{
				"type": "function", "name": scenarioEndpointTool,
				"description": "Look up exact weather data.",
				"parameters": map[string]any{
					"type": "object", "properties": map[string]any{
						"city": map[string]any{"type": "string"},
					}, "required": []string{"city"},
				},
			}},
		},
	}
}

func assertScenarioEndpointSession(t testing.TB, event map[string]any) {
	t.Helper()
	session, _ := event["session"].(map[string]any)
	if session["instructions"] != scenarioEndpointPrompt || session["model"] != scenarioEndpointModelName {
		t.Fatalf("scenario endpoint updated session = %+v", session)
	}
	modalities, _ := session["output_modalities"].([]any)
	if len(modalities) != 1 || modalities[0] != "audio" {
		t.Fatalf("scenario endpoint modalities = %v", modalities)
	}
	tools, _ := session["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("scenario endpoint tools = %v", tools)
	}
}

func scenarioEndpointTextMessage(itemID, text string) map[string]any {
	return map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"id": itemID, "type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
	}
}

func scenarioEndpointTone(samples int) string {
	return base64.StdEncoding.EncodeToString(scenarioEndpointPCM(samples))
}

func scenarioEndpointSilence(samples int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, samples*2))
}

func scenarioEndpointPCM(samples int) []byte {
	result := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(8_000)
		if index%32 >= 16 {
			value = -value
		}
		binary.LittleEndian.PutUint16(result[index*2:], uint16(value))
	}
	return result
}

func scenarioEndpointJPEG(t testing.TB, width, height int) ([]byte, string) {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 5), B: 177, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 91}); err != nil {
		t.Fatal(err)
	}
	content := slices.Clone(buffer.Bytes())
	return content, "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(content)
}

func assertScenarioEndpointAudio(t testing.TB, event map[string]any) {
	t.Helper()
	encoded, _ := event["delta"].(string)
	audio, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(audio) == 0 {
		t.Fatalf("scenario endpoint audio delta = %+v, err %v", event, err)
	}
}

func receiveScenarioEndpoint[T any](t testing.TB, source <-chan T, label string) T {
	t.Helper()
	select {
	case value := <-source:
		return value
	case <-time.After(10 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", label)
		return zero
	}
}

var (
	_ v1.PerceptionProvider      = (*scenarioEndpointASR)(nil)
	_ continuation.Provider      = (*scenarioEndpointModel)(nil)
	_ v1.StreamingSpeechProvider = (*scenarioEndpointTTS)(nil)
)
