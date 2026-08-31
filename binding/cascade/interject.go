package cascade

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"

	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
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
// finishedSomething reports that a stretch of new speech completes a sentence.
//
// Sentence-final marks only. A clause was the first attempt and commas are
// far too common - a recogniser that punctuates well puts one in almost every
// chunk it settles, so the test passed on nearly every revision and the count
// still came back "four five". What a running commentary answers is a thing
// somebody finished saying.
func finishedSomething(text string) bool {
	return strings.ContainsAny(text, ".!?。！？")
}

// pausedAsIfFinished reports that they have stopped for as long as this
// deployment treats as the end of an utterance.
//
// It is the operator's own endpoint threshold rather than a number chosen
// here: whatever they set is what this deployment means by "they stopped", and
// a second opinion about that would be a second answer to a question already
// settled.
func (runtime *runtime) pausedAsIfFinished(decision interaction.Context) bool {
	silence := runtime.config.EndpointSilenceMS
	if silence <= 0 {
		return false
	}
	return decision.Revision.SilenceNS >= uint64(silence)*uint64(time.Millisecond)
}

func (runtime *runtime) interject(decision interaction.Context) {
	runtime.interjectFor(decision, interaction.ActSpeakThrough)
}

// interjectFor produces speech while another speaker still holds the acoustic
// floor. Speaking through and cutting in share this path because they share
// the delivery - words while somebody else is talking - and differ in why, so
// the reason travels with the request and the voice is told which one it got.
func (runtime *runtime) interjectFor(decision interaction.Context, reason interaction.Act) {
	if !runtime.hasActPolicy() {
		return
	}
	// Speaking through somebody is how a standing policy gets honoured while
	// they keep talking, so with no policy in force there is nothing to
	// honour. Without this the agent read "count the animals as I mention
	// them" out of the live transcript and said "One" four seconds in, before
	// extraction had even seen the sentence - and every later count was wrong
	// by one.
	//
	// The guard below catches the same thing once a policy exists. This
	// catches it in the seconds before, which is exactly when the request is
	// being spoken.
	//
	// Only for speaking through. Cutting in is not the honouring of a standing
	// policy - it is a correction that cannot wait - and requiring a pinned
	// policy for it would refuse every interruption a deployment asked for in
	// its own instructions.
	if reason == interaction.ActSpeakThrough && len(runtime.pinboard.InForce()) == 0 {
		runtime.noteInterject("no standing policy to speak through for")
		return
	}
	// Not into the sentence that set the policy up. "Count the animals as I
	// mention them" mentions no animal, and a count that starts there is wrong
	// by one for the rest of the conversation - measured, the agent said "One"
	// four seconds in with no animal anywhere, and then called the capybara
	// two.
	//
	// The runtime knows this where the model does not: extraction read that
	// utterance and produced the policy from it, so acting on the same text is
	// acting on the request rather than on anything it asked to watch for. The
	// instruction says so too and the model does not follow it, which makes
	// this the reliable half of the pair rather than a duplicate of it.
	//
	// Against the speech that actually set a policy, not against whatever
	// extraction last read. Those used to be the same thing and are not any
	// more: the pass re-reads a whole turn every time the speaker adds a word,
	// so "the text extraction last read" became "whatever they are saying now"
	// and this refused every occurrence there was - eighty times in one run,
	// which is the whole scenario.
	//
	// And only until the policy has been carried out once, since a policy
	// cannot have been carried out before it was set. That bound needs the
	// first one to get through, which is why it cannot be the only one.
	runtime.audioMu.Lock()
	pinnedFrom := runtime.pinnedFromText
	carriedOut := runtime.carriedOutPolicy
	runtime.audioMu.Unlock()
	if heard := strings.TrimSpace(decision.Revision.Text()); reason == interaction.ActSpeakThrough &&
		!carriedOut && pinnedFrom != "" && strings.HasPrefix(heard, pinnedFrom) {
		runtime.noteInterject("this is the utterance the policy came from")
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
	// And something they have not already been answered about. One revision
	// per interjection is not enough on its own: a recogniser emits one every
	// couple of hundred milliseconds, and most of them add nothing but a comma
	// to a sentence the agent has already spoken into. Measured on an
	// afternoon with two animals in it, the counts came out "1 3 3 1".
	//
	// This is the deterministic half of the rule the instruction states - the
	// condition fires the policy, not the arrival of more text - and it is the
	// half that does not depend on a model reading a line correctly.
	// Against the stable text, not the whole revision. A recogniser keeps
	// revising its own tail - re-punctuating, re-spelling a proper noun,
	// adding a word and taking it back - and every one of those is "something
	// new" to a comparison that reads the lot. Measured on one afternoon, that
	// is twenty chances to answer the same occurrence, each answer becoming
	// the context for the next, and one slip compounds through all of them:
	// the counts came back "1 3 3 1", then "4 5", then "four five six".
	//
	// The stable prefix is what the recogniser has committed to and only grows,
	// so acting on it means acting once per thing actually said.
	stable := strings.TrimSpace(decision.Revision.StableText)
	if stable == "" {
		stable = strings.TrimSpace(decision.Revision.Text())
	}
	// Something new, and - if the agent has already spoken into this same
	// stretch of speech - something finished.
	//
	// Asking twice about one occurrence is what corrupts a running count, and
	// no wording fixes it: with "1 2 3" already in the conversation the model
	// answers "4" eight times out of eight, even told in as many words that
	// its own numbers are not a sequence to continue. The first count is
	// right and everything after it follows from the second one.
	//
	// A sentence is the unit, not a number of words. A count I tried at three
	// new words cut the duplicates and took the phone menu from four in five
	// to none, because those scenarios turn on answering the moment something
	// is named. This delays nothing that has not already been answered: the
	// first act on a new stretch of speech is always allowed, and only acting
	// again inside the same one waits for them to finish saying something.
	newly := runtime.heardSinceSpeaking(stable)
	if newly == "" {
		runtime.noteInterject("nothing new since the agent last spoke")
		return
	}
	if reason == interaction.ActSpeakThrough && runtime.policies.TranscriptEvents != nil &&
		runtime.countingIsInForce() {
		// This records the event policy's content judgement, not successful
		// delivery. It belongs after the guards for the utterance that set the
		// policy: a model often chooses speak-through while somebody is still
		// saying "count the animals", and retaining that as an occurrence would
		// make the completed instruction count as the first animal. It belongs
		// before the completeness guard because waiting there is exactly what a
		// later completed partial has to recover.
		runtime.audioMu.Lock()
		runtime.countRequestedThisUtterance = true
		runtime.audioMu.Unlock()
	}
	// Wait for them to finish saying something. Always when answering a
	// stretch the agent has already spoken into, and on the first answer too
	// when the policy is a running count.
	//
	// The count is the one kind whose next answer depends on its last, so
	// being asked several times about one occurrence corrupts it rather than
	// merely repeating it: the voice is asked again every time the recogniser
	// extends the text, and each ask that says a number moves the count on.
	// Measured, allowing the first answer mid-sentence took counting from five
	// in five to one in five, saying "three" where the second animal was two.
	//
	// Nothing else has that property, and making everything wait was measured
	// too: a correction is useless once they have finished the sentence it was
	// about, which is the whole reason interrupting exists, and the wait took
	// cutting in from five in five to one in five and the visual case with it.
	//
	// So the rule follows the property rather than the act. Whether a policy
	// is a count was read off the person's own words when it was pinned, which
	// is the same reading the arithmetic instruction hangs on.
	if newly != stable || runtime.countingIsInForce() {
		//
		// What stops the same occurrence being answered twice once it is
		// through is the fact the voice is handed: how much of what they are
		// saying it has already spoken for. That is the layer that can tell a
		// second look at one sentence from a second thing to say about it,
		// which is a judgement about content and is not available here.
		// Punctuation is the recogniser's guess at where a sentence ended, and
		// it arrives late: the final partial of "Then a heron landed on the
		// far bank and stared at us" often carries no full stop until the
		// utterance commits, so a count that was ready waited for a mark that
		// had not been written yet and the second animal went uncounted.
		//
		// A pause is the same fact from the audio, which is where it actually
		// lives, and the runtime measures it for the interaction model already.
		if !finishedSomething(newly) && !runtime.pausedAsIfFinished(decision) {
			runtime.noteInterject("they have not finished saying anything since the agent last spoke")
			return
		}
	}
	// One at a time, but not forever. An interjection runs a continuation and
	// commits it through a loop with one driver, so it can sit behind other
	// work for seconds - and a flag held for that long silences every later
	// moment worth speaking at. Measured, one interjection blocked the next
	// eight in a single conversation and the agent counted once.
	//
	// Speech that would overlap is already prevented where speech is
	// scheduled. What this guards is two continuations at once, which is worth
	// bounding rather than holding open.
	claim, claimed := runtime.claimInterjection()
	if !claimed {
		runtime.noteInterject("already in flight")
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.releaseInterjection(claim)
		// The state may have moved while this was starting. Speaking into a
		// turn that has since ended is worse than not speaking: the turn that
		// ended will produce its own answer, and this would talk over it.
		if state := runtime.duplex.Snapshot(); !state.UserSpeaking || state.AgentSpeaking {
			runtime.noteInterject("the moment passed while starting")
			return
		}
		standing, _, _ := runtime.cognitionExtras(0)
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
			VisualIntentID: runtime.currentUtterance(),
			Standing:       standing, Counting: runtime.countingIsInForce(), Interjecting: true, Because: string(reason),
			Heard: heard,
			// This is the path that speaks while somebody is still talking, so
			// it is the path that answers one stretch of speech more than
			// once. Trimming Heard to what is new is not enough on its own:
			// the conversation above still holds the whole sentence, and a
			// policy that says to count everything they have said is answered
			// from there - with the agent's own earlier numbers in view, which
			// is the one thing it cannot reconcile.
			Answered: runtime.alreadyAnsweredFor(runtime.store.Snapshot()),
		}
		// An interjection that cannot run is an interjection that does not
		// happen, not a fault in the conversation. It is opportunistic by
		// nature - the turn it would have spoken into belongs to somebody else
		// - and reporting it reached the client as a session error for a
		// moment that had simply passed.
		// The mark that records how much has been answered is set by runFast,
		// once the turn has actually been published with something in it. It
		// used to be set here as well, before the model had even been asked -
		// so a turn that came back <wait>, or was withheld, still counted as
		// having spoken for everything said up to that point.
		//
		// That is self-reinforcing, and it silences exactly the case this path
		// exists for. Measured on the interrupting scenario: the agent said
		// nothing at all, and was then told "you have already spoken once for
		// this much of what they are saying" quoting the sentence containing
		// the wrong date - so the one fact worth interrupting for was the one
		// fact it believed it had already given. It answered <wait>, correctly,
		// eleven times.
		//
		// The unit is unchanged, because runFast records the same whole turn:
		// the mark is read back as a prefix of the turn, and a fragment from
		// the middle never is one. heardSinceSpeaking still trims what the
		// voice is shown, which is a different question - what is new to say
		// something about, rather than how much has been covered.
		if len(standing) > 0 {
			// A policy has now been acted on, so no later sentence is the one
			// that set it.
			runtime.audioMu.Lock()
			runtime.carriedOutPolicy = true
			runtime.audioMu.Unlock()
		}
		// Bounded, because an interjection that has missed its moment must not
		// take the next one with it: it commits through a loop with a single
		// driver, so it can sit behind other work indefinitely while every
		// later moment worth speaking at is refused as "already in flight".
		// Abandoning it is the honest outcome - what was worth saying was
		// worth saying then.
		ctx, cancel := context.WithTimeout(runtime.ctx, interjectionDeadline)
		defer cancel()
		err := runtime.runFast(ctx, request, &turnReport{}, true, false)
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
	// A person's committed turn has already passed through the floor policy.
	// It is authority to answer, not an autonomous observer update to gate a
	// second time. Letting an interaction-model "listen" decision suppress it
	// made a complete spoken request visible in the trajectory while starting
	// no cognition at all.
	if batchHasUserObservation(batch) {
		return true
	}
	if !runtime.observationHasUserIntent(batch) {
		return false
	}
	// Once the person has armed a visual task, direct pixels must reach the
	// direct-vision actor before a text interaction policy can discard them.
	// The reflex still has the narrower ACT/WAIT/ABSTAIN contract and the sole
	// effect authority; this only preserves the evidence path. Without it, an
	// alert could be present in the frame while a policy reasoning from weaker
	// evidence selected stay-silent and prevented the pixel owner from seeing
	// the condition at all.
	if visualObservation(batch) && runtime.engine != nil {
		if _, enabled := runtime.engine.VisualReflexDescriptor(); enabled {
			return true
		}
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

func (runtime *runtime) claimInterjection() (chan struct{}, bool) {
	now := runtime.scheduler.NowNS()
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	if runtime.interjectStartNS != 0 && now-runtime.interjectStartNS < uint64(interjectionStale) {
		return nil, false
	}
	claim := make(chan struct{})
	runtime.interjectStartNS = now
	runtime.interjectDone = claim
	return claim, true
}

func (runtime *runtime) releaseInterjection(claim chan struct{}) {
	runtime.audioMu.Lock()
	if runtime.interjectDone == claim {
		runtime.interjectStartNS = 0
		runtime.interjectDone = nil
	}
	close(claim)
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
// minimumBetweenSilentActs is how long must pass before the agent may act on
// the world again without saying anything.
//
// Three seconds. A recorded menu reads its options a few seconds apart, so a
// bound much longer than this would miss a real second choice, and a bound
// much shorter is no bound at all against a recogniser that starts a fresh
// utterance every few seconds.
const minimumBetweenSilentActs = 3 * time.Second

func (runtime *runtime) actSilently(decision interaction.Context) {
	if !runtime.hasActPolicy() {
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
	heard := strings.TrimSpace(decision.Revision.Text())
	nowNS := runtime.scheduler.NowNS()
	runtime.audioMu.Lock()
	sameRevision := runtime.lastSilentActRev == decision.Revision.ID
	// The same words, not the same stretch. Acting twice on one reason is what
	// this refuses, and a recording that has since named three options is not
	// one reason - measured, the act fired on "Thank you for calling." and
	// every revision after it extended that prefix, so one press at a greeting
	// that offered nothing disabled the rest of the call and the menu ended
	// where it started, after zero useful presses.
	//
	// What stops the nine presses that this guard was written for is the bound
	// in time below. It is the honest one: pressing twice for one reason is a
	// judgement about reasons, and growth in a revision is not evidence either
	// way, but three seconds between keypresses is a fact about phone menus.
	sameStretch := runtime.actedOnHeard != "" && heard == runtime.actedOnHeard
	// And a bound in time behind it, for the reason the interruption bound
	// carries one: the recogniser commits and starts a fresh utterance every
	// few seconds, so each piece of one recorded menu looks like a new stretch
	// of speech and the prefix test lets it through. Measured after the prefix
	// test was added, a single call still took six key presses - which lands
	// three menus deep.
	//
	// It is deliberately not the interruption's five seconds. Pressing a key
	// is not talking over somebody: a menu that genuinely offers a second
	// choice does so within a few seconds of the first, and a bound long
	// enough to be safe here would be long enough to miss it.
	tooSoon := runtime.lastSilentActNS != 0 && nowNS >= runtime.lastSilentActNS &&
		nowNS-runtime.lastSilentActNS < uint64(minimumBetweenSilentActs)
	if sameRevision || sameStretch || tooSoon {
		runtime.audioMu.Unlock()
		runtime.noteInterject("already acted, and nothing new has asked for another")
		return
	}
	runtime.lastSilentActNS = nowNS
	runtime.lastSilentActRev = decision.Revision.ID
	runtime.actedOnHeard = heard
	runtime.audioMu.Unlock()
	visualIntentID := runtime.currentUtterance()
	snapshot := runtime.store.Snapshot()
	visualIntentID, visualTask := runtime.liveVisualTask(
		snapshot, visualIntentID, decision.Revision.Text(), decision.Revision.ObservedNS,
	)
	claim, claimed := runtime.claimInterjection()
	if !claimed {
		runtime.noteInterject("acting silently, but something is already in flight")
		return
	}
	if runtime.visualRevisionHandled(visualIntentID, decision.Revision.ID, false) {
		runtime.releaseInterjection(claim)
		runtime.noteInterject("acting silently, but this visual revision was already handled")
		return
	}
	// The floor handoff and the fixed-cadence live trigger can be decided from
	// the same revision on adjacent goroutines. They share one visual actor
	// lane: whichever claims it first owns this micro-turn, and the other path
	// must not spend a second VLM call on the same pixels and words.
	if !runtime.visualPartial.CompareAndSwap(false, true) {
		runtime.releaseInterjection(claim)
		runtime.noteInterject("acting silently, but the live visual lane already owns this revision")
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.releaseInterjection(claim)
		defer runtime.releaseSilentVisualLane(visualIntentID, decision.Revision.ID)
		standing, _, _ := runtime.cognitionExtras(0)
		request := cognition.Request{
			SourceRevision: decision.Revision.ID, VisualIntentID: visualIntentID, VisualTask: visualTask,
			VisualUnstable: decision.Revision.UnstableText,
			Standing:       standing, Counting: runtime.countingIsInForce(),
			Because: string(interaction.ActActSilently),
			Heard:   decision.Revision.Text(), InFlight: inFlightToolNames(runtime.store.Snapshot()),
			CompletedVisualActions: runtime.completedVisualActionsForIntent(runtime.store.Snapshot(), visualIntentID),
		}
		// runSlow rather than the engine directly: a proposal that nobody
		// dispatches is a key nobody presses. The engine produces the call and
		// the runtime is what executes it, and calling past that layer meant
		// the phase ran nineteen times and the menu never heard a tone.
		ctx, cancel, generation := runtime.beginVisualDecision(runtime.ctx, interjectionDeadline)
		defer runtime.endVisualDecision(generation, cancel)
		err := runtime.runSilentAct(ctx, request)
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := "acted"
			if err != nil {
				outcome = "refused"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "act-silently: " + decision.Revision.Text(),
				Act: outcome, Predicates: map[string]string{"where": "act-silently"},
				Error: errorText(err),
			})
		}
	}()
}

func (runtime *runtime) releaseSilentVisualLane(intentID string, revision uint64) {
	runtime.markVisualRevisionHandled(intentID, revision, false)
	runtime.visualActionMu.Lock()
	if runtime.visualPending != nil && runtime.visualPending.intentID == intentID &&
		runtime.visualPending.context.Revision.ID <= revision {
		runtime.visualPending = nil
	}
	runtime.visualActionMu.Unlock()
	runtime.visualPartial.Store(false)

	// Preserve a genuinely newer revision that arrived while the bounded floor
	// act was in flight. The ordinary worker will coalesce and classify it; a
	// same-or-older duplicate was removed above.
	runtime.visualActionMu.Lock()
	queued := runtime.visualPending != nil
	runtime.visualActionMu.Unlock()
	if queued {
		runtime.startLiveVisualWorker()
	}
}

// runSilentAct gives a configured visual actor first refusal before the
// general reasoner. The interaction model has already decided that acting
// without speech is appropriate; this method decides only which cognition
// lane can ground the action. A visual tool call stays on the bounded fast
// authority surface, while abstention, timeout, or malformed output retains
// the existing slow fallback for arbitrary tools and deliberation.
func (runtime *runtime) runSilentAct(ctx context.Context, request cognition.Request) error {
	authority := interaction.VisualIntentNone
	if _, enabled := runtime.engine.VisualReflexDescriptor(); enabled &&
		strings.TrimSpace(request.VisualIntentID) != "" && strings.TrimSpace(request.VisualTask) != "" {
		authority = runtime.visualInteractionIntent(
			ctx, request.VisualIntentID, request.VisualTask, true, request.VisualUnstable,
		)
		switch authority {
		case interaction.VisualIntentNone:
			return runtime.runSlow(ctx, request, &turnReport{})
		case interaction.VisualIntentMonitor:
			runtime.armVisualMonitor(request.VisualIntentID)
			return runtime.signalCompositeResumeOnce(request.VisualIntentID, request.Heard)
		}
	}
	if explicitVisualActionsComplete(request) {
		return runtime.resumeImmediateNonvisualOnce(
			request.VisualIntentID, request.VisualTask, request.Heard,
		)
	}
	if authority == interaction.VisualIntentDirect && runtime.visualAwaitingFreshFrame() {
		// A delayed floor decision can arrive after the ordinary live worker has
		// already dispatched this intent's preceding action and released the
		// shared lane. It still sees the pre-action image; retain the newer words
		// as controller state, but never ground another coordinate until adaptive
		// observation admits post-action pixels.
		return nil
	}
	outcome, reflexErr := runtime.engine.RunVisualReflex(ctx, request)
	if reflexErr == nil {
		if targetErr := validateVisualTarget(request, outcome); targetErr != nil {
			runtime.profileVisualOutcome("silent-rejected", request, outcome, targetErr)
			if _, placeholderErr := runtime.engine.PlaceholderCalls(
				outcome.Result.ToolCalls, "visual target rejected: "+targetErr.Error(),
			); placeholderErr != nil {
				return placeholderErr
			}
			return targetErr
		}
		runtime.applyVisualOutcome(request.VisualIntentID, outcome)
		switch outcome.Kind {
		case cognition.VisualReflexAct:
			return runtime.dispatchAutonomousVisual(ctx, outcome.Result, request.VisualIntentID)
		case cognition.VisualReflexWait:
			// The current frame needs no action. A later visual observation will
			// ask the reflex again; meanwhile, preserve any independent work in
			// this utterance (for example, present now while monitoring). The
			// signal re-enters normal turn bracketing so this silent goroutine
			// never becomes a second speech path.
			runtime.rememberCompositeResumeHeard(request.Heard)
			if err := runtime.signal(interaction.SignalCompositeResume); err != nil {
				runtime.clearCompositeResumeHeard(request.Heard)
				return err
			}
			return nil
		case cognition.VisualReflexAbstain:
			// A typed direct-screen command stays on the direct-pixel lane. The
			// next ASR revision or canonical endpoint may retry it; a visionless
			// slow fallback would add latency and cannot ground coordinates.
		}
	}
	if authority == interaction.VisualIntentDirect {
		return reflexErr
	}
	return runtime.runSlow(ctx, request, &turnReport{})
}

// considerLiveVisual opens the fast pixel-grounded micro-turn directly from a
// live ASR revision. The trigger already bounds opportunities to the selected
// cadence (200 ms in the meeting profile); this adds one in-flight bound and
// coalesces arrivals during that work to only their newest revision.
//
// Unlike ActActSilently, this is not a judgement that speech is unwelcome. It
// is the independent computer-control branch of a multimodal request: a person
// may keep speaking while a deadline-sensitive explicit UI operation becomes
// clear. The visual-intent policy grants coordinate authority; the VLM grounds
// at most one action; a fresh frame is required before any continuation.
type liveVisualDecision struct {
	context      interaction.Context
	intentID     string
	groundVisual bool
}

func (runtime *runtime) considerLiveVisual(decision interaction.Context) {
	if !runtime.liveVisualIntentPolicyEnabled() || decision.Revision.Empty() ||
		runtime.engine == nil || !snapshotHasVisual(runtime.store.Snapshot()) {
		return
	}
	if _, enabled := runtime.engine.VisualReflexDescriptor(); !enabled {
		return
	}
	// The interaction floor already selected a silent act for this instant.
	// Let that path own it rather than racing two entries into the same actor.
	runtime.audioMu.Lock()
	interjectionRunning := runtime.interjectStartNS != 0
	runtime.audioMu.Unlock()
	if interjectionRunning {
		return
	}
	intentID := runtime.currentUtterance()
	if strings.TrimSpace(intentID) == "" {
		return
	}
	if runtime.visualRevisionHandled(intentID, decision.Revision.ID, false) {
		return
	}
	// Keep only the latest revision while the VLM is occupied. The next worker
	// iteration observes all words heard so far without paying for obsolete
	// intermediate prefixes. This is receding-horizon coalescing, not a model-
	// call queue: there remains exactly one in-flight visual micro-turn.
	runtime.visualActionMu.Lock()
	pending := &liveVisualDecision{context: decision, intentID: intentID}
	runtime.visualPending = pending
	runtime.visualLast = pending
	runtime.visualActionMu.Unlock()
	runtime.startLiveVisualWorker()
}

func (runtime *runtime) liveVisualIntentPolicyEnabled() bool {
	return runtime.policies.Interaction != nil || runtime.config.RequireExplicitVisualAuthority
}

func (runtime *runtime) startLiveVisualWorker() {
	if !runtime.visualPartial.CompareAndSwap(false, true) {
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		runtime.runLiveVisualMicroTurns()
	}()
}

func (runtime *runtime) runLiveVisualMicroTurns() {
	for runtime.ctx.Err() == nil {
		runtime.visualActionMu.Lock()
		pending := runtime.visualPending
		runtime.visualPending = nil
		runtime.visualActionMu.Unlock()
		if pending != nil {
			if runtime.visualAwaitingFreshFrame() {
				runtime.absorbLiveVisualDecision(*pending)
				continue
			}
			runtime.runLiveVisualMicroTurn(*pending)
			continue
		}

		// Hand the lane back without losing a revision that races this edge.
		// A caller that sees false starts its own worker; a revision queued while
		// the flag was still true is claimed here.
		runtime.visualPartial.Store(false)
		runtime.visualActionMu.Lock()
		queued := runtime.visualPending != nil
		runtime.visualActionMu.Unlock()
		if !queued || !runtime.visualPartial.CompareAndSwap(false, true) {
			return
		}
	}
	runtime.visualPartial.Store(false)
}

// absorbLiveVisualDecision retains semantic user evidence that arrived after
// an ACT without grounding another coordinate against the pre-action frame.
// The interaction policy may classify those new words now; the visual actor
// remains blocked until admitFreshVisualObservation sees post-action pixels.
func (runtime *runtime) absorbLiveVisualDecision(pending liveVisualDecision) {
	snapshot := runtime.store.Snapshot()
	intentID, task := "", ""
	if pending.groundVisual {
		// Observer work uses the controller task already reconstructed from the
		// canonical utterance. Running wholeUtterance again with the final text
		// can append that same utterance to itself and needlessly perturb both
		// policy caching and pixel grounding.
		intentID, task = runtime.visualTask(snapshot, false)
	} else {
		intentID, task = runtime.liveVisualTask(
			snapshot, pending.intentID, pending.context.Revision.Text(),
			pending.context.Revision.ObservedNS,
		)
	}
	if intentID == "" || task == "" {
		return
	}
	_ = runtime.visualInteractionIntent(runtime.ctx, intentID, task, true)
	runtime.visualActionMu.Lock()
	copy := pending
	runtime.visualDeferred = &copy
	runtime.visualLast = &copy
	runtime.visualActionMu.Unlock()
}

func (runtime *runtime) runLiveVisualMicroTurn(pending liveVisualDecision) {
	if runtime.visualRevisionHandled(pending.intentID, pending.context.Revision.ID, pending.groundVisual) {
		return
	}
	runtime.visualActionMu.Lock()
	copy := pending
	runtime.visualLast = &copy
	runtime.visualActionMu.Unlock()
	snapshot := runtime.store.Snapshot()
	intentID, task := runtime.liveVisualTask(
		snapshot, pending.intentID, pending.context.Revision.Text(),
		pending.context.Revision.ObservedNS,
	)
	defer runtime.markVisualRevisionHandled(intentID, pending.context.Revision.ID, pending.groundVisual)
	if intentID == "" || task == "" || !runtime.visualIntentEligible(intentID) {
		return
	}
	authority := runtime.visualInteractionIntent(
		runtime.ctx, intentID, task, true, pending.context.Revision.UnstableText,
	)
	if authority == interaction.VisualIntentNone || runtime.ctx.Err() != nil {
		return
	}
	// A monitor request is authority to evaluate later frames, not a reason to
	// spend a VLM call proving that the named future condition has not happened
	// on the retained frame. Arming here also keeps the independent fast voice
	// free to begin speaking at the endpoint. The next observer frame remains
	// direct pixel evidence and performs the actual act/wait decision.
	if authority == interaction.VisualIntentMonitor && !pending.groundVisual {
		runtime.armVisualMonitor(intentID)
		if runtime.config.ProfileTurns {
			fmt.Fprintf(os.Stderr,
				"visual-profile at=%s where=live-policy intent=%q task=%q outcome=armed-monitor\n",
				time.Now().UTC().Format(time.RFC3339Nano), intentID, task)
		}
		if err := runtime.signalCompositeResumeOnce(intentID, pending.context.Revision.Text()); err != nil {
			runtime.fail("visual_composite_resume_error", err)
		}
		return
	}
	standing, _, _ := runtime.cognitionExtras(0)
	request := cognition.Request{
		SourceRevision: pending.context.Revision.ID, VisualIntentID: intentID, VisualTask: task,
		VisualUnstable: pending.context.Revision.UnstableText,
		Standing:       standing, Counting: runtime.countingIsInForce(), Silent: true,
		Heard: pending.context.Revision.Text(), InFlight: inFlightToolNames(runtime.store.Snapshot()),
		CompletedVisualActions: runtime.completedVisualActionsForIntent(runtime.store.Snapshot(), intentID),
	}
	if explicitVisualActionsComplete(request) {
		// The newest ASR prefix contains no complete screen command beyond the
		// chunks already executed. Wait for the clause to finish instead of
		// asking the pixel actor to infer a second target from words like
		// "share" or "share your".
		if err := runtime.resumeImmediateNonvisualOnce(intentID, task, pending.context.Revision.Text()); err != nil {
			runtime.fail("visual_composite_resume_error", err)
		}
		return
	}
	decisionCtx, cancel, generation := runtime.beginVisualDecision(runtime.ctx, 0)
	outcome, err := runtime.engine.RunVisualReflex(decisionCtx, request)
	runtime.endVisualDecision(generation, cancel)
	where := "live"
	if pending.groundVisual {
		where = "observer"
	}
	runtime.profileVisualOutcome(where, request, outcome, err)
	if err != nil {
		return
	}
	if targetErr := validateVisualTarget(request, outcome); targetErr != nil {
		runtime.profileVisualOutcome(where+"-rejected", request, outcome, targetErr)
		if _, placeholderErr := runtime.engine.PlaceholderCalls(
			outcome.Result.ToolCalls, "visual target rejected: "+targetErr.Error(),
		); placeholderErr != nil {
			runtime.fail("visual_target_placeholder_error", placeholderErr)
			return
		}
		if strings.TrimSpace(request.VisualUnstable) != "" {
			runtime.armProvisionalVisualRetry(intentID, task, pending)
		}
		return
	}
	if outcome.Kind == cognition.VisualReflexAbstain && !pending.context.Revision.Final {
		runtime.visualActionMu.Lock()
		runtime.visualProvisionalTerminal[intentID] = true
		runtime.visualActionMu.Unlock()
	}
	runtime.applyVisualOutcome(intentID, outcome)
	if outcome.Kind == cognition.VisualReflexAct {
		if err := runtime.dispatchAutonomousVisual(runtime.ctx, outcome.Result, intentID); err != nil {
			if runtime.config.ProfileTurns {
				fmt.Fprintf(os.Stderr,
					"visual-profile at=%s where=%s-dispatch intent=%q task=%q error=%q\n",
					time.Now().UTC().Format(time.RFC3339Nano), where, intentID, task, err.Error())
			}
			runtime.fail("live_visual_action_error", err)
		} else if !outcome.Continue {
			if err := runtime.resumeImmediateNonvisualOnce(intentID, task, pending.context.Revision.Text()); err != nil {
				runtime.fail("visual_composite_resume_error", err)
			}
		}
	} else if outcome.Kind == cognition.VisualReflexWait && !pending.groundVisual {
		heard := pending.context.Revision.Text()
		runtime.rememberCompositeResumeHeard(heard)
		if err := runtime.signal(interaction.SignalCompositeResume); err != nil {
			runtime.clearCompositeResumeHeard(heard)
			runtime.fail("visual_composite_resume_error", err)
		}
	} else if outcome.Kind == cognition.VisualReflexAbstain && request.CompletedVisualActions > 0 {
		if err := runtime.resumeImmediateNonvisualOnce(intentID, task, pending.context.Revision.Text()); err != nil {
			runtime.fail("visual_composite_resume_error", err)
		}
	}
}

func (runtime *runtime) resumeImmediateNonvisualOnce(intentID, task, heard string) error {
	if interaction.ImmediateNonvisualClause(task) == "" {
		return nil
	}
	return runtime.signalCompositeResumeOnce(intentID, heard)
}

const provisionalVisualConfirmation = 200 * time.Millisecond

// armProvisionalVisualRetry is temporal confirmation, not endpointing. If the
// same reconstructed task survives one more 200 ms micro-turn without a newer
// ASR revision, retry it as confirmed text against the same current pixels.
// This preserves the fast loop while preventing one transient "over" from
// being autocompleted into a visible destination.
func (runtime *runtime) armProvisionalVisualRetry(
	intentID, task string, pending liveVisualDecision,
) {
	if runtime.scheduler == nil || strings.TrimSpace(intentID) == "" || strings.TrimSpace(task) == "" {
		return
	}
	revision := pending.context.Revision.ID
	runtime.visualActionMu.Lock()
	runtime.visualProvisionalRetry[intentID] = revision
	runtime.visualActionMu.Unlock()
	runtime.scheduler.AfterFunc(provisionalVisualConfirmation, func() {
		if runtime.ctx.Err() != nil {
			return
		}
		runtime.visualActionMu.Lock()
		if runtime.visualProvisionalRetry[intentID] != revision ||
			runtime.visualTaskID != intentID || runtime.visualTaskText != task {
			runtime.visualActionMu.Unlock()
			return
		}
		delete(runtime.visualProvisionalRetry, intentID)
		copy := pending
		copy.intentID = runtime.visualTaskRawID
		copy.context.NowNS = runtime.scheduler.NowNS()
		copy.context.Revision = interaction.Revision{
			ID: revision, StableText: task, ObservedNS: runtime.scheduler.NowNS(),
		}
		runtime.visualPending = &copy
		runtime.visualLast = &copy
		runtime.visualActionMu.Unlock()
		runtime.startLiveVisualWorker()
	})
}

// visualHandledRevisions keeps provider-local counters in separate domains.
// Audio ASR and the screen observer both start their revisions at one; treating
// those unrelated numbers as a single sequence can make a later screen change
// look older than the user utterance that armed its monitor.
type visualHandledRevisions struct {
	acoustic uint64
	grounded uint64
}

func (runtime *runtime) visualRevisionHandled(intentID string, revision uint64, grounded bool) bool {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" || revision == 0 {
		return false
	}
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	handled := runtime.visualHandledRev[intentID]
	if grounded {
		return handled.grounded >= revision
	}
	return handled.acoustic >= revision
}

func (runtime *runtime) markVisualRevisionHandled(intentID string, revision uint64, grounded bool) {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" || revision == 0 {
		return
	}
	runtime.visualActionMu.Lock()
	handled := runtime.visualHandledRev[intentID]
	if grounded {
		if revision > handled.grounded {
			handled.grounded = revision
		}
	} else if revision > handled.acoustic {
		handled.acoustic = revision
	}
	runtime.visualHandledRev[intentID] = handled
	runtime.visualActionMu.Unlock()
}

// liveVisualTask preserves controller intent across recognition-created
// fragments before any of them necessarily exists in the canonical log.
func (runtime *runtime) liveVisualTask(
	snapshot trajectory.Snapshot, rawID, heard string, observedNS uint64,
) (string, string) {
	rawID, heard = strings.TrimSpace(rawID), strings.TrimSpace(heard)
	if rawID == "" || heard == "" {
		return "", ""
	}
	gap, adjacent := runtime.gapBeforeUtteranceNS(snapshot)
	runtime.visualActionMu.Lock()
	defer runtime.visualActionMu.Unlock()
	currentRawID := runtime.visualTaskRawID
	if currentRawID == "" {
		currentRawID = runtime.visualTaskID
	}
	liveGap := observedNS > runtime.visualTaskObservedNS && runtime.visualTaskObservedNS != 0 &&
		observedNS-runtime.visualTaskObservedNS > uint64(visualTaskContinuationGap) &&
		!trajectory.SaidFurther(runtime.visualTaskText, heard) &&
		!trajectory.SaidFurther(heard, runtime.visualTaskText) && heard != runtime.visualTaskText
	newControllerTask := runtime.visualTaskID == "" ||
		liveGap || (rawID != currentRawID && (!adjacent || gap > uint64(visualTaskContinuationGap)))
	if newControllerTask {
		// wholeUtterance intentionally spans until the assistant audibly answers.
		// That is the right scope for conversational policy, but not for effect
		// authority: a person can correct a completed screen action while the
		// assistant is still presenting or the slow lane is still working. The
		// new acoustic utterance owns only its own cumulative ASR text.
		controllerID := rawID
		if liveGap && rawID == currentRawID {
			// An interaction floor can deliberately hold one acoustic turn open
			// across a long pause. The recognizer then retains its correlation ID,
			// but screen-effect authority must still distinguish the later spoken
			// correction. The observed-time suffix is internal controller lineage;
			// the canonical audio event keeps its original correlation unchanged.
			controllerID += "/visual-" + strconv.FormatUint(observedNS, 10)
		}
		runtime.visualTaskID, runtime.visualTaskRawID, runtime.visualTaskText = controllerID, rawID, heard
		runtime.visualTaskObservedNS = observedNS
		runtime.visualEvaluated[controllerID] = false
		return runtime.visualTaskID, runtime.visualTaskText
	}
	if observedNS > runtime.visualTaskObservedNS {
		runtime.visualTaskObservedNS = observedNS
	}
	if asrWithinWordRegression(heard, runtime.visualTaskText) {
		return runtime.visualTaskID, runtime.visualTaskText
	}
	task := heard
	if rawID != currentRawID {
		// A recognizer-created acoustic tail has a new ID but belongs to the
		// controller task inside the short continuation horizon. Join from the
		// controller's own text, never from the wider unanswered voice turn.
		switch {
		case trajectory.SaidFurther(runtime.visualTaskText, heard):
			task = heard
		case trajectory.SaidFurther(heard, runtime.visualTaskText):
			task = runtime.visualTaskText
		case !strings.Contains(runtime.visualTaskText, heard):
			task = strings.TrimSpace(runtime.visualTaskText + " " + heard)
		default:
			task = runtime.visualTaskText
		}
		runtime.visualTaskRawID = rawID
	}
	// A fresh frame may wake a decision captured before a newer ASR revision
	// arrived. Never let that older work shrink the reconstructed task: doing
	// so erases a trailing ordered control ("...and share your screen") and
	// makes the next frame look as though only the first action was requested.
	if trajectory.SaidFurther(task, runtime.visualTaskText) {
		task = runtime.visualTaskText
	}
	if task != "" && task != runtime.visualTaskText {
		runtime.visualTaskText = task
		runtime.visualEvaluated[runtime.visualTaskID] = false
	}
	return runtime.visualTaskID, runtime.visualTaskText
}

func asrWithinWordRegression(candidate, current string) bool {
	candidateWords, currentWords := trajectory.SpokenWords(candidate), trajectory.SpokenWords(current)
	if len(candidateWords) == 0 || len(candidateWords) != len(currentWords) {
		return false
	}
	for index := 0; index < len(candidateWords)-1; index++ {
		if candidateWords[index] != currentWords[index] {
			return false
		}
	}
	last := len(candidateWords) - 1
	return len(candidateWords[last]) < len(currentWords[last]) &&
		strings.HasPrefix(currentWords[last], candidateWords[last])
}

func (runtime *runtime) rememberCompositeResumeHeard(heard string) {
	runtime.audioMu.Lock()
	runtime.compositeResumeHeard = strings.TrimSpace(heard)
	runtime.audioMu.Unlock()
}

func (runtime *runtime) signalCompositeResumeOnce(intentID, heard string) error {
	intentID = strings.TrimSpace(intentID)
	if intentID == "" {
		return nil
	}
	runtime.visualActionMu.Lock()
	if runtime.visualResumed[intentID] {
		runtime.visualActionMu.Unlock()
		return nil
	}
	runtime.visualResumed[intentID] = true
	runtime.visualActionMu.Unlock()
	runtime.rememberCompositeResumeHeard(heard)
	if err := runtime.signal(interaction.SignalCompositeResume); err != nil {
		runtime.clearCompositeResumeHeard(heard)
		runtime.visualActionMu.Lock()
		delete(runtime.visualResumed, intentID)
		runtime.visualActionMu.Unlock()
		return err
	}
	return nil
}

func (runtime *runtime) clearCompositeResumeHeard(heard string) {
	runtime.audioMu.Lock()
	if runtime.compositeResumeHeard == strings.TrimSpace(heard) {
		runtime.compositeResumeHeard = ""
	}
	runtime.audioMu.Unlock()
}

func (runtime *runtime) takeCompositeResumeHeard() string {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	heard := runtime.compositeResumeHeard
	runtime.compositeResumeHeard = ""
	return heard
}

// quietAfter is how long nothing may happen before the quiet is itself
// evidence.
//
// Below this the pause path already governs: it runs on every gate close and
// asks the same model the same question with the words in front of it. This is
// for the stretch after that, where there is no utterance to hold open and
// nothing else will ever ask again. Three seconds because a policy about
// elapsed time is written in seconds - nobody says "tell me if I go quiet for
// four hundred milliseconds" - and because it keeps ordinary pauses between
// sentences out of a decision they are already covered by.
const quietAfter = 3 * time.Second

// quietInterval is how often the question is asked while the quiet lasts.
//
// The decision costs 30 to 40 milliseconds against a local mixture-of-experts,
// so once a second while nothing at all is happening is affordable in a way it
// would not be per frame.
const quietInterval = time.Second

// considerQuiet asks what to do when nothing has happened.
//
// Every other route to this model is caused by an arrival - words, a frame, a
// result. That is the right default and it has one hole in it: somebody can
// ask to be told about something that happens on its own, and the moment they
// mean is exactly the one where nothing arrives. Measured, the agent
// acknowledged "ask whether I'm still there if I go quiet for fifteen seconds"
// and then never asked, because after the acknowledgement nothing ever
// consulted anything again.
//
// It is gated on a policy being in force, which is what keeps the inertia
// intact: with nothing standing, the quiet decides nothing and this costs one
// comparison per frame.
func (runtime *runtime) considerQuiet(ctx context.Context, nowNS uint64, state session.Snapshot) {
	if runtime.policies.Interaction == nil {
		return
	}
	since := state.UserSpeechEndedNS
	if state.PlayoutHorizonNS > since {
		since = state.PlayoutHorizonNS
	}
	if since == 0 || nowNS < since || nowNS-since < uint64(quietAfter) {
		return
	}
	runtime.audioMu.Lock()
	if runtime.lastQuietNS != 0 && nowNS-runtime.lastQuietNS < uint64(quietInterval) {
		runtime.audioMu.Unlock()
		return
	}
	runtime.lastQuietNS = nowNS
	runtime.audioMu.Unlock()

	// Only for a policy that waits on a stretch of quiet and has had it.
	//
	// A policy with no delay is not about quiet at all - it waits for
	// something to happen, and nothing happening is not that. Gating on the
	// number here is what lets the model be asked a question it can answer:
	// asked to compare fifteen seconds against a silence line reading 8199ms
	// it said speak five times out of five, and a paragraph telling it to
	// compare fixed one phrasing of the transcript and left a near-identical
	// one still wrong.
	quiet := time.Duration(nowNS - since)
	due := false
	for _, standing := range runtime.pinboard.InForce() {
		if standing.After > 0 && standing.Due(quiet) {
			due = true
			break
		}
	}
	if !due {
		return
	}
	decision := interaction.Context{NowNS: nowNS, Duplex: state}
	situation := runtime.situation(decision)
	situation.Quiet = true
	situation.Silence = renderSilence(nowNS - since)
	decision.Situation = &situation
	act, _, err := runtime.policies.Interaction.Decide(ctx, situation)
	if recorder := runtime.policies.ShadowInteraction; recorder != nil {
		recorder(interaction.ShadowDecision{
			NowNS: nowNS, Situation: situation.Render(), Act: string(act),
			Predicates: map[string]string{"where": "quiet"}, Error: errorText(err),
		})
	}
	// answer, not only speak-through. Nobody holds the floor here, so the act
	// for saying something is the ordinary one - and measured, the model chose
	// it sixty-six times out of sixty-six while the runtime listened for the
	// other one and dropped every one of them. Speaking through is what you do
	// over somebody; there is nobody to speak over in a silence.
	if err != nil || (act != interaction.ActAnswer && act != interaction.ActSpeakThrough) {
		return
	}
	runtime.audioMu.Lock()
	spoken := runtime.quietSpokeSince == since
	runtime.quietSpokeSince = since
	runtime.audioMu.Unlock()
	if spoken {
		// One per stretch of quiet, for the reason every other act here is
		// bounded that way: the silence does not stop being evidence once it
		// has been acted on, so without this the agent asks whether they are
		// still there once a second for as long as they are not.
		return
	}
	runtime.speakIntoSilence(decision, act, quiet)
}

// speakIntoSilence runs a turn when nobody is speaking and nothing arrived.
//
// interject cannot serve this. It guards on somebody still holding the floor -
// "speaking into a turn that has since ended is worse than not speaking" -
// which is right for every case it was written for and false for this one,
// where the whole point is that the turn ended long ago and nothing has
// happened since.
func (runtime *runtime) speakIntoSilence(
	decision interaction.Context, act interaction.Act, quiet time.Duration,
) {
	claim, claimed := runtime.claimInterjection()
	if !claimed {
		runtime.noteInterject("the quiet is worth speaking into, but something is already in flight")
		return
	}
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		defer runtime.releaseInterjection(claim)
		standing, _, _ := runtime.cognitionExtras(0)
		request := cognition.Request{
			SourceRevision: decision.Revision.ID,
			Standing:       standing, Counting: runtime.countingIsInForce(),
			Because: string(act),
			// The silence is the thing that happened, and a turn with nothing
			// new in front of it produces nothing at all.
			Observed: "nobody has said anything for " + renderSilence(uint64(quiet)),
		}
		ctx, cancel := context.WithTimeout(runtime.ctx, interjectionDeadline)
		defer cancel()
		err := runtime.runFast(ctx, request, &turnReport{}, true, false)
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := "spoke"
			if err != nil {
				outcome = "refused"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "quiet: spoke into the silence",
				Act: outcome, Predicates: map[string]string{"where": "quiet"},
				Error: errorText(err),
			})
		}
	}()
}
