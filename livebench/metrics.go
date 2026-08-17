package livebench

import (
	"cmp"
	"encoding/binary"
	"math"
	"slices"
	"time"
)

const EnergyVADName = "openrealtime-energy-vad-v2-fdb-timing"

type VADConfig struct {
	FrameDuration     time.Duration
	ThresholdDBFS     float64
	MinSpeechDuration time.Duration
	MinSilence        time.Duration
	MergeGap          time.Duration
}

func DefaultVADConfig() VADConfig {
	return VADConfig{
		FrameDuration: 20 * time.Millisecond, ThresholdDBFS: -42,
		MinSpeechDuration: 80 * time.Millisecond, MinSilence: 160 * time.Millisecond,
		MergeGap: 500 * time.Millisecond,
	}
}

func DetectSpeech(audio Audio, config VADConfig) []SpeechSegment {
	if audio.SampleRateHz <= 0 || len(audio.PCM16) < 2 {
		return nil
	}
	frameSamples := int(config.FrameDuration * time.Duration(audio.SampleRateHz) / time.Second)
	if frameSamples < 1 {
		return nil
	}
	minSpeechFrames := max(1, int((config.MinSpeechDuration+config.FrameDuration-1)/config.FrameDuration))
	minSilenceFrames := max(1, int((config.MinSilence+config.FrameDuration-1)/config.FrameDuration))
	totalSamples := len(audio.PCM16) / 2
	var raw []SpeechSegment
	inSpeech := false
	startFrame, speechFrames, silenceFrames := 0, 0, 0
	frameCount := (totalSamples + frameSamples - 1) / frameSamples
	for frame := range frameCount {
		start := frame * frameSamples
		end := min(start+frameSamples, totalSamples)
		active := frameDBFS(audio.PCM16[start*2:end*2]) >= config.ThresholdDBFS
		if active {
			if !inSpeech {
				if speechFrames == 0 {
					startFrame = frame
				}
				speechFrames++
				if speechFrames >= minSpeechFrames {
					inSpeech = true
					silenceFrames = 0
				}
			} else {
				silenceFrames = 0
			}
			continue
		}
		if !inSpeech {
			speechFrames = 0
			continue
		}
		silenceFrames++
		if silenceFrames >= minSilenceFrames {
			endFrame := frame - silenceFrames + 1
			raw = append(raw, segmentFromFrames(startFrame, endFrame, config.FrameDuration))
			inSpeech = false
			speechFrames = 0
			silenceFrames = 0
		}
	}
	if inSpeech {
		endFrame := frameCount - silenceFrames
		raw = append(raw, segmentFromFrames(startFrame, endFrame, config.FrameDuration))
	}
	if len(raw) < 2 {
		return raw
	}
	merged := []SpeechSegment{raw[0]}
	mergeGapMS := milliseconds(config.MergeGap)
	for _, segment := range raw[1:] {
		last := &merged[len(merged)-1]
		if segment.StartMS-last.EndMS <= mergeGapMS {
			last.EndMS = segment.EndMS
			continue
		}
		merged = append(merged, segment)
	}
	return merged
}

func frameDBFS(frame []byte) float64 {
	if len(frame) < 2 {
		return math.Inf(-1)
	}
	var sum float64
	for offset := 0; offset+1 < len(frame); offset += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(frame[offset:]))) / 32768
		sum += sample * sample
	}
	rms := math.Sqrt(sum / float64(len(frame)/2))
	if rms == 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

func segmentFromFrames(start, end int, frameDuration time.Duration) SpeechSegment {
	return SpeechSegment{
		StartMS: milliseconds(time.Duration(start) * frameDuration),
		EndMS:   milliseconds(time.Duration(end) * frameDuration),
	}
}

func ScoreTiming(input, output Audio, overlapStartS, overlapEndS *float64) TimingMetrics {
	userConfig := DefaultVADConfig()
	userConfig.MergeGap = 600 * time.Millisecond
	userSegments := DetectSpeech(input, userConfig)
	outputSegments := DetectSpeech(output, DefaultVADConfig())
	stopIntervals := speechIntersections(userSegments, outputSegments)
	responseIntervals := responseGaps(userSegments, outputSegments)
	metrics := TimingMetrics{
		VAD: EnergyVADName, UserSegments: userSegments, OutputSegments: outputSegments,
		LatencyStopIntervals: stopIntervals, LatencyResponseIntervals: responseIntervals,
		MeanStopLatencyMS: meanIntervalDuration(stopIntervals), MeanResponseLatencyMS: meanIntervalDuration(responseIntervals),
	}
	if len(outputSegments) > 0 {
		value := outputSegments[0].StartMS
		metrics.FirstOutputMS = &value
	}
	if overlapStartS == nil || overlapEndS == nil {
		return metrics
	}
	startMS, endMS := *overlapStartS*1000, *overlapEndS*1000
	for _, segment := range outputSegments {
		overlapStart := max(segment.StartMS, startMS)
		overlapEnd := min(segment.EndMS, endMS)
		if overlapEnd > overlapStart {
			metrics.SpeechDuringOverlap = true
			metrics.OverlapSpeechMS += overlapEnd - overlapStart
		}
	}
	return metrics
}

// speechIntersections reproduces Full-Duplex-Bench get_timing.py's overlap
// interval and millisecond-rounded de-duplication rules.
func speechIntersections(user, model []SpeechSegment) []SpeechSegment {
	type candidate struct {
		segment SpeechSegment
		endKey  int64
	}
	var raw []candidate
	for userIndex, modelIndex := 0, 0; userIndex < len(user) && modelIndex < len(model); {
		userSegment, modelSegment := user[userIndex], model[modelIndex]
		start, end := max(userSegment.StartMS, modelSegment.StartMS), min(userSegment.EndMS, modelSegment.EndMS)
		if end > start {
			raw = append(raw, candidate{segment: SpeechSegment{StartMS: start, EndMS: end}, endKey: int64(math.Round(end))})
		}
		if userSegment.EndMS < modelSegment.EndMS {
			userIndex++
		} else {
			modelIndex++
		}
	}
	best := make(map[int64]SpeechSegment)
	for _, item := range raw {
		prior, exists := best[item.endKey]
		if !exists || item.segment.EndMS-item.segment.StartMS < prior.EndMS-prior.StartMS {
			best[item.endKey] = item.segment
		}
	}
	result := make([]SpeechSegment, 0, len(best))
	for _, segment := range best {
		result = append(result, segment)
	}
	slices.SortFunc(result, func(left, right SpeechSegment) int { return cmp.Compare(left.EndMS, right.EndMS) })
	return result
}

// responseGaps reproduces Full-Duplex-Bench's next-model-start rule and keeps
// only the shortest gap for a shared millisecond-rounded model start.
func responseGaps(user, model []SpeechSegment) []SpeechSegment {
	best := make(map[int64]SpeechSegment)
	for _, userSegment := range user {
		for _, modelSegment := range model {
			if modelSegment.StartMS <= userSegment.EndMS {
				continue
			}
			key := int64(math.Round(modelSegment.StartMS))
			candidate := SpeechSegment{StartMS: userSegment.EndMS, EndMS: modelSegment.StartMS}
			prior, exists := best[key]
			if !exists || candidate.StartMS > prior.StartMS {
				best[key] = candidate
			}
			break
		}
	}
	result := make([]SpeechSegment, 0, len(best))
	for _, segment := range best {
		result = append(result, segment)
	}
	slices.SortFunc(result, func(left, right SpeechSegment) int { return cmp.Compare(left.EndMS, right.EndMS) })
	return result
}

func meanIntervalDuration(intervals []SpeechSegment) *float64 {
	if len(intervals) == 0 {
		return nil
	}
	var total float64
	for _, interval := range intervals {
		total += interval.EndMS - interval.StartMS
	}
	value := total / float64(len(intervals))
	return &value
}
