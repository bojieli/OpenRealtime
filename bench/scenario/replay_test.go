package scenario

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

// These are scorer fixtures, not model performance evidence. Every authored
// case must be replayable even when its behavioral checks fail.
func TestReplayRecordedCoversEveryCanonicalScenario(t *testing.T) {
	t.Chdir("../..")
	for _, item := range Suite() {
		t.Run(item.Name, func(t *testing.T) {
			result, wav := replayFixture(t, item, nil, nil)
			got, err := ReplayRecorded(t.Context(), item, result, wav)
			if err != nil || !reflect.DeepEqual(got, result) {
				t.Fatalf("canonical case failed exact replay: %v", err)
			}
		})
	}
}

func TestReplayRecordedRejectsChangedScoresInputsAndTimings(t *testing.T) {
	t.Chdir("../..")
	item := acknowledgementScenario(t)
	result, wav := replayFixture(t, item, nil, nil)
	if len(result.Holds) == 0 || len(result.Latencies) == 0 || len(result.Failures) == 0 {
		t.Fatal("fixture must expose acoustic measurements, latency and negative outcomes")
	}
	for _, test := range []struct {
		name   string
		change func(*Result, []byte)
	}{
		{"false pass", func(r *Result, _ []byte) { r.Passed = true; r.Failures = nil }},
		{"failure reason", func(r *Result, _ []byte) { r.Failures[0] = "unrelated" }},
		{"hold measurement", func(r *Result, _ []byte) { r.Holds[0].BeforeActiveMS++ }},
		{"latency", func(r *Result, _ []byte) { r.Latencies[0].MS++ }},
		{"missed trigger", func(r *Result, _ []byte) { r.Latencies[0].Heard = !r.Latencies[0].Heard }},
		{"missing evidence", func(r *Result, _ []byte) { r.Replay = nil }},
		{"older scorer", func(r *Result, _ []byte) { r.ScorerVersion-- }},
		{"unknown replay", func(r *Result, _ []byte) { r.Replay.Version++ }},
		{"input count", func(r *Result, _ []byte) { r.Replay.Inputs[0].Samples++ }},
		{"allocation overflow", func(r *Result, _ []byte) { r.Replay.Inputs[0].Samples = math.MaxInt }},
		{"input digest", func(r *Result, _ []byte) { r.Replay.Inputs[0].PCM16SHA256 = "changed" }},
		{"input inventory", func(r *Result, _ []byte) { r.Replay.Inputs = nil }},
		{"authored identity", func(r *Result, _ []byte) { r.Replay.ScenarioSHA256 = "changed" }},
		{"partial playback", func(r *Result, _ []byte) { r.Transcript.PlaybackMS-- }},
		{"failed playback", func(r *Result, _ []byte) { r.Transcript.Failure = "disconnected" }},
		{"nonfinite event", func(r *Result, _ []byte) { r.Transcript.Moments[0].AtMS = math.NaN() }},
		{"overlapping audio", func(r *Result, _ []byte) {
			r.Transcript.Moments = append(r.Transcript.Moments, r.Transcript.Moments[0])
		}},
		{"audio outside recording", func(r *Result, _ []byte) { r.Transcript.Moments[0].PlayoutAtMS = maximumReplayMS }},
		{"fractional sample", func(r *Result, _ []byte) { r.Transcript.Moments[0].AudioMS += 0.01 }},
		{"unattributed PCM", func(r *Result, _ []byte) { r.Transcript.Moments = r.Transcript.Moments[1:] }},
		{"room PCM", func(_ *Result, w []byte) { w[44] ^= 1 }},
		{"undeclared room PCM", func(_ *Result, w []byte) { w[len(w)-4] ^= 1 }},
		{"undeclared agent PCM", func(_ *Result, w []byte) { w[len(w)-2] ^= 1 }},
		{"WAV rate", func(_ *Result, w []byte) { w[24] ^= 1 }},
		{"extra hearing", func(r *Result, _ []byte) { r.Replay.Hearings = []ReplayHearing{{Text: "unrequested"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			retained, audio := cloneReplayResult(t, result), slices.Clone(wav)
			test.change(&retained, audio)
			if _, err := ReplayRecorded(t.Context(), item, retained, audio); err == nil {
				t.Fatal("changed evidence or score was accepted")
			}
		})
	}
	for _, change := range []func(*Scenario){
		func(s *Scenario) { s.Instructions += " changed" },
		func(s *Scenario) { s.Script[0].Text += " changed" },
		func(s *Scenario) { s.Checks[0].AfterMS++ },
	} {
		changed := acknowledgementScenario(t)
		change(&changed)
		if _, err := ReplayRecorded(t.Context(), changed, result, wav); err == nil {
			t.Fatal("changed authored scenario was accepted")
		}
	}
}

func TestReplayRecordedBindsIndependentHearingToRequestedPCMWindows(t *testing.T) {
	item := SubturnSuite()[0]
	for _, mode := range []string{"continues", "restarts", "recognizer error", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			var listen heard
			if mode != "unavailable" {
				listen = func(from, to int) (string, error) {
					if mode == "recognizer error" {
						return "", errors.New("recognizer unavailable")
					}
					if from == 0 {
						return "one two three", nil
					}
					if mode == "restarts" {
						return "one two three", nil
					}
					return "four five six", nil
				}
			}
			capture := activityCapture([2]int{1000, 2000}, [2]int{14000, 14700}, [2]int{25300, 26000})
			result, wav := replayFixture(t, item, &capture, listen)
			if result.Passed != (mode == "continues") {
				t.Fatalf("wrong count fixture: %+v", result.Failures)
			}
			if _, err := ReplayRecorded(t.Context(), item, result, wav); err != nil {
				t.Fatal(err)
			}
			if mode != "continues" {
				return
			}
			for _, change := range []func(*ReplayEvidence){
				func(e *ReplayEvidence) { e.Hearings[0].FromMS++ },
				func(e *ReplayEvidence) { e.Hearings[1].ToMS-- },
				func(e *ReplayEvidence) { e.Hearings[0].PCM16SHA256 = "changed" },
				func(e *ReplayEvidence) { e.Hearings[1].Text = "one two three" },
				func(e *ReplayEvidence) { e.Hearings = e.Hearings[:1] },
				func(e *ReplayEvidence) { e.Hearings = append(e.Hearings, e.Hearings[0]) },
				func(e *ReplayEvidence) { e.Hearings[0], e.Hearings[1] = e.Hearings[1], e.Hearings[0] },
				func(e *ReplayEvidence) { e.HearingAvailable = false },
			} {
				changed := cloneReplayResult(t, result)
				change(changed.Replay)
				if _, err := ReplayRecorded(t.Context(), item, changed, wav); err == nil {
					t.Fatal("changed hearing accepted")
				}
			}
			// Changing the observed PCM while keeping recognizer text must fail
			// even if its waveform-only behavior would be identical.
			wav[46+1000*24*4] ^= 1
			if _, err := ReplayRecorded(t.Context(), item, result, wav); err == nil {
				t.Fatal("hearing detached from its PCM")
			}
		})
	}
}

func TestReplayRecordedReconstructsMenuAndRejectsForgedResults(t *testing.T) {
	item := Suite()[2]
	capture := bench.SessionAudioCapture{SampleRateHz: 24000}
	result, wav := replayFixture(t, item, &capture, nil)
	if !result.Passed {
		t.Fatalf("correct menu fixture failed: %v", result.Failures)
	}
	if _, err := ReplayRecorded(t.Context(), item, result, wav); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Result){
		func(r *Result) { r.Transcript.Moments[0].Arguments = `{"digit":"1"}` },
		func(r *Result) { r.Transcript.Moments[1].Text = `{"ok":true}` },
		func(r *Result) { r.Transcript.Moments[0].CallID = "" },
		func(r *Result) { r.Transcript.Moments = append(r.Transcript.Moments, r.Transcript.Moments[0]) },
		func(r *Result) { r.Transcript.Moments = append(r.Transcript.Moments, r.Transcript.Moments[1]) },
		func(r *Result) {
			r.Transcript.Moments[0], r.Transcript.Moments[1] = r.Transcript.Moments[1], r.Transcript.Moments[0]
		},
		func(r *Result) { r.Transcript.Moments = r.Transcript.Moments[:1] },
	} {
		changed := cloneReplayResult(t, result)
		change(&changed)
		if _, err := ReplayRecorded(t.Context(), item, changed, wav); err == nil {
			t.Fatal("forged menu evidence accepted")
		}
	}
	changed := item
	changed.Menu = func() *Menu { menu := OrderStatusMenu(); menu.Goal = "billing"; return menu }
	if _, err := ReplayRecorded(t.Context(), changed, result, wav); err == nil {
		t.Fatal("changed authored menu accepted")
	}
}

func TestReplayRecordedBindsAuthoredVisualBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sight.png")
	if err := os.WriteFile(path, []byte("authored visual"), 0600); err != nil {
		t.Fatal(err)
	}
	item := Scenario{Name: "visual", Sees: []Sight{{AtMS: 1000, Path: path}}, TrailingMS: 1000, Checks: []Check{{Kind: CheckSilent, Line: -1}}}
	result, wav := replayFixture(t, item, nil, nil)
	if _, err := ReplayRecorded(t.Context(), item, result, wav); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement visual"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayRecorded(t.Context(), item, result, wav); err == nil {
		t.Fatal("visual replacement accepted")
	}
}

func TestReviewReplayEvidenceIsCopiedAndRedacted(t *testing.T) {
	result := Result{Replay: &ReplayEvidence{Inputs: []ReplayInput{{Samples: 24}}, Hearings: []ReplayHearing{{Text: "private-value", Error: "private-value"}}}}
	copy := sanitizedReviewResult(result, func(text string) string { return strings.ReplaceAll(text, "private-value", "[redacted]") })
	copy.Replay.Inputs[0].Samples++
	if result.Replay.Inputs[0].Samples != 24 || result.Replay.Hearings[0].Text != "private-value" || strings.Contains(copy.Replay.Hearings[0].Text+copy.Replay.Hearings[0].Error, "private-value") {
		t.Fatal("review replay evidence aliased caller data or retained secrets")
	}
}

func replayFixture(t *testing.T, item Scenario, audio *bench.SessionAudioCapture, listen heard) (Result, []byte) {
	t.Helper()
	voice := &recordingVoice{voice: speechCueVoice{}}
	timeline, err := Compose(t.Context(), voice, item)
	if err != nil {
		t.Fatal(err)
	}
	capture := activityCapture([2]int{500, 1000})
	if audio != nil {
		capture = *audio
	}
	// Non-integral millisecond offsets exercise exact sample reconstruction.
	for index := range capture.Agent {
		capture.Agent[index].AtMS += 0.125
	}
	capture.RoomPCM16 = timeline.Samples
	transcript := bench.Transcript{PlaybackMS: float64(timeline.TotalMS)}
	for _, chunk := range capture.Agent {
		transcript.Moments = append(transcript.Moments, bench.Moment{Kind: bench.MomentAgentAudio, AtMS: chunk.AtMS, PlayoutAtMS: chunk.AtMS, AudioMS: float64(len(chunk.PCM16)) / 24, ResponseID: "fixture"})
	}
	var menu *Menu
	if item.Menu != nil {
		menu = item.Menu()
		output, err := menu.Respond("press_key", json.RawMessage(`{"digit":"2"}`))
		if err != nil {
			t.Fatal(err)
		}
		transcript.Moments = append(transcript.Moments,
			bench.Moment{Kind: bench.MomentToolCall, AtMS: 7100, CallID: "key-1", Name: "press_key", Arguments: `{"digit":"2"}`},
			bench.Moment{Kind: bench.MomentToolResult, AtMS: 7101, CallID: "key-1", Name: "press_key", Text: string(output)})
	}
	digest, err := replayScenarioDigest(item)
	if err != nil {
		t.Fatal(err)
	}
	evidence := &ReplayEvidence{Version: ReplayVersion, ScenarioSHA256: digest, Inputs: voice.inputs}
	result := score(item, timeline, transcript, menu, recordHearing(listen, capture, evidence), &capture)
	result.Latencies, result.Replay = latencies(item, timeline, transcript), evidence
	wav, _, err := encodeReviewStereoWAV(capture)
	if err != nil {
		t.Fatal(err)
	}
	return result, wav
}

func cloneReplayResult(t *testing.T, result Result) Result {
	t.Helper()
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var copy Result
	if err := json.Unmarshal(payload, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}
