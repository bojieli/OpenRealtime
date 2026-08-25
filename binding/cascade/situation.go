package cascade

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/interaction"
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
		Speaker:       "user",
		Speaking:      decision.Duplex.UserSpeaking,
		Heard:         strings.TrimSpace(decision.Revision.Text()),
		Silence:       renderSilence(decision.Revision.SilenceNS),
		SincePrevious: runtime.gapBeforeUtterance(snapshot),
		InFlight:      workInFlight(snapshot),
		Seen:          lastSeen(snapshot),
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

// lastSeen is the newest thing an observer noticed that nobody said out loud.
func lastSeen(snapshot trajectory.Snapshot) string {
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation {
			continue
		}
		if trajectory.AuthorityOf(item) != trajectory.AuthorityObserver {
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
	runtime.wait.Add(1)
	go func() {
		defer runtime.wait.Done()
		// The conversation, not just the utterance: a recogniser splits where a
		// speaker breathes, and a fragment read alone means something else.
		recent := interaction.RecentLines(runtime.store.Snapshot().Items, 6)
		extraction, err := runtime.policies.Extraction.Extract(
			runtime.ctx, runtime.pinboard.InForce(), recent, text)
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			outcome := extraction.Kind
			if err != nil {
				outcome = "error"
			}
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Situation: "extract: " + text,
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
		case "revoke":
			runtime.pinboard.Revoke(extraction.Instruction.Text)
		}
	}()
}

// setInterjecting records whether the turn about to run was taken rather than
// offered.
func (runtime *runtime) setInterjecting(interjecting bool) {
	runtime.audioMu.Lock()
	runtime.interjecting = interjecting
	runtime.audioMu.Unlock()
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
	interjecting = runtime.interjecting
	latest := runtime.heard
	runtime.audioMu.Unlock()
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
	runtime.audioMu.Lock()
	startedNS := runtime.speechStartNS
	runtime.audioMu.Unlock()
	if startedNS == 0 {
		return ""
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
		return renderSilence(startedNS - item.MonotonicNS)
	}
	return ""
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
