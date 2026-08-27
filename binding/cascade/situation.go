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
		Restricted:    runtime.restrictionIsInForce(),
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
	whole, turn := runtime.wholeUtterance(snapshot, text)
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		// The conversation, not just the utterance: a recogniser splits where a
		// speaker breathes, and a fragment read alone means something else.
		//
		// Minus the pieces the utterance is made of. The utterance is the whole
		// turn joined, so the lines that make it up are already in it, and
		// showing both puts the same words in front of the pass twice - which
		// reads as emphasis. Measured on "Right? So let me plan this out",
		// with "user: Right?" also in the recent lines, an announcement was
		// pinned as a rule of silence one reading in six; without the
		// duplicate, none in six.
		recent := withoutEchoesOf(interaction.RecentLines(snapshot.Items, 6), whole)
		extraction, err := runtime.policies.Extraction.Extract(
			runtime.ctx, runtime.pinboard.InForceExcept(turn), recent, whole)
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := "none"
			if len(extraction.Pins) > 0 {
				outcome = "pin"
			}
			if len(extraction.Revokes) > 0 {
				outcome = "revoke"
			}
			if err != nil {
				outcome = "error"
			}
			var texts, scopes []string
			for _, instruction := range extraction.Pins {
				texts = append(texts, instruction.Text)
				scopes = append(scopes, string(instruction.Scope))
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "extract: " + whole,
				Act: outcome,
				Predicates: map[string]string{
					"where": "extract", "scope": strings.Join(scopes, " | "),
					"text": strings.Join(texts, " | "),
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
		runtime.audioMu.Lock()
		if turn == runtime.extractTurn && len(whole) < len(runtime.extractTurnSource) {
			// A shorter reading of this same turn, finishing late. What it has
			// to say about the request is strictly less.
			runtime.audioMu.Unlock()
			return
		}
		runtime.extractTurn, runtime.extractTurnSource = turn, whole
		runtime.audioMu.Unlock()
		for _, lifted := range extraction.Revokes {
			runtime.pinboard.Revoke(lifted)
		}
		now := runtime.scheduler.NowNS()
		pins := make([]interaction.StandingInstruction, 0, len(extraction.Pins))
		for _, instruction := range extraction.Pins {
			instruction.SetNS = now
			pins = append(pins, instruction)
		}
		// The whole of what this turn asked for, replacing whatever an earlier
		// reading of the same turn made of it.
		if runtime.pinboard.SetForTurn(turn, pins) > 0 {
			// And this is the speech that set it, which is the one thing the
			// agent must not act on: "count the animals as I mention them"
			// mentions no animal.
			runtime.audioMu.Lock()
			runtime.pinnedFromText = whole
			runtime.audioMu.Unlock()
		}
	}()
}

// setInterjecting records that the endpoint about to be committed was taken
// rather than offered. The canonical source revision does not exist until the
// final observation is committed, so this first marks that hand-off as pending.
func (runtime *runtime) setInterjecting(_ uint64, interjecting bool) {
	if !interjecting {
		return
	}
	runtime.audioMu.Lock()
	runtime.interjectingPending = true
	runtime.audioMu.Unlock()
}

// bindInterjectingRevision attaches a pending floor act to the canonical
// revision the event loop will later process. Acoustic onset may already have
// cleared runtime.heard by then; the act must survive independently of that
// newer utterance.
func (runtime *runtime) bindInterjectingRevision(revision uint64) {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	if runtime.interjectingPending {
		if runtime.interjectingRevs == nil {
			runtime.interjectingRevs = make(map[uint64]struct{})
		}
		runtime.interjectingRevs[revision] = struct{}{}
	}
}

func (runtime *runtime) finishInterjectingEndpoint() {
	runtime.audioMu.Lock()
	runtime.interjectingPending = false
	runtime.audioMu.Unlock()
}

// tookTheFloor reports whether the turn for this revision was taken from
// somebody mid-sentence.
func (runtime *runtime) tookTheFloor(revision uint64) bool {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	_, interjecting := runtime.interjectingRevs[revision]
	return revision != 0 && interjecting
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
func (runtime *runtime) cognitionExtras(sourceRevision uint64) (standing []string, interjecting bool, heard string) {
	runtime.audioMu.Lock()
	latest := runtime.heard
	runtime.audioMu.Unlock()
	interjecting = runtime.tookTheFloor(sourceRevision)
	// Only while they are still talking. Once the utterance is committed it is
	// in the log, and repeating it there would show the voice the same sentence
	// twice with no way to tell that it is one.
	//
	// Whole, not trimmed against what has already been committed. Trimming it
	// there left the voice holding "a capybara" with nothing around it, because
	// the log it was trimmed against is itself collapsed away before the
	// provider sees it. The sentence in progress is the fullest version of what
	// they are saying, so it is the copy that survives, and the partials it
	// continues are dropped where the prefix is assembled.
	if runtime.duplex.Snapshot().UserSpeaking {
		heard = strings.TrimSpace(latest.Text())
	}
	return runtime.pinboard.Lines(runtime.scheduler.NowNS()), interjecting, heard
}

// countingIsInForce reports that one of the pinned policies asks for a running
// count, which was read off the policy by the pass that pinned it.
//
// It replaces a list of words - count, tally, running total - that I chose
// after watching one benchmark. The words people use for this are not
// enumerable, and a system that only recognises the ones I happened to see is
// a system tuned to what I happened to see.
// restrictionIsInForce says whether any policy standing right now closes off
// everything it did not ask for.
func (runtime *runtime) restrictionIsInForce() bool {
	for _, standing := range runtime.pinboard.InForce() {
		if standing.Restricting {
			return true
		}
	}
	return false
}

func (runtime *runtime) countingIsInForce() bool {
	for _, standing := range runtime.pinboard.InForce() {
		if standing.Counting {
			return true
		}
	}
	return false
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

// wholeUtterance joins a piece of a sentence back onto the rest of what the
// person is saying, and names the policy that was read off an earlier piece.
//
// A recogniser cuts where somebody breathes, and every piece read alone means
// something other than what they said. "Count the animals out loud as I
// mention them, and say nothing else" arrives in pieces, and the tail - "and
// say nothing else." - capitalised and punctuated like a sentence of its own,
// was read as a revocation and deleted the policy the same sentence had just
// set. The agent then stood with no policy at all through the story, fell back
// on an ordinary reply at the first pause, and said "one" at a sentence with
// no animal in it.
//
// This used to join on the clock: pieces less than a second apart were one
// sentence carrying on. That is the right observation about recognisers and
// the wrong unit. People pause where they qualify - measured, the breath
// before "and say nothing else" ran past a second and the join was lost - and
// no threshold separates a breath from a turn, because the same silence is
// both depending on whether the speaker was finished.
//
// The unit that does hold is the turn. Somebody sets a policy inside a stretch
// of their own speech, and it stays one request until the agent answers it. So
// the pieces to read together are everything they have said since the agent
// last said anything out loud, however they were cut up and however long the
// speaker paused inside them.
func (runtime *runtime) wholeUtterance(
	snapshot trajectory.Snapshot, text string,
) (whole string, turn uint64) {
	pieces, began := speechSoFar(snapshot)
	// The live piece is that same last one further along when it carries it on,
	// and a piece of its own when it does not.
	if trimmed := strings.TrimSpace(text); trimmed != "" {
		switch {
		case len(pieces) == 0:
			pieces = []string{trimmed}
		case trajectory.SaidFurther(pieces[len(pieces)-1], trimmed):
			pieces[len(pieces)-1] = trimmed
		case pieces[len(pieces)-1] != trimmed:
			pieces = append(pieces, trimmed)
		}
	}
	if len(pieces) == 0 {
		return text, began
	}
	return strings.Join(pieces, " "), began
}

// speechSoFar is everything this person has said since the agent last spoke
// out loud, oldest first.
//
// It stops at the agent's own speech rather than at a length, because that is
// what ends a request: once the agent has answered, what the person says next
// is a new thing they are asking for. Background the reasoner wrote is not
// speech and does not end anything - nobody heard it.
//
// Except when they were cut off. An agent that speaks while somebody is still
// mid-sentence has not been answered and has not ended their turn, and the
// rest of the sentence arriving afterwards is still the same request: "tell me
// the moment the build" / "finishes and don't say anything else", with the
// agent's "will do" landing between the two halves. Whether they had finished
// is not a guess - a recogniser punctuates what it commits, and a piece that
// stops without reaching the end of a sentence stopped because the speaker had
// not.
func speechSoFar(snapshot trajectory.Snapshot) ([]string, uint64) {
	items := trajectory.WithoutSupersededPartials(snapshot.Items)
	var pieces []string
	var began uint64
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		text := strings.TrimSpace(item.Content)
		if item.Kind == trajectory.KindAssistant && text != "" &&
			text != interaction.WaitToken &&
			item.Producer.SpeechAuthority != string(continuation.SpeechAuthoritySilent) {
			// The agent spoke here. That ends the person's turn only if they
			// had finished the sentence it landed in - looking back at what
			// they had said by then, not forward at what they say next, which
			// on the reading that matters has not been committed yet.
			if said, ok := lastSpokenBefore(items[:index]); !ok || endsASentence(said) {
				break
			}
			continue
		}
		if item.Kind != trajectory.KindObservation || text == "" {
			continue
		}
		if trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		pieces = append([]string{text}, pieces...)
		began = item.MonotonicNS
	}
	return pieces, began
}

// settingAPolicy reports that what this person is saying now is the speech a
// policy was read out of.
//
// A policy is not in force for the sentence that set it. "Count the animals
// out loud as I mention them" mentions no animal, and an agent that carries it
// out there is wrong by one for the rest of the conversation - measured, the
// voice said "one" four times during the instruction itself, having found the
// word "animals" in it.
//
// The interjection path has refused this from the start. The ordinary turn had
// no equivalent, and it is the path that answers while somebody is setting a
// policy, because setting one is a thing people do in whole sentences.
func (runtime *runtime) settingAPolicy(snapshot trajectory.Snapshot) bool {
	runtime.audioMu.Lock()
	pinnedFrom := strings.TrimSpace(runtime.pinnedFromText)
	runtime.audioMu.Unlock()
	if pinnedFrom == "" {
		return false
	}
	// Against everything they have said, for the reason coverage is: the agent
	// answers while somebody is setting a policy - briefly agreeing is what it
	// should do there - and its own speech would otherwise end the turn this
	// compares against, so the sentence that set the policy stops being the
	// sentence in front of it. Measured, the guard went silent and the voice
	// counted through the instruction itself: "One." after "as I mentioned
	// them", then "Two." four times after "and say nothing else".
	said := everythingSaid(snapshot)
	return said != "" && beganWith(pinnedFrom, said)
}

// withoutEchoesOf drops conversation lines whose words are already inside the
// utterance being read.
func withoutEchoesOf(lines []string, utterance string) []string {
	spoken := strings.ToLower(strings.Join(trajectory.SpokenWords(utterance), " "))
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		said := line
		if index := strings.Index(line, ": "); index >= 0 {
			said = line[index+2:]
		}
		words := strings.Join(trajectory.SpokenWords(said), " ")
		if words != "" && strings.Contains(spoken, strings.ToLower(words)) {
			continue
		}
		kept = append(kept, line)
	}
	return kept
}

// lastSpokenBefore is the most recent thing this person had said by some point
// in the log.
func lastSpokenBefore(items []trajectory.Item) (string, bool) {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Kind != trajectory.KindObservation {
			continue
		}
		if trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		if text := strings.TrimSpace(item.Content); text != "" {
			return text, true
		}
	}
	return "", false
}

// endsASentence reports that a committed piece of speech reached the end of
// one. The recogniser punctuates as it commits, so this is reading what it
// decided rather than guessing where a sentence stops.
func endsASentence(text string) bool {
	trimmed := strings.TrimRightFunc(strings.TrimSpace(text), func(r rune) bool {
		return r == '"' || r == '\'' || r == ')' || r == ']' || r == '”' || r == '’'
	})
	if trimmed == "" {
		return false
	}
	last := []rune(trimmed)[len([]rune(trimmed))-1]
	return strings.ContainsRune(".!?。！？…", last)
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
// alreadyAnsweredFor is the part of what this person is saying that the agent
// has already spoken once for, or nothing if it last spoke into some earlier
// utterance.
//
// The prefix test is what ties the mark to the sentence in front of it. A mark
// left over from a previous utterance says nothing about this one, and handing
// it over would tell the voice it had already answered something it has not.
// Compared as words, because a recogniser re-punctuates what it has already
// given you between one commit and the next.
func (runtime *runtime) alreadyAnsweredFor(snapshot trajectory.Snapshot) string {
	runtime.audioMu.Lock()
	mark := strings.TrimSpace(runtime.heardWhenSpoke)
	runtime.audioMu.Unlock()
	if mark == "" {
		return ""
	}
	// Everything they have said, not the turn they are in. These are two
	// different questions and they were sharing an answer.
	//
	// A request is read in the turn it was made in, and a turn ends when the
	// agent speaks. How much the agent has covered cannot use that unit: the
	// mark is taken at the moment the agent speaks, so the turn it would be
	// compared against starts immediately after it, and the mark is never a
	// prefix of it. Measured, the fact went missing from the voice entirely on
	// every reading after the first interjection, and one animal collected
	// "one two three four".
	//
	// Speaking does not unsay what somebody said. The person's own speech runs
	// on through the agent's interjections, so that is what this measures
	// against, and the mark is always a prefix of it.
	said := everythingSaid(snapshot)
	if said == "" || !beganWith(mark, said) {
		return ""
	}
	return mark
}

// everythingSaid is all of this person's speech in the conversation, joined,
// oldest first. It is not bounded by the agent's own turns: an agent speaking
// does not unsay what somebody said.
func everythingSaid(snapshot trajectory.Snapshot) string {
	items := trajectory.WithoutSupersededPartials(snapshot.Items)
	var pieces []string
	for _, item := range items {
		if item.Kind != trajectory.KindObservation {
			continue
		}
		if trajectory.AuthorityOf(item) != trajectory.AuthorityUser {
			continue
		}
		if text := strings.TrimSpace(item.Content); text != "" {
			pieces = append(pieces, text)
		}
	}
	return strings.Join(pieces, " ")
}

// beganWith reports that a committed utterance starts with what the agent
// spoke into.
//
// The last word is allowed to be a prefix of its counterpart, because a
// recogniser's partial cuts wherever the audio has got to and that is usually
// mid-word: the agent speaks into "A cap", the sentence commits as "A capybara
// wandered over and sat down next to me", and compared strictly the mark is
// not a prefix of it at all. Which made this return nothing in precisely the
// case it exists for - the agent had spoken into that sentence, and the turn
// that ran when it committed was told it had not.
func beganWith(mark, said string) bool {
	spoken, whole := trajectory.SpokenWords(mark), trajectory.SpokenWords(said)
	if len(spoken) == 0 || len(spoken) > len(whole) {
		return false
	}
	for index := range spoken[:len(spoken)-1] {
		if spoken[index] != whole[index] {
			return false
		}
	}
	return strings.HasPrefix(whole[len(spoken)-1], spoken[len(spoken)-1])
}

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
// otherVoiceSource names a voice that is not the one the session is with.
//
// One string, used both as the observation's source and as the live speaker,
// so the conversation and the instant call the same person the same thing.
const otherVoiceSource = "someone else in the room"

func (runtime *runtime) speakerNow(snapshot trajectory.Snapshot) string {
	// A voice that does not belong to the person this session is with is
	// somebody else in the room, whatever the channel it arrived on. One
	// microphone carries everybody, so without this the situation asserts that
	// the user said whatever was heard - and a model told the user asked about
	// the milk answers about the milk. It is a correct answer to a false
	// premise, and the premise is the part that was wrong.
	if runtime.voices.Verdict() == voices.Different {
		return otherVoiceSource
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
