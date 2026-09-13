package deepgram

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

func persistentListener(t *testing.T, fake *fakeDeepgram, keepAlive time.Duration) *Listener {
	t.Helper()
	listener, err := NewListener(ListenConfig{
		URL: fake.url(), APIKey: "key", Persistent: true,
		KeepAliveInterval: keepAlive, DrainTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// Two utterances, one socket. Finalize asks Deepgram to flush rather than
// close, the flushed results settle the utterance, and the next utterance's
// first frame goes down the stream that is already open - so it costs no dial,
// no handshake, and no warm-up.
func TestAPersistentStreamCarriesConsecutiveUtterancesOverOneConnection(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{
		results("hello", false), results("hello there", true),
		results("and again", false), results("and again please", true),
	})
	listener := persistentListener(t, fake, time.Hour)

	push(t, listener, 0, 0, 160)
	push(t, listener, 1, 160, 160)
	first, err := listener.Finalize(context.Background(), 320)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Final || first.StableText != "hello there" {
		t.Fatalf("first utterance = %+v", first)
	}
	if listener.SpeechEndpointed() {
		t.Fatal("endpoint state leaked out of the finished utterance")
	}

	// The next utterance starts its own frame and sample numbering, as every
	// utterance does; the stream underneath does not restart.
	push(t, listener, 0, 0, 160)
	push(t, listener, 1, 160, 160)
	second, err := listener.Finalize(context.Background(), 320)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Final || second.StableText != "and again please" {
		t.Fatalf("second utterance = %+v", second)
	}
	if second.Delta != "and again please" {
		t.Fatalf("second utterance delta carried the first utterance's text: %q", second.Delta)
	}
	if got := fake.accepts.Load(); got != 1 {
		t.Fatalf("connections dialled = %d, want 1", got)
	}
	if got := fake.finalizes.Load(); got != 2 {
		t.Fatalf("Finalize messages = %d, want 2", got)
	}
}

// A pause in the conversation is not the end of the stream. While no audio
// is being sent the adapter says so, or Deepgram closes the socket after ten
// silent seconds and the next word pays for a reconnect.
func TestAPersistentStreamIsKeptAliveWhileNobodySpeaks(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{results("hi", true)})
	listener := persistentListener(t, fake, 20*time.Millisecond)
	push(t, listener, 0, 0, 160)
	if _, err := listener.Finalize(context.Background(), 160); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fake.keepAlives.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fake.keepAlives.Load(); got < 2 {
		t.Fatalf("keepalives sent during silence = %d, want at least 2", got)
	}
	if got := fake.accepts.Load(); got != 1 {
		t.Fatalf("silence caused %d connections, want 1", got)
	}
}

// A stream that died between utterances is repaired at the next first frame,
// silently: the person hears nothing of it and the utterance is recognised on
// the new stream.
func TestAPersistentStreamReconnectsAfterTheServiceDropsIt(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{results("before", true), results("after", true)})
	listener := persistentListener(t, fake, time.Hour)
	push(t, listener, 0, 0, 160)
	if _, err := listener.Finalize(context.Background(), 160); err != nil {
		t.Fatal(err)
	}
	fake.dropConnections()
	deadline := time.Now().Add(2 * time.Second)
	for !listener.streamDead() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !listener.streamDead() {
		t.Fatal("the dropped stream was not noticed")
	}
	push(t, listener, 0, 0, 160)
	revision, err := listener.Finalize(context.Background(), 160)
	if err != nil {
		t.Fatal(err)
	}
	if revision.StableText != "after" {
		t.Fatalf("utterance after the drop = %+v", revision)
	}
	if got := fake.accepts.Load(); got != 2 {
		t.Fatalf("connections dialled = %d, want 2 (one repair)", got)
	}
}

// An abandoned utterance is flushed and forgotten, not carried into the next
// one's opening words, and it does not cost the stream.
func TestEndUtteranceDiscardsWhatDeepgramStillHeld(t *testing.T) {
	t.Parallel()
	fake := newFakeDeepgram(t, []string{results("cut off mid", false), results("cut off mid sentence", true), results("fresh", true)})
	listener := persistentListener(t, fake, time.Hour)
	push(t, listener, 0, 0, 160)
	push(t, listener, 1, 160, 160)
	if err := listener.EndUtterance(); err != nil {
		t.Fatal(err)
	}
	push(t, listener, 0, 0, 160)
	revision, err := listener.Finalize(context.Background(), 160)
	if err != nil {
		t.Fatal(err)
	}
	if revision.StableText != "fresh" || revision.Delta != "fresh" {
		t.Fatalf("utterance after an abandoned one = %+v", revision)
	}
	if got := fake.accepts.Load(); got != 1 {
		t.Fatalf("connections dialled = %d, want 1", got)
	}
	var _ v1.UtteranceReusable = listener
}

// streamDead is a test seam: whether the current stream's reader has stopped.
func (listener *Listener) streamDead() bool {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.stream == nil || listener.stream.dead()
}

// The final's confidence stays readable after Finalize: the language mux
// compares the lanes' finals by it, and a listener that cleared it with the
// rest of the utterance made every comparison 0 against 0.
func TestFinalizeKeepsTheFinalsConfidence(t *testing.T) {
	t.Parallel()
	withConfidence := func(transcript string, confidence float64) string {
		payload, _ := json.Marshal(map[string]any{
			"type": "Results", "is_final": true, "speech_final": true,
			"channel": map[string]any{"alternatives": []map[string]any{{"transcript": transcript, "confidence": confidence}}},
		})
		return string(payload)
	}
	fake := newFakeDeepgram(t, []string{results("你好", false), withConfidence("你好 很高兴见到你", 0.99)})
	listener := persistentListener(t, fake, time.Hour)
	push(t, listener, 0, 0, 160)
	push(t, listener, 1, 160, 160)
	final, err := listener.Finalize(context.Background(), 320)
	if err != nil || final.StableText != "你好 很高兴见到你" {
		t.Fatalf("final = %+v %v", final, err)
	}
	if got := listener.Confidence(); got != 0.99 {
		t.Fatalf("confidence after Finalize = %.2f, want the final's 0.99", got)
	}
}
