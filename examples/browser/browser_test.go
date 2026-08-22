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

	// What follows reads the page's source, so it can only speak about the
	// shape of the code. It cannot say the demo works, and it once said so
	// anyway: the page was served intact, its script parsed, every event name
	// below was present, and its Content-Security-Policy forbade the one
	// request it exists to make, so it could not reach an adapter in any
	// documented configuration. This test called that healthy - while its own
	// comment warned that a broken demo is worse than none because it looks
	// like it works. It was describing itself.
	//
	// TestTheDemoHoldsAConversation is the one that answers "does it work",
	// by running the page in a browser against a real adapter. This one
	// answers the narrower question of whether the source still has the form
	// a client author should copy, which a live test does not reach: the
	// status-rendering branches below are checked here and nowhere else,
	// because provoking each status from a live session would put recogniser
	// timing inside a test about a page.
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

	// A turn can end without having said anything, and the reason rides on
	// response.done. A demo that read only "the turn is over" would connect,
	// listen, answer nothing, and report itself healthy - which is worse than
	// no demo, for the same reason as drifting off the protocol.
	if !strings.Contains(page, "status_details") {
		t.Error("the demo does not read why a turn ended, so a silent turn looks healthy")
	}
	// And not by treating every non-completed status as trouble: a cancelled
	// turn is barge-in, the most ordinary thing in a voice session, and
	// reporting it is a demo crying wolf whenever someone interrupts.
	if strings.Contains(page, `!== "completed"`) {
		t.Error("a cancelled turn is the user interrupting on purpose, not a fault to report")
	}

	// Which statuses deserve a warning is a judgement about the ones we know.
	// Whether a status is shown at all must not be, or the demo goes silent
	// the day the server learns a new word - the same absence one layer down.
	// This is the form client authors copy, so it is the form to get right.
	if !strings.Contains(page, "connected · ${status}") {
		t.Error("a status the demo predates must be shown, not decided to mean nothing")
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
