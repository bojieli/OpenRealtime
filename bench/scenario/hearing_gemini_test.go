package scenario

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A window of audio goes to Gemini as inline WAV with the prompt, and what
// comes back is the transcript alone.
func TestGeminiHearingSendsTheWindowAndReturnsTheWords(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-goog-api-key") != "secret" || !strings.HasSuffix(request.URL.Path, "/models/gemini-test:generateContent") {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewDecoder(request.Body).Decode(&seen)
		_, _ = writer.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":" One. Two. \n"}]}}]}`))
	}))
	defer server.Close()
	listen := Hearing{GeminiAPIKey: "secret", GeminiModel: "gemini-test", GeminiEndpoint: server.URL}
	samples := make([]int16, 2400)
	for index := range samples {
		samples[index] = int16(index % 200 * 50)
	}
	text, err := listen.hear(context.Background(), samples, 24_000)
	if err != nil || text != "One. Two." {
		t.Fatalf("heard %q, %v", text, err)
	}
	contents, _ := seen["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("request contents = %v", seen["contents"])
	}
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("request parts = %v", parts)
	}
	inline, _ := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "audio/wav" || inline["data"] == "" {
		t.Fatalf("audio part = %v", parts[0])
	}
	if prompt, _ := parts[1].(map[string]any)["text"].(string); !strings.Contains(prompt, "numbers written as words") {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestPacedShortensAPauseInsideALineAndKeepsItsEdges(t *testing.T) {
	tone := func(frames int) []int16 {
		out := make([]int16, frames*240)
		for index := range out {
			out[index] = int16(8000 * math.Sin(float64(index)*0.3))
		}
		return out
	}
	silence := func(frames int) []int16 { return make([]int16, frames*240) }
	line := append(append(append(append(silence(20), tone(30)...), silence(115)...), tone(30)...), silence(50)...)
	out := paced(line)
	want := (20 + 30 + 40 + 30 + 50) * 240
	if len(out) != want {
		t.Fatalf("paced length = %d frames, want %d", len(out)/240, want/240)
	}
	if !equalSamples(out[:20*240], silence(20)) || !equalSamples(out[len(out)-50*240:], silence(50)) {
		t.Fatal("paced moved the line's edges")
	}
	short := append(append(tone(30), silence(30)...), tone(30)...)
	if len(paced(short)) != len(short) {
		t.Fatal("a pause a speaker takes was shortened")
	}
}

func equalSamples(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
