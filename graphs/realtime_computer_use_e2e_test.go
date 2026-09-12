package graphs_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

// videoAdmissionInterval is how long to wait so the next frame this test sends
// is admitted rather than rate-dropped.
//
// A source is rate-limited independently at the negotiated three frames per
// second, so the interval is about 333ms. These waits were 350ms, which is
// seventeen milliseconds of margin - enough on an idle machine and nothing on
// a loaded one, where two sends reach the gateway close enough together that
// the second is dropped exactly as designed and the test waits for evidence
// that was never coming. The same seventeen milliseconds cost the meeting
// graph handoff test the third of its three frames on CI. Twice the interval
// keeps the intent - one frame per admission window - with margin scheduling
// jitter cannot erase.
const videoAdmissionInterval = 700 * time.Millisecond

func TestRealtimeComputerUseGraphRoundTripsStableRealtimeEndpoint(t *testing.T) {
	target := computeruse.Target{
		Name: "benchmark-browser", Sources: []string{realtimecu.SourceScreen},
		Width: 320, Height: 240,
	}
	descriptor := testRealtimeCUDescriptor()
	observer := newTestRealtimeCUObserver("endpoint-audiovisual-observer")
	model := &multiStepRealtimeCUModel{
		descriptor: descriptor, invocations: make(chan int32, 8),
		secondContexts: make(chan multiStepRealtimeCUContext, 1),
		blockAt:        3, blocked: make(chan int32, 1),
	}
	var modelFactories, policyFactories, observerFactories atomic.Int32
	applicationArtifact := testRealtimeCUArtifact("endpoint-application", "1")
	providerArtifact := testRealtimeCUArtifact("endpoint-provider", "2")
	runtimeArtifact := testRealtimeCUArtifact("endpoint-runtime", "4")
	modelArtifact := testRealtimeCUArtifact("endpoint-model", "5")
	observerArtifact := testRealtimeCUArtifact("endpoint-observer", "6")
	gatewayArtifact := testRealtimeCUArtifact("endpoint-gateway", "7")
	policyArtifact := testRealtimeCUArtifact("endpoint-settlement-policy", "8")
	modelReference := "go://test/realtime-cu/endpoint-model/v1"
	policyReference := "policy://test/realtime-cu/settlement/endpoint/v1"
	observerReference := "go://test/realtime-cu/endpoint-observer/v1"
	observerSources := []string{
		realtimecu.SourceCamera, realtimecu.SourceMicrophone, realtimecu.SourceScreen,
	}
	modelFactory := func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelFactories.Add(1)
		return model, nil
	}
	observerFactory := func(
		_ context.Context, _ legacy.Options, resources realtimecu.ObserverResources,
	) (realtimecu.Observer, error) {
		observerFactories.Add(1)
		observer.retainer = resources.Retainer
		return observer, nil
	}
	policyDescriptor := testRealtimeCUPolicyDescriptor()
	policy := &cancellableRealtimeCUDispositionDecider{
		descriptor: policyDescriptor, blockAt: 2, blocked: make(chan int32, 1),
	}
	policyFactory := func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
		policyFactories.Add(1)
		return policy, nil
	}
	registration, err := graphs.RealtimeComputerUseApplicationRegistration(
		realtimecu.ApplicationRegistrationConfig{
			ApplicationArtifact: applicationArtifact,
			ProviderArtifact:    providerArtifact,
			RuntimeArtifact:     runtimeArtifact,
			Models: []realtimecu.ModelFactoryRegistration{{
				ApplicationModelSelection: realtimecu.ApplicationModelSelection{
					Reference: modelReference, Artifact: modelArtifact, Descriptor: descriptor,
				},
				Factory: modelFactory,
			}},
			Policies: []realtimecu.PolicyFactoryRegistration{{
				ApplicationPolicySelection: realtimecu.ApplicationPolicySelection{
					Reference: policyReference, Artifact: policyArtifact, Descriptor: policyDescriptor,
				},
				Factory: policyFactory,
			}},
			Observers: []realtimecu.ObserverFactoryRegistration{{
				ApplicationObserverSelection: realtimecu.ApplicationObserverSelection{
					Reference: observerReference, Name: observer.name, Artifact: observerArtifact,
					Sources: observerSources,
				},
				ResourceFactory: observerFactory,
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	applicationPayload, err := json.Marshal(realtimecu.ApplicationConfig{
		FormatVersion: realtimecu.ApplicationFormatVersion,
		Model: realtimecu.ApplicationModelSelection{
			Reference: modelReference, Artifact: modelArtifact, Descriptor: descriptor,
		},
		SettlementPolicy: realtimecu.ApplicationPolicySelection{
			Reference: policyReference, Artifact: policyArtifact, Descriptor: policyDescriptor,
		},
		Observer: realtimecu.ApplicationObserverSelection{
			Reference: observerReference, Name: observer.name, Artifact: observerArtifact,
			Sources: observerSources,
		},
		Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	previewConfig, err := registration.Factory(context.Background(), applicationPayload)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), previewConfig)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          "openrealtime.realtime-cu.endpoint-test",
		Revision:      1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: realtimecu.ApplicationReference, Artifact: applicationArtifact,
			},
			Configuration: applicationPayload,
		},
		Plan: preview.Plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference: realtimecu.AdapterReference, RuntimeArtifact: runtimeArtifact,
			ProfileName: realtimecu.ProfileName, ProfileRevision: realtimecu.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.realtime-cu-endpoint-test", ProfileRevision: 1,
			ProviderArtifact: providerArtifact, GatewayArtifact: gatewayArtifact,
			Model: "realtime-cu-endpoint-test", TranscriptionModel: "realtime-cu-observer",
			ValidateWire: true, InspectionTokenTTLMS: 30_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := serverplugin.NewProfileGraphBundle(context.Background(),
		serverplugin.ProfileGraphBundleConfig{
			Profile: profile, Applications: registry, GatewayArtifact: gatewayArtifact,
		})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.GraphPlan.Identity() != profile.Plan {
		t.Fatal("profiled endpoint composition lost its exact graph identity")
	}
	if modelFactories.Load() != 0 || policyFactories.Load() != 0 || observerFactories.Load() != 0 {
		t.Fatalf("endpoint composition acquired session resources: model=%d policy=%d observer=%d",
			modelFactories.Load(), policyFactories.Load(), observerFactories.Load())
	}
	realm, err := bundle.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if closeErr := realm.Close(ctx); closeErr != nil {
			t.Errorf("close realtime-CU endpoint realm: %v", closeErr)
		}
	})
	if modelFactories.Load() != 0 || policyFactories.Load() != 0 || observerFactories.Load() != 0 {
		t.Fatalf("endpoint mount acquired session resources: model=%d policy=%d observer=%d",
			modelFactories.Load(), policyFactories.Load(), observerFactories.Load())
	}

	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	connection, _, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+
			"/v1/realtime?model=realtime-cu-endpoint-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})
	client := &realtimeCUWireClient{
		t: t, connection: connection, validator: protocol.NewValidator(),
	}
	client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == "session.created"
	})
	if observerFactories.Load() != 1 || modelFactories.Load() != 1 || policyFactories.Load() != 1 {
		t.Fatalf("endpoint session factories after dial: model=%d policy=%d observer=%d",
			modelFactories.Load(), policyFactories.Load(), observerFactories.Load())
	}

	spec := testRealtimeCUToolSpecs(t, target)[0]
	var parameters any
	if err := json.Unmarshal(spec.Parameters, &parameters); err != nil {
		t.Fatal(err)
	}
	client.send(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime", "instructions": "click the visible control",
			"output_modalities": []string{"text"},
			"audio": map[string]any{
				"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24_000}},
			},
			"openrealtime": map[string]any{
				"version": openrealtime.Version,
				"supports": []string{
					string(openrealtime.FeatureVideoInput),
					string(openrealtime.FeatureObservations),
					string(openrealtime.FeatureComputerUse),
				},
				"observers": []string{observer.name},
			},
			"tools": []map[string]any{{
				"type": "function", "name": spec.Name,
				"description": spec.Description, "parameters": parameters,
				"openrealtime": map[string]any{
					"confirm": string(spec.Confirm), "target": spec.Target,
				},
			}},
		},
	})
	updated := client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == "session.updated"
	})
	assertRealtimeCUNegotiation(t, updated, observer.name)

	// The public cancellation event crosses the single session_cancel graph
	// boundary and the adapter does not return until the coordinator publishes
	// its terminal cancellation_outcome. There is no wire acknowledgement for
	// response.cancel itself, so the following frame is the round-trip witness:
	// the gateway reads client events serially and cannot admit it while Cancel
	// is still waiting. With no durable intent yet, the exact terminal outcome
	// is no_current_intent and the session remains usable.
	client.send(map[string]any{"type": "response.cancel"})

	frame := realtimeCUJPEG(t, 320, 240)
	preIntentTimestampMS := time.Now().UnixMilli()
	for _, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoSourceUpdate, "source": source,
			"state": "active", "width": 320, "height": 240,
		})
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": preIntentTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source && message["authority"] == "observer"
		})
	}

	client.send(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString([]byte{1, 0, 2, 0}),
	})
	transcript := client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == "conversation.item.input_audio_transcription.completed"
	})
	if transcript["transcript"] != "click the visible control" {
		t.Fatalf("endpoint microphone transcript = %+v", transcript)
	}
	// Both observer/source pairs present before the intent are frozen. Refresh
	// each after the final transcript before cognition may activate.
	time.Sleep(videoAdmissionInterval)
	freshIntentTimestampMS := time.Now().Add(time.Millisecond).UnixMilli()
	for index, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": freshIntentTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source &&
				message["timestamp_ms"] == float64(freshIntentTimestampMS)
		})
		if index == 0 {
			select {
			case invocation := <-model.invocations:
				t.Fatalf("fresh screen activated before frozen camera refreshed: %d", invocation)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	call := client.await(10*time.Second, func(message map[string]any) bool {
		return message["type"] == "response.function_call_arguments.done"
	})
	callID, _ := call["call_id"].(string)
	if callID == "" || call["name"] != computeruse.Click ||
		call["arguments"] != `{"source":"screen","x":10,"y":20}` {
		t.Fatalf("endpoint client action = %+v", call)
	}
	if modelFactories.Load() != 1 {
		t.Fatalf("endpoint model factories after cognition = %d, want one", modelFactories.Load())
	}
	if invocation := receiveRealtimeCU(t, model.invocations, "first model invocation"); invocation != 1 {
		t.Fatalf("first model invocation sequence = %d", invocation)
	}

	// Continuous camera and screen cadence updates canonical context, but the
	// first effect is still unsettled. The activation policy admits no overlap:
	// changed visual evidence can reactivate only after the model returned no
	// proposal or the exact proposed effect has a canonical visual consequence.
	time.Sleep(videoAdmissionInterval)
	pendingEffectTimestampMS := freshIntentTimestampMS + 1
	for _, source := range []string{realtimecu.SourceCamera, realtimecu.SourceScreen} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": pendingEffectTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source &&
				message["timestamp_ms"] == float64(pendingEffectTimestampMS)
		})
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("pending effect allowed overlapping audiovisual invocation %d", invocation)
	case <-time.After(250 * time.Millisecond):
	}

	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": callID, "output": `{"clicked":true}`,
		},
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		if message["type"] != "conversation.item.created" {
			return false
		}
		item, _ := message["item"].(map[string]any)
		return item["type"] == "function_call_output" && item["call_id"] == callID
	})
	// This is the exact resume event sent by the benchmark harness after each
	// function_call_output. The graph remains observation-driven and accepts it
	// without inventing a second, ungrounded activation.
	client.send(map[string]any{"type": "response.create"})

	consequence := receiveRealtimeCU(t, observer.consequences, "endpoint visual consequence")
	if consequence.CallID != callID || consequence.Name != computeruse.Click ||
		consequence.TargetSource != realtimecu.SourceScreen ||
		consequence.CanonicalResultItemID == "" || consequence.StoreVersion == 0 {
		t.Fatalf("endpoint visual consequence = %+v", consequence)
	}
	// A source is rate-limited independently at the negotiated three FPS. Wait
	// one interval so this is an admitted post-effect screen, not a dropped one.
	time.Sleep(videoAdmissionInterval)
	postEffectTimestampMS := pendingEffectTimestampMS + 1
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": realtimecu.SourceScreen,
		"frame": frame, "timestamp_ms": postEffectTimestampMS,
	})
	feedback := client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == openrealtime.EventObservationAdded &&
			message["source"] == realtimecu.SourceScreen &&
			message["timestamp_ms"] == float64(postEffectTimestampMS)
	})
	if feedback["authority"] != "observer" {
		t.Fatalf("post-effect screen feedback = %+v", feedback)
	}
	if invocation := receiveRealtimeCU(t, model.invocations, "second model invocation"); invocation != 2 {
		t.Fatalf("second model invocation sequence = %d", invocation)
	}
	secondContext := receiveRealtimeCU(t, model.secondContexts, "second-action canonical context")
	if secondContext.UserItemID == "" || secondContext.ResultItemID != consequence.CanonicalResultItemID ||
		secondContext.ScreenItemID == "" ||
		!slices.Contains(secondContext.ScreenParents, secondContext.UserItemID) ||
		!slices.Contains(secondContext.ScreenParents, secondContext.ResultItemID) {
		t.Fatalf("second-action canonical context = %+v", secondContext)
	}
	secondCall := client.await(10*time.Second, func(message map[string]any) bool {
		return message["type"] == "response.function_call_arguments.done" &&
			message["call_id"] != callID
	})
	secondCallID, _ := secondCall["call_id"].(string)
	if secondCallID == "" || secondCall["name"] != computeruse.Click ||
		secondCall["arguments"] != `{"source":"screen","x":30,"y":40}` {
		t.Fatalf("second endpoint client action = %+v", secondCall)
	}
	client.send(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": secondCallID,
			"output": `{"clicked":true,"step":2}`,
		},
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		if message["type"] != "conversation.item.created" {
			return false
		}
		item, _ := message["item"].(map[string]any)
		return item["type"] == "function_call_output" && item["call_id"] == secondCallID
	})
	client.send(map[string]any{"type": "response.create"})
	secondConsequence := receiveRealtimeCU(t, observer.consequences, "second endpoint visual consequence")
	if secondConsequence.CallID != secondCallID ||
		secondConsequence.CanonicalResultItemID == consequence.CanonicalResultItemID ||
		secondConsequence.StoreVersion <= consequence.StoreVersion {
		t.Fatalf("second endpoint visual consequence = %+v", secondConsequence)
	}

	// Present the second result-linked visual consequence and hold the real
	// disposition provider in flight. Cancellation must stop that provider and
	// wait for its exact quiescence outcome before moving on to activation,
	// model, model-commit, and all configured action stages.
	time.Sleep(videoAdmissionInterval)
	settlementTimestampMS := postEffectTimestampMS + 1
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": realtimecu.SourceScreen,
		"frame": frame, "timestamp_ms": settlementTimestampMS,
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == openrealtime.EventObservationAdded &&
			message["source"] == realtimecu.SourceScreen &&
			message["timestamp_ms"] == float64(settlementTimestampMS)
	})
	if decision := receiveRealtimeCU(t, policy.blocked, "in-flight settlement decision"); decision != 2 {
		t.Fatalf("blocked settlement decision sequence = %d, want 2", decision)
	}
	select {
	case invocation := <-model.invocations:
		t.Fatalf("unsettled second effect reactivated model invocation %d", invocation)
	case <-time.After(100 * time.Millisecond):
	}

	// The next frame can be observed only after the gateway's serial
	// response.cancel handler receives the coordinator's terminal outcome. It
	// must not reactivate the canceled intent.
	client.send(map[string]any{"type": "response.cancel"})
	time.Sleep(videoAdmissionInterval)
	afterCancelTimestampMS := settlementTimestampMS + 1
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": realtimecu.SourceScreen,
		"frame": frame, "timestamp_ms": afterCancelTimestampMS,
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == openrealtime.EventObservationAdded &&
			message["source"] == realtimecu.SourceScreen &&
			message["timestamp_ms"] == float64(afterCancelTimestampMS)
	})
	select {
	case invocation := <-model.invocations:
		t.Fatalf("post-cancel observation reactivated model invocation %d", invocation)
	case <-time.After(250 * time.Millisecond):
	}

	// Cancellation is scoped to the exact durable-intent epoch rather than the
	// whole session. A later final user transcript must establish a new intent,
	// but it still cannot reuse either source's pre-intent frame. Refreshing only
	// screen remains insufficient because the production after-intent policy
	// froze both observer/source pairs; the fresh camera frame completes the new
	// cohort and activates exactly one new model run.
	firstTranscriptItemID, _ := transcript["item_id"].(string)
	if firstTranscriptItemID == "" {
		t.Fatalf("first endpoint transcript omitted item identity: %+v", transcript)
	}
	client.send(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString([]byte{3, 0, 4, 0}),
	})
	newTranscript := client.await(5*time.Second, func(message map[string]any) bool {
		itemID, _ := message["item_id"].(string)
		return message["type"] == "conversation.item.input_audio_transcription.completed" &&
			itemID != "" && itemID != firstTranscriptItemID
	})
	if newTranscript["transcript"] != "click the visible control" {
		t.Fatalf("new-intent endpoint transcript = %+v", newTranscript)
	}

	time.Sleep(videoAdmissionInterval)
	newIntentTimestampMS := time.Now().Add(time.Millisecond).UnixMilli()
	for index, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": newIntentTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source &&
				message["timestamp_ms"] == float64(newIntentTimestampMS)
		})
		if index == 0 {
			select {
			case invocation := <-model.invocations:
				t.Fatalf("new intent reused stale camera for model invocation %d", invocation)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if invocation := receiveRealtimeCU(t, model.invocations, "post-cancel new-intent invocation"); invocation != 3 {
		t.Fatalf("post-cancel new-intent invocation sequence = %d, want 3", invocation)
	}
	if invocation := receiveRealtimeCU(t, model.blocked, "active replacement model run"); invocation != 3 {
		t.Fatalf("blocked replacement model invocation = %d, want 3", invocation)
	}

	// Exercise a second cancellation with cognition itself in flight and no
	// model result yet. The coordinator must wait for the model cancellation
	// outcome, then settle every idle action stage. A later frame is again the
	// serial gateway witness that the terminal coordinator outcome was received.
	client.send(map[string]any{"type": "response.cancel"})
	time.Sleep(videoAdmissionInterval)
	afterModelCancelTimestampMS := newIntentTimestampMS + 1
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": realtimecu.SourceScreen,
		"frame": frame, "timestamp_ms": afterModelCancelTimestampMS,
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == openrealtime.EventObservationAdded &&
			message["source"] == realtimecu.SourceScreen &&
			message["timestamp_ms"] == float64(afterModelCancelTimestampMS)
	})
	select {
	case invocation := <-model.invocations:
		t.Fatalf("post-model-cancel cadence reactivated invocation %d", invocation)
	case <-time.After(150 * time.Millisecond):
	}

	secondTranscriptItemID, _ := newTranscript["item_id"].(string)
	client.send(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString([]byte{5, 0, 6, 0}),
	})
	thirdTranscript := client.await(5*time.Second, func(message map[string]any) bool {
		itemID, _ := message["item_id"].(string)
		return message["type"] == "conversation.item.input_audio_transcription.completed" &&
			itemID != "" && itemID != firstTranscriptItemID && itemID != secondTranscriptItemID
	})
	if thirdTranscript["transcript"] != "click the visible control" {
		t.Fatalf("post-model-cancel endpoint transcript = %+v", thirdTranscript)
	}
	time.Sleep(videoAdmissionInterval)
	thirdIntentTimestampMS := time.Now().Add(time.Millisecond).UnixMilli()
	for index, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": thirdIntentTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source &&
				message["timestamp_ms"] == float64(thirdIntentTimestampMS)
		})
		if index == 0 {
			select {
			case invocation := <-model.invocations:
				t.Fatalf("third intent reused stale camera for model invocation %d", invocation)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if invocation := receiveRealtimeCU(t, model.invocations, "post-model-cancel new-intent invocation"); invocation != 4 {
		t.Fatalf("post-model-cancel new-intent invocation sequence = %d, want 4", invocation)
	}
	crossedCall := client.await(10*time.Second, func(message map[string]any) bool {
		return message["type"] == "response.function_call_arguments.done" &&
			message["call_id"] != callID && message["call_id"] != secondCallID
	})
	crossedCallID, _ := crossedCall["call_id"].(string)
	if crossedCallID == "" || crossedCall["name"] != computeruse.Click ||
		crossedCall["arguments"] != `{"source":"screen","x":50,"y":60}` {
		t.Fatalf("crossed endpoint client action = %+v", crossedCall)
	}

	// The dispatcher has crossed the external client boundary, so cancellation
	// cannot truthfully claim that no effect happened. The graph returns an
	// explicit incomplete result, while the dispatch cancellation still creates
	// the mandatory canonical error result and requests visual consequence
	// observation for whatever the client may already have changed.
	client.send(map[string]any{
		"type": "response.cancel", "event_id": "cancel_crossed_client_effect",
	})
	cancelError := client.awaitAllowError(5*time.Second, func(message map[string]any) bool {
		if message["type"] != "error" {
			return false
		}
		failure, _ := message["error"].(map[string]any)
		return failure["event_id"] == "cancel_crossed_client_effect"
	})
	failure, _ := cancelError["error"].(map[string]any)
	if failure["code"] != "invalid_request_error" ||
		!strings.Contains(fmt.Sprint(failure["message"]), "action_already_crossed") {
		t.Fatalf("crossed-action cancellation error = %+v", cancelError)
	}
	canceledConsequence := receiveRealtimeCU(t, observer.consequences, "canceled crossed-action consequence")
	if canceledConsequence.CallID != crossedCallID || canceledConsequence.Name != computeruse.Click ||
		canceledConsequence.CanonicalResultItemID == "" ||
		canceledConsequence.StoreVersion <= secondConsequence.StoreVersion {
		t.Fatalf("canceled crossed-action consequence = %+v", canceledConsequence)
	}

	time.Sleep(videoAdmissionInterval)
	afterCrossedCancelTimestampMS := thirdIntentTimestampMS + 1
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": realtimecu.SourceScreen,
		"frame": frame, "timestamp_ms": afterCrossedCancelTimestampMS,
	})
	client.await(5*time.Second, func(message map[string]any) bool {
		return message["type"] == openrealtime.EventObservationAdded &&
			message["source"] == realtimecu.SourceScreen &&
			message["timestamp_ms"] == float64(afterCrossedCancelTimestampMS)
	})
	select {
	case invocation := <-model.invocations:
		t.Fatalf("crossed-action cancellation reactivated invocation %d", invocation)
	case <-time.After(150 * time.Millisecond):
	}

	thirdTranscriptItemID, _ := thirdTranscript["item_id"].(string)
	client.send(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString([]byte{7, 0, 8, 0}),
	})
	fourthTranscript := client.await(5*time.Second, func(message map[string]any) bool {
		itemID, _ := message["item_id"].(string)
		return message["type"] == "conversation.item.input_audio_transcription.completed" &&
			itemID != "" && itemID != firstTranscriptItemID &&
			itemID != secondTranscriptItemID && itemID != thirdTranscriptItemID
	})
	if fourthTranscript["transcript"] != "click the visible control" {
		t.Fatalf("post-crossed-cancel endpoint transcript = %+v", fourthTranscript)
	}
	time.Sleep(videoAdmissionInterval)
	fourthIntentTimestampMS := time.Now().Add(time.Millisecond).UnixMilli()
	for index, source := range []string{realtimecu.SourceScreen, realtimecu.SourceCamera} {
		client.send(map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": source,
			"frame": frame, "timestamp_ms": fourthIntentTimestampMS,
		})
		client.await(5*time.Second, func(message map[string]any) bool {
			return message["type"] == openrealtime.EventObservationAdded &&
				message["source"] == source &&
				message["timestamp_ms"] == float64(fourthIntentTimestampMS)
		})
		if index == 0 {
			select {
			case invocation := <-model.invocations:
				t.Fatalf("fourth intent reused stale camera for model invocation %d", invocation)
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	if invocation := receiveRealtimeCU(t, model.invocations, "post-crossed-cancel new-intent invocation"); invocation != 5 {
		t.Fatalf("post-crossed-cancel new-intent invocation sequence = %d, want 5", invocation)
	}
}

type multiStepRealtimeCUContext struct {
	UserItemID    string
	ResultItemID  string
	ScreenItemID  string
	ScreenParents []string
}

type cancellableRealtimeCUDispositionDecider struct {
	descriptor  policyelements.SemanticDeciderDescriptor
	invocations atomic.Int32
	blockAt     int32
	blocked     chan int32
}

func (*cancellableRealtimeCUDispositionDecider) Name() string {
	return "cancellable-realtime-cu-settlement"
}

func (decider *cancellableRealtimeCUDispositionDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *cancellableRealtimeCUDispositionDecider) Decide(
	ctx context.Context, request coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	invocation := decider.invocations.Add(1)
	if invocation == decider.blockAt {
		select {
		case decider.blocked <- invocation:
		case <-ctx.Done():
			return coreinteraction.Outcome{}, context.Cause(ctx)
		}
		<-ctx.Done()
		return coreinteraction.Outcome{}, context.Cause(ctx)
	}
	wanted := string(policyelements.IntentDispositionContinue)
	index := slices.Index(request.Options, wanted)
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("settlement decision does not offer %q", wanted)
	}
	return coreinteraction.Outcome{Index: index, Option: wanted}, nil
}

type multiStepRealtimeCUModel struct {
	descriptor      continuation.Descriptor
	invocationCount atomic.Int32
	invocations     chan int32
	secondContexts  chan multiStepRealtimeCUContext
	blockAt         int32
	blocked         chan int32
}

func (model *multiStepRealtimeCUModel) Descriptor() continuation.Descriptor {
	return model.descriptor
}

func (model *multiStepRealtimeCUModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	invocation := model.invocationCount.Add(1)
	if model.invocations != nil {
		model.invocations <- invocation
	}
	if model.blockAt == invocation {
		select {
		case model.blocked <- invocation:
		case <-ctx.Done():
			return continuation.Completion{}, context.Cause(ctx)
		}
		<-ctx.Done()
		return continuation.Completion{}, context.Cause(ctx)
	}
	if len(request.Trajectory.Items) == 0 {
		return continuation.Completion{StopReason: "stop"}, nil
	}
	var userItemID, resultItemID string
	results := 0
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			userItemID = item.ID
		}
		if item.Kind == trajectory.KindToolResult {
			results++
			resultItemID = item.ID
		}
	}
	last := request.Trajectory.Items[len(request.Trajectory.Items)-1]
	arguments := json.RawMessage(`{"source":"screen","x":10,"y":20}`)
	step := 1
	switch {
	case invocation == 4:
		step = 3
		arguments = json.RawMessage(`{"source":"screen","x":50,"y":60}`)
	case results == 0 && last.Kind == trajectory.KindObservation &&
		last.Observation != nil && last.Observation.Source == realtimecu.SourceCamera:
	case results == 1 && last.Kind == trajectory.KindObservation &&
		last.Observation != nil && last.Observation.Source == realtimecu.SourceScreen:
		step = 2
		arguments = json.RawMessage(`{"source":"screen","x":30,"y":40}`)
		model.secondContexts <- multiStepRealtimeCUContext{
			UserItemID: userItemID, ResultItemID: resultItemID,
			ScreenItemID: last.ID, ScreenParents: slices.Clone(last.CausalParentIDs),
		}
	default:
		return continuation.Completion{StopReason: "stop"}, nil
	}
	call := trajectory.ToolCall{
		CallID: request.InvocationID + fmt.Sprintf(":click-%d", step),
		Name:   computeruse.Click, Arguments: arguments,
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

type realtimeCUWireClient struct {
	t          *testing.T
	connection *websocket.Conn
	validator  *protocol.Validator
	received   []map[string]any
}

func (client *realtimeCUWireClient) send(message map[string]any) {
	client.t.Helper()
	encoded, err := json.Marshal(message)
	if err != nil {
		client.t.Fatal(err)
	}
	if err := client.connection.Write(context.Background(), websocket.MessageText, encoded); err != nil {
		client.t.Fatal(err)
	}
}

func (client *realtimeCUWireClient) await(
	timeout time.Duration, matches func(map[string]any) bool,
) map[string]any {
	return client.awaitMessage(timeout, false, matches)
}

func (client *realtimeCUWireClient) awaitAllowError(
	timeout time.Duration, matches func(map[string]any) bool,
) map[string]any {
	return client.awaitMessage(timeout, true, matches)
}

func (client *realtimeCUWireClient) awaitMessage(
	timeout time.Duration, allowError bool, matches func(map[string]any) bool,
) map[string]any {
	client.t.Helper()
	for _, message := range client.received {
		if matches(message) {
			return message
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, encoded, err := client.connection.Read(ctx)
		if err != nil {
			client.t.Fatalf("read Realtime-CU endpoint: %v (events %v)", err, realtimeCUEventTypes(client.received))
		}
		var message map[string]any
		if err := json.Unmarshal(encoded, &message); err != nil {
			client.t.Fatalf("decode Realtime-CU endpoint event: %v", err)
		}
		client.received = append(client.received, message)
		eventType, _ := message["type"].(string)
		if !strings.HasPrefix(eventType, "openrealtime.") {
			decoded, err := protocol.Decode(encoded)
			if err != nil {
				client.t.Fatalf("pinned Realtime registry rejected server event %q: %v", eventType, err)
			}
			if err := client.validator.Validate(protocol.ProfileRealtime, protocol.DirectionServer, decoded); err != nil {
				client.t.Fatalf("pinned Realtime schema rejected server event %q: %v", eventType, err)
			}
		}
		if eventType == "error" && !allowError {
			client.t.Fatalf("Realtime-CU endpoint error: %+v", message["error"])
		}
		if matches(message) {
			return message
		}
	}
}

func realtimeCUEventTypes(messages []map[string]any) []string {
	types := make([]string, 0, len(messages))
	for _, message := range messages {
		value, _ := message["type"].(string)
		types = append(types, value)
	}
	return types
}

func assertRealtimeCUNegotiation(t *testing.T, event map[string]any, observer string) {
	t.Helper()
	session, ok := event["session"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated omitted session object: %+v", event)
	}
	extension, ok := session["openrealtime"].(map[string]any)
	if !ok {
		t.Fatalf("session.updated omitted OpenRealtime negotiation: %+v", session)
	}
	rawEnabled, _ := extension["enabled"].([]any)
	enabled := make([]string, 0, len(rawEnabled))
	for _, value := range rawEnabled {
		name, _ := value.(string)
		enabled = append(enabled, name)
	}
	slices.Sort(enabled)
	want := []string{
		string(openrealtime.FeatureComputerUse),
		string(openrealtime.FeatureObservations),
		string(openrealtime.FeatureVideoInput),
	}
	slices.Sort(want)
	if !slices.Equal(enabled, want) {
		t.Fatalf("Realtime-CU enabled features = %v, want %v", enabled, want)
	}
	observers, _ := extension["observers"].([]any)
	available, _ := extension["available_observers"].([]any)
	if len(observers) != 1 || observers[0] != observer ||
		len(available) != 1 || available[0] != observer {
		t.Fatalf("Realtime-CU observer negotiation = selected %v available %v", observers, available)
	}
}

func realtimeCUJPEG(t *testing.T, width, height int) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, width, height)),
		&jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}
