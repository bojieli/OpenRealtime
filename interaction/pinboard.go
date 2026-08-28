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

// SetForTurn makes the policies from one stretch of speech exactly these.
//
// A turn is read many times as the words arrive, and every reading answers for
// the whole of it, so the answer replaces what that turn said before rather
// than adding to it. Policies from earlier turns are untouched: they were
// separate requests and nobody has lifted them.
//
// This is what Supersede tried to be and could not, given one policy per
// reading. Watching a sequence of single answers, the runtime had to guess
// whether each replaced the last or joined it, and the guess was wrong in both
// directions - measured, replacing destroyed a live counting policy the moment
// the same turn also asked not to be interrupted, leaving nothing in force at
// all, and joining turned one request into three policies that each fired.
//
// The age of a policy that is still here carries over. Re-reading somebody's
// sentence as more of it arrives is not them asking again.
// It reports how many of them the board did not already have, which is what
// tells the runtime that this stretch of speech is the one that set something.
func (board *Pinboard) SetForTurn(turn uint64, instructions []StandingInstruction) (added int) {
	board.mu.Lock()
	defer board.mu.Unlock()
	was := make(map[string]StandingInstruction, len(board.pinned))
	var earlier []StandingInstruction
	kept := board.pinned[:0]
	for _, existing := range board.pinned {
		if existing.Turn == turn {
			was[strings.ToLower(existing.Text)] = existing
			earlier = append(earlier, existing)
			continue
		}
		kept = append(kept, existing)
	}
	board.pinned = kept
	for _, instruction := range instructions {
		if strings.TrimSpace(instruction.Text) == "" {
			continue
		}
		// A policy set earlier, listed again by a later turn, stays where it
		// was set. Where a policy came from is a fact about the moment
		// somebody asked for it, and re-reading it somewhere else does not
		// move it - the runtime declines to act on the utterance that set a
		// policy, correctly, so letting the origin drift onto whatever the
		// speaker happened to be saying makes that refusal land on an
		// occurrence instead: measured, the second animal in a story went
		// uncounted because the policy had been re-listed from the sentence
		// that mentioned it.
		if standing, ok := board.find(instruction.Text); ok && standing != turn {
			continue
		}
		instruction.Turn = turn
		if existing, ok := was[strings.ToLower(instruction.Text)]; ok {
			instruction.SetNS = existing.SetNS
			instruction.Counting = instruction.Counting || existing.Counting
		} else if existing, ok := expandedPolicy(earlier, instruction.Text); ok {
			// The pass can phrase the same policy more fully as an utterance
			// grows. Its auxiliary classifications are deliberately cheap and
			// occasionally unstable: measured, adding "and say nothing else"
			// to "count the animals ..." changed Counting from true to false.
			// A longer reading has not made the already-established operation
			// stop being a count. Carry that positive fact, and the policy's
			// original age, across a strict textual expansion.
			instruction.SetNS = existing.SetNS
			instruction.Counting = instruction.Counting || existing.Counting
		} else {
			added++
		}
		board.pinned = append(board.pinned, instruction)
	}
	return added
}

// expandedPolicy finds a policy from the previous reading that the new text
// only extends. Restrict this to a word-boundary prefix: two policies from one
// turn may be entirely different, and sharing a few words is not enough to
// merge their metadata.
func expandedPolicy(earlier []StandingInstruction, text string) (StandingInstruction, bool) {
	wanted := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	for _, existing := range earlier {
		prefix := strings.Join(strings.Fields(strings.ToLower(existing.Text)), " ")
		if prefix != "" && strings.HasPrefix(wanted, prefix+" ") {
			return existing, true
		}
	}
	return StandingInstruction{}, false
}

// find reports which turn set a policy already in force.
func (board *Pinboard) find(text string) (uint64, bool) {
	for _, existing := range board.pinned {
		if strings.EqualFold(existing.Text, text) {
			return existing.Turn, true
		}
	}
	return 0, false
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

// InForceExcept copies out what is standing apart from one turn's own
// policies.
//
// It is what the extraction pass is shown. That pass is asked what a stretch of
// speech establishes, and it is asked again every time the speaker adds to it,
// so showing it what it answered last time invites it to defer: told both to
// list everything this stretch sets and not to repeat what is already in
// force, it sees its own answer on the list and declines to repeat it - and
// since the answer replaces that turn's policies, declining deletes them.
// Measured, "count the animals out loud as I mention them and say nothing
// else" was pinned correctly at eleven seconds and gone by twenty-one.
//
// Policies from other turns stay visible, because a revocation is invisible
// without them.
func (board *Pinboard) InForceExcept(turn uint64) []StandingInstruction {
	board.mu.Lock()
	defer board.mu.Unlock()
	kept := make([]StandingInstruction, 0, len(board.pinned))
	for _, existing := range board.pinned {
		if existing.Turn == turn {
			continue
		}
		kept = append(kept, existing)
	}
	return kept
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
