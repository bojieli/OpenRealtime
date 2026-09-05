package bench_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/coder/websocket"
)

func TestSessionRetainsResponseIdentityStatusAndSerializedPlayout(t *testing.T) {
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
			for _, status := range []string{"completed", "cancelled", "incomplete", "failed"} {
				id := "response-" + status
				for _, event := range []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": id}},
					{"type": "response.output_audio_transcript.delta", "response_id": id, "delta": "Here are the refund details."},
					{"type": "response.output_audio.delta", "response_id": id, "delta": base64.StdEncoding.EncodeToString(make([]byte, 2400))},
					{"type": "response.done", "response": map[string]any{"id": id, "status": status,
						"status_details": map[string]any{"type": status, "reason": "fixture-terminal-reason"}}},
				} {
					encoded, _ := json.Marshal(event)
					if connection.Write(r.Context(), websocket.MessageText, encoded) != nil {
						return
					}
				}
			}
		}
	}))
	defer server.Close()
	var capture bench.SessionAudioCapture
	transcript, err := bench.PlaySamples(t.Context(), bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: 3 * time.Second,
		TrailingSilence: 100 * time.Millisecond, PostPlaybackQuiet: 10 * time.Millisecond,
		CaptureAudio: func(audio bench.SessionAudioCapture) error { capture = audio; return nil },
	}, make([]int16, 240))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(transcript)
	if err != nil {
		t.Fatal(err)
	}
	var retained struct {
		Moments []map[string]any `json:"moments"`
	}
	if err := json.Unmarshal(payload, &retained); err != nil {
		t.Fatal(err)
	}
	ends, audio := 0, 0
	for _, moment := range retained.Moments {
		switch moment["kind"] {
		case bench.MomentAgentText, bench.MomentAgentAudio, bench.MomentResponseDone:
			if id, ok := moment["response_id"].(string); !ok || !strings.HasPrefix(id, "response-") {
				t.Fatalf("retained response identity missing: %v", moment)
			}
		}
		if moment["kind"] == bench.MomentAgentAudio {
			if moment["playout_at_ms"] != capture.Agent[audio].AtMS {
				t.Fatalf("retained playout differs from audio: %v", moment)
			}
			audio++
		}
		if moment["kind"] == bench.MomentResponseDone {
			status := strings.TrimPrefix(moment["response_id"].(string), "response-")
			if moment["response_status"] != status || moment["response_status_reason"] != "fixture-terminal-reason" {
				t.Fatalf("terminal outcome was lost: %v", moment)
			}
			ends++
		}
	}
	if ends != 4 || audio != 4 {
		t.Fatalf("retained ends=%d audio=%d", ends, audio)
	}
}

func TestLegacyResponseMomentsKeepTheirOriginalEncoding(t *testing.T) {
	for _, source := range []string{
		`{"at_ms":123,"kind":"response_done"}`,
		`{"at_ms":100,"kind":"agent_audio","audio_ms":50}`,
	} {
		var moment bench.Moment
		if err := json.Unmarshal([]byte(source), &moment); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(moment)
		if err != nil || string(payload) != source {
			t.Fatalf("historical moment changed: %s, %v", payload, err)
		}
	}
}
