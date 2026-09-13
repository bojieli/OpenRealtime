package main

import (
	"context"
	"encoding/json"
	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in diagnostic scores one attempt for each canonical case against a
// running room. It is separate from the preregistered repeated-run benchmark.
func TestLiveRoomTwelveScenarios(t *testing.T) {
	endpoint := os.Getenv("OPENREALTIME_ROOM_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set OPENREALTIME_ROOM_TEST_ENDPOINT for the live twelve-case room diagnostic")
	}
	// Gemini listens to the agent's audio when a key is present; the local
	// Whisper is the fallback without one.
	voice := scenario.SpeechVoice{Endpoint: "http://127.0.0.1:8123/v1/audio/speech", Model: "fishaudio/fish-speech-1.5", Default: "default", Voices: map[string]string{"other": "alloy"}, Listen: scenario.Hearing{
		Endpoint: "http://127.0.0.1:8003/v1/audio/transcriptions", Model: "whisper-turbo", Language: "en",
		GeminiAPIKey: strings.TrimSpace(os.Getenv("GEMINI_API_KEY")),
	}}
	// The shared speech cache is rooted at the checkout, not the Go package.
	cache, err := filepath.Abs("../../.runtime/speech-cache")
	if err != nil {
		t.Fatal(err)
	}
	previous := scenario.CacheDir
	scenario.CacheDir = cache
	t.Cleanup(func() { scenario.CacheDir = previous })
	root := filepath.Dir(filepath.Dir(cache))
	// OPENREALTIME_LIVE_SCENARIOS names a comma-separated subset, for
	// bisecting one behaviour without the whole nine-minute pass.
	wanted := map[string]bool{}
	for _, name := range strings.Split(os.Getenv("OPENREALTIME_LIVE_SCENARIOS"), ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			wanted[trimmed] = true
		}
	}
	for _, item := range scenario.Suite() {
		if len(wanted) > 0 && !wanted[item.Name] {
			continue
		}
		// A scenario names its pictures relative to the checkout, and this
		// test runs from its package directory.
		for index := range item.Sees {
			if !filepath.IsAbs(item.Sees[index].Path) {
				item.Sees[index].Path = filepath.Join(root, item.Sees[index].Path)
			}
		}
		t.Run(item.Name, func(t *testing.T) {
			// Independent sessions against one room: they play at the same
			// time, as several people in several rooms would.
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			result, err := scenario.Play(ctx, voice, bench.SessionConfig{Endpoint: endpoint, Token: os.Getenv("OPENREALTIME_TOKEN"), CaptureRuntimeEvidence: true}, item)
			payload, _ := json.Marshal(result)
			t.Logf("diagnostic evidence: %s", payload)
			if err != nil {
				t.Fatal(err)
			}
			if result.Transcript.Runtime == nil || result.Transcript.Runtime.Binding != "openrealtime.scenario_conversation" {
				t.Fatalf("unexpected runtime: %+v", result.Transcript.Runtime)
			}
			if !result.Passed {
				t.Errorf("scenario failures: %v", result.Failures)
			}
		})
	}
}
