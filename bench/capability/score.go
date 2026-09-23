package capability

import (
	"strings"
	"time"
	"unicode"
)

// ContentScreen is a conservative lexical diagnostic, not semantic or human
// validation. Only entirely played segments inside the window contribute text.
// Withheld-feedback controls are scored by the same rule, never forced to fail.
type ContentScreen struct {
	Passed                     bool       `json:"passed"`
	Text                       string     `json:"text"`
	MissingGroups              [][]string `json:"missing_groups,omitempty"`
	Forbidden                  []string   `json:"forbidden,omitempty"`
	TooShort                   bool       `json:"too_short"`
	InvalidPlaybackEvidence    bool       `json:"invalid_playback_evidence"`
	SpeechInSilentWindow       bool       `json:"speech_in_silent_window"`
	EstimatedFromSynthesisText bool       `json:"estimated_from_synthesis_text"`
}

func normalizeText(text string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
}

func containsPhrase(text, term string) bool {
	return strings.Contains(" "+normalizeText(text)+" ", " "+normalizeText(term)+" ")
}

// ScreenContent scores the window that opens when the expectation's anchor is
// admitted. Speech generated before the window opened is not an adaptation to
// the evidence and does not count.
func ScreenContent(expect Expectation, opens time.Duration, playback Playback) ContentScreen {
	score := ContentScreen{EstimatedFromSynthesisText: true}
	closes := opens + expect.Within
	var completed []string
	for _, segment := range playback.Segments {
		if !validPartialPlayback(segment) {
			score.InvalidPlaybackEvidence = true
			continue
		}
		if segment.CompletedAt != nil && !validCompletedSegment(segment) {
			score.InvalidPlaybackEvidence = true
			continue
		}
		if segment.PlayedSamples > 0 && *segment.FirstPlayedAt < closes && *segment.LastPlayedAt > opens {
			score.SpeechInSilentWindow = true
		}
		if segment.GeneratedAt < opens || segment.Cut || segment.CompletedAt == nil ||
			*segment.CompletedAt > closes || segment.PlayedSamples != segment.GeneratedSamples ||
			segment.PlayedSamples == 0 {
			continue
		}
		completed = append(completed, segment.Text)
	}
	score.Text = strings.Join(completed, " ")
	if expect.Silent {
		score.Passed = !score.InvalidPlaybackEvidence && !score.SpeechInSilentWindow
		return score
	}
	for _, group := range expect.RequireAnyOf {
		found := false
		for _, term := range group {
			if containsPhrase(score.Text, term) {
				found = true
				break
			}
		}
		if !found {
			score.MissingGroups = append(score.MissingGroups, group)
		}
	}
	for _, term := range expect.Forbid {
		if containsPhrase(score.Text, term) {
			score.Forbidden = append(score.Forbidden, term)
		}
	}
	score.TooShort = len(strings.Fields(normalizeText(score.Text))) < expect.MinWords
	score.Passed = !score.InvalidPlaybackEvidence && !score.TooShort && len(score.MissingGroups) == 0 && len(score.Forbidden) == 0
	return score
}

func validCompletedSegment(s Segment) bool {
	if !s.Ended || s.Cut || s.SampleRate <= 0 || s.GeneratedSamples <= 0 ||
		s.PlayedSamples != s.GeneratedSamples || s.FirstPlayedAt == nil ||
		s.LastPlayedAt == nil || s.CompletedAt == nil {
		return false
	}
	duration := time.Duration(s.PlayedSamples) * time.Second / time.Duration(s.SampleRate)
	return *s.FirstPlayedAt >= s.GeneratedAt && *s.LastPlayedAt >= *s.FirstPlayedAt+duration-time.Millisecond &&
		*s.CompletedAt >= *s.LastPlayedAt
}

// Even unfinished audio requires internally consistent playback marks. Missing
// marks cannot be treated as proof of silence.
func validPartialPlayback(s Segment) bool {
	if s.SampleRate <= 0 || s.GeneratedAt < 0 || s.GeneratedSamples < 0 ||
		s.PlayedSamples < 0 || s.PlayedSamples > s.GeneratedSamples {
		return false
	}
	if s.PlayedSamples == 0 {
		return s.FirstPlayedAt == nil && s.LastPlayedAt == nil && s.CompletedAt == nil
	}
	if s.FirstPlayedAt == nil || s.LastPlayedAt == nil {
		return false
	}
	duration := time.Duration(s.PlayedSamples) * time.Second / time.Duration(s.SampleRate)
	return *s.FirstPlayedAt >= s.GeneratedAt && *s.LastPlayedAt >= *s.FirstPlayedAt+duration-time.Millisecond
}
