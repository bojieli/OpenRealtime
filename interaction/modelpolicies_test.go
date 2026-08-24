package interaction_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
)

// recordingDecider captures exactly what a policy model was shown, which is
// what the decision-time-information rule has to be checked against.
type recordingDecider struct {
	mu         sync.Mutex
	seen       []interaction.Decision
	answer     string
	confidence float64
	err        error
}

func (decider *recordingDecider) Name() string { return "recording" }

func (decider *recordingDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	decider.mu.Lock()
	decider.seen = append(decider.seen, decision)
	answer, confidence, err := decider.answer, decider.confidence, decider.err
	decider.mu.Unlock()
	if err != nil {
		return interaction.Outcome{}, err
	}
	for index, option := range decision.Options {
		if option == answer {
			// Measured, because a double that reports a confidence is
			// standing in for a server that actually returned one.
			return interaction.Outcome{
				Index: index, Option: option, Confidence: confidence, Measured: true,
			}, nil
		}
	}
	return interaction.Outcome{}, nil
}

func (decider *recordingDecider) decisions() []interaction.Decision {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	return append([]interaction.Decision(nil), decider.seen...)
}

// A turn-projection model may see only what was available at the instant of
// the decision. Prompted on hindsight - the final transcript, what the user
// said next, whether the turn did end - it yields a judgement that cannot be
// reproduced online, which is the standard trap for a learned endpointer.
//
// This test is what makes that a property rather than an intention: it drives
// a projection at a moment when future text exists in the test's own scope and
// asserts that none of it reached the model.
func TestProjectionSeesOnlyDecisionTimeInformation(t *testing.T) {
	decider := &recordingDecider{answer: "finished", confidence: 0.9}
	projection, err := interaction.NewModelProjection(decider, interaction.ProjectionOptions{
		MinimumSilence: 50 * time.Millisecond, Confidence: 0.7,
	})
	if err != nil {
		t.Fatalf("new projection: %v", err)
	}

	const heardSoFar = "transfer forty dollars to"
	const whatCameNext = "account 8815 please, the one ending in five"
	const finalTranscript = heardSoFar + " " + whatCameNext

	result := projection.Project(interaction.Context{
		NowNS: uint64(3 * time.Second),
		Duplex: session.Snapshot{
			Phase: session.PhaseListening, UserSpeechStartedNS: uint64(time.Second),
		},
		Revision: interaction.Revision{
			ID: 7, StableText: heardSoFar, SilenceNS: uint64(150 * time.Millisecond),
		},
	})
	if !result.Ending {
		t.Fatalf("expected a projection, got %+v", result)
	}

	decisions := decider.decisions()
	if len(decisions) != 1 {
		t.Fatalf("expected one model call, got %d", len(decisions))
	}
	shown := decisions[0].Prompt + "\n" + decisions[0].Evidence
	if !strings.Contains(shown, heardSoFar) {
		t.Fatal("the model must see what was actually heard")
	}
	for _, hindsight := range []string{whatCameNext, finalTranscript, "8815"} {
		if strings.Contains(shown, hindsight) {
			t.Fatalf("the model was shown information from after the decision: %q", hindsight)
		}
	}
	// Neither may it be told the answer in another form.
	for _, leak := range []string{"the turn did end", "final transcript", "ground truth"} {
		if strings.Contains(strings.ToLower(shown), leak) {
			t.Fatalf("the prompt leaks hindsight: %q", leak)
		}
	}
}

func TestProjectionWaitsForSilenceAndCachesPerRevision(t *testing.T) {
	decider := &recordingDecider{answer: "finished", confidence: 0.95}
	projection, err := interaction.NewModelProjection(decider, interaction.ProjectionOptions{
		MinimumSilence: 100 * time.Millisecond, Confidence: 0.7,
	})
	if err != nil {
		t.Fatalf("new projection: %v", err)
	}
	speaking := interaction.Context{
		Duplex:   session.Snapshot{UserSpeaking: true, Phase: session.PhaseUserSpeaking},
		Revision: interaction.Revision{ID: 1, StableText: "hello", SilenceNS: uint64(time.Second)},
	}
	if projection.Project(speaking).Ending {
		t.Fatal("a projection must not fire while the user is audible")
	}
	tooEarly := interaction.Context{
		Revision: interaction.Revision{ID: 2, StableText: "hello", SilenceNS: uint64(10 * time.Millisecond)},
	}
	if projection.Project(tooEarly).Ending {
		t.Fatal("a projection must wait for enough silence to judge")
	}
	if len(decider.decisions()) != 0 {
		t.Fatal("the cheap checks must answer before a model is called")
	}

	ready := interaction.Context{
		Revision: interaction.Revision{ID: 3, StableText: "hello", SilenceNS: uint64(200 * time.Millisecond)},
	}
	if !projection.Project(ready).Ending {
		t.Fatal("expected a projection once there is enough silence")
	}
	if !projection.Project(ready).Ending {
		t.Fatal("the same revision must give the same answer")
	}
	if len(decider.decisions()) != 1 {
		t.Fatalf("one revision is one decision, got %d calls", len(decider.decisions()))
	}
}

func TestLowConfidenceProjectionsDoNotEndTurns(t *testing.T) {
	decider := &recordingDecider{answer: "finished", confidence: 0.4}
	projection, _ := interaction.NewModelProjection(decider, interaction.ProjectionOptions{
		MinimumSilence: 10 * time.Millisecond, Confidence: 0.8,
	})
	result := projection.Project(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "and then", SilenceNS: uint64(time.Second)},
	})
	if result.Ending {
		t.Fatal("cutting the user off on a guess is worse than waiting for silence")
	}
}

func TestAFailingProjectionModelFallsBackToSilence(t *testing.T) {
	decider := &recordingDecider{err: context.DeadlineExceeded}
	projection, _ := interaction.NewModelProjection(decider, interaction.ProjectionOptions{
		MinimumSilence: 10 * time.Millisecond,
	})
	result := projection.Project(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "hello", SilenceNS: uint64(time.Second)},
	})
	if result.Ending {
		t.Fatal("a failing policy model must not end a turn")
	}
}

func TestBackchannelAnswersCheaplyMostOfTheTime(t *testing.T) {
	decider := &recordingDecider{answer: "acknowledge", confidence: 0.9}
	backchannel, err := interaction.NewModelBackchannel(decider, interaction.BackchannelOptions{
		MinimumSpeech: time.Second, MinimumInterval: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("new backchannel: %v", err)
	}
	ctx := context.Background()

	// Not listening: no model call.
	if decision, _ := backchannel.Decide(ctx, interaction.Context{
		Duplex:   session.Snapshot{AgentSpeaking: true, Phase: session.PhaseAgentSpeaking},
		Revision: interaction.Revision{ID: 1, StableText: "hello"},
	}); decision.Choice != interaction.BackchannelNone {
		t.Fatal("the agent must not interject over itself")
	}
	// The user has only just started: no model call.
	if decision, _ := backchannel.Decide(ctx, interaction.Context{
		NowNS:    uint64(500 * time.Millisecond),
		Duplex:   session.Snapshot{UserSpeaking: true, UserSpeechStartedNS: uint64(400 * time.Millisecond)},
		Revision: interaction.Revision{ID: 2, StableText: "so"},
	}); decision.Choice != interaction.BackchannelNone {
		t.Fatal("a continuer half a second in is an interruption")
	}
	if len(decider.decisions()) != 0 {
		t.Fatalf("the cheap checks must answer first, got %d model calls", len(decider.decisions()))
	}

	listening := interaction.Context{
		NowNS:    uint64(5 * time.Second),
		Duplex:   session.Snapshot{UserSpeaking: true, UserSpeechStartedNS: uint64(time.Second)},
		Revision: interaction.Revision{ID: 3, StableText: "so I was thinking about the account"},
	}
	decision, err := backchannel.Decide(ctx, listening)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Choice != interaction.BackchannelAcknowledge || decision.Token != "mm-hm" {
		t.Fatalf("unexpected decision %+v", decision)
	}

	// Immediately after, the interval check answers without a model.
	before := len(decider.decisions())
	next := listening
	next.NowNS = uint64(6 * time.Second)
	next.Revision.ID = 4
	if again, _ := backchannel.Decide(ctx, next); again.Choice != interaction.BackchannelNone {
		t.Fatal("a continuer every second is not attentiveness")
	}
	if len(decider.decisions()) != before {
		t.Fatal("the interval check must answer without a model call")
	}
}

func TestBackchannelFailureIsSilenceNotAnError(t *testing.T) {
	decider := &recordingDecider{err: context.DeadlineExceeded}
	backchannel, _ := interaction.NewModelBackchannel(decider, interaction.BackchannelOptions{
		MinimumSpeech: time.Millisecond,
	})
	decision, err := backchannel.Decide(context.Background(), interaction.Context{
		NowNS:    uint64(time.Second),
		Duplex:   session.Snapshot{UserSpeaking: true, UserSpeechStartedNS: 1},
		Revision: interaction.Revision{ID: 1, StableText: "hello"},
	})
	if decision.Choice != interaction.BackchannelNone {
		t.Fatal("silence is always a safe answer when the policy model fails")
	}
	if err == nil {
		t.Fatal("the failure must still be reported so it can be seen")
	}
}

func TestPolicyModelsRequireADecider(t *testing.T) {
	if _, err := interaction.NewModelBackchannel(nil, interaction.BackchannelOptions{}); err == nil {
		t.Fatal("a model-backed policy needs a model")
	}
	if _, err := interaction.NewModelProjection(nil, interaction.ProjectionOptions{}); err == nil {
		t.Fatal("a model-backed policy needs a model")
	}
}

// answering is a decider that returns one option, with or without a measured
// confidence, so both halves of the asymmetry can be tested.
type answering struct {
	option     string
	confidence float64
	measured   bool
}

func (answering) Name() string { return "answering" }
func (decider answering) Decide(context.Context, interaction.Decision) (interaction.Outcome, error) {
	return interaction.Outcome{
		Option: decider.option, Confidence: decider.confidence, Measured: decider.measured,
	}, nil
}

// An endpoint that fires early cuts a person off mid-sentence, so it needs
// evidence. Holding one open costs latency the hold already bounds, so it does
// not - and requiring the same evidence for both spends the cheap failure to
// avoid the expensive one.
func TestEndingEarlyNeedsEvidenceAndWaitingDoesNot(t *testing.T) {
	unsure := interaction.ProjectionOptions{Confidence: 0.7, MinimumSilence: time.Millisecond}
	decision := interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "the order id is", SilenceNS: uint64(time.Second)},
	}

	early, err := interaction.NewModelProjection(answering{option: "finished", confidence: 0.4, measured: true}, unsure)
	if err != nil {
		t.Fatal(err)
	}
	if early.Project(decision).Ending {
		t.Error("a poorly evidenced endpoint must not cut the user off")
	}

	waiting, err := interaction.NewModelProjection(answering{option: "continuing", confidence: 0.4, measured: true}, unsure)
	if err != nil {
		t.Fatal(err)
	}
	if !waiting.Project(decision).Continuing {
		t.Error("waiting a moment longer does not need the same evidence")
	}
}

// A server that returns no log probabilities has said nothing about how sure
// the model was. Reading that as "not sure enough" discards a decision the
// model actually made - which is how turn projection ran for months without
// ever firing.
func TestAnUnmeasuredConfidenceIsNotALowOne(t *testing.T) {
	options := interaction.ProjectionOptions{Confidence: 0.7, MinimumSilence: time.Millisecond}
	policy, err := interaction.NewModelProjection(answering{option: "finished"}, options)
	if err != nil {
		t.Fatal(err)
	}
	projected := policy.Project(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "that is all", SilenceNS: uint64(time.Second)},
	})
	if !projected.Ending {
		t.Fatalf("an unmeasured answer is still an answer: %+v", projected)
	}
}
