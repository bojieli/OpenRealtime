package scenario

import (
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/bojieli/OpenRealtime/bench"
)

// SpeechWindow makes a line conditional on audible agent output. AtMS remains
// the earliest input position; LatestMS bounds the opportunity. An unmet
// opportunity fails the diagnostic instead of silently omitting the line.
type SpeechWindow struct {
	LatestMS        int
	LookbackMS      int
	MinimumActiveMS int
	RecentMS        int
}

const SpeechAcknowledgementDiagnostic = "acknowledgements during observed speech"

// Diagnostics are explicitly selected extensions. They never enter Suite's
// release population, even when every diagnostic repetition passes.
func Diagnostics() []Scenario {
	var item Scenario
	for _, candidate := range Suite() {
		if candidate.Name == "an acknowledgement is not an interruption" {
			item = candidate
		}
	}
	item.Name = SpeechAcknowledgementDiagnostic
	item.Note = "acknowledgements wait for recorded agent speech; missing opportunities fail explicitly"
	item.Script[1].AtMS = 6000
	item.Script[1].AfterSpeech = &SpeechWindow{LatestMS: 20000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}
	item.Script[2].AtMS = 11500
	item.Script[2].AfterSpeech = &SpeechWindow{LatestMS: 26000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}
	return []Scenario{item}
}

// Catalog includes the release suite followed by optional diagnostics. Default
// execution still uses Suite; exact names select entries from this catalog.
func Catalog() []Scenario { return append(Suite(), Diagnostics()...) }

func cueName(line int) string { return "scenario.line." + strconv.Itoa(line) }

// resolveSpeechCues uses actual successful input positions and independently
// recomputes the release opportunity from captured PCM. Authored AtMS values
// are never substituted for a cue that was not sent.
func resolveSpeechCues(item Scenario, timeline Timeline, transcript bench.Transcript, capture *bench.SessionAudioCapture) (Timeline, []string) {
	count := 0
	for _, line := range item.Script {
		if line.AfterSpeech != nil {
			count++
		}
	}
	if count == 0 && len(transcript.SpeechCues) == 0 {
		return timeline, nil
	}
	timeline.Spans = slices.Clone(timeline.Spans)
	failures := []string{}
	if count != len(transcript.SpeechCues) || count != len(timeline.Cues) {
		return timeline, []string{"NOT VERIFIED: speech cue evidence does not match the authored input population"}
	}
	next, previousEnd := 0, 0.0
	for lineIndex, line := range item.Script {
		if line.AfterSpeech == nil {
			continue
		}
		cue, observed := timeline.Cues[next], transcript.SpeechCues[next]
		next++
		if lineIndex >= len(timeline.Spans) {
			failures = append(failures, "NOT VERIFIED: speech cue has no timeline anchor")
			continue
		}
		timeline.Spans[lineIndex] = Span{StartMS: -1, EndMS: -1}
		window := line.AfterSpeech
		if cue.EarliestMS < max(line.AtMS, window.LookbackMS) || cue.EarliestMS > cue.LatestMS || cue.LatestMS != window.LatestMS || cue.LatestMS > 120_000 ||
			cue.LookbackMS != window.LookbackMS || cue.LookbackMS < 20 || cue.LookbackMS > 10_000 || cue.MinimumActiveMS != window.MinimumActiveMS || cue.MinimumActiveMS < 1 || cue.MinimumActiveMS > cue.LookbackMS ||
			cue.RecentMS != window.RecentMS || cue.RecentMS < 20 || cue.RecentMS > cue.LookbackMS || cue.MinimumGapMS != breathMS || len(cue.PCM16) == 0 || len(cue.PCM16) > 30_000*24 {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: speech cue on line %d differs from the authored timing policy", lineIndex))
			continue
		}
		if observed.Name != cueName(lineIndex) || observed.Name != cue.Name ||
			observed.PCM16SHA256 != fmt.Sprintf("sha256:%x", sha256.Sum256(cuePCM(cue))) ||
			observed.EarliestMS != cue.EarliestMS || observed.LatestMS != cue.LatestMS || observed.LookbackMS != cue.LookbackMS ||
			observed.MinimumActiveMS != cue.MinimumActiveMS || observed.RecentMS != cue.RecentMS || observed.MinimumGapMS != cue.MinimumGapMS ||
			observed.ExpectedSamples != len(cue.PCM16) {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: speech cue on line %d has changed identity, audio, or timing policy", lineIndex))
			continue
		}
		if observed.Status != "sent" || observed.SentSamples != observed.ExpectedSamples {
			failures = append(failures, fmt.Sprintf("speech opportunity on line %d was %s: sent %d/%d samples", lineIndex, observed.Status, observed.SentSamples, observed.ExpectedSamples))
			continue
		}
		end := float64(observed.StartMS*24+len(cue.PCM16)) / 24
		if observed.StartMS < cue.EarliestMS || observed.StartMS > cue.LatestMS || observed.EndMS != end ||
			float64(observed.StartMS) < previousEnd+float64(cue.MinimumGapMS) || capture == nil || capture.SampleRateHz != 24000 {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: speech cue on line %d has invalid position or missing capture", lineIndex))
			continue
		}
		active, recent := bench.SpeechActivity(capture.Agent, observed.StartMS, cue.LookbackMS, cue.RecentMS)
		if active < float64(cue.MinimumActiveMS) || recent == 0 || active != observed.ActiveMS || recent != observed.RecentActiveMS {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: speech cue on line %d lacks its recorded audible opportunity", lineIndex))
			continue
		}
		startSample := observed.StartMS * 24
		if startSample+len(cue.PCM16) > len(capture.RoomPCM16) || !slices.Equal(capture.RoomPCM16[startSample:startSample+len(cue.PCM16)], cue.PCM16) {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: speech cue on line %d differs from retained input audio", lineIndex))
			continue
		}
		timeline.Spans[lineIndex] = Span{StartMS: observed.StartMS, EndMS: int(math.Ceil(end))}
		previousEnd = end
	}
	return timeline, failures
}

func cuePCM(cue bench.SpeechCue) []byte {
	data := make([]byte, len(cue.PCM16)*2)
	for i, sample := range cue.PCM16 {
		data[i*2], data[i*2+1] = byte(sample), byte(uint16(sample)>>8)
	}
	return data
}
