package interaction

import (
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Window is the rolling conversation an interaction model reads, and it rolls
// as rarely as it can get away with.
//
// The obvious implementation drops the oldest turn each time it gains one,
// which is wrong for a reason that has nothing to do with what the model sees.
// The window is a cached prefix. Changing where it starts invalidates every
// token after it, so a window that shifts on every turn re-prefills the whole
// prompt on every turn and the cache never pays for itself.
//
// So it grows, and only when it exceeds an upper bound does it truncate - in
// one step, down to a lower bound. Between truncations the prefix is stable and
// growth is pure append, which is the case a prefix cache is built for. With
// the defaults here that is one invalidation per several hundred decisions
// instead of one per decision.
//
// The same hysteresis is why the speech gate has an onset threshold separate
// from its release: a boundary that both directions share is a boundary
// everything oscillates across.
type Window struct {
	// LowerBudget is what a truncation leaves behind, in estimated tokens.
	LowerBudget int
	// UpperBudget is what triggers one.
	UpperBudget int

	// startID is the item the window currently begins at. It is an item
	// identity rather than an index because the trajectory is pruned from the
	// front, and an index would silently come to mean a different item.
	startID string
}

// DefaultWindowBudgets are the bounds a session starts with.
const (
	DefaultLowerBudget = 600
	DefaultUpperBudget = 1500
)

func (window *Window) budgets() (lower, upper int) {
	lower, upper = window.LowerBudget, window.UpperBudget
	if lower <= 0 {
		lower = DefaultLowerBudget
	}
	if upper <= lower {
		upper = lower + DefaultUpperBudget - DefaultLowerBudget
	}
	return lower, upper
}

// Lines renders the window over a trajectory, advancing its start only when
// the upper bound is passed.
func (window *Window) Lines(items []trajectory.Item) []string {
	lower, upper := window.budgets()
	speakable := make([]trajectory.Item, 0, len(items))
	for _, item := range items {
		if line := windowLine(item); line != "" {
			speakable = append(speakable, item)
		}
	}
	speakable = trajectory.WithoutSupersededPartials(speakable)
	start := 0
	if window.startID != "" {
		for index, item := range speakable {
			if item.ID == window.startID {
				start = index
				break
			}
		}
	}
	if cost(speakable[start:]) > upper {
		for start < len(speakable)-1 && cost(speakable[start:]) > lower {
			start++
		}
	}
	if start < len(speakable) {
		window.startID = speakable[start].ID
	}
	lines := make([]string, 0, len(speakable)-start)
	for _, item := range speakable[start:] {
		lines = append(lines, windowLine(item))
	}
	return lines
}

// cost estimates a stretch of conversation in tokens.
//
// Four characters to the token is crude, and deliberately so: this decides
// when to pay a cache invalidation, and being wrong by a third changes how
// often that happens rather than whether anything is correct. A real tokeniser
// here would be a dependency on the model, which is the thing this type most
// wants not to know about.
func cost(items []trajectory.Item) int {
	total := 0
	for _, item := range items {
		total += len([]rune(windowLine(item)))/4 + 2
	}
	return total
}

// windowLine renders one item as a line of conversation, or empty for items
// that are not conversation. What an interaction model needs from the past is
// who said what; reasoning, tool plumbing and assistant state are the agent
// talking to itself.
// silentAuthority marks a producer that is never heard. It is the string form
// of continuation.SpeechAuthoritySilent, spelled out here because interaction
// cannot import continuation without a cycle.
const silentAuthority = "silent"

func windowLine(item trajectory.Item) string {
	text := strings.TrimSpace(item.Content)
	if text == "" {
		return ""
	}
	switch item.Kind {
	case trajectory.KindObservation:
		if trajectory.AuthorityOf(item) == trajectory.AuthorityObserver {
			return "seen: " + text
		}
		return SpeakerOf(item) + ": " + text
	case trajectory.KindAssistant:
		// The reasoner is never heard. What it writes is background state the
		// voice reads before it speaks next, and rendering it as "agent:" tells
		// the voice it said something it never said - measured, a conversation
		// in which the agent appeared to have replied "The user wants to finish
		// a task but has not yet specified what the task is. Please ask the
		// user to provide the details", in the third person, to somebody who
		// was mid-sentence.
		//
		// Kept rather than dropped, because it is often the only record of what
		// the reasoner found, and labelled rather than kept quietly, because a
		// line that reads as speech is read as speech.
		if text == WaitToken {
			// Whoever produced it. A decision to say nothing is not a line of
			// conversation and not a piece of background state either; it is
			// the absence of both, and rendering it teaches the next turn that
			// the agent says "<wait>" out loud.
			return ""
		}
		if item.Producer.SpeechAuthority == silentAuthority {
			return "background (not said out loud): " + text
		}
		return "agent: " + text
	default:
		return ""
	}
}

// RecentLines is the tail of a conversation, for a reader that needs context
// without owning a window.
//
// It is separate from Window because Window carries hysteresis state: calling
// it from two places would move the truncation point for reasons the other
// caller knows nothing about, and the cache it exists to protect is the
// interaction model's alone.
func RecentLines(items []trajectory.Item, max int) []string {
	if max <= 0 {
		max = 6
	}
	// Canonical state retains every ASR revision, but a decision context must
	// present the current utterance once. Without this projection a partial and
	// its longer replacement look like two separate things the person said,
	// which causes counting, translation, and menu policies to fire repeatedly.
	items = trajectory.WithoutSupersededPartials(items)
	var lines []string
	for index := len(items) - 1; index >= 0 && len(lines) < max; index-- {
		if line := windowLine(items[index]); line != "" {
			lines = append([]string{line}, lines...)
		}
	}
	return lines
}

// WaitToken is how the voice says nothing. It is duplicated from cognition
// rather than imported, because interaction must not depend on the layer that
// produces the content it governs - the third authority rule in ADR-0009.
const WaitToken = "<wait>"

// SpeakerOf names whoever produced an observation.
//
// Everything that reaches a microphone used to be labelled "user", and that is
// not a simplification, it is a false statement about who said something.
// Measured: two people in a room discussing the milk were reported to the
// decision layer as the user asking the agent about milk, and it answered -
// inventing having added it to a list. Read the situation back and no model
// would do otherwise, because it was told the user asked.
//
// The label is the observation's own source, which is what the runtime
// actually knows. A deployment that separates channels - a phone line's far
// end, a second microphone, a recogniser that says who spoke - gets the truth
// here for free. One undiarised microphone still says "user" for everybody in
// the room, and that is a limit of the perception rather than a claim.
func SpeakerOf(item trajectory.Item) string {
	if item.Observation == nil {
		return "user"
	}
	switch source := strings.TrimSpace(item.Observation.Source); source {
	case "", "microphone", "voice", "text":
		return "user"
	default:
		return source
	}
}
