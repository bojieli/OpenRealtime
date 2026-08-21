// Package session owns the session core: the two facts every interaction
// policy depends on, and the identity and media plumbing a live session needs.
//
// The duplex state is the reason this package exists. Before it, "is the user
// speaking" lived in the gateway's VAD, "is the agent speaking" lived in the
// speech scheduler's active phase, and a state machine that modelled both sat
// unwired in a third package. Three answers to two questions is three chances
// to disagree, and every overlap decision depends on getting them right.
package session

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/internal/clock"
)

// Phase is the conversational configuration derived from the two facts. It is
// derived rather than stored, so it can never disagree with them.
type Phase string

const (
	// PhaseListening: neither party is producing audio.
	PhaseListening Phase = "listening"
	// PhaseUserSpeaking: the user is speaking and the agent is not.
	PhaseUserSpeaking Phase = "user_speaking"
	// PhaseAgentSpeaking: agent audio is reaching the user.
	PhaseAgentSpeaking Phase = "agent_speaking"
	// PhaseOverlap: both are producing audio. Whether that overlap is an
	// interruption, a backchannel, or side speech is an interaction policy
	// judgement, not a fact, and is decided elsewhere.
	PhaseOverlap Phase = "overlap"
)

// Snapshot is the complete duplex state at one instant.
//
// AgentSpeaking means audio is reaching the user right now. It does not mean a
// continuation is generating, and it does not mean speech was enqueued.
// Synthesised audio takes real time to play, and conflating the decision to
// speak with the user hearing us gets every overlap decision wrong.
type Snapshot struct {
	Phase         Phase `json:"phase"`
	UserSpeaking  bool  `json:"user_speaking"`
	AgentSpeaking bool  `json:"agent_speaking"`

	UserSpeechStartedNS uint64 `json:"user_speech_started_ns,omitempty"`
	UserSpeechEndedNS   uint64 `json:"user_speech_ended_ns,omitempty"`
	AgentAudioStartedNS uint64 `json:"agent_audio_started_ns,omitempty"`
	// PlayoutHorizonNS is when the audio already handed to the client finishes
	// playing. It is the server's honest estimate of what the user is hearing:
	// audio is on the wire and its duration has not yet elapsed.
	PlayoutHorizonNS uint64 `json:"playout_horizon_ns,omitempty"`
	AgentUtteranceID string `json:"agent_utterance_id,omitempty"`
	// Generation increments on every transition, so a policy can tell whether
	// the state it decided against is still the current one.
	Generation uint64 `json:"generation"`
}

// Overlapping reports whether both parties are producing audio.
func (snapshot Snapshot) Overlapping() bool { return snapshot.Phase == PhaseOverlap }

// Silent reports whether neither party is producing audio. It is the condition
// under which deferred work can run without talking over anyone.
func (snapshot Snapshot) Silent() bool { return snapshot.Phase == PhaseListening }

// TransitionKind names the four edges of the duplex state.
type TransitionKind string

const (
	UserSpeechStarted TransitionKind = "user_speech_started"
	UserSpeechStopped TransitionKind = "user_speech_stopped"
	AgentAudioStarted TransitionKind = "agent_audio_started"
	AgentAudioStopped TransitionKind = "agent_audio_stopped"
)

// Transition is one edge, with the state on both sides of it.
//
// Every deferral condition an interaction policy can express is a predicate
// over Snapshot, so every one of them clears on some transition here. That is
// what makes "every deferral has a wake-up" mechanically checkable rather than
// a discipline each policy has to remember.
type Transition struct {
	Kind   TransitionKind `json:"kind"`
	AtNS   uint64         `json:"at_ns"`
	Before Snapshot       `json:"before"`
	After  Snapshot       `json:"after"`
}

// Reason renders a transition as a wake-up reason.
func (transition Transition) Reason() string {
	switch transition.Kind {
	case UserSpeechStopped:
		return "user turn ended"
	case AgentAudioStopped:
		return "agent playback complete"
	case UserSpeechStarted:
		return "user speech started"
	case AgentAudioStarted:
		return "agent playback started"
	default:
		return string(transition.Kind)
	}
}

// Observer receives transitions. It is called without the duplex lock held, in
// transition order, and must not block: the wake-ups it drives are signals,
// not work.
type Observer func(Transition)

// DuplexConfig supplies the scheduler used to notice that playback finished.
type DuplexConfig struct {
	// Scheduler defaults to the system clock. Tests pass a manual one so a
	// playout horizon can be crossed deterministically.
	Scheduler clock.Scheduler
}

// Duplex is the single authoritative owner of the two facts.
type Duplex struct {
	mu        sync.Mutex
	scheduler clock.Scheduler
	state     Snapshot
	timer     clock.Timer
	closed    bool

	observerMu sync.RWMutex
	nextHandle uint64
	observers  map[uint64]Observer
}

// NewDuplex creates a duplex state in the listening phase.
func NewDuplex(config DuplexConfig) *Duplex {
	scheduler := config.Scheduler
	if scheduler == nil {
		scheduler = clock.NewSystem()
	}
	return &Duplex{
		scheduler: scheduler,
		state:     Snapshot{Phase: PhaseListening},
		observers: make(map[uint64]Observer),
	}
}

// Snapshot returns the current state.
func (duplex *Duplex) Snapshot() Snapshot {
	duplex.mu.Lock()
	defer duplex.mu.Unlock()
	return duplex.state
}

// Observe registers an observer and returns a function that removes it.
func (duplex *Duplex) Observe(observer Observer) func() {
	if observer == nil {
		return func() {}
	}
	duplex.observerMu.Lock()
	duplex.nextHandle++
	handle := duplex.nextHandle
	duplex.observers[handle] = observer
	duplex.observerMu.Unlock()
	return func() {
		duplex.observerMu.Lock()
		delete(duplex.observers, handle)
		duplex.observerMu.Unlock()
	}
}

// UserSpeechStarted records that the user began speaking. The source is the
// acoustic gate, or a model-native signal where the binding provides one.
// Repeated starts without an intervening stop are idempotent.
func (duplex *Duplex) UserSpeechStarted(atNS uint64) {
	duplex.transition(UserSpeechStarted, atNS, func(state *Snapshot) bool {
		if state.UserSpeaking {
			return false
		}
		state.UserSpeaking = true
		state.UserSpeechStartedNS = atNS
		return true
	})
}

// UserSpeechStopped records that the user stopped speaking.
func (duplex *Duplex) UserSpeechStopped(atNS uint64) {
	duplex.transition(UserSpeechStopped, atNS, func(state *Snapshot) bool {
		if !state.UserSpeaking {
			return false
		}
		state.UserSpeaking = false
		state.UserSpeechEndedNS = atNS
		return true
	})
}

// AgentAudioHandedOff records that audio of the given duration has been handed
// to the client for an utterance.
//
// This is the only way agent-speaking becomes true, which is the point: it is
// called by the code that puts frames on the wire, not by the code that
// decides to speak. The horizon extends from whichever is later - now, or the
// end of audio already handed off - so a stream that runs ahead of real time
// still reports playback ending when the user will actually stop hearing it.
func (duplex *Duplex) AgentAudioHandedOff(utteranceID string, duration time.Duration) error {
	if duration <= 0 {
		return errors.New("handed-off audio duration must be positive")
	}
	now := duplex.scheduler.NowNS()
	var transition Transition
	fired := false

	duplex.mu.Lock()
	if duplex.closed {
		duplex.mu.Unlock()
		return errors.New("duplex state is closed")
	}
	before := duplex.state
	horizon := duplex.state.PlayoutHorizonNS
	if horizon < now {
		horizon = now
	}
	horizon += uint64(duration.Nanoseconds())
	duplex.state.PlayoutHorizonNS = horizon
	if utteranceID != "" {
		duplex.state.AgentUtteranceID = utteranceID
	}
	if !duplex.state.AgentSpeaking {
		duplex.state.AgentSpeaking = true
		duplex.state.AgentAudioStartedNS = now
		duplex.state.Phase = derivePhase(duplex.state)
		duplex.state.Generation++
		transition = Transition{Kind: AgentAudioStarted, AtNS: now, Before: before, After: duplex.state}
		fired = true
	}
	duplex.rearmLocked(now)
	duplex.mu.Unlock()

	if fired {
		duplex.publish(transition)
	}
	return nil
}

// AgentAudioStopped ends agent playback immediately, whatever the horizon
// said. Barge-in cancellation uses it: frames that were never sent are not
// audio the user is hearing.
func (duplex *Duplex) AgentAudioStopped(atNS uint64) {
	duplex.transition(AgentAudioStopped, atNS, func(state *Snapshot) bool {
		if !state.AgentSpeaking {
			return false
		}
		state.AgentSpeaking = false
		state.PlayoutHorizonNS = 0
		state.AgentUtteranceID = ""
		return true
	})
}

// Close stops the playout watcher. It does not publish a transition: a closed
// session has no conversational state to report.
func (duplex *Duplex) Close() {
	duplex.mu.Lock()
	duplex.closed = true
	if duplex.timer != nil {
		duplex.timer.Stop()
		duplex.timer = nil
	}
	duplex.mu.Unlock()
}

// String renders the state for logs.
func (snapshot Snapshot) String() string {
	return fmt.Sprintf("%s(user=%t agent=%t gen=%d)", snapshot.Phase, snapshot.UserSpeaking, snapshot.AgentSpeaking, snapshot.Generation)
}

func (duplex *Duplex) transition(kind TransitionKind, atNS uint64, apply func(*Snapshot) bool) {
	duplex.mu.Lock()
	if duplex.closed {
		duplex.mu.Unlock()
		return
	}
	before := duplex.state
	next := duplex.state
	if !apply(&next) {
		duplex.mu.Unlock()
		return
	}
	next.Phase = derivePhase(next)
	next.Generation = before.Generation + 1
	duplex.state = next
	if kind == AgentAudioStopped && duplex.timer != nil {
		duplex.timer.Stop()
		duplex.timer = nil
	}
	duplex.mu.Unlock()
	duplex.publish(Transition{Kind: kind, AtNS: atNS, Before: before, After: next})
}

// rearmLocked schedules the check that notices the playout horizon passing.
// Without it, an agent that finishes speaking with the user silent would sit
// in agent-speaking forever and every deferral waiting on playback completion
// would wait forever with it.
func (duplex *Duplex) rearmLocked(now uint64) {
	if duplex.timer != nil {
		duplex.timer.Stop()
		duplex.timer = nil
	}
	if !duplex.state.AgentSpeaking {
		return
	}
	remaining := time.Duration(0)
	if duplex.state.PlayoutHorizonNS > now {
		remaining = time.Duration(duplex.state.PlayoutHorizonNS - now)
	}
	duplex.timer = duplex.scheduler.AfterFunc(remaining, duplex.playoutElapsed)
}

func (duplex *Duplex) playoutElapsed() {
	now := duplex.scheduler.NowNS()
	duplex.mu.Lock()
	duplex.timer = nil
	if duplex.closed || !duplex.state.AgentSpeaking {
		duplex.mu.Unlock()
		return
	}
	if duplex.state.PlayoutHorizonNS > now {
		// More audio arrived while the timer was in flight.
		duplex.rearmLocked(now)
		duplex.mu.Unlock()
		return
	}
	before := duplex.state
	next := duplex.state
	next.AgentSpeaking = false
	next.PlayoutHorizonNS = 0
	next.AgentUtteranceID = ""
	next.Phase = derivePhase(next)
	next.Generation = before.Generation + 1
	duplex.state = next
	duplex.mu.Unlock()
	duplex.publish(Transition{Kind: AgentAudioStopped, AtNS: now, Before: before, After: next})
}

func (duplex *Duplex) publish(transition Transition) {
	duplex.observerMu.RLock()
	observers := make([]Observer, 0, len(duplex.observers))
	for _, observer := range duplex.observers {
		observers = append(observers, observer)
	}
	duplex.observerMu.RUnlock()
	for _, observer := range observers {
		observer(transition)
	}
}

func derivePhase(state Snapshot) Phase {
	switch {
	case state.UserSpeaking && state.AgentSpeaking:
		return PhaseOverlap
	case state.UserSpeaking:
		return PhaseUserSpeaking
	case state.AgentSpeaking:
		return PhaseAgentSpeaking
	default:
		return PhaseListening
	}
}
