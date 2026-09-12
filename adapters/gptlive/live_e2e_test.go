package gptlive_test

// These tests run against the real GPT-Live endpoint. They are the evidence
// behind the catalogue's live-turn claim and behind every behaviour the fake
// cannot vouch for: whether an append is injected, whether the model delegates
// when told to, whether a hand-off is spoken. They cost a few cents of voice
// time and need a working credential, so they run only when asked:
//
//	OPENREALTIME_LIVE_E2E=1 OPENAI_API_KEY=... go test ./adapters/gptlive/ -run Live -v
//
// The spoken utterance that provokes a delegation is synthesised at test time
// through the vendor's own speech API rather than shipped as a fixture, so
// nothing here has a provenance the repository cannot state.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gptlive"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

func liveClient(t *testing.T, instruction string) *gptlive.Client {
	t.Helper()
	if os.Getenv("OPENREALTIME_LIVE_E2E") == "" {
		t.Skip("set OPENREALTIME_LIVE_E2E=1 to run against the real endpoint")
	}
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		t.Skip("OPENAI_API_KEY is not set")
	}
	client, err := gptlive.Dial(t.Context(), gptlive.Config{APIKey: key})
	if err != nil {
		t.Fatalf("dial GPT-Live: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": instruction},
	}); err != nil {
		t.Fatalf("send session.update: %v", err)
	}
	created := awaitLive(t, client, 15*time.Second, "session.created")
	t.Logf("live %s", created.Raw)
	return client
}

// awaitLive reads events until one of the wanted types arrives, failing on an
// error event or the deadline. Everything else is logged, because on a real
// endpoint what arrived in between is the evidence.
func awaitLive(t *testing.T, client *gptlive.Client, patience time.Duration, wanted ...string) realtimeclient.Event {
	t.Helper()
	deadline := time.After(patience)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("the stream closed while waiting for %v: %v", wanted, client.Err())
			}
			for _, want := range wanted {
				if event.Type == want {
					return event
				}
			}
			if event.Type == "error" {
				t.Fatalf("the endpoint reported an error while waiting for %v: %s", wanted, event.Raw)
			}
			if event.Type != "response.output_audio.delta" {
				t.Logf("  %s %s", event.Type, truncate(string(event.Raw), 140))
			}
		case <-deadline:
			t.Fatalf("no %v within %s", wanted, patience)
		}
	}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// speak synthesises one utterance as 24 kHz mono PCM16 through the vendor's
// speech API. The first model is the current one; the second is the one that
// has existed longest, for an account that lacks the first.
func speak(t *testing.T, text string) []byte {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	for _, model := range []string{"gpt-4o-mini-tts", "tts-1"} {
		body, _ := json.Marshal(map[string]any{
			"model": model, "input": text, "voice": "alloy", "response_format": "pcm",
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"https://api.openai.com/v1/audio/speech", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("synthesise with %s: %v", model, err)
		}
		payload, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode == http.StatusOK && len(payload) > 0 {
			t.Logf("synthesised %q with %s: %d bytes (%.1fs at 24 kHz)",
				text, model, len(payload), float64(len(payload))/2/24000)
			return payload
		}
		t.Logf("%s returned %d: %s", model, response.StatusCode, truncate(string(payload), 200))
	}
	t.Fatal("no speech model accepted the request")
	return nil
}

// stream paces PCM into the session the way a microphone would: 20 ms frames
// at wall-clock rate. Piping it all at once is not a conversation. It runs on
// its own goroutine, so a failure is recorded rather than fatal - the test
// that started it notices when what it was waiting for never arrives.
func stream(t *testing.T, client *gptlive.Client, pcm []byte) {
	const frame = 24000 / 1000 * 20 * 2
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for offset := 0; offset < len(pcm); offset += frame {
		end := min(offset+frame, len(pcm))
		if err := client.Send(t.Context(), map[string]any{
			"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm[offset:end]),
		}); err != nil {
			t.Errorf("stream audio: %v", err)
			return
		}
		<-ticker.C
	}
}

// TestLiveAppendsAreAcknowledged proves each push channel is injected, which
// the vendor says only the acknowledgement establishes.
func TestLiveAppendsAreAcknowledged(t *testing.T) {
	client := liveClient(t, "You are a probe. Say nothing unless asked.")

	sendInternal(t, client, map[string]any{
		"type": gptlive.EventContext, "text": "The user is looking at an order page. Nothing has been submitted.",
	})
	ack := awaitLive(t, client, 20*time.Second, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "of"); got != "session.thinking.appended" {
		t.Errorf("context was acknowledged as %q", got)
	}
	sendInternal(t, client, map[string]any{
		"type": gptlive.EventSteer, "text": "If the user greets you, greet back in one short sentence.",
	})
	ack = awaitLive(t, client, 20*time.Second, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "of"); got != "session.instructions.appended" {
		t.Errorf("steer was acknowledged as %q", got)
	}
	sendInternal(t, client, map[string]any{"type": gptlive.EventMute, "muted": true})
	ack = awaitLive(t, client, 20*time.Second, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "of"); got != "session.input_audio.muted" {
		t.Errorf("mute was acknowledged as %q", got)
	}
	sendInternal(t, client, map[string]any{"type": gptlive.EventMute, "muted": false})
	ack = awaitLive(t, client, 20*time.Second, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "of"); got != "session.input_audio.unmuted" {
		t.Errorf("unmute was acknowledged as %q", got)
	}
}

// TestLiveDelegationRoundTrip is the seam itself, end to end on the real
// endpoint: a spoken request the voice was told not to answer alone, the
// delegation it raises, the reasoner's answer handed back against that
// delegation, and the voice saying it.
func TestLiveDelegationRoundTrip(t *testing.T) {
	client := liveClient(t,
		"You are the voice of a parcel company. You cannot look anything up yourself. "+
			"Whenever the caller asks about an order, you must delegate to the backend and "+
			"tell the caller you are checking; never guess an order's status.")
	utterance := speak(t, "Hi there. Could you check the status of my order, number four two one seven?")

	go stream(t, client, utterance)

	delegation := awaitLive(t, client, 40*time.Second, "openrealtime.upstream.delegation")
	id := field(t, delegation.Raw, "delegation_id")
	if id == "" {
		t.Fatalf("delegation carried no id: %s", delegation.Raw)
	}
	t.Logf("the voice delegated: %s", delegation.Raw)

	// What the reasoner would produce, handed back through the portable pair.
	handOff(t, client, "Order 4217 shipped yesterday and will arrive tomorrow before noon.")
	ack := awaitLive(t, client, 20*time.Second, "openrealtime.upstream.ack")
	if got := field(t, ack.Raw, "of"); got != "session.commentary.appended" {
		t.Fatalf("the hand-off was acknowledged as %q", got)
	}

	var spoken strings.Builder
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-client.Events():
			switch event.Type {
			case "response.output_audio_transcript.delta":
				spoken.WriteString(field(t, event.Raw, "delta"))
			case "response.done":
				said := strings.ToLower(spoken.String())
				t.Logf("the voice said: %q", spoken.String())
				if !strings.Contains(said, "tomorrow") && !strings.Contains(said, "shipped") {
					t.Fatalf("the voice did not say the handed-off answer: %q", spoken.String())
				}
				return
			case "error":
				t.Fatalf("error while waiting for speech: %s", event.Raw)
			}
		case <-deadline:
			t.Fatalf("the voice never finished speaking; heard so far: %q", spoken.String())
		}
	}
}

// TestLiveCancelInterruptsSpeech asks for a long answer and stops it.
func TestLiveCancelInterruptsSpeech(t *testing.T) {
	client := liveClient(t, "You are a probe. When handed an answer, read all of it.")
	handOff(t, client, "Here is the full list of every parcel: "+strings.Repeat("parcel number one hundred, ", 30))
	awaitLive(t, client, 20*time.Second, "response.output_audio_transcript.delta")
	sendInternal(t, client, map[string]any{"type": "response.cancel"})
	// The hand-off's own acknowledgement may still be in flight; the one
	// wanted is the stop's, correlated by the id the translator gave it.
	awaitAck(t, client, "openrealtime_stop_1")
	done := awaitLive(t, client, 30*time.Second, "response.done")
	t.Logf("speech ended after the stop: %s", done.Raw)
}

// awaitAck reads until the acknowledgement for one command arrives.
func awaitAck(t *testing.T, client *gptlive.Client, clientEventID string) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("stream closed waiting for the ack of %s", clientEventID)
			}
			if event.Type == "openrealtime.upstream.ack" && field(t, event.Raw, "client_event_id") == clientEventID {
				return
			}
			if event.Type == "error" {
				t.Fatalf("error waiting for the ack of %s: %s", clientEventID, event.Raw)
			}
		case <-deadline:
			t.Fatalf("no acknowledgement of %s", clientEventID)
		}
	}
}

// TestLiveCloseIsFinalised checks that a session closed by this side is
// finalised by the vendor with usage, which is the billing record.
func TestLiveCloseIsFinalised(t *testing.T) {
	client := liveClient(t, "You are a probe.")
	awaitLive(t, client, 25*time.Second, "openrealtime.upstream.usage")
	started := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Logf("close returned in %s; session id %s", time.Since(started).Round(time.Millisecond), client.SessionID())
	if client.SessionID() == "" {
		t.Error("the session id was never recorded")
	}
}

// TestLiveMuLawSessionSpeaks opens a telephone-format session on the real
// endpoint: the hand-off is spoken and the G.711 audio that comes back
// expands to audible 24 kHz PCM for the caller.
func TestLiveMuLawSessionSpeaks(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_E2E") == "" {
		t.Skip("set OPENREALTIME_LIVE_E2E=1 to run against the real endpoint")
	}
	client, err := gptlive.Dial(t.Context(), gptlive.Config{
		APIKey: os.Getenv("OPENAI_API_KEY"), SessionFormat: "pcmu",
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "You are a probe. Say exactly what you are handed."},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	created := awaitLive(t, client, 15*time.Second, "session.created")
	t.Logf("µ-law session %s", created.Raw)
	handOff(t, client, "Telephone check: one two three.")
	frames, peak := 0, 0
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-client.Events():
			switch event.Type {
			case "response.output_audio.delta":
				frames++
				raw, _ := base64.StdEncoding.DecodeString(field(t, event.Raw, "delta"))
				for index := 0; index+1 < len(raw); index += 2 {
					value := int(int16(uint16(raw[index]) | uint16(raw[index+1])<<8))
					if value < 0 {
						value = -value
					}
					peak = max(peak, value)
				}
			case "response.done":
				t.Logf("%d audio frames after expansion, peak %d", frames, peak)
				if frames == 0 || peak < 2000 {
					t.Fatalf("the µ-law session produced no audible audio: %d frames, peak %d", frames, peak)
				}
				return
			case "error":
				t.Fatalf("error: %s", event.Raw)
			}
		case <-deadline:
			t.Fatalf("the voice never finished; %d frames so far", frames)
		}
	}
}

// TestLiveForkContinuesAStoredSession stores a session, closes it, and forks
// it on the real endpoint: the fork starts with a new id and speaks.
func TestLiveForkContinuesAStoredSession(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_E2E") == "" {
		t.Skip("set OPENREALTIME_LIVE_E2E=1 to run against the real endpoint")
	}
	key := os.Getenv("OPENAI_API_KEY")
	source, err := gptlive.Dial(t.Context(), gptlive.Config{APIKey: key, Store: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := source.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "You are a probe. Say exactly what you are handed."},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	skipIfStorageRefused(t, source)
	sourceID := source.SessionID()
	awaitLive(t, source, 25*time.Second, "openrealtime.upstream.usage")
	if err := source.Close(); err != nil {
		t.Fatalf("close the source: %v", err)
	}
	t.Logf("stored session %s closed", sourceID)

	fork, err := gptlive.Dial(t.Context(), gptlive.Config{APIKey: key, ForkOf: sourceID})
	if err != nil {
		t.Fatalf("dial the fork: %v", err)
	}
	t.Cleanup(func() { _ = fork.Close() })
	if err := fork.Send(t.Context(), map[string]any{"type": "session.update", "session": map[string]any{}}); err != nil {
		t.Fatalf("start the fork: %v", err)
	}
	created := awaitLive(t, fork, 20*time.Second, "session.created")
	t.Logf("fork %s", created.Raw)
	if fork.SessionID() == "" || fork.SessionID() == sourceID {
		t.Fatalf("the fork must have an id of its own, got %q from %q", fork.SessionID(), sourceID)
	}
	handOff(t, fork, "Fork check: the conversation continues.")
	var spoken strings.Builder
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event := <-fork.Events():
			switch event.Type {
			case "response.output_audio_transcript.delta":
				spoken.WriteString(field(t, event.Raw, "delta"))
			case "response.done":
				t.Logf("the fork said: %q", spoken.String())
				if !strings.Contains(strings.ToLower(spoken.String()), "continues") {
					t.Fatalf("the fork did not speak the hand-off: %q", spoken.String())
				}
				return
			case "error":
				t.Fatalf("error on the fork: %s", event.Raw)
			}
		case <-deadline:
			t.Fatalf("the fork never spoke; heard %q", spoken.String())
		}
	}
}

// TestLiveSidebandWatchesThePrimary attaches a second socket to a running
// session and sees what the primary's voice says, with reflected timing.
func TestLiveSidebandWatchesThePrimary(t *testing.T) {
	primary := liveClient(t, "You are a probe. Say exactly what you are handed.")
	sideband, err := gptlive.Attach(t.Context(), gptlive.Config{APIKey: os.Getenv("OPENAI_API_KEY")}, primary.SessionID())
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			// The vendor documents the sideband for sessions whose primary
			// connection is WebRTC or SIP - created by a server that then
			// attaches. A WebSocket primary already owns the session's
			// events, and the endpoint answers 404 to attaching to one. The
			// attach path is verified against the fake; this records why it
			// cannot be verified from here.
			t.Skipf("the endpoint does not offer a sideband on a WebSocket-primary session: %v", err)
		}
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { _ = sideband.Close() })

	handOff(t, primary, "Sideband check: the watcher hears this.")
	var heard strings.Builder
	var reflected int
	var timed bool
	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, open := <-sideband.Events():
			if !open {
				t.Fatalf("the sideband closed: %v", sideband.Err())
			}
			switch event.Type {
			case "response.output_audio_transcript.delta":
				heard.WriteString(field(t, event.Raw, "delta"))
			case "response.output_audio.delta":
				reflected++
				if strings.Contains(string(event.Raw), `"end_ms"`) {
					timed = true
				}
			case "response.done":
				t.Logf("the sideband heard %q over %d reflected frames (timed=%v)", heard.String(), reflected, timed)
				if !strings.Contains(strings.ToLower(heard.String()), "watcher") {
					t.Fatalf("the sideband did not see the primary's speech: %q", heard.String())
				}
				if reflected == 0 || !timed {
					t.Fatalf("reflected output must arrive with timing: %d frames, timed=%v", reflected, timed)
				}
				return
			case "error":
				t.Fatalf("sideband error: %s", event.Raw)
			}
		case <-deadline:
			t.Fatalf("the sideband never saw the speech end; heard %q", heard.String())
		}
	}
}

// TestLiveRecordingIsDownloaded stores a session, closes it, and fetches the
// recording. Under Zero Data Retention the vendor keeps nothing and this
// reports that rather than pretending.
func TestLiveRecordingIsDownloaded(t *testing.T) {
	if os.Getenv("OPENREALTIME_LIVE_E2E") == "" {
		t.Skip("set OPENREALTIME_LIVE_E2E=1 to run against the real endpoint")
	}
	key := os.Getenv("OPENAI_API_KEY")
	client, err := gptlive.Dial(t.Context(), gptlive.Config{APIKey: key, Store: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "You are a probe. Say exactly what you are handed."},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	skipIfStorageRefused(t, client)
	id := client.SessionID()
	handOff(t, client, "Recording check.")
	awaitLive(t, client, 30*time.Second, "response.done")
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var payload []byte
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		payload, err = gptlive.Recording(t.Context(), gptlive.Config{APIKey: key}, id)
		if err == nil {
			break
		}
		t.Logf("recording not ready: %v", err)
		time.Sleep(5 * time.Second)
	}
	if err != nil {
		t.Fatalf("the recording of %s never became available: %v", id, err)
	}
	t.Logf("recording of %s: %d bytes of WAV", id, len(payload))
}

// skipIfStorageRefused waits for the session to start and skips the test if
// the project refused to store it, which is the vendor's answer on a project
// without data persistence and not something a test can change.
func skipIfStorageRefused(t *testing.T, client *gptlive.Client) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("the stream closed before the session started: %v", client.Err())
			}
			switch event.Type {
			case "openrealtime.upstream.info":
				if field(t, event.Raw, "code") == "storage_refused" {
					t.Skip("this project does not permit data persistence; store, fork and recording cannot be exercised here")
				}
			case "session.created":
				return
			case "error":
				t.Fatalf("error before the session started: %s", event.Raw)
			}
		case <-deadline:
			t.Fatal("the session never started")
		}
	}
}

// TestLiveSteeringChangesWhatTheVoiceSaysNext is the control surface a running
// full-duplex agent has, and the one Full-Duplex-Bench does not reach.
//
// A Live session's instruction is fixed at startup; the only thing that
// changes its behaviour afterwards is an instruction append. Every guardrail,
// every standing instruction a user sets out loud, and every disclosure rides
// on that, so it has to actually take effect - not merely be acknowledged.
func TestLiveSteeringChangesWhatTheVoiceSaysNext(t *testing.T) {
	client := liveClient(t, "You are a probe. Say exactly what you are handed, and nothing else.")

	// Establish that it speaks normally first.
	handOff(t, client, "The weather today is fine.")
	awaitLive(t, client, 30*time.Second, "response.done")

	// A mid-conversation instruction with an unmistakable signature.
	const marker = "PINEAPPLE"
	sendInternal(t, client, map[string]any{
		"type": gptlive.EventSteer,
		"text": "New rule, effective immediately and for the rest of this conversation: " +
			"begin everything you say with the single word " + marker + ", then continue.",
	})
	awaitAck(t, client, "openrealtime_steer_1")

	// Now hand it something and listen for the rule being applied.
	handOff(t, client, "Your parcel arrives tomorrow.")
	var spoken strings.Builder
	deadline := time.After(40 * time.Second)
	for {
		select {
		case event, open := <-client.Events():
			if !open {
				t.Fatalf("the stream closed: %v", client.Err())
			}
			switch event.Type {
			case "response.output_audio_transcript.delta":
				spoken.WriteString(field(t, event.Raw, "delta"))
			case "response.done":
				said := spoken.String()
				t.Logf("after steering, the voice said: %q", said)
				if !strings.Contains(strings.ToUpper(said), marker) {
					t.Fatalf("the mid-conversation instruction did not take effect: %q", said)
				}
				return
			case "error":
				t.Fatalf("error: %s", event.Raw)
			}
		case <-deadline:
			t.Fatalf("no reply after steering; heard %q", spoken.String())
		}
	}
}
