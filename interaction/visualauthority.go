package interaction

import (
	"strings"
	"unicode"
)

// ExplicitVisualAuthority recognizes only high-precision, imperative screen
// commands. It is a compiler-like fast path in front of the learned
// interaction policy: obvious authority should not become stochastic merely
// because the ASR tail is provisional, while ambiguous, conditional, and
// semantic language still belongs to the model.
//
// This function never chooses a target or coordinate. The direct-pixel actor
// must still ground a visible label, and the runtime must still validate that
// label against the user's words before executing an effect.
func ExplicitVisualAuthority(task string) (VisualIntent, bool) {
	words := visualAuthorityWords(task)
	if len(words) == 0 || hasFutureVisualCondition(words) {
		return VisualIntentNone, false
	}
	for _, clause := range visualAuthorityClauses(strings.ToLower(strings.TrimSpace(task))) {
		for _, segment := range splitVisualAuthoritySegments(visualAuthorityWords(clause)) {
			segment = trimVisualAuthorityLeadIn(segment)
			if explicitDirectVisualSegment(segment) {
				return VisualIntentDirect, true
			}
		}
	}
	return VisualIntentNone, false
}

// ExplicitVisualMonitor recognizes narrow, user-authored future screen
// conditions whose fulfillment includes a visible control action. It is the
// monitor counterpart to ExplicitVisualAuthority: the function grants no
// coordinate and does not claim that the condition is present. It only lets a
// deployment without a learned interaction classifier arm the direct-pixel
// actor for later frames instead of asking that actor to infer controller
// lifetime through a generated continuation bit.
//
// A named visual condition and an explicit operation are both required. Thus
// "call me if the build finishes" and "explain how to acknowledge an alert"
// remain semantic work, while both "acknowledge an alert if it appears" and
// "if an alert appears, acknowledge it" establish monitor authority.
func ExplicitVisualMonitor(task string) bool {
	words := visualAuthorityWords(task)
	if len(words) == 0 || !hasFutureVisualCondition(words) || !hasNamedVisualCondition(words) {
		return false
	}
	for _, clause := range visualAuthorityClauses(strings.ToLower(strings.TrimSpace(task))) {
		for _, segment := range splitVisualAuthoritySegments(visualAuthorityWords(clause)) {
			segment = trimVisualMonitorLeadIn(segment)
			if explicitDirectVisualSegment(segment) || explicitMonitoredPronounAction(segment) {
				return true
			}
		}
	}
	return false
}

func trimVisualMonitorLeadIn(words []string) []string {
	words = trimVisualAuthorityLeadIn(words)
	futureLead := len(words) > 0
	if futureLead {
		switch words[0] {
		case "if", "when", "whenever", "once", "until":
		default:
			futureLead = len(words) >= 3 && words[0] == "as" && words[1] == "soon" && words[2] == "as"
		}
	}
	if futureLead {
		boundary := -1
		for index, word := range words {
			if boundary >= 0 {
				break
			}
			switch word {
			case "appear", "appeared", "appears", "arrive", "arrived", "arrives", "open", "opened", "opens", "show", "shown", "shows":
				boundary = index
			}
		}
		if boundary >= 0 {
			words = words[boundary+1:]
		}
	}
	for len(words) > 0 && (words[0] == "silently" || words[0] == "immediately") {
		words = words[1:]
	}
	return words
}

func hasNamedVisualCondition(words []string) bool {
	for _, word := range words {
		switch word {
		case "alert", "banner", "dialog", "modal", "notification", "prompt", "warning":
			return true
		}
	}
	return false
}

func explicitMonitoredPronounAction(words []string) bool {
	if len(words) < 2 || words[0] != "acknowledge" {
		return false
	}
	switch words[1] {
	case "it", "that", "this", "them":
		return true
	default:
		return false
	}
}

// ExplicitVisualActionCount returns the number of complete, unambiguous screen
// actions present in the task so far. It uses the same deliberately narrow
// grammar as ExplicitVisualAuthority: this is controller state for deciding
// whether another direct-pixel action may be grounded, not an attempt to parse
// arbitrary language or predict a coordinate.
//
// In particular, an ASR prefix such as "open the review, share your" still
// contains only one complete action. Once that first action has succeeded, the
// visual actor must wait for "screen" (or equivalent completing evidence)
// rather than guessing which visible control the unfinished clause refers to.
func ExplicitVisualActionCount(task string) int {
	return len(explicitVisualActions(task))
}

// ExplicitVisualActionAt returns the zero-based complete screen command in the
// user's stated order. The returned clause is semantic authority only; direct
// pixels remain the source of the visible label and coordinates.
func ExplicitVisualActionAt(task string, index int) (string, bool) {
	actions := explicitVisualActions(task)
	if index < 0 || index >= len(actions) {
		return "", false
	}
	return actions[index], true
}

func explicitVisualActions(task string) []string {
	task = visualPrefixBeforeFutureCondition(task)
	var actions []string
	for _, clause := range visualAuthorityClauses(strings.ToLower(strings.TrimSpace(task))) {
		for _, segment := range splitVisualAuthoritySegments(visualAuthorityWords(clause)) {
			segment = trimVisualAuthorityLeadIn(segment)
			if explicitDirectVisualSegment(segment) {
				actions = append(actions, strings.Join(segment, " "))
			}
		}
	}
	return actions
}

type visualWordSpan struct {
	word  string
	start int
}

// visualPrefixBeforeFutureCondition keeps immediate actions that precede a
// monitoring clause while excluding actions governed by that condition. Byte
// offsets preserve punctuation in the prefix, which matters when two actions
// are comma-separated rather than joined by "and".
func visualPrefixBeforeFutureCondition(text string) string {
	var spans []visualWordSpan
	start := -1
	for index, char := range text {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			if start < 0 {
				start = index
			}
			continue
		}
		if start >= 0 {
			spans = append(spans, visualWordSpan{
				word: strings.ToLower(text[start:index]), start: start,
			})
			start = -1
		}
	}
	if start >= 0 {
		spans = append(spans, visualWordSpan{
			word: strings.ToLower(text[start:]), start: start,
		})
	}
	for index, span := range spans {
		conditional := false
		switch span.word {
		case "if", "when", "whenever", "once", "until":
			conditional = true
		}
		if index+1 < len(spans) {
			pair := span.word + " " + spans[index+1].word
			switch pair {
			case "wait for", "watch for", "look for":
				conditional = true
			}
		}
		if index+2 < len(spans) && span.word == "as" &&
			spans[index+1].word == "soon" && spans[index+2].word == "as" {
			conditional = true
		}
		if conditional {
			return strings.TrimSpace(text[:span.start])
		}
	}
	return text
}

// ImmediateNonvisualClause extracts only an unambiguously immediate semantic
// clause from a composite future-monitoring request. It deliberately refuses
// the generic "call me if ready" shape: a clause before if is not necessarily
// due now. Punctuation/conjunction before the condition, or explicit continuity
// language such as "without stopping your presentation", supplies the missing
// evidence that the first clause is already in progress or due immediately.
func ImmediateNonvisualClause(task string) string {
	if clause := immediateConditionalNonvisualClause(task); clause != "" {
		return clause
	}
	return immediateNonvisualAfterVisualActions(task)
}

func immediateConditionalNonvisualClause(task string) string {
	words := visualAuthorityWords(task)
	condition := -1
	for index, word := range words {
		switch word {
		case "if", "when", "whenever", "once", "until":
			condition = index
		}
		if condition >= 0 {
			break
		}
	}
	if condition < 2 {
		return ""
	}
	prefix := trimVisualAuthorityLeadIn(words[:condition])
	for len(prefix) > 0 && (prefix[len(prefix)-1] == "and" || prefix[len(prefix)-1] == "then") {
		prefix = prefix[:len(prefix)-1]
	}
	if len(prefix) < 2 || !semanticImperative(prefix[0]) {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(task))
	marker := " " + words[condition] + " "
	markerIndex := strings.Index(lower, marker)
	if markerIndex < 0 {
		return ""
	}
	before := strings.TrimSpace(lower[:markerIndex])
	after := lower[markerIndex+len(marker):]
	delimited := strings.HasSuffix(before, ",") || strings.HasSuffix(before, ";") ||
		strings.HasSuffix(before, ".") || strings.HasSuffix(before, " and") ||
		strings.HasSuffix(before, " then")
	continuous := strings.Contains(after, "without stopping") ||
		strings.Contains(after, "without pausing") ||
		strings.Contains(after, "while continuing") ||
		strings.Contains(after, "continue the presentation") ||
		strings.Contains(after, "keep presenting")
	if !delimited && !continuous {
		return ""
	}
	return strings.Join(prefix, " ")
}

func immediateNonvisualAfterVisualActions(task string) string {
	seenVisualAction := false
	for _, clause := range visualAuthorityClauses(strings.ToLower(strings.TrimSpace(task))) {
		for _, segment := range splitVisualAuthoritySegments(visualAuthorityWords(clause)) {
			segment = trimVisualAuthorityLeadIn(segment)
			if explicitDirectVisualSegment(segment) {
				seenVisualAction = true
				continue
			}
			if !seenVisualAction || !completeSemanticSegment(segment) {
				continue
			}
			return strings.Join(segment, " ")
		}
	}
	return ""
}

func completeSemanticSegment(words []string) bool {
	if len(words) < 2 {
		return false
	}
	if (words[0] == "begin" || words[0] == "start" || words[0] == "continue") &&
		len(words) >= 2 && (words[1] == "presenting" || words[1] == "speaking") {
		return true
	}
	if !semanticImperative(words[0]) {
		return false
	}
	// "tell everyone the latest" is still waiting for the thing to tell;
	// require one more word for tell/report than for a self-contained command
	// such as "present this".
	if words[0] == "tell" || words[0] == "report" {
		return len(words) >= 5
	}
	return true
}

func semanticImperative(word string) bool {
	switch word {
	case "present", "explain", "summarize", "describe", "discuss", "report", "read", "tell", "review":
		return true
	default:
		return false
	}
}

func visualAuthorityWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(char rune) bool {
		return !unicode.IsLetter(char) && !unicode.IsDigit(char)
	})
}

func visualAuthorityClauses(text string) []string {
	return strings.FieldsFunc(text, func(char rune) bool {
		switch char {
		case '.', ',', ';', ':', '!', '?', '\n', '\r':
			return true
		default:
			return false
		}
	})
}

func splitVisualAuthoritySegments(words []string) [][]string {
	var result [][]string
	start := 0
	for index, word := range words {
		if word != "and" && word != "then" && word != "but" {
			continue
		}
		if index > start {
			result = append(result, words[start:index])
		}
		start = index + 1
	}
	if start < len(words) {
		result = append(result, words[start:])
	}
	return result
}

func trimVisualAuthorityLeadIn(words []string) []string {
	for len(words) > 0 {
		switch words[0] {
		case "wait", "please", "okay", "ok", "now", "first", "next", "also":
			words = words[1:]
			continue
		}
		break
	}
	if len(words) >= 2 &&
		(words[0] == "can" || words[0] == "could" || words[0] == "would" || words[0] == "will") &&
		words[1] == "you" {
		words = words[2:]
		for len(words) > 0 && words[0] == "please" {
			words = words[1:]
		}
	}
	if len(words) >= 4 && words[0] == "i" &&
		(words[1] == "need" || words[1] == "want") && words[2] == "you" && words[3] == "to" {
		words = words[4:]
	}
	return words
}

func explicitDirectVisualSegment(words []string) bool {
	if len(words) == 0 {
		return false
	}
	for _, prefix := range [][]string{
		{"go", "to"}, {"go", "back", "to"}, {"navigate", "to"}, {"switch", "to"},
		{"click"}, {"click", "on"}, {"press"}, {"tap"}, {"select"},
	} {
		if visualWordsHavePrefix(words, prefix) && hasNamedVisualObject(words[len(prefix):]) {
			return true
		}
	}
	// "Open questions remain" is descriptive, while an article makes the
	// common computer-use imperative "open the review" unambiguous enough for
	// this high-precision path. Less explicit forms remain model decisions.
	if len(words) >= 3 && words[0] == "open" &&
		(words[1] == "the" || words[1] == "a" || words[1] == "an" ||
			words[1] == "my" || words[1] == "your" || words[1] == "our") &&
		hasNamedVisualObject(words[2:]) {
		return true
	}
	if len(words) >= 2 && words[0] == "share" {
		for _, word := range words[1:] {
			if word == "screen" || word == "window" || word == "tab" {
				return true
			}
		}
	}
	if len(words) >= 3 && words[0] == "start" && words[1] == "sharing" &&
		hasNamedVisualObject(words[2:]) {
		return true
	}
	if len(words) >= 2 && words[0] == "acknowledge" {
		for _, word := range words[1:] {
			switch word {
			case "alert", "dialog", "notification", "prompt", "warning":
				return true
			}
		}
	}
	return false
}

func visualWordsHavePrefix(words, prefix []string) bool {
	if len(words) < len(prefix) {
		return false
	}
	for index := range prefix {
		if words[index] != prefix[index] {
			return false
		}
	}
	return true
}

func hasNamedVisualObject(words []string) bool {
	for _, word := range words {
		switch word {
		case "the", "a", "an", "my", "your", "our", "this", "that", "to", "on":
			continue
		default:
			return len([]rune(word)) >= 2
		}
	}
	return false
}

func hasFutureVisualCondition(words []string) bool {
	for index, word := range words {
		switch word {
		case "if", "when", "whenever", "once", "until":
			return true
		}
		if index+1 < len(words) {
			pair := word + " " + words[index+1]
			switch pair {
			case "wait for", "watch for", "look for":
				return true
			}
		}
		if index+2 < len(words) && word == "as" && words[index+1] == "soon" && words[index+2] == "as" {
			return true
		}
	}
	return false
}
