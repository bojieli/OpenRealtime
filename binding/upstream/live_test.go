package upstream_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// startLive opens the binding against the fake remote declared as GPT-Live.
// The fake still speaks Realtime; what changes is which of the binding's
// behaviours are in force, which is exactly what these tests are about.
func startLive(t *testing.T, remote *fakeRemote, slow continuation.Provider, adjust func(*upstream.Config)) (binding.Runtime, *collectingSink) {
	t.Helper()
	config := upstream.Config{
		URL: remote.url(), Slow: slow, Model: "gpt-live-1", Dialect: upstream.DialectGPTLive,
		ContextPushDebounce: 50 * time.Millisecond,
	}
	if adjust != nil {
		adjust(&config)
	}
	bind, err := upstream.New(config)
	if err != nil {
		t.Fatalf("new upstream: %v", err)
	}
	sink := &collectingSink{}
	runtime, err := bind.Start(context.Background(), binding.Options{Sink: sink, SessionID: "live"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background(), nil) })
	<-remote.ready
	return runtime, sink
}

func sentOfType(remote *fakeRemote, eventType string) []map[string]any {
	var matches []map[string]any
	for _, message := range remote.sent() {
		if message["type"] == eventType {
			matches = append(matches, message)
		}
	}
	return matches
}

func slowCalls(slow *scriptedSlow) int {
	slow.mu.Lock()
	defer slow.mu.Unlock()
	return slow.calls
}

// TestGatedSessionDeliberatesOnlyWhenTheVoiceAsks is decision D1: on an
// endpoint that delegates, a mirrored turn is evidence and a delegation is the
// request. The reasoner runs on the second, never on the first alone.
func TestGatedSessionDeliberatesOnlyWhenTheVoiceAsks(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "Order 4217 shipped yesterday."},
	}}}
	runtime, _ := startLive(t, remote, slow, nil)

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "hello there",
	})
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.Content == "hello there" {
				return true
			}
		}
		return false
	}, "the turn must be mirrored whether or not it is deliberated")
	time.Sleep(150 * time.Millisecond)
	if calls := slowCalls(slow); calls != 0 {
		t.Fatalf("the reasoner answered a turn the voice handled itself: %d run(s)", calls)
	}
	if len(sentOfType(remote, "response.create")) != 0 {
		t.Fatal("a hand-off went out with nothing to hand off")
	}

	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_2", "transcript": "where is my order 4217",
	})
	remote.emit(map[string]any{
		"type": "openrealtime.upstream.delegation", "delegation_id": "item_d1",
		"target": "client", "offset_ms": 3000,
	})
	waitFor(t, func() bool { return len(sentOfType(remote, "response.create")) == 1 },
		"the delegation must start the reasoner and its answer must be handed back")
	if calls := slowCalls(slow); calls != 1 {
		t.Fatalf("the reasoner ran %d times for one delegation", calls)
	}
	status := runtime.Status()
	if status.Remote == nil || status.Remote.OpenDelegation != "item_d1" {
		t.Fatalf("status does not show the open delegation: %+v", status.Remote)
	}
}

// TestUngatedSessionDeliberatesOnEveryTurn checks the switch, and that a
// Realtime endpoint keeps the behaviour it always had.
func TestUngatedSessionDeliberatesOnEveryTurn(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "Hello to you too."},
	}}}
	startLive(t, remote, slow, func(config *upstream.Config) { config.DelegationGating = upstream.GatingOff })
	remote.emit(map[string]any{
		"type":    "conversation.item.input_audio_transcription.completed",
		"item_id": "item_1", "transcript": "hello there",
	})
	waitFor(t, func() bool { return len(sentOfType(remote, "response.create")) == 1 },
		"with gating off every user turn is deliberated and handed back")
}

// TestObserverEvidenceIsPushedSilently is decision D2: evidence that is not
// the user goes to the voice as context, with no model call and no request to
// speak.
func TestObserverEvidenceIsPushedSilently(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{}
	runtime, _ := startLive(t, remote, slow, nil)

	if err := runtime.Text(context.Background(), binding.TextInput{
		Role: "system", Text: "Card field now filled, ends 4242. Expiry still empty.",
	}); err != nil {
		t.Fatalf("text: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.context") {
			if text, _ := message["text"].(string); strings.Contains(text, "ends 4242") {
				return true
			}
		}
		return false
	}, "observer evidence must reach the voice as context")
	if len(sentOfType(remote, "response.create")) != 0 {
		t.Fatal("context must never ask the voice to speak")
	}
	if calls := slowCalls(slow); calls != 0 {
		t.Fatalf("pushing evidence must not cost a model call, ran %d", calls)
	}
	var authority trajectory.Authority
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindObservation && strings.Contains(item.Content, "4242") {
			authority = trajectory.AuthorityOf(item)
		}
	}
	if authority != trajectory.AuthorityObserver {
		t.Fatalf("a client's system message must be observer authority, got %q", authority)
	}
}

// TestTheVoicesOwnWordsAreNotPushedBackToIt is the feedback-loop guard.
func TestTheVoicesOwnWordsAreNotPushedBackToIt(t *testing.T) {
	remote := newFakeRemote(t)
	startLive(t, remote, &scriptedSlow{}, nil)
	remote.emit(map[string]any{
		"type": "response.output_audio_transcript.done", "transcript": "Let me check that for you.",
	})
	time.Sleep(300 * time.Millisecond)
	for _, message := range sentOfType(remote, "openrealtime.upstream.context") {
		t.Fatalf("the voice was told what it just said: %v", message["text"])
	}
}

// TestTypedTextReachesTheReasonerAndTheVoice is decision D3: a typed message is
// committed for the reasoner, which runs even on a gated session because the
// voice never saw it, and the voice is told the user typed.
func TestTypedTextReachesTheReasonerAndTheVoice(t *testing.T) {
	remote := newFakeRemote(t)
	slow := &scriptedSlow{turns: [][]continuation.Event{{
		{Kind: continuation.EventAssistantDelta, Text: "Order 4217 is out for delivery."},
	}}}
	runtime, _ := startLive(t, remote, slow, nil)

	if err := runtime.Text(context.Background(), binding.TextInput{Text: "my order number is 4217"}); err != nil {
		t.Fatalf("text: %v", err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.Content == "my order number is 4217" &&
				trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
				return true
			}
		}
		return false
	}, "typed text must be committed as the user talking")
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.context") {
			if text, _ := message["text"].(string); strings.Contains(text, "typed") && strings.Contains(text, "4217") {
				return true
			}
		}
		return false
	}, "the voice must be told the user typed")
	waitFor(t, func() bool { return len(sentOfType(remote, "response.create")) == 1 },
		"the reasoner must answer typed input even on a gated session")
	if len(sentOfType(remote, "conversation.item.create")) != 1 {
		// Exactly the hand-off item, never the typed message forwarded as an
		// item: Live has no items, and the translator would drop it.
		t.Fatalf("expected one hand-off item, got %d", len(sentOfType(remote, "conversation.item.create")))
	}
}

// TestCancelSteersTheVoiceToStop is decision D5.
func TestCancelSteersTheVoiceToStop(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)
	if err := runtime.Cancel(context.Background(), "user interrupted"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitFor(t, func() bool {
		for _, message := range sentOfType(remote, "openrealtime.upstream.steer") {
			if text, _ := message["text"].(string); strings.Contains(strings.ToLower(text), "stop speaking") {
				return true
			}
		}
		return false
	}, "cancel must become an instruction to stop")
	if len(sentOfType(remote, "response.cancel")) != 0 {
		t.Fatal("response.cancel means nothing on Live and must not be sent")
	}
}

// TestUsageAndIdentityReachStatus is decision D7's visible half.
func TestUsageAndIdentityReachStatus(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)
	remote.emit(map[string]any{
		"type": "session.created", "session": map[string]any{"id": "live_x1", "expires_at": 1789210099},
	})
	remote.emit(map[string]any{
		"type": "openrealtime.upstream.usage", "seconds": 12.5, "context_window_ratio": 0.42,
	})
	waitFor(t, func() bool {
		status := runtime.Status().Remote
		return status != nil && status.SessionID == "live_x1" && status.UsageSeconds == 12.5 &&
			status.ContextWindowRatio == 0.42 && status.ExpiresAt == 1789210099
	}, "the remote's identity and meter must reach Status")
}

// TestContextIsSuppressedWhenTheWindowIsNearlyFull is decision D7's throttle.
func TestContextIsSuppressedWhenTheWindowIsNearlyFull(t *testing.T) {
	remote := newFakeRemote(t)
	runtime, _ := startLive(t, remote, &scriptedSlow{}, nil)
	remote.emit(map[string]any{"type": "openrealtime.upstream.usage", "seconds": 900, "context_window_ratio": 0.95})
	waitFor(t, func() bool {
		status := runtime.Status().Remote
		return status != nil && status.ContextWindowRatio == 0.95
	}, "usage must land first")
	if err := runtime.Text(context.Background(), binding.TextInput{Role: "system", Text: "Page changed."}); err != nil {
		t.Fatalf("text: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if len(sentOfType(remote, "openrealtime.upstream.context")) != 0 {
		t.Fatal("context was pushed into a window about to compact")
	}
	found := false
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindObservation && item.Content == "Page changed." {
			found = true
		}
	}
	if !found {
		t.Fatal("suppressing the push must not drop the evidence from the trajectory")
	}
}

// TestLiveCapabilitiesAreHonest is decision D8.
func TestLiveCapabilitiesAreHonest(t *testing.T) {
	remote := newFakeRemote(t)
	live, err := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if live.Capabilities().ManualTurns {
		t.Error("Live has no turn commit; a client must not be told it can take the floor")
	}
	realtime, err := upstream.New(upstream.Config{URL: remote.url(), Slow: &scriptedSlow{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if !realtime.Capabilities().ManualTurns {
		t.Error("a Realtime endpoint forwards the floor and must still say so")
	}
	if _, err := upstream.New(upstream.Config{
		URL: remote.url(), Slow: &scriptedSlow{}, Dialect: upstream.DialectGPTLive, FloorOwner: binding.OwnerEngine,
	}); err == nil {
		t.Error("an engine floor on Live must be refused, not accepted and ignored")
	}
	if _, err := upstream.ParseGating("sometimes"); err == nil {
		t.Error("an unknown gating level must be refused")
	}
}
