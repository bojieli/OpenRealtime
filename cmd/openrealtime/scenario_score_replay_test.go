package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/coder/websocket"
)

type replaySourceVoice struct{}

func (replaySourceVoice) Speak(context.Context, string, string) ([]int16, error) {
	return make([]int16, 2400), nil
}

func TestScenarioSourceReplayRejectsSealedHistoricalScores(t *testing.T) {
	t.Chdir("../..")
	directory, receipt, _ := publishScenarioGraphPopulationFixture(t, 1, false, "an ordinary question")
	options := graphnative.SourceBundleOptions{Directory: directory}
	if _, err := graphnative.VerifySourceBundle(t.Context(), options, receipt); err != nil {
		t.Fatalf("historical integrity reader regressed: %v", err)
	}
	if _, err := graphnative.VerifyScoredSourceBundle(t.Context(), options, receipt); err == nil || !strings.Contains(err.Error(), "no current deterministic replay evidence") {
		t.Fatalf("historical fabricated score acquired acceptance: %v", err)
	}
}

func TestScenarioSourceReplayRecomputesSealedOutcomeAndArchitecture(t *testing.T) {
	t.Chdir("../..")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		sent := false
		for {
			_, payload, err := connection.Read(r.Context())
			if err != nil {
				return
			}
			var incoming struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &incoming) != nil || incoming.Type != "input_audio_buffer.append" || sent {
				continue
			}
			sent = true
			for _, event := range []map[string]any{
				{"type": "response.created"},
				{"type": "response.output_audio_transcript.delta", "response_id": "answer", "delta": "Paris."},
				{"type": "response.output_audio.delta", "response_id": "answer", "delta": base64.StdEncoding.EncodeToString(make([]byte, 24000))},
				{"type": "response.done", "response": map[string]any{"id": "answer", "status": "completed"}},
			} {
				data, _ := json.Marshal(event)
				if connection.Write(r.Context(), websocket.MessageText, data) != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	var item scenario.Scenario
	for _, candidate := range scenario.Suite() {
		if candidate.Name == "an ordinary question" {
			item = candidate
		}
	}
	var audio bench.SessionAudioCapture
	result, err := scenario.Play(t.Context(), replaySourceVoice{}, bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 20 * time.Second,
		TrailingSilence: time.Millisecond, PostPlaybackQuiet: time.Millisecond,
		CaptureAudio: func(value bench.SessionAudioCapture) error { audio = value; return nil },
	}, item)
	if err != nil || result.Replay == nil {
		t.Fatalf("record production fixture: %v", err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"original", "changed outcome", "changed architecture metric"} {
		t.Run(mode, func(t *testing.T) {
			capture := func(identity scenario.Result) (scenario.Result, bench.SessionAudioCapture) {
				var retained scenario.Result
				if err := json.Unmarshal(payload, &retained); err != nil {
					t.Fatal(err)
				}
				retained.Transcript.Runtime, retained.Transcript.Execution = identity.Transcript.Runtime, identity.Transcript.Execution
				if mode == "changed outcome" {
					retained.Passed = !retained.Passed
					retained.Failures = []string{"fabricated failure"}
				}
				return retained, audio
			}
			var mutate func(*archbench.Result)
			if mode == "changed architecture metric" {
				mutate = func(result *archbench.Result) {
					result.Measurement.Tasks[0].Metrics["reaction_latency_p50_ms"] = 999999
				}
			}
			directory, receipt, _ := publishScenarioGraphReplayFixture(t, 1, false, capture, mutate, item.Name)
			options := graphnative.SourceBundleOptions{Directory: directory}
			// Every variant has a fresh coherent receipt and a valid summary.
			if _, err := graphnative.VerifySourceBundle(t.Context(), options, receipt); err != nil {
				t.Fatal(err)
			}
			_, err := graphnative.VerifyScoredSourceBundle(t.Context(), options, receipt)
			if mode == "original" && err != nil {
				t.Fatalf("production source failed replay: %v", err)
			}
			if mode != "original" && err == nil {
				t.Fatal("resealing forged scores granted replay credit")
			}
			var output strings.Builder
			commandErr := runReview([]string{"replay-scenario", "-source-dir", directory, "-source-receipt", directory + ".receipt.json"}, &output)
			if (commandErr == nil) != (mode == "original") || commandErr == nil && !strings.Contains(output.String(), "Replayed 1 scenario attempts") {
				t.Fatalf("public replay command disagrees with verifier: %v %s", commandErr, output.String())
			}
		})
	}
}

func TestScenarioScoreReplayCommandRequiresExactInputs(t *testing.T) {
	for _, args := range [][]string{{}, {"-source-dir", "source"}, {"-source-receipt", "receipt"}, {"unexpected"}} {
		if err := runScenarioScoreReplay(args, io.Discard); err == nil {
			t.Fatalf("incomplete replay command accepted %v", args)
		}
	}
}
