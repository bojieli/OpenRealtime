package gptlive_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gptlive"
)

// sendInternal sends one of the binding's internal events.
func sendInternal(t *testing.T, client *gptlive.Client, message map[string]any) {
	t.Helper()
	if err := client.Send(t.Context(), message); err != nil {
		t.Fatalf("send %v: %v", message["type"], err)
	}
}

// TestContextBecomesSilentThinking checks the silent hand-off: evidence the
// voice should know reaches it as thinking, never as something to say.
func TestContextBecomesSilentThinking(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	sendInternal(t, client, map[string]any{
		"type": gptlive.EventContext, "text": "Card field now filled, ends 4242. Expiry still empty.",
	})
	thinking := fake.awaitSent(t, "session.thinking.append")
	if got := decodeString(thinking["content"]); !strings.Contains(got, "ends 4242") {
		t.Errorf("thinking carried %q", got)
	}
	raw, ok := thinking["delegation_id"]
	if !ok || string(raw) != "null" {
		t.Errorf("session-wide context must carry delegation_id null, got %s (present=%v)", raw, ok)
	}
	fake.refuteSent(t, "session.commentary.append")
}

// TestSteerBecomesAnInstruction checks the steering channel.
func TestSteerBecomesAnInstruction(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	sendInternal(t, client, map[string]any{
		"type": gptlive.EventSteer, "text": "Do not continue or act on the last request; refuse briefly.",
	})
	instruction := fake.awaitSent(t, "session.instructions.append")
	if got := decodeString(instruction["content"]); !strings.Contains(got, "refuse briefly") {
		t.Errorf("instruction carried %q", got)
	}
	if string(instruction["delegation_id"]) != "null" {
		t.Errorf("steering is session-wide; delegation_id was %s", instruction["delegation_id"])
	}
}

// TestCancelTellsTheVoiceToStop checks that a Realtime cancel, which Live has
// no response to apply to, becomes the one thing Live can do: interrupt.
func TestCancelTellsTheVoiceToStop(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	sendInternal(t, client, map[string]any{"type": "response.cancel"})
	stop := fake.awaitSent(t, "session.instructions.append")
	if got := decodeString(stop["content"]); !strings.Contains(strings.ToLower(got), "stop speaking") {
		t.Errorf("cancel became %q, want an instruction to stop", got)
	}
}

// TestMuteAndUnmute checks the hearing hold.
func TestMuteAndUnmute(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	sendInternal(t, client, map[string]any{"type": gptlive.EventMute, "muted": true})
	fake.awaitSent(t, "session.input_audio.mute")
	sendInternal(t, client, map[string]any{"type": gptlive.EventMute, "muted": false})
	fake.awaitSent(t, "session.input_audio.unmute")
}

// TestALaterInstructionChangeIsAppended checks that a session.update after
// the handshake is reduced to what changed, and that an unchanged one is
// nothing at all.
func TestALaterInstructionChangeIsAppended(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	// The same instruction again is not a change.
	sendInternal(t, client, map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "Be brief."},
	})
	fake.refuteSent(t, "session.instructions.append")

	sendInternal(t, client, map[string]any{
		"type": "session.update", "session": map[string]any{"instructions": "Be brief, and speak French."},
	})
	appended := fake.awaitSent(t, "session.instructions.append")
	if got := decodeString(appended["content"]); got != "Be brief, and speak French." {
		t.Errorf("the changed instruction was appended as %q", got)
	}
	// And it is not a second session.start.
	starts := 0
	for _, message := range fake.sent() {
		if decodeString(message["type"]) == "session.start" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("session.start was sent %d times", starts)
	}
}

// TestTheVoiceComesFromTheSessionUpdate checks that a client's voice choice,
// forwarded by the binding, is what the session is opened with.
func TestTheVoiceComesFromTheSessionUpdate(t *testing.T) {
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
	message := fake.awaitSent(t, "session.start")
	var session struct {
		Store bool `json:"store"`
		Audio struct {
			Output struct {
				Voice string `json:"voice"`
			} `json:"output"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(message["session"], &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if session.Audio.Output.Voice != "cedar" {
		t.Errorf("session.start asked for voice %q, want the client's cedar", session.Audio.Output.Voice)
	}
	if !session.Store {
		t.Error("store was configured and session.start did not ask for it")
	}
}

// TestAStalledSessionIsNamed checks the watchdog: a started session that sends
// nothing is reported, and anything it sends resets the clock.
func TestAStalledSessionIsNamed(t *testing.T) {
	fake := newFakeLive(t)
	client, scheduler := connect(t, fake, func(config *gptlive.Config) {
		config.StallTimeout = 30 * time.Second
		config.FrameInterval = -1
	})
	start(t, fake, client, "Be brief.")

	// A heartbeat two thirds of the way through resets the clock.
	scheduler.AdvanceNS(uint64(20 * time.Second))
	fake.emit(map[string]any{"type": "session.usage.updated", "usage": map[string]any{"seconds": 20}})
	expect(t, client, "openrealtime.upstream.usage")
	scheduler.AdvanceNS(uint64(20 * time.Second))
	select {
	case event := <-client.Events():
		t.Fatalf("a stall was reported 20s after a heartbeat: %s", event.Type)
	case <-time.After(50 * time.Millisecond):
	}

	scheduler.AdvanceNS(uint64(15 * time.Second))
	failure := expect(t, client, "error")
	if !strings.Contains(string(failure.Raw), "upstream_stalled") {
		t.Errorf("the stall was not named: %s", failure.Raw)
	}
}

// TestAnUnrequestedCloseIsReported checks that a session the vendor ended is
// distinguished from one this side closed.
func TestAnUnrequestedCloseIsReported(t *testing.T) {
	fake := newFakeLive(t)
	client, _ := connect(t, fake, nil)
	start(t, fake, client, "Be brief.")

	fake.emit(map[string]any{
		"type": "session.closed", "reason": "expired", "usage": map[string]any{"seconds": 3600},
	})
	failure := expect(t, client, "error")
	if !strings.Contains(string(failure.Raw), "session_closed_expired") {
		t.Errorf("the close reason was lost: %s", failure.Raw)
	}
}
