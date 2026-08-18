// Package audiobench measures real incremental ASR and streaming TTS providers
// without depending on an OpenAI Realtime wire extension.
package audiobench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/livebench"
)

const SchemaVersion = "0.3.0"

// ASRConfig describes one immutable input utterance and cadence condition.
type ASRConfig struct {
	CaseID        string
	InputSHA256   string
	ReferenceText string
	Audio         livebench.Audio
	FrameDuration time.Duration
	Paced         bool
	// OnRevision receives validated provider revisions after their benchmark
	// timestamp is recorded. It should enqueue work and return quickly; blocking
	// time is intentionally visible in end-to-end wall latency.
	OnRevision func(context.Context, v1.PerceptionRevision) error
}

// RevisionRecord preserves only semantic text and timing, never input audio.
type RevisionRecord struct {
	RevisionID   uint64  `json:"revision_id"`
	SourceSample uint64  `json:"source_sample"`
	SourceMS     float64 `json:"source_ms"`
	WallMS       float64 `json:"wall_ms"`
	StableText   string  `json:"stable_text,omitempty"`
	UnstableText string  `json:"unstable_text,omitempty"`
	Delta        string  `json:"delta,omitempty"`
	Final        bool    `json:"final"`
}

// ASRFrameRecord captures scheduler delay and elapsed time at the provider
// boundary. ProviderBoundaryMS includes local admission, transport, and model
// service time; it is not a GPU-kernel compute measurement.
type ASRFrameRecord struct {
	Index               uint64  `json:"index"`
	EndSample           uint64  `json:"end_sample"`
	AudioEndMS          float64 `json:"audio_end_ms"`
	InvocationStartMS   float64 `json:"invocation_start_ms"`
	ScheduleLatenessMS  float64 `json:"schedule_lateness_ms"`
	ProviderBoundaryMS  float64 `json:"provider_boundary_ms"`
	ProviderInvocations uint64  `json:"provider_invocations"`
	RevisionCount       int     `json:"revision_count"`
}

// ASRResult is a secret-free measurement of one complete utterance.
type ASRResult struct {
	CaseID                   string           `json:"case_id"`
	InputSHA256              string           `json:"input_sha256"`
	Descriptor               v1.Descriptor    `json:"descriptor"`
	Paced                    bool             `json:"paced"`
	FrameDurationMS          float64          `json:"frame_duration_ms"`
	InputSampleRateHz        int              `json:"input_sample_rate_hz"`
	InputDurationMS          float64          `json:"input_duration_ms"`
	Frames                   []ASRFrameRecord `json:"frames"`
	Revisions                []RevisionRecord `json:"revisions"`
	SchedulerTicks           uint64           `json:"scheduler_ticks"`
	ProviderInvocations      uint64           `json:"provider_invocations"`
	FinalizationInvocations  uint64           `json:"finalization_provider_invocations"`
	FirstPartialSourceMS     *float64         `json:"first_partial_source_ms,omitempty"`
	FirstPartialWallMS       *float64         `json:"first_partial_wall_ms,omitempty"`
	EndToFinalMS             float64          `json:"end_to_final_ms"`
	FinalizationBoundaryMS   float64          `json:"finalization_provider_boundary_ms"`
	ElapsedMS                float64          `json:"elapsed_ms"`
	ProviderServiceMS        float64          `json:"provider_service_ms"`
	ProviderServiceRTF       float64          `json:"provider_service_rtf"`
	MeanProviderInvocationMS float64          `json:"mean_provider_invocation_ms"`
	P95ProviderInvocationMS  float64          `json:"p95_provider_invocation_ms"`
	MaxScheduleLatenessMS    float64          `json:"max_schedule_lateness_ms"`
	FinalTranscript          string           `json:"final_transcript"`
	ReferenceText            string           `json:"reference_text,omitempty"`
	WordErrorRate            *float64         `json:"word_error_rate,omitempty"`
}

// RunASR replays audio at the declared cadence when Paced is true. A tick opens
// an invocation only after its frame has arrived; if inference falls behind,
// subsequent frames are processed immediately and lateness is recorded.
func RunASR(ctx context.Context, provider v1.PerceptionProvider, config ASRConfig) (ASRResult, error) {
	if provider == nil {
		return ASRResult{}, errors.New("ASR provider is required")
	}
	config.CaseID = strings.TrimSpace(config.CaseID)
	if config.CaseID == "" {
		return ASRResult{}, errors.New("ASR case ID is required")
	}
	if config.Audio.SampleRateHz <= 0 || len(config.Audio.PCM16) == 0 || len(config.Audio.PCM16)%2 != 0 {
		return ASRResult{}, errors.New("ASR benchmark requires non-empty PCM16 audio")
	}
	if config.FrameDuration <= 0 {
		return ASRResult{}, errors.New("ASR frame duration must be positive")
	}
	frameSamples := int(config.FrameDuration * time.Duration(config.Audio.SampleRateHz) / time.Second)
	if frameSamples <= 0 {
		return ASRResult{}, errors.New("ASR frame duration is shorter than one sample")
	}
	if config.InputSHA256 == "" {
		digest := sha256.Sum256(config.Audio.PCM16)
		config.InputSHA256 = hex.EncodeToString(digest[:])
	}

	inputSamples := len(config.Audio.PCM16) / 2
	inputDuration := time.Duration(inputSamples) * time.Second / time.Duration(config.Audio.SampleRateHz)
	result := ASRResult{
		CaseID: config.CaseID, InputSHA256: config.InputSHA256, Descriptor: provider.Descriptor(),
		Paced: config.Paced, FrameDurationMS: milliseconds(config.FrameDuration),
		InputSampleRateHz: config.Audio.SampleRateHz, InputDurationMS: milliseconds(inputDuration),
		ReferenceText: config.ReferenceText,
	}
	started := time.Now()
	var providerService time.Duration
	var providerInvocations uint64
	frameDurations := make([]float64, 0, (inputSamples+frameSamples-1)/frameSamples)
	for index, startSample := uint64(0), 0; startSample < inputSamples; index, startSample = index+1, startSample+frameSamples {
		endSample := min(startSample+frameSamples, inputSamples)
		audioEnd := time.Duration(endSample) * time.Second / time.Duration(config.Audio.SampleRateHz)
		if config.Paced {
			if err := waitUntil(ctx, started.Add(audioEnd)); err != nil {
				return ASRResult{}, err
			}
		}
		invocationStart := time.Now()
		wallStart := invocationStart.Sub(started)
		beforeCount, countsInvocations := providerInvocationCount(provider)
		providerStart := time.Now()
		revisions, err := provider.PushFrame(ctx, v1.AudioFrame{
			Index: index, SampleOffset: uint64(startSample), SampleRateHz: uint32(config.Audio.SampleRateHz),
			PCM16LE: config.Audio.PCM16[startSample*2 : endSample*2],
		})
		providerDuration := time.Since(providerStart)
		if err != nil {
			return ASRResult{}, fmt.Errorf("ASR frame %d: %w", index, err)
		}
		invocations := uint64(1)
		if countsInvocations {
			afterCount, _ := providerInvocationCount(provider)
			if afterCount < beforeCount {
				return ASRResult{}, errors.New("ASR provider invocation counter moved backwards")
			}
			invocations = afterCount - beforeCount
		}
		providerInvocations += invocations
		if invocations > 0 {
			providerService += providerDuration
			frameDurations = append(frameDurations, milliseconds(providerDuration))
		}
		frameMS := milliseconds(providerDuration)
		lateness := wallStart - audioEnd
		if lateness < 0 {
			lateness = 0
		}
		result.MaxScheduleLatenessMS = max(result.MaxScheduleLatenessMS, milliseconds(lateness))
		result.Frames = append(result.Frames, ASRFrameRecord{
			Index: index, EndSample: uint64(endSample), AudioEndMS: milliseconds(audioEnd),
			InvocationStartMS: milliseconds(wallStart), ScheduleLatenessMS: milliseconds(lateness),
			ProviderBoundaryMS: frameMS, ProviderInvocations: invocations, RevisionCount: len(revisions),
		})
		for _, revision := range revisions {
			recordRevision(&result, revision, time.Since(started), config.Audio.SampleRateHz)
			if config.OnRevision != nil {
				if err := config.OnRevision(ctx, revision); err != nil {
					return ASRResult{}, fmt.Errorf("observe ASR revision %d: %w", revision.RevisionID, err)
				}
			}
		}
	}
	finalizationBefore, finalizationCounts := providerInvocationCount(provider)
	finalizationStart := time.Now()
	finalRevision, err := provider.Finalize(ctx, uint64(inputSamples))
	finalizationDuration := time.Since(finalizationStart)
	providerService += finalizationDuration
	if err != nil {
		return ASRResult{}, fmt.Errorf("finalize ASR: %w", err)
	}
	if finalizationCounts {
		finalizationAfter, _ := providerInvocationCount(provider)
		if finalizationAfter < finalizationBefore {
			return ASRResult{}, errors.New("ASR provider invocation counter moved backwards during finalization")
		}
		result.FinalizationInvocations = finalizationAfter - finalizationBefore
		providerInvocations += result.FinalizationInvocations
	}
	recordRevision(&result, finalRevision, time.Since(started), config.Audio.SampleRateHz)
	if config.OnRevision != nil {
		if err := config.OnRevision(ctx, finalRevision); err != nil {
			return ASRResult{}, fmt.Errorf("observe final ASR revision %d: %w", finalRevision.RevisionID, err)
		}
	}
	elapsed := time.Since(started)
	result.ElapsedMS = milliseconds(elapsed)
	result.SchedulerTicks = uint64(len(result.Frames))
	result.ProviderInvocations = providerInvocations
	result.FinalizationBoundaryMS = milliseconds(finalizationDuration)
	result.ProviderServiceMS = milliseconds(providerService)
	result.EndToFinalMS = milliseconds(elapsed - inputDuration)
	if result.EndToFinalMS < 0 {
		result.EndToFinalMS = 0
	}
	if inputDuration > 0 {
		result.ProviderServiceRTF = float64(providerService) / float64(inputDuration)
	}
	result.MeanProviderInvocationMS = mean(frameDurations)
	result.P95ProviderInvocationMS = nearestRank(frameDurations, 0.95)
	result.FinalTranscript = finalRevision.StableText
	if strings.TrimSpace(config.ReferenceText) != "" {
		value := WordErrorRate(config.ReferenceText, result.FinalTranscript)
		result.WordErrorRate = &value
	}
	return result, nil
}

type invocationCounter interface {
	ProviderInvocationCount() uint64
}

func providerInvocationCount(provider v1.PerceptionProvider) (uint64, bool) {
	counter, ok := provider.(invocationCounter)
	if !ok {
		return 0, false
	}
	return counter.ProviderInvocationCount(), true
}

func recordRevision(result *ASRResult, revision v1.PerceptionRevision, elapsed time.Duration, sampleRate int) {
	sourceMS := float64(revision.SourceSample) * 1_000 / float64(sampleRate)
	wallMS := milliseconds(elapsed)
	result.Revisions = append(result.Revisions, RevisionRecord{
		RevisionID: revision.RevisionID, SourceSample: revision.SourceSample,
		SourceMS: sourceMS, WallMS: wallMS, StableText: revision.StableText,
		UnstableText: revision.UnstableText, Delta: revision.Delta, Final: revision.Final,
	})
	text := revision.StableText + revision.UnstableText
	if !revision.Final && strings.TrimSpace(text) != "" && result.FirstPartialWallMS == nil {
		source, wall := sourceMS, wallMS
		result.FirstPartialSourceMS = &source
		result.FirstPartialWallMS = &wall
	}
}

// TTSConfig describes one synthesis measurement.
type TTSConfig struct {
	CaseID string
	Text   string
}

// TTSChunkRecord contains timing and size without embedding audio bytes.
type TTSChunkRecord struct {
	Sequence     int     `json:"sequence"`
	ArrivalMS    float64 `json:"arrival_ms"`
	AudioBytes   int     `json:"audio_bytes"`
	SampleOffset uint64  `json:"sample_offset"`
	SampleRateHz uint32  `json:"sample_rate_hz"`
	Final        bool    `json:"final"`
}

// TTSResult includes OutputPCM16 only in memory; report JSON contains its hash.
type TTSResult struct {
	CaseID           string           `json:"case_id"`
	Text             string           `json:"text"`
	Descriptor       v1.Descriptor    `json:"descriptor"`
	FirstAudioMS     float64          `json:"first_audio_ms"`
	ElapsedMS        float64          `json:"elapsed_ms"`
	OutputDurationMS float64          `json:"output_duration_ms"`
	RealTimeFactor   float64          `json:"real_time_factor"`
	OutputBytes      int              `json:"output_bytes"`
	OutputSHA256     string           `json:"output_sha256"`
	Chunks           []TTSChunkRecord `json:"chunks"`
	OutputPCM16      []byte           `json:"-"`
	OutputSampleRate int              `json:"output_sample_rate_hz"`
}

// RunTTS measures time to the first actual PCM bytes and validates continuous
// sample offsets and exactly one final chunk.
func RunTTS(ctx context.Context, provider v1.StreamingSpeechProvider, config TTSConfig) (TTSResult, error) {
	if provider == nil {
		return TTSResult{}, errors.New("TTS provider is required")
	}
	config.CaseID = strings.TrimSpace(config.CaseID)
	config.Text = strings.TrimSpace(config.Text)
	if config.CaseID == "" || config.Text == "" {
		return TTSResult{}, errors.New("TTS case ID and text are required")
	}
	result := TTSResult{CaseID: config.CaseID, Text: config.Text, Descriptor: provider.Descriptor()}
	started := time.Now()
	var expectedOffset uint64
	var sampleRate uint32
	finalCount := 0
	err := provider.Stream(ctx, v1.SpeechPlan{CandidateID: config.CaseID, Text: config.Text}, func(chunk v1.SpeechChunk) error {
		if len(chunk.PCM16LE) == 0 || len(chunk.PCM16LE)%2 != 0 || chunk.SampleRateHz == 0 {
			return errors.New("TTS provider emitted invalid PCM16")
		}
		if chunk.SampleOffset != expectedOffset {
			return fmt.Errorf("TTS chunk starts at sample %d; expected %d", chunk.SampleOffset, expectedOffset)
		}
		if sampleRate == 0 {
			sampleRate = chunk.SampleRateHz
			result.FirstAudioMS = milliseconds(time.Since(started))
		} else if chunk.SampleRateHz != sampleRate {
			return fmt.Errorf("TTS sample rate changed from %d to %d", sampleRate, chunk.SampleRateHz)
		}
		if chunk.Final {
			finalCount++
		}
		result.Chunks = append(result.Chunks, TTSChunkRecord{
			Sequence: len(result.Chunks) + 1, ArrivalMS: milliseconds(time.Since(started)),
			AudioBytes: len(chunk.PCM16LE), SampleOffset: chunk.SampleOffset,
			SampleRateHz: chunk.SampleRateHz, Final: chunk.Final,
		})
		result.OutputPCM16 = append(result.OutputPCM16, chunk.PCM16LE...)
		expectedOffset += uint64(len(chunk.PCM16LE) / 2)
		return nil
	})
	if err != nil {
		return TTSResult{}, err
	}
	if len(result.Chunks) == 0 || finalCount != 1 || !result.Chunks[len(result.Chunks)-1].Final {
		return TTSResult{}, fmt.Errorf("TTS stream requires one terminal final chunk; chunks=%d final=%d", len(result.Chunks), finalCount)
	}
	result.ElapsedMS = milliseconds(time.Since(started))
	result.OutputBytes = len(result.OutputPCM16)
	result.OutputSampleRate = int(sampleRate)
	result.OutputDurationMS = float64(len(result.OutputPCM16)/2) * 1_000 / float64(sampleRate)
	if result.OutputDurationMS > 0 {
		result.RealTimeFactor = result.ElapsedMS / result.OutputDurationMS
	}
	digest := sha256.Sum256(result.OutputPCM16)
	result.OutputSHA256 = hex.EncodeToString(digest[:])
	return result, nil
}

// WordErrorRate computes word-level Levenshtein distance after Unicode-aware
// case folding and punctuation removal. An empty reference scores 0 only when
// the hypothesis is also empty.
func WordErrorRate(reference, hypothesis string) float64 {
	referenceWords := normalizedWords(reference)
	hypothesisWords := normalizedWords(hypothesis)
	if len(referenceWords) == 0 {
		if len(hypothesisWords) == 0 {
			return 0
		}
		return 1
	}
	previous := make([]int, len(hypothesisWords)+1)
	for index := range previous {
		previous[index] = index
	}
	for referenceIndex, referenceWord := range referenceWords {
		current := make([]int, len(hypothesisWords)+1)
		current[0] = referenceIndex + 1
		for hypothesisIndex, hypothesisWord := range hypothesisWords {
			cost := 0
			if referenceWord != hypothesisWord {
				cost = 1
			}
			current[hypothesisIndex+1] = min(
				current[hypothesisIndex]+1,
				previous[hypothesisIndex+1]+1,
				previous[hypothesisIndex]+cost,
			)
		}
		previous = current
	}
	return float64(previous[len(hypothesisWords)]) / float64(len(referenceWords))
}

func normalizedWords(input string) []string {
	var builder strings.Builder
	for _, character := range strings.ToLower(input) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			builder.WriteRune(character)
		} else {
			builder.WriteByte(' ')
		}
	}
	return strings.Fields(builder.String())
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	delay := time.Until(deadline)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func nearestRank(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	rank := int(math.Ceil(quantile*float64(len(ordered)))) - 1
	rank = max(0, min(rank, len(ordered)-1))
	return ordered[rank]
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
