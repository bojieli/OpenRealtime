package cascade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// actionUtterance is a local alias so the audio path can name the predicate
// type without importing action for one signature.
type actionUtterance = action.Utterance

// Process executes the interaction plane's rollout plan for one committed
// batch. It is the only place cognition runs.
//
// It plans once and returns. Everything a step produces - an escalation, a
// finished background result, a tool result - re-enters as its own event, and
// the loop decides when to act on it. A processor that chained its own steps
// would be a second scheduler for the same conversation, running without the
// gate that owns that decision, which is how a turn ends up spoken over
// another one or answered twice.
func (runtime *runtime) Process(ctx context.Context, batch eventloop.Batch) error {
	// An obligation the ledger is holding becomes visible to the model here,
	// at the first safe point after the evidence that created it committed.
	// Raising it earlier is not possible - the log refuses a repair whose
	// target is not yet recorded as played - and raising it later would let a
	// turn answer the user while still owing them a correction.
	if err := runtime.raiseRepairs(); err != nil {
		runtime.fail("repair_error", err)
	}
	// A turn-scoped policy suppresses one answer - wait, I have more to say -
	// and is discharged once that answer happens. Expiring it when the speaker
	// stopped would be too early: they stop constantly while making the point
	// the policy was protecting. Expiring it here, where the agent is about to
	// respond to a completed turn, is the moment it was asking about.
	if batch.Contains(trajectory.KindObservation) {
		defer runtime.pinboard.EndTurn()
	}
	revision := runtime.latestRevision(batch)
	plan := runtime.policies.Rollout.Plan(interaction.RolloutInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
		},
		Cause: interaction.Cause{
			Observation:      batch.Contains(trajectory.KindObservation),
			Escalated:        batch.Signalled(interaction.SignalEscalated),
			ToolResult:       batch.Contains(trajectory.KindToolResult),
			ToolError:        batchHasToolError(batch),
			PendingRepair:    len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
			BackgroundResult: batch.Signalled(interaction.SignalBackgroundResult),
			SlowInvocations:  runtime.engine.SlowInvocations(revision),
			Parallel:         batch.Triage == eventloop.TriageParallel,
		},
	})
	planned := make([]map[string]any, 0, len(plan))
	for _, step := range plan {
		planned = append(planned, map[string]any{"kind": step.Kind, "reason": step.Reason})
	}
	runtime.debug(ctx, binding.DebugEvent{
		Category: "policy", Name: "policy.rollout", Phase: "decision",
		CorrelationID: fmt.Sprint(revision), Attributes: map[string]any{
			"step_count": len(plan), "triage": batch.Triage,
		}, Payload: map[string]any{"steps": planned},
	})
	if len(plan) == 0 {
		return nil
	}
	// A passive screen opening is evidence, not an instruction to start an
	// autonomous turn. Before any user-authority observation has established
	// intent, observer-only batches are committed and exposed to clients but do
	// not run cognition. The first user turn becomes the wake-up; later visual
	// changes may then trigger the action the user asked the agent to monitor
	// for. Besides avoiding premature actions, doing this before TurnBegin keeps
	// a silent observation from opening a protocol response it never closes.
	if !runtime.worthActingOn(ctx, batch) {
		return nil
	}

	// Everything this plan produces belongs to one turn, and the client is
	// told the turn is done once. Bracketing here rather than around each
	// output kind is what makes that true: a turn that speaks and then calls a
	// tool is one response with two output items, not two responses of which
	// the client stops reading after the first.
	if err := runtime.sink.TurnBegin(ctx); err != nil {
		return err
	}
	turn := &turnReport{}
	batchBegan := runtime.scheduler.NowNS()
	defer func() {
		turn.stage("turn", runtime.scheduler.NowNS()-batchBegan)
		if timings := turn.timings(); timings != "" && runtime.config.ProfileTurns {
			fmt.Fprintf(os.Stderr, "turn-profile %s\n", timings)
		}
	}()
	defer func() {
		if err := runtime.sink.TurnEnd(ctx, turn.outcome()); err != nil {
			runtime.fail("sink_error", err)
		}
	}()

	standing, interjecting, heard := runtime.cognitionExtras(revision)
	because := ""
	if interjecting {
		// The floor took this turn from somebody mid-sentence, which is what
		// interrupt means, and the voice needs that rather than only knowing
		// the turn is not its own.
		because = string(interaction.ActInterrupt)
	}
	request := cognition.Request{
		Standing: standing, Counting: runtime.countingIsInForce(), Interjecting: interjecting, Heard: heard, Because: because,
		Answered:       runtime.alreadyAnsweredFor(runtime.store.Snapshot()),
		SourceRevision: revision,
		AllowFastTools: runtime.observationHasUserIntent(batch),
		PendingRepair:  len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
	}
	// A visual reflex is a separate optional cognition role, not a mutation of
	// the voice. It gets first refusal only on a batch carrying current visual
	// evidence. Act and wait complete this event; abstain, timeout, or malformed
	// output fall through to the unchanged fast/slow rollout below.
	if visualObservation(batch) {
		outcome, reflexErr := runtime.engine.RunVisualReflex(ctx, request)
		if !errors.Is(reflexErr, cognition.ErrVisualReflexDisabled) {
			turn.record(outcome.Result)
		}
		if reflexErr == nil {
			switch outcome.Kind {
			case cognition.VisualReflexAct:
				return runtime.dispatch(ctx, outcome.Result)
			case cognition.VisualReflexWait:
				return nil
			case cognition.VisualReflexAbstain:
				// The ordinary rollout is the fallback.
			}
		}
	}
	// A plan that already contains the reasoner does not need the voice to ask
	// for it, and asking would run it twice.
	plansSlow := slices.ContainsFunc(plan, func(step interaction.Step) bool {
		return step.Kind == interaction.StepSlow
	})
	var failures []error
	for _, step := range plan {
		select {
		case <-ctx.Done():
			return errors.Join(append(failures, context.Cause(ctx))...)
		default:
		}
		if err := runtime.runStep(ctx, step, request, turn, plansSlow); err != nil {
			if errors.Is(err, continuation.ErrStalePrefix) {
				// Somebody said something while this was being computed, and
				// the safe point refused the output because it answers a
				// sentence that has since been superseded. That is the rule
				// working: the output is discarded, nothing was committed, and
				// the observation that overtook it will get its own turn.
				//
				// It is not a fault in the conversation, and reporting it as
				// one killed a session over a moment that had simply moved on.
				runtime.noteWithheld(continuation.RunResult{}, request, "overtaken by what they said next")
				continue
			}
			failures = append(failures, fmt.Errorf("%s step: %w", step.Kind, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	return errors.Join(failures...)
}

func visualObservation(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind != trajectory.KindObservation || item.Observation == nil {
			continue
		}
		for _, media := range item.Observation.Media {
			if strings.HasPrefix(strings.ToLower(media.MIMEType), "image/") {
				return true
			}
		}
	}
	return false
}

// signal opens a safe point because a cognition phase finished. It appends
// nothing: what it refers to is already in the trajectory.
func (runtime *runtime) signal(eventType string) error {
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: eventType, Source: "cognition", Channel: "cognition",
		Priority: eventloop.PriorityRoutine, Kind: eventloop.KindSignal,
	})
	return err
}

// runStep executes one rollout step.
func (runtime *runtime) runStep(
	ctx context.Context, step interaction.Step, request cognition.Request, turn *turnReport,
	plansSlow bool,
) error {
	switch step.Kind {
	case interaction.StepFast:
		// A background result is worth saying, and not necessarily now. It
		// arrives with no utterance to answer, which is the shape the holding
		// line has - and, like the holding line, it used to go out whatever
		// else was happening. Measured at a phone menu, "I've pressed 2 for
		// order status" was read to a recording that could not hear it and was
		// still talking.
		if step.Reason == interaction.ReasonBackgroundResult && !runtime.speechIsWelcome() {
			runtime.noteInterject("the result is worth saying, and not at this moment")
			return nil
		}
		backgroundResult := step.Reason == interaction.ReasonBackgroundResult
		handsOn := backgroundResult ||
			step.Reason == interaction.ReasonToolFailure || plansSlow
		return runtime.runFast(ctx, request, turn, handsOn, true)
	case interaction.StepSlow:
		return runtime.runSlow(ctx, request, turn)
	default:
		return fmt.Errorf("unknown rollout step %q", step.Kind)
	}
}

// runFast speaks, and hands the turn on unless the voice declared it finished.
//
// A turn that is itself speaking a background result or reporting a tool
// failure never hands on again. In the first case the reasoner has just
// answered; in the second, handing on would retry a failed action without new
// evidence. The next thing the user says opens the question again either way.
func (runtime *runtime) runFast(
	ctx context.Context, request cognition.Request, turn *turnReport,
	alreadyHandedOn, guardSolicitation bool,
) error {
	if !request.Interjecting {
		runtime.ordinaryFastRunning.Add(1)
		defer runtime.ordinaryFastRunning.Add(-1)
	}
	debugBegan := time.Now()
	runtime.debug(ctx, binding.DebugEvent{
		Category: "cognition", Name: "cognition.fast", Phase: "start",
		CorrelationID: fmt.Sprint(request.SourceRevision),
	})
	defer func() {
		runtime.debug(ctx, binding.DebugEvent{
			Category: "cognition", Name: "cognition.fast", Phase: "end",
			CorrelationID: fmt.Sprint(request.SourceRevision), DurationMS: elapsedMS(debugBegan),
		})
	}()
	// A preparation that answered this exact sentence is adopted rather than
	// regenerated. It is the same continuation, produced earlier.
	result, adopted := runtime.adopt(trajectory.PhaseFast, canonicalText(
		runtime.store.Snapshot(), request.SourceRevision))
	var err error
	if !adopted {
		began := runtime.scheduler.NowNS()
		runtime.noteVoiceTurn(request)
		watch := runtime.watchFirstPhrase(began)
		result, err = runtime.engine.RunFast(ctx, request, watch.observe)
		turn.stage("voice", runtime.scheduler.NowNS()-began)
		watch.report(turn)
	}
	turn.record(result)
	if cause := context.Cause(ctx); cause != nil {
		// The user resumed after the provider reached a safe point but before
		// anything crossed the action boundary. The result answered the earlier
		// fragment; neither its speech nor any executable proposal may escape
		// after the turn has moved on.
		runtime.noteWithheld(result, request, "overtaken before action")
		return errors.Join(err, cause)
	}
	// A bounded reflex action crosses the action boundary before speech is
	// queued. Both happen only after the continuation commits at its terminal
	// safe point; ordering the cheap enqueue second keeps it off the critical
	// cue-to-action path.
	var dispatchErr error
	if err == nil && len(result.ToolCalls) > 0 {
		dispatchErr = runtime.dispatch(ctx, result)
	}
	publishBegan := runtime.scheduler.NowNS()
	publishErr := runtime.publishAssistant(ctx, result, request, guardSolicitation)
	turn.stage("publish", runtime.scheduler.NowNS()-publishBegan)
	var signalErr error
	// A turn the voice did not declare finished goes to the reasoner. So does
	// one carrying a proposal, which is the voice naming a capability it
	// cannot run. Silence from a small model means "not finished", because the
	// alternative reading loses every capability the agent has the moment the
	// marker is forgotten.
	if !alreadyHandedOn && (!result.Finished || len(result.ToolProposals) > 0 || len(result.ToolCalls) > 0) {
		signalErr = runtime.signal(interaction.SignalEscalated)
	}
	return errors.Join(err, dispatchErr, publishErr, signalErr)
}

// runSlow deliberates and acts. It never speaks: what it writes is recorded as
// background state, and the signal it raises is what causes the voice to read
// that state and say something of its own.
func (runtime *runtime) runSlow(
	ctx context.Context, request cognition.Request, turn *turnReport,
) error {
	debugBegan := time.Now()
	runtime.debug(ctx, binding.DebugEvent{
		Category: "cognition", Name: "cognition.slow", Phase: "start",
		CorrelationID: fmt.Sprint(request.SourceRevision),
	})
	defer func() {
		runtime.debug(ctx, binding.DebugEvent{
			Category: "cognition", Name: "cognition.slow", Phase: "end",
			CorrelationID: fmt.Sprint(request.SourceRevision), DurationMS: elapsedMS(debugBegan),
		})
	}()
	defer runtime.breakSilenceWhileDeliberating(ctx, request, turn)()
	result, adopted := runtime.adopt(trajectory.PhaseSlow, canonicalText(
		runtime.store.Snapshot(), request.SourceRevision))
	var err error
	if !adopted {
		began := runtime.scheduler.NowNS()
		watch := runtime.watchFirstPhrase(began)
		result, err = runtime.engine.RunSlow(ctx, request, watch.observe)
		turn.stage("reason", runtime.scheduler.NowNS()-began)
		watch.reportAs(turn, "reason")
	}
	if err != nil {
		if ctx.Err() != nil {
			// Work in flight was interrupted. Any executable call it had
			// already committed must be closed out so the prefix stays
			// well-formed rather than dangling.
			if _, placeholderErr := runtime.engine.PlaceholderForInterrupted("interrupted"); placeholderErr != nil {
				return errors.Join(err, placeholderErr)
			}
		}
		return err
	}
	if len(result.ToolCalls) > 0 {
		if runtime.toolCallPrefixOvertaken(request.SourceRevision, request.Because) {
			// The call is already in the append-only trajectory because the
			// provider reached its terminal safe point. It has not crossed the
			// action boundary, though, and renewed user speech is newer evidence
			// even before recognition turns it into words. Close the pending call
			// so no later continuation mistakes it for in-flight work.
			if _, placeholderErr := runtime.engine.PlaceholderForInterrupted("user resumed before action"); placeholderErr != nil {
				return placeholderErr
			}
			return fmt.Errorf("user resumed before action dispatch: %w", continuation.ErrStalePrefix)
		}
		return runtime.dispatch(ctx, result)
	}
	if strings.TrimSpace(result.AssistantText) != "" {
		// The correction, if one was owed, is this. Recording it here rather
		// than when it is spoken is deliberate: the slow provider is the one
		// that was instructed to correct, and the voice saying so afterwards
		// is a rendering of the correction rather than the correction itself.
		if err := runtime.resolveRepairs(result); err != nil {
			runtime.fail("repair_error", err)
		}
		return runtime.signal(interaction.SignalBackgroundResult)
	}
	return nil
}

// toolCallPrefixOvertaken reports whether a slow call has lost the safe point
// it was computed for. Acoustic speech onset is evidence before it has words;
// revision is incremented before a finalized observation enters the event
// loop, which also covers the short flush interval after duplex state becomes
// quiet but before that newer observation can be processed.
//
// Except for a turn that exists to act without speaking, where this was a
// second authority quietly overruling the first. The interaction model decides
// whether to act; it re-decides continuously; and it chose to act silently while
// looking at a situation that said "user: speaking right now" - the very fact
// this would veto on. Re-deciding it here is not a safety check, it is the
// dispatch path disagreeing with the layer that owns the judgement.
//
// Against a recording it disagreed every time. A phone menu never stops
// talking and never stops producing observations, so both clauses stand
// permanently true: measured over five calls, twenty-two of twenty-three
// keypresses were refused as stale and the one that landed was luck. The act
// exists precisely because the other party cannot be replied to and will not
// pause - waiting for them to stop is waiting for something that does not
// happen.
//
// What bounds it instead is time. Acting silently already runs under
// interjectionDeadline, and a key that cannot be pressed inside that window is
// dropped for being late rather than for the menu having carried on.
func (runtime *runtime) toolCallPrefixOvertaken(sourceRevision uint64, because string) bool {
	if because == string(interaction.ActActSilently) {
		return false
	}
	return runtime.duplex.Snapshot().UserSpeaking || runtime.revision.Load() > sourceRevision
}

func batchHasToolError(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil && item.ToolResult.Error != "" {
			return true
		}
	}
	return false
}

// breakSilenceWhileDeliberating arms one spoken turn to fill the gap the
// reasoner is about to leave, and returns the stop.
//
// The reasoning half never speaks, so a question that needs it produces a
// silence whose length is a property of the question. A caller cannot tell
// that from a broken agent: both sound like nothing.
//
// What fills it is not a second turn. The base protocol has one active
// response, the turn that started this deliberation is still open, and the
// voice adding a sentence to a turn it is already in is what "the voice keeps
// talking while the reasoner reasons" actually means. So this runs the same
// fast phase through the same commitment boundary as any other utterance -
// only the goroutine differs, and the trajectory tolerates that now: an
// assistant turn is not evidence, so speaking here does not discard what the
// reasoner is in the middle of working out.
//
// It fires once. A second "still working" is the repetition the voice is told
// to avoid, and the reasoner finishing is what ends the silence properly.
func (runtime *runtime) breakSilenceWhileDeliberating(
	ctx context.Context, request cognition.Request, turn *turnReport,
) func() {
	after := runtime.config.HoldingAfter
	if after <= 0 {
		return func() {}
	}
	// A turn that exists to act without speaking has nowhere to put a holding
	// line. The act was chosen because speech would be pointless - a recording
	// that cannot hear it, or somebody who asked not to be spoken to - and
	// "I'm calling now, please hold" is speech, addressed to whoever the agent
	// decided not to address. Measured at a phone menu, it covered the options
	// the agent was waiting to hear.
	if request.Because == string(interaction.ActActSilently) {
		return func() {}
	}
	var mu sync.Mutex
	var timer clock.Timer
	stopped := false

	// Once, by construction: nothing re-arms after it speaks. The only path
	// that re-arms is the one that found the agent still talking, which has
	// not spoken yet.
	var arm func()
	arm = func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		timer = runtime.scheduler.AfterFunc(after, func() {
			// Nothing to break while the agent is talking: the user is hearing
			// the last turn, not silence. The silence starts when that ends,
			// so this waits again - a one-shot timer that landed mid-utterance
			// would leave the gap it exists for unattended.
			if runtime.duplex.Snapshot().AgentSpeaking {
				arm()
				return
			}
			mu.Lock()
			done := stopped
			mu.Unlock()
			if done {
				return
			}

			// A holding line is speech, and whether the agent speaks is not a
			// question a timer gets to answer on its own. Somebody who said
			// they were going to read for a while and would be checked on in
			// fifteen seconds got "I'm here, waiting for you to continue" at
			// twelve, because the reasoner was still working and nothing
			// between the timer and the speech queue had ever heard the
			// policy. The model that decides whether to speak decides here
			// too; what it says is still not its business.
			if !runtime.speechIsWelcome() {
				arm()
				return
			}

			holding := request
			holding.Holding = true
			// Handed on already: the reasoning this is reporting on is running.
			err := runtime.runFast(ctx, holding, turn, true, false)
			// A holding turn overtaken by the answer arriving is the outcome
			// this whole mechanism is hoping for, not a fault. The reasoner
			// finishing cancels the turn's context, and a continuation cut off
			// that way reports it - which reached the client as a session
			// error for a silence that had just been filled properly.
			//
			// A safe point refusing it for the same reason - the answer
			// committed first, so this was computed from a prefix that has
			// moved - is handled where every session failure is decided.
			if err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				runtime.fail("holding_error", err)
			}
		})
	}
	arm()

	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
		if timer != nil {
			timer.Stop()
		}
	}
}

// silenceWasAskedFor reports that a standing policy names a length of quiet
// that has not elapsed.
//
// Dead air presupposes somebody waiting to be spoken to. Somebody who said they
// would be quiet for a while and asked to be checked on after fifteen seconds
// is not waiting, and filling that silence is the one behaviour the policy
// exists to prevent. The delay and the clock are facts the runtime holds rather
// than judgements, so this is settled before anybody is asked.
//
// It governs every path that produces speech without an utterance to answer -
// the holding line, and the turn that reports what the reasoner came back with.
// Both were built to break a silence, and neither had ever heard a policy
// about silence.
func (runtime *runtime) silenceWasAskedFor() bool {
	quiet, ok := runtime.quietSoFar()
	if !ok {
		return false
	}
	for _, standing := range runtime.pinboard.InForce() {
		if standing.After > 0 && !standing.Due(quiet) {
			return true
		}
	}
	return false
}

// quietSoFar is how long nothing has been heard and nothing has been played.
func (runtime *runtime) quietSoFar() (time.Duration, bool) {
	state := runtime.duplex.Snapshot()
	since := state.UserSpeechEndedNS
	if state.PlayoutHorizonNS > since {
		since = state.PlayoutHorizonNS
	}
	nowNS := runtime.scheduler.NowNS()
	if since == 0 || nowNS < since {
		return 0, false
	}
	return time.Duration(nowNS - since), true
}

// speechIsWelcome asks whether this is a moment to break the silence.
//
// The same question every other moment asks, of the same model, from the same
// rendered situation - so a standing instruction about when to speak governs a
// holding line exactly as it governs an answer. Without an interaction policy
// there is nobody to ask and the silence gets broken, which is the behaviour
// this had before the question existed.
func (runtime *runtime) speechIsWelcome() bool {
	if runtime.silenceWasAskedFor() {
		return false
	}
	if runtime.policies.Interaction == nil {
		return true
	}
	decision := interaction.Context{
		NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
	}
	state := runtime.situation(decision)
	act, _, err := runtime.policies.Interaction.Decide(runtime.ctx, state)
	if err != nil {
		// A question that could not be asked is not an answer of no. The
		// silence this exists to fill is the failure it was built for.
		return true
	}
	return act != interaction.ActStaySilent
}

// noteVoiceTurn records what the voice was given, alongside what the
// interaction model was given.
//
// The interaction shadow answers "why did it decide that" and has answered it
// well. It says nothing about the other half, and the other half is where the
// content comes from: measured, a counting policy came back "4 5" from a model
// that answers the same question correctly five times out of five when the
// conversation is assembled by hand. Something in the real one differs, and
// four rounds of reasoning about which four things it might be would have been
// one round of reading it.
func (runtime *runtime) noteVoiceTurn(request cognition.Request) {
	recorder := runtime.policies.ShadowInteraction
	if recorder == nil {
		return
	}
	snapshot := runtime.store.Snapshot()
	lines := interaction.RecentLines(snapshot.Items, 12)
	// What the request actually adds to the phase prompt. Recorded because
	// four rounds of reasoning about which paragraph might differ would each
	// have been one round of reading it, and three of those four rounds were
	// wrong: it was not the scenario preamble, not the holding paragraph, and
	// not the sampling temperature, all of which reproduce the wanted answer
	// ten times out of ten when tried by hand.
	composed := cognition.Instruct("", request)
	recorder(interaction.ShadowDecision{
		NowNS:     runtime.scheduler.NowNS(),
		Situation: "voice: " + strings.Join(lines, "\n"),
		Act:       "asked",
		Predicates: map[string]string{
			"where": "voice", "because": request.Because,
			"heard": request.Heard, "standing": strings.Join(request.Standing, " | "),
			// The flags decide which paragraphs get composed into the
			// instruction, so a turn that behaved unlike the same prompt tried
			// by hand differs in one of these or in nothing.
			"composed":     composed,
			"holding":      strconv.FormatBool(request.Holding),
			"interjecting": strconv.FormatBool(request.Interjecting),
			"repair":       strconv.FormatBool(request.PendingRepair),
			"observed":     request.Observed,
		},
	})
}

// publishAssistant applies the commitment policy to one continuation's output
// and, when it commits, queues speech and records the queued transition.
//
// The request comes down with the result because what the turn was asked for
// decides two things here: whether the speech was begun over somebody else's
// floor, which barge-in has to know, and what to say about a turn that
// produced nothing.
func (runtime *runtime) publishAssistant(
	ctx context.Context, result continuation.RunResult, request cognition.Request,
	guardSolicitation bool,
) error {
	if strings.TrimSpace(result.AssistantText) == "" || !result.Committed {
		runtime.noteWithheld(result, request, "the model said nothing that reached a safe point")
		return nil
	}
	// Speech and action are separate commitment channels. When one continuation
	// selected an executable call or a typed non-executable proposal, its prose
	// cannot cross as well: before a ToolResult it has no authority to describe
	// the outcome, and after one it would race the result-driven voice turn.
	//
	// This is intentionally about structured state, not words such as "done" or
	// "processed". A sentence-level filter would miss paraphrases and swallow
	// legitimate conversation. The proposal remains in the trajectory for the
	// slow phase to validate, while ordinary speech without action intent keeps
	// the same low-latency path.
	if len(result.ToolProposals) > 0 || len(result.ToolCalls) > 0 {
		return runtime.withholdAssistant(result, request, "speech accompanied an action proposal or call")
	}
	// A model that writes a tool call as prose instead of emitting one has not
	// said anything a person should hear. Measured on a phone menu, the text
	// [{"name":"press_key","arguments":{"key":"2"}}] was spoken aloud - the
	// worst kind of leak, because it is both useless and unmistakably a bug to
	// whoever is listening. There is nothing to salvage: the call is malformed
	// as a call and the sentence is malformed as speech.
	if looksLikeToolCall(result.AssistantText) {
		return runtime.withholdAssistant(result, request, "the model wrote a tool call as prose")
	}
	if isStageDirection(result.AssistantText) {
		return runtime.withholdAssistant(result, request, "the model described saying nothing instead of saying nothing")
	}
	if strings.Contains(result.AssistantText, cognition.WaitToken) {
		// A decision to be silent, which is a different thing from a turn that
		// produced nothing, and the whole reason the token exists: the second
		// is worth looking at and the first is the system working.
		//
		// Anywhere in the text, not only alone. A turn that both says
		// something and asks for silence is a model in two minds, and the safe
		// reading of a token whose entire purpose is silence is silence -
		// where the other reading speaks a control token out loud. Measured at
		// a phone menu, "Pressing the key for order status. <wait>" was read
		// to a recording that could not hear it and was still talking.
		return runtime.withholdAssistant(result, request, "the voice chose to stay silent")
	}
	// A question creates an obligation for the other person to answer. Slow
	// work finishing may give the voice a different clarification to ask, but
	// it does not erase the first obligation and it contributes no new user
	// evidence. Queuing both questions turns one request into an interrogation
	// pile-up: measured in a retail call, every identity answer received a
	// second clarification, the caller began spelling over it, and the agent
	// eventually treated the overlap it created as a communication failure.
	//
	// A background result is voiced by a fresh fast continuation. If the slow
	// phase added nothing, that continuation can produce the same sentence the
	// first fast turn already queued. Both are individually valid outputs, but
	// together they are one answer spoken twice with no user event between
	// them.
	//
	// Keep this deliberately exact and revision-scoped. A tiny textual change
	// can be a material correction ("$14" rather than "$40"), and the same
	// words after a new observation can be a requested repetition. Cancelled or
	// merely prepared content was never committed to the listener and therefore
	// cannot suppress anything.
	if runtime.alreadyPublishedForRevision(result) {
		return runtime.withholdAssistant(result, request, "the same response was already queued for this observation")
	}
	items := runtime.assistantItems(result)
	if len(items) == 0 {
		return nil
	}
	authority := items[0].Producer.SpeechAuthority
	decision := runtime.policies.Commitment.Decide(interaction.CommitmentInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
			Phase: items[0].Producer.Phase,
		},
		Text: result.AssistantText, Complete: !result.Interrupted, SpeechAuthority: authority,
	})
	if !decision.Committed() {
		return runtime.withholdAssistant(result, request, decision.Reason)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	utterance := action.Utterance{
		ID: idFor("speech", runtime.sequence.Add(1)), Text: decision.Emit,
		Phase: items[0].Producer.Phase, SourceRevision: result.SourceRevision,
		AssistantItemIDs: ids, SpokeOver: request.Interjecting,
	}
	claimedSolicitation := false
	if guardSolicitation && asksForReply(result.AssistantText) {
		if !runtime.claimSolicitation(utterance.ID) {
			return runtime.withholdAssistant(result, request, "another question is already awaiting an answer")
		}
		claimedSolicitation = true
	}

	if runtime.textOnly() {
		// No synthesiser, no pacing, no duplex state: a text turn is delivered
		// the moment it is written. It crosses the same commit boundary, which
		// is the whole reason the boundary is one boundary for every output.
		err := runtime.emitText(ctx, utterance, authority)
		if err != nil && claimedSolicitation {
			runtime.clearUncrossedSolicitation(utterance.ID)
		}
		return err
	}
	// The queued transition is committed before audio can be emitted, so the
	// log's account of what the world heard never runs ahead of the world.
	events := make([]eventloop.Event, 0, len(ids))
	for _, id := range ids {
		events = append(events, eventloop.Event{
			Type: "speech.queued", Source: "action", Channel: "voice",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindAssistantState,
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: id, Visibility: trajectory.VisibilityQueued,
			},
		})
	}
	if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
		if claimedSolicitation {
			runtime.clearSolicitation(utterance.ID)
		}
		return err
	}
	if err := runtime.speech.Enqueue(utterance, authority); err != nil {
		if claimedSolicitation {
			runtime.clearSolicitation(utterance.ID)
		}
		if errors.Is(err, action.ErrSilentProducer) {
			// The commitment policy should have held this. Enforcing it again
			// here is the point of having the boundary at the commit site.
			return nil
		}
		cancelErr := runtime.recordCancellations([]action.Commitment{{
			ID: utterance.ID, AssistantItemIDs: ids,
		}}, "speech-enqueue", eventloop.PriorityRoutine)
		return errors.Join(err, cancelErr)
	}
	_ = ctx
	return nil
}

// withholdAssistant records that committed model text did not cross the
// speech boundary and appends the matching visibility transition. Cancellation
// is part of the semantic boundary, not only cleanup: provider projections
// must not present unheard text to later cognition as something the user was
// told. A mixed action result normally has no assistant item because the
// continuation runner isolates its typed call; in that case this only records
// the diagnostic reason.
func (runtime *runtime) withholdAssistant(
	result continuation.RunResult, request cognition.Request, why string,
) error {
	runtime.noteWithheld(result, request, why)
	items := runtime.assistantItems(result)
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return runtime.recordCancellations([]action.Commitment{{
		ID: "withheld-" + result.InvocationID, AssistantItemIDs: ids,
	}}, "speech-withheld", eventloop.PriorityRoutine)
}

// claimSolicitation reserves the one ordinary request that may await an
// answer. The reservation happens before the speech queue and its trajectory
// visibility event, closing the small race in which both continuations could
// otherwise decide they were first. A cancelled commitment never crossed and
// therefore does not keep the reservation.
func (runtime *runtime) claimSolicitation(id string) bool {
	runtime.solicitationMu.Lock()
	defer runtime.solicitationMu.Unlock()
	if current := runtime.solicitationID; current != "" {
		commitment, exists := runtime.ledger.Lookup(current)
		if !exists || commitment.State != action.StateCancelled {
			return false
		}
	}
	runtime.solicitationID = id
	return true
}

func (runtime *runtime) clearSolicitation(id string) {
	runtime.solicitationMu.Lock()
	defer runtime.solicitationMu.Unlock()
	if id == "" || runtime.solicitationID == id {
		runtime.solicitationID = ""
	}
}

func (runtime *runtime) clearUncrossedSolicitation(id string) {
	commitment, exists := runtime.ledger.Lookup(id)
	if !exists || !commitment.State.Crossed() {
		runtime.clearSolicitation(id)
	}
}

// asksForReply identifies output that creates an obligation for the other
// person to answer. Explicit question punctuation is the primary boundary.
// Voice models also phrase requests as polite imperatives ("please spell that
// again.") often enough that punctuation alone is not the conversational
// boundary it appears to be, so this recognizes a deliberately narrow set of
// reply-seeking constructions. It does not treat directions such as "please
// hold" or "please note" as questions.

func asksForReply(text string) bool {
	if strings.ContainsAny(text, "?？") {
		return true
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(character rune) bool {
		return character < 'a' || character > 'z'
	})
	for index, word := range words {
		switch word {
		case "please":
			if index+1 < len(words) && replySeekingVerb(words[index+1]) {
				return true
			}
			if index+2 < len(words) && words[index+1] == "double" && words[index+2] == "check" {
				return true
			}
		case "can", "could", "will", "would":
			verb := index + 2
			if index+1 >= len(words) || words[index+1] != "you" {
				continue
			}
			if verb < len(words) && words[verb] == "please" {
				verb++
			}
			if verb < len(words) && replySeekingVerb(words[verb]) {
				return true
			}
		}
	}
	return false
}

func replySeekingVerb(word string) bool {
	switch word {
	case "answer", "choose", "clarify", "confirm", "describe", "explain",
		"give", "identify", "provide", "repeat", "say", "select", "share",
		"spell", "state", "tell", "verify":
		return true
	default:
		return false
	}
}

func (runtime *runtime) alreadyPublishedForRevision(result continuation.RunResult) bool {
	candidate := comparableSpeech(result.AssistantText)
	if candidate == "" {
		return false
	}
	appended := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		appended[id] = struct{}{}
	}
	snapshot := runtime.store.Snapshot()
	visibility := trajectory.AssistantVisibility(snapshot)
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindAssistant || item.SourceRevision != result.SourceRevision {
			continue
		}
		if _, current := appended[item.ID]; current {
			continue
		}
		state := visibility[item.ID]
		if state != trajectory.VisibilityQueued && state != trajectory.VisibilityPlayed {
			continue
		}
		if repeatsPublishedSpeech(comparableSpeech(item.Content), candidate) {
			return true
		}
	}
	return false
}

// comparableSpeech removes differences that cannot change what was said.
// Punctuation and word content remain significant so a correction is never
// mistaken for a duplicate merely because most of its sentence is the same.
func comparableSpeech(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// repeatsPublishedSpeech also recognises a second sentence lifted verbatim
// from the end of an already-published response. Voice models commonly drop
// the acknowledgement and repeat only its question on the background pass:
// "I can help. What is the order ID?" followed by "What is the order ID?".
// Requiring a sentence boundary before the suffix keeps negation and other
// meaning-changing prefixes significant.
func repeatsPublishedSpeech(published, candidate string) bool {
	if published == candidate {
		return true
	}
	if candidate == "" || len(candidate) >= len(published) || !strings.HasSuffix(published, candidate) {
		return false
	}
	prefix := strings.TrimSpace(published[:len(published)-len(candidate)])
	if prefix == "" {
		return false
	}
	for _, boundary := range []string{".", "!", "?", "。", "！", "？"} {
		if strings.HasSuffix(prefix, boundary) {
			return true
		}
	}
	return false
}

func (runtime *runtime) assistantItems(result continuation.RunResult) []trajectory.Item {
	appended := make(map[string]struct{}, len(result.AppendedIDs))
	for _, id := range result.AppendedIDs {
		appended[id] = struct{}{}
	}
	var items []trajectory.Item
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind != trajectory.KindAssistant {
			continue
		}
		if _, ok := appended[item.ID]; ok {
			items = append(items, item)
		}
	}
	return items
}

// recordCancellations commits the visibility transitions for output that never
// reached the world, so a provider compiling the prefix does not see cancelled
// content as conversational history.
func (runtime *runtime) recordCancellations(
	cancelled []action.Commitment, source string, priority eventloop.Priority,
) error {
	seen := make(map[string]struct{})
	events := make([]eventloop.Event, 0, len(cancelled))
	for _, commitment := range cancelled {
		for _, id := range commitment.AssistantItemIDs {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			events = append(events, eventloop.Event{
				Type: "speech.cancelled", Source: source, Channel: "voice",
				Priority: priority, Kind: trajectory.KindAssistantState,
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: id, Visibility: trajectory.VisibilityCancelled,
				},
			})
		}
	}
	if len(events) == 0 {
		return nil
	}
	if _, err := runtime.coordinator.SubmitBatch(events); err != nil {
		return fmt.Errorf("record %d cancellations: %w", len(events), err)
	}
	return nil
}

// dispatch splits an authoritative call batch between the tools this process
// executes and the tools the client executes, and reports whether results were
// appended here.
func (runtime *runtime) dispatch(ctx context.Context, result continuation.RunResult) error {
	for _, call := range result.ToolCalls {
		runtime.debug(ctx, binding.DebugEvent{
			Category: "tool", Name: "tool.call.planned", Phase: "start",
			CorrelationID: call.CallID, Attributes: map[string]any{
				"name": call.Name, "invocation_id": result.InvocationID,
			}, Payload: map[string]any{"arguments": string(call.Arguments)},
		})
	}
	var local, remote []trajectory.ToolCall
	immediate := make(map[string]trajectory.ToolResult, len(result.ToolCalls))
	for _, call := range result.ToolCalls {
		call.Arguments = slices.Clone(call.Arguments)
		spec, declared := runtime.registry.Lookup(call.Name)
		if declared && spec.Dispatcher != nil {
			local = append(local, call)
			continue
		}
		// The client owns this implementation, not its authority. Confirmation
		// and the irreversible ledger still run before the call crosses the
		// protocol boundary. A refusal becomes an ordinary tool result and the
		// client never sees the call.
		if err := runtime.tools.EmitRemote(ctx, call); err != nil {
			immediate[call.CallID] = trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name, Error: err.Error(),
			}
			continue
		}
		remote = append(remote, call)
	}
	if len(remote) > 0 {
		if err := runtime.sendToClient(ctx, result, remote); err != nil {
			return err
		}
	}
	if len(local) > 0 {
		results, err := runtime.tools.DispatchAll(ctx, local)
		if err != nil {
			return err
		}
		for _, toolResult := range results {
			immediate[toolResult.CallID] = toolResult
		}
	}
	if len(remote) > 0 {
		// A split batch cannot be appended atomically: the invocation's
		// outstanding set includes calls this process is not executing. The
		// local results wait for the client's, which the tool-result path
		// assembles.
		runtime.clientCalls.Hold(result.InvocationID, resultOrder(result.ToolCalls, immediate))
		return nil
	}
	if len(immediate) == 0 {
		return nil
	}
	// Locally dispatched results rejoin exactly where a client's do. Appending
	// them here instead would resume the chain without the loop ever seeing
	// that a tool came back, which is the one thing every completion owes it.
	return runtime.commitToolResults(result.InvocationID, resultOrder(result.ToolCalls, immediate))
}

func resultOrder(calls []trajectory.ToolCall, indexed map[string]trajectory.ToolResult) []trajectory.ToolResult {
	ordered := make([]trajectory.ToolResult, 0, len(indexed))
	for _, call := range calls {
		if result, exists := indexed[call.CallID]; exists {
			ordered = append(ordered, result)
		}
	}
	return ordered
}

func (runtime *runtime) sendToClient(ctx context.Context, result continuation.RunResult, calls []trajectory.ToolCall) error {
	if err := runtime.clientCalls.Track(result.InvocationID, result.ToolCalls); err != nil {
		return err
	}
	var usage *continuation.Usage
	if strings.TrimSpace(result.AssistantText) == "" {
		usage = &result.Completion.Usage
	}
	return runtime.sink.ToolCalls(ctx, binding.ToolCallEvent{
		InvocationID: result.InvocationID, Calls: calls, Usage: usage,
	})
}

// commitToolResults appends a batch every call in which now has a result,
// whether the client answered them or the deadline did.
func (runtime *runtime) commitToolResults(invocationID string, results []trajectory.ToolResult) error {
	for _, result := range results {
		runtime.tools.Complete(result.CallID)
		phase := "end"
		if result.Error != "" {
			phase = "error"
		}
		runtime.debug(runtime.ctx, binding.DebugEvent{
			Category: "tool", Name: "tool.result.committed", Phase: phase,
			CorrelationID: result.CallID, Message: result.Error,
			Attributes: map[string]any{"name": result.Name, "invocation_id": invocationID},
			Payload:    map[string]any{"output": string(result.Output)},
		})
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "tool.results", Source: "client", Channel: "tool",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindToolResult,
		InvocationID: invocationID, ToolResults: results,
	})
	return err
}

// reportUnanswered tells the client what its own silence cost.
//
// The model already knows - it has results saying the calls failed - and the
// client is the one part of the system that would otherwise have no way to
// find out that the work it was asked for never came back.
func (runtime *runtime) reportUnanswered(invocationID string, unanswered []trajectory.ToolCall) {
	names := make([]string, 0, len(unanswered))
	for _, call := range unanswered {
		names = append(names, call.Name)
	}
	runtime.fail("tool_result_timeout", fmt.Errorf(
		"invocation %s: no result for %s, and the call has been failed",
		invocationID, strings.Join(names, ", ")))
}

// ToolResult accepts a client-executed function result. The batch is committed
// only when every outstanding call in the invocation has one, because a
// partial batch would leave the model looking at a call with no result and no
// way to tell whether one is coming.
func (runtime *runtime) ToolResult(_ context.Context, result trajectory.ToolResult) error {
	return runtime.clientCalls.Result(result)
}

// Cancel cancels response generation in flight.
func (runtime *runtime) Cancel(_ context.Context, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "client cancellation"
	}
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	cancelled, heard := runtime.speech.Cancel(reason)
	if heard {
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	}
	return runtime.recordCancellations(cancelled, "client", eventloop.PriorityInterrupt)
}

func (runtime *runtime) latestRevision(batch eventloop.Batch) uint64 {
	revision := batch.SourceRevision()
	// A late tool result or repair resumes from the newest canonical
	// observation. Its own older source stays on the result item, but new
	// output must not be labelled as an old branch.
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind == trajectory.KindObservation {
			revision = max(revision, item.SourceRevision)
		}
	}
	return revision
}

var _ eventloop.Processor = (*runtime)(nil)

// looksLikeToolCall reports whether text is a model emitting a call as prose.
//
// Deliberately narrow. It matches a JSON object or array carrying the keys a
// call is made of, which is a shape no spoken sentence has, and leaves
// everything else alone - a guard that swallows real speech to catch this
// would trade a rare embarrassment for a common silence.
func looksLikeToolCall(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 {
		return false
	}
	lowered := strings.ToLower(trimmed)
	if !strings.Contains(lowered, `"name"`) && !strings.Contains(lowered, `"function"`) {
		return false
	}
	if !strings.Contains(lowered, `"arguments"`) && !strings.Contains(lowered, `"parameters"`) {
		return false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return true
	}
	// Qwen-class chat templates use this tagged representation when a model
	// writes a call into content instead of the structured tool channel. It is
	// just as non-conversational as the JSON inside it and, observed in a real
	// voice benchmark, otherwise reaches synthesis literally as "tool call".
	return strings.HasPrefix(lowered, "<tool_call>")
}

// isStageDirection reports text that describes an absence of speech rather
// than being speech.
//
// A voice asked to stay quiet sometimes writes "(silence)" instead of nothing,
// and every character here is synthesised - so the one thing the turn existed
// to avoid is what the person hears. It is the same shape as a tool call
// written as prose: unmistakably a bug to whoever is listening, and nothing in
// it is worth salvaging.
//
// Bracketed and short, both. A sentence that merely contains the word silence
// is somebody talking about silence, which is ordinary speech.
func isStageDirection(text string) bool {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) < 2 || len(trimmed) > 40 {
		return false
	}
	open, close := trimmed[0], trimmed[len(trimmed)-1]
	if !((open == '(' && close == ')') || (open == '[' && close == ']') || (open == '*' && close == '*')) {
		return false
	}
	inner := strings.ToLower(strings.Trim(trimmed, "()[]* "))
	for _, phrase := range []string{"silence", "silent", "no response", "nothing", "says nothing", "no reply", "pause"} {
		if inner == phrase || strings.HasPrefix(inner, phrase+" ") || strings.HasSuffix(inner, " "+phrase) {
			return true
		}
	}
	return false
}
