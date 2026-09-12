package gptlive_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gptlive"
	"github.com/bojieli/OpenRealtime/pcm"
)

func tone8k(samples int) []byte {
	out := make([]byte, samples*2)
	for index := 0; index < samples; index++ {
		sample := int16(9000 * math.Sin(2*math.Pi*float64(index)*440/8000))
		out[index*2] = byte(uint16(sample))
		out[index*2+1] = byte(uint16(sample) >> 8)
	}
	return out
}

func decodeAudio(t *testing.T, message map[string]json.RawMessage, field string) []byte {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(decodeString(message[field]))
	if err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	return payload
}

// TestMuLawSessionCompandsBothWays is a telephone session: the caller's 24 kHz
// PCM is resampled and companded on the way in, and G.711 output is expanded
// and resampled on the way out, with the caller none the wiser.
func TestMuLawSessionCompandsBothWays(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.SessionFormat = "pcmu"
		config.FrameInterval = 20 * time.Millisecond
	})
	start(t, fake, client, "Be brief.")

	message := fake.awaitSent(t, "session.start")
	if !strings.Contains(string(message["session"]), `"type":"audio/pcmu"`) ||
		!strings.Contains(string(message["session"]), `"rate":8000`) {
		t.Fatalf("session.start did not ask for µ-law at 8 kHz: %s", message["session"])
	}

	// 100 ms of speech at 24 kHz becomes 100 ms of µ-law: 800 bytes, one per
	// sample, and not the codec's silence byte.
	if err := client.Send(t.Context(), map[string]any{
		"type": "input_audio_buffer.append", "audio": speechFrame(2400),
	}); err != nil {
		t.Fatalf("send audio: %v", err)
	}
	appended := fake.awaitSent(t, "session.input_audio.append")
	sent := decodeAudio(t, appended, "audio")
	if len(sent) < 780 || len(sent) > 820 {
		t.Errorf("100 ms at 24 kHz became %d µ-law bytes, want about 800", len(sent))
	}
	if bytes.Count(sent, []byte{0xFF}) > len(sent)/4 {
		t.Error("speech was companded as mostly silence")
	}

	// The keepalive is the codec's own silence, one byte per sample. The
	// first tick after the caller's frame is the caller's; the next is quiet.
	scheduler.AdvanceNS(uint64(20 * time.Millisecond))
	scheduler.AdvanceNS(uint64(20 * time.Millisecond))
	deadline := time.Now().Add(3 * time.Second)
	var silent []byte
	for silent == nil && time.Now().Before(deadline) {
		for _, message := range fake.sent() {
			if decodeString(message["type"]) != "session.input_audio.append" {
				continue
			}
			if payload := decodeAudio(t, message, "audio"); bytes.Equal(payload, bytes.Repeat([]byte{0xFF}, 160)) {
				silent = payload
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if silent == nil {
		t.Error("no 20 ms µ-law silence frame (160 bytes of 0xFF) reached the endpoint")
	}

	// Output: 30 ms of µ-law tone becomes 30 ms of 24 kHz PCM.
	fake.emit(map[string]any{
		"type":  "session.output_audio.delta",
		"delta": base64.StdEncoding.EncodeToString(pcm.EncodeMuLaw(tone8k(240))),
	})
	audio := expect(t, client, "response.output_audio.delta")
	decoded, err := base64.StdEncoding.DecodeString(field(t, audio.Raw, "delta"))
	if err != nil {
		t.Fatalf("decode forwarded audio: %v", err)
	}
	if samples := len(decoded) / 2; samples < 690 || samples > 750 {
		t.Errorf("240 µ-law samples became %d PCM samples at 24 kHz, want about 720", samples)
	}
	peak := 0
	for index := 0; index+1 < len(decoded); index += 2 {
		value := int(int16(uint16(decoded[index]) | uint16(decoded[index+1])<<8))
		if value < 0 {
			value = -value
		}
		peak = max(peak, value)
	}
	if peak < 4000 {
		t.Errorf("the expanded tone peaked at %d; it should be audible", peak)
	}
}

// TestALawSessionSpellsSilenceItsOwnWay checks the other law's start and
// its silence byte.
func TestALawSessionSpellsSilenceItsOwnWay(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.SessionFormat = "alaw"
		config.FrameInterval = 20 * time.Millisecond
	})
	start(t, fake, client, "Be brief.")
	message := fake.awaitSent(t, "session.start")
	if !strings.Contains(string(message["session"]), `"type":"audio/pcma"`) {
		t.Fatalf("session.start did not ask for A-law: %s", message["session"])
	}
	awaitFrameClock(t, fake, scheduler)
	found := false
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == "session.input_audio.append" {
			if payload := decodeAudio(t, message, "audio"); bytes.Equal(payload, bytes.Repeat([]byte{0xD5}, 160)) {
				found = true
			}
		}
	}
	if !found {
		t.Error("no A-law silence frame (160 bytes of 0xD5) reached the endpoint")
	}
}

// TestAnUnknownFormatIsRefused checks that a format the endpoint would
// reject is refused here, with the accepted ones named.
func TestAnUnknownFormatIsRefused(t *testing.T) {
	fake := newFakeLive(t)
	_, err := gptlive.Dial(t.Context(), gptlive.Config{URL: fake.url(), APIKey: "k", SessionFormat: "opus"})
	if err == nil || !strings.Contains(err.Error(), "pcmu") {
		t.Fatalf("an unknown format must be refused naming the accepted ones, got %v", err)
	}
}

// TestForkContinuesAStoredSession checks the explicit fork: the connection
// goes to the source session's fork endpoint, and the start carries only the
// audio format, since everything else is inherited from the recording.
func TestForkContinuesAStoredSession(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.ForkOf = "live_source_1"
		config.Store = true
	})
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "Be brief."},
	}); err != nil {
		t.Fatalf("send session.update: %v", err)
	}
	message := fake.awaitSent(t, "session.start")
	paths := fake.connectionPaths()
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/live_source_1/fork") {
		t.Fatalf("the fork was dialled at %v, want …/live_source_1/fork", paths)
	}
	var session map[string]json.RawMessage
	if err := json.Unmarshal(message["session"], &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	for _, inherited := range []string{"model", "instructions", "delegation"} {
		if _, present := session[inherited]; present {
			t.Errorf("a fork must inherit %s from the recording, but the start carried it", inherited)
		}
	}
	if _, present := session["audio"]; !present {
		t.Error("a WebSocket fork does not inherit its audio format and must declare one")
	}
	if string(session["store"]) != "true" {
		t.Error("the fork was asked to store and the start did not say so")
	}
}

// TestADroppedStoredSessionIsForkedAndContinues is the reconnect: the
// vendor's own recovery guidance, done for the caller without its knowledge.
func TestADroppedStoredSessionIsForkedAndContinues(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.Store = true
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")
	fake.awaitSent(t, "session.start")

	fake.hangUp()

	// A second connection arrives at the fork endpoint of the session that
	// dropped, with a start that inherits everything but the audio format.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(fake.connectionPaths()) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	paths := fake.connectionPaths()
	if len(paths) != 2 || !strings.HasSuffix(paths[1], "/live_abc123/fork") {
		t.Fatalf("expected a fork of live_abc123, connections went to %v", paths)
	}
	reconnected := expect(t, client, "openrealtime.upstream.reconnected")
	if got := field(t, reconnected.Raw, "from"); got != "live_abc123" {
		t.Errorf("the reconnect named %q as its source", got)
	}
	var starts []map[string]json.RawMessage
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == "session.start" {
			starts = append(starts, message)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("expected two session.start messages, got %d", len(starts))
	}
	if strings.Contains(string(starts[1]["session"]), `"model"`) {
		t.Errorf("the fork's start repeated the model: %s", starts[1]["session"])
	}

	fake.emit(map[string]any{
		"type": "session.started", "session": map[string]any{"id": "live_fork_9", "expires_at": 1789300000},
	})
	created := expect(t, client, "session.created")
	if !strings.Contains(string(created.Raw), "live_fork_9") {
		t.Errorf("the fork's new identity did not reach the caller: %s", created.Raw)
	}

	// The conversation carries on over the new connection.
	before := fake.countSent("session.input_audio.append")
	if err := client.Send(t.Context(), map[string]any{
		"type": "input_audio_buffer.append", "audio": speechFrame(480),
	}); err != nil {
		t.Fatalf("send audio after the fork: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fake.countSent("session.input_audio.append") == before {
		time.Sleep(5 * time.Millisecond)
	}
	if fake.countSent("session.input_audio.append") == before {
		t.Fatal("audio did not flow over the forked connection")
	}
	if client.SessionID() != "live_fork_9" {
		t.Errorf("the client still reports %q as its session", client.SessionID())
	}
}

// TestReconnectsAreBounded checks that a session that keeps dropping is
// eventually given up on rather than forked forever.
func TestReconnectsAreBounded(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) {
		config.Store = true
		config.MaxReconnects = 1
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")
	fake.awaitSent(t, "session.start")

	fake.hangUp()
	expect(t, client, "openrealtime.upstream.reconnected")
	fake.emit(map[string]any{"type": "session.started", "session": map[string]any{"id": "live_fork_1"}})
	expect(t, client, "session.created")

	fake.hangUp()
	select {
	case event, open := <-client.Events():
		if open {
			t.Fatalf("after the last allowed reconnect the stream must end, got %s", event.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never ended after the reconnect bound")
	}
	if len(fake.connectionPaths()) != 2 {
		t.Errorf("expected exactly two connections, got %v", fake.connectionPaths())
	}
}

// TestAnUnstoredSessionIsNotForked checks the default: with no recording to
// continue from, a drop ends the stream as it always did.
func TestAnUnstoredSessionIsNotForked(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")
	fake.hangUp()
	select {
	case _, open := <-client.Events():
		if open {
			t.Fatal("an unstored session produced an event after dropping")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never ended")
	}
	if paths := fake.connectionPaths(); len(paths) != 1 {
		t.Errorf("an unstored session was forked: %v", paths)
	}
}

// TestARefusedStoreDegradesToAnUnstoredSession reproduces the real endpoint
// on a project without data persistence: the start is refused over the store
// flag, and the session must run anyway - unstored, and saying so.
func TestARefusedStoreDegradesToAnUnstoredSession(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, func(config *gptlive.Config) { config.Store = true })
	if err := client.Send(t.Context(), map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"instructions": "Be brief.",
			"audio":        map[string]any{"output": map[string]any{"voice": "cedar"}},
		},
	}); err != nil {
		t.Fatalf("send session.update: %v", err)
	}
	first := fake.awaitSent(t, "session.start")
	if !strings.Contains(string(first["session"]), `"store":true`) {
		t.Fatalf("the first start did not ask to store: %s", first["session"])
	}
	fake.emit(map[string]any{
		"type": "error", "error": map[string]any{
			"type": "invalid_request_error", "code": "session_storage_not_allowed",
			"message": "Stored sessions require a project that permits data persistence.",
		},
	})
	info := expect(t, client, "openrealtime.upstream.info")
	if got := field(t, info.Raw, "code"); got != "storage_refused" {
		t.Fatalf("the refusal was reported as %q", got)
	}
	// A second start goes out, unstored, with the instruction and voice the
	// caller chose.
	deadline := time.Now().Add(3 * time.Second)
	var second map[string]json.RawMessage
	for time.Now().Before(deadline) && second == nil {
		var starts []map[string]json.RawMessage
		for _, message := range fake.sent() {
			if decodeString(message["type"]) == "session.start" {
				starts = append(starts, message)
			}
		}
		if len(starts) >= 2 {
			second = starts[1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	if second == nil {
		t.Fatal("no second session.start after the refusal")
	}
	if strings.Contains(string(second["session"]), `"store"`) {
		t.Errorf("the restart still asked to store: %s", second["session"])
	}
	if !strings.Contains(string(second["session"]), `"cedar"`) || !strings.Contains(string(second["session"]), "Be brief.") {
		t.Errorf("the restart lost the caller's voice or instruction: %s", second["session"])
	}
	fake.emit(map[string]any{"type": "session.started", "session": map[string]any{"id": "live_unstored"}})
	expect(t, client, "session.created")
	// And a later drop is not forked: there is nothing to fork from.
	fake.hangUp()
	select {
	case _, open := <-client.Events():
		if open {
			t.Fatal("an unstored session produced an event after dropping")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never ended")
	}
	if paths := fake.connectionPaths(); len(paths) != 1 {
		t.Errorf("a session the project would not store was forked anyway: %v", paths)
	}
}
