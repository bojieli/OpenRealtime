package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/ir"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

func TestScenarioProfileFreezePinsLocalProductionSelection(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "scenario-profile.yaml")
	graphPath := filepath.Join(directory, "scenario.ir.json")
	valuesPath := filepath.Join(directory, "scenario.values.json")
	var output bytes.Buffer
	if err := runLaunchProfile([]string{
		"scenario", "-out", path, "-graph-out", graphPath, "-values-out", valuesPath,
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
		`"verify_voice_activation":true`,
		`"minimum_activation_confidence":0.7`,
		`"standing_memory":64`,
		`"reference":"provider.openrealtime.model.vllm.v1"`,
		`"model":"qwen-fast"`,
		`"base_url":"http://127.0.0.1:8000/v1"`,
		`"speech_authority":"voice"`,
		`"speech_authority":"silent"`,
		`"reference":"provider.openrealtime.tts.fish-audio.v1"`,
		`"model":"fishaudio/fish-speech-1.5"`,
		`"base_url":"http://127.0.0.1:8123/v1/tts"`,
		`"description":"Send a keypad tone on the open call."`,
		`"digit":{"type":"string"}`,
		`"max_output_tokens":512`,
	} {
		if !bytes.Contains(profile.Application.Configuration, []byte(exact)) {
			t.Fatalf("profile application configuration omitted %s", exact)
		}
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
		DirectVisualInput           bool    `json:"direct_visual_input"`
		StandingExtraction          bool    `json:"standing_extraction"`
		VerifyVoiceActivation       bool    `json:"verify_voice_activation"`
		MinimumActivationConfidence float64 `json:"minimum_activation_confidence"`
		StandingMemory              int     `json:"standing_memory"`
	}
	if err := json.Unmarshal(values.Nodes["semantic_admission"], &semanticAdmission); err != nil {
		t.Fatal(err)
	}
	if !semanticAdmission.DirectVisualInput || !semanticAdmission.StandingExtraction ||
		!semanticAdmission.VerifyVoiceActivation ||
		semanticAdmission.MinimumActivationConfidence != 0.7 || semanticAdmission.StandingMemory != 64 {
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
	if err := runLaunchProfile([]string{"scenario", "-out", path}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "exclusively") {
		t.Fatalf("create-only second freeze error = %v", err)
	}
}

func TestScenarioProfileFreezeRequiresPairedDistinctCompanionOutputs(t *testing.T) {
	directory := t.TempDir()
	profile := filepath.Join(directory, "scenario-profile.yaml")
	graph := filepath.Join(directory, "scenario.ir.json")
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
