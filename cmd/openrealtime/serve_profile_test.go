package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
	pion "github.com/pion/webrtc/v4"
	"golang.org/x/sys/unix"
)

func TestProductionServeLaunchProfileResolvesExactInstalledApplications(t *testing.T) {
	options := serveProfileTestOptions()
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	providerInventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	host, err := newServeProfileHost(artifacts, providerInventory)
	if err != nil {
		t.Fatal(err)
	}
	profile := freezeServeProfileTestDocument(t, host)
	path := writeServeProfileTestDocument(t, profile)
	options.launchProfile = path

	composition, err := newProductionProfiledServeComposition(
		context.Background(), options, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if composition.Graph == nil || composition.Graph.GraphPlan == nil ||
		composition.Graph.ServerBundle == nil {
		t.Fatal("production launch-profile composition omitted the generic graph/server bundle")
	}
	if len(composition.Graph.Readiness) != 5 || composition.Readiness.Ready() {
		t.Fatalf("production selected readiness checks = %d ready=%t, want 5 false",
			len(composition.Graph.Readiness), composition.Readiness.Ready())
	}
	if composition.Graph.GraphPlan.Identity() != profile.Plan ||
		composition.Profile.Fingerprint != profile.Fingerprint {
		t.Fatal("production launch-profile composition changed the exact checked profile")
	}
	if composition.Host.Delegate.Reference != scenarioconversation.ApplicationReference ||
		composition.Host.ScenarioSuite.Reference != graphnative.ApplicationReference ||
		composition.Host.Applications == nil {
		t.Fatalf("production application registry = delegate %q suite %q registry=%p",
			composition.Host.Delegate.Reference, composition.Host.ScenarioSuite.Reference,
			composition.Host.Applications)
	}
	if len(composition.Host.Providers.ASR) != len(providers.ASRs()) ||
		len(composition.Host.Providers.Models) != len(providers.LLMs()) ||
		len(composition.Host.Providers.TTS) != len(providers.TTSs()) {
		t.Fatalf("production host provider registry is not the broad catalog: ASR=%d model=%d TTS=%d",
			len(composition.Host.Providers.ASR), len(composition.Host.Providers.Models),
			len(composition.Host.Providers.TTS))
	}
	assertServeProviderRegistrationIdentities(t, composition.Host.Providers)
}

func TestProductionServeProfileDescriptorsMatchLazyLiveFactories(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	asrRegistration := findServeASRRegistration(t, inventory, "qwen-asr")
	asrRaw := mustJSON(t, serveASRConfiguration{
		FormatVersion: 1, Model: "Qwen/Qwen3-ASR-0.6B", BaseURL: "http://127.0.0.1:8001",
		RequestTimeoutMS: 30_000, CadenceMS: 200,
	})
	asrDescriptor, err := asrRegistration.DescribeConfiguration(asrRaw)
	if err != nil {
		t.Fatal(err)
	}
	asr, err := asrRegistration.FactoryConfiguration(context.Background(), legacy.Options{}, asrRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asr.Descriptor(), asrDescriptor) {
		t.Fatalf("live ASR descriptor = %+v, registered %+v",
			asr.Descriptor(), asrDescriptor)
	}
	modelRegistration := findServeModelRegistration(t, inventory, "vllm")
	vision, retain := true, false
	zero := 0.0
	modelRaw := mustJSON(t, serveModelConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		Effort: "minimal", Vision: &vision, Reason: "off", RetainReasoning: &retain,
		Temperature: &zero, RequestTimeoutMS: 30_000, SpeechAuthority: "voice",
	})
	modelDescriptor, err := modelRegistration.DescribeConfiguration(modelRaw)
	if err != nil {
		t.Fatal(err)
	}
	model, err := modelRegistration.FactoryConfiguration(context.Background(), legacy.Options{}, modelRaw)
	if err != nil {
		t.Fatal(err)
	}
	if model.Descriptor() != modelDescriptor {
		t.Fatalf("live model descriptor = %+v, registered %+v",
			model.Descriptor(), modelDescriptor)
	}
	policyRegistration := inventory.Policies[0]
	guided := true
	policyRaw := mustJSON(t, servePolicyConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		RequestTimeoutMS: 2_000, GuidedChoice: &guided, Reasoning: "chat_template_kwargs",
	})
	policyDescriptor, err := policyRegistration.DescribeConfiguration(policyRaw)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policyRegistration.FactoryConfiguration(
		context.Background(), legacy.Options{}, policyRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Descriptor() != policyDescriptor {
		t.Fatalf("live semantic-policy descriptor = %+v, registered %+v",
			policy.Descriptor(), policyDescriptor)
	}
	ttsRegistration := findServeTTSRegistration(t, inventory, "openai-compatible")
	wrap := true
	ttsRaw := mustJSON(t, serveTTSConfiguration{
		FormatVersion: 1, Model: "tts-test", BaseURL: "http://127.0.0.1:8081/v1/audio/speech",
		Voice: "default", OutputSampleRateHz: 24_000, RequestTimeoutMS: 30_000,
		SentenceWrapping: &wrap, SentenceMinimumRunes: 12,
	})
	ttsDescriptor, _, err := ttsRegistration.DescribeConfiguration(ttsRaw)
	if err != nil {
		t.Fatal(err)
	}
	tts, err := ttsRegistration.FactoryConfiguration(context.Background(), legacy.Options{}, ttsRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tts.Descriptor(), ttsDescriptor) {
		t.Fatalf("live TTS descriptor = %+v, registered %+v",
			tts.Descriptor(), ttsDescriptor)
	}
}

func TestProfiledServePreflightIsLazyAndKeepsRealtimeEndpoint(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	providers, counters := serveProfileTestProviders(artifacts)
	host, err := newServeProfileHost(artifacts, providers)
	if err != nil {
		t.Fatal(err)
	}
	profile := freezeServeProfileTestDocument(t, host)
	assertServeProfileFactories(t, counters, 0)

	composition, err := newProfiledServeComposition(
		context.Background(), profile, host, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if composition.Graph.GraphPlan.Identity() != profile.Plan {
		t.Fatal("profiled serve composition did not retain the generic profile graph plan")
	}
	assertServeProfileFactories(t, counters, 0)

	realm, err := composition.Graph.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close profiled serve realm: %v", err)
		}
	})
	// Server-profile mount compiles routes and observability only. Provider
	// factories must remain unopened until a Realtime session starts.
	assertServeProfileFactories(t, counters, 0)

	httpServer := httptest.NewServer(realm.Handler())
	t.Cleanup(httpServer.Close)
	for _, path := range []string{"/", "/scenario", "/ui"} {
		assertHTTPStatusCode(t, httpServer.URL+path, http.StatusNotFound)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+
			"/v1/realtime?model=serve-profile-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})
	client := &serveProfileWireClient{t: t, connection: connection}
	client.awaitType(5*time.Second, "session.created")
	assertServeProfileFactories(t, counters, 1)

	client.send(map[string]any{
		"type": "session.update", "event_id": "profile_update",
		"session": map[string]any{
			"type": "realtime", "instructions": "Exercise the exact production profile.",
			"output_modalities": []string{"audio"},
			"audio": map[string]any{
				"input": map[string]any{
					"format":        map[string]any{"type": "audio/pcm", "rate": 24_000},
					"transcription": map[string]any{"model": "serve-profile-test-asr"},
					"turn_detection": map[string]any{
						"type": "server_vad", "threshold": 0.5,
						"prefix_padding_ms": 300, "silence_duration_ms": 500,
					},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24_000},
					"voice":  "test-voice",
				},
			},
			"tools": []map[string]any{{
				"type": "function", "name": "press_key", "description": "Press a reviewed menu key.",
				"parameters": map[string]any{
					"type": "object", "properties": map[string]any{
						"key": map[string]any{"type": "string"},
					}, "required": []string{"key"},
				},
			}},
		},
	})
	client.awaitType(5*time.Second, "session.updated")
	// Scenario-conversation owns endpointing. The unchanged endpoint must
	// still parse a manual commit and return its precise typed refusal rather
	// than silently accepting a second turn owner.
	client.send(map[string]any{"type": "input_audio_buffer.commit", "event_id": "profile_commit"})
	commitError := client.awaitTypeAllowError(5*time.Second, "error")
	commitDetail, _ := commitError["error"].(map[string]any)
	if !strings.Contains(fmt.Sprint(commitDetail["message"]), "capability not supported") {
		t.Fatalf("profiled commit refusal = %+v", commitError)
	}
	for index := 0; index < 3; index++ {
		client.send(map[string]any{
			"type": "input_audio_buffer.append", "event_id": fmt.Sprintf("profile_tone_%d", index),
			"audio": serveProfileTone(2_400),
		})
	}
	for index := 0; index < 6; index++ {
		client.send(map[string]any{
			"type": "input_audio_buffer.append", "event_id": fmt.Sprintf("profile_silence_%d", index),
			"audio": base64.StdEncoding.EncodeToString(make([]byte, 4_800)),
		})
	}
	client.awaitType(5*time.Second, "input_audio_buffer.speech_started")
	client.awaitType(5*time.Second, "input_audio_buffer.speech_stopped")
	transcript := client.awaitType(5*time.Second, "conversation.item.input_audio_transcription.completed")
	if transcript["transcript"] != "run the test tool" {
		t.Fatalf("profiled manual-commit transcript = %+v", transcript)
	}
	call := client.awaitType(10*time.Second, "response.function_call_arguments.done")
	if call["call_id"] != "serve_profile_call_1" || call["name"] != "press_key" ||
		call["arguments"] != `{"key":"Enter"}` {
		t.Fatalf("profiled tool call = %+v", call)
	}
	client.awaitResponseStatus(10*time.Second, fmt.Sprint(call["response_id"]), "completed")

	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "profile_tool_result",
		"item": map[string]any{
			"type": "function_call_output", "call_id": "serve_profile_call_1",
			"output": `{"ok":true}`,
		},
	})
	client.awaitType(5*time.Second, "conversation.item.created")
	client.send(map[string]any{"type": "response.create", "event_id": "profile_tool_resume"})
	audio := client.awaitType(10*time.Second, "response.output_audio.delta")
	if decoded, err := base64.StdEncoding.DecodeString(fmt.Sprint(audio["delta"])); err != nil || len(decoded) == 0 {
		t.Fatalf("profiled response audio = %+v, err %v", audio, err)
	}
	client.awaitResponseStatus(10*time.Second, fmt.Sprint(audio["response_id"]), "completed")

	client.send(map[string]any{
		"type": "conversation.item.create", "event_id": "profile_cancel_item",
		"item": map[string]any{
			"id": "profile_cancel_item_1", "type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": "Start cancellation."}},
		},
	})
	client.awaitType(5*time.Second, "conversation.item.created")
	client.send(map[string]any{"type": "response.create", "event_id": "profile_cancel_create"})
	awaitServeProfileSignal(t, counters.modelProvider.cancelStarted, "cancelable model invocation")
	cancelAudio := client.awaitType(10*time.Second, "response.output_audio.delta")
	cancelResponse := fmt.Sprint(cancelAudio["response_id"])
	client.send(map[string]any{"type": "response.cancel", "event_id": "profile_cancel"})
	client.awaitResponseStatus(10*time.Second, cancelResponse, "cancelled")
	awaitServeProfileSignal(t, counters.modelProvider.cancelObserved, "model cancellation")
	awaitServeProfileSignal(t, counters.ttsProvider.cancelObserved, "TTS cancellation")
}

func TestProfiledHealthWaitsForSelectedProviderCredentials(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	host, err := newServeProfileHost(artifacts, inventory)
	if err != nil {
		t.Fatal(err)
	}

	asrRegistration := findServeASRRegistration(t, inventory, "openai")
	asrRaw := mustJSON(t, serveASRConfiguration{
		FormatVersion: 1, Model: "gpt-4o-transcribe", BaseURL: "https://api.openai.com/v1",
		RequestTimeoutMS: 30_000, CadenceMS: 200,
	})
	asrDescriptor, err := asrRegistration.DescribeConfiguration(asrRaw)
	if err != nil {
		t.Fatal(err)
	}
	asrSelection := scenarioconversation.ApplicationASRSelection{
		Reference: asrRegistration.Reference, Artifact: asrRegistration.Artifact,
		Descriptor: asrDescriptor, Configuration: asrRaw,
	}
	profile := freezeServeProfileTestDocumentWithSelections(t, host,
		asrSelection, serveProfileTestModelSelection(t, inventory),
		serveProfileTestTTSSelection(t, inventory),
	)
	composition, err := newProfiledServeComposition(context.Background(), profile, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	realm, err := composition.Graph.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close realm: %v", err)
		}
	})
	server := httptest.NewServer(realm.Handler())
	t.Cleanup(server.Close)
	assertHTTPStatusCode(t, server.URL+"/healthz", http.StatusServiceUnavailable)

	t.Setenv("OPENAI_API_KEY", "")
	if err := composition.Readiness.CheckOnce(context.Background(), composition.Graph.Readiness); err == nil ||
		!strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing credential readiness error = %v", err)
	}
	assertHTTPStatusCode(t, server.URL+"/healthz", http.StatusServiceUnavailable)

	t.Setenv("OPENAI_API_KEY", "readiness-test-secret")
	if err := composition.Readiness.CheckOnce(context.Background(), composition.Graph.Readiness); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatusCode(t, server.URL+"/healthz", http.StatusOK)
}

type serveProfileWireClient struct {
	t          testing.TB
	connection *websocket.Conn
	pending    []map[string]any
}

func (client *serveProfileWireClient) send(event map[string]any) {
	client.t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		client.t.Fatal(err)
	}
	if err := client.connection.Write(context.Background(), websocket.MessageText, payload); err != nil {
		client.t.Fatal(err)
	}
}

func (client *serveProfileWireClient) awaitType(
	timeout time.Duration, wanted string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, false, func(event map[string]any) bool { return event["type"] == wanted })
}

func (client *serveProfileWireClient) awaitTypeAllowError(
	timeout time.Duration, wanted string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, true, func(event map[string]any) bool { return event["type"] == wanted })
}

func (client *serveProfileWireClient) awaitResponseStatus(
	timeout time.Duration, responseID, status string,
) map[string]any {
	client.t.Helper()
	return client.await(timeout, false, func(event map[string]any) bool {
		if event["type"] != "response.done" {
			return false
		}
		response, _ := event["response"].(map[string]any)
		return fmt.Sprint(response["id"]) == responseID && response["status"] == status
	})
}

func (client *serveProfileWireClient) await(
	timeout time.Duration, allowError bool, match func(map[string]any) bool,
) map[string]any {
	client.t.Helper()
	for index, event := range client.pending {
		if match(event) {
			client.pending = append(client.pending[:index], client.pending[index+1:]...)
			return event
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, payload, err := client.connection.Read(ctx)
		if err != nil {
			client.t.Fatalf("read profiled endpoint: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			client.t.Fatal(err)
		}
		if event["type"] == "error" && !allowError {
			client.t.Fatalf("profiled endpoint error: %+v", event)
		}
		if match(event) {
			return event
		}
		client.pending = append(client.pending, event)
	}
}

func serveProfileTone(samples int) string {
	pcm := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		value := int16(8_000)
		if index%32 >= 16 {
			value = -value
		}
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
	}
	return base64.StdEncoding.EncodeToString(pcm)
}

func awaitServeProfileSignal(t testing.TB, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestProfileProviderPreflightRejectsStaticConfigurationBeforeCredentials(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	registration := findServeModelRegistration(t, inventory, "openai")
	vision, retain := true, false
	malformed := mustJSON(t, serveModelConfiguration{
		FormatVersion: 1, Model: "gpt-test", BaseURL: "ftp://example.test/v1",
		Effort: "minimal", Vision: &vision, Reason: "off", RetainReasoning: &retain,
		RequestTimeoutMS: 30_000, SpeechAuthority: "voice",
	})
	if _, err := registration.DescribeConfiguration(malformed); err == nil ||
		!strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("static preflight error = %v", err)
	}
	if _, err := registration.FactoryConfiguration(
		context.Background(), legacy.Options{}, malformed,
	); err == nil || !strings.Contains(err.Error(), "unsupported scheme") {
		t.Fatalf("live static configuration error = %v", err)
	}
}

func TestSemanticPolicyConfigurationPinsDescriptorAndProviderOwnedCredential(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	registration := inventory.Policies[0]
	guided := true
	const credentialEnvironment = "OPENREALTIME_TEST_POLICY_TOKEN"
	raw := mustJSON(t, servePolicyConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		RequestTimeoutMS: 2_000, GuidedChoice: &guided, Reasoning: "chat_template_kwargs",
		TokenEnvironment: credentialEnvironment,
	})
	t.Setenv(credentialEnvironment, "")
	descriptor, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registration.FactoryConfiguration(
		context.Background(), legacy.Options{}, raw,
	); err == nil || !strings.Contains(err.Error(), credentialEnvironment) {
		t.Fatalf("missing provider-owned policy credential error = %v", err)
	}
	t.Setenv(credentialEnvironment, "policy-secret-value")
	provider, err := registration.FactoryConfiguration(
		context.Background(), legacy.Options{}, raw,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Descriptor() != descriptor {
		t.Fatalf("credentialed semantic-policy descriptor = %+v, want %+v",
			provider.Descriptor(), descriptor)
	}
	t.Setenv(credentialEnvironment, "rotated-policy-secret")
	rotated, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rotated != descriptor {
		t.Fatal("provider credential value leaked into the frozen semantic-policy descriptor")
	}
	driftedRaw := mustJSON(t, servePolicyConfiguration{
		FormatVersion: 1, Model: "qwen-fast-revision-2", BaseURL: "http://127.0.0.1:8000/v1",
		RequestTimeoutMS: 2_000, GuidedChoice: &guided, Reasoning: "chat_template_kwargs",
		TokenEnvironment: credentialEnvironment,
	})
	drifted, err := registration.DescribeConfiguration(driftedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if drifted.ConfigurationDigest == descriptor.ConfigurationDigest || drifted.Model == descriptor.Model {
		t.Fatalf("semantic-policy configuration drift was not sealed: old=%+v new=%+v",
			descriptor, drifted)
	}
}

func TestSemanticPolicyConfigurationRejectsUnpinnedAndAmbiguousValues(t *testing.T) {
	guided := true
	valid := servePolicyConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		RequestTimeoutMS: 2_000, GuidedChoice: &guided, Reasoning: "chat_template_kwargs",
	}
	tests := []struct {
		name   string
		mutate func(*servePolicyConfiguration)
		want   string
	}{
		{name: "guided choice omitted", mutate: func(config *servePolicyConfiguration) {
			config.GuidedChoice = nil
		}, want: "explicit boolean"},
		{name: "reasoning control unknown", mutate: func(config *servePolicyConfiguration) {
			config.Reasoning = "automatic"
		}, want: "not canonical"},
		{name: "credential environment lowercase", mutate: func(config *servePolicyConfiguration) {
			config.TokenEnvironment = "policy_token"
		}, want: "token_environment"},
		{name: "credential environment starts with digit", mutate: func(config *servePolicyConfiguration) {
			config.TokenEnvironment = "7_POLICY_TOKEN"
		}, want: "token_environment"},
		{name: "mutable endpoint omitted", mutate: func(config *servePolicyConfiguration) {
			config.BaseURL = ""
		}, want: "base_url"},
		{name: "timeout unbounded", mutate: func(config *servePolicyConfiguration) {
			config.RequestTimeoutMS = 0
		}, want: "request_timeout_ms"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, _, err := decodeServePolicyConfiguration("vllm", mustJSON(t, config)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid semantic-policy configuration error = %v, want %q", err, test.want)
			}
		})
	}
	unknown := append([]byte(nil), mustJSON(t, valid)...)
	unknown = bytes.TrimSuffix(unknown, []byte("}"))
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	if _, _, err := decodeServePolicyConfiguration("vllm", unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown semantic-policy field error = %v", err)
	}
}

func TestProfiledWebRTCPresentsTheExactGatewayTokenSnapshot(t *testing.T) {
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	inventory, _ := serveProfileTestProviders(artifacts)
	host, err := newServeProfileHost(artifacts, inventory)
	if err != nil {
		t.Fatal(err)
	}
	const tokenEnvironment = "OPENREALTIME_TEST_PROFILE_GATEWAY_TOKEN"
	t.Setenv(tokenEnvironment, "profile-token-snapshot")
	profile := freezeServeProfileTestDocumentWithToken(t, host,
		serveProfileTestASRSelection(t, inventory),
		serveProfileTestModelSelection(t, inventory),
		serveProfileTestTTSSelection(t, inventory), tokenEnvironment,
	)
	composition, err := newProfiledServeComposition(context.Background(), profile, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	if composition.GatewayToken != "profile-token-snapshot" {
		t.Fatal("profile composition did not retain the exact resolved gateway token")
	}
	realm, err := composition.Graph.ServerBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close realm: %v", err)
		}
	})
	protocolServer := httptest.NewServer(realm.Handler())
	t.Cleanup(protocolServer.Close)
	options := serveProfileTestOptions()
	options.listen = strings.TrimPrefix(protocolServer.URL, "http://")

	// Mutating the environment after graph/server composition must not change
	// the credential the transport presents to that already-composed gateway.
	t.Setenv(tokenEnvironment, "replacement-token")
	adapter, err := newWebRTCAdapter(options, composition.GatewayToken, profile.Server.Model)
	if err != nil {
		t.Fatal(err)
	}
	transportServer := httptest.NewServer(adapter.Handler())
	t.Cleanup(transportServer.Close)
	if status, body := postServeProfileWebRTCOffer(t, transportServer.URL); status != http.StatusCreated {
		t.Fatalf("profiled WebRTC offer with snapshot token = %d: %s", status, body)
	}

	wrongAdapter, err := newWebRTCAdapter(options, os.Getenv(tokenEnvironment), profile.Server.Model)
	if err != nil {
		t.Fatal(err)
	}
	wrongServer := httptest.NewServer(wrongAdapter.Handler())
	t.Cleanup(wrongServer.Close)
	if status, _ := postServeProfileWebRTCOffer(t, wrongServer.URL); status != http.StatusBadGateway {
		t.Fatalf("profiled WebRTC offer with reread token = %d, want %d", status, http.StatusBadGateway)
	}
}

func postServeProfileWebRTCOffer(t testing.TB, endpoint string) (int, string) {
	t.Helper()
	peer, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{
		MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", "profile-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.CreateDataChannel("oai-events", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := peer.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(peer)
	if err := peer.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	response, err := http.Post(endpoint+"/v1/realtime", "application/sdp",
		strings.NewReader(peer.LocalDescription().SDP))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestReadServeLaunchProfileRejectsNonRegularAndOversizedInputs(t *testing.T) {
	if _, err := readServeLaunchProfile(context.Background(), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory launch-profile error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "oversized.yaml")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maximumServeLaunchProfileBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readServeLaunchProfile(context.Background(), path); err == nil ||
		!strings.Contains(err.Error(), "expected 1..") {
		t.Fatalf("oversized launch-profile error = %v", err)
	}
}

func TestSecureServeLaunchProfileOpenRejectsAliasesAndMutation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "profile.yaml")
	if err := os.WriteFile(path, []byte("format_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secureReadServeProfileFile(context.Background(), "relative.yaml", 1024, nil); err == nil ||
		!strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path error = %v", err)
	}
	symlink := filepath.Join(directory, "profile-link.yaml")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := secureReadServeProfileFile(context.Background(), symlink, 1024, nil); err == nil {
		t.Fatal("symlink launch profile was accepted")
	}
	hardlink := filepath.Join(directory, "profile-hardlink.yaml")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := secureReadServeProfileFile(context.Background(), path, 1024, nil); err == nil ||
		!strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("hard-linked launch profile error = %v", err)
	}
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(directory, "profile.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := secureReadServeProfileFile(context.Background(), fifo, 1024, nil); err == nil ||
		!strings.Contains(err.Error(), "regular") {
		t.Fatalf("FIFO launch profile error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO rejection blocked for %v", elapsed)
	}

	if _, err := secureReadServeProfileFile(context.Background(), path, 1024, func() error {
		replacement := filepath.Join(directory, "replacement.yaml")
		if err := os.WriteFile(replacement, []byte("format_version: 2\n"), 0o600); err != nil {
			return err
		}
		return os.Rename(replacement, path)
	}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("concurrent replacement error = %v", err)
	}

	parentRoot := t.TempDir()
	parent := filepath.Join(parentRoot, "profile-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(parent, "profile.yaml")
	if err := os.WriteFile(parentPath, []byte("format_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secureReadServeProfileFile(context.Background(), parentPath, 1024, func() error {
		if err := os.Rename(parent, parent+"-old"); err != nil {
			return err
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			return err
		}
		return os.WriteFile(parentPath, []byte("format_version: 1\n"), 0o600)
	}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("concurrent parent replacement error = %v", err)
	}
}

func TestRunServeLaunchProfileFlagUsesProfilePreflightBeforeLegacyBinding(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-profile.yaml")
	var output strings.Builder
	err := runServe([]string{"-launch-profile", missing}, &output)
	if err == nil || !strings.Contains(err.Error(), "graph launch profile") {
		t.Fatalf("serve launch-profile error = %v", err)
	}
	if strings.Contains(err.Error(), "binding") || strings.Contains(err.Error(), "rollout") {
		t.Fatalf("serve launch-profile unexpectedly entered legacy binding composition: %v", err)
	}
}

func TestRunServeLaunchProfileRejectsIgnoredLegacySelections(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-profile.yaml")
	var output strings.Builder
	err := runServe([]string{
		"-launch-profile", missing,
		"-binding", "upstream",
		"-model", "silently-ignored-model",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "-binding, -model") ||
		!strings.Contains(err.Error(), "strict profile") {
		t.Fatalf("ignored launch-profile flag error = %v", err)
	}
}

func TestLaunchProfileRejectsEveryCLIProviderBehaviorKnob(t *testing.T) {
	for _, name := range []string{
		"request-timeout", "asr-provider", "asr-url", "asr-model", "asr-language",
		"language", "asr-cadence", "asr-partial-interval", "asr-endpointing",
		"fast-provider", "fast-url", "fast-model", "fast-token-env", "fast-effort",
		"fast-sees", "tts-provider", "tts-url", "tts-model", "tts-voice",
		"speak-by-sentence",
	} {
		t.Run(name, func(t *testing.T) {
			options := serveProfileTestOptions()
			options.explicit = map[string]bool{name: true}
			if err := validateServeProfileFlags(options); err == nil ||
				!strings.Contains(err.Error(), "-"+name) {
				t.Fatalf("profile behavior flag %q error = %v", name, err)
			}
		})
	}
}

func TestProductionServeLaunchProfileHonorsCancellationBeforeFileOrHostWork(t *testing.T) {
	want := errors.New("profile preflight canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(want)
	options := serveProfileTestOptions()
	options.launchProfile = filepath.Join(t.TempDir(), "must-not-be-read.yaml")
	if _, err := newProductionProfiledServeComposition(ctx, options, nil); !errors.Is(err, want) {
		t.Fatalf("canceled production profile error = %v, want %v", err, want)
	}
}

func TestSecureServeLaunchProfileReadHonorsInFlightCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte("format_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("stop profile read")
	ctx, cancel := context.WithCancelCause(context.Background())
	_, err := secureReadServeProfileFile(ctx, path, 1024, func() error {
		cancel(want)
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("in-flight profile read cancellation = %v, want %v", err, want)
	}
}

type serveProfileFactoryCounters struct {
	asr           atomic.Int32
	policy        atomic.Int32
	model         atomic.Int32
	tts           atomic.Int32
	modelProvider *serveProfileModel
	ttsProvider   *serveProfileTTS
}

type serveProfilePolicy struct {
	descriptor policyelements.SemanticDeciderDescriptor
}

func (provider serveProfilePolicy) Name() string { return "serve-profile-policy" }
func (provider serveProfilePolicy) Descriptor() policyelements.SemanticDeciderDescriptor {
	return provider.descriptor
}
func (serveProfilePolicy) Decide(
	_ context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	for index, option := range decision.Options {
		if option == string(coreinteraction.ActAnswer) {
			return coreinteraction.Outcome{Index: index, Option: option}, nil
		}
	}
	return coreinteraction.Outcome{}, errors.New("answer act is unavailable")
}

func serveProfilePolicyDescriptor() policyelements.SemanticDeciderDescriptor {
	return policyelements.SemanticDeciderDescriptor{
		Provider: "test", Model: "serve-profile-policy", Protocol: "test-enumerated", Revision: "1",
		ConfigurationDigest: "sha256:" + strings.Repeat("0", 64), DecisionTimeoutMS: 1000,
	}
}

type serveProfileSpeechAuthorityProvider struct {
	continuation.Provider
	descriptor continuation.Descriptor
}

func (provider serveProfileSpeechAuthorityProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func serveProfileTestProviders(
	artifacts serveProfileArtifacts,
) (serveScenarioProviders, *serveProfileFactoryCounters) {
	counters := &serveProfileFactoryCounters{}
	counters.modelProvider = &serveProfileModel{
		descriptor: modelDescriptorForServeProfileTest(), cancelStarted: make(chan struct{}),
		cancelObserved: make(chan struct{}),
	}
	counters.ttsProvider = &serveProfileTTS{
		descriptor: serveProfileTTSDescriptor(), cancelStarted: make(chan struct{}),
		cancelObserved: make(chan struct{}),
	}
	asrArtifact, err := serveProviderArtifact(artifacts.Gateway, "asr", "test")
	if err != nil {
		panic(err)
	}
	modelArtifact, err := serveProviderArtifact(artifacts.Gateway, "model", "test")
	if err != nil {
		panic(err)
	}
	policyArtifact, err := serveProviderArtifact(artifacts.Gateway, "policy", "test")
	if err != nil {
		panic(err)
	}
	ttsArtifact, err := serveProviderArtifact(artifacts.Gateway, "tts", "test")
	if err != nil {
		panic(err)
	}
	asrDescriptor := v1.Descriptor{
		Name: "serve-profile-test-asr", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityRevisions:      true,
			v1.CapabilityCancellation:   true,
		},
	}
	modelDescriptor := modelDescriptorForServeProfileTest()
	silentModelDescriptor := modelDescriptor
	silentModelDescriptor.SpeechAuthority = continuation.SpeechAuthoritySilent
	policyDescriptor := serveProfilePolicyDescriptor()
	ttsDescriptor := serveProfileTTSDescriptor()
	return serveScenarioProviders{
		ASR: []scenarioconversation.ASRFactoryRegistration{{
			ApplicationASRSelection: scenarioconversation.ApplicationASRSelection{
				Reference: "provider.openrealtime.asr.test.v1", Artifact: asrArtifact,
				Descriptor: asrDescriptor,
			},
			Factory: func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				counters.asr.Add(1)
				return serveProfileASR{descriptor: asrDescriptor}, nil
			},
		}},
		Policies: []scenarioconversation.PolicyFactoryRegistration{{
			ApplicationPolicySelection: scenarioconversation.ApplicationPolicySelection{
				Reference: "provider.openrealtime.policy.test.v1", Artifact: policyArtifact,
				Descriptor: policyDescriptor,
			},
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				counters.policy.Add(1)
				return serveProfilePolicy{descriptor: policyDescriptor}, nil
			},
		}},
		Models: []scenarioconversation.ModelFactoryRegistration{{
			ApplicationModelSelection: scenarioconversation.ApplicationModelSelection{
				Reference: "provider.openrealtime.model.test.v1", Artifact: modelArtifact,
				Descriptor: modelDescriptor,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				counters.model.Add(1)
				return counters.modelProvider, nil
			},
		}, {
			ApplicationModelSelection: scenarioconversation.ApplicationModelSelection{
				Reference: "provider.openrealtime.model-silent.test.v1", Artifact: modelArtifact,
				Descriptor: silentModelDescriptor,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				counters.model.Add(1)
				return serveProfileSpeechAuthorityProvider{
					Provider: counters.modelProvider, descriptor: silentModelDescriptor,
				}, nil
			},
		}},
		TTS: []scenarioconversation.TTSFactoryRegistration{{
			ApplicationTTSSelection: scenarioconversation.ApplicationTTSSelection{
				Reference: "provider.openrealtime.tts.test.v1", Artifact: ttsArtifact,
				Descriptor: ttsDescriptor, Voice: "test-voice",
			},
			Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				counters.tts.Add(1)
				return counters.ttsProvider, nil
			},
		}},
	}, counters
}

func assertServeProfileFactories(
	t testing.TB, counters *serveProfileFactoryCounters, want int32,
) {
	t.Helper()
	modelWant, policyWant := want, want
	if want > 0 {
		modelWant = 2 * want
	}
	if counters.asr.Load() != want || counters.policy.Load() != policyWant || counters.model.Load() != modelWant ||
		counters.tts.Load() != want {
		t.Fatalf("profile provider factories ASR=%d policy=%d model=%d TTS=%d, want %d/%d/%d/%d",
			counters.asr.Load(), counters.policy.Load(), counters.model.Load(), counters.tts.Load(),
			want, policyWant, modelWant, want)
	}
}

func assertServeProviderRegistrationIdentities(
	t testing.TB, inventory serveScenarioProviders,
) {
	t.Helper()
	references := map[string]struct{}{}
	artifacts := map[string]inspect.ArtifactIdentity{}
	check := func(reference string, artifact inspect.ArtifactIdentity) {
		if _, duplicate := references[reference]; duplicate {
			t.Fatalf("duplicate production provider reference %q", reference)
		}
		if existing, duplicate := artifacts[artifact.ID]; duplicate && existing != artifact {
			t.Fatalf("production provider artifact ID %q has multiple identities", artifact.ID)
		}
		if err := artifact.Validate(); err != nil {
			t.Fatal(err)
		}
		references[reference] = struct{}{}
		artifacts[artifact.ID] = artifact
	}
	for _, registration := range inventory.ASR {
		check(registration.Reference, registration.Artifact)
	}
	for _, registration := range inventory.Policies {
		check(registration.Reference, registration.Artifact)
	}
	for _, registration := range inventory.Models {
		check(registration.Reference, registration.Artifact)
	}
	for _, registration := range inventory.TTS {
		check(registration.Reference, registration.Artifact)
	}
}

func serveProfileTestASRSelection(
	t testing.TB, inventory serveScenarioProviders,
) scenarioconversation.ApplicationASRSelection {
	t.Helper()
	if len(inventory.ASR) == 0 {
		t.Fatal("empty test ASR inventory")
	}
	if inventory.ASR[0].DescribeConfiguration == nil {
		return inventory.ASR[0].ApplicationASRSelection
	}
	registration := findServeASRRegistration(t, inventory, "qwen-asr")
	raw := mustJSON(t, serveASRConfiguration{
		FormatVersion: 1, Model: "Qwen/Qwen3-ASR-0.6B",
		BaseURL: "http://127.0.0.1:8001", RequestTimeoutMS: 30_000, CadenceMS: 200,
	})
	descriptor, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioconversation.ApplicationASRSelection{
		Reference: registration.Reference, Artifact: registration.Artifact,
		Descriptor: descriptor, Configuration: raw,
	}
}

func serveProfileTestModelSelection(
	t testing.TB, inventory serveScenarioProviders,
) scenarioconversation.ApplicationModelSelection {
	return serveProfileTestModelSelectionWithAuthority(t, inventory, continuation.SpeechAuthorityVoice)
}

func serveProfileTestModelSelectionWithAuthority(
	t testing.TB, inventory serveScenarioProviders, authority continuation.SpeechAuthority,
) scenarioconversation.ApplicationModelSelection {
	t.Helper()
	if len(inventory.Models) == 0 {
		t.Fatal("empty test model inventory")
	}
	if inventory.Models[0].DescribeConfiguration == nil {
		index := 0
		if authority == continuation.SpeechAuthoritySilent {
			index = 1
		}
		return inventory.Models[index].ApplicationModelSelection
	}
	registration := findServeModelRegistration(t, inventory, "vllm")
	vision, retain, zero := true, false, 0.0
	raw := mustJSON(t, serveModelConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		Effort: "minimal", Vision: &vision, Reason: "off", RetainReasoning: &retain,
		Temperature: &zero, RequestTimeoutMS: 30_000, SpeechAuthority: string(authority),
	})
	descriptor, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioconversation.ApplicationModelSelection{
		Reference: registration.Reference, Artifact: registration.Artifact,
		Descriptor: descriptor, Configuration: raw,
	}
}

func serveProfileTestPolicySelection(
	t testing.TB, inventory serveScenarioProviders,
) scenarioconversation.ApplicationPolicySelection {
	t.Helper()
	if len(inventory.Policies) == 0 {
		t.Fatal("empty test semantic-policy inventory")
	}
	if inventory.Policies[0].DescribeConfiguration == nil {
		return inventory.Policies[0].ApplicationPolicySelection
	}
	registration := inventory.Policies[0]
	guided := true
	raw := mustJSON(t, servePolicyConfiguration{
		FormatVersion: 1, Model: "qwen-fast", BaseURL: "http://127.0.0.1:8000/v1",
		RequestTimeoutMS: 2_000, GuidedChoice: &guided, Reasoning: "chat_template_kwargs",
	})
	descriptor, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioconversation.ApplicationPolicySelection{
		Reference: registration.Reference, Artifact: registration.Artifact,
		Descriptor: descriptor, Configuration: raw,
	}
}

func serveProfileTestTTSSelection(
	t testing.TB, inventory serveScenarioProviders,
) scenarioconversation.ApplicationTTSSelection {
	t.Helper()
	if len(inventory.TTS) == 0 {
		t.Fatal("empty test TTS inventory")
	}
	if inventory.TTS[0].DescribeConfiguration == nil {
		return inventory.TTS[0].ApplicationTTSSelection
	}
	registration := findServeTTSRegistration(t, inventory, "openai-compatible")
	wrap := true
	raw := mustJSON(t, serveTTSConfiguration{
		FormatVersion: 1, Model: "tts-test",
		BaseURL: "http://127.0.0.1:8081/v1/audio/speech", Voice: "default",
		OutputSampleRateHz: 24_000, RequestTimeoutMS: 30_000,
		SentenceWrapping: &wrap, SentenceMinimumRunes: 12,
	})
	descriptor, voice, err := registration.DescribeConfiguration(raw)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioconversation.ApplicationTTSSelection{
		Reference: registration.Reference, Artifact: registration.Artifact,
		Descriptor: descriptor, Voice: voice, Configuration: raw,
	}
}

func findServeASRRegistration(
	t testing.TB, inventory serveScenarioProviders, provider string,
) scenarioconversation.ASRFactoryRegistration {
	t.Helper()
	want := serveProviderReference("asr", provider)
	for _, registration := range inventory.ASR {
		if registration.Reference == want {
			return registration
		}
	}
	t.Fatalf("ASR inventory is missing %q", want)
	return scenarioconversation.ASRFactoryRegistration{}
}

func findServeModelRegistration(
	t testing.TB, inventory serveScenarioProviders, provider string,
) scenarioconversation.ModelFactoryRegistration {
	t.Helper()
	want := serveProviderReference("model", provider)
	for _, registration := range inventory.Models {
		if registration.Reference == want {
			return registration
		}
	}
	t.Fatalf("model inventory is missing %q", want)
	return scenarioconversation.ModelFactoryRegistration{}
}

func findServeTTSRegistration(
	t testing.TB, inventory serveScenarioProviders, provider string,
) scenarioconversation.TTSFactoryRegistration {
	t.Helper()
	want := serveProviderReference("tts", provider)
	for _, registration := range inventory.TTS {
		if registration.Reference == want {
			return registration
		}
	}
	t.Fatalf("TTS inventory is missing %q", want)
	return scenarioconversation.TTSFactoryRegistration{}
}

func mustJSON(t testing.TB, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func freezeServeProfileTestDocument(
	t testing.TB, host serveProfileHost,
) launchprofile.Document {
	t.Helper()
	return freezeServeProfileTestDocumentWithSelections(t, host,
		serveProfileTestASRSelection(t, host.Providers),
		serveProfileTestModelSelection(t, host.Providers),
		serveProfileTestTTSSelection(t, host.Providers),
	)
}

func freezeServeProfileTestDocumentWithSelections(
	t testing.TB,
	host serveProfileHost,
	asr scenarioconversation.ApplicationASRSelection,
	model scenarioconversation.ApplicationModelSelection,
	tts scenarioconversation.ApplicationTTSSelection,
) launchprofile.Document {
	return freezeServeProfileTestDocumentWithToken(
		t, host, asr, model, tts, "",
	)
}

func freezeServeProfileTestDocumentWithToken(
	t testing.TB,
	host serveProfileHost,
	asr scenarioconversation.ApplicationASRSelection,
	model scenarioconversation.ApplicationModelSelection,
	tts scenarioconversation.ApplicationTTSSelection,
	tokenEnvironment string,
) launchprofile.Document {
	t.Helper()
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	architecture, err := projectarch.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	application := scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		Architecture:  architecture.Identity(),
		ASR:           asr, Policy: serveProfileTestPolicySelection(t, host.Providers), Model: model,
		SilentModel: serveProfileTestModelSelectionWithAuthority(
			t, host.Providers, continuation.SpeechAuthoritySilent,
		), TTS: tts,
		Tools: []scenarioconversation.ToolDeclaration{{
			Name: "press_key", Description: "Press a reviewed menu key.",
			Parameters: json.RawMessage(
				`{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}`,
			),
			Confirm: legacyaction.ConfirmNever,
		}},
		Target: computeruse.Target{
			Name: "scenario-client", Sources: []string{scenarioconversation.SourceMessage},
			Width: 64, Height: 48,
		},
		Gate: scenarioconversation.ApplicationGateSelection{
			Threshold: 0.5, PrefixPaddingMS: 300,
			SilenceDurationMS: 500, SpeechDurationMS: 120,
		},
		Media: scenarioconversation.MediaLimits{
			MaxItems: 8, MaxBytes: 4 << 20, MaxItemBytes: 2 << 20,
			MaxPending: 8, MaxActiveLeases: 16,
		},
		MaxOutputTokens: 4096,
	}
	delegateConfiguration, err := json.Marshal(application)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, host.Delegate, delegateConfiguration,
	)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := graphnative.FreezeLaunchProfile(
		context.Background(), graphnative.LaunchProfileConfig{
			Name: "openrealtime.launch.serve-profile-test", Revision: 1,
			Contract: contract, Application: host.ScenarioSuite,
			Configuration: configuration,
			Server: launchprofile.Server{
				ProfileName: "openrealtime.server.serve-profile-test", ProfileRevision: 1,
				ProviderArtifact:   host.Artifacts.ScenarioProvider,
				GatewayArtifact:    host.Artifacts.Gateway,
				TokenEnvironment:   tokenEnvironment,
				Model:              "serve-profile-test",
				TranscriptionModel: "serve-profile-test-asr",
				ValidateWire:       true, InspectionTokenTTLMS: 30_000,
				MaxAudioFrameBytes: 1 << 20, VideoLimits: openrealtime.DefaultLimits(),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func assertHTTPStatusCode(t testing.TB, endpoint string, want int) {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", endpoint, response.StatusCode, want)
	}
}

func writeServeProfileTestDocument(
	t testing.TB, profile launchprofile.Document,
) string {
	t.Helper()
	payload, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "launch-profile.yaml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serveProfileTestOptions() serveOptions {
	return serveOptions{
		requestTimeout: 2 * time.Minute,
		asrProvider:    "qwen-asr", asrModel: "Qwen/Qwen3-ASR-1.7B",
		asrCadence: 200 * time.Millisecond, asrEndpointing: 300 * time.Millisecond,
		fastProvider: "vllm", fastModel: "qwen-fast",
		fastEffort: string(continuation.EffortMinimal), fastVision: true,
		ttsProvider: "openai-compatible", ttsModel: "fishaudio/s2-pro",
		ttsVoice: "default",
		explicit: map[string]bool{},
	}
}

type serveProfileASR struct{ descriptor v1.Descriptor }

func (provider serveProfileASR) Descriptor() v1.Descriptor { return provider.descriptor }
func (serveProfileASR) PushFrame(
	context.Context, v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (serveProfileASR) Finalize(
	context.Context, uint64,
) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{
		RevisionID: 1, StableText: "run the test tool", Final: true,
	}, nil
}

type serveProfileModel struct {
	descriptor     continuation.Descriptor
	invocations    atomic.Int32
	cancelStarted  chan struct{}
	cancelObserved chan struct{}
	cancelStart    sync.Once
	cancelDone     sync.Once
}

func (provider *serveProfileModel) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (provider *serveProfileModel) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	switch provider.invocations.Add(1) {
	case 1:
		call := trajectory.ToolCall{
			CallID: "serve_profile_call_1", Name: "press_key",
			Arguments: json.RawMessage(`{"key":"Enter"}`),
		}
		if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &call}); err != nil {
			return continuation.Completion{}, err
		}
		return continuation.Completion{StopReason: "tool_call"}, nil
	case 2:
		for _, item := range request.Trajectory.Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
				item.ToolResult.CallID == "serve_profile_call_1" {
				if err := emit(continuation.Event{
					Kind: continuation.EventAssistantDelta,
					Text: "The tool completed successfully.",
				}); err != nil {
					return continuation.Completion{}, err
				}
				return continuation.Completion{StopReason: "stop"}, nil
			}
		}
		return continuation.Completion{}, errors.New("tool result is missing")
	case 3:
		if err := emit(continuation.Event{
			Kind: continuation.EventAssistantDelta,
			Text: "Cancellation audio starts. Pending response remains",
		}); err != nil {
			return continuation.Completion{}, err
		}
		provider.cancelStart.Do(func() {
			if provider.cancelStarted != nil {
				close(provider.cancelStarted)
			}
		})
		<-ctx.Done()
		provider.cancelDone.Do(func() {
			if provider.cancelObserved != nil {
				close(provider.cancelObserved)
			}
		})
		return continuation.Completion{}, context.Cause(ctx)
	default:
		return continuation.Completion{}, errors.New("unexpected serve profile model invocation")
	}
}

func modelDescriptorForServeProfileTest() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "serve-profile-test", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortLow, Streaming: true, Vision: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

type serveProfileTTS struct {
	descriptor     v1.Descriptor
	cancelStarted  chan struct{}
	cancelObserved chan struct{}
	cancelStart    sync.Once
	cancelDone     sync.Once
}

func (provider *serveProfileTTS) Descriptor() v1.Descriptor { return provider.descriptor }
func (provider *serveProfileTTS) Synthesize(
	ctx context.Context, plan v1.SpeechPlan,
) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := provider.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}
func (provider *serveProfileTTS) Stream(
	ctx context.Context, plan v1.SpeechPlan, emit func(v1.SpeechChunk) error,
) error {
	pcm := make([]byte, 4_800)
	for index := 0; index < len(pcm)/2; index++ {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(4_000))
	}
	chunk := v1.SpeechChunk{
		ChunkID: plan.CandidateID + ":audio:1", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: pcm, Final: true,
	}
	if strings.Contains(plan.Text, "Cancellation audio starts") {
		chunk.Final = false
		if err := emit(chunk); err != nil {
			return err
		}
		provider.cancelStart.Do(func() {
			if provider.cancelStarted != nil {
				close(provider.cancelStarted)
			}
		})
		<-ctx.Done()
		provider.cancelDone.Do(func() {
			if provider.cancelObserved != nil {
				close(provider.cancelObserved)
			}
		})
		return context.Cause(ctx)
	}
	return emit(chunk)
}

func serveProfileTTSDescriptor() v1.Descriptor {
	return v1.Descriptor{
		Name: "serve-profile-test-tts", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
			v1.CapabilityCancellation: true,
		},
	}
}
