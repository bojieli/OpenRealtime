package cascade

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception/voices"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// situation assembles what an interaction model decides from.
//
// Everything here is read out of state the runtime already keeps. That is the
// point: the decision is about the live instant, and anything assembled from
// somewhere else would be describing a moment that has already passed. The
// order the fields are rendered in is fixed by the cache rather than by
// readability - instructions and conversation change rarely, the instant
// changes on every partial, and reversing them would invalidate the prefix on
// every decision.
func (runtime *runtime) situation(decision interaction.Context) interaction.Situation {
	snapshot := runtime.store.Snapshot()
	state := interaction.Situation{
		Contract:      runtime.Settings().Instruction,
		Pins:          runtime.pinboard.Lines(decision.NowNS),
		Recent:        runtime.window.Lines(snapshot.Items),
		AgentSpeaking: decision.Duplex.AgentSpeaking,
		Speaker:       runtime.speakerNow(snapshot),
		Speaking:      decision.Duplex.UserSpeaking,
		Heard:         strings.TrimSpace(decision.Revision.Text()),
		HeardSince:    runtime.heardSinceSpeaking(strings.TrimSpace(decision.Revision.Text())),
		Silence:       renderSilence(decision.Revision.SilenceNS),
		SincePrevious: runtime.gapBeforeUtterance(snapshot),
		InFlight:      workInFlight(snapshot),
		Seen:          lastSeen(snapshot),
		Seeing:        runtime.lastFrame(snapshot),
		Tools:         runtime.toolLines(),
	}
	if state.AgentSpeaking {
		state.AgentSaying = speakingNow(snapshot)
	}
	return state
}

// renderSilence states a pause the way a decision needs it.
//
// Milliseconds, not the coarser form the cognition projection uses. Three
// hundred milliseconds is nothing to a reader of a transcript and is the whole
// question here, which is why the two projections of the same clock differ.
func renderSilence(silenceNS uint64) string {
	return fmt.Sprintf("%dms", time.Duration(silenceNS).Milliseconds())
}

// speakingNow is what the agent has said so far in the sentence it is in the
// middle of. Without it, an overlap decision is being taken with only half the
// overlap visible.
func speakingNow(snapshot trajectory.Snapshot) string {
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind == trajectory.KindAssistant && strings.TrimSpace(item.Content) != "" {
			return strings.TrimSpace(item.Content)
		}
	}
	return ""
}

// inFlight is what has already been decided and is still running. It is the
// only thing standing between a phone menu that keeps talking and an agent
// that presses the same key four times.
func workInFlight(snapshot trajectory.Snapshot) string {
	pending := trajectory.UnresolvedToolCalls(snapshot)
	if len(pending) == 0 {
		return ""
	}
	names := make([]string, 0, len(pending))
	for _, call := range pending {
		names = append(names, call.Call.Name+" awaiting its result")
	}
	return strings.Join(names, ", ")
}

// lastSeen is the newest thing the agent looked at rather than heard.
//
// An observer's own observation is one route to that. The other is a picture a
// client put into the conversation, which carries user authority because a
// person attaching a screenshot is a person talking - and is still, entirely,
// something seen. Reading only the first route left the whole visual axis
// invisible to a decision layer that cannot see, on the exact path every
// Realtime client uses to send an image.
func lastSeen(snapshot trajectory.Snapshot) string {
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation {
			continue
		}
		observer := trajectory.AuthorityOf(item) == trajectory.AuthorityObserver
		attached := item.Observation != nil && len(item.Observation.Media) > 0
		if !observer && !attached {
			continue
		}
		return strings.TrimSpace(item.Content)
	}
	return ""
}

// toolLines names what the agent could do without speaking. An act offered
// with nothing able to perform it is an act a model cannot sensibly choose.
func (runtime *runtime) toolLines() []string {
	specs := runtime.registry.Specs()
	if len(specs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(specs))
	for _, spec := range specs {
		line := spec.Name
		if description := strings.TrimSpace(spec.Description); description != "" {
			line += " - " + description
		}
		lines = append(lines, line)
	}
	return lines
}

// beginShadow captures the instant the predicates are about to act on.
//
// It is taken before them rather than after because the two decisions have to
// be about the same moment. A situation assembled once the floor has already
// ended the turn describes what the predicates did, not what they were looking
// at, and comparing an answer to that is comparing answers to two questions.
func (runtime *runtime) beginShadow(decision interaction.Context) *interaction.Situation {
	if runtime.policies.ShadowInteraction == nil || decision.Situation == nil {
		return nil
	}
	return decision.Situation
}

// endShadow asks the interaction model and records both answers.
func (runtime *runtime) endShadow(
	state *interaction.Situation, decision interaction.Context, projected, triggered bool,
) {
	if state == nil {
		return
	}
	// Off the audio path. A shadow decision costs a model call, and one taken
	// synchronously would delay the partials the predicates are judging - the
	// measurement would change the behaviour it exists to record, and the
	// disagreements it found would be partly its own fault.
	runtime.wait.Add(1)
	go runtime.shadowDecide(*state, decision, projected, triggered)
}

func (runtime *runtime) shadowDecide(
	state interaction.Situation, decision interaction.Context, projected, triggered bool,
) {
	defer runtime.wait.Done()
	ctx := runtime.ctx
	started := runtime.scheduler.NowNS()
	act, _, err := runtime.policies.Interaction.Decide(ctx, state)
	if ctx.Err() != nil {
		return
	}
	record := interaction.ShadowDecision{
		NowNS: decision.NowNS, Situation: state.Render(), Act: string(act),
		Predicates: map[string]string{
			"endpoint_projected": strconv.FormatBool(projected),
			"trigger_open":       strconv.FormatBool(triggered),
			"agent_speaking":     strconv.FormatBool(decision.Duplex.AgentSpeaking),
			"user_speaking":      strconv.FormatBool(decision.Duplex.UserSpeaking),
		},
		ElapsedNS: runtime.scheduler.NowNS() - started,
	}
	if err != nil {
		record.Error = err.Error()
	}
	record.Agreed = act == predicateAct(state, projected)
	runtime.policies.ShadowInteraction(record)
}

// predicateAct is what the shipped policies collectively did, named in the
// vocabulary the interaction model answers in.
//
// The translation is lossy in one direction only, and knowingly so: the four
// predicates have no way to express speaking through somebody, interrupting
// them, or acting without speaking, because none of those was reachable before
// this. Every such answer will read as a disagreement, which is correct - it
// is the new capability showing up in the corpus rather than an error in it.
func predicateAct(state interaction.Situation, projected bool) interaction.Act {
	if projected {
		return interaction.ActAnswer
	}
	return interaction.InertialAct(state)
}

// noticeStanding runs the pass that lifts an interaction policy out of what
// somebody just said.
//
// Off the critical path, and deliberately so. A policy governs what happens
// next rather than what happens now, so it has to land before the following
// utterance rather than before this reply - a budget of hundreds of
// milliseconds where the decision it feeds has tens.
func (runtime *runtime) noticeStanding(text string) {
	if runtime.policies.Extraction == nil || !v1.CarriesSpeech(text) {
		return
	}
	// Once per stretch of text. The partial path calls this as an utterance
	// grows, and re-reading the same sentence would spend a model call to
	// learn what is already pinned.
	runtime.audioMu.Lock()
	if text == runtime.extractedText {
		runtime.audioMu.Unlock()
		return
	}
	runtime.extractedText = text
	runtime.audioMu.Unlock()
	snapshot := runtime.store.Snapshot()
	// A new utterance retires the one before it. Within one utterance this
	// runs many times as the text grows, and the piece to join onto has to
	// stay the piece before rather than becoming what the last reading made.
	current := runtime.currentUtterance()
	runtime.audioMu.Lock()
	if current != runtime.extractUtterance {
		runtime.previousUtterance = runtime.extractUtteranceText
		runtime.previousPin, runtime.lastPin = runtime.lastPin, interaction.StandingInstruction{}
		runtime.extractUtterance = current
		runtime.extractUtteranceText = ""
	}
	runtime.audioMu.Unlock()
	whole, stale := runtime.wholeUtterance(snapshot, text)
	// The joined text, not this piece: a sentence cut into three joins onto
	// what the first two already made.
	runtime.audioMu.Lock()
	runtime.extractUtteranceText = whole
	runtime.audioMu.Unlock()
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		if stale.Text != "" {
			// The policy the front half produced was read off half a sentence.
			// It goes before the whole one is read, or the pass is comparing
			// what somebody said against a truncation of it - measured, with
			// "tell me the moment the build" standing, the tail "finishes and
			// don't say anything else" revoked it five times out of five.
			runtime.pinboard.Revoke(stale.Text)
		}
		// The conversation, not just the utterance: a recogniser splits where a
		// speaker breathes, and a fragment read alone means something else.
		recent := interaction.RecentLines(snapshot.Items, 6)
		extraction, err := runtime.policies.Extraction.Extract(
			runtime.ctx, runtime.pinboard.InForce(), recent, whole)
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := extraction.Kind
			if err != nil {
				outcome = "error"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "extract: " + whole,
				Act: outcome,
				Predicates: map[string]string{
					"where": "extract", "scope": string(extraction.Instruction.Scope),
					"text": extraction.Instruction.Text,
				},
				Error: errorText(err),
			})
		}
		if err != nil || runtime.ctx.Err() != nil {
			// A policy that could not be read is a policy nobody recorded,
			// which is the failure this leans away from - but inventing one
			// from an answer nobody understood is worse.
			return
		}
		switch extraction.Kind {
		case "pin":
			instruction := extraction.Instruction
			instruction.SetNS = runtime.scheduler.NowNS()
			runtime.pinboard.Pin(instruction)
			runtime.audioMu.Lock()
			runtime.lastPin = instruction
			runtime.audioMu.Unlock()
		case "revoke":
			runtime.pinboard.Revoke(extraction.Instruction.Text)
		}
	}()
}

// setInterjecting records that the turn for a particular revision was taken
// rather than offered.
//
// Keyed by revision, because a single shared flag does not survive the trip.
// The floor is consulted on every partial and clears the flag for every act
// that is not an interruption, so between the decision that set it and the
// turn that reads it there are several chances for it to be reset - and
// measured, the voice that answers perfectly when told it is correcting
// something invented a date instead, because by then nothing was telling it.
func (runtime *runtime) setInterjecting(revision uint64, interjecting bool) {
	if !interjecting {
		return
	}
	runtime.audioMu.Lock()
	runtime.interjectingRev = revision
	runtime.audioMu.Unlock()
}

// tookTheFloor reports whether the turn for this revision was taken from
// somebody mid-sentence.
func (runtime *runtime) tookTheFloor(revision uint64) bool {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	return revision != 0 && runtime.interjectingRev == revision
}

// cognitionExtras is what cognition needs beyond the trajectory, so that the
// three models see one conversation rather than three.
//
// They cannot see identical context and should not: the interaction model
// needs milliseconds of silence where the voice needs none, and the voice needs
// the whole log where the interaction model needs a bounded window. What they
// must not differ on is *what happened*. Interaction decides on partials and
// cognition reads committed items, so anything still in a partial is invisible
// to the voice unless it is carried across - and a turn triggered by a partial
// then reaches the voice with its own cause missing.
func (runtime *runtime) cognitionExtras() (standing []string, interjecting bool, heard string) {
	runtime.audioMu.Lock()
	latest := runtime.heard
	runtime.audioMu.Unlock()
	interjecting = runtime.tookTheFloor(latest.ID)
	// Only while they are still talking. Once the utterance is committed it is
	// in the log, and repeating it there would show the voice the same sentence
	// twice with no way to tell that it is one.
	if runtime.duplex.Snapshot().UserSpeaking {
		heard = strings.TrimSpace(latest.Text())
	}
	return runtime.pinboard.Lines(runtime.scheduler.NowNS()), interjecting, heard
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// gapBeforeUtterance is how long passed between the previous utterance ending
// and the one now in progress starting.
//
// It is the difference between a turn and a breath. A recogniser splits a
// sentence where the speaker pauses, so the tail of an instruction arrives
// capitalised and punctuated like a sentence of its own, and nothing in the
// text says which it is. Adjacency in time does.
func (runtime *runtime) gapBeforeUtterance(snapshot trajectory.Snapshot) string {
	gap, ok := runtime.gapBeforeUtteranceNS(snapshot)
	if !ok {
		return ""
	}
	return renderSilence(gap)
}

// gapBeforeUtteranceNS is the same measurement as a number.
func (runtime *runtime) gapBeforeUtteranceNS(snapshot trajectory.Snapshot) (uint64, bool) {
	runtime.audioMu.Lock()
	startedNS := runtime.speechStartNS
	runtime.audioMu.Unlock()
	if startedNS == 0 {
		return 0, false
	}
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation ||
			trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		if item.MonotonicNS == 0 || item.MonotonicNS >= startedNS {
			continue
		}
		return startedNS - item.MonotonicNS, true
	}
	return 0, false
}

// breathGap is how soon after one utterance another has to start to be the
// same sentence carrying on.
//
// The gate needs half a second of quiet before it closes an utterance at all,
// so a speaker who was only drawing breath is heard again almost the instant
// the previous piece is committed - measured at 115 and 210 milliseconds on
// the two recogniser splits that broke a standing policy in half. Somebody
// starting a genuinely new turn has been quiet far longer than this: the lines
// of a conversation in this suite are seconds apart, and a person who has
// finished waits for an answer.
const breathGap = time.Second

// wholeUtterance joins a piece of a sentence back onto the piece before it,
// and names the policy that was read off that earlier piece.
//
// A recogniser cuts where somebody breathes. "Tell me the moment the build
// finishes and don't say anything else" arrives as two utterances, and both
// halves read alone are wrong: the front half pins "tell me the moment the
// build", and the back half - capitalised and punctuated like a sentence of
// its own - revokes it. Read whole, the same model pins the whole instruction,
// with the right scope, five times out of five.
//
// Which pieces belong together is a fact about the clock rather than the
// words, and the runtime already measures it for the interaction model.
func (runtime *runtime) wholeUtterance(
	snapshot trajectory.Snapshot, text string,
) (whole string, stale interaction.StandingInstruction) {
	gap, ok := runtime.gapBeforeUtteranceNS(snapshot)
	if !ok || gap >= uint64(breathGap) {
		return text, interaction.StandingInstruction{}
	}
	runtime.audioMu.Lock()
	previous := runtime.previousUtterance
	// Taken rather than read: the piece before is retired once, by the first
	// reading that joins onto it. Every later reading of the same utterance
	// joins onto the same text, and by then the pin worth keeping is the one
	// the joined text produced.
	stale, runtime.previousPin = runtime.previousPin, interaction.StandingInstruction{}
	runtime.audioMu.Unlock()
	if previous == "" {
		return text, interaction.StandingInstruction{}
	}
	return strings.TrimSpace(previous) + " " + text, stale
}

// noticeStandingInPartial runs extraction before an utterance has finished.
//
// A policy set at the start of a two-minute monologue has to govern the rest of
// that monologue. Extraction ran only on committed observations, and the floor
// - correctly - holds a turn open while somebody keeps talking, so nothing
// commits and nothing is pinned: "count the animals as I mention them" was
// still sitting unextracted inside the partial when the first animal went by.
// The policy was in the text the decision could read and not in the standing
// list it acts on.
//
// It waits for a sentence to close. A fragment mid-clause is not yet a policy,
// and asking about one costs a model call to be told so.
func (runtime *runtime) noticeStandingInPartial(stable string) {
	trimmed := strings.TrimSpace(stable)
	if runtime.policies.Extraction == nil || len(trimmed) < 12 {
		return
	}
	if !strings.ContainsAny(trimmed[len(trimmed)-1:], ".!?。！？") {
		return
	}
	// Rate-limited, because this shares an endpoint with the decision that
	// runs on the audio path. Extracting at every sentence boundary of a long
	// monologue put enough load on that endpoint to slow the decisions it was
	// meant to inform, and one run in five produced no audio at all. A policy
	// pinned three seconds after it was stated is pinned in time to govern
	// what follows; one that starves the decision layer is not.
	now := runtime.scheduler.NowNS()
	runtime.audioMu.Lock()
	tooSoon := runtime.lastPartialExtractNS != 0 &&
		now-runtime.lastPartialExtractNS < uint64(partialExtractInterval)
	if !tooSoon {
		runtime.lastPartialExtractNS = now
	}
	runtime.audioMu.Unlock()
	if tooSoon {
		return
	}
	runtime.noticeStanding(trimmed)
}

// partialExtractInterval bounds how often an unfinished utterance is re-read
// for a policy.
const partialExtractInterval = 3 * time.Second

// heardSinceSpeaking is the part of an utterance the agent has not decided
// about yet.
//
// While the floor holds a turn open the heard text only grows, so a decision
// taken forty times over one monologue sees the same wall of text with a new
// clause on the end each time. What it is being asked about is the new clause.
func (runtime *runtime) heardSinceSpeaking(heard string) string {
	runtime.audioMu.Lock()
	mark := runtime.heardWhenSpoke
	runtime.audioMu.Unlock()
	// The agent has not spoken yet, or it last spoke before a different
	// utterance began. Either way none of this has been answered, so all of it
	// is new. Returning nothing here said the opposite: it collapsed "the
	// agent has never spoken" into "nothing has been said since it did", and
	// the control scenario - a finished question with nothing standing in the
	// way - went to 0/5 the moment a rule started reading the line.
	if mark == "" {
		return heard
	}
	// Compared as words, because a recogniser rewrites what it has already
	// given you: "a warm afternoon and I was walking" becomes "a warm
	// afternoon. And I was walking" and back again between revisions of the
	// same sentence. Compared as text, the mark stops being a prefix and every
	// revision reads as a whole new utterance - so the line said all of this
	// is new each time, the agent counted the same animal again, and an
	// afternoon with two animals in it was counted to sixteen.
	said, _ := spokenWords(mark)
	nowSaid, nowWords := spokenWords(heard)
	if len(said) == 0 || len(nowSaid) < len(said) {
		return heard
	}
	for index := range said {
		if said[index] != nowSaid[index] {
			return heard
		}
	}
	return strings.Join(nowWords[len(said):], " ")
}

// spokenWords splits text into the words somebody said, lowercased for
// comparison and original for reading back.
func spokenWords(text string) (compare, original []string) {
	original = strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	compare = make([]string, len(original))
	for index, word := range original {
		compare[index] = strings.ToLower(word)
	}
	return compare, original
}

// markSpoken records how much had been heard when the agent last spoke.
func (runtime *runtime) markSpoken(heard string) {
	runtime.audioMu.Lock()
	runtime.heardWhenSpoke = heard
	runtime.audioMu.Unlock()
}

// noteWithheld records a turn that ran and produced nothing anybody heard.
//
// Three things stand between a continuation and the world - it can return
// nothing, it can fail to reach a safe point, and the commitment policy can
// hold it - and all three returned silently. From outside they are
// indistinguishable from the interaction model having decided wrongly, which
// is the bug class that has cost the most here: an act chosen correctly and
// never carried out. Measured, the second animal in a counting policy is
// exactly this shape - the model chooses speak-through, the interjection runs,
// no error is reported, and nothing is said.
func (runtime *runtime) noteWithheld(
	result continuation.RunResult, request cognition.Request, why string,
) {
	recorder := runtime.policies.ShadowInteraction
	if recorder == nil {
		return
	}
	recorder(interaction.ShadowDecision{
		NowNS: runtime.scheduler.NowNS(), Act: "withheld",
		Situation: "withheld: " + strings.TrimSpace(result.AssistantText),
		Predicates: map[string]string{
			"where": "publish", "why": why,
			// What the turn was asked for, because a turn that produced
			// nothing is a question about the request and not about the
			// result. Reconstructing it afterwards from timestamps is how an
			// afternoon goes.
			"because":     request.Because,
			"heard":       request.Heard,
			"standing":    strings.Join(request.Standing, " | "),
			"over_floor":  strconv.FormatBool(request.Interjecting),
			"interrupted": strconv.FormatBool(result.Interrupted),
			"committed":   strconv.FormatBool(result.Committed),
		},
	})
}

// lastFrame is the newest picture the agent has, for a decision model that can
// look at one.
//
// Only when the deployment says its decider can see. A frame handed to a
// text-only model is bytes it will refuse or ignore, and paying to base64 a
// screenshot on the critical path of a decision taken several times a second
// is worse than not having it.
//
// It replaces the narration rather than joining it: a description of a screen
// is a cloud round trip that costs 1.45 seconds and throws away everything the
// sentence left out.
func (runtime *runtime) lastFrame(snapshot trajectory.Snapshot) []interaction.Image {
	if !runtime.config.DeciderSees || runtime.media == nil {
		return nil
	}
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation || item.Observation == nil {
			continue
		}
		if len(item.Observation.Media) == 0 {
			continue
		}
		frames := make([]interaction.Image, 0, len(item.Observation.Media))
		for _, reference := range item.Observation.Media {
			media, err := runtime.media.Resolve(reference.Handle)
			if err != nil {
				continue
			}
			frames = append(frames, interaction.Image{
				MIMEType: media.Ref.MIMEType, Bytes: media.Bytes,
			})
		}
		return frames
	}
	return nil
}

// speakerNow names whoever the runtime believes is talking.
//
// It was the constant "user", which is a false statement rather than a
// simplification: every voice that reached the microphone was reported to the
// decision layer as the person the agent is working for. Measured on two
// people discussing the milk in a room, the situation read "heard from user so
// far: did you get the milk on the way in" and the agent answered them,
// inventing having added it to a list. No model would do otherwise.
//
// The answer comes from the observation's own source, so a deployment that
// separates channels - a phone line's far end, a second microphone, a
// recogniser that reports who spoke - is described correctly without anything
// here changing. One undiarised microphone still calls everybody in the room
// the user, which is the honest reading of what it knows.
func (runtime *runtime) speakerNow(snapshot trajectory.Snapshot) string {
	// A voice that does not belong to the person this session is with is
	// somebody else in the room, whatever the channel it arrived on. One
	// microphone carries everybody, so without this the situation asserts that
	// the user said whatever was heard - and a model told the user asked about
	// the milk answers about the milk. It is a correct answer to a false
	// premise, and the premise is the part that was wrong.
	if runtime.voices.Verdict() == voices.Different {
		return "someone else in the room"
	}
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation ||
			trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		return interaction.SpeakerOf(item)
	}
	return "user"
}
