package bench

import (
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"strings"
)

// SpeechCue reserves a bounded, initially silent input interval for speech that
// starts only after audible agent output has played. PCM16 is mono 24 kHz.
// Cues execute in declaration order, with MinimumGapMS after the previous cue.
// Packet arrival and queued future audio cannot satisfy the opportunity.
type SpeechCue struct {
	Name                                                string
	PCM16                                               []int16
	EarliestMS, LatestMS                                int
	LookbackMS, MinimumActiveMS, RecentMS, MinimumGapMS int
}

// SpeechCueObservation records the actual input clock and the observation that
// released one authored cue. SentSamples is updated only after transport send
// succeeds. Missed opportunities are behavioral evidence, never silent passes.
type SpeechCueObservation struct {
	Name            string  `json:"name"`
	Status          string  `json:"status"`
	PCM16SHA256     string  `json:"pcm16_sha256"`
	EarliestMS      int     `json:"earliest_ms"`
	LatestMS        int     `json:"latest_ms"`
	LookbackMS      int     `json:"lookback_ms"`
	MinimumActiveMS int     `json:"minimum_active_ms"`
	RecentMS        int     `json:"recent_ms"`
	MinimumGapMS    int     `json:"minimum_gap_ms"`
	StartMS         int     `json:"start_ms,omitempty"`
	EndMS           float64 `json:"end_ms,omitempty"`
	ActiveMS        float64 `json:"active_ms,omitempty"`
	RecentActiveMS  float64 `json:"recent_active_ms,omitempty"`
	ExpectedSamples int     `json:"expected_samples"`
	SentSamples     int     `json:"sent_samples"`
}

type speechCuePlayer struct {
	cues          []SpeechCue
	observed      []SpeechCueObservation
	next          int
	lastEndSample int
}

func prepareSpeechCues(cues []SpeechCue, samples []int16, realtime bool) (*speechCuePlayer, error) {
	player := &speechCuePlayer{}
	if len(cues) == 0 {
		return player, nil
	}
	if !realtime || len(cues) > 32 {
		return nil, fmt.Errorf("speech cues require realtime playback and at most 32 cues")
	}
	names := map[string]bool{}
	for i, cue := range cues {
		if cue.Name == "" || cue.Name != strings.TrimSpace(cue.Name) || len(cue.Name) > 128 || strings.ContainsAny(cue.Name, "\x00\r\n") || names[cue.Name] {
			return nil, fmt.Errorf("speech cue %d requires a unique canonical name", i)
		}
		names[cue.Name] = true
		if len(cue.PCM16) == 0 || len(cue.PCM16) > 30_000*24 ||
			cue.LookbackMS < 20 || cue.LookbackMS > 10_000 || cue.MinimumActiveMS < 1 || cue.MinimumActiveMS > cue.LookbackMS ||
			cue.RecentMS < 20 || cue.RecentMS > cue.LookbackMS || cue.MinimumGapMS < 0 || cue.MinimumGapMS > 10_000 ||
			cue.EarliestMS < cue.LookbackMS || cue.LatestMS < cue.EarliestMS || cue.LatestMS > 120_000 ||
			cue.LatestMS*24+len(cue.PCM16) > len(samples) || i > 0 && cue.EarliestMS < cues[i-1].EarliestMS {
			return nil, fmt.Errorf("speech cue %q has invalid bounded timing or audio", cue.Name)
		}
		for _, sample := range samples[cue.EarliestMS*24 : cue.LatestMS*24+len(cue.PCM16)] {
			if sample != 0 {
				return nil, fmt.Errorf("speech cue %q overlaps authored static input", cue.Name)
			}
		}
		cue.PCM16 = slices.Clone(cue.PCM16)
		player.cues = append(player.cues, cue)
		player.observed = append(player.observed, SpeechCueObservation{
			Name: cue.Name, Status: "unobserved", PCM16SHA256: fmt.Sprintf("sha256:%x", sha256.Sum256(encodePCM(cue.PCM16))),
			EarliestMS: cue.EarliestMS, LatestMS: cue.LatestMS, LookbackMS: cue.LookbackMS, MinimumActiveMS: cue.MinimumActiveMS,
			RecentMS: cue.RecentMS, MinimumGapMS: cue.MinimumGapMS, ExpectedSamples: len(cue.PCM16),
		})
	}
	return player, nil
}

// SpeechActivity measures only the bounded window ending at atMS. It uses
// 20 ms RMS frames at PCM16 128, the same audible proxy as scenario holds.
// Recent activity must include non-silent samples in the recent subwindow.
func SpeechActivity(chunks []TimedAudioChunk, atMS, lookbackMS, recentMS int) (activeMS, recentActiveMS float64) {
	if lookbackMS < 20 || lookbackMS > 10_000 || atMS < lookbackMS || atMS > 120_000 || recentMS < 20 || recentMS > lookbackMS {
		return 0, 0
	}
	from := atMS - lookbackMS
	window := make([]int16, lookbackMS*24)
	for _, chunk := range chunks {
		end := chunk.AtMS + float64(len(chunk.PCM16))/24
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < 0 || end <= float64(from) || chunk.AtMS >= float64(atMS) {
			continue
		}
		offset := int(math.Round(chunk.AtMS*24)) - from*24
		start, stop := max(0, -offset), min(len(chunk.PCM16), len(window)-offset)
		if start < stop {
			copy(window[offset+start:offset+stop], chunk.PCM16[start:stop])
		}
	}
	for offset := 0; offset < len(window); offset += 480 {
		end := min(offset+480, len(window))
		energy := 0.0
		for _, sample := range window[offset:end] {
			energy += float64(sample) * float64(sample)
		}
		if energy < float64(end-offset)*128*128 {
			continue
		}
		activeMS += float64(end-offset) / 24
		// Recompute clipped recent frames; a vowel outside the recent window
		// must not make its silent tail count as ongoing speech.
		start := max(offset, (lookbackMS-recentMS)*24)
		if start >= end {
			continue
		}
		energy = 0
		for _, sample := range window[start:end] {
			energy += float64(sample) * float64(sample)
		}
		if energy >= float64(end-start)*128*128 {
			recentActiveMS += float64(end-start) / 24
		}
	}
	return
}

func (player *speechCuePlayer) frame(offset int, base []int16, recorder *sessionAudioRecorder) []int16 {
	if player.next >= len(player.cues) {
		return base
	}
	cue, observed := player.cues[player.next], &player.observed[player.next]
	atMS := offset / 24
	if observed.Status == "unobserved" {
		if atMS > cue.LatestMS {
			observed.Status = "missed"
			player.next++
			return player.frame(offset, base, recorder)
		}
		if atMS < cue.EarliestMS || offset < player.lastEndSample+cue.MinimumGapMS*24 {
			return base
		}
		recorder.mu.Lock()
		active, recent := SpeechActivity(recorder.agent, atMS, cue.LookbackMS, cue.RecentMS)
		recorder.mu.Unlock()
		if active < float64(cue.MinimumActiveMS) || recent == 0 {
			return base
		}
		observed.Status, observed.StartMS, observed.ActiveMS, observed.RecentActiveMS = "partial", atMS, active, recent
		observed.EndMS = float64(offset+len(cue.PCM16)) / 24
	}
	frame := slices.Clone(base)
	position := offset - observed.StartMS*24
	if position >= 0 && position < len(cue.PCM16) {
		copy(frame, cue.PCM16[position:min(position+len(frame), len(cue.PCM16))])
	}
	return frame
}

func (player *speechCuePlayer) sent(offset int, frame []int16, recorder *sessionAudioRecorder) {
	if len(player.cues) == 0 {
		return
	}
	recorder.mu.Lock()
	copy(recorder.room[offset:offset+len(frame)], frame)
	recorder.mu.Unlock()
	if player.next >= len(player.cues) {
		return
	}
	observed := &player.observed[player.next]
	if observed.Status != "partial" {
		return
	}
	observed.SentSamples += min(len(frame), observed.ExpectedSamples-observed.SentSamples)
	if observed.SentSamples == observed.ExpectedSamples {
		observed.Status = "sent"
		player.lastEndSample = observed.StartMS*24 + observed.ExpectedSamples
		player.next++
	}
}
