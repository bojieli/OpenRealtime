package browser_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/examples/browser"
)

func TestTheDemoIsServedAndIsTheRealPage(t *testing.T) {
	server := httptest.NewServer(browser.Handler())
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("expected HTML, got %q", contentType)
	}

	// The demo is only worth shipping if it still speaks the protocol. A page
	// that had drifted into referring to events the server does not send would
	// be worse than no demo, because it looks like it works.
	page := string(browser.Page)
	for _, required := range []string{
		"session.update",
		"response.output_audio_transcript.delta",
		"input_audio_buffer.speech_started",
		"openrealtime.observation.added",
		"RTCPeerConnection",
	} {
		if !strings.Contains(page, required) {
			t.Errorf("the demo no longer mentions %q", required)
		}
	}
}

func TestTheDemoServesOneFileAndNotADirectory(t *testing.T) {
	server := httptest.NewServer(browser.Handler())
	t.Cleanup(server.Close)

	// A directory handler here would turn a convenience into a way to read
	// whatever happened to be next to it.
	response, err := http.Get(server.URL + "/../browser.go")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer response.Body.Close()
	body := make([]byte, 64)
	n, _ := response.Body.Read(body)
	if strings.Contains(string(body[:n]), "package browser") {
		t.Fatal("the handler served a neighbouring file")
	}
}
