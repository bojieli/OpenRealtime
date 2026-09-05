package scenario

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
)

// ScorerVersion changes whenever the deterministic checks or their matching
// semantics change. Earlier unversioned results used substring matching and
// weaker translation requirements. Version 2 added whole-word content checks;
// version 3 also requires recorded acoustic continuity across acknowledgements.
// Version 4 supplies refund source content, checks explanation coverage, and
// retains response-terminal evidence without inferring cancellation from silence.
// Earlier scores are not evidence for this version.
const ScorerVersion uint64 = 4

func validateCheckKind(check Check) error {
	switch check.Kind {
	case CheckSilent, CheckSpoke, CheckAnsweredWithin, CheckResumed:
	case CheckHeldAcross:
		if check.Line < 0 || check.Sight != 0 || check.FromMS != 0 ||
			check.BeforeMS <= 0 || check.AfterMS <= 0 || check.MaxGapMS <= 0 {
			return errors.New("held-across check requires a spoken line and positive before, after, and gap windows")
		}
		if check.BeforeMS > 10_000 || check.AfterMS > 10_000 || check.MaxGapMS > 10_000 {
			return errors.New("held-across check window exceeds ten seconds")
		}
	case CheckToolCalled:
		if strings.TrimSpace(check.Tool) == "" {
			return errors.New("tool check has no tool name")
		}
	case CheckReachedMenu:
	case CheckSaid, CheckNotSaid:
		if len(check.Any) == 0 {
			return errors.New("content check has no phrases")
		}
		for _, phrase := range check.Any {
			if strings.TrimSpace(phrase) == "" {
				return errors.New("content check has an empty phrase")
			}
		}
	default:
		return fmt.Errorf("unknown kind %q", check.Kind)
	}
	return nil
}

func validateCheckAnchor(check Check, lines, sights int) error {
	// These two checks concern the call's complete action history, not a
	// timed response. Their zero-valued Line does not require spoken input.
	if check.Kind == CheckToolCalled || check.Kind == CheckReachedMenu {
		return nil
	}
	if check.Sight < 0 || check.Sight > sights {
		return fmt.Errorf("sight %d is absent from the timeline", check.Sight)
	}
	if check.Sight == 0 && check.Line >= lines {
		return fmt.Errorf("line %d is absent from the timeline", check.Line)
	}
	if check.Kind == CheckResumed && (check.Interrupted < 0 || check.Interrupted >= lines) {
		return errors.New("interruption is absent from the timeline")
	}
	return nil
}

func validateScenarioChecks(item Scenario) error {
	if len(item.Checks) == 0 {
		return errors.New("scenario has no behavior checks")
	}
	for _, check := range item.Checks {
		if err := validateCheckKind(check); err != nil {
			return err
		}
		if err := validateCheckAnchor(check, len(item.Script), len(item.Sees)); err != nil {
			return err
		}
		if check.Kind == CheckReachedMenu && item.Menu == nil {
			return errors.New("menu outcome is unavailable")
		}
	}
	return nil
}

func validateCheck(check Check, timeline Timeline) error {
	if err := validateCheckKind(check); err != nil {
		return err
	}
	if err := validateCheckAnchor(check, len(timeline.Spans), len(timeline.Sights)); err != nil {
		return err
	}
	if check.Kind == CheckToolCalled || check.Kind == CheckReachedMenu {
		return nil
	}
	if check.Sight > 0 {
		if !canAddMS(timeline.Sights[check.Sight-1], check.AfterMS) {
			return errors.New("sight window is outside the timeline clock")
		}
	} else if check.Line >= 0 {
		span := timeline.Spans[check.Line]
		if span.StartMS < 0 || span.EndMS <= span.StartMS ||
			!canAddMS(span.EndMS, check.AfterMS) || !canAddMS(span.EndMS, check.FromMS) {
			return errors.New("line window is outside the timeline clock")
		}
	}
	return nil
}

func canAddMS(base, offset int) bool {
	return base >= 0 && offset >= -base && (offset <= 0 || base <= math.MaxInt-offset)
}

func checkedText(check Check, transcript bench.Transcript, from, to int) string {
	line := check.Line
	if check.Sight > 0 {
		// A sight owns its window even if Line requests the whole conversation.
		line = 0
	}
	return normalizePhrase(saidBetween(transcript, line, from, to))
}

func normalizePhrase(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func phraseWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || r == '_'
}

// containsPhrase keeps punctuation checks such as "?" literal, but a word or
// number cannot be found inside a different word or number. In particular,
// "none", "undone", and "30" are not evidence for "one", "done", or "3".
func containsPhrase(text, phrase string) bool {
	phrase = normalizePhrase(phrase)
	if phrase == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(phrase)
	last, _ := utf8.DecodeLastRuneInString(phrase)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], phrase)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(phrase)
		left, right := true, true
		if start > 0 && phraseWord(first) {
			previous, _ := utf8.DecodeLastRuneInString(text[:start])
			left = !phraseWord(previous)
		}
		if end < len(text) && phraseWord(last) {
			next, _ := utf8.DecodeRuneInString(text[end:])
			right = !phraseWord(next)
		}
		if left && right {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		offset = start + size
	}
	return false
}
