package scenario

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/coder/websocket"
)

type speechCueVoice struct{}

func (speechCueVoice) Speak(context.Context, string, string) ([]int16, error) {
	return activityCapture([2]int{0, 200}).Agent[0].PCM16, nil
}

func TestSpeechTriggeredScenarioUsesSentPositionsThroughRecordedWAVAndReview(t *testing.T) {
	for _, mode := range []string{"continues", "silent", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
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
					pcm := make([]int16, 900*24)
					pcm = append(pcm, activityCapture([2]int{0, 3100}).Agent[0].PCM16...)
					if mode == "silent" {
						clear(pcm)
					}
					status := "completed"
					if mode == "cancelled" {
						status = "cancelled"
					}
					for _, event := range []map[string]any{{"type": "response.created"}, {"type": "response.output_audio.delta", "response_id": "speech", "delta": base64.StdEncoding.EncodeToString(cuePCM(bench.SpeechCue{PCM16: pcm}))}, {"type": "response.done", "response": map[string]any{"id": "speech", "status": status}}} {
						data, _ := json.Marshal(event)
						if connection.Write(r.Context(), websocket.MessageText, data) != nil {
							return
						}
					}
				}
			}))
			defer server.Close()
			item := Scenario{Name: "speech-cue-fixture", Script: []Line{
				{Speaker: "user", Text: "Explain."},
				{Speaker: "user", Text: "Mhm.", AtMS: 1000, AfterSpeech: &SpeechWindow{LatestMS: 1700, LookbackMS: 500, MinimumActiveMS: 300, RecentMS: 100}},
				{Speaker: "user", Text: "Yeah.", AtMS: 2200, AfterSpeech: &SpeechWindow{LatestMS: 2600, LookbackMS: 500, MinimumActiveMS: 300, RecentMS: 100}},
			}, TrailingMS: 300, Checks: []Check{
				{Kind: CheckHeldAcross, Line: 1, BeforeMS: 500, AfterMS: 300, MaxGapMS: 500},
				{Kind: CheckHeldAcross, Line: 2, BeforeMS: 500, AfterMS: 300, MaxGapMS: 500},
			}}
			var capture bench.SessionAudioCapture
			result, err := Play(t.Context(), speechCueVoice{}, bench.SessionConfig{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 6 * time.Second, TrailingSilence: time.Millisecond, PostPlaybackQuiet: time.Millisecond, CaptureAudio: func(value bench.SessionAudioCapture) error { capture = value; return nil }}, item)
			if err != nil || result.Passed != (mode == "continues") || len(result.Transcript.SpeechCues) != 2 {
				t.Fatalf("wrong cue scenario: %v %+v", err, result)
			}
			cue := result.Transcript.SpeechCues[0]
			if mode == "silent" {
				if cue.Status != "missed" || !strings.Contains(strings.Join(result.Failures, " "), "speech opportunity") {
					t.Fatalf("missing opportunity did not fail: %+v", result)
				}
			} else {
				// The window is the cue's own: at or after the authored 1000 ms
				// and no later than its 1700 ms deadline. What proves the score
				// used the sent position rather than the authored one is the
				// equality that follows it, which holds wherever inside that
				// window the cue lands. A tighter bound proved nothing extra
				// and failed the verification gate under load, where the
				// agent's audio arrived later and the cue landed legally late.
				if cue.Status != "sent" || cue.StartMS < 1000 || cue.StartMS > 1700 ||
					result.Holds[0].TriggerStartMS != cue.StartMS ||
					result.Holds[0].TriggerEndMS != int(cue.EndMS) {
					t.Fatalf("score used authored cue instead of sent position: %+v", result.Holds)
				}
				if mode == "cancelled" && !strings.Contains(strings.Join(result.Failures, " "), "cancelled") {
					t.Fatalf("cancelled speech received hold credit: %+v", result)
				}
			}
			directory := filepath.Join(t.TempDir(), "review")
			run, err := NewReviewRun(ReviewOptions{Directory: directory, Scenarios: []Scenario{item}, Repeats: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Record(item.Name, 1, capture, result, nil); err != nil {
				t.Fatal(err)
			}
			original := slices.Clone(result.Transcript.SpeechCues)
			result.Transcript.SpeechCues[0].Name = "caller-mutated"
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			result.Transcript.SpeechCues = original
			manifest := readReviewManifest(t, directory)
			wav, err := os.ReadFile(filepath.Join(directory, manifest.Attempts[0].Audio.Path))
			if err != nil {
				t.Fatal(err)
			}
			room, agent := decodeStereoReviewWAV(t, wav)
			_, replayErr := ReplayRecorded(t.Context(), item, result, wav)
			if mode == "silent" {
				if replayErr == nil || !strings.Contains(replayErr.Error(), "complete sent cue") {
					t.Fatalf("missing source cue should refuse replay: %v", replayErr)
				}
			} else if replayErr != nil {
				t.Fatalf("production sent cues did not replay: %v", replayErr)
			}
			timeline, err := Compose(t.Context(), speechCueVoice{}, item)
			if err != nil {
				t.Fatal(err)
			}
			reopened := ScoreWithAudio(item, timeline, result.Transcript, bench.SessionAudioCapture{SampleRateHz: 24000, RoomPCM16: room, Agent: []bench.TimedAudioChunk{{PCM16: agent}}})
			if reopened.Passed != result.Passed || !reflect.DeepEqual(reopened.Failures, result.Failures) || !reflect.DeepEqual(reopened.Holds, result.Holds) {
				t.Fatalf("retained WAV did not reproduce actual cue scoring: %+v versus %+v", reopened, result)
			}
			review, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
			if err != nil || !strings.Contains(string(review), "Speech cue scenario.line.1:") || strings.Contains(string(review), "caller-mutated") {
				t.Fatalf("review lost/aliased cue evidence: %v %s", err, review)
			}
			if mode != "continues" {
				return
			}
			for _, mutate := range []func(*bench.Transcript, *bench.SessionAudioCapture){
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { tr.SpeechCues[0].StartMS = 1000 },
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { tr.SpeechCues[0].SentSamples-- },
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { tr.SpeechCues[0].PCM16SHA256 = "changed" },
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { tr.SpeechCues[0].ActiveMS++ },
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) {
					tr.SpeechCues[0], tr.SpeechCues[1] = tr.SpeechCues[1], tr.SpeechCues[0]
				},
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { tr.SpeechCues = nil },
				func(tr *bench.Transcript, c *bench.SessionAudioCapture) { c.RoomPCM16 = nil },
			} {
				tr := result.Transcript
				tr.SpeechCues = slices.Clone(tr.SpeechCues)
				audio := capture
				mutate(&tr, &audio)
				if bad := ScoreWithAudio(item, timeline, tr, audio); bad.Passed {
					t.Fatal("tampered cue acquired credit")
				}
			}
		})
	}
}

func TestSpeechDiagnosticKeepsReleasePopulationAndGrounding(t *testing.T) {
	if len(Suite()) != 12 || len(Diagnostics()) != 1 || len(Catalog()) != 13 {
		t.Fatal("diagnostic changed release population")
	}
	item := Diagnostics()[0]
	if item.Name != SpeechAcknowledgementDiagnostic || item.Script[1].AfterSpeech == nil || item.Script[2].AfterSpeech == nil || len(item.Checks) != 8 || !strings.Contains(item.Instructions, "original payment method") {
		t.Fatalf("diagnostic lost authored checks/content: %+v", item)
	}
	timeline, err := Compose(t.Context(), speechCueVoice{}, item)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Cues) != 2 || timeline.Spans[1].StartMS != -1 || timeline.Spans[2].StartMS != -1 {
		t.Fatalf("conditional cues claimed authored positions: %+v", timeline.Spans)
	}
	for _, sample := range timeline.Samples[6000*24:] {
		if sample != 0 {
			t.Fatal("conditional stimulus was sent at authored time")
		}
	}
	item.Script[1].AfterSpeech.LatestMS = 1
	if _, err := Compose(t.Context(), speechCueVoice{}, item); err == nil {
		t.Fatal("invalid speech window synthesized into a run")
	}
}
