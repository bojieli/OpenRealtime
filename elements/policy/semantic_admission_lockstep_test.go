package policy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The policy runs in lockstep with the voice: a choice that invokes the model
// holds every later decision until the model has answered, the revisions that
// arrive meanwhile collapse to the latest, and the next decision is shown
// what the agent said. No two generations can ever run at once, and no step
// has to guess whether the one before it was answered.
func TestSemanticAdmissionRunsInLockstepWithTheVoice(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak", "keep"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
		Rules: "Classify this transcript.",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := mountSemanticAdmission(t, decider, config)
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	items := []trajectory.Item{
		semanticTranscriptObservation("count-final", "asr.endpoint", 1, "A capybara wandered over."),
		semanticTranscriptObservation("zebra-a", "asr.revision", 1, "Then a"),
		semanticTranscriptObservation("zebra-b", "asr.revision", 2, "Then a zebra ran past"),
	}
	streams := []string{"count-stream", "zebra-stream", "zebra-stream"}
	commit := func(index int) {
		snapshot := trajectory.Snapshot{
			Version: uint64(index + 1), Items: append([]trajectory.Item(nil), items[:index+1]...),
		}
		stateID := "state-" + string(rune('1'+index))
		sendSemanticContext(t, harness, stateID, snapshot)
		prefix, identifyErr := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if identifyErr != nil {
			t.Fatal(identifyErr)
		}
		outcome := semanticCommittedOutcome(items[index], streams[index], prefix, stateID, snapshot.Version)
		outcome.ObservationRevision = items[index].SourceRevision
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type:   stateelements.ObservationCommitOutcomeType(),
			ItemID: "commit-" + string(rune('1'+index)), SessionID: "semantic-session",
			Payload: outcome,
		})
	}
	state := func() policyelements.SemanticAdmissionState {
		return receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	}

	// The final is decided and the voice admitted; the runner now holds.
	commit(0)
	_ = state()
	first := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	if held := state(); !first.Choice.Speak || !held.Holding {
		t.Fatalf("admitted voice did not hold the runner: decision=%+v state=%+v", first, held)
	}

	// Two revisions of the next utterance arrive while the model is
	// answering: neither is decided, and the older one is superseded.
	commit(1)
	if pending := state(); pending.Pending != 1 || pending.Active || !pending.Holding {
		t.Fatalf("first revision was not held: %+v", pending)
	}
	commit(2)
	superseded := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	if superseded.Kind != policyelements.SemanticAdmissionCanceled || superseded.Code != "decision_superseded" ||
		superseded.SourceRevision != 1 {
		t.Fatalf("older revision was not superseded: %+v", superseded)
	}
	if pending := state(); pending.Pending != 1 || pending.Active || !pending.Holding {
		t.Fatalf("latest revision was not held: %+v", pending)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "decision"))
	if calls := len(decider.captured()); calls != 1 {
		t.Fatalf("decider was asked %d times during the hold, want 1", calls)
	}

	// The lifecycle sees the generation run, then answer. Only then is the
	// latest revision decided - shown the step that spoke and what it said.
	acknowledgeSemanticVoice(t, harness, coreinteraction.AgentOutput{
		Audible: true, Saying: "One.", InFlight: "voice output active: model=1",
	}, 2, 1)
	second := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	if released := state(); !second.Choice.Idle() || second.SourceRevision != 2 ||
		second.Event != coreinteraction.TranscriptPartial || outcome.Code != "keep" ||
		released.Holding || released.Pending != 0 {
		t.Fatalf("released decision=%+v outcome=%+v state=%+v", second, outcome, released)
	}
	captured := decider.captured()
	if len(captured) != 2 {
		t.Fatalf("decider calls = %d, want 2", len(captured))
	}
	evidence := captured[1].Evidence
	for _, want := range []string{
		"Recent steps",
		`final "A capybara wandered over." -> speak; agent said "One."`,
		`heard from user so far: "Then a zebra ran past"`,
	} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("the next step's evidence omits %q:\n%s", want, evidence)
		}
	}
	if strings.Contains(evidence, "Then a\"") {
		t.Fatalf("the superseded revision leaked into the evidence:\n%s", evidence)
	}
}
