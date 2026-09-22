// Package capability runs the interaction-capability study: how far an agent
// gets from a sparse, time-aligned representation of a conversation, and which
// behaviours need richer acoustic evidence or tighter language/speech coupling.
//
// The unit of evidence here is the matched pair, not the run. Two branches of
// a pair share one prefix - structurally the same slice of events, not two
// copies that happen to agree - and diverge only at an authored piece of
// feedback. A difference in what the agent says after that point is therefore
// attributable to the feedback rather than to sampling, because the negative
// control is the same branch with the feedback removed.
//
// Everything in this package is causal by construction. An event carries the
// time it was said and, separately, the time a policy is allowed to see it;
// nothing that has not yet happened can be admitted, and no scorer may consult
// a transcript the policy never received. See docs/interaction-capability-study.md
// for the design and docs/experiments/interaction-capability-v1.yaml for the
// frozen cells.
package capability

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Family is one of the eight task families the study scores separately.
// Per-family reporting is required: an average over families hides that a
// system which never intervenes scores well on half of them.
type Family string

const (
	FamilySemanticCorrection Family = "semantic-correction"
	FamilySteering           Family = "mid-explanation-steering"
	FamilyProsodic           Family = "prosodic-acknowledgement"
	FamilySilence            Family = "silence-and-hesitation"
	FamilyAddressing         Family = "addressing-and-overlap"
	FamilyProactive          Family = "proactive-semantic-action"
	FamilyConcurrent         Family = "concurrent-task"
	FamilyRevision           Family = "output-revision"
)

// Families is every family in the preregistered order used by reports.
var Families = []Family{
	FamilySemanticCorrection, FamilySteering, FamilyProsodic, FamilySilence,
	FamilyAddressing, FamilyProactive, FamilyConcurrent, FamilyRevision,
}

// Split keeps prompt design away from the material that produces the reported
// numbers. Scorers and prompts are frozen on the pilot; the held-out split is
// run once against that frozen configuration.
type Split string

const (
	SplitPilot   Split = "pilot"
	SplitHeldOut Split = "held-out"
)

// Kind is what a piece of annotated evidence is.
//
// The kinds are separate because a policy's declared input channels are
// enforced by admitting some kinds and not others. A text-only cell that could
// still see KindSound would be deciding on acoustic evidence while reporting
// that it had none.
type Kind string

const (
	// KindWord is one recognised or annotated word of speech.
	KindWord Kind = "word"
	// KindSound is acoustic activity without decoded words: evidence that
	// somebody is making sound now, which a trailing recogniser cannot give.
	KindSound Kind = "sound"
	// KindCue is an acoustic annotation - rising intonation, hesitancy,
	// emphasis. Only cells that declare acoustic evidence admit it.
	KindCue Kind = "cue"
	// KindCommit marks an earlier provisional event final without changing
	// its text, so the cost of waiting for commitment stays visible.
	KindCommit Kind = "commit"
)

// SpeakerUser is the participant the agent is talking to. Other speakers exist
// so the addressing family can put speech in the room that is not addressed to
// the agent; they reach the agent down the same microphone.
const SpeakerUser = "user"

// Event is one piece of evidence with two distinct times.
//
// SourceStart/SourceEnd are when the sound happened. AvailableAt is when a
// policy may first see it, which is strictly later: recognition trails speech,
// and an annotation of a whole utterance is not available while it is still
// being said. Keeping both is what makes a delay sweep a manipulation of one
// variable rather than a rewrite of the conversation.
type Event struct {
	ID string `json:"id"`
	// Group ties the events of one authored phrase together - its words, the
	// sound they made, and any cue annotating them. The negative control
	// removes a group, because removing a single word of "I know that part"
	// would leave a different sentence rather than no feedback.
	Group       string        `json:"group"`
	Kind        Kind          `json:"kind"`
	Speaker     string        `json:"speaker,omitempty"`
	Text        string        `json:"text,omitempty"`
	Cue         string        `json:"cue,omitempty"`
	SourceStart time.Duration `json:"source_start"`
	SourceEnd   time.Duration `json:"source_end"`
	AvailableAt time.Duration `json:"available_at"`
	// Provisional marks a hypothesis a recogniser may still revise. A cell
	// that waits for commitment pays for the wait; one that acts on
	// provisional text risks acting on words that were never said.
	Provisional bool `json:"provisional,omitempty"`
	// Replaces names the earlier event this supersedes, for a revision, or
	// the event a KindCommit finalises.
	Replaces string `json:"replaces,omitempty"`
}

func (event Event) speaker() string {
	if event.Speaker == "" {
		return SpeakerUser
	}
	return event.Speaker
}

// Variant is one branch of a matched pair: what the user does after the shared
// prefix, and what the agent must then say.
type Variant struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Feedback names the event group carrying the authored difference.
	// Removing it is the negative control, so the difference has to be one
	// identifiable phrase rather than a property of the whole branch.
	Feedback string      `json:"feedback"`
	Events   []Event     `json:"events"`
	Expect   Expectation `json:"expect"`
}

// Expectation is the preregistered scoring policy for one branch.
//
// It is stated in terms of an event and a bound rather than a wall-clock
// instant because how long the shared prefix takes to say is decided by a
// synthesiser, and a window pinned to a millisecond would pass or fail on the
// length of a vowel.
type Expectation struct {
	// After names the event whose availability opens the window. Content the
	// agent had already committed to before that moment cannot be credited.
	After string `json:"after"`
	// Within bounds how long the adapted content may take to become audible.
	Within time.Duration `json:"within"`
	// RequireAnyOf is a conjunction of disjunctions: every group must be
	// satisfied by at least one of its terms. Groups carry synonyms so the
	// scorer does not reward one particular wording.
	RequireAnyOf [][]string `json:"require_any_of"`
	// Forbid is content that shows the agent did not adapt: continuing with
	// the superseded topic, or answering the question it was asked to skip.
	Forbid []string `json:"forbid"`
	// MinWords is how much the agent must actually say in the window. It
	// exists because stopping the old answer and acknowledging the user is
	// not fulfilling the changed request: "Kyoto, got it." contains every
	// required term and completes nothing.
	MinWords int `json:"min_words"`
	// Silent instead requires that nothing be audible in the window - the
	// branch where the right move is to keep waiting. It is only ever half
	// of a pair: the other branch requires speech, so a system that never
	// speaks fails the pair rather than scoring half of it.
	Silent bool `json:"silent,omitempty"`
}

// Pair is one matched counterfactual.
//
// Prefix is one slice, shared by both branches rather than duplicated, so
// "the branches share an identical prefix" is a property of the type instead
// of a check that could pass while the two drifted apart.
type Pair struct {
	ID     string `json:"id"`
	Family Family `json:"family"`
	Split  Split  `json:"split"`
	// Instructions is the session prompt, identical across branches and
	// cells: a treatment must not smuggle in a different task.
	Instructions string    `json:"instructions"`
	Prefix       []Event   `json:"prefix"`
	Variants     []Variant `json:"variants"`
}

// PrefixEnd is when the shared audio stops. A branch may not place speech
// before it.
//
// It is measured in source time, not availability time: the prefix's last word
// may still be arriving from a recogniser while a branch's first word is being
// said, and that is ordinary. What matters is that both branches receive the
// identical prefix admissions, which they do because the prefix is one slice
// rather than two copies.
func (pair Pair) PrefixEnd() time.Duration {
	var end time.Duration
	for _, event := range pair.Prefix {
		end = max(end, event.SourceEnd)
	}
	return end
}

// Branch is the full event stream of one variant: the shared prefix followed
// by that branch's own events.
func (pair Pair) Branch(variant Variant) []Event {
	branch := make([]Event, 0, len(pair.Prefix)+len(variant.Events))
	branch = append(branch, pair.Prefix...)
	branch = append(branch, variant.Events...)
	slices.SortStableFunc(branch, func(left, right Event) int {
		return int(left.AvailableAt - right.AvailableAt)
	})
	return branch
}

// WithoutFeedback is the negative control: the same branch with the authored
// feedback removed and nothing else changed. A run of it must not satisfy the
// branch's expectation - if it does, the expectation is measuring something
// other than the feedback.
func (pair Pair) WithoutFeedback(variant Variant) (Variant, error) {
	kept := make([]Event, 0, len(variant.Events))
	for _, event := range variant.Events {
		if event.Group != variant.Feedback {
			kept = append(kept, event)
		}
	}
	if len(kept) == len(variant.Events) {
		return Variant{}, fmt.Errorf("variant %q: feedback group %q is not in the branch", variant.ID, variant.Feedback)
	}
	control := variant
	control.ID = variant.ID + "-nofeedback"
	control.Events = kept
	return control, nil
}

// Validate reports every way a pair violates the study's causal contract. It
// returns all of them at once: a fixture with four mistakes should be fixed
// once, not four times.
func (pair Pair) Validate() error {
	var problems []error
	fail := func(format string, arguments ...any) {
		problems = append(problems, fmt.Errorf(format, arguments...))
	}
	if pair.ID == "" {
		fail("pair has no ID")
	}
	if !slices.Contains(Families, pair.Family) {
		fail("pair %q: unknown family %q", pair.ID, pair.Family)
	}
	if pair.Split != SplitPilot && pair.Split != SplitHeldOut {
		fail("pair %q: unknown split %q", pair.ID, pair.Split)
	}
	if strings.TrimSpace(pair.Instructions) == "" {
		fail("pair %q: no session instructions", pair.ID)
	}
	if len(pair.Variants) != 2 {
		fail("pair %q: a matched pair has two branches, got %d", pair.ID, len(pair.Variants))
	}

	seen := map[string]Event{}
	checkEvents := func(where string, events []Event, prefixEnd time.Duration) {
		previous := time.Duration(-1)
		for position, event := range events {
			at := fmt.Sprintf("%s: event %d (%q)", where, position, event.ID)
			if event.ID == "" {
				fail("%s: has no ID", at)
			} else if _, duplicate := seen[event.ID]; duplicate {
				fail("%s: duplicate event ID", at)
			}
			switch event.Kind {
			case KindWord, KindSound, KindCue, KindCommit:
			default:
				fail("%s: unknown kind %q", at, event.Kind)
			}
			if event.Kind == KindWord && strings.TrimSpace(event.Text) == "" {
				fail("%s: a word with no text", at)
			}
			if event.Kind == KindCue && strings.TrimSpace(event.Cue) == "" {
				fail("%s: a cue with no annotation", at)
			}
			if event.SourceEnd < event.SourceStart {
				fail("%s: ends (%s) before it starts (%s)", at, event.SourceEnd, event.SourceStart)
			}
			// The causal contract: nothing may be seen before it happened.
			// Sound is the one kind observable while it is still going on -
			// that a voice has started is exactly the evidence a trailing
			// recogniser cannot give - so its onset, not its end, bounds it.
			switch event.Kind {
			case KindSound:
				if event.AvailableAt < event.SourceStart {
					fail("%s: available at %s, before it began at %s", at, event.AvailableAt, event.SourceStart)
				}
			case KindWord, KindCue:
				if event.AvailableAt < event.SourceEnd {
					fail("%s: available at %s, before it finished at %s", at, event.AvailableAt, event.SourceEnd)
				}
			}
			if event.Group == "" {
				fail("%s: belongs to no phrase group", at)
			}
			// Words are authored in the order they were said; availability
			// may overlap, because two people can talk at once and a
			// recogniser trails both, and the release schedule sorts by
			// availability. Sounds and cues span whole phrases, so they
			// legitimately begin before the last word they cover.
			if event.Kind == KindWord {
				if event.SourceStart < previous {
					fail("%s: starts at %s, before the previous word at %s", at, event.SourceStart, previous)
				}
				previous = event.SourceStart
			}
			if prefixEnd > 0 && event.SourceStart < prefixEnd {
				fail("%s: starts at %s, inside the shared prefix which ends at %s",
					at, event.SourceStart, prefixEnd)
			}
			if event.Replaces != "" {
				replaced, known := seen[event.Replaces]
				switch {
				case !known:
					fail("%s: replaces unknown event %q", at, event.Replaces)
				case !replaced.Provisional:
					fail("%s: replaces %q, which was committed", at, event.Replaces)
				case event.AvailableAt <= replaced.AvailableAt:
					fail("%s: replaces %q but is available no later than it", at, event.Replaces)
				}
			} else if event.Kind == KindCommit {
				fail("%s: a commit names no event", at)
			}
			if event.ID != "" {
				seen[event.ID] = event
			}
		}
	}
	checkEvents(pair.ID+" prefix", pair.Prefix, 0)

	prefixEnd := pair.PrefixEnd()
	prefixOnly := map[string]Event{}
	for id, event := range seen {
		prefixOnly[id] = event
	}
	for _, variant := range pair.Variants {
		where := pair.ID + " variant " + variant.ID
		if variant.ID == "" {
			fail("%s: a branch has no ID", pair.ID)
		}
		// Each branch is validated against the prefix alone: an event in one
		// branch must not be able to reference an event in the other.
		seen = map[string]Event{}
		for id, event := range prefixOnly {
			seen[id] = event
		}
		checkEvents(where, variant.Events, prefixEnd)
		if variant.Feedback == "" {
			fail("%s: names no feedback event", where)
		} else if !slices.ContainsFunc(variant.Events, func(event Event) bool { return event.Group == variant.Feedback }) {
			fail("%s: feedback group %q is not in its own events", where, variant.Feedback)
		}
		problems = append(problems, variant.Expect.validate(where, seen)...)
	}
	return errors.Join(problems...)
}

func (expect Expectation) validate(where string, seen map[string]Event) []error {
	var problems []error
	fail := func(format string, arguments ...any) {
		problems = append(problems, fmt.Errorf(format, arguments...))
	}
	if expect.After == "" {
		fail("%s: expectation opens no window", where)
	} else if _, known := seen[expect.After]; !known {
		fail("%s: expectation opens after unknown event %q", where, expect.After)
	}
	if expect.Within <= 0 {
		fail("%s: expectation has no time bound", where)
	}
	switch {
	case expect.Silent && len(expect.RequireAnyOf) > 0:
		fail("%s: expectation requires both silence and spoken content", where)
	case expect.Silent && expect.MinWords > 0:
		fail("%s: a silent expectation cannot also require words", where)
	case !expect.Silent && len(expect.RequireAnyOf) == 0:
		fail("%s: expectation requires nothing, so anything passes", where)
	case !expect.Silent && expect.MinWords <= 0:
		fail("%s: expectation sets no floor on how much is said, so an acknowledgement passes", where)
	}
	for group, terms := range expect.RequireAnyOf {
		if len(terms) == 0 {
			fail("%s: requirement group %d is empty", where, group)
		}
		for _, term := range terms {
			if strings.TrimSpace(term) == "" {
				fail("%s: requirement group %d has a blank term", where, group)
			}
		}
	}
	for _, term := range expect.Forbid {
		if strings.TrimSpace(term) == "" {
			fail("%s: a forbidden term is blank", where)
		}
	}
	return problems
}

// ValidatePairs checks a whole suite, including that pair IDs are unique.
func ValidatePairs(pairs []Pair) error {
	var problems []error
	seen := map[string]bool{}
	for _, pair := range pairs {
		if seen[pair.ID] {
			problems = append(problems, fmt.Errorf("duplicate pair ID %q", pair.ID))
		}
		seen[pair.ID] = true
		problems = append(problems, pair.Validate())
	}
	return errors.Join(problems...)
}
