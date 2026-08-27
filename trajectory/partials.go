package trajectory

import (
	"strings"
	"unicode"
)

// WithoutSupersededPartials drops an utterance that a later one continues.
//
// A recogniser commits its stable text as it goes, so one sentence reaches the
// trajectory several times, each version a little longer than the last. That is
// what the trajectory is for: every version was really committed, and adoption
// and repair are decided against them.
//
// It is not what a conversation looks like. Rendered as messages, somebody who
// mentioned a capybara once appears to have mentioned it three times, and a
// model asked to count the animals answers three - correctly, for the
// conversation it was shown. Measured on an afternoon with two animals in it,
// the counts came back "3 4".
//
// Only consecutive items, only from the same authority, and only
// where the earlier is a word-prefix of the later, which is what "the same
// sentence, further along" means and what nothing else looks like.
func WithoutSupersededPartials(items []Item) []Item {
	kept := make([]Item, 0, len(items))
	for index, item := range items {
		if index+1 < len(items) && Continues(item, items[index+1]) {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// Continues reports that later is earlier said further.
func Continues(earlier, later Item) bool {
	if earlier.Kind != KindObservation || later.Kind != KindObservation {
		return false
	}
	if AuthorityOf(earlier) != AuthorityOf(later) {
		return false
	}
	return SaidFurther(earlier.Content, later.Content)
}

// SaidFurther reports that later is earlier carried on: the same words, and
// then more of them. It is the text-level half of Continues, separate because
// a recogniser's pieces are compared before they are ever items.
func SaidFurther(earlier, later string) bool {
	was, now := SpokenWords(earlier), SpokenWords(later)
	if len(was) == 0 || len(now) <= len(was) {
		return false
	}
	for index := range was {
		if was[index] != now[index] {
			return false
		}
	}
	return true
}

// SpokenWords lowercases and drops punctuation, because a recogniser
// re-punctuates what it has already given you between one commit and the next:
// "a warm afternoon and I was walking" becomes "a warm afternoon. And I was
// walking" and back again, and compared as text neither is a prefix of the
// other.
func SpokenWords(text string) []string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for index, word := range words {
		words[index] = strings.ToLower(word)
	}
	return words
}
