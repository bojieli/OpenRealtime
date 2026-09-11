package main

import (
	"context"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"strings"
	"testing"
)

func TestRoomDefaultUsesBenchmarkPipelineAndFullScenarioContract(t *testing.T) {
	selection := defaultRoomProfileOptions()
	if selection.maxOutputTokens < 1024 {
		t.Fatal("room must leave ample output capacity beyond the thinking budget")
	}
	if selection.modelName != "gemini-3.7-flash" || selection.modelEffort != "128" {
		t.Fatal("room must use the profiled low-latency model and thinking budget")
	}
	if selection.asrProvider != "deepgram" || selection.modelProvider != "google" || selection.policyProvider != "vllm" || selection.ttsProvider != "fish-audio" || selection.speakerURL == "" || selection.wordTimingsURL == "" {
		t.Fatalf("room pipeline lost a benchmark component: %+v", selection)
	}
	if selection.transcriptPolicy != "event-aware" || selection.transcriptPartialRules == "" || selection.transcriptFinalRules == "" {
		t.Fatal("room must select both streaming transcript policies")
	}
	if _, err := scenarioProfileTranscriptEvents(selection); err != nil {
		t.Fatal(err)
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
