package cascade

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// hasActPolicy reports whether this runtime has a model capable of selecting
// an interaction act. The transcript-event policy is deliberately sufficient:
// requiring the legacy whole-interaction model as well would make the new path
// a hidden mutation of the old one rather than a parallel policy.
func (runtime *runtime) hasActPolicy() bool {
	return runtime.policies.Interaction != nil || runtime.policies.TranscriptEvents != nil
}

// decideTranscriptEvent invokes the opt-in policy with an explicit event kind.
// The ordinary situation builder is reused, but the marker and instruction are
// private to TranscriptEventPolicy, so the existing interaction model's prompt
// and observation space are unchanged.
func (runtime *runtime) decideTranscriptEvent(
	ctx context.Context, kind interaction.TranscriptEventKind, revision interaction.Revision,
) (interaction.Context, interaction.Act, error) {
	decision := interaction.Context{
		NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(), Revision: revision,
	}
	if runtime.policies.TranscriptEvents == nil {
		return decision, "", nil
	}
	state := runtime.situation(decision)
	// Synthesis may have claimed an ordinary response before its first audio
	// frame. Duplex state quite correctly says it is not audible yet, but the
	// transcript policy still has a real keep-or-stop decision to make. Treat
	// that reversible queued interval as speaking for act availability; this is
	// the event-aware equivalent of the legacy barge-in path's pending check.
	if !state.AgentSpeaking && runtime.speech.ActiveOrdinary() {
		state.AgentSpeaking = true
		state.AgentSaying = speakingNow(runtime.store.Snapshot())
	}
	state.TranscriptEvent = kind
	// Retain the same constrained act set the policy will render internally.
	// Debug and shadow evidence must be the exact question the model saw, not a
	// reconstruction with the ordinary interaction model's broader vocabulary.
	state.AllowedActs = runtime.policies.TranscriptEvents.AllowedActs(kind)
	decision.Situation = &state
	act, outcome, err := runtime.policies.TranscriptEvents.Decide(ctx, kind, state)
	if err != nil && kind == interaction.TranscriptFinal {
		// A completed utterance used to enter the ordinary cascade
		// unconditionally. Preserve that liveness contract when the new policy
		// endpoint is unavailable; a policy outage must not mute the session.
		act = interaction.ActAnswer
	}
	constraint := ""
	spokeOver := runtime.speech != nil && runtime.speech.ActiveSpokeOver()
	act, constraint = constrainDeliberateSpokeOver(state, act, spokeOver)
	var actConstraint string
	act, actConstraint = runtime.constrainTranscriptAct(kind, state, act)
	if actConstraint != "" {
		constraint = actConstraint
	}

	attributes := map[string]any{
		"event": kind, "act": act, "option": outcome.Option,
	}
	if constraint != "" {
		attributes["constraint"] = constraint
	}
	if outcome.Measured {
		attributes["confidence"] = outcome.Confidence
	}
	phase, message := "decision", ""
	if err != nil {
		phase, message = "error", err.Error()
	}
	runtime.debug(ctx, binding.DebugEvent{
		Category: "policy", Name: "policy.transcript_event", Phase: phase,
		CorrelationID: strconv.FormatUint(revision.ID, 10), Attributes: attributes,
		Payload: map[string]any{"heard": revision.Text(), "situation": state.Render()},
		Message: message,
	})
	if recorder := runtime.policies.ShadowInteraction; recorder != nil {
		recorder(interaction.ShadowDecision{
			NowNS: decision.NowNS, Situation: state.Render(), Act: string(act),
			Predicates: map[string]string{"where": "transcript." + string(kind)},
			Error:      errorText(err), ElapsedNS: outcome.ElapsedNS,
		})
	}
	return decision, act, err
}

// constrainDeliberateSpokeOver protects output the agent intentionally began
// over the current utterance. Continued words from that utterance are the
// premise of an interrupt or speak-through act, not a new barge-in.
func constrainDeliberateSpokeOver(
	state interaction.Situation, act interaction.Act, active bool,
) (interaction.Act, string) {
	if !state.AgentSpeaking || !active || act != interaction.ActStopSpeaking {
		return act, ""
	}
	// The legacy overlap path enforces the same boundary before
	// classification. Without it here, a correction selected 31 ms after
	// "thirteenth" was cancelled by "which gives us" after one 100 ms audio
	// frame and failed the interruption despite starting at the right moment.
	return interaction.ActKeepSpeaking,
		"continued utterance cannot cancel deliberate spoke-over output"
}

// constrainTranscriptAct enforces the authority boundary after the model has
// still seen and decided every event. Speaker identity is evidence, but it is
// also an authorization fact: with no standing instruction delegating a
// current action, somebody else's nearby speech is not a user request. A
// small policy model repeatedly answered direct-sounding fragments despite
// correctly receiving the speaker label. Constraining only answer preserves
// menu tools and standing waiter/translation policies while preventing an
// unaddressed conversation from opening ordinary cognition.
func (runtime *runtime) constrainTranscriptAct(
	kind interaction.TranscriptEventKind, state interaction.Situation, act interaction.Act,
) (interaction.Act, string) {
	if kind == interaction.TranscriptPartial && act == interaction.ActStaySilent &&
		!state.AgentSpeaking && runtime.countingIsInForce() && endsASentence(state.Heard) {
		runtime.audioMu.Lock()
		requested := runtime.countRequestedThisUtterance
		alreadyCounted := runtime.countSpokeThisUtterance
		runtime.audioMu.Unlock()
		if requested && !alreadyCounted {
			// The live policy already found the counting condition, but delivery
			// deliberately waited for a complete phrase. ASR revisions and an
			// uncertain speaker label can make a later policy call retreat to
			// listen. Recover at the first completed partial rather than at the
			// final event: final recovery enters ordinary synthesis, which can be
			// cancelled by the next line before its first audio frame.
			return interaction.ActSpeakThrough,
				"recover counting act requested earlier in this completed partial"
		}
	}
	if kind != interaction.TranscriptFinal {
		return act, ""
	}
	if runtime.countingIsInForce() {
		runtime.audioMu.Lock()
		alreadyCounted := runtime.countSpokeThisUtterance
		requested := runtime.countRequestedThisUtterance
		runtime.audioMu.Unlock()
		if alreadyCounted && act == interaction.ActAnswer {
			return interaction.ActStaySilent, "count already spoken during this utterance"
		}
		if requested && !alreadyCounted && act == interaction.ActStaySilent && !state.AgentSpeaking {
			return interaction.ActAnswer, "recover counting act requested on a partial"
		}
	}
	if act != interaction.ActAnswer {
		return act, ""
	}
	if state.Speaker == otherVoiceSource && len(runtime.pinboard.InForce()) == 0 {
		return interaction.ActStaySilent, "other speaker has no delegated standing instruction"
	}
	return act, ""
}

// handlePartialTranscriptAct carries out an act chosen while the hypothesis is
// still live. Both speak-through and interrupt keep the recogniser open: the
// event-aware path must continue hearing the other speaker while it talks over
// them. Interrupt remains a distinct cognition reason, so the generated line
// is a correction rather than a simultaneous commentary.
func (runtime *runtime) handlePartialTranscriptAct(
	ctx context.Context, decision interaction.Context, act interaction.Act,
) error {
	switch act {
	case interaction.ActSpeakThrough:
		runtime.interject(decision)
	case interaction.ActInterrupt:
		runtime.interjectFor(decision, interaction.ActInterrupt)
	case interaction.ActActSilently:
		runtime.actSilently(decision)
	case interaction.ActStopSpeaking:
		return runtime.stopSpeakingForTranscript(ctx)
	}
	return nil
}

// handleFinalTranscriptAct carries out the final-event acts whose effect is
// immediate. Whether the committed observation opens ordinary cognition is
// recorded separately by rememberTranscriptAct and applied by Process.
func (runtime *runtime) handleFinalTranscriptAct(
	ctx context.Context, decision interaction.Context, act interaction.Act,
) error {
	switch act {
	case interaction.ActActSilently:
		runtime.actSilently(decision)
	case interaction.ActStopSpeaking:
		return runtime.stopSpeakingForTranscript(ctx)
	}
	return nil
}

func (runtime *runtime) stopSpeakingForTranscript(ctx context.Context) error {
	reason := "the transcript-event policy chose stop-speaking"
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	cancelled, heard := runtime.speech.Cancel(reason)
	if err := runtime.recordCancellations(cancelled, "transcript-policy", eventloop.PriorityInterrupt); err != nil {
		return err
	}
	if heard {
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	}
	return nil
}

func revisionFromObservation(
	observation perception.Observation, observedNS, silenceNS uint64,
) interaction.Revision {
	return interaction.Revision{
		ID: observation.Revision, StableText: observation.StableText,
		UnstableText: trimObservationTail(observation.Text, observation.StableText),
		Final:        observation.Final, ObservedNS: observedNS, SilenceNS: silenceNS,
	}
}

// waitForTranscriptInterjection holds the final canonical commit behind the
// content-bearing action chosen from its partials. Without this narrow join,
// a 300 ms recogniser endpoint invariably lands before a cloud voice finishes
// composing; the trajectory then rejects the otherwise correct action as
// stale. The interjection itself is already bounded by interjectionDeadline,
// and cancellation of the session releases this wait as well.
func (runtime *runtime) waitForTranscriptInterjection(ctx context.Context) {
	if runtime.policies.TranscriptEvents == nil {
		return
	}
	runtime.audioMu.Lock()
	done := runtime.interjectDone
	runtime.audioMu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func trimObservationTail(text, stable string) string {
	if stable == "" {
		return text
	}
	if len(text) >= len(stable) && text[:len(stable)] == stable {
		return text[len(stable):]
	}
	return text
}

// rememberTranscriptAct attaches the final-event verdict to the canonical
// source revision it governs. The trajectory still commits unconditionally;
// only the later choice to run cognition is conditional.
func (runtime *runtime) rememberTranscriptAct(revision uint64, act interaction.Act) {
	runtime.transcriptMu.Lock()
	defer runtime.transcriptMu.Unlock()
	if runtime.transcriptActs == nil {
		runtime.transcriptActs = make(map[uint64]interaction.Act)
	}
	runtime.transcriptActs[revision] = act
}

func (runtime *runtime) forgetTranscriptAct(revision uint64) {
	runtime.transcriptMu.Lock()
	delete(runtime.transcriptActs, revision)
	runtime.transcriptMu.Unlock()
}

// transcriptActFor returns the newest event-aware observation in a batch and
// whether that batch also contains an observation this policy did not govern.
// Deferred batches can contain several revisions; the final governed one is
// the current transcript decision and supersedes the earlier view of the same
// speech. An ungoverned observation remains an independent reason to run: a
// final transcript choosing listen must not swallow a screen change or typed
// request merely because the coordinator committed both at one safe point.
func (runtime *runtime) transcriptActFor(
	batch eventloop.Batch,
) (interaction.Act, uint64, bool, bool) {
	runtime.transcriptMu.Lock()
	defer runtime.transcriptMu.Unlock()
	var newest uint64
	var act interaction.Act
	ungoverned := false
	for _, item := range batch.Items {
		if item.Kind != trajectory.KindObservation {
			continue
		}
		candidate, ok := runtime.transcriptActs[item.SourceRevision]
		if !ok {
			ungoverned = true
			continue
		}
		if item.SourceRevision < newest {
			continue
		}
		newest, act = item.SourceRevision, candidate
	}
	return act, newest, newest != 0, ungoverned
}

func (runtime *runtime) forgetTranscriptActs(batch eventloop.Batch) {
	runtime.transcriptMu.Lock()
	defer runtime.transcriptMu.Unlock()
	for _, item := range batch.Items {
		delete(runtime.transcriptActs, item.SourceRevision)
	}
}
