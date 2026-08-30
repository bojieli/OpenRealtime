package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

func TestFishTTSWiresTheExactSelectedVoiceToTheNativeReference(t *testing.T) {
	for _, voice := range []string{"default", "alloy"} {
		t.Run(voice, func(t *testing.T) {
			requested := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var body struct {
					ReferenceID string `json:"reference_id"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode Fish request: %v", err)
				}
				requested <- body.ReferenceID
				_, _ = writer.Write([]byte{1, 0, 2, 0})
			}))
			defer server.Close()

			provider, err := NewTTS(TTSRequest{
				Provider: "fish-audio", Model: "fishaudio/fish-speech-1.5",
				BaseURL: server.URL, Voice: voice, OutputSampleRateHz: 24_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := provider.Stream(context.Background(), v1.SpeechPlan{
				CandidateID: "candidate", Text: "hello",
			}, func(v1.SpeechChunk) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if got := <-requested; got != voice {
				t.Fatalf("Fish reference_id = %q, want selected voice %q", got, voice)
			}
		})
	}
}
