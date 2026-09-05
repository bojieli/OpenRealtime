package scenario

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/coder/websocket"
)

func TestPlayScoresAndRetainsActualAcknowledgementAudio(t *testing.T) {
	for _, mode := range []string{"continues", "stops", "silent-packet"} {
		t.Run(mode, func(t *testing.T) {
			samples := activityCapture([2]int{0, 2000}).Agent[0].PCM16
			if mode == "stops" {
				samples = samples[:300*24]
			}
			if mode == "silent-packet" {
				clear(samples)
			}
			pcm := make([]byte, len(samples)*2)
			for index, sample := range samples {
				binary.LittleEndian.PutUint16(pcm[index*2:], uint16(sample))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
				if err != nil {
					return
				}
				defer connection.Close(websocket.StatusNormalClosure, "fixture finished")
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
						{"type": "response.output_audio_transcript.delta", "response_id": "response-fixture", "delta": "Here are the refund details."},
						{"type": "response.output_audio.delta", "response_id": "response-fixture", "delta": base64.StdEncoding.EncodeToString(pcm)},
						{"type": "response.done", "response": map[string]any{"id": "response-fixture", "status": "completed"}},
					} {
						encoded, _ := json.Marshal(event)
						if err := connection.Write(r.Context(), websocket.MessageText, encoded); err != nil {
							return
						}
					}
				}
			}))
			t.Cleanup(server.Close)
			item := Scenario{Name: "acknowledgement-playback", Script: []Line{
				{Speaker: "user", Text: "Explain the refund process."}, {Speaker: "user", Text: "Mhm.", AtMS: 1000},
			}, TrailingMS: 500, Checks: []Check{{Kind: CheckHeldAcross, Line: 1, BeforeMS: 300, AfterMS: 300, MaxGapMS: 200}}}
			var capture bench.SessionAudioCapture
			captures := 0
			result, err := Play(t.Context(), fixedVoice{ms: 200}, bench.SessionConfig{
				Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 5 * time.Second,
				TrailingSilence: time.Millisecond, PostPlaybackQuiet: time.Millisecond,
				CaptureAudio: func(audio bench.SessionAudioCapture) error { capture = audio; captures++; return nil },
			}, item)
			if err != nil || result.Passed != (mode == "continues") || captures != 1 || len(result.Holds) != 1 {
				t.Fatalf("Play failed to use actual capture: error=%v captures=%d result=%+v", err, captures, result)
			}
			directory := filepath.Join(t.TempDir(), "review")
			run, err := NewReviewRun(ReviewOptions{Directory: directory, Scenarios: []Scenario{item}, Repeats: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Record(item.Name, 1, capture, result, nil); err != nil {
				t.Fatal(err)
			}
			// Retention owns the measured numbers; later caller mutation must
			// not rewrite the human review independently of its recorded audio.
			original := result.Holds[0]
			result.Holds[0].BeforeActiveMS = 9999
			if len(original.Responses) > 0 {
				result.Holds[0].Responses[0].Status = "caller-mutated"
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			result.Holds[0] = original
			manifest := readReviewManifest(t, directory)
			if !manifest.Complete || len(manifest.Attempts) != 1 || manifest.Attempts[0].Passed != result.Passed {
				t.Fatalf("retained outcome differs: %+v", manifest)
			}
			wav, err := os.ReadFile(filepath.Join(directory, manifest.Attempts[0].Audio.Path))
			if err != nil {
				t.Fatal(err)
			}
			_, agent := decodeStereoReviewWAV(t, wav)
			timeline, err := Compose(t.Context(), fixedVoice{ms: 200}, item)
			if err != nil {
				t.Fatal(err)
			}
			reopened := ScoreWithAudio(item, timeline, result.Transcript, bench.SessionAudioCapture{SampleRateHz: 24_000,
				Agent: []bench.TimedAudioChunk{{AtMS: 0, PCM16: agent}}})
			// Recompute from the retained audio and original terminal transcript.
			if len(original.Responses) > 0 {
				result.Holds[0].Responses[0].Status = "completed"
			}
			if reopened.Passed != result.Passed || !reflect.DeepEqual(reopened.Holds, result.Holds) {
				t.Fatalf("retained WAV does not reproduce activity: %+v versus %+v", reopened.Holds, result.Holds)
			}
			review, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
			if err != nil || !strings.Contains(string(review), "Acknowledgement line 1:") || !strings.Contains(string(review), "longest pause") || strings.Contains(string(review), "9999 ms") || strings.Contains(string(review), "caller-mutated") {
				t.Fatalf("final review lost or changed measurements: %v\n%s", err, review)
			}
			if mode == "continues" && (!strings.Contains(string(review), "response-fixture: completed") || !strings.Contains(string(review), "does not establish why speech ended")) {
				t.Fatalf("review lost terminal evidence or attribution limit: %s", review)
			}
		})
	}
}
