package gptlive_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gptlive"
	"github.com/bojieli/OpenRealtime/internal/clock"
)

// TestASidebandWatchesAndSteersButNeverSpeaks checks the attach path: it goes
// to the session's attach endpoint, starts nothing, receives the session's
// reflected audio with its timing, may push context, and refuses audio.
func TestASidebandWatchesAndSteersButNeverSpeaks(t *testing.T) {
	fake := newFakeLive(t)
	client, err := gptlive.Attach(t.Context(), gptlive.Config{
		URL: fake.url(), APIKey: "k", Scheduler: clock.NewManual(0), CloseTimeout: -1,
	}, "live_primary_1")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	<-fake.ready
	if paths := fake.connectionPaths(); len(paths) != 1 || !strings.HasSuffix(paths[0], "/live_primary_1/attach") {
		t.Fatalf("the sideband dialled %v, want …/live_primary_1/attach", paths)
	}
	if client.SessionID() != "live_primary_1" {
		t.Errorf("the sideband reports session %q", client.SessionID())
	}
	fake.refuteSent(t, "session.start")

	// The session is running: its events arrive at once, with no gate.
	fake.emit(map[string]any{
		"type": "session.output_audio.delta", "delta": speechFrame(240), "start_ms": 1000, "end_ms": 1010,
	})
	audio := expect(t, client, "response.output_audio.delta")
	if got := field(t, audio.Raw, "start_ms"); got != "" && got != "1000" {
		t.Errorf("reflected audio lost its timing: %s", audio.Raw)
	}
	if !strings.Contains(string(audio.Raw), `"end_ms":1010`) {
		t.Errorf("reflected audio must carry its range on the session timeline: %s", audio.Raw)
	}
	fake.emit(map[string]any{"type": "session.input_audio.append", "audio": speechFrame(480)})
	reflected := expect(t, client, "openrealtime.upstream.reflected_input")
	if field(t, reflected.Raw, "audio") == "" {
		t.Error("the reflected input carried no audio")
	}

	// It may steer and push context like the primary...
	sendInternal(t, client, map[string]any{"type": gptlive.EventContext, "text": "The caller is on the checkout page."})
	fake.awaitSent(t, "session.thinking.append")
	// ...but it must not carry audio, and says so rather than dropping it.
	if err := client.Send(t.Context(), map[string]any{
		"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(make([]byte, 480)),
	}); err == nil {
		t.Fatal("a sideband accepted audio it is forbidden to send")
	}
	fake.refuteSent(t, "session.input_audio.append")

	// Closing a sideband releases the socket and does not end the session.
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	fake.refuteSent(t, "session.close")
}

// TestTransportEventsAreNamed checks the telephony lifecycle reaches the
// binding under one name.
func TestTransportEventsAreNamed(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")
	fake.emit(map[string]any{"type": "transport.ringing"})
	event := expect(t, client, "openrealtime.upstream.transport")
	if got := field(t, event.Raw, "kind"); got != "ringing" {
		t.Errorf("transport event kind was %q", got)
	}
}

// TestARecordingIsDownloadedFromTheContentEndpoint checks the address is
// derived from the sessions endpoint and the payload is checked to be WAV.
func TestARecordingIsDownloadedFromTheContentEndpoint(t *testing.T) {
	wav := append([]byte("RIFF\x24\x00\x00\x00WAVE"), make([]byte, 32)...)
	var seenPath, seenAuth string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seenPath, seenAuth = request.URL.Path, request.Header.Get("Authorization")
		if strings.HasSuffix(request.URL.Path, "/live_missing/content") {
			http.Error(writer, `{"error":{"message":"no stored recording"}}`, http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "audio/wav")
		_, _ = writer.Write(wav)
	}))
	t.Cleanup(server.Close)
	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/live/sessions"

	payload, err := gptlive.Recording(t.Context(), gptlive.Config{URL: base, APIKey: "secret"}, "live_stored_1")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(payload[:4]) != "RIFF" || len(payload) != len(wav) {
		t.Errorf("the recording came back altered: %d bytes", len(payload))
	}
	if seenPath != "/v1/live/sessions/live_stored_1/content" {
		t.Errorf("the download went to %q", seenPath)
	}
	if seenAuth != "Bearer secret" {
		t.Errorf("the download carried Authorization %q", seenAuth)
	}
	if _, err := gptlive.Recording(t.Context(), gptlive.Config{URL: base, APIKey: "secret"}, "live_missing"); err == nil ||
		!strings.Contains(err.Error(), "no stored recording") {
		t.Errorf("a missing recording must fail with the endpoint's reason, got %v", err)
	}
	_ = time.Second
}
