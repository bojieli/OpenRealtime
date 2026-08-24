package interaction_test

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestDefaultsAreCompleteAndReportable(t *testing.T) {
	policies := interaction.Defaults()
	if err := policies.Validate(); err != nil {
		t.Fatalf("shipped defaults must be complete: %v", err)
	}
	report := policies.Report()
	if report.Trigger != "fixed-cadence-200ms" || report.Rollout != "fast+slow" ||
		report.Backchannel != "off" || report.TurnProjection != "vad-only" {
		t.Fatalf("unexpected default report %+v", report)
	}
	incomplete := policies
	incomplete.Rollout = nil
	if err := incomplete.Validate(); err == nil {
		t.Fatal("a missing policy must be an error rather than a silent default")
	}
}

func TestFixedCadenceRateLimitsPartialsButNeverTheEndpoint(t *testing.T) {
	trigger := interaction.NewFixedCadenceTrigger(200 * time.Millisecond)
	partial := func(id uint64, at uint64) interaction.Context {
		return interaction.Context{NowNS: at, Revision: interaction.Revision{ID: id, StableText: "hello"}}
	}
	if !trigger.Next(partial(1, 0)).Open {
		t.Fatal("the first revision opens an opportunity")
	}
	if trigger.Next(partial(2, uint64(100*time.Millisecond))).Open {
		t.Fatal("a revision inside the cadence must not open one")
	}
	if !trigger.Next(partial(3, uint64(250*time.Millisecond))).Open {
		t.Fatal("a revision after the cadence must open one")
	}
	final := interaction.Context{
		NowNS:    uint64(260 * time.Millisecond),
		Revision: interaction.Revision{ID: 4, StableText: "hello there", Final: true},
	}
	opportunity := trigger.Next(final)
	if !opportunity.Open || opportunity.Reason != "final revision" {
		t.Fatalf("the endpoint is never rate limited: %+v", opportunity)
	}
}

func TestEndpointTriggerOnlyFiresAtTheEndpoint(t *testing.T) {
	trigger := interaction.NewEndpointTrigger()
	if trigger.Next(interaction.Context{Revision: interaction.Revision{ID: 1, StableText: "partial"}}).Open {
		t.Fatal("endpoint-only must ignore partials")
	}
	if !trigger.Next(interaction.Context{Revision: interaction.Revision{ID: 2, StableText: "done", Final: true}}).Open {
		t.Fatal("endpoint-only must fire at the endpoint")
	}
}

func TestContinuousPreparationPacesOnlyTheSlowPhase(t *testing.T) {
	preparation := interaction.NewContinuousPreparation(500 * time.Millisecond)
	first := preparation.Prepare(interaction.Context{NowNS: 0, Revision: interaction.Revision{ID: 1, StableText: "a"}})
	if !first.Start || len(first.Phases) != 2 {
		t.Fatalf("the first revision starts both phases: %+v", first)
	}
	second := preparation.Prepare(interaction.Context{
		NowNS: uint64(100 * time.Millisecond), Revision: interaction.Revision{ID: 2, StableText: "ab"},
	})
	if !second.Start || len(second.Phases) != 1 || second.Phases[0] != trajectory.PhaseFast {
		t.Fatalf("a paced-out slow phase still prepares fast: %+v", second)
	}
	third := preparation.Prepare(interaction.Context{
		NowNS: uint64(700 * time.Millisecond), Revision: interaction.Revision{ID: 3, StableText: "abc"},
	})
	if len(third.Phases) != 2 {
		t.Fatalf("slow resumes once the pace has elapsed: %+v", third)
	}
	if preparation.Prepare(interaction.Context{
		NowNS: uint64(800 * time.Millisecond), Revision: interaction.Revision{ID: 3, StableText: "abc"},
	}).Start {
		t.Fatal("an unchanged revision must not re-prepare")
	}
}

// An observation is answered by the voice and examined by the reasoner, in
// that order.
//
// Deliberation used to wait for the voice to decline to mark the turn
// complete, which put every capability the agent has behind one judgement by
// the phase that cannot act on it. The voice still decides whether it has
// finished speaking; whether anything remains to be done is decided by looking.
func TestAnObservationIsAnsweredNowAndExaminedAnyway(t *testing.T) {
	rollout := interaction.NewFastThenSlowRollout(interaction.RolloutOptions{})
	plan := rollout.Plan(interaction.RolloutInput{Cause: interaction.Cause{Observation: true}})
	if len(plan) != 2 {
		t.Fatalf("an observation answers and deliberates: %+v", plan)
	}
	if plan[0].Kind != interaction.StepFast || plan[1].Kind != interaction.StepSlow {
		t.Fatalf("the voice goes first, or the user waits on the reasoner: %+v", plan)
	}
	escalated := rollout.Plan(interaction.RolloutInput{Cause: interaction.Cause{Escalated: true}})
	if len(escalated) != 1 || escalated[0].Kind != interaction.StepSlow {
		t.Fatalf("deliberation runs when fast hands the turn on: %+v", escalated)
	}
	spoken := rollout.Plan(interaction.RolloutInput{Cause: interaction.Cause{BackgroundResult: true}})
	if len(spoken) != 1 || spoken[0].Kind != interaction.StepFast {
		t.Fatalf("a finished background result is spoken by a fast turn: %+v", spoken)
	}
	if spoken[0].Phase() != trajectory.PhaseFast {
		t.Fatal("the only phase that speaks is fast")
	}
	bounded := rollout.Plan(interaction.RolloutInput{
		Cause: interaction.Cause{Escalated: true, SlowInvocations: 8},
	})
	if len(bounded) != 0 {
		t.Fatalf("the slow invocation bound must stop the chain: %+v", bounded)
	}
}

func TestToolResultProgressIsARolloutLever(t *testing.T) {
	quiet := interaction.NewFastThenSlowRollout(interaction.RolloutOptions{})
	plan := quiet.Plan(interaction.RolloutInput{Cause: interaction.Cause{ToolResult: true}})
	if len(plan) != 1 || plan[0].Kind != interaction.StepSlow {
		t.Fatalf("without progress reporting a tool result resumes slow only: %+v", plan)
	}
	talkative := interaction.NewFastThenSlowRollout(interaction.RolloutOptions{ToolResultProgress: true})
	plan = talkative.Plan(interaction.RolloutInput{Cause: interaction.Cause{ToolResult: true}})
	if len(plan) != 2 || plan[0].Kind != interaction.StepFast || plan[0].Reason != "report progress" {
		t.Fatalf("with it, the fast provider reports what came back: %+v", plan)
	}
	if talkative.Name() != "fast+slow+progress" {
		t.Fatalf("the lever must be visible in the policy name, got %q", talkative.Name())
	}
}

func TestParallelBranchAnswersWithoutStartingSlowWork(t *testing.T) {
	rollout := interaction.NewFastThenSlowRollout(interaction.RolloutOptions{})
	plan := rollout.Plan(interaction.RolloutInput{Cause: interaction.Cause{Observation: true, Parallel: true}})
	if len(plan) != 1 || plan[0].Kind != interaction.StepFast {
		t.Fatalf("a parallel branch answers and stops: %+v", plan)
	}
}

func TestRolloutLevelsParse(t *testing.T) {
	for _, level := range []string{"fast-only", "fast+slow", "endpointed-slow-only"} {
		rollout, err := interaction.ParseRollout(level, interaction.RolloutOptions{})
		if err != nil {
			t.Fatalf("parse %q: %v", level, err)
		}
		if rollout.Name() == "" {
			t.Fatalf("level %q has no name", level)
		}
	}
	if _, err := interaction.ParseRollout("magic", interaction.RolloutOptions{}); err == nil {
		t.Fatal("expected an unknown level to be rejected")
	}
}

func TestEngineFloorEndpointsOnSilenceAndProjection(t *testing.T) {
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{SilenceDuration: 500 * time.Millisecond})
	if floor.Endpoint(interaction.Context{
		Duplex:   session.Snapshot{UserSpeaking: true},
		Revision: interaction.Revision{ID: 1, StableText: "hello"},
	}).Ended {
		t.Fatal("a speaking user has not finished")
	}
	decision := floor.Endpoint(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "hello", SilenceNS: uint64(600 * time.Millisecond)},
	})
	if !decision.Ended || decision.Projected {
		t.Fatalf("silence ends the turn without projecting it: %+v", decision)
	}
	if !floor.EngineOwned() {
		t.Fatal("the engine floor is engine owned")
	}
}

type eagerProjection struct{}

func (eagerProjection) Name() string { return "eager" }
func (eagerProjection) Project(interaction.Context) interaction.Projection {
	return interaction.Projection{Ending: true, Confidence: 0.9, Reason: "projected"}
}

func TestProjectedEndpointIsRecordedAsProjected(t *testing.T) {
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{Projection: eagerProjection{}})
	decision := floor.Endpoint(interaction.Context{Revision: interaction.Revision{ID: 1, StableText: "hello"}})
	if !decision.Ended || !decision.Projected {
		t.Fatalf("a projected endpoint must be marked as one: %+v", decision)
	}
}

func TestModelFloorKeepsTheTurnThroughOverlap(t *testing.T) {
	floor := interaction.NewModelFloor("moshi")
	if floor.EngineOwned() {
		t.Fatal("a model floor is not engine owned")
	}
	holder := floor.Holder(session.Snapshot{UserSpeaking: true, AgentSpeaking: true, Phase: session.PhaseOverlap})
	if holder != interaction.HolderAgent {
		t.Fatalf("the engine must not overrule a model it delegated to, got %q", holder)
	}
}

func TestBargeInRespectsTypedOverlapEvidence(t *testing.T) {
	overlap := session.Snapshot{UserSpeaking: true, AgentSpeaking: true, Phase: session.PhaseOverlap}
	immediate := interaction.NewImmediateBargeIn()
	if !immediate.Decide(interaction.BargeInInput{Context: interaction.Context{Duplex: overlap}}).Cancel {
		t.Fatal("unclassified overlap yields the floor")
	}
	backchannel := immediate.Decide(interaction.BargeInInput{
		Context: interaction.Context{Duplex: overlap}, Evidence: interaction.OverlapBackchannel,
	})
	if backchannel.Cancel {
		t.Fatal("a listener backchannel must not cut the agent off")
	}

	sustained := interaction.NewSustainedBargeIn(300 * time.Millisecond)
	early := sustained.Decide(interaction.BargeInInput{
		Context: interaction.Context{Duplex: overlap}, OverlapNS: uint64(100 * time.Millisecond),
	})
	if early.Cancel {
		t.Fatal("a brief overlap must not cancel under the sustained policy")
	}
	late := sustained.Decide(interaction.BargeInInput{
		Context: interaction.Context{Duplex: overlap}, OverlapNS: uint64(400 * time.Millisecond),
	})
	if !late.Cancel {
		t.Fatal("a sustained overlap must cancel")
	}

	// The hold is a maximum, not a minimum: once something has said the
	// overlap is directed, waiting out the rest of it is talking over somebody
	// who is already interrupting.
	classified := sustained.Decide(interaction.BargeInInput{
		Context: interaction.Context{Duplex: overlap}, OverlapNS: uint64(50 * time.Millisecond),
		Evidence: interaction.OverlapDirected,
	})
	if !classified.Cancel {
		t.Fatal("directed evidence must yield the floor without waiting out the hold")
	}
}

func TestCommitmentRefusesToVoiceSilentProducers(t *testing.T) {
	for _, policy := range []interaction.Commitment{
		interaction.NewCompleteCommitment(),
		interaction.NewSentenceCommitment(4),
	} {
		decision := policy.Decide(interaction.CommitmentInput{
			Text: "The balance is forty dollars.", Complete: true, SpeechAuthority: "silent",
		})
		if decision.Committed() {
			t.Fatalf("%s voiced a silent producer", policy.Name())
		}
	}
}

func TestSentenceCommitmentEmitsCompletedSentences(t *testing.T) {
	policy := interaction.NewSentenceCommitment(4)
	decision := policy.Decide(interaction.CommitmentInput{Text: "Checking now. Let me see", Complete: false})
	if decision.Emit != "Checking now." || decision.Hold != " Let me see" {
		t.Fatalf("unexpected split %+v", decision)
	}
	partial := policy.Decide(interaction.CommitmentInput{Text: "Checking", Complete: false})
	if partial.Committed() {
		t.Fatalf("an unfinished sentence is held: %+v", partial)
	}
}

func TestRepairRequiresACorrectionOnlyForAudioThatWasHeard(t *testing.T) {
	policy := interaction.NewAudibleRepair()
	if got := policy.Decide(interaction.RepairInput{PlayedAudioMS: 0}); got.Action != interaction.RepairNone {
		t.Fatalf("nothing heard is not a repair: %+v", got)
	}
	if got := policy.Decide(interaction.RepairInput{PlayedAudioMS: 400}); got.Action != interaction.RepairSpeak {
		t.Fatalf("heard audio requires an audible correction: %+v", got)
	}
	threshold := interaction.NewAudibleRepairAfter(500 * time.Millisecond)
	if got := threshold.Decide(interaction.RepairInput{PlayedAudioMS: 100}); got.Action != interaction.RepairSilent {
		t.Fatalf("a fragment below the threshold is recorded, not spoken: %+v", got)
	}
}

// The wake-up must be wired from the policy's own declared conditions, so a
// condition cannot be deferred on without something eventually releasing it.
func TestBindWiresAWakeUpForEveryDeclaredCondition(t *testing.T) {
	scheduler := clock.NewManual(0)
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: scheduler})
	defer duplex.Close()
	store := trajectory.NewStore()
	var counter atomic.Uint64
	var ran atomic.Int64
	coordinator, err := eventloop.New(eventloop.Config{
		Store: store, MaxPendingEvents: 16,
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(context.Context, eventloop.Batch) error {
			ran.Add(1)
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("new coordinator: %v", err)
	}
	gate, err := interaction.Bind(interaction.NewDuplexDeferral(interaction.DeferralOptions{}), duplex, coordinator)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer gate.Close()
	coordinator, err = eventloop.New(eventloop.Config{
		Store: store, Gate: gate, MaxPendingEvents: 16,
		NextID: func(prefix string) string { return prefix + "-" + strconv.FormatUint(counter.Add(1), 10) },
		Processor: eventloop.ProcessorFunc(func(context.Context, eventloop.Batch) error {
			ran.Add(1)
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("rebuild coordinator: %v", err)
	}
	gate2, err := interaction.Bind(interaction.NewDuplexDeferral(interaction.DeferralOptions{}), duplex, coordinator)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer gate2.Close()

	// The agent is speaking: a tool result commits and waits.
	if err := duplex.AgentAudioHandedOff("utt-1", 400*time.Millisecond); err != nil {
		t.Fatalf("hand off: %v", err)
	}
	if _, err := coordinator.Submit(eventloop.Event{
		Type: "asr.endpoint", Source: "asr", Channel: "voice", Priority: eventloop.PriorityRoutine,
		Kind: trajectory.KindObservation, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "and the balance?",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := coordinator.RunNext(context.Background()); !errors.Is(err, eventloop.ErrDeferred) {
		t.Fatalf("expected a deferral while the agent is audible, got %v", err)
	}
	if ran.Load() != 0 {
		t.Fatal("deferred work must not have run")
	}

	// Nothing external happens: playback simply finishes.
	drained := make(chan struct{})
	go func() {
		<-coordinator.Signal()
		close(drained)
	}()
	scheduler.AdvanceNS(uint64(500 * time.Millisecond))
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("playback completion did not wake the deferred work")
	}
	if _, err := coordinator.RunNext(context.Background()); err != nil {
		t.Fatalf("run after wake-up: %v", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("expected exactly one run, got %d", ran.Load())
	}
}

func TestBackpressureIsOffByDefaultAndWakesOnRelief(t *testing.T) {
	duplex := session.NewDuplex(session.DuplexConfig{Scheduler: clock.NewManual(0)})
	defer duplex.Close()
	var woke atomic.Int64
	waker := wakerFunc(func(string) { woke.Add(1) })

	off, err := interaction.Bind(interaction.NewDuplexDeferral(interaction.DeferralOptions{}), duplex, waker)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	off.SetBackpressure(true)
	if admitted, reason := off.AdmitRun(context.Background(), eventloop.Batch{}); !admitted {
		t.Fatalf("backpressure must not throttle by default, got %q", reason)
	}

	on, err := interaction.Bind(
		interaction.NewDuplexDeferral(interaction.DeferralOptions{DeferUnderBackpressure: true}), duplex, waker)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	on.SetBackpressure(true)
	if admitted, reason := on.AdmitRun(context.Background(), eventloop.Batch{}); admitted || reason != "provider backpressure" {
		t.Fatalf("expected a backpressure deferral, got admitted=%v %q", admitted, reason)
	}
	before := woke.Load()
	on.SetBackpressure(false)
	if woke.Load() != before+1 {
		t.Fatal("relief owes exactly one wake-up")
	}
}

type wakerFunc func(string)

func (function wakerFunc) Wake(reason string) { function(reason) }

func TestAlwaysRunNeedsNoWakeUps(t *testing.T) {
	policy := interaction.AlwaysRun{}
	if conditions := policy.Conditions(); len(conditions) != 0 {
		t.Fatalf("a policy that never defers declares no conditions, got %v", conditions)
	}
	if admitted, _ := policy.Admit(interaction.Waiting{
		Duplex: session.Snapshot{UserSpeaking: true, AgentSpeaking: true},
	}); !admitted {
		t.Fatal("always-run admits everything")
	}
}

// patientProjection always says the person is mid-thought, which is the shape
// of a projection model that is wrong in the expensive direction.
type patientProjection struct{}

func (patientProjection) Name() string { return "patient" }
func (patientProjection) Project(interaction.Context) interaction.Projection {
	return interaction.Projection{Continuing: true, Confidence: 0.9, Reason: "still going"}
}

// Silence past the threshold is what ends a turn, and a disfluent speaker
// produces plenty of it mid-sentence. The projection model is asked whether
// the person has finished; when it says they have not, that answer has to
// reach the decision or the pause is endpointed anyway.
func TestAProjectedPauseHoldsTheTurnOpen(t *testing.T) {
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{
		SilenceDuration: 500 * time.Millisecond,
		ProjectionHold:  time.Second,
		Projection:      patientProjection{},
	})
	decision := floor.Endpoint(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "the order ID is", SilenceNS: uint64(600 * time.Millisecond)},
	})
	if decision.Ended {
		t.Fatalf("silence past the threshold ended a turn the projection called unfinished: %+v", decision)
	}
	if !decision.Projected {
		t.Fatalf("a held endpoint is a projected decision and must say so: %+v", decision)
	}
}

// The hold is a maximum. A model that keeps answering "still going" is asked
// again on every revision, so without a bound it would hold the floor for as
// long as it kept saying so, and the turn would never end.
func TestAProjectedPauseCannotHoldTheTurnForever(t *testing.T) {
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{
		SilenceDuration: 500 * time.Millisecond,
		ProjectionHold:  time.Second,
		Projection:      patientProjection{},
	})
	for _, silence := range []time.Duration{1400 * time.Millisecond, 1499 * time.Millisecond} {
		if floor.Endpoint(interaction.Context{
			Revision: interaction.Revision{ID: 1, StableText: "the order ID is", SilenceNS: uint64(silence)},
		}).Ended {
			t.Fatalf("%s is inside the hold and must not end the turn", silence)
		}
	}
	decision := floor.Endpoint(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "the order ID is", SilenceNS: uint64(1600 * time.Millisecond)},
	})
	if !decision.Ended {
		t.Fatalf("past the hold the floor ends the turn whatever the model thinks: %+v", decision)
	}
}

// A deployment with no policy model must behave exactly as it did before.
func TestWithoutAProjectionSilenceStillDecidesAlone(t *testing.T) {
	floor := interaction.NewEngineFloor(interaction.EngineFloorOptions{
		SilenceDuration: 500 * time.Millisecond,
		Projection:      interaction.VADOnlyProjection{},
	})
	decision := floor.Endpoint(interaction.Context{
		Revision: interaction.Revision{ID: 1, StableText: "hello", SilenceNS: uint64(600 * time.Millisecond)},
	})
	if !decision.Ended || decision.Projected {
		t.Fatalf("the rule fallback ends on silence and projects nothing: %+v", decision)
	}
}
