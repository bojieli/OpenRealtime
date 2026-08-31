package cascade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
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
	transcriptAct, _, eventAware, ungovernedObservation := runtime.transcriptActFor(batch)
	if eventAware {
		defer runtime.forgetTranscriptActs(batch)
	}
	respondToObservation := batch.Contains(trajectory.KindObservation) &&
		(!eventAware || ungovernedObservation || transcriptAct == interaction.ActAnswer)
	// An obligation the ledger is holding becomes visible to the model here,
	// at the first safe point after the evidence that created it committed.
	// Raising it earlier is not possible - the log refuses a repair whose
	// target is not yet recorded as played - and raising it later would let a
	// turn answer the user while still owing them a correction.
	if err := runtime.raiseRepairs(); err != nil {
		runtime.fail("repair_error", err)
	}
	queuedFreshVisual := runtime.admitFreshVisualObservation(batch)
	if !queuedFreshVisual && visualObservation(batch) && !batchHasUserObservation(batch) {
		queuedFreshVisual = runtime.queueArmedVisualObservation(batch)
	}
	if queuedFreshVisual && !batchHasUserObservation(batch) {
		return nil
	}
	// A successful visual action's result is controller memory, not a new
	// request for speech or arbitrary reasoning. Composite-resume and the next
	// fresh frame already carry its independent semantic and visual successors;
	// treating the result as progress creates an unsolicited voice turn. A
	// failure is still reportable once the user yields, while an in-progress
	// utterance keeps the pre-existing non-interruption behavior.
	if runtime.batchOnlyVisualToolResults(batch) {
		if runtime.duplex.Snapshot().UserSpeaking || batchToolResultsSucceeded(batch) {
			return nil
		}
	}
	if handled, err := runtime.processParallelVisual(ctx, batch); handled {
		return err
	}
	compositeSpeechCovered := false
	if batchHasUserObservation(batch) {
		snapshot := runtime.store.Snapshot()
		intentID, task := runtime.visualTask(snapshot, true)
		assistantItemIDs, active := runtime.activeCompositeResumeSpeechFor(snapshot, intentID, task)
		if active && runtime.batchOnlyCompositeEndpointArtifacts(batch, assistantItemIDs) {
			// A live visual branch already resumed this exact nonvisual clause,
			// and its exact assistant item is queued or played. Keep processing
			// the canonical ASR item so it can refine or complete the visual branch,
			// but do not treat it as a second semantic request.
			compositeSpeechCovered = true
			respondToObservation = false
		}
	} else if visualObservation(batch) && !batchHasUserObservation(batch) {
		snapshot := runtime.store.Snapshot()
		intentID, task := runtime.visualTask(snapshot, false)
		if _, active := runtime.activeCompositeResumeSpeechFor(snapshot, intentID, task); active {
			// Frames and successful effects advance the visual controller. They
			// are not new authority to repeat the independent clause which the
			// same composite request already spoke.
			compositeSpeechCovered = true
			respondToObservation = false
		}
	}
	revision := runtime.latestRevision(batch)
	plan := runtime.policies.Rollout.Plan(interaction.RolloutInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
		},
		Cause: interaction.Cause{
			Observation:           respondToObservation,
			AutonomousObservation: !compositeSpeechCovered && interaction.AutonomousObservation(batch),
			Escalated:             batch.Signalled(interaction.SignalEscalated),
			ToolResult:            !compositeSpeechCovered && batch.Contains(trajectory.KindToolResult),
			ToolError:             batchHasToolError(batch),
			PendingRepair:         len(trajectory.PendingRepairs(runtime.store.Snapshot())) > 0,
			BackgroundResult:      batch.Signalled(interaction.SignalBackgroundResult),
			CompositeResume:       batch.Signalled(interaction.SignalCompositeResume),
			SlowInvocations:       runtime.engine.SlowInvocations(revision),
			Parallel:              batch.Triage == eventloop.TriageParallel,
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
	if len(plan) == 0 && !compositeSpeechCovered {
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
	// A turn-scoped policy suppresses one answer - wait, I have more to say -
	// and is discharged once that answer happens. Observer-only evidence may
	// be admitted in a manual-turn session so its silent monitor stays current,
	// but a zero-step or otherwise inactionable batch is not an answer and must
	// not spend that policy. Install the expiry only after a real response turn
	// has begun; the matching TurnEnd below then closes the exact lifetime the
	// policy governed.
	if respondToObservation {
		defer runtime.pinboard.EndTurn()
	}
	turn := &turnReport{}
	batchBegan := runtime.scheduler.NowNS()
	defer func() {
		turn.stage("turn", runtime.scheduler.NowNS()-batchBegan)
		if timings := turn.timings(); timings != "" && runtime.config.ProfileTurns {
			fmt.Fprintf(os.Stderr, "turn-profile at=%s revision=%d %s\n",
				time.Now().UTC().Format(time.RFC3339Nano), revision, timings)
		}
	}()
	defer func() {
		if err := runtime.sink.TurnEnd(ctx, turn.outcome()); err != nil {
			runtime.fail("sink_error", err)
		}
	}()

	standing, interjecting, heard := runtime.cognitionExtras(revision)
	compositeResume := batch.Signalled(interaction.SignalCompositeResume)
	if compositeResume {
		if pending := runtime.takeCompositeResumeHeard(); strings.TrimSpace(heard) == "" {
			heard = pending
		}
	}
	because := ""
	if interjecting {
		// The floor took this turn from somebody mid-sentence, which is what
		// interrupt means, and the voice needs that rather than only knowing
		// the turn is not its own.
		because = string(interaction.ActInterrupt)
	} else if compositeResume {
		because = interaction.ReasonCompositeResume
	}
	// One snapshot for the whole request. Two calls read the same log twice
	// on the path between hearing a word and answering it.
	snapshot := runtime.store.Snapshot()
	visualIntentID, visualTask := runtime.visualTask(snapshot, batchHasUserObservation(batch))
	request := cognition.Request{
		Standing: standing, Counting: runtime.countingIsInForce(), Interjecting: interjecting, Heard: heard, Because: because,
		Answered:       runtime.alreadyAnsweredFor(snapshot),
		Setting:        runtime.settingAPolicy(snapshot),
		InFlight:       inFlightToolNames(snapshot),
		VisualIntentID: visualIntentID,
		VisualTask:     visualTask,
		SourceRevision: revision,
		ToolResult:     batch.Contains(trajectory.KindToolResult),
		AllowFastTools: runtime.observationHasUserIntent(batch),
		PendingRepair:  len(trajectory.PendingRepairs(snapshot)) > 0,
	}
	if request.VisualIntentID != "" {
		runtime.prepareVisualRequest(snapshot, &request)
	}
	// A visual reflex is a separate optional cognition role, not a mutation of
	// the voice. It gets first refusal only on a batch carrying current visual
	// evidence. Act and wait complete an observer-only event. If the gate merged
	// that frame with fresh user-authority input, WAIT completes only the visual
	// branch and the independent voice/reasoning branch continues below. ACT
	// re-enters that branch after dispatch: a local or promptly answered action
	// can advance the canonical trajectory, so voice generated from the
	// pre-action prefix would correctly be withheld as stale.
	resumeAfterVisual := false
	var visualDispatchErr error
	userVisualTask := batchHasUserObservation(batch) && snapshotHasVisual(snapshot)
	visualAuthority := runtime.visualInteractionIntent(
		ctx, request.VisualIntentID, request.VisualTask, batchHasUserObservation(batch),
	)
	// Monitor authority already says that the requested action belongs to a
	// future visual condition. Arm that condition from the user's words and let
	// the next observer frame be the first grounding opportunity. A mixed batch
	// already carrying fresh visual evidence is that opportunity and therefore
	// continues into grounding below. Running the
	// visual actor on the retained pre-condition frame here only makes it say
	// WAIT, and—more importantly—serializes an independent spoken obligation
	// such as "present this while watching for an alert" behind a full VLM
	// deadline. At 5 fps the next direct-pixel observation is at most one frame
	// away; no visual narration or coordinate guess is introduced.
	monitorArmedFromUser := batchHasUserObservation(batch) &&
		visualAuthority == interaction.VisualIntentMonitor && !visualObservation(batch)
	if monitorArmedFromUser {
		runtime.armVisualMonitor(request.VisualIntentID)
		// This is the same semantic handoff a visual WAIT produces: the silent
		// branch is now monitoring, while the independent spoken/semantic part
		// of the request still needs to run. Preserve that signal even though we
		// avoided the unnecessary retained-frame VLM call that used to produce
		// the WAIT token.
		resumeAfterVisual = true
		if runtime.config.ProfileTurns {
			fmt.Fprintf(os.Stderr,
				"visual-profile at=%s where=user-policy intent=%q task=%q outcome=armed-monitor\n",
				time.Now().UTC().Format(time.RFC3339Nano), request.VisualIntentID, request.VisualTask)
		}
	}
	visualAuthorized := visualAuthority != interaction.VisualIntentNone
	_, visualReflexEnabled := runtime.engine.VisualReflexDescriptor()
	if visualAuthorized && visualReflexEnabled {
		// The direct-pixel role is the sole owner of this request's visible
		// effects. Keep background-safe semantic tools available to the voice,
		// but do not offer it a second copy of the coordinate action surface.
		request.FastBackgroundToolsOnly = true
	}
	userVisualTask = userVisualTask && visualAuthorized && runtime.visualIntentEligible(request.VisualIntentID)
	userVisualTask = userVisualTask && !monitorArmedFromUser
	if userVisualTask && runtime.visualPartial.Load() {
		// The canonical observation supersedes the provider prefix used by any
		// live partial decision. Cancel it before waiting so the endpoint can run
		// one fresh decision instead of waiting for a guaranteed stale commit.
		runtime.cancelVisualDecision()
		if err := runtime.waitForLiveVisual(ctx); err != nil {
			return err
		}
		// The live outcome may have armed or completed the task while this
		// endpoint waited. Re-read the typed state instead of acting on the
		// eligibility snapshot from before that bounded micro-turn finished.
		userVisualTask = visualAuthorized && runtime.visualIntentEligible(request.VisualIntentID)
	}
	armedVisualUpdate := visualObservation(batch) && visualAuthorized &&
		runtime.visualIntentEligible(request.VisualIntentID) && !monitorArmedFromUser
	if explicitVisualActionsComplete(request) {
		// A completed action chunk is not authority to guess the unfinished
		// clause that currently follows it. Keep the controller eligible for a
		// later ASR extension, while allowing an already-complete semantic tail
		// to resume through the ordinary voice/slow path.
		userVisualTask = false
		armedVisualUpdate = false
		resumeAfterVisual = interaction.ImmediateNonvisualClause(request.VisualTask) != ""
	}
	if userVisualTask || armedVisualUpdate {
		outcome, reflexErr := runtime.engine.RunVisualReflex(ctx, request)
		if !errors.Is(reflexErr, cognition.ErrVisualReflexDisabled) {
			turn.record(outcome.Result)
		}
		if reflexErr == nil {
			if targetErr := validateVisualTarget(request, outcome); targetErr != nil {
				runtime.profileVisualOutcome("batch-rejected", request, outcome, targetErr)
				if _, placeholderErr := runtime.engine.PlaceholderCalls(
					outcome.Result.ToolCalls, "visual target rejected: "+targetErr.Error(),
				); placeholderErr != nil {
					return placeholderErr
				}
				resumeAfterVisual = batchHasUserObservation(batch)
				outcome.Kind = cognition.VisualReflexAbstain
				outcome.Result = continuation.RunResult{}
			}
			runtime.applyVisualOutcome(request.VisualIntentID, outcome)
			runtime.profileVisualOutcome("batch", request, outcome, nil)
			switch outcome.Kind {
			case cognition.VisualReflexAct:
				visualDispatchErr = runtime.dispatchVisual(ctx, outcome.Result, request.VisualIntentID)
				if !batchHasUserObservation(batch) {
					return visualDispatchErr
				}
				// Preserve a live partial that has not reached the canonical log,
				// then let the ordinary rollout compile against the post-action
				// prefix. The signal is submitted even when dispatch reports an
				// error: the independent voice/reasoning obligation still exists,
				// and the tool-result path (where one was committed) is not a
				// substitute for it.
				signalErr := runtime.signalCompositeResumeOnce(
					request.VisualIntentID, request.Heard,
				)
				return errors.Join(visualDispatchErr, signalErr)
			case cognition.VisualReflexWait:
				if !batchHasUserObservation(batch) {
					return nil
				}
				resumeAfterVisual = true
			case cognition.VisualReflexAbstain:
				// The ordinary rollout is the fallback.
			}
		}
		if reflexErr != nil {
			runtime.profileVisualOutcome("batch", request, outcome, reflexErr)
		}
	}
	if resumeAfterVisual && !request.Interjecting {
		request.Because = interaction.ReasonCompositeResume
	}
	if (compositeResume || resumeAfterVisual) && strings.TrimSpace(request.VisualTask) != "" {
		request.Standing = runtime.standingExceptCurrentComposite(request.Standing, request.VisualTask)
		request.ImmediateNonvisual = interaction.ImmediateNonvisualClause(request.VisualTask)
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
			return errors.Join(append(failures, visualDispatchErr, context.Cause(ctx))...)
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
	return errors.Join(append(failures, visualDispatchErr)...)
}

// standingExceptCurrentComposite removes only the policy extraction produced
// from the same still-current request. The direct visual controller already
// retains its future monitoring clause; showing that extraction to the voice
// again can make a small model treat the whole mixed request as future-only
// and answer <wait> instead of performing its independent immediate clause.
// Policies from every earlier turn stay visible and authoritative.
func (runtime *runtime) standingExceptCurrentComposite(current []string, task string) []string {
	task = strings.TrimSpace(task)
	if task == "" || len(current) == 0 {
		return current
	}
	runtime.audioMu.Lock()
	pinnedFrom, turn := strings.TrimSpace(runtime.pinnedFromText), runtime.extractTurn
	runtime.audioMu.Unlock()
	sameWords := slices.Equal(trajectory.SpokenWords(pinnedFrom), trajectory.SpokenWords(task))
	if pinnedFrom == "" || turn == 0 ||
		(!sameWords && !trajectory.SaidFurther(pinnedFrom, task) && !trajectory.SaidFurther(task, pinnedFrom)) {
		return current
	}
	return runtime.pinboard.LinesExcept(runtime.scheduler.NowNS(), turn)
}

func (runtime *runtime) profileVisualOutcome(
	where string, request cognition.Request, outcome cognition.VisualReflexOutcome, err error,
) {
	if !runtime.config.ProfileTurns {
		return
	}
	tool := ""
	if len(outcome.Result.ToolCalls) > 0 {
		tool = outcome.Result.ToolCalls[0].Name + ":" + string(outcome.Result.ToolCalls[0].Arguments)
	}
	fmt.Fprintf(os.Stderr,
		"visual-profile at=%s where=%s intent=%q task=%q completed=%d outcome=%s continue=%t target=%q tool=%q error=%q\n",
		time.Now().UTC().Format(time.RFC3339Nano), where, request.VisualIntentID, request.VisualTask, request.CompletedVisualActions,
		outcome.Kind, outcome.Continue, outcome.Target, tool, errorText(err),
	)
}

// validateVisualTarget joins semantic authority to pixel grounding without
// trusting either alone. The actor must name the visible control it grounded,
// and that label must overlap a non-generic word the user actually supplied.
// This rejects a model that autocompletes provisional "over" into the already
// selected Summary tab while preserving arbitrary coordinates and direct
// pixels as the source of spatial truth.
func validateVisualTarget(request cognition.Request, outcome cognition.VisualReflexOutcome) error {
	if outcome.Kind != cognition.VisualReflexAct {
		return nil
	}
	target := strings.TrimSpace(outcome.Target)
	if target == "" {
		// Compatibility for providers/tests predating target metadata is safe
		// only on committed text. A live provisional action without a declared
		// label has no way to prove it did not autocomplete the ASR tail.
		if strings.TrimSpace(request.VisualUnstable) != "" {
			return errors.New("visual action omitted its grounded target for provisional speech")
		}
		return nil
	}
	generic := map[string]struct{}{
		"button": {}, "control": {}, "dialog": {}, "menu": {}, "screen": {},
		"slide": {}, "tab": {}, "the": {}, "this": {}, "that": {},
		"open": {}, "share": {}, "go": {}, "back": {}, "navigate": {},
		"switch": {}, "click": {}, "press": {}, "tap": {}, "select": {},
		"start": {}, "sharing": {}, "acknowledge": {},
	}
	// Ordered action chunks narrow grounding to the next unfulfilled command.
	// Comparing against the whole utterance would let a model repeat the first
	// named control after its successful result merely because that old label
	// still appears earlier in the user's sentence.
	expectedTask := request.VisualTask
	if next, ok := interaction.ExplicitVisualActionAt(
		request.VisualTask, request.CompletedVisualActions,
	); ok {
		expectedTask = next
	}
	taskWords := make(map[string]struct{})
	for _, word := range trajectory.SpokenWords(expectedTask) {
		taskWords[word] = struct{}{}
	}
	for _, word := range trajectory.SpokenWords(target) {
		if len([]rune(word)) < 3 {
			continue
		}
		if _, skip := generic[word]; skip {
			continue
		}
		for taskWord := range taskWords {
			if visualTargetWordsEquivalent(word, taskWord) {
				return nil
			}
		}
	}
	// Some real controls are labelled entirely with UI language (notably
	// "Share screen"). Accept that only when the complete two-or-more-word
	// label is present in the next action after articles/possessives are
	// removed. A lone generic "button" or a shared verb such as "open" cannot
	// grant this fallback.
	targetLabelWords := visualLabelWords(target)
	expectedLabelWords := visualLabelWords(expectedTask)
	if len(targetLabelWords) >= 2 && visualWordsContained(targetLabelWords, expectedLabelWords) {
		return nil
	}
	if len(targetLabelWords) == 1 && targetLabelWords[0] == "acknowledge" &&
		visualWordsContained(targetLabelWords, expectedLabelWords) {
		return nil
	}
	return fmt.Errorf("grounded visual target %q was not named by next action %q", target, expectedTask)
}

func visualLabelWords(text string) []string {
	words := trajectory.SpokenWords(text)
	return slices.DeleteFunc(words, func(word string) bool {
		switch word {
		case "the", "a", "an", "my", "your", "our", "this", "that", "to", "on":
			return true
		default:
			return false
		}
	})
}

func visualWordsContained(needles, words []string) bool {
	available := make(map[string]struct{}, len(words))
	for _, word := range words {
		available[word] = struct{}{}
	}
	for _, needle := range needles {
		if _, ok := available[needle]; !ok {
			return false
		}
	}
	return len(needles) > 0
}

func visualTargetWordsEquivalent(left, right string) bool {
	left, right = strings.ToLower(strings.TrimSpace(left)), strings.ToLower(strings.TrimSpace(right))
	return left != "" && right != "" && singularVisualWord(left) == singularVisualWord(right)
}

func singularVisualWord(word string) string {
	if len(word) > 3 && strings.HasSuffix(word, "ies") {
		return strings.TrimSuffix(word, "ies") + "y"
	}
	if len(word) > 2 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
		return strings.TrimSuffix(word, "s")
	}
	return word
}

func explicitVisualActionsComplete(request cognition.Request) bool {
	count := interaction.ExplicitVisualActionCount(request.VisualTask)
	return count > 0 && request.CompletedVisualActions >= count
}

func (runtime *runtime) waitForLiveVisual(ctx context.Context) error {
	if !runtime.visualPartial.Load() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for runtime.visualPartial.Load() {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
	return nil
}

func (runtime *runtime) beginVisualDecision(
	parent context.Context, timeout time.Duration,
) (context.Context, context.CancelFunc, uint64) {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	} else {
		ctx, cancel = context.WithCancel(parent)
	}
	runtime.visualActionMu.Lock()
	runtime.visualDecisionGeneration++
	generation := runtime.visualDecisionGeneration
	runtime.visualDecisionCancel = cancel
	runtime.visualActionMu.Unlock()
	return ctx, cancel, generation
}

func (runtime *runtime) endVisualDecision(generation uint64, cancel context.CancelFunc) {
	cancel()
	runtime.visualActionMu.Lock()
	if runtime.visualDecisionGeneration == generation {
		runtime.visualDecisionCancel = nil
	}
	runtime.visualActionMu.Unlock()
}

func (runtime *runtime) cancelVisualDecision() {
	runtime.visualActionMu.Lock()
	cancel := runtime.visualDecisionCancel
	runtime.visualActionMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// processParallelVisual keeps receding-horizon screen control independent of
// a slow cognition call already in flight. It runs before response bracketing:
// the lane cannot speak and may emit at most one bounded action, exactly like
// the partial-triggered silent-act path which also operates outside an audible
// turn. Abstention falls through to the ordinary rollout.
func (runtime *runtime) processParallelVisual(ctx context.Context, batch eventloop.Batch) (bool, error) {
	if batch.Triage != eventloop.TriageParallel || !visualObservation(batch) || batchHasUserObservation(batch) {
		return false, nil
	}
	if runtime.visualPartial.Load() {
		return false, nil
	}
	snapshot := runtime.store.Snapshot()
	intentID, task := runtime.visualTask(snapshot, false)
	if !runtime.parallelVisualIntentAuthorized(ctx, intentID, task) {
		return false, nil
	}
	revision := runtime.latestRevision(batch)
	if runtime.visualRevisionHandled(intentID, revision, true) {
		return true, nil
	}
	standing, _, _ := runtime.cognitionExtras(revision)
	request := cognition.Request{
		SourceRevision: revision, VisualIntentID: intentID,
		VisualTask: task,
		Standing:   standing, Counting: runtime.countingIsInForce(), Silent: true,
		InFlight: inFlightToolNames(snapshot), PendingRepair: len(trajectory.PendingRepairs(snapshot)) > 0,
	}
	runtime.prepareVisualRequest(snapshot, &request)
	if explicitVisualActionsComplete(request) {
		// This observer frame satisfies the fresh-frame barrier after the last
		// chunk, but the user has not completed another screen command yet. Do
		// not spend the actor call or terminally consume the still-growing task.
		return true, nil
	}
	outcome, err := runtime.engine.RunVisualReflex(ctx, request)
	runtime.profileVisualOutcome("parallel", request, outcome, err)
	if errors.Is(err, cognition.ErrVisualReflexDisabled) {
		return false, nil
	}
	if err != nil {
		return false, nil
	}
	if targetErr := validateVisualTarget(request, outcome); targetErr != nil {
		runtime.profileVisualOutcome("parallel-rejected", request, outcome, targetErr)
		if _, placeholderErr := runtime.engine.PlaceholderCalls(
			outcome.Result.ToolCalls, "visual target rejected: "+targetErr.Error(),
		); placeholderErr != nil {
			return true, placeholderErr
		}
		return true, nil
	}
	runtime.applyVisualOutcome(request.VisualIntentID, outcome)
	switch outcome.Kind {
	case cognition.VisualReflexAct:
		return true, runtime.dispatchAutonomousVisual(ctx, outcome.Result, request.VisualIntentID)
	case cognition.VisualReflexWait:
		return true, nil
	default:
		return false, nil
	}
}

// parallelVisualIntentAuthorized prevents an observer-only safe point from
// spending (and terminally resolving) the pixel actor before user-derived
// interaction policy has granted screen authority. In particular, a frame
// arriving while ASR still says "go to the" must not let an ABSTAIN consume
// the task just before the completed "go to the Summary" is classified.
//
// update=false is essential: pixels may reuse authority already derived from
// user words, but an autonomous observer frame cannot create that authority.
func (runtime *runtime) parallelVisualIntentAuthorized(
	ctx context.Context, intentID, task string,
) bool {
	if strings.TrimSpace(intentID) == "" || strings.TrimSpace(task) == "" ||
		!runtime.visualIntentEligible(intentID) {
		return false
	}
	return runtime.visualInteractionIntent(ctx, intentID, task, false) != interaction.VisualIntentNone
}

func inFlightToolNames(snapshot trajectory.Snapshot) []string {
	pending := trajectory.UnresolvedToolCalls(snapshot)
	names := make([]string, 0, len(pending))
	seen := make(map[string]struct{}, len(pending))
	for _, call := range pending {
		name := strings.TrimSpace(call.Call.Name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

func (runtime *runtime) batchOnlyVisualToolResults(batch eventloop.Batch) bool {
	callIDs := make([]string, 0, len(batch.Items))
	for _, item := range batch.Items {
		if item.Kind != trajectory.KindToolResult || item.ToolResult == nil {
			return false
		}
		callIDs = append(callIDs, item.ToolResult.CallID)
	}
	if len(callIDs) == 0 {
		return false
	}
	for _, event := range batch.Events {
		if event.Kind != trajectory.KindToolResult {
			return false
		}
	}
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	for _, callID := range callIDs {
		if strings.TrimSpace(runtime.visualIntentByCall[callID]) == "" {
			return false
		}
	}
	return true
}

func batchToolResultsSucceeded(batch eventloop.Batch) bool {
	if len(batch.Items) == 0 {
		return false
	}
	for _, item := range batch.Items {
		if item.Kind != trajectory.KindToolResult || item.ToolResult == nil ||
			!successfulToolResult(*item.ToolResult) {
			return false
		}
	}
	return true
}

func (runtime *runtime) batchOnlyCompositeEndpointArtifacts(
	batch eventloop.Batch, assistantItemIDs []string,
) bool {
	if len(batch.Items) == 0 {
		return false
	}
	runtime.visualActionMu.Lock()
	visualCalls := make(map[string]struct{}, len(runtime.visualIntentByCall))
	for callID, intentID := range runtime.visualIntentByCall {
		if strings.TrimSpace(intentID) != "" {
			visualCalls[callID] = struct{}{}
		}
	}
	runtime.visualActionMu.Unlock()
	coveredAssistantIDs := make(map[string]struct{}, len(assistantItemIDs))
	for _, assistantItemID := range assistantItemIDs {
		if assistantItemID = strings.TrimSpace(assistantItemID); assistantItemID != "" {
			coveredAssistantIDs[assistantItemID] = struct{}{}
		}
	}
	coveredActiveState := func(state *trajectory.AssistantState) bool {
		if state == nil {
			return false
		}
		if _, covered := coveredAssistantIDs[state.AssistantItemID]; !covered {
			return false
		}
		switch state.Visibility {
		case trajectory.VisibilityQueued, trajectory.VisibilityPlayed:
			return true
		default:
			return false
		}
	}
	hasUser := false
	for _, item := range batch.Items {
		switch item.Kind {
		case trajectory.KindObservation:
			if trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
				return false
			}
			hasUser = true
		case trajectory.KindAssistantState:
			if !coveredActiveState(item.AssistantState) {
				return false
			}
		case trajectory.KindToolResult:
			if item.ToolResult == nil || !successfulToolResult(*item.ToolResult) {
				return false
			}
			if _, visual := visualCalls[item.ToolResult.CallID]; !visual {
				return false
			}
		default:
			return false
		}
	}
	for _, event := range batch.Events {
		switch event.Kind {
		case eventloop.KindSignal:
			if event.Type != interaction.SignalEscalated {
				return false
			}
		case trajectory.KindObservation:
		case trajectory.KindAssistantState:
			if !coveredActiveState(event.AssistantState) {
				return false
			}
		case trajectory.KindToolResult:
			for _, result := range event.ToolResults {
				if !successfulToolResult(result) {
					return false
				}
				if _, visual := visualCalls[result.CallID]; !visual {
					return false
				}
			}
		default:
			return false
		}
	}
	return hasUser
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

func snapshotHasVisual(snapshot trajectory.Snapshot) bool {
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
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

func latestUserVisualIntent(snapshot trajectory.Snapshot) string {
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation || trajectory.AuthorityOf(item) != trajectory.AuthorityUser || item.Event == nil {
			continue
		}
		if intent := strings.TrimSpace(item.Event.CorrelationID); intent != "" {
			return intent
		}
	}
	return ""
}

// A gap below this is a recognizer-created continuation unless another agent
// turn has established a new conversational boundary. This is the same timing
// evidence the interaction situation already exposes as SincePrevious: a few
// hundred milliseconds is a breath/tail, while seconds is a new request.
const visualTaskContinuationGap = 1500 * time.Millisecond

// visualTask returns the stable controller identity and full current task.
// It never derives screen meaning from text; pixels remain the grounding
// evidence. The text only preserves user authority across ASR segmentation.
func (runtime *runtime) visualTask(snapshot trajectory.Snapshot, update bool) (string, string) {
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	if !update {
		return runtime.visualTaskID, runtime.visualTaskText
	}
	var item *trajectory.Item
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		candidate := &snapshot.Items[index]
		if candidate.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(*candidate) == trajectory.AuthorityUser {
			item = candidate
			break
		}
	}
	if item == nil || item.Event == nil || strings.TrimSpace(item.Event.CorrelationID) == "" {
		return runtime.visualTaskID, runtime.visualTaskText
	}
	rawID := strings.TrimSpace(item.Event.CorrelationID)
	text := strings.TrimSpace(item.Content)
	if runtime.visualTaskID == "" {
		runtime.visualTaskID, runtime.visualTaskRawID, runtime.visualTaskText = rawID, rawID, text
		return runtime.visualTaskID, runtime.visualTaskText
	}
	currentRawID := runtime.visualTaskRawID
	if currentRawID == "" {
		currentRawID = runtime.visualTaskID
	}
	if rawID == currentRawID {
		if runtime.visualProvisionalTerminal[runtime.visualTaskID] {
			runtime.visualEvaluated[runtime.visualTaskID] = false
			delete(runtime.visualProvisionalTerminal, runtime.visualTaskID)
		}
		canonicalFinalWordCorrection := strings.HasSuffix(
			strings.ToLower(strings.TrimSpace(item.Event.Type)), ".endpoint",
		) && asrFinalWordCorrection(runtime.visualTaskText, text)
		if trajectory.SaidFurther(runtime.visualTaskText, text) ||
			asrWithinWordRegression(runtime.visualTaskText, text) || canonicalFinalWordCorrection {
			runtime.visualTaskText = text
			runtime.visualEvaluated[runtime.visualTaskID] = false
		}
		return runtime.visualTaskID, runtime.visualTaskText
	}
	gap, adjacent := runtime.gapBeforeUtteranceNS(snapshot)
	if adjacent && gap <= uint64(visualTaskContinuationGap) {
		runtime.visualTaskRawID = rawID
		if text != "" && !strings.Contains(runtime.visualTaskText, text) {
			runtime.visualTaskText = strings.TrimSpace(runtime.visualTaskText + " " + text)
			// New user words can turn an earlier terminal fragment into a
			// complete request ("Wait." / "go back to Overview"). Reopen one
			// controller decision for those words. Observer frames do not call
			// this update path and therefore cannot reopen authority themselves.
			runtime.visualEvaluated[runtime.visualTaskID] = false
		}
		return runtime.visualTaskID, runtime.visualTaskText
	}
	runtime.visualTaskID, runtime.visualTaskRawID, runtime.visualTaskText = rawID, rawID, text
	return runtime.visualTaskID, runtime.visualTaskText
}

func (runtime *runtime) visualIntentArmed(intentID string) bool {
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	return strings.TrimSpace(intentID) != "" && runtime.visualArmed[strings.TrimSpace(intentID)]
}

func (runtime *runtime) visualIntentEligible(intentID string) bool {
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	intentID = strings.TrimSpace(intentID)
	return intentID != "" && (!runtime.visualEvaluated[intentID] || runtime.visualArmed[intentID])
}

func (runtime *runtime) applyVisualOutcome(intentID string, outcome cognition.VisualReflexOutcome) {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" {
		return
	}
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	runtime.visualEvaluated[intentID] = true
	switch outcome.Kind {
	case cognition.VisualReflexWait:
		runtime.visualArmed[intentID] = true
	case cognition.VisualReflexAct:
		runtime.visualArmed[intentID] = outcome.Continue
		runtime.visualNeedsFreshFrame = true
	case cognition.VisualReflexAbstain:
		delete(runtime.visualArmed, intentID)
	}
}

// armVisualMonitor records typed interaction authority without pretending a
// visual inference happened. Pixels still decide whether and where to act;
// this state only lets future observer frames ask that question.
func (runtime *runtime) armVisualMonitor(intentID string) {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" {
		return
	}
	runtime.visualActionMu.Lock()
	runtime.visualEvaluated[intentID] = true
	runtime.visualArmed[intentID] = true
	runtime.visualActionMu.Unlock()
}

func (runtime *runtime) admitFreshVisualObservation(batch eventloop.Batch) bool {
	if !visualObservation(batch) {
		return false
	}
	return runtime.admitFreshVisualEvidence()
}

func (runtime *runtime) admitFreshVisualEvidence() bool {
	runtime.visualActionMu.Lock()
	needed := runtime.visualNeedsFreshFrame
	runtime.visualNeedsFreshFrame = false
	if !needed {
		runtime.visualActionMu.Unlock()
		return false
	}
	pending := runtime.visualDeferred
	if pending == nil {
		pending = runtime.visualLast
	}
	if pending == nil {
		runtime.visualActionMu.Unlock()
		return false
	}
	copy := *pending
	copy.groundVisual = true
	runtime.visualPending = &copy
	runtime.visualLast = &copy
	runtime.visualDeferred = nil
	runtime.visualActionMu.Unlock()
	runtime.startLiveVisualWorker()
	return true
}

// queueArmedVisualObservation hands an ordinary observer frame to the same
// latest-evidence worker used by live ASR and post-action replanning. A monitor
// can receive frames faster than its VLM can decide; keeping one request in
// flight and replacing its pending successor prevents both a stale-frame queue
// and serialization of an independent endpoint voice behind visual latency.
func (runtime *runtime) queueArmedVisualObservation(batch eventloop.Batch) bool {
	snapshot := runtime.store.Snapshot()
	intentID, task := runtime.visualTask(snapshot, false)
	revision := runtime.latestRevision(batch)
	if intentID == "" || task == "" || !runtime.visualIntentArmed(intentID) ||
		!runtime.visualIntentEligible(intentID) || runtime.visualRevisionHandled(intentID, revision, true) {
		return false
	}
	runtime.visualActionMu.Lock()
	var pending liveVisualDecision
	if runtime.visualLast != nil && runtime.visualLast.intentID == intentID {
		pending = *runtime.visualLast
	} else {
		pending = liveVisualDecision{
			context: interaction.Context{
				NowNS: runtime.scheduler.NowNS(),
				Revision: interaction.Revision{
					ID: revision, StableText: task, Final: true,
				},
			},
			intentID: intentID,
		}
	}
	pending.context.NowNS = runtime.scheduler.NowNS()
	pending.context.Revision = interaction.Revision{
		ID: revision, StableText: task, Final: true, ObservedNS: runtime.scheduler.NowNS(),
	}
	pending.groundVisual = true
	runtime.visualPending = &pending
	runtime.visualLast = &pending
	runtime.visualActionMu.Unlock()
	runtime.startLiveVisualWorker()
	return true
}

func (runtime *runtime) visualAwaitingFreshFrame() bool {
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	return runtime.visualNeedsFreshFrame
}

// visualInteractionIntent couples semantic interaction policy to pixel
// grounding without collapsing either into the other. The policy is asked
// only when new user words change a task. Frames reuse that typed decision;
// they remain direct evidence for grounding but cannot create user authority.
func (runtime *runtime) visualInteractionIntent(
	ctx context.Context, intentID, task string, update bool, provisional ...string,
) interaction.VisualIntent {
	intentID, task = strings.TrimSpace(intentID), strings.TrimSpace(task)
	if intentID == "" {
		return interaction.VisualIntentNone
	}
	// A deployment without an interaction model keeps the pre-existing visual
	// actor contract. An explicit future visual condition is the one controller
	// distinction that cannot safely be delegated to ACT's generated private
	// continuation bit: the retained pre-condition frame is not actionable, and
	// a false bit on an unrelated immediate action would disable every later
	// frame. Compile that narrow authority locally; all other tasks retain the
	// compatibility behavior in which the visual actor has ACT/WAIT/ABSTAIN and
	// the action boundary.
	if runtime.policies.Interaction == nil {
		if interaction.ExplicitVisualMonitor(task) {
			return interaction.VisualIntentMonitor
		}
		if runtime.config.RequireExplicitVisualAuthority {
			if explicit, ok := interaction.ExplicitVisualAuthority(task); ok {
				return explicit
			}
			return interaction.VisualIntentNone
		}
		return interaction.VisualIntentDirect
	}
	runtime.visualActionMu.Lock()
	classified, exists := runtime.visualPolicy[intentID]
	classifiedTask := runtime.visualPolicyTask[intentID]
	runtime.visualActionMu.Unlock()
	unstable := ""
	if len(provisional) > 0 {
		unstable = strings.TrimSpace(provisional[0])
	}
	if exists && classifiedTask == task && (unstable != "" || classified != interaction.VisualIntentNone) {
		return classified
	}
	if !update {
		return interaction.VisualIntentNone
	}
	// Compile unambiguous imperative UI clauses locally before asking the
	// learned policy. This keeps the 200 ms action clock useful when a model
	// overweights the provisional ASR warning in a mixed request such as "go to
	// Summary and begin presenting". Conditional, incomplete, and semantic
	// language deliberately falls through to the interaction model.
	if explicit, ok := interaction.ExplicitVisualAuthority(task); ok {
		runtime.visualActionMu.Lock()
		runtime.visualPolicy[intentID], runtime.visualPolicyTask[intentID] = explicit, task
		runtime.visualActionMu.Unlock()
		if runtime.config.ProfileTurns {
			fmt.Fprintf(os.Stderr,
				"visual-policy at=%s intent=%q task=%q classified=%q source=explicit elapsed=0s error=%q\n",
				time.Now().UTC().Format(time.RFC3339Nano), intentID, task, explicit, "")
		}
		return explicit
	}
	timeout := runtime.policies.Interaction.DecisionTimeout()
	if timeout <= 0 {
		timeout = 250 * time.Millisecond
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	evidenceTask := task
	if unstable != "" {
		evidenceTask += "\nProvisional ASR tail (possibly an unfinished word): \"" +
			unstable + "\". Do not autocomplete it into a destination."
	}
	classified, outcome, err := runtime.policies.Interaction.DecideVisualIntent(bounded, evidenceTask)
	if runtime.config.ProfileTurns {
		fmt.Fprintf(os.Stderr,
			"visual-policy at=%s intent=%q task=%q classified=%q elapsed=%s error=%q\n",
			time.Now().UTC().Format(time.RFC3339Nano), intentID, task, classified,
			time.Duration(outcome.ElapsedNS), errorText(err))
	}
	runtime.debug(ctx, binding.DebugEvent{
		Category: "policy", Name: "policy.visual_intent", Phase: "decision",
		CorrelationID: intentID, DurationMS: float64(outcome.ElapsedNS) / float64(time.Millisecond),
		Attributes: map[string]any{"intent": classified}, Message: errorText(err),
		Payload: map[string]any{"task": task},
	})
	if err != nil {
		// Preserve compatibility on a transient policy failure. The direct
		// visual actor is itself constrained and may abstain; caching a timeout
		// as denial would make one missed 250 ms deadline silence the task for
		// every later frame.
		return interaction.VisualIntentDirect
	}
	runtime.visualActionMu.Lock()
	runtime.visualPolicy[intentID], runtime.visualPolicyTask[intentID] = classified, task
	runtime.visualActionMu.Unlock()
	return classified
}

// batchHasUserObservation distinguishes a mixed user+screen safe point from
// an autonomous observer update. A visual action may finish the latter; doing
// so to the former would silently discard whatever the person asked for in
// the same batch.
func batchHasUserObservation(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
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
	if runtime.config.ProfileTurns {
		fmt.Fprintf(os.Stderr,
			"fast-profile at=%s revision=%d because=%q committed=%t finished=%t text=%q tools=%d proposals=%d error=%q\n",
			time.Now().UTC().Format(time.RFC3339Nano), request.SourceRevision, request.Because,
			result.Committed, result.Finished, result.AssistantText, len(result.ToolCalls),
			len(result.ToolProposals), errorText(err))
	}
	if cause := context.Cause(ctx); cause != nil && !acrossTheFloor(request.Because) {
		// The user resumed after the provider reached a safe point but before
		// anything crossed the action boundary. The result answered the earlier
		// fragment; neither its speech nor any executable proposal may escape
		// after the turn has moved on.
		//
		// Unless the turn was authorized to cross the floor, where being
		// spoken over is the premise rather than the refutation - see
		// acrossTheFloor.
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
	spoke, publishErr := runtime.publishAssistant(ctx, result, request, guardSolicitation)
	turn.stage("publish", runtime.scheduler.NowNS()-publishBegan)
	if spoke {
		if request.Interjecting && request.Counting &&
			request.Because == string(interaction.ActSpeakThrough) {
			// This is the exact fact final-event recovery needs. Record it only
			// after publish reports audible output: a continuation that returned
			// <wait>, was withheld, or lost its safe point did not count anything.
			runtime.audioMu.Lock()
			runtime.countSpokeThisUtterance = true
			runtime.audioMu.Unlock()
		}
		// The agent has now answered this much of what they are saying. Only
		// the interjection path recorded that, so an ordinary turn had no way
		// to know it had spoken - and one instruction arriving as four
		// committed pieces got four replies: "I'm listening", "I'm ready,
		// please list the animals", twice more, and then a count before any
		// animal had been mentioned.
		//
		// It is the publish that reports this, not the absence of an error.
		// Withholding is not a failure - it succeeds at not speaking - so
		// every one of the seven ways to withhold returned nil, and a turn
		// that answered <wait> marked itself as having spoken for everything
		// said up to that point. <wait> is text, so the emptiness test did not
		// catch it either. The mark then told the next turn it had already
		// covered the very sentence it was being asked to interrupt over, and
		// each refusal made the next one likelier.
		spokenFor := everythingSaid(runtime.store.Snapshot())
		if strings.TrimSpace(request.Heard) != "" && (request.Interjecting ||
			request.Because == interaction.ReasonCompositeResume) {
			// Live interjections and composite resumes both speak before their
			// triggering utterance is canonical. Include the exact fragment this
			// output covered; otherwise the endpoint looks wholly new and repeats
			// the same correction or nonvisual half of a composite request.
			spokenFor = strings.TrimSpace(spokenFor + " " + request.Heard)
		}
		runtime.markSpoken(spokenFor)
		if request.Because == interaction.ReasonCompositeResume {
			assistantItemIDs := make([]string, 0, len(result.AppendedIDs))
			for _, item := range runtime.assistantItems(result) {
				assistantItemIDs = append(assistantItemIDs, item.ID)
			}
			runtime.markCompositeResumeSpoken(
				request.VisualIntentID, request.VisualTask, assistantItemIDs,
			)
		}
	}
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
		if runtime.toolCallPrefixOvertaken(request.SourceRevision, request.Because) &&
			!runtime.backgroundToolCalls(result.ToolCalls) {
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
	if request.ToolResult {
		// The tool result is itself authoritative background state. Some
		// providers consume it, decide no further call is needed, and emit no
		// prose; without this signal the voice is never given a turn in which to
		// tell the user what the action returned. The signal batch does not carry
		// ToolResult, so this cannot reopen the slow lane in a loop.
		return runtime.signal(interaction.SignalBackgroundResult)
	}
	return nil
}

// backgroundToolCalls reports whether every selected call was explicitly
// declared safe to start while newer user speech is arriving. One undeclared
// or ordinary call keeps the whole provider batch behind the stale-prefix
// boundary: partially executing a model-authored batch would invent ordering
// and dependency semantics the model never supplied.
func (runtime *runtime) backgroundToolCalls(calls []trajectory.ToolCall) bool {
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		spec, declared := runtime.registry.Lookup(call.Name)
		if !declared || !spec.Background {
			return false
		}
	}
	return true
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
	if acrossTheFloor(because) {
		return false
	}
	return runtime.duplex.Snapshot().UserSpeaking || runtime.revision.Load() > sourceRevision
}

// acrossTheFloor reports whether an act was chosen on the understanding that
// somebody else holds the floor.
//
// Three of the acts mean "do this while they are still talking". Answering
// means the opposite: it presumes they finished, so their carrying on is
// evidence the answer was computed against half a question, and the answer is
// rightly dropped. For the other three, their carrying on is the situation the
// interaction model was looking at when it chose - so vetoing them for it is
// the dispatch path re-deciding the judgement it does not own, and it decides
// against every time, because the condition it tests is permanent for as long
// as the act makes sense.
//
// What bounds these instead is time: an interjection runs under
// interjectionDeadline and is dropped for being late, not for the other party
// having carried on.
//
// The cost of getting this wrong is not symmetric. A turn withheld cannot be
// recovered; a turn spoken can be judged and repaired. Measured with a voice
// that thinks before it speaks, a third of everything composed was discarded
// here, including corrections that were word-for-word right - and the slower
// the voice, the more certain the veto, because a reply is overtaken by
// whatever was said during the time it took to write.
func acrossTheFloor(because string) bool {
	switch interaction.Act(because) {
	case interaction.ActActSilently, interaction.ActSpeakThrough, interaction.ActInterrupt:
		return true
	}
	return false
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
) (spoke bool, err error) {
	if strings.TrimSpace(result.AssistantText) == "" || !result.Committed {
		runtime.noteWithheld(result, request, "the model said nothing that reached a safe point")
		return false, nil
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
		return false, runtime.withholdAssistant(result, request, "speech accompanied an action proposal or call")
	}
	// A model that writes a tool call as prose instead of emitting one has not
	// said anything a person should hear. Measured on a phone menu, the text
	// [{"name":"press_key","arguments":{"key":"2"}}] was spoken aloud - the
	// worst kind of leak, because it is both useless and unmistakably a bug to
	// whoever is listening. There is nothing to salvage: the call is malformed
	// as a call and the sentence is malformed as speech.
	if looksLikeToolCall(result.AssistantText) {
		return false, runtime.withholdAssistant(result, request, "the model wrote a tool call as prose")
	}
	if isStageDirection(result.AssistantText) {
		return false, runtime.withholdAssistant(result, request, "the model described saying nothing instead of saying nothing")
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
		return false, runtime.withholdAssistant(result, request, "the voice chose to stay silent")
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
		return false, runtime.withholdAssistant(result, request, "the same response was already queued for this observation")
	}
	items := runtime.assistantItems(result)
	if len(items) == 0 {
		return false, nil
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
		return false, runtime.withholdAssistant(result, request, decision.Reason)
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
			return false, runtime.withholdAssistant(result, request, "another question is already awaiting an answer")
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
		return err == nil, err
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
		return false, err
	}
	if err := runtime.speech.Enqueue(utterance, authority); err != nil {
		if claimedSolicitation {
			runtime.clearSolicitation(utterance.ID)
		}
		if errors.Is(err, action.ErrSilentProducer) {
			// The commitment policy should have held this. Enforcing it again
			// here is the point of having the boundary at the commit site.
			return false, nil
		}
		cancelErr := runtime.recordCancellations([]action.Commitment{{
			ID: utterance.ID, AssistantItemIDs: ids,
		}}, "speech-enqueue", eventloop.PriorityRoutine)
		return false, errors.Join(err, cancelErr)
	}
	_ = ctx
	return true, nil
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
		snapshot := runtime.store.Snapshot()
		if duplicate := earlierIdenticalPendingCall(snapshot, call); duplicate != "" {
			immediate[call.CallID] = trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name,
				Output: json.RawMessage(fmt.Sprintf(`{"status":"already_in_flight","call_id":%q}`, duplicate)),
			}
			continue
		}
		runtime.visualActionMu.Lock()
		candidateIntent := runtime.visualIntentByCall[call.CallID]
		intents := make(map[string]string, len(runtime.visualIntentByCall))
		for callID, intent := range runtime.visualIntentByCall {
			intents[callID] = intent
		}
		runtime.visualActionMu.Unlock()
		if duplicate := earlierIdenticalCompletedVisualCall(snapshot, call, candidateIntent, intents); duplicate != "" {
			// The provider has already committed this call to the canonical log,
			// so refusal is represented as an ordinary result rather than by
			// deleting history. This guard joins all visual entry paths: a partial,
			// its final ASR revision, and a changed post-action frame can each ask
			// the reflex, but they cannot execute the same completed click for one
			// request. A new user request or a disappear/reappear visual cycle
			// re-arms the same coordinate.
			immediate[call.CallID] = trajectory.ToolResult{
				CallID: call.CallID, Name: call.Name,
				Output: json.RawMessage(fmt.Sprintf(`{"status":"already_completed","call_id":%q}`, duplicate)),
			}
			continue
		}
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

func (runtime *runtime) dispatchVisual(
	ctx context.Context, result continuation.RunResult, intentID string,
) error {
	intentID = strings.TrimSpace(intentID)
	runtime.visualActionMu.Lock()
	if runtime.visualIntentByCall == nil {
		runtime.visualIntentByCall = make(map[string]string)
	}
	for _, call := range result.ToolCalls {
		runtime.visualIntentByCall[call.CallID] = intentID
	}
	intents := make(map[string]string, len(runtime.visualIntentByCall))
	for callID, intent := range runtime.visualIntentByCall {
		intents[callID] = intent
	}
	runtime.visualActionMu.Unlock()
	// A visual model may repeat the coordinate selected on the preceding frame.
	// Dispatch correctly records that as already_completed, but no effect means
	// there will be no changed post-action frame for adaptive observation to
	// emit. Clear the fresh-frame barrier after that no-op or a later correction
	// is absorbed forever waiting for pixels that cannot arrive.
	noEffect := false
	snapshot := runtime.store.Snapshot()
	for _, call := range result.ToolCalls {
		if earlierIdenticalPendingCall(snapshot, call) != "" ||
			earlierIdenticalCompletedVisualCall(snapshot, call, intentID, intents) != "" {
			noEffect = true
			break
		}
	}
	err := runtime.dispatch(ctx, result)
	if noEffect {
		runtime.visualActionMu.Lock()
		runtime.visualNeedsFreshFrame = false
		// The no-op consumed neither the task nor a frame transition. Keep the
		// intent eligible so a fuller ASR revision or canonical final can ground
		// the actual correction without waiting for pixels that cannot change.
		runtime.visualEvaluated[intentID] = false
		runtime.visualActionMu.Unlock()
	}
	return err
}

// dispatchAutonomousVisual gives a silent observer action its own protocol
// turn. The visual worker runs outside Process's ordinary response bracketing;
// without this boundary a graph-native or Realtime sink cannot publish the
// client tool call at all. The turn contains only the bounded effect—no voice
// or general rollout—and does not consume a manual response request because
// the interaction gate admitted the observer batch independently.
func (runtime *runtime) dispatchAutonomousVisual(
	ctx context.Context, result continuation.RunResult, intentID string,
) error {
	if err := runtime.sink.TurnBegin(ctx); err != nil {
		return err
	}
	dispatchErr := runtime.dispatchVisual(ctx, result, intentID)
	outcome := binding.TurnOutcome{}
	if dispatchErr != nil {
		outcome.Incomplete = true
		outcome.Detail = "autonomous visual action dispatch failed"
	}
	return errors.Join(dispatchErr, runtime.sink.TurnEnd(ctx, outcome))
}

func earlierIdenticalPendingCall(snapshot trajectory.Snapshot, call trajectory.ToolCall) string {
	for _, pending := range trajectory.UnresolvedToolCalls(snapshot) {
		if pending.Call.CallID == call.CallID {
			return ""
		}
		if pending.Call.Name == call.Name && sameToolArguments(call.Name, pending.Call.Arguments, call.Arguments) {
			return pending.Call.CallID
		}
	}
	return ""
}

// earlierIdenticalCompletedVisualCall returns the prior successful screen
// action when the candidate merely repeats it for the same user intent.
//
// Exact coordinates alone are not enough to suppress forever: a person can
// later ask to revisit a tab, and a standing visual obligation can recur. A
// genuinely new user observation re-arms it; observer feedback alone refines
// the next action chunk but carries no new authority to repeat an effect
// already completed for this intent.
func earlierIdenticalCompletedVisualCall(
	snapshot trajectory.Snapshot, call trajectory.ToolCall,
	candidateIntent string, intents map[string]string,
) string {
	if !strings.HasPrefix(call.Name, "computer.") {
		return ""
	}
	results := make(map[string]struct {
		index  int
		result trajectory.ToolResult
	})
	for index, item := range snapshot.Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil {
			results[item.ToolResult.CallID] = struct {
				index  int
				result trajectory.ToolResult
			}{index: index, result: *item.ToolResult}
		}
	}
	for callIndex := len(snapshot.Items) - 1; callIndex >= 0; callIndex-- {
		item := snapshot.Items[callIndex]
		if item.Kind != trajectory.KindToolCall || item.ToolCall == nil ||
			item.ToolCall.CallID == call.CallID || item.ToolCall.Name != call.Name ||
			!sameToolArguments(call.Name, item.ToolCall.Arguments, call.Arguments) {
			continue
		}
		resolved, ok := results[item.ToolCall.CallID]
		if !ok || resolved.index <= callIndex || !successfulToolResult(resolved.result) {
			continue
		}
		priorIntent := strings.TrimSpace(intents[item.ToolCall.CallID])
		sameTypedIntent := priorIntent != "" && strings.TrimSpace(candidateIntent) != "" && priorIntent == strings.TrimSpace(candidateIntent)
		if priorIntent != "" && strings.TrimSpace(candidateIntent) != "" && !sameTypedIntent {
			return ""
		}

		lineage := map[uint64]struct{}{}
		if item.SourceRevision != 0 {
			lineage[item.SourceRevision] = struct{}{}
		}
		liveHeard := invocationHeard(snapshot.Items, item.InvocationID, callIndex)
		for index := resolved.index + 1; index < len(snapshot.Items); index++ {
			observation := snapshot.Items[index]
			if observation.Kind != trajectory.KindObservation {
				continue
			}
			switch trajectory.AuthorityOf(observation) {
			case trajectory.AuthorityUser:
				if sameTypedIntent {
					// The same utterance can become a final canonical observation
					// after an action chosen from its live partial. It is evidence
					// refinement, not new authority to repeat the effect.
					continue
				}
				supersedes := uint64(0)
				if observation.Event != nil {
					supersedes = observation.Event.SupersedesRevision
				}
				_, supersedesSame := lineage[supersedes]
				if (supersedesSame && supersedes != 0) || extendsLiveUtterance(liveHeard, observation.Content) {
					lineage[observation.SourceRevision] = struct{}{}
					liveHeard = observation.Content
					continue
				}
				return ""
			case trajectory.AuthorityObserver:
				// Visual feedback refines the state for the next action chunk; it
				// does not create fresh user authority to repeat this completed
				// effect. A genuinely new user utterance above re-arms it.
			}
		}
		return item.ToolCall.CallID
	}
	return ""
}
func sameToolArguments(name string, left, right json.RawMessage) bool {
	if string(left) == string(right) {
		return true
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	if reflect.DeepEqual(leftValue, rightValue) {
		return true
	}
	return nearbyClickArguments(name, leftValue, rightValue)
}

func nearbyClickArguments(name string, left, right any) bool {
	leftMap, leftOK := left.(map[string]any)
	rightMap, rightOK := right.(map[string]any)
	if !leftOK || !rightOK ||
		(name != computeruse.Click && name != computeruse.ClickNormalized) {
		return false
	}
	leftX, leftXOK := leftMap["x"].(float64)
	leftY, leftYOK := leftMap["y"].(float64)
	rightX, rightXOK := rightMap["x"].(float64)
	rightY, rightYOK := rightMap["y"].(float64)
	if !leftXOK || !leftYOK || !rightXOK || !rightYOK || leftMap["source"] != rightMap["source"] {
		return false
	}
	tolerance := 20.0
	if name == computeruse.ClickNormalized {
		// Normalized coordinates span 0..1000. Vision models jitter by a few
		// percent across identical frames and have repeatedly moved the centre of
		// one large control by 21-39 units. Treat that as the same action chunk;
		// genuinely distinct meeting controls are separated by far more, while
		// raw pixel clicks retain the tighter 20-pixel identity below.
		tolerance = 45
	}
	return reflect.DeepEqual(normalizedClickOptions(leftMap), normalizedClickOptions(rightMap)) &&
		absFloat(leftX-rightX) <= tolerance && absFloat(leftY-rightY) <= tolerance
}

// normalizedClickOptions removes only schema-default-equivalent decoration.
// Coordinate action models alternate between omitting these optional members
// and spelling out the defaults; neither changes the physical effect. Unknown
// members and non-default values remain significant.
func normalizedClickOptions(arguments map[string]any) map[string]any {
	options := make(map[string]any, len(arguments))
	for key, value := range arguments {
		if key != "x" && key != "y" {
			options[key] = value
		}
	}
	if button, present := options["button"]; !present || button == "left" {
		delete(options, "button")
	}
	if actionType, present := options["type"]; !present || actionType == "click" {
		delete(options, "type")
	}
	return options
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func invocationHeard(items []trajectory.Item, invocationID string, before int) string {
	if strings.TrimSpace(invocationID) == "" {
		return ""
	}
	prefix := cognition.HeardInstruction + ` "`
	for index := before - 1; index >= 0; index-- {
		item := items[index]
		if item.InvocationID != invocationID || item.Kind != trajectory.KindInstruction {
			continue
		}
		start := strings.Index(item.Content, prefix)
		if start < 0 {
			return ""
		}
		remainder := item.Content[start+len(prefix):]
		if end := strings.Index(remainder, `"\n\n`); end >= 0 {
			return strings.TrimSpace(remainder[:end])
		}
		if end := strings.LastIndex(remainder, `"`); end >= 0 {
			return strings.TrimSpace(remainder[:end])
		}
		return ""
	}
	return ""
}

func extendsLiveUtterance(heard, committed string) bool {
	heard = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(heard)), " "))
	committed = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(committed)), " "))
	if heard == "" || committed == "" {
		return false
	}
	return strings.HasPrefix(committed, heard) || strings.HasPrefix(heard, committed)
}

func successfulToolResult(result trajectory.ToolResult) bool {
	if strings.TrimSpace(result.Error) != "" || len(result.Output) == 0 {
		return false
	}
	var output map[string]json.RawMessage
	if json.Unmarshal(result.Output, &output) == nil {
		if raw, failed := output["error"]; failed && string(raw) != "null" && string(raw) != `""` {
			return false
		}
	}
	return true
}

func toolResultPerformedEffect(result trajectory.ToolResult) bool {
	if !successfulToolResult(result) {
		return false
	}
	var output map[string]json.RawMessage
	if json.Unmarshal(result.Output, &output) != nil {
		return true
	}
	if raw, present := output["status"]; present {
		var status string
		if json.Unmarshal(raw, &status) == nil && (status == "already_completed" || status == "already_in_flight") {
			return false
		}
	}
	return true
}

// completedVisualActionsForIntent renders effect memory as control state for
// the next fresh-frame replan. Generic coordinate tools cannot name the button
// they clicked, but the runtime does know whether the effect succeeded and
// which user utterance authorized it. Synthetic deduplication results are not
// action chunks: counting them would make one real click look like progress
// through several ordered controls.
func (runtime *runtime) completedVisualActionsForIntent(
	snapshot trajectory.Snapshot, intentID string,
) int {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" {
		return 0
	}
	runtime.visualActionMu.Lock()
	intents := make(map[string]string, len(runtime.visualIntentByCall))
	for callID, intent := range runtime.visualIntentByCall {
		intents[callID] = intent
	}
	runtime.visualActionMu.Unlock()

	effected := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
			toolResultPerformedEffect(*item.ToolResult) {
			effected[item.ToolResult.CallID] = struct{}{}
		}
	}
	completed := 0
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindToolCall || item.ToolCall == nil ||
			item.Producer.Phase != trajectory.PhaseFast ||
			!strings.HasPrefix(item.ToolCall.Name, "computer.") ||
			strings.TrimSpace(intents[item.ToolCall.CallID]) != intentID {
			continue
		}
		if _, succeeded := effected[item.ToolCall.CallID]; succeeded {
			completed++
		}
	}
	return completed
}

// prepareVisualRequest binds the semantic action horizon and effect history to
// one controller intent. The cognition package deliberately does not parse
// interaction language or own cascade's call-to-intent ledger; this boundary
// supplies both as typed state before the direct-pixel role is invoked.
func (runtime *runtime) prepareVisualRequest(
	snapshot trajectory.Snapshot, request *cognition.Request,
) {
	if request == nil || strings.TrimSpace(request.VisualIntentID) == "" {
		return
	}
	request.CompletedVisualActions = runtime.completedVisualActionsForIntent(
		snapshot, request.VisualIntentID,
	)
	if next, ok := interaction.ExplicitVisualActionAt(
		request.VisualTask, request.CompletedVisualActions,
	); ok {
		request.NextVisualAction = next
	}
	runtime.visualActionMu.Lock()
	for callID, intentID := range runtime.visualIntentByCall {
		if strings.TrimSpace(intentID) == strings.TrimSpace(request.VisualIntentID) {
			request.CurrentVisualCallIDs = append(request.CurrentVisualCallIDs, callID)
		}
	}
	runtime.visualActionMu.Unlock()
	slices.Sort(request.CurrentVisualCallIDs)
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
	runtime.refreshVisualObserversAfter(results)
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

func (runtime *runtime) refreshVisualObserversAfter(results []trajectory.ToolResult) {
	refresh := false
	for _, result := range results {
		if strings.HasPrefix(result.Name, "computer.") && toolResultPerformedEffect(result) {
			refresh = true
			break
		}
	}
	if !refresh {
		return
	}
	for _, observer := range runtime.observers.Observers() {
		if visual, ok := observer.(perception.RefreshableObserver); ok {
			visual.RefreshNext()
		}
	}
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
