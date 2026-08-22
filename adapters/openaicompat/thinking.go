package openaicompat

import "strings"

// Some models write their reasoning into the content field.
//
// A Qwen-class model with thinking left on emits a <think> block inline rather
// than in reasoning_content, and an endpoint that does not implement any of the
// vendor extensions this adapter can send has no way to be asked not to. What
// arrives is assistant content that opens with the model's deliberation, and
// the fast provider's assistant content is what gets spoken - so the agent
// reads its own chain of thought out loud.
//
// The delimiter is a well-known reasoning marker in this dialect rather than a
// guess, so recognising it is the same job ReasoningDeltaField already does for
// the structured case: this is reasoning, and it belongs in the reasoning
// channel. Recognition is deliberately narrow - only a block that opens the
// content, which is where a model's deliberation appears - so a turn that
// happens to discuss the tag is untouched.
//
// It is a safety net rather than a substitute for configuring the provider. A
// model that thinks still spends the tokens and the latency on it, and can
// spend a whole short fast budget without reaching an answer; what this
// prevents is the deliberation being spoken.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// thinkingFilter splits a content stream into reasoning and assistant text.
//
// It is a state machine rather than a search because the stream is chunked at
// arbitrary boundaries: an opening tag can arrive one character at a time, and
// a filter that only recognised whole tags would pass the first half of one
// through as something to say.
type thinkingFilter struct {
	state   thinkingState
	pending string
	leaked  bool
}

type thinkingState int

const (
	// thinkingUndecided is the opening of the content, where a block may
	// begin. Text is held only while it could still turn out to be the tag.
	thinkingUndecided thinkingState = iota
	thinkingInside
	thinkingOutside
)

// push splits one delta. Anything returned as reasoning was the model
// deliberating; anything returned as assistant content is what it said.
func (filter *thinkingFilter) push(delta string) (reasoning, assistant string) {
	for delta != "" {
		switch filter.state {
		case thinkingUndecided:
			filter.pending += delta
			delta = ""
			switch {
			case strings.HasPrefix(filter.pending, thinkOpen):
				filter.leaked = true
				filter.state = thinkingInside
				delta = filter.pending[len(thinkOpen):]
				filter.pending = ""
			case strings.HasPrefix(thinkOpen, filter.pending):
				// Still a possible prefix of the tag. Hold it: emitting now
				// would put "<th" on the wire as something to say.
			default:
				filter.state = thinkingOutside
				delta = filter.pending
				filter.pending = ""
			}
		case thinkingInside:
			filter.pending += delta
			delta = ""
			if index := strings.Index(filter.pending, thinkClose); index >= 0 {
				reasoning += filter.pending[:index]
				delta = filter.pending[index+len(thinkClose):]
				filter.pending = ""
				filter.state = thinkingOutside
				continue
			}
			// Hold back only as much as could be a split closing tag; the rest
			// is reasoning that will not change its mind.
			if held := partialSuffix(filter.pending, thinkClose); held < len(filter.pending) {
				reasoning += filter.pending[:len(filter.pending)-held]
				filter.pending = filter.pending[len(filter.pending)-held:]
			}
		case thinkingOutside:
			assistant += delta
			delta = ""
		}
	}
	return reasoning, assistant
}

// flush releases whatever was being held when the stream ended.
//
// An unterminated block is the common failure rather than an odd one: a short
// output budget runs out mid-deliberation, so the closing tag never arrives.
// What was held is still reasoning, and treating it as something to say would
// speak exactly the half of the thought that fit.
func (filter *thinkingFilter) flush() (reasoning, assistant string) {
	pending := filter.pending
	filter.pending = ""
	switch filter.state {
	case thinkingInside:
		return pending, ""
	default:
		return "", pending
	}
}

// Leaked reports whether this stream carried reasoning in its content.
func (filter *thinkingFilter) Leaked() bool { return filter.leaked }

// partialSuffix is the length of the longest suffix of text that is a strict
// prefix of marker, which is exactly how much has to be held back in case the
// rest of the marker is in the next chunk.
func partialSuffix(text, marker string) int {
	limit := min(len(text), len(marker)-1)
	for length := limit; length > 0; length-- {
		if strings.HasPrefix(marker, text[len(text)-length:]) {
			return length
		}
	}
	return 0
}
