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
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

const (
	scenarioEndpointModelName    = "scenario-conversation-endpoint-test"
	scenarioEndpointVoice        = "scenario-endpoint-voice"
	scenarioEndpointSilentCallID = "call_click_silent_endpoint_1"
	scenarioEndpointPrompt       = "Follow the exact endpoint scenario instructions."
)

var errScenarioEndpointProvider = errors.New("scripted endpoint provider failure")

type scenarioEndpointToolCase struct {
	name               string
	tool               string
	callID             string
	stableTranscript   string
	unstableTranscript string
	finalTranscript    string
	description        string
	parameters         string
	normalizers        []legacyaction.ToolArgumentNormalizer
	proposalArguments  string
	effectiveArguments string
	result             string
	reply              string
	wantSpeechPlans    []string
}

func scenarioEndpointToolCases() []scenarioEndpointToolCase {
	return []scenarioEndpointToolCase{
		{
			name:               "byte-exact weather call",
			tool:               "lookup.weather",
			callID:             "call_weather_endpoint_1",
			stableTranscript:   "weather",
			unstableTranscript: " in Paris",
			finalTranscript:    "weather in Paris",
			description:        "Look up exact weather data.",
			parameters:         `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`,
			proposalArguments:  `{"city":"Paris"}`,
			effectiveArguments: `{"city":"Paris"}`,
			result:             `{"ok":true,"temperature_c":21}`,
			reply:              "Weather result accepted.",
		},
		{
			name:               "deployment-normalized spoken identifier",
			tool:               "track_order",
			callID:             "call_track_order_endpoint_1",
			stableTranscript:   "track order",
			unstableTranscript: " X Y Z88",
			finalTranscript:    "track order X Y Z88",
			description:        "Track an order by its spoken identifier.",
			parameters:         `{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"]}`,
			normalizers: []legacyaction.ToolArgumentNormalizer{{
				Argument: "order_id", Normalizer: legacyaction.ToolParameterCompactASCIIAlphanumericV1,
			}},
			proposalArguments:  `{"order_id":"X Y Z88"}`,
			effectiveArguments: `{"order_id":"XYZ88"}`,
			result:             `{"ok":true,"status":"shipped"}`,
			reply:              "Order result accepted.",
		},
	}
}

func TestScenarioConversationGraphRoundTripsUnchangedRealtimeEndpoint(t *testing.T) {
	for _, toolCase := range scenarioEndpointToolCases() {
		toolCase := toolCase
		t.Run(toolCase.name, func(t *testing.T) {
			testScenarioConversationGraphRoundTrip(t, toolCase)
		})
	}
}

func TestScenarioConversationSpeechCoalescesShortClausesAndKeepsShortAnswers(t *testing.T) {
	test := scenarioEndpointToolCases()[0]
	test.reply = "First, check the forecast. Yes. Done"
	test.wantSpeechPlans = []string{"First, check the forecast.", "Yes.", "Done"}
	testScenarioConversationGraphRoundTrip(t, test)
}

func TestScenarioConversationCompletionBurstDrainsThroughRealtimeEndpoint(t *testing.T) {
	test := scenarioEndpointToolCases()[0]
	for i := 0; i < 40; i++ {
		test.wantSpeechPlans = append(test.wantSpeechPlans, fmt.Sprintf("This is sentence number %d.", i+1))
	}
	test.reply = strings.Join(test.wantSpeechPlans, " ")
	testScenarioConversationGraphRoundTrip(t, test)
}

func testScenarioConversationGraphRoundTrip(t *testing.T, toolCase scenarioEndpointToolCase) {
	t.Helper()
	fixture := newScenarioEndpointFixture(t, toolCase)
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.Cases) != 12 {
		t.Fatalf("graph-native scenario contract has %d cases, want 12", len(contract.Cases))
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

	client.send(scenarioEndpointSessionUpdate(t, toolCase))
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
	if transcript["transcript"] != toolCase.finalTranscript {
		t.Fatalf("scenario endpoint transcript = %+v", transcript)
	}
	call := client.awaitType(10*time.Second, "response.function_call_arguments.done")
	if call["call_id"] != toolCase.callID || call["name"] != toolCase.tool ||
		call["arguments"] != toolCase.effectiveArguments {
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
			"type": "function_call_output", "call_id": toolCase.callID,
			"output": toolCase.result,
		},
	})
	client.await(5*time.Second, false, func(message map[string]any) bool {
		if message["type"] != "conversation.item.created" {
			return false
		}
		item, _ := message["item"].(map[string]any)
		return item["type"] == "function_call_output" && item["call_id"] == toolCase.callID
	})
	client.send(map[string]any{"type": "response.create", "event_id": "evt_tool_resume"})
	var resultEvidence trajectory.ToolResult
	select {
	case resultEvidence = <-fixture.model.toolResults:
	case <-time.After(10 * time.Second):
		readContext, cancelRead := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, wire, wireErr := client.connection.Read(readContext)
		cancelRead()
		t.Fatalf("timed out waiting for canonical tool result; model invocations=%d policy decisions=%d next_wire=%s read_error=%v",
			fixture.model.invocations.Load(), fixture.policy.decisions.Load(), wire, wireErr)
	}
	if resultEvidence.CallID != toolCase.callID || resultEvidence.Name != toolCase.tool ||
		string(resultEvidence.Output) != toolCase.result {
		t.Fatalf("scenario endpoint model tool result = %+v", resultEvidence)
	}
	firstSpeech := client.awaitType(10*time.Second, "response.output_audio.delta")
	assertScenarioEndpointAudio(t, firstSpeech)
	firstSpeechResponse, _ := firstSpeech["response_id"].(string)
	if toolCase.wantSpeechPlans != nil {
		client.await(10*time.Second, false, func(message map[string]any) bool {
			if message["type"] != "response.done" {
				return false
			}
			audioChunks := 0
			var lastResponse any
			for _, received := range client.received {
				if received["type"] == "response.output_audio.delta" {
					audioChunks++
					lastResponse = received["response_id"]
				}
			}
			response, _ := message["response"].(map[string]any)
			return audioChunks >= len(toolCase.wantSpeechPlans) && response["id"] == lastResponse
		})
	} else {
		client.awaitResponseDone(10*time.Second, firstSpeechResponse, "completed")
	}
	spokenPlans := fixture.tts.plannedTexts()
	if spoken := strings.Join(spokenPlans, " "); spoken != toolCase.reply {
		t.Fatalf("action continuation reached TTS as %q", spoken)
	}
	if toolCase.wantSpeechPlans != nil && !slices.Equal(spokenPlans, toolCase.wantSpeechPlans) {
		t.Fatalf("scenario speech plans = %q, want %q", spokenPlans, toolCase.wantSpeechPlans)
	}
	for _, control := range []string{
		toolCase.tool, toolCase.proposalArguments, toolCase.effectiveArguments, "order_id",
	} {
		if slices.ContainsFunc(spokenPlans, func(text string) bool { return strings.Contains(text, control) }) {
			t.Fatalf("serialized action control %q reached TTS: %q", control, spokenPlans)
		}
	}
	if toolCase.wantSpeechPlans != nil {
		// Inspect all segments, including a response opened after earlier
		// segments completed. All expected speech must reach the client and
		// every response must finish successfully before this variant ends.
		var audible []string
		audioChunks := 0
		for _, message := range client.received {
			if message["type"] == "response.done" {
				response, _ := message["response"].(map[string]any)
				if response["status"] != "completed" {
					t.Fatalf("segmented reply ended unsuccessfully: %+v", response)
				}
			}
			if message["type"] == "response.output_audio_transcript.delta" {
				audible = append(audible, fmt.Sprint(message["delta"]))
			}
			if message["type"] == "response.output_audio.delta" {
				audioChunks++
				assertScenarioEndpointAudio(t, message)
			}
		}
		if !slices.Equal(audible, toolCase.wantSpeechPlans) || audioChunks != len(toolCase.wantSpeechPlans) {
			t.Fatalf("scenario endpoint rendered %q with %d audio chunks, want %q",
				audible, audioChunks, toolCase.wantSpeechPlans)
		}
		client.assertBalancedResponseLifecycle(t)
		return
	}

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

	// A second automatic audio turn deterministically selects act-silently.
	// Silent cognition may still produce an authorized tool action, but its
	// text port is graph-terminated before segmentation/TTS/playback.
	policyDecisionsBeforeSilent := fixture.policy.decisions.Load()
	if !fixture.policy.silentNext.CompareAndSwap(false, true) {
		t.Fatal("scenario endpoint silent policy decision was already armed")
	}
	ttsPlansBeforeSilent := fixture.tts.plans.Load()
	silentEventStart := len(client.received)
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
	client.awaitType(5*time.Second, "conversation.item.input_audio_transcription.completed")
	silentCall := client.awaitType(10*time.Second, "response.function_call_arguments.done")
	if silentCall["call_id"] != scenarioEndpointSilentCallID ||
		silentCall["name"] != computeruse.Click ||
		silentCall["arguments"] != `{"source":"screen","x":10,"y":20}` {
		t.Fatalf("silent cognition tool call = %+v", silentCall)
	}
	silentResponseID, _ := silentCall["response_id"].(string)
	client.awaitResponseDone(10*time.Second, silentResponseID, "completed")
	if fixture.policy.silentNext.Load() || fixture.policy.silentDecisions.Load() != 1 ||
		fixture.policy.decisions.Load() != policyDecisionsBeforeSilent+1 {
		t.Fatalf("scenario endpoint silent policy decision: armed=%t silent=%d decisions=%d, want false/1/%d",
			fixture.policy.silentNext.Load(), fixture.policy.silentDecisions.Load(),
			fixture.policy.decisions.Load(), policyDecisionsBeforeSilent+1)
	}
	barrier := scenarioEndpointSessionUpdate(t, toolCase)
	barrier["event_id"] = "evt_final_session_barrier"
	client.send(barrier)
	client.awaitType(5*time.Second, "session.updated")
	client.assertNoResponseOutputAfterDone(t, cancelResponse)
	if fixture.tts.plans.Load() != ttsPlansBeforeSilent {
		t.Fatalf("silent cognition reached TTS: plans moved from %d to %d",
			ttsPlansBeforeSilent, fixture.tts.plans.Load())
	}
	for _, event := range client.received[silentEventStart:] {
		if event["type"] == "response.output_audio.delta" {
			t.Fatalf("silent cognition emitted gateway audio: %+v", event)
		}
	}

	if fixture.model.invocations.Load() != 7 {
		t.Fatalf("scenario endpoint model invocations=%d, want 7",
			fixture.model.invocations.Load())
	}
	if fixture.asrFactories.Load() != 2 || fixture.policyFactories.Load() != 2 ||
		fixture.modelFactories.Load() != 2 || fixture.ttsFactories.Load() != 1 {
		t.Fatalf("scenario endpoint factories ASR=%d policy=%d model=%d TTS=%d, want 2/2/2/1",
			fixture.asrFactories.Load(), fixture.policyFactories.Load(),
			fixture.modelFactories.Load(), fixture.ttsFactories.Load())
	}
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
	policyFactories atomic.Int32
	ttsFactories    atomic.Int32
	policy          *scenarioEndpointPolicy
}

func newScenarioEndpointFixture(
	t testing.TB, toolCase scenarioEndpointToolCase,
) *scenarioEndpointFixture {
	t.Helper()
	fixture := &scenarioEndpointFixture{}
	fixture.asr = &scenarioEndpointASR{toolCase: toolCase}
	fixture.model = newScenarioEndpointModel(toolCase)
	fixture.policy = &scenarioEndpointPolicy{}
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
	silentModelSelection := modelSelection
	silentModelSelection.Reference = "plugin.test.scenario-endpoint.model-silent.v1"
	silentModelSelection.Descriptor.SpeechAuthority = continuation.SpeechAuthoritySilent
	policySelection := scenarioconversation.ApplicationPolicySelection{
		Reference: "plugin.test.scenario-endpoint.policy.v1", Artifact: fixture.artifact("policy", "1"),
		Descriptor: scenarioEndpointPolicyDescriptor(),
	}
	ttsSelection := scenarioconversation.ApplicationTTSSelection{
		Reference: "plugin.test.scenario-endpoint.tts.v1", Artifact: fixture.artifact("tts", "1"),
		Descriptor: scenarioEndpointTTSDescriptor(), Voice: scenarioEndpointVoice,
	}
	gate := perception.DefaultGateConfig()
	architecture, err := projectarch.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	target := scenarioEndpointTarget()
	click := scenarioEndpointClickDefinition(t)
	fixture.application = scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		Architecture:  architecture.Identity(),
		ASR:           asrSelection, Policy: policySelection, Model: modelSelection,
		SilentModel: silentModelSelection, TTS: ttsSelection,
		Tools: []scenarioconversation.ToolDeclaration{{
			Name: toolCase.tool, Description: toolCase.description,
			Parameters:          json.RawMessage(toolCase.parameters),
			ArgumentNormalizers: slices.Clone(toolCase.normalizers),
			Confirm:             legacyaction.ConfirmNever,
		}, {
			Name: click.Name, Description: click.Description,
			Parameters: slices.Clone(click.Parameters), Confirm: legacyaction.ConfirmNever,
			Target: target.Name,
		}},
		Target: target,
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
		Policies: []scenarioconversation.PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				fixture.policyFactories.Add(1)
				return fixture.policy, nil
			},
		}},
		Models: []scenarioconversation.ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				fixture.modelFactories.Add(1)
				return fixture.model, nil
			},
		}, {
			ApplicationModelSelection: silentModelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				fixture.modelFactories.Add(1)
				return scenarioSpeechAuthorityProvider{
					Provider: fixture.model, descriptor: silentModelSelection.Descriptor,
				}, nil
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
	modelWanted, policyWanted := wanted, wanted
	if wanted > 0 {
		modelWanted = wanted * 2
		policyWanted = wanted * 2
	}
	if fixture.asrFactories.Load() != wanted || fixture.modelFactories.Load() != modelWanted ||
		fixture.policyFactories.Load() != policyWanted || fixture.ttsFactories.Load() != wanted {
		t.Fatalf("scenario endpoint factories ASR=%d policy=%d model=%d TTS=%d, want %d/%d/%d/%d",
			fixture.asrFactories.Load(), fixture.policyFactories.Load(),
			fixture.modelFactories.Load(), fixture.ttsFactories.Load(),
			wanted, policyWanted, modelWanted, wanted)
	}
}

type scenarioSpeechAuthorityProvider struct {
	continuation.Provider
	descriptor continuation.Descriptor
}

func (provider scenarioSpeechAuthorityProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

type scenarioEndpointPolicy struct {
	decisions       atomic.Int32
	silentNext      atomic.Bool
	silentDecisions atomic.Int32
}

func (*scenarioEndpointPolicy) Name() string { return "scenario-endpoint-policy" }
func (*scenarioEndpointPolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return scenarioEndpointPolicyDescriptor()
}
func (policy *scenarioEndpointPolicy) Decide(
	_ context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	policy.decisions.Add(1)
	wanted := coreinteraction.ActAnswer
	if slices.Contains(decision.Options, string(coreinteraction.ActActSilently)) &&
		policy.silentNext.CompareAndSwap(true, false) {
		wanted = coreinteraction.ActActSilently
		policy.silentDecisions.Add(1)
	}
	for index, option := range decision.Options {
		if option == string(wanted) {
			return coreinteraction.Outcome{Index: index, Option: option}, nil
		}
	}
	return coreinteraction.Outcome{}, fmt.Errorf("%s act is unavailable", wanted)
}

func scenarioEndpointPolicyDescriptor() policyelements.SemanticDeciderDescriptor {
	return policyelements.SemanticDeciderDescriptor{
		Provider: "test", Model: "scenario-endpoint-policy", Protocol: "test-enumerated", Revision: "1",
		ConfigurationDigest: "sha256:" + strings.Repeat("0", 64), DecisionTimeoutMS: 1000,
	}
}

type scenarioEndpointASR struct {
	pushes    atomic.Int32
	finalizes atomic.Int32
	toolCase  scenarioEndpointToolCase
}

func (*scenarioEndpointASR) Descriptor() v1.Descriptor { return scenarioEndpointASRDescriptor() }

func (provider *scenarioEndpointASR) PushFrame(
	_ context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	sequence := provider.pushes.Add(1)
	return []v1.PerceptionRevision{{
		RevisionID: uint64(sequence), SourceSample: frame.SampleOffset,
		StableText: provider.toolCase.stableTranscript, UnstableText: provider.toolCase.unstableTranscript,
	}}, nil
}

func (provider *scenarioEndpointASR) Finalize(
	_ context.Context, sample uint64,
) (v1.PerceptionRevision, error) {
	sequence := provider.finalizes.Add(1)
	return v1.PerceptionRevision{
		RevisionID: uint64(provider.pushes.Load()) + uint64(sequence), SourceSample: sample,
		StableText: provider.toolCase.finalTranscript, Final: true,
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
	toolCase       scenarioEndpointToolCase
	toolResults    chan trajectory.ToolResult
	media          chan scenarioEndpointMediaEvidence
	cancelStarted  chan struct{}
	cancelObserved chan struct{}
	cancelStart    sync.Once
	cancelDone     sync.Once
}

func newScenarioEndpointModel(toolCase scenarioEndpointToolCase) *scenarioEndpointModel {
	return &scenarioEndpointModel{
		toolCase:      toolCase,
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
			len(request.Invocation.Tools) != 2 || request.Invocation.Tools[0].Name != provider.toolCase.tool ||
			request.Invocation.Tools[1].Name != computeruse.Click {
			return continuation.Completion{}, errors.New("endpoint invocation settings drifted")
		}
		toolJSON, err := json.Marshal(request.Invocation.Tools[0])
		if err != nil {
			return continuation.Completion{}, err
		}
		if bytes.Contains(toolJSON, []byte("normalizer")) || bytes.Contains(toolJSON, []byte("pattern")) {
			return continuation.Completion{}, errors.New("deployment argument policy leaked to provider tool schema")
		}
		if !scenarioEndpointHasFinalAudio(request.Trajectory, provider.toolCase.finalTranscript) {
			return continuation.Completion{}, errors.New("endpoint final audio observation is missing")
		}
		call := trajectory.ToolCall{
			CallID: provider.toolCase.callID, Name: provider.toolCase.tool,
			Arguments: json.RawMessage(provider.toolCase.proposalArguments),
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
			return continuation.Completion{}, err
		}
		return continuation.Completion{StopReason: "tool_call"}, nil
	case 2:
		result, err := scenarioEndpointActionEvidence(request.Trajectory, provider.toolCase)
		if err != nil {
			return continuation.Completion{}, err
		}
		provider.toolResults <- result
		return scenarioEndpointSpeak(emit, provider.toolCase.reply)
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
	case 7:
		if err := emit(continuation.Event{
			Kind: continuation.EventAssistantDelta,
			Text: "Background reasoning that must never be synthesized.",
		}); err != nil {
			return continuation.Completion{}, err
		}
		call := trajectory.ToolCall{
			CallID: scenarioEndpointSilentCallID, Name: computeruse.Click,
			Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
			return continuation.Completion{}, err
		}
		return continuation.Completion{StopReason: "tool_call"}, nil
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

func scenarioEndpointActionEvidence(
	snapshot trajectory.Snapshot, toolCase scenarioEndpointToolCase,
) (trajectory.ToolResult, error) {
	var proposal, call *trajectory.Item
	var result *trajectory.ToolResult
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		switch {
		case item.Kind == trajectory.KindToolProposal && item.ToolCall != nil &&
			item.ToolCall.CallID == toolCase.callID:
			proposal = item
		case item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
			item.ToolCall.CallID == toolCase.callID:
			call = item
		case item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
			item.ToolResult.CallID == toolCase.callID:
			result = item.ToolResult
		}
	}
	if proposal == nil || call == nil || result == nil {
		return trajectory.ToolResult{}, errors.New("normalized canonical action lifecycle is incomplete")
	}
	if proposal.ToolCall.Name != toolCase.tool || call.ToolCall.Name != toolCase.tool ||
		result.Name != toolCase.tool {
		return trajectory.ToolResult{}, errors.New("canonical action lifecycle changed the tool identity")
	}
	if string(proposal.ToolCall.Arguments) != toolCase.proposalArguments {
		return trajectory.ToolResult{}, errors.New("canonical model proposal did not retain raw argument bytes")
	}
	if string(call.ToolCall.Arguments) != toolCase.effectiveArguments ||
		!slices.Contains(call.CausalParentIDs, proposal.ID) {
		return trajectory.ToolResult{}, errors.New("canonical action did not causally promote the effective call")
	}
	derivation := call.ToolCallDerivation
	if len(toolCase.normalizers) == 0 {
		if derivation != nil {
			return trajectory.ToolResult{}, errors.New("byte-exact canonical action unexpectedly carries a derivation")
		}
		return *result, nil
	}
	if derivation == nil || derivation.Kind != trajectory.ToolCallDerivationSchemaNormalizationV1 ||
		derivation.SourceArgumentsDigest != trajectory.ToolCallArgumentsDigest(proposal.ToolCall.Arguments) ||
		derivation.EffectiveArgumentsDigest != trajectory.ToolCallArgumentsDigest(call.ToolCall.Arguments) ||
		len(derivation.Rewrites) != len(toolCase.normalizers) {
		return trajectory.ToolResult{}, errors.New("canonical action lacks the exact auditable argument derivation")
	}
	for index, normalizer := range toolCase.normalizers {
		if derivation.Rewrites[index] != (trajectory.ToolCallArgumentRewrite{
			Argument: normalizer.Argument, Normalizer: normalizer.Normalizer,
		}) {
			return trajectory.ToolResult{}, errors.New("canonical action changed its declared argument derivation")
		}
	}
	return *result, nil
}

type scenarioEndpointTTS struct {
	plans          atomic.Int32
	mu             sync.Mutex
	texts          []string
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
	provider.mu.Lock()
	provider.texts = append(provider.texts, plan.Text)
	provider.mu.Unlock()
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

func (provider *scenarioEndpointTTS) plannedTexts() []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return slices.Clone(provider.texts)
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

func scenarioEndpointTarget() computeruse.Target {
	return computeruse.Target{
		Name: "scenario-client", Sources: []string{"screen"}, Width: 64, Height: 48,
	}
}

func scenarioEndpointClickDefinition(t testing.TB) computeruse.Definition {
	t.Helper()
	definitions, err := computeruse.DefinitionsFor(scenarioEndpointTarget())
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Name == computeruse.Click {
			return definition
		}
	}
	t.Fatal("computer-use click definition is unavailable")
	return computeruse.Definition{}
}

func scenarioEndpointSessionUpdate(
	t testing.TB, toolCase scenarioEndpointToolCase,
) map[string]any {
	t.Helper()
	click := scenarioEndpointClickDefinition(t)
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
				"type": "function", "name": toolCase.tool,
				"description": toolCase.description, "parameters": json.RawMessage(toolCase.parameters),
			}, {
				"type": "function", "name": click.Name,
				"description": click.Description, "parameters": json.RawMessage(click.Parameters),
				"openrealtime": map[string]any{"target": scenarioEndpointTarget().Name},
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
	if len(tools) != 2 {
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
