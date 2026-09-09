package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// Exercises multiple spoken requests in the same session: a greeting alone
// cannot establish that the room releases the floor for subsequent questions.
func TestLiveRoomRepeatedAudioQuestions(t *testing.T) {
	item := scenario.Scenario{Name: "room repeated audio questions", Instructions: "You are a helpful customer service agent. Answer each question directly in one short sentence.", TrailingMS: 12000,
		Script: []scenario.Line{
			{Speaker: "user", Text: "Hello, can you help me?"},
			{Speaker: "user", AtMS: 16000, Text: "What is the capital of France?"},
			{Speaker: "user", AtMS: 32000, Text: "And what is the capital of Japan?"},
		},
		Checks: []scenario.Check{
			{Kind: scenario.CheckAnsweredWithin, Line: 0, AfterMS: 8000},
			{Kind: scenario.CheckAnsweredWithin, Line: 1, AfterMS: 8000},
			{Kind: scenario.CheckSaid, Line: 1, AfterMS: 10000, Any: []string{"Paris"}},
			{Kind: scenario.CheckAnsweredWithin, Line: 2, AfterMS: 8000},
			{Kind: scenario.CheckSaid, Line: 2, AfterMS: 10000, Any: []string{"Tokyo"}},
		}}
	runLiveRoomDiagnostic(t, item)
}

func TestLiveRoomInterruptAndFollowup(t *testing.T) {
	runLiveRoomDiagnostic(t, scenario.Scenario{
		Name:         "room interruption and followup",
		Instructions: "You are a customer service agent helping troubleshoot home internet. Explain clearly. If the user asks you to stop, stop immediately and remain silent until another question.",
		Script: []scenario.Line{
			{Speaker: "user", Text: "Please explain in detail how to troubleshoot a slow home internet connection, step by step."},
			{Speaker: "user", AtMS: 10000, Text: "Stop talking now. Please wait silently.", AfterSpeech: &scenario.SpeechWindow{LatestMS: 18000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}},
			{Speaker: "user", AtMS: 28000, Text: "What does restarting my router do? Answer in one sentence."},
		}, TrailingMS: 12000,
		Checks: []scenario.Check{
			{Kind: scenario.CheckSilent, Line: 1, FromMS: 1500, AfterMS: 5000, Note: "stop speech after an audio-triggered interruption"},
			{Kind: scenario.CheckAnsweredWithin, Line: 2, AfterMS: 8000, Note: "a new question after cancellation must get a new answer"},
			{Kind: scenario.CheckSaid, Line: 2, AfterMS: 10000, Any: []string{"connection", "connections", "network", "memory"}},
		},
	})
}

func runLiveRoomDiagnostic(t *testing.T, item scenario.Scenario) {
	t.Helper()
	scenarioEvidence := map[string]any{"name": item.Name, "instructions": item.Instructions, "script": item.Script, "checks": item.Checks}
	if _, err := json.Marshal(scenarioEvidence); err != nil {
		t.Fatal(err)
	}
	endpoint := os.Getenv("OPENREALTIME_ROOM_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OPENREALTIME_ROOM_TEST_ENDPOINT")
	}
	voice := scenario.SpeechVoice{Endpoint: "http://127.0.0.1:8123/v1/audio/speech", Model: "fishaudio/fish-speech-1.5", Default: "default"}
	previous := scenario.CacheDir
	scenario.CacheDir, _ = filepath.Abs("../../.runtime/speech-cache")
	defer func() { scenario.CacheDir = previous }()
	var capture bench.SessionAudioCapture
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := scenario.Play(ctx, voice, bench.SessionConfig{Endpoint: endpoint, Token: os.Getenv("OPENREALTIME_TOKEN"), CaptureRuntimeEvidence: true, CaptureAudio: func(value bench.SessionAudioCapture) error { capture = value; return nil }}, item)
	if directory := os.Getenv("OPENREALTIME_ROOM_REVIEW_DIR"); directory != "" {
		run, e := scenario.NewReviewRun(scenario.ReviewOptions{Directory: directory, Repeats: 1, Scenarios: []scenario.Scenario{item}})
		if e != nil {
			t.Fatal(e)
		}
		if e = run.Record(item.Name, 1, capture, result, err); e != nil {
			t.Error(e)
		}
		if e = run.Close(); e != nil {
			t.Error(e)
		}

	}
	var interruptionMeasurements []map[string]float64
	for _, cue := range result.Transcript.SpeechCues {
		if item.Name != "room interruption and followup" {
			continue
		}
		if cue.Status != "sent" {
			continue
		}
		last := float64(cue.StartMS)
		for at := cue.StartMS; at < int(cue.EndMS)+5000; at += 20 {
			active, _ := bench.SpeechActivity(capture.Agent, at+20, 20, 20)
			if active > 0 {
				last = float64(at + 20)
			}
		}
		interruptionMeasurements = append(interruptionMeasurements, map[string]float64{
			"cessation_from_input_onset_ms": last - float64(cue.StartMS),
			"cessation_from_input_end_ms":   last - cue.EndMS,
		})
		t.Logf("interruption acoustic tail from injected speech onset: %.0f ms; from utterance end: %.0f ms (20ms RMS windows, threshold 128 PCM16)", last-float64(cue.StartMS), last-cue.EndMS)
	}
	if directory := os.Getenv("OPENREALTIME_ROOM_REVIEW_DIR"); directory != "" {
		evidence, e := json.MarshalIndent(map[string]any{"scenario": scenarioEvidence, "result": result, "interruption_measurements": interruptionMeasurements}, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(directory, "result.json"), evidence, 0600); e != nil {
			t.Fatal(e)
		}
	}
	payload, _ := json.Marshal(result)
	t.Logf("diagnostic evidence: %s", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("room diagnostic failed: %v", result.Failures)
	}
}

func TestLiveRoomObservedBackchannels(t *testing.T) {
	item := scenario.Diagnostics()[0]
	// Retain enough audio for the complete five-step explanation.
	item.TrailingMS = 20000
	runLiveRoomDiagnostic(t, item)
}

func TestLiveRoomQuestionDuringSpeech(t *testing.T) {
	runLiveRoomDiagnostic(t, scenario.Scenario{
		Name:         "room question during speech",
		Instructions: "You are a customer service agent. Answer questions directly. When interrupted by a new question, stop the old answer and answer the new question.",
		Script: []scenario.Line{
			{Speaker: "user", Text: "Explain in detail how to troubleshoot slow internet at home, including all the steps."},
			{Speaker: "user", AtMS: 10000, Text: "Actually, what is the capital of Japan? Answer in one sentence.", AfterSpeech: &scenario.SpeechWindow{LatestMS: 20000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}},
		}, TrailingMS: 12000,
		Checks: []scenario.Check{
			{Kind: scenario.CheckAnsweredWithin, Line: 1, AfterMS: 8000},
			{Kind: scenario.CheckSaid, Line: 1, FromMS: 0, AfterMS: 10000, Any: []string{"Tokyo"}},
		},
	})
}

func TestLiveRoomSubstantiveReply(t *testing.T) {
	runLiveRoomDiagnostic(t, scenario.Scenario{
		Name:         "room substantive answer to agent question",
		Instructions: "You are a patient internet support agent. Keep replies to one brief English sentence. First ask whether the caller can unplug the router for 30 seconds. Respond helpfully to their concern with the next step.",
		Script: []scenario.Line{
			{Speaker: "user", Text: "My home internet is slow. Can you help?"},
			{Speaker: "user", AtMS: 16000, Text: "Alright, I guess I can try that, but it's a real hassle to get behind the desk to unplug it, so I hope this actually works."},
		}, TrailingMS: 12000,
		Checks: []scenario.Check{{Kind: scenario.CheckAnsweredWithin, Line: 1, AfterMS: 8000}},
	})
}
