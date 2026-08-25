package cascade

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// interject speaks into somebody else's turn without ending it.
//
// This is the act the four predicates could not express, and the reason the
// counting case never worked: an interaction model can choose speak-through
// perfectly and produce nothing, because until now the only paths to speech
// were a turn ending and a fixed backchannel token.
//
// It is not a backchannel. A continuer is a noise the runtime makes to show it
// is still there, carries no trajectory item, and deliberately teaches the next
// continuation nothing. This is the agent saying something with content - a
// count, a translated sentence, the fact that could not wait - and it has to be
// recorded, because the turn after a count of one is a count of two and a voice
// that cannot see what it just said will say one again.
//
// It never escalates. An interjection is not an answer, and handing it to the
// reasoner would start work on a turn nobody has finished.
func (runtime *runtime) interject(decision interaction.Context) {
	if runtime.policies.Interaction == nil {
		return
	}
	// One interjection per revision. The floor is consulted on every partial,
	// and a speaker who keeps talking through the words that triggered this
	// would otherwise be answered once per partial for as long as they went on.
	runtime.audioMu.Lock()
	if runtime.lastInterjectRev == decision.Revision.ID {
		runtime.audioMu.Unlock()
		return
	}
	runtime.lastInterjectRev = decision.Revision.ID
	runtime.audioMu.Unlock()
	if !runtime.interjecting_.CompareAndSwap(false, true) {
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.interjecting_.Store(false)
		// The state may have moved while this was starting. Speaking into a
		// turn that has since ended is worse than not speaking: the turn that
		// ended will produce its own answer, and this would talk over it.
		if state := runtime.duplex.Snapshot(); !state.UserSpeaking || state.AgentSpeaking {
			return
		}
		standing, _, _ := runtime.cognitionExtras()
		request := cognition.Request{
			SourceRevision: decision.Revision.ID,
			Standing:       standing, Interjecting: true,
			// The sentence that caused this. It is in no committed item yet,
			// and without it the voice is asked to speak about something it
			// cannot see.
			Heard: decision.Revision.Text(),
		}
		err := runtime.runFast(runtime.ctx, request, &turnReport{}, true)
		if err != nil && runtime.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			runtime.fail("interjection_error", err)
		}
	}()
}

// worthActingOn asks the interaction model whether an observation nobody spoke
// aloud is worth a turn.
//
// Every other path to speech consults it. This one did not, and the gap is the
// same one that made "don't interrupt me" impossible: a decision layer that
// governs some routes to the microphone and not others governs nothing, since
// the ungoverned route is always available. An observer commits what it saw and
// the rollout plans a turn on any observation at all, so a screen that changed
// in a way nobody asked about produced a turn exactly as one that mattered did.
//
// Only observer-authority observations are gated here. What a person said is
// already governed by the floor, which decided the turn was theirs to end.
func (runtime *runtime) worthActingOn(ctx context.Context, batch eventloop.Batch) bool {
	if runtime.policies.Interaction == nil || !batch.Contains(trajectory.KindObservation) {
		return true
	}
	snapshot := runtime.store.Snapshot()
	seen := lastSeen(snapshot)
	if seen == "" {
		return true
	}
	// Nothing was said; the observation is the whole of the evidence.
	if runtime.duplex.Snapshot().UserSpeaking {
		return true
	}
	state := runtime.situation(interaction.Context{
		NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
	})
	act, _, err := runtime.policies.Interaction.Decide(ctx, state)
	if err != nil {
		// A decision that could not be taken is not a reason to go silent on
		// something that may matter.
		return true
	}
	return act != interaction.ActStaySilent
}
