package cascade

import (
	"context"
	"strings"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"time"

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
	// Nothing to speak into yet. A policy is often stated in the same breath as
	// the thing it governs, and the floor asks about every partial of that
	// breath - so eight speak-through decisions arrive while somebody is still
	// saying "count the animals as I mention them", before any animal exists.
	// Interjecting there claims the slot and spends it on nothing.
	if !v1.CarriesSpeech(decision.Revision.Text()) {
		runtime.noteInterject("nothing said yet to speak into")
		return
	}
	// One interjection per revision. The floor is consulted on every partial,
	// and a speaker who keeps talking through the words that triggered this
	// would otherwise be answered once per partial for as long as they went on.
	runtime.audioMu.Lock()
	if runtime.lastInterjectRev == decision.Revision.ID {
		runtime.audioMu.Unlock()
		runtime.noteInterject("already answered this revision")
		return
	}
	runtime.lastInterjectRev = decision.Revision.ID
	runtime.audioMu.Unlock()
	// One at a time, but not forever. An interjection runs a continuation and
	// commits it through a loop with one driver, so it can sit behind other
	// work for seconds - and a flag held for that long silences every later
	// moment worth speaking at. Measured, one interjection blocked the next
	// eight in a single conversation and the agent counted once.
	//
	// Speech that would overlap is already prevented where speech is
	// scheduled. What this guards is two continuations at once, which is worth
	// bounding rather than holding open.
	if !runtime.claimInterjection() {
		runtime.noteInterject("already in flight")
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.releaseInterjection()
		// The state may have moved while this was starting. Speaking into a
		// turn that has since ended is worse than not speaking: the turn that
		// ended will produce its own answer, and this would talk over it.
		if state := runtime.duplex.Snapshot(); !state.UserSpeaking || state.AgentSpeaking {
			runtime.noteInterject("the moment passed while starting")
			return
		}
		standing, _, _ := runtime.cognitionExtras()
		// The sentence that caused this. It is in no committed item yet, and
		// without it the voice is asked to speak about something it cannot see.
		//
		// The part since the agent last spoke, where there is one. While the
		// floor holds a turn open the heard text only grows, so an interjection
		// late in a monologue was sending the whole monologue every time - and
		// counting, whose entire story is one held utterance, waited 6.3
		// seconds a turn against 2.1 for interpreting, where a second speaker's
		// turns commit and clear it. An interjection is about what just
		// happened; the rest is already in the conversation above.
		heard := decision.Revision.Text()
		if since := runtime.heardSinceSpeaking(strings.TrimSpace(heard)); since != "" {
			heard = since
		}
		request := cognition.Request{
			SourceRevision: decision.Revision.ID,
			Standing:       standing, Interjecting: true,
			Heard: heard,
		}
		// An interjection that cannot run is an interjection that does not
		// happen, not a fault in the conversation. It is opportunistic by
		// nature - the turn it would have spoken into belongs to somebody else
		// - and reporting it reached the client as a session error for a
		// moment that had simply passed.
		runtime.markSpoken(decision.Revision.Text())
		// Bounded, because an interjection that has missed its moment must not
		// take the next one with it: it commits through a loop with a single
		// driver, so it can sit behind other work indefinitely while every
		// later moment worth speaking at is refused as "already in flight".
		// Abandoning it is the honest outcome - what was worth saying was
		// worth saying then.
		ctx, cancel := context.WithTimeout(runtime.ctx, interjectionDeadline)
		defer cancel()
		err := runtime.runFast(ctx, request, &turnReport{}, true)
		// Silent to the caller and not to the operator. Swallowing it outright
		// traded a noisy bug for an invisible one, and worse: it made me read
		// the absence of these records as the absence of the thing they record.
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := "spoke"
			if err != nil {
				outcome = "refused"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "interject: " + decision.Revision.Text(),
				Act: outcome, Predicates: map[string]string{"where": "interject"},
				Error: errorText(err),
			})
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
	if !batch.Contains(trajectory.KindObservation) {
		return true
	}
	if !runtime.observationHasUserIntent(batch) {
		return false
	}
	if runtime.policies.Interaction == nil {
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

// observationHasUserIntent arms observer-driven cognition after a person has
// spoken once. A user observation in this batch counts because eventloop
// commits before processing; otherwise a prior user observation in the
// canonical trajectory carries the standing intent into later screen/camera
// changes.
func (runtime *runtime) observationHasUserIntent(batch eventloop.Batch) bool {
	for _, item := range batch.Items {
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
		}
	}
	for _, item := range runtime.store.Snapshot().Items {
		if item.Kind == trajectory.KindObservation &&
			trajectory.AuthorityOf(item) == trajectory.AuthorityUser {
			return true
		}
	}
	return false
}

// noteInterject records why an interjection did not happen.
//
// Every one of these is a legitimate refusal and none of them should reach the
// caller, which is exactly why they need somewhere to go: an interjection that
// was decided on and never heard is otherwise indistinguishable from one that
// was never decided on.
func (runtime *runtime) noteInterject(reason string) {
	if recorder := runtime.policies.ShadowInteraction; recorder != nil {
		recorder(interaction.ShadowDecision{
			NowNS: runtime.scheduler.NowNS(), Situation: "interject refused",
			Act: reason, Predicates: map[string]string{"where": "interject"},
		})
	}
}

// interjectionStale is how long an in-flight interjection may hold its claim.
//
// Long enough that two do not run together in the ordinary case, short enough
// that one stuck behind the event loop does not silence the rest of a
// conversation.
const interjectionStale = 3 * time.Second

// interjectionDeadline bounds one interjection.
//
// Generous, because cancelling a slow interjection wastes both the work and
// the slot: at six seconds the counting case regressed to a 13.9s median wait
// from 2.1s, which is what abandoning turns that were about to finish looks
// like. What this is for is a turn that will never finish, not one that is
// late, and those differ by an order of magnitude rather than by a little.
const interjectionDeadline = 12 * time.Second

func (runtime *runtime) claimInterjection() bool {
	now := runtime.scheduler.NowNS()
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	if runtime.interjectStartNS != 0 && now-runtime.interjectStartNS < uint64(interjectionStale) {
		return false
	}
	runtime.interjectStartNS = now
	return true
}

func (runtime *runtime) releaseInterjection() {
	runtime.audioMu.Lock()
	runtime.interjectStartNS = 0
	runtime.audioMu.Unlock()
}

// actSilently runs the phase that may act and may not speak.
//
// The act existed, the model chose it correctly, and nothing happened - the
// floor recognised answer and interrupt and speak-through and let this one
// fall through its default branch. The phone menu passed for a while anyway,
// because a tool call can also come out of an ordinary turn, and stopped
// passing the moment the model got better at choosing this instead.
//
// It is ADR-0006's second boundary under another name: the slow phase acts and
// cannot speak, which is exactly what pressing a key at a recording needs.
func (runtime *runtime) actSilently(decision interaction.Context) {
	if runtime.policies.Interaction == nil {
		return
	}
	// Something already sent and not yet answered is a decision already taken.
	// The situation says so and the model reads it and presses anyway - six
	// times in one call to a phone menu - because each partial is a fresh
	// question and "I already did this" is a property of the sequence. The
	// guard belongs here for the same reason the interruption bound does.
	if pending := trajectory.UnresolvedToolCalls(runtime.store.Snapshot()); len(pending) > 0 {
		runtime.noteInterject("a tool call is already awaiting its result")
		return
	}
	runtime.audioMu.Lock()
	if runtime.lastSilentActRev == decision.Revision.ID {
		runtime.audioMu.Unlock()
		return
	}
	runtime.lastSilentActRev = decision.Revision.ID
	runtime.audioMu.Unlock()
	if !runtime.claimInterjection() {
		runtime.noteInterject("acting silently, but something is already in flight")
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.releaseInterjection()
		standing, _, _ := runtime.cognitionExtras()
		request := cognition.Request{
			SourceRevision: decision.Revision.ID,
			Standing:       standing,
			Heard:          decision.Revision.Text(),
		}
		// runSlow rather than the engine directly: a proposal that nobody
		// dispatches is a key nobody presses. The engine produces the call and
		// the runtime is what executes it, and calling past that layer meant
		// the phase ran nineteen times and the menu never heard a tone.
		ctx, cancel := context.WithTimeout(runtime.ctx, interjectionDeadline)
		defer cancel()
		err := runtime.runSlow(ctx, request, &turnReport{})
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := "acted"
			if err != nil {
				outcome = "refused"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "call-tool: " + decision.Revision.Text(),
				Act: outcome, Predicates: map[string]string{"where": "call-tool"},
				Error: errorText(err),
			})
		}
	}()
}
