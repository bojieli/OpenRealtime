package main

import (
	"context"
	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/interaction"
	"strings"
	"testing"
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
