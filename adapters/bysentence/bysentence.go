// Package bysentence synthesises a turn one sentence at a time.
//
// A streaming synthesiser still reads the whole text before it emits anything.
// Measured against Fish Audio S2-Pro on SGLang, with stream set and PCM out,
// the wait for the first byte is a function of the text it was handed:
//
//	one word        289ms
//	a short clause  607ms
//	one sentence    911ms
//	two sentences   977ms
//
// That is not what streaming is supposed to mean, and it is not something to
// fix in the engine: whatever it is doing - planning prosody, prefilling,
// deciding a length - it is doing it over everything it was given. So give it
// less. The first sentence is all the agent needs to start talking, and the
// rest is synthesised while that sentence plays.
//
// The saving is the difference between those rows, which is three to six
// hundred milliseconds on every spoken turn in the system.
package bysentence

import (
	"context"
	"strings"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// Provider wraps a streaming speech provider.
type Provider struct {
	Inner v1.StreamingSpeechProvider
	// Minimum is how short a first piece may be. Below this the split is not
	// worth the second request: "Yes." synthesises in under three hundred
	// milliseconds whole, and cutting it in two would add a round trip to save
	// nothing.
	Minimum int
}

const defaultMinimum = 12

func (provider Provider) Descriptor() v1.Descriptor { return provider.Inner.Descriptor() }

func (provider Provider) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return provider.Inner.Synthesize(ctx, plan)
}

// Stream synthesises each sentence in turn and forwards the audio as it comes.
//
// Sequential rather than concurrent, and deliberately. The pieces have to reach
// the ear in order, so a second request running ahead would have to be buffered
// until the first finished - which saves nothing, because playback is paced in
// real time and a sentence takes longer to say than the next one takes to
// synthesise. What matters is only that the first piece is short.
func (provider Provider) Stream(
	ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error,
) error {
	pieces := Split(plan.Text, provider.minimum())
	if len(pieces) <= 1 {
		return provider.Inner.Stream(ctx, plan, consume)
	}
	for index, piece := range pieces {
		part := plan
		part.Text = piece
		// The last piece carries the plan's own final marker; the ones before
		// it must not, or everything downstream treats the turn as over after
		// the first sentence.
		if err := provider.Inner.Stream(ctx, part, func(chunk v1.SpeechChunk) error {
			if index < len(pieces)-1 {
				chunk.Final = false
			}
			return consume(chunk)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (provider Provider) minimum() int {
	if provider.Minimum > 0 {
		return provider.Minimum
	}
	return defaultMinimum
}

// Split cuts text at sentence ends, keeping each piece at least minimum runes
// long so a stray "Mr." does not produce a one-word request.
//
// Only the first cut earns anything - it is what the agent starts talking on -
// so the rest is left whole rather than chopped into as many requests as there
// are full stops, each paying its own round trip.
func Split(text string, minimum int) []string {
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) < minimum*2 {
		return []string{trimmed}
	}
	runes := []rune(trimmed)
	for index, symbol := range runes {
		if index+1 < minimum {
			continue
		}
		if !isSentenceEnd(symbol) {
			continue
		}
		// A boundary is only a boundary if what follows it is a gap. "3.5" and
		// "Mr. Smith" are not two sentences.
		if index+1 < len(runes) && !unicode.IsSpace(runes[index+1]) {
			continue
		}
		head := strings.TrimSpace(string(runes[:index+1]))
		tail := strings.TrimSpace(string(runes[index+1:]))
		if tail == "" || len([]rune(tail)) < 2 {
			return []string{trimmed}
		}
		return []string{head, tail}
	}
	return []string{trimmed}
}

func isSentenceEnd(symbol rune) bool {
	switch symbol {
	case '.', '!', '?', '。', '！', '？':
		return true
	}
	return false
}
