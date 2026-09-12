package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/fdbv3"
	"github.com/bojieli/OpenRealtime/graph/ir"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestScenarioProfileFreezePinsLocalProductionSelection(t *testing.T) {
	const operatorEnvironment = "OPENREALTIME_TEST_SCENARIO_OPERATOR_CAPABILITY"
	t.Setenv(operatorEnvironment, "mgmt_"+strings.Repeat("A", 43))
	directory := t.TempDir()
	path := filepath.Join(directory, "scenario-profile.yaml")
	graphPath := filepath.Join(directory, "scenario.ir.json")
	valuesPath := filepath.Join(directory, "scenario.values.json")
	resolutionPath := filepath.Join(directory, "scenario.resolution.json")
	executionPath := filepath.Join(directory, "scenario.execution.json")
	var output bytes.Buffer
	if err := runLaunchProfile([]string{
		"scenario", "-out", path, "-graph-out", graphPath, "-values-out", valuesPath,
		"-resolution-out", resolutionPath, "-execution-out", executionPath,
		"-operator-capability-env", operatorEnvironment,
	}, &output); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if profile.Server.GatewayArtifact != artifacts.Gateway ||
		profile.Server.ProviderArtifact != artifacts.ScenarioProvider {
		t.Fatalf("profile executable artifacts = %+v, %+v; want %+v, %+v",
			profile.Server.GatewayArtifact, profile.Server.ProviderArtifact,
			artifacts.Gateway, artifacts.ScenarioProvider)
	}
	if profile.Server.OperatorCapabilityEnvironment != operatorEnvironment {
		t.Fatalf("profile operator capability environment = %q", profile.Server.OperatorCapabilityEnvironment)
	}
	for _, exact := range []string{
		`"architecture":{"fingerprint":"sha256:`,
		`"id":"cascade.composed-policy-direct-visual","revision":1`,
		`"reference":"provider.openrealtime.asr.sensevoice.v1"`,
		`"model":"iic/SenseVoiceSmall"`,
		`"base_url":"http://127.0.0.1:8002/v1"`,
		`"reference":"provider.openrealtime.policy.vllm.v1"`,
		`"vision":true`,
		`"guided_choice":true`,
		`"reasoning":"chat_template_kwargs"`,
		`"semantic_admission":{`,
		`"standing_extraction":true`,
		`"standing_memory":64`,
		`"reference":"provider.openrealtime.model.vllm.v1"`,
		`"model":"qwen-fast"`,
		`"base_url":"http://127.0.0.1:8000/v1"`,
		`"speech_authority":"voice"`,
		`"reference":"provider.openrealtime.tts.fish-audio.v1"`,
		`"model":"fishaudio/fish-speech-1.5"`,
		`"base_url":"http://127.0.0.1:8123/v1/tts"`,
		`"description":"Send a keypad tone on the open call."`,
		`"digit":{"type":"string"}`,
		`"max_output_tokens":128`,
		`"continuation_instruction":"Ground every response in canonical evidence already received.`,
	} {
		if !bytes.Contains(profile.Application.Configuration, []byte(exact)) {
			t.Fatalf("profile application configuration omitted %s", exact)
		}
	}
	encodedInstruction, err := json.Marshal(productionScenarioContinuationInstruction)
	if err != nil {
		t.Fatal(err)
	}
	wantInstruction := []byte(`"continuation_instruction":` + string(encodedInstruction))
	if !bytes.Contains(profile.Application.Configuration, wantInstruction) {
		t.Fatalf("profile continuation instruction is not the exact production selection: %s",
			profile.Application.Configuration)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %v, want 0600", info.Mode())
	}
	if !strings.Contains(output.String(), "executable  sha256:") {
		t.Fatalf("profile output omitted executable identity:\n%s", output.String())
	}
	graphPayload, err := os.ReadFile(graphPath)
	if err != nil {
		t.Fatal(err)
	}
	boundGraph, err := ir.Parse(graphPayload)
	if err != nil {
		t.Fatal(err)
	}
	valuesPayload, err := os.ReadFile(valuesPath)
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseJSON(valuesPath, valuesPayload)
	if err != nil {
		t.Fatal(err)
	}
	var semanticAdmission struct {
		DirectVisualInput  bool   `json:"direct_visual_input"`
		StandingExtraction bool   `json:"standing_extraction"`
		StandingMemory     int    `json:"standing_memory"`
		Rules              string `json:"rules"`
	}
	if err := json.Unmarshal(values.Nodes["semantic_admission"], &semanticAdmission); err != nil {
		t.Fatal(err)
	}
	if !semanticAdmission.DirectVisualInput || !semanticAdmission.StandingExtraction ||
		semanticAdmission.StandingMemory != 64 {
		t.Fatalf("frozen production scenario graph omitted semantic admission selection: %+v", semanticAdmission)
	}
	rebound, err := graphvalues.Bind(boundGraph, values)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Graph.Fingerprint != boundGraph.Fingerprint ||
		boundGraph.Fingerprint != profile.Plan.GraphFingerprint {
		t.Fatalf("emitted graph/values/profile disagree: rebound=%s graph=%s profile=%s",
			rebound.Graph.Fingerprint, boundGraph.Fingerprint, profile.Plan.GraphFingerprint)
	}
	resolution, err := bench.ReadExpectedResolution(resolutionPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Deployment == nil || len(resolution.Elements) != len(boundGraph.Nodes) {
		t.Fatalf("scenario expected resolution is incomplete: deployment=%v elements=%d nodes=%d",
			resolution.Deployment != nil, len(resolution.Elements), len(boundGraph.Nodes))
	}
	requirement, err := bench.ReadExecutionRequirement(executionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !requirement.Required() || requirement.Graph == nil ||
		requirement.Graph.Graph.Fingerprint != boundGraph.Fingerprint ||
		requirement.Graph.Configuration.Digest != rebound.Fingerprint {
		t.Fatalf("scenario execution requirement does not bind emitted artifacts: %+v", requirement)
	}
	for _, artifactPath := range []string{graphPath, valuesPath, resolutionPath, executionPath} {
		if info, statErr := os.Stat(artifactPath); statErr != nil {
			t.Fatal(statErr)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("scenario companion %s mode = %v, want 0600", artifactPath, info.Mode())
		}
	}
	if !strings.Contains(output.String(), "resolution  "+resolutionPath) ||
		!strings.Contains(output.String(), "execution   "+executionPath) {
		t.Fatalf("profile output omitted execution companions:\n%s", output.String())
	}
	if err := runLaunchProfile([]string{"scenario", "-out", path}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "exclusively") {
		t.Fatalf("create-only second freeze error = %v", err)
	}
}

func TestScenarioProfileFreezeRejectsInvalidContinuationInstructionBeforeOutput(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "leading whitespace", value: " leading", want: "leading or trailing whitespace"},
		{name: "oversized", value: strings.Repeat("x", 4097), want: "no larger than 4096"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
			err := runLaunchProfile([]string{
				"scenario", "-out", path, "-continuation-instruction", testCase.value,
			}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("continuation instruction error = %v, want %q", err, testCase.want)
			}
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Fatalf("invalid continuation instruction created output: %v", statErr)
			}
		})
	}
}

func TestScenarioProfileFreezePinsRepeatedDeepgramKeyterms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
	if err := runLaunchProfile([]string{
		"scenario", "-out", path,
		"-asr-provider", "deepgram",
		"-asr-model", "nova-3",
		"-asr-url", "wss://api.deepgram.com/v1/listen",
		"-asr-keyterm", "sea bass",
		"-asr-keyterm", "fennel",
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(profile.Application.Configuration,
		[]byte(`"keyterms":["sea bass","fennel"]`)) {
		t.Fatalf("frozen scenario profile omitted ordered Deepgram keyterms: %s",
			profile.Application.Configuration)
	}
}

func TestScenarioProfileFreezePinsExactWordTimingSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
	if err := runLaunchProfile([]string{
		"scenario", "-out", path,
		"-word-timings-url", "http://127.0.0.1:8127/v1/audio/transcriptions",
		"-word-timings-model", "Systran/faster-whisper-base.en",
		"-word-timings-language", "en",
		"-word-timings-interval-ms", "900",
		"-word-timings-timeout-ms", "12000",
	}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{
		`"word_timing":{"artifact":{`,
		`"reference":"provider.openrealtime.word-timing.openai-compatible.v1"`,
		`"interval_ms":900`,
		`"endpoint":"http://127.0.0.1:8127/v1/audio/transcriptions"`,
		`"model":"Systran/faster-whisper-base.en"`,
		`"language":"en"`,
		`"request_timeout_ms":12000`,
	} {
		if !bytes.Contains(profile.Application.Configuration, []byte(exact)) {
			t.Fatalf("frozen scenario profile omitted %s: %s", exact, profile.Application.Configuration)
		}
	}
}

func TestScenarioProfileFreezePinsFDBV3ToolUnion(t *testing.T) {
	root := t.TempDir()
	dataset := filepath.Join(root, "fdb-v3")
	for name, metadata := range map[string]string{
		"one": `{"id":"one","domain":"ecommerce","title":"Track","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"ABC123"}}]}`,
		"two": `{"id":"two","domain":"ecommerce","title":"Cart","difficulty":"easy","expected_tool_calls":[{"function":"add_to_cart","args":{"product_id":"K2","quantity":2}}]}`,
	} {
		directory := filepath.Join(dataset, name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("catalog-only fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := fdbv3.Load(dataset, 0)
	if err != nil {
		t.Fatal(err)
	}
	declarations, err := fdbV3ToolDeclarations(tasks)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(declarations)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{
		`"name":"add_to_cart"`,
		`"description":"MANDATORY tool to add an item to the shopping cart. Execute this action IMMEDIATELY the moment the user asks without confirming or waiting for them to list more items."`,
		`"product_id":{"description":"ID of the product","type":"string"}`,
		`"quantity":{"description":"Amount to add","type":"integer"}`,
		`"name":"track_order"`,
		`"description":"MANDATORY tool to track physical package status. Do NOT answer from memory or batch tracking requests. EXECUTE THIS TOOL IMMEDIATELY for every order ID mentioned."`,
		`"order_id":{"description":"Order identifier to track, e.g. 'BOB12'","type":"string"}`,
		`"argument_normalizers":[{"argument":"order_id","normalizer":"compact-ascii-alphanumeric-v1"}]`,
		`"required":["product_id"]`,
		`"required":["order_id"]`,
	} {
		if !bytes.Contains(payload, []byte(exact)) {
			t.Fatalf("FDB v3 tool projection omitted %s", exact)
		}
	}
	if bytes.Contains(payload, []byte(`x-openrealtime-normalizer`)) ||
		bytes.Contains(payload, []byte(`"pattern":"^[A-Za-z0-9]+$"`)) {
		t.Fatal("FDB v3 profile leaked runtime normalization policy into provider-facing JSON Schema")
	}
	if bytes.Contains(payload, []byte(`"name":"press_key"`)) {
		t.Fatal("FDB v3 profile widened its exact action surface with scenario-suite tools")
	}

	// A production profile may never be frozen from this convenient two-task
	// catalog fixture. It requires the exact pinned 100-recording release and
	// validates every metadata/audio byte before emitting any artifact.
	path := filepath.Join(root, "scenario-fdbv3-profile.yaml")
	err = runLaunchProfile([]string{
		"scenario", "-out", path, "-fdbv3-dataset", dataset,
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "released inventory differs") {
		t.Fatalf("incomplete FDB v3 production profile error = %v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("incomplete FDB v3 inventory created output: %v", statErr)
	}
}

func TestScenarioProfileFreezeRequiresPairedDistinctCompanionOutputs(t *testing.T) {
	directory := t.TempDir()
	profile := filepath.Join(directory, "scenario-profile.yaml")
	graph := filepath.Join(directory, "scenario.ir.json")
	values := filepath.Join(directory, "scenario.values.json")
	resolution := filepath.Join(directory, "scenario.resolution.json")
	execution := filepath.Join(directory, "scenario.execution.json")
	if err := runLaunchProfile([]string{
		"scenario", "-out", profile, "-graph-out", graph,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("unpaired companion error = %v", err)
	}
	if err := runLaunchProfile([]string{
		"scenario", "-out", profile, "-graph-out", graph, "-values-out", graph,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("aliased companion error = %v", err)
	}
	if err := runLaunchProfile([]string{
		"scenario", "-out", profile, "-resolution-out", resolution,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("unpaired execution companion error = %v", err)
	}
	if err := runLaunchProfile([]string{
		"scenario", "-out", profile, "-resolution-out", resolution, "-execution-out", execution,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "require -graph-out") {
		t.Fatalf("execution companions without graph artifacts error = %v", err)
	}
	if err := runLaunchProfile([]string{
		"scenario", "-out", profile, "-graph-out", graph, "-values-out", values,
		"-resolution-out", resolution, "-execution-out", resolution,
	}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("aliased execution companion error = %v", err)
	}
	if _, err := os.Lstat(profile); !os.IsNotExist(err) {
		t.Fatalf("invalid companion selection created profile: %v", err)
	}
}

func TestScenarioProfileFreezeRejectsUnsafeOutputBeforeCreation(t *testing.T) {
	if err := runLaunchProfile([]string{"scenario", "-out", "relative.yaml"}, &bytes.Buffer{}); err == nil {
		t.Fatal("relative profile output was accepted")
	}
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(linkedParent, "profile.yaml")
	if err := runLaunchProfile([]string{"scenario", "-out", path}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink-parent profile output error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(realParent, "profile.yaml")); !os.IsNotExist(err) {
		t.Fatalf("unsafe profile freeze wrote through symlink: %v", err)
	}
}

func TestScenarioProfileFreezeRejectsInvalidProviderBeforeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
	err := runLaunchProfile([]string{
		"scenario", "-out", path, "-model-provider", "missing-provider",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "inventory is missing") {
		t.Fatalf("missing-provider error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid selection created output: %v", err)
	}
}

func TestScenarioProfileFreezeRejectsUnsupportedArchitectureBeforeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
	err := runLaunchProfile([]string{
		"scenario", "-out", path, "-architecture", "cascade.text-policy@3",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not the exact composed semantic-policy controller") {
		t.Fatalf("unsupported-architecture error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid architecture created output: %v", err)
	}
}

func TestScenarioProfileFreezeRejectsDirectVisualPolicyCapabilityDriftBeforeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario-profile.yaml")
	err := runLaunchProfile([]string{
		"scenario", "-out", path, "-policy-vision=false",
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "vision-capable semantic policy") {
		t.Fatalf("direct-visual policy drift error = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("drifted direct-visual profile created output: %v", err)
	}
}
