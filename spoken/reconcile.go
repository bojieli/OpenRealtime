package spoken

import (
	"strings"
	"unicode"
)

// Reconcile puts a recogniser's word times onto the words the synthesiser was
// actually given.
//
// The recogniser is being used for timing, not for wording. What was said is
// already known exactly - it is the text the synthesiser was handed - and the
// recogniser's transcript of it is a worse copy: it re-spells numbers, drops
// punctuation, splits and joins words, and occasionally mishears one outright.
// Taking its words as the utterance's words would mean the agent resumed from a
// misspelling of its own sentence.
//
// So the two are aligned rather than substituted. The alignment runs over
// characters instead of words, and over a canonical form of them, because the
// disagreements that matter are exactly the ones that break a word-to-word
// correspondence: "23" against "twenty three" is one word against two, "sea
// bass" against "seabass" is two against one, and a hyphen is a word boundary
// in one and not in the other. Canonicalised into a stream of letters, all
// three become the same characters in the same order, and an ordinary sequence
// alignment lands each reference word on the audio that carried it.
//
// Reference words no recogniser word matched - a swallowed article, a word
// mangled past recognition - are interpolated between their neighbours, which
// is what the estimated layout would have done for the whole utterance anyway.
// The result is measured because its anchors came from the audio.
// spanMS is how long the whole utterance is believed to run, which while
// synthesis is still going is longer than the audio that was listened to. Words
// the recogniser never reached are laid out past that audio rather than
// squeezed inside it: audio that does not exist yet cannot have been played,
// and a layout that puts the tail of a sentence inside the first second of it
// reports the whole sentence as spoken one second in.
func Reconcile(reference string, heard []Word, spanMS uint64) Timeline {
	words := Words(reference)
	if len(words) == 0 || len(heard) == 0 {
		return Estimate(reference, spanMS)
	}
	referenceRunes, referenceOwner := canonicalStream(words)
	heardRunes, heardStart, heardEnd, heardOwner := heardStream(heard)
	if len(referenceRunes) == 0 || len(heardRunes) == 0 ||
		len(referenceRunes) > alignmentLimit || len(heardRunes) > alignmentLimit {
		return Estimate(reference, spanMS)
	}
	matched := align(referenceRunes, heardRunes)

	starts := make([]uint64, len(words))
	ends := make([]uint64, len(words))
	known := make([]bool, len(words))
	covered := make([]int, len(words))
	length := make([]int, len(words))
	referenceCanonical := make([]string, len(words))
	heardCanonical := make([]string, len(heard))
	for index, word := range words {
		referenceCanonical[index] = canonical(word)
	}
	for index, word := range heard {
		heardCanonical[index] = canonical(word.Text)
	}
	for _, owner := range referenceOwner {
		length[owner]++
	}
	exactMatches := make([]map[int]int, len(words))
	for referenceIndex, heardIndex := range matched {
		if heardIndex < 0 {
			continue
		}
		owner := referenceOwner[referenceIndex]
		start, end := heardStart[heardIndex], heardEnd[heardIndex]
		covered[owner]++
		source := heardOwner[heardIndex]
		if referenceCanonical[owner] != "" && referenceCanonical[owner] == heardCanonical[source] {
			if exactMatches[owner] == nil {
				exactMatches[owner] = make(map[int]int)
			}
			exactMatches[owner][source]++
		}
		if !known[owner] {
			starts[owner], ends[owner], known[owner] = start, end, true
			continue
		}
		if start < starts[owner] {
			starts[owner] = start
		}
		if end > ends[owner] {
			ends[owner] = end
		}
	}
	// Character alignment deliberately permits split and joined words, but its
	// equally scoring paths can give the final repeated character of an exact
	// word to a later reference word. For example, the final "o" in a heard
	// "two" may align with the "o" in a later "four". When the canonical word
	// itself is an exact match, snap a majority match back to the recogniser's
	// whole word interval. That interval is direct timing evidence; leaving the
	// truncated character slice would claim the word was heard before its last
	// phoneme was played.
	for owner, candidates := range exactMatches {
		best, count := -1, 0
		for source, candidateCount := range candidates {
			if candidateCount > count || (candidateCount == count && (best < 0 || source < best)) {
				best, count = source, candidateCount
			}
		}
		if best >= 0 && count*2 >= length[owner] && (count >= 2 || length[owner] <= 1) {
			starts[owner] = heard[best].StartMS
			ends[owner] = heard[best].EndMS
			if ends[owner] < starts[owner] {
				ends[owner] = starts[owner]
			}
		}
	}
	// A global alignment will happily match one stray letter of a word nobody
	// said to one stray letter of a word somebody did - the final "e" of
	// "three" against the final "e" of "five" - and a word anchored on a
	// single coincidence is anchored in the wrong place entirely. Measured, it
	// put the last two words of a sentence inside audio that had not been
	// synthesised, which is the one thing the layout must never say. So an
	// anchor has to be most of a word rather than a letter of one.
	for index := range known {
		if !known[index] {
			continue
		}
		if covered[index]*2 < length[index] || (covered[index] < 2 && length[index] > 1) {
			known[index] = false
		}
	}
	if !anyKnown(known) {
		return Estimate(reference, spanMS)
	}

	span := spanMS
	if last := ends[lastKnown(known)]; last > span {
		span = last
	}
	heardExtent := uint64(0)
	for index, word := range heard {
		if heardCanonical[index] == "" {
			continue
		}
		end := word.EndMS
		if end < word.StartMS {
			end = word.StartMS
		}
		if end > heardExtent {
			heardExtent = end
		}
	}
	fillUnmatched(words, starts, ends, known, span, heardExtent)
	// The layout may now run past the audio, and it should: words the
	// recogniser never reached were placed beyond it on purpose. So the order
	// pass is bounded by where the layout actually ends rather than by where
	// the audio does, or it would pull those words straight back inside it.
	limit := span
	if last := ends[len(ends)-1]; last > limit {
		limit = last
	}
	enforceOrder(starts, ends, limit)
	span = limit

	timeline := Timeline{
		Text: strings.TrimSpace(reference), AudioMS: span,
		Words: make([]Word, 0, len(words)), Measured: true,
	}
	for index, word := range words {
		timeline.Words = append(timeline.Words, Word{
			Text: word, StartMS: starts[index], EndMS: ends[index],
		})
	}
	return timeline
}

// alignmentLimit bounds the quadratic alignment. An utterance is a sentence or
// two; anything past this is not one, and paying for a matrix that large to
// place a boundary nobody will use is the wrong trade. Over the limit the
// proportional layout stands.
const alignmentLimit = 2000

func anyKnown(known []bool) bool {
	for _, value := range known {
		if value {
			return true
		}
	}
	return false
}

func lastKnown(known []bool) int {
	for index := len(known) - 1; index >= 0; index-- {
		if known[index] {
			return index
		}
	}
	return 0
}

// fillUnmatched gives a time to every reference word the alignment did not
// anchor, by spreading the gap between the anchors around it in proportion to
// how long each word takes to say.
func fillUnmatched(
	words []string, starts, ends []uint64, known []bool, span, heardExtent uint64,
) {
	index := 0
	for index < len(words) {
		if known[index] {
			index++
			continue
		}
		run := index
		for run < len(words) && !known[run] {
			run++
		}
		from := uint64(0)
		if index > 0 {
			from = ends[index-1]
		}
		to := span
		if run < len(words) {
			to = starts[run]
		}
		if run == len(words) {
			// The recogniser never reached these, which usually means the
			// audio carrying them does not exist yet. Give them the prior's
			// duration past whatever it did reach, rather than compressing
			// them into audio that has already been played.
			if heardExtent > from {
				from = heardExtent
			}
			if extended := from + PriorDuration(words[index:run]); extended > to {
				to = extended
			}
		}
		if to < from {
			to = from
		}
		filled := make([]Word, run-index)
		spread(filled, words[index:run], from, to)
		for offset, word := range filled {
			starts[index+offset], ends[index+offset] = word.StartMS, word.EndMS
		}
		index = run
	}
}

// enforceOrder makes the layout monotonic.
//
// A recogniser can report a word ending before the one before it ended, and an
// out-of-order boundary makes At produce a spoken prefix with a hole in it. The
// times are evidence about a sequence that is known to be in order, so the
// order is imposed rather than trusted.
func enforceOrder(starts, ends []uint64, span uint64) {
	previous := uint64(0)
	for index := range starts {
		if starts[index] < previous {
			starts[index] = previous
		}
		if ends[index] < starts[index] {
			ends[index] = starts[index]
		}
		if span > 0 && ends[index] > span {
			ends[index] = span
		}
		if starts[index] > ends[index] {
			starts[index] = ends[index]
		}
		previous = ends[index]
	}
}

// canonicalStream turns the reference words into the character stream that gets
// aligned, remembering which word every character came from.
func canonicalStream(words []string) ([]rune, []int) {
	runes := make([]rune, 0, len(words)*6)
	owner := make([]int, 0, len(words)*6)
	for index, word := range words {
		for _, character := range canonical(word) {
			runes = append(runes, character)
			owner = append(owner, index)
		}
	}
	return runes, owner
}

// heardStream turns the recogniser's words into the same kind of stream, with
// each character carrying the slice of its word's interval that it covers.
//
// Spreading a word's interval evenly across its characters is crude and is
// exactly as accurate as it needs to be: the interval it subdivides is one word
// long, so the worst error a subdivision can introduce is a fraction of a word,
// and the boundary being placed is a boundary between words.
func heardStream(heard []Word) ([]rune, []uint64, []uint64, []int) {
	runes := make([]rune, 0, len(heard)*6)
	starts := make([]uint64, 0, len(heard)*6)
	ends := make([]uint64, 0, len(heard)*6)
	owners := make([]int, 0, len(heard)*6)
	for owner, word := range heard {
		text := canonical(word.Text)
		count := uint64(len([]rune(text)))
		if count == 0 {
			continue
		}
		end := word.EndMS
		if end < word.StartMS {
			end = word.StartMS
		}
		width := end - word.StartMS
		for position, character := range []rune(text) {
			runes = append(runes, character)
			starts = append(starts, word.StartMS+width*uint64(position)/count)
			ends = append(ends, word.StartMS+width*uint64(position+1)/count)
			owners = append(owners, owner)
		}
	}
	return runes, starts, ends, owners
}

// canonical reduces one word to the sounds it stands for.
//
// Case, punctuation and hyphens go, because none of them is audible. Digits are
// spelled out, because a synthesiser reads "23" aloud as two words and a
// recogniser writes those two words back down: left as a digit it aligns with
// nothing, and the whole number disappears from the layout. Letters outside the
// Latin alphabet are kept as themselves - a Han character is already the unit
// both sides write - so a Mandarin utterance aligns without a script-specific
// path.
func canonical(word string) string {
	var builder strings.Builder
	digits := make([]rune, 0, 8)
	flush := func() {
		if len(digits) == 0 {
			return
		}
		builder.WriteString(spellDigits(string(digits)))
		digits = digits[:0]
	}
	for _, character := range strings.ToLower(word) {
		switch {
		case unicode.IsDigit(character) && character < 128:
			digits = append(digits, character)
		case unicode.IsLetter(character):
			flush()
			builder.WriteRune(character)
		default:
			flush()
		}
	}
	flush()
	return builder.String()
}

// spellDigits writes a run of digits the way it is likely to be read aloud.
//
// Up to three digits is a quantity and is read as one - "23" is "twenty
// three", not "two three" - which is the case this exists for, since counting
// out loud is where knowing the exact spoken boundary matters most. Longer runs
// are identifiers far more often than they are quantities, and an identifier is
// read digit by digit. Neither reading is certain, and neither has to be: the
// alignment is tolerant of a mismatched stretch and only needs enough agreement
// on either side to place the boundary.
func spellDigits(run string) string {
	if len(run) <= 3 {
		value := 0
		for _, character := range run {
			value = value*10 + int(character-'0')
		}
		if spelled := spellNumber(value); spelled != "" {
			return spelled
		}
	}
	var builder strings.Builder
	for _, character := range run {
		builder.WriteString(digitNames[character-'0'])
	}
	return builder.String()
}

var digitNames = [10]string{
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
}

var teenNames = [10]string{
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
	"sixteen", "seventeen", "eighteen", "nineteen",
}

var tensNames = [10]string{
	"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety",
}

// spellNumber writes a number under a thousand the way it is said.
func spellNumber(value int) string {
	switch {
	case value < 0 || value > 999:
		return ""
	case value < 10:
		return digitNames[value]
	case value < 20:
		return teenNames[value-10]
	case value < 100:
		return tensNames[value/10] + digitNames[value%10]
	default:
		spelled := digitNames[value/100] + "hundred"
		if value%100 != 0 {
			spelled += spellNumber(value % 100)
		}
		return spelled
	}
}

// align is a global sequence alignment over two character streams. It returns,
// for every character of the first, the index of the character of the second it
// was matched with, or -1.
//
// Global rather than local, because both streams are the same utterance: every
// reference character belongs somewhere, and a local alignment would silently
// drop the ends, which are precisely where a cut is most likely to fall.
func align(left, right []rune) []int {
	const (
		match    = 2
		mismatch = -1
		gap      = -1
	)
	width := len(right) + 1
	// Directions are stored for the whole matrix; the scores only ever need the
	// row above, which is what keeps a long utterance from holding two integer
	// matrices at once.
	directions := make([]uint8, (len(left)+1)*width)
	previous := make([]int, width)
	current := make([]int, width)
	for column := 1; column <= len(right); column++ {
		previous[column] = previous[column-1] + gap
		directions[column] = fromLeft
	}
	for row := 1; row <= len(left); row++ {
		current[0] = previous[0] + gap
		directions[row*width] = fromAbove
		for column := 1; column <= len(right); column++ {
			step := mismatch
			if left[row-1] == right[column-1] {
				step = match
			}
			best, direction := previous[column-1]+step, diagonal
			if score := previous[column] + gap; score > best {
				best, direction = score, fromAbove
			}
			if score := current[column-1] + gap; score > best {
				best, direction = score, fromLeft
			}
			current[column] = best
			directions[row*width+column] = direction
		}
		previous, current = current, previous
	}

	matched := make([]int, len(left))
	for index := range matched {
		matched[index] = -1
	}
	row, column := len(left), len(right)
	for row > 0 || column > 0 {
		switch directions[row*width+column] {
		case diagonal:
			if left[row-1] == right[column-1] {
				matched[row-1] = column - 1
			}
			row, column = row-1, column-1
		case fromAbove:
			row--
		default:
			column--
		}
	}
	return matched
}

const (
	diagonal  uint8 = 0
	fromAbove uint8 = 1
	fromLeft  uint8 = 2
)
