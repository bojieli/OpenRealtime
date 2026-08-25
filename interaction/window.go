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
		return "user: " + text
	case trajectory.KindAssistant:
		return "agent: " + text
	default:
		return ""
	}
}
