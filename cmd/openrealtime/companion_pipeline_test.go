package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/interaction"
)

func TestRoomDefaultUsesBenchmarkPipelineAndFullScenarioContract(t *testing.T) {
	selection := defaultRoomProfileOptions()
	if selection.maxOutputTokens < 1024 {
		t.Fatal("room must leave ample output capacity beyond the thinking budget")
	}
	if selection.modelName != "gemini-3.8-flash" || selection.modelEffort != "128" {
		t.Fatal("room must use the profiled low-latency model and thinking budget")
	}
	if selection.asrProvider != "deepgram" || selection.modelProvider != "google" || selection.policyProvider != "vllm" || selection.ttsProvider != "fish-audio" || selection.speakerURL == "" || selection.wordTimingsURL == "" {
		t.Fatalf("room pipeline lost a benchmark component: %+v", selection)
	}
	if selection.transcriptRules != interaction.ChoiceInstruction {
		t.Fatal("room must pass the interaction rules explicitly so the frozen profile records them")
	}
	contract, err := graphnative.BuildContract(selection.cases...)
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.Cases) != 12 || len(scenario.Suite()) != 12 {
		t.Fatal("room no longer selects all twelve cases")
	}
}

func TestRoomRejectsHostedRealtimeBinding(t *testing.T) {
	for _, args := range [][]string{{"-binding", "upstream"}, {"--binding=upstream"}} {
		options := companionOptions{serveArguments: args}
		cleanup, _, err := prepareCompanionPipeline(context.Background(), &options)
		cleanup()
		if err == nil || !strings.Contains(err.Error(), "hosted upstream") {
			t.Fatalf("hosted realtime was accepted: %v", err)
		}
	}
}

func TestFilteredRoomProfileFreezesWithPreASRFilter(t *testing.T) {
	selection := defaultFilteredRoomProfileOptions()
	if selection.noiseFilterTimeoutMS >= int(selection.asrCadenceMS) || selection.noiseFilterTimeoutMS <= 0 {
		t.Fatal("filter deadline must precede the ASR polling cadence")
	}
	if _, _, err := freezeProductionScenarioProfile(context.Background(), selection); err != nil {
		t.Fatal(err)
	}
}

func TestTargetRoomUsesInitialReferenceWithoutUtteranceSpeakerComparison(t *testing.T) {
	selection := defaultTargetRoomProfileOptions()
	if selection.noiseFilterModel != "real-tse" || selection.noiseFilterURL == "" || selection.speakerURL != "" {
		t.Fatal("target room lost extraction or enabled per-utterance comparison")
	}
	if selection.asrProvider != "deepgram" || selection.policyProvider != "vllm" || selection.modelProvider != "google" || selection.ttsProvider != "fish-audio" {
		t.Fatal("target room changed the downstream pipeline")
	}
	if _, _, err := freezeProductionScenarioProfile(context.Background(), selection); err != nil {
		t.Fatal(err)
	}
}

// TestRoomWordTimingsUseTheWordTimingServiceNotTheRecogniser pins the room to
// the word-timing adapter's own documented endpoint and model.
//
// The room previously named :8003 with model "whisper-turbo", which is the
// local speech recogniser, not the word-timing service. That server answers
// the same route with {"text", "language"} and ignores
// timestamp_granularities[], so adapters/wordtimings returned ErrNoWordTimes
// for every non-silent utterance and every interruption boundary silently fell
// back to the proportional estimate. A non-empty URL was the only thing
// checked, and a URL pointing at the wrong service is non-empty.
func TestRoomWordTimingsUseTheWordTimingServiceNotTheRecogniser(t *testing.T) {
	selection := defaultRoomProfileOptions()
	if selection.wordTimingsURL != wordtimings.DefaultEndpoint {
		t.Fatalf("room word timings must use the word-timing service default %q, got %q",
			wordtimings.DefaultEndpoint, selection.wordTimingsURL)
	}
	if selection.wordTimingsModel != wordtimings.DefaultModel {
		t.Fatalf("room word timings must name the word-timing model %q, got %q",
			wordtimings.DefaultModel, selection.wordTimingsModel)
	}
	if strings.HasPrefix(selection.wordTimingsURL, realtimeCULocalASRURL) {
		t.Fatalf("room word timings point at the speech recogniser %q, which reports no word timestamps",
			selection.wordTimingsURL)
	}
}

func TestEveryRoomPipelineFreezes(t *testing.T) {
	pipelines := roomPipelines()
	if pipelines[0].Name != "room" {
		t.Fatalf("the default pipeline must be the room, got %q", pipelines[0].Name)
	}
	seen := map[string]bool{}
	for _, pipeline := range pipelines {
		if seen[pipeline.Name] || strings.TrimSpace(pipeline.Summary) == "" {
			t.Fatalf("pipeline %q is repeated or undescribed", pipeline.Name)
		}
		seen[pipeline.Name] = true
		profile, _, err := freezeProductionScenarioProfile(context.Background(), pipeline.options())
		if err != nil {
			t.Fatalf("%s does not freeze: %v", pipeline.Name, err)
		}
		if pipeline.Name != "room-flux" {
			continue
		}
		payload, err := launchprofile.MarshalYAML(profile)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"model: flux-general-en", "wss://api.deepgram.com/v2/listen", "deepgram-flux/flux-general-en"} {
			if !bytes.Contains(payload, []byte(want)) {
				t.Fatalf("the frozen Flux room does not record %q", want)
			}
		}
	}
}

// The Flux room is a comparison of recognisers only if the recogniser is all
// that differs.
func TestFluxRoomChangesOnlyTheRecogniser(t *testing.T) {
	room, flux := defaultRoomProfileOptions(), defaultFluxRoomProfileOptions()
	if flux.asrModel != "flux-general-en" || flux.asrURL != "wss://api.deepgram.com/v2/listen" ||
		flux.asrEndpointingMS != 0 || flux.asrLanguage != "en-US" {
		t.Fatalf("the Flux room's recogniser = %s %s %d %q", flux.asrModel, flux.asrURL, flux.asrEndpointingMS, flux.asrLanguage)
	}
	flux.name, flux.asrModel, flux.asrURL = room.name, room.asrModel, room.asrURL
	flux.asrEndpointingMS, flux.asrLanguage = room.asrEndpointingMS, room.asrLanguage
	if !reflect.DeepEqual(room, flux) {
		t.Fatal("the Flux room changed more than the recogniser")
	}
}

func TestPipelineConfigAppliesProfileSettingsOverThePipeline(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "eager.yaml")
	if err := os.WriteFile(path, []byte("asr-eager-eot-threshold: 0.5\nasr:\n  eot-threshold: 0.8\ngate-silence-ms: 400\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, label, err := roomPipelineSelection("room-flux", path)
	if err != nil {
		t.Fatal(err)
	}
	if selection.asrEagerEOTThreshold != 0.5 || selection.asrEOTThreshold != 0.8 ||
		selection.gateSilenceMS != 400 || selection.asrModel != "flux-general-en" {
		t.Fatalf("config did not apply over the pipeline: %+v", selection)
	}
	if label != "room-flux with eager.yaml" {
		t.Fatalf("label = %q", label)
	}
	if _, _, err := freezeProductionScenarioProfile(context.Background(), selection); err != nil {
		t.Fatalf("configured pipeline does not freeze: %v", err)
	}

	typo := filepath.Join(directory, "typo.yaml")
	if err := os.WriteFile(typo, []byte("asr: {modle: nova-3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := roomPipelineSelection("room", typo); err == nil || !strings.Contains(err.Error(), "not a setting") {
		t.Fatalf("a misspelt setting was accepted: %v", err)
	}
	if _, _, err := roomPipelineSelection("room-nova", ""); err == nil || !strings.Contains(err.Error(), "room-flux") {
		t.Fatalf("an unknown pipeline did not list the known ones: %v", err)
	}
	if selection, label, err := roomPipelineSelection("", ""); err != nil || label != "room" ||
		selection.asrModel != "nova-3" {
		t.Fatalf("no pipeline must select the room: %q %v", label, err)
	}
}

func TestCompanionRunsTheSelectedPipeline(t *testing.T) {
	options, err := parseCompanionOptions([]string{"-client", "none", "-pipeline", "room-flux"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	options.tokenEnvironment = "OPENREALTIME_PIPELINE_TEST_UNSET_TOKEN"
	cleanup, profiled, err := prepareCompanionPipeline(context.Background(), &options)
	defer cleanup()
	if err != nil || !profiled {
		t.Fatalf("prepare = %v, profiled %v", err, profiled)
	}
	path := options.serveArguments[len(options.serveArguments)-1]
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte("flux-general-en")) || options.pipelineLabel != "room-flux" {
		t.Fatalf("the companion did not freeze the chosen pipeline (label %q)", options.pipelineLabel)
	}

	conflicting := companionOptions{pipeline: "room-flux", serveArguments: []string{"-launch-profile", "/nonexistent.yaml"}}
	cleanup, _, err = prepareCompanionPipeline(context.Background(), &conflicting)
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("-pipeline was silently ignored beside an explicit launch profile: %v", err)
	}
}

// The described settings are a usable config: applied to the room they select
// the described pipeline's components.
func TestPipelinesCommandDescribesAUsableConfig(t *testing.T) {
	var list bytes.Buffer
	if err := runPipelines(nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, pipeline := range roomPipelines() {
		if !strings.Contains(list.String(), pipeline.Name) {
			t.Fatalf("pipelines list omits %q:\n%s", pipeline.Name, list.String())
		}
	}
	var described bytes.Buffer
	if err := runPipelines([]string{"room-flux"}, &described); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "flux.yaml")
	if err := os.WriteFile(path, described.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, _, err := roomPipelineSelection("room", path)
	if err != nil {
		t.Fatalf("described settings are not a valid config: %v\n%s", err, described.String())
	}
	flux := defaultFluxRoomProfileOptions()
	if selection.asrModel != flux.asrModel || selection.asrURL != flux.asrURL ||
		selection.asrLanguage != flux.asrLanguage || selection.asrEndpointingMS != flux.asrEndpointingMS {
		t.Fatalf("described settings did not reproduce the recogniser: %+v", selection)
	}
	if !reflect.DeepEqual(selection.asrKeyterms, flux.asrKeyterms) {
		t.Fatalf("describing a pipeline doubled its keyterms: %q", selection.asrKeyterms)
	}
	if err := runPipelines([]string{"room-nova"}, io.Discard); err == nil {
		t.Fatal("an unknown pipeline was described")
	}
}
