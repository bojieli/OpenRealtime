package session_test

import (
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/session"
)

func record(duplex *session.Duplex) (*[]session.Transition, *sync.Mutex) {
	var mu sync.Mutex
	seen := make([]session.Transition, 0, 8)
	duplex.Observe(func(transition session.Transition) {
		mu.Lock()
		seen = append(seen, transition)
		mu.Unlock()
	})
	return &seen, &mu
}

func TestAgentSpeakingMeansAudioIsReachingTheUser(t *testing.T) {
	scheduler := clock.NewManual(0)
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: scheduler})
	defer duplex.Close()
	seen, mu := record(duplex)

	if !duplex.Snapshot().Silent() {
		t.Fatal("a new session is listening")
	}
	// Deciding to speak changes nothing here: only handing audio to the client
	// makes the agent audible.
	if err := duplex.AgentAudioHandedOff("utt-1", 300*time.Millisecond); err != nil {
		t.Fatalf("hand off audio: %v", err)
	}
	state := duplex.Snapshot()
	if !state.AgentSpeaking || state.Phase != session.PhaseAgentSpeaking {
		t.Fatalf("expected agent speaking, got %s", state)
	}
	if state.PlayoutHorizonNS != uint64(300*time.Millisecond) {
		t.Fatalf("unexpected playout horizon %d", state.PlayoutHorizonNS)
	}

	// Halfway through, the agent is still speaking.
	scheduler.AdvanceNS(uint64(150 * time.Millisecond))
	if !duplex.Snapshot().AgentSpeaking {
		t.Fatal("playback is not finished halfway through")
	}
	// The horizon passes with no external event at all.
	scheduler.AdvanceNS(uint64(200 * time.Millisecond))
	if duplex.Snapshot().AgentSpeaking {
		t.Fatal("playback must end when its audio has played out")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 2 ||
		(*seen)[0].Kind != session.AgentAudioStarted ||
		(*seen)[1].Kind != session.AgentAudioStopped {
		t.Fatalf("unexpected transitions %+v", *seen)
	}
	if (*seen)[1].Reason() != "agent playback complete" {
		t.Fatalf("unexpected wake-up reason %q", (*seen)[1].Reason())
	}
}

func TestContinuousAudioExtendsTheHorizonInsteadOfEndingPlayback(t *testing.T) {
	scheduler := clock.NewManual(0)
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: scheduler})
	defer duplex.Close()
	seen, mu := record(duplex)

	for frame := 0; frame < 5; frame++ {
		if err := duplex.AgentAudioHandedOff("utt-1", 100*time.Millisecond); err != nil {
			t.Fatalf("hand off frame %d: %v", frame, err)
		}
		scheduler.AdvanceNS(uint64(80 * time.Millisecond))
		if !duplex.Snapshot().AgentSpeaking {
			t.Fatalf("frame %d ended playback early", frame)
		}
	}
	// 500ms handed off, 400ms elapsed: 100ms of audio still to play.
	scheduler.AdvanceNS(uint64(99 * time.Millisecond))
	if !duplex.Snapshot().AgentSpeaking {
		t.Fatal("playback ended before the last frame finished")
	}
	scheduler.AdvanceNS(uint64(2 * time.Millisecond))
	if duplex.Snapshot().AgentSpeaking {
		t.Fatal("playback must end once the buffered audio has played")
	}
	mu.Lock()
	defer mu.Unlock()
	starts := 0
	for _, transition := range *seen {
		if transition.Kind == session.AgentAudioStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("continuous audio is one speaking turn, got %d starts", starts)
	}
}

func TestBargeInStopsPlaybackBeforeTheHorizon(t *testing.T) {
	scheduler := clock.NewManual(0)
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: scheduler})
	defer duplex.Close()

	if err := duplex.AgentAudioHandedOff("utt-1", time.Second); err != nil {
		t.Fatalf("hand off: %v", err)
	}
	duplex.UserSpeechStarted(scheduler.NowNS())
	state := duplex.Snapshot()
	if !state.Overlapping() {
		t.Fatalf("expected overlap, got %s", state)
	}
	// Cancellation drops the frames that were never sent.
	duplex.AgentAudioStopped(scheduler.NowNS())
	state = duplex.Snapshot()
	if state.Phase != session.PhaseUserSpeaking {
		t.Fatalf("expected user speaking after cancellation, got %s", state)
	}
	// The cancelled horizon must not resurrect playback later.
	scheduler.AdvanceNS(uint64(2 * time.Second))
	if duplex.Snapshot().AgentSpeaking {
		t.Fatal("a cancelled utterance must not come back")
	}
}

func TestPhaseIsDerivedFromTheTwoFacts(t *testing.T) {
	scheduler := clock.NewManual(0)
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: scheduler})
	defer duplex.Close()

	cases := []struct {
		apply func()
		want  session.Phase
	}{
		{func() { duplex.UserSpeechStarted(1) }, session.PhaseUserSpeaking},
		{func() { _ = duplex.AgentAudioHandedOff("a", time.Second) }, session.PhaseOverlap},
		{func() { duplex.UserSpeechStopped(2) }, session.PhaseAgentSpeaking},
		{func() { duplex.AgentAudioStopped(3) }, session.PhaseListening},
	}
	for index, testCase := range cases {
		testCase.apply()
		if got := duplex.Snapshot().Phase; got != testCase.want {
			t.Fatalf("case %d: expected %q, got %q", index, testCase.want, got)
		}
	}
}

func TestRepeatedEdgesAreIdempotent(t *testing.T) {
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: clock.NewManual(0)})
	defer duplex.Close()
	seen, mu := record(duplex)

	duplex.UserSpeechStarted(1)
	duplex.UserSpeechStarted(2)
	duplex.UserSpeechStopped(3)
	duplex.UserSpeechStopped(4)
	duplex.AgentAudioStopped(5)

	mu.Lock()
	defer mu.Unlock()
	if len(*seen) != 2 {
		t.Fatalf("expected two real transitions, got %d: %+v", len(*seen), *seen)
	}
	if duplex.Snapshot().Generation != 2 {
		t.Fatalf("generation must count real transitions only, got %d", duplex.Snapshot().Generation)
	}
}

func TestObserverCancellationStopsDelivery(t *testing.T) {
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: clock.NewManual(0)})
	defer duplex.Close()
	count := 0
	cancel := duplex.Observe(func(session.Transition) { count++ })
	duplex.UserSpeechStarted(1)
	cancel()
	duplex.UserSpeechStopped(2)
	if count != 1 {
		t.Fatalf("expected one delivery before cancellation, got %d", count)
	}
}

func TestHandOffRejectsNonPositiveDurationAndClosedState(t *testing.T) {
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: clock.NewManual(0)})
	if err := duplex.AgentAudioHandedOff("a", 0); err == nil {
		t.Fatal("expected zero-duration rejection")
	}
	duplex.Close()
	if err := duplex.AgentAudioHandedOff("a", time.Second); err == nil {
		t.Fatal("expected closed-state rejection")
	}
}
