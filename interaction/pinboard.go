package interaction

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pinboard holds the interaction policies currently in force.
//
// It is a state machine rather than a list, and the difference is the whole
// point. People lift these as readily as they set them - go ahead and
// interrupt me, never mind about the kettle - and a policy nobody can turn off
// is worse than one that was never set: it fails in a direction the person who
// set it did not ask for, at a moment they cannot predict, and it looks like
// the agent being broken rather than the agent obeying.
//
// Turn-scoped policies expire on their own, which matters more than it looks.
// "Wait, I have more to say" and "don't cut me off" are the same request at two
// lifetimes, and giving the first the lifetime of the second leaves an agent
// permanently mute for a reason nobody would ever connect back to the sentence
// that caused it.
type Pinboard struct {
	mu     sync.Mutex
	pinned []StandingInstruction
}

// Pin records a policy. Re-pinning something already in force replaces it
// rather than doubling it, because someone restating a rule is not asking for
// it twice - and every entry here is prompt that every later decision pays for.
func (board *Pinboard) Pin(instruction StandingInstruction) {
	if strings.TrimSpace(instruction.Text) == "" {
		return
	}
	board.mu.Lock()
	defer board.mu.Unlock()
	for index, existing := range board.pinned {
		if strings.EqualFold(existing.Text, instruction.Text) {
			board.pinned[index] = instruction
			return
		}
	}
	board.pinned = append(board.pinned, instruction)
}

// Revoke lifts a policy. It matches loosely because the words that lift a
// policy are rarely the words that set it, and a revocation that fails to
// match leaves somebody governed by a rule they just cancelled out loud.
func (board *Pinboard) Revoke(text string) bool {
	wanted := strings.ToLower(strings.TrimSpace(text))
	if wanted == "" {
		return false
	}
	board.mu.Lock()
	defer board.mu.Unlock()
	for index, existing := range board.pinned {
		current := strings.ToLower(existing.Text)
		if current == wanted || strings.Contains(current, wanted) || strings.Contains(wanted, current) {
			board.pinned = append(board.pinned[:index], board.pinned[index+1:]...)
			return true
		}
	}
	return false
}

// EndTurn expires the policies that were only ever about the turn just ended.
func (board *Pinboard) EndTurn() {
	board.mu.Lock()
	defer board.mu.Unlock()
	kept := board.pinned[:0]
	for _, existing := range board.pinned {
		if existing.Scope != ScopeTurn {
			kept = append(kept, existing)
		}
	}
	board.pinned = kept
}

// InForce copies out what is standing.
func (board *Pinboard) InForce() []StandingInstruction {
	board.mu.Lock()
	defer board.mu.Unlock()
	return append([]StandingInstruction(nil), board.pinned...)
}

// Lines renders the board for a decision, each policy carrying its age.
func (board *Pinboard) Lines(nowNS uint64) []string {
	inForce := board.InForce()
	if len(inForce) == 0 {
		return nil
	}
	lines := make([]string, 0, len(inForce))
	for _, existing := range inForce {
		line := existing.Text
		// The delay belongs in the line. Extraction lifts "after 15s" out of
		// the text and into a number the runtime reads, which is right for the
		// runtime and leaves the model deciding whether to speak with no idea
		// there was a number at all. Measured: somebody said they would be
		// quiet and asked to be checked on after fifteen seconds, the policy
		// was read and pinned correctly as "ask whether they are still there",
		// and the agent asked at eight - which is the right act on the
		// evidence it was shown.
		if existing.After > 0 {
			line += " (after " + describeAge(existing.After) + " of quiet)"
		}
		if existing.SetNS != 0 && nowNS > existing.SetNS {
			line += " (" + describeAge(time.Duration(nowNS-existing.SetNS)) + " ago)"
		}
		lines = append(lines, line)
	}
	return lines
}

// describeAge states an age the way it would be spoken. Go's own duration
// formatting says "2m0s", which reads as a measurement rather than as how long
// ago somebody said something.
func describeAge(age time.Duration) string {
	switch {
	case age < time.Minute:
		return strconv.Itoa(int(age.Round(time.Second).Seconds())) + "s"
	case age < time.Hour:
		minutes := int(age.Minutes())
		if seconds := int(age.Seconds()) % 60; seconds >= 30 {
			minutes++
		}
		return strconv.Itoa(minutes) + "m"
	default:
		hours := int(age.Hours())
		minutes := int(age.Minutes()) % 60
		if minutes == 0 {
			return strconv.Itoa(hours) + "h"
		}
		return strconv.Itoa(hours) + "h" + strconv.Itoa(minutes) + "m"
	}
}
