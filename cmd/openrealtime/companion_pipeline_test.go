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
		t.Fatal("room must leave output capacity beyond the 512-token thinking budget")
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
