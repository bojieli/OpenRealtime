package m3

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

func TestDuplexScenariosPreserveHistoryAndPolicySemantics(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	manifest, err := reference.LoadManifest(filepath.Join(root, "tests", "fixtures", "m1-reference-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), Config{
		FixturePath: filepath.Join(root, "tests", "fixtures", "m0-tone.wav"),
		Manifest:    manifest, Trials: 5, Seed: 20260817, FrameMS: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Scenarios) != 4 {
		t.Fatalf("unexpected scenarios: %d", len(report.Scenarios))
	}
	for _, condition := range report.Scenarios {
		if condition.FalseStopCount != 0 || condition.FailureToStopCount != 0 || condition.HistoryViolationCount != 0 {
			t.Fatalf("scenario %s failed safety metrics: %+v", condition.Scenario, condition)
		}
		for _, trial := range condition.Trials {
			if len(trial.Trace) == 0 || !trial.PlayedHistoryPreserved {
				t.Fatalf("scenario %s has incomplete evidence: %+v", condition.Scenario, trial)
			}
		}
		types := make(map[openaiwire.EventType]bool)
		for _, record := range condition.Trials[0].Trace {
			message, err := openaiwire.Decode(record.Message)
			if err != nil {
				t.Fatal(err)
			}
			types[message.Type()] = true
		}
		switch condition.Scenario {
		case ScenarioDirectedInterruption:
			if condition.StopLatency == nil || condition.StopLatency.MinNS < 12_000_000 || condition.StopLatency.MaxNS > 28_000_000 {
				t.Fatalf("unexpected directed stop latency: %+v", condition.StopLatency)
			}
			for _, required := range []openaiwire.EventType{
				openaiwire.EventInputAudioBufferAppend, openaiwire.EventResponseCancel,
				openaiwire.EventOutputAudioBufferClear, openaiwire.EventConversationItemTruncate,
			} {
				if !types[required] {
					t.Fatalf("directed trace is missing %s", required)
				}
			}
		case ScenarioListenerBackchannel, ScenarioSideSpeech:
			if condition.StopLatency != nil {
				t.Fatalf("non-interruption scenario %s stopped", condition.Scenario)
			}
			if types[openaiwire.EventResponseCancel] || !types[openaiwire.EventInputAudioBufferAppend] {
				t.Fatalf("scenario %s has incorrect duplex events", condition.Scenario)
			}
		case ScenarioInvalidationRepair:
			if condition.RepairCount != 5 {
				t.Fatalf("invalidation repairs = %d, want 5", condition.RepairCount)
			}
		}
	}
}

func TestReferenceRunIsDeterministic(t *testing.T) {
	t.Parallel()
	for scenario := uint64(0); scenario < 4; scenario++ {
		if sampleStopLatency(9, scenario) != sampleStopLatency(9, scenario) {
			t.Fatalf("scenario %d stop latency changed", scenario)
		}
	}
}
