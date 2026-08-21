package cascade_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Every policy in this file was constructed, validated, and named in the health
// report before it was ever consulted. These tests exist to say that consulting
// it changes what the session does - which is the only thing that makes a
// measured factor a factor rather than a label.

// --- turn projection --------------------------------------------------------

// projectingFloor ends the turn as soon as there is anything to project from.
type projectingFloor struct {
	projected atomic.Int64
}

func (floor *projectingFloor) Name() string      { return "projecting" }
func (floor *projectingFloor) EngineOwned() bool { return true }

func (floor *projectingFloor) Endpoint(decision interaction.Context) interaction.EndpointDecision {
	if decision.Revision.Empty() || decision.Revision.Final {
		return interaction.EndpointDecision{}
	}
	floor.projected.Add(1)
	return interaction.EndpointDecision{
		Ended: true, Projected: true, Reason: "test projection",
	}
}

func (floor *projectingFloor) Holder(session.Snapshot) interaction.Holder {
	return interaction.HolderNobody
}

// A projected endpoint has to close the turn. Before this was wired, the floor
// policy was asked nothing and the acoustic gate was the only endpoint there
// was, so turn projection could not shorten a turn by a millisecond.
func TestAProjectedEndpointClosesTheTurnBeforeSilenceDoes(t *testing.T) {
	floor := &projectingFloor{}
	policies := interaction.Defaults()
	policies.Floor = floor
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right away."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twenty", final: "transfer twenty"}, nil
		},
		Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})

	// Speak, and never stop: no silence, so the acoustic gate cannot end this.
	for index := 0; index < 6; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool { return floor.projected.Load() > 0 }, "the floor policy was never consulted")
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		for _, transcript := range sink.transcripts {
			if transcript.Final {
				return true
			}
		}
		return false
	}, "a projected endpoint did not end the turn")
}

// The shipped default must not project. Silence stays the authority unless a
// deployment asked for something else.
func TestTheDefaultFloorNeverProjects(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right away."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twenty", final: "transfer twenty"}, nil
		},
		Fast: fast, Slow: slow,
	}, binding.Settings{})

	for index := 0; index < 6; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	time.Sleep(150 * time.Millisecond)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, transcript := range sink.transcripts {
		if transcript.Final {
			t.Fatal("the default floor ended a turn nobody stopped speaking in")
		}
	}
}

// --- backchannel ------------------------------------------------------------

type eagerBackchannel struct {
	mu       sync.Mutex
	consults int
}

func (policy *eagerBackchannel) Name() string { return "model:test" }

func (policy *eagerBackchannel) Decide(
	_ context.Context, decision interaction.Context,
) (interaction.BackchannelDecision, error) {
	policy.mu.Lock()
	policy.consults++
	policy.mu.Unlock()
	if !decision.Duplex.UserSpeaking || decision.Duplex.AgentSpeaking || decision.Revision.Empty() {
		return interaction.BackchannelDecision{Choice: interaction.BackchannelNone}, nil
	}
	return interaction.BackchannelDecision{
		Choice: interaction.BackchannelAcknowledge, Token: "mm-hm",
	}, nil
}

// A backchannel policy that decides and emits nothing is a model call with no
// output. The token has to reach the world.
func TestABackchannelDecisionReachesTheWorld(t *testing.T) {
	policies := interaction.Defaults()
	policies.Backchannel = &eagerBackchannel{}
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right away."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "so the thing is", final: "so the thing is broken"}, nil
		},
		Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})

	for index := 0; index < 4; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	waitFor(t, func() bool {
		for _, spoken := range sink.spokenTexts() {
			if strings.Contains(spoken, "mm-hm") {
				return true
			}
		}
		return false
	}, "the continuer never reached the world")

	// And it is not conversational history: the model must not read its own
	// listening noise back as something it said.
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindAssistant && strings.Contains(item.Content, "mm-hm") {
			t.Fatal("a continuer must not be committed as assistant content")
		}
	}
}

// The shipped default is off, and off has to mean silent.
func TestNoBackchannelPolicyEmitsNothing(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right away."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Done."}})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "so the thing is", final: "so the thing is broken"}, nil
		},
		Fast: fast, Slow: slow,
	}, binding.Settings{})

	for index := 0; index < 4; index++ {
		pushAudio(t, runtime, tone(2400, 8000), 1)
	}
	time.Sleep(150 * time.Millisecond)
	if texts := sink.spokenTexts(); len(texts) != 0 {
		t.Fatalf("nothing should have been said while the user is talking, got %v", texts)
	}
	_ = runtime
}
