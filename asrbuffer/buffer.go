// Package asrbuffer decouples the realtime scheduler cadence from an ASR
// provider's minimum useful audio chunk size.
//
// A Buffer accepts small, contiguous PCM16 frames at the interaction cadence
// (for example, 50 or 80 milliseconds), combines them into exact provider
// chunks (for example, 200 milliseconds), and implements the unchanged
// api/v1.PerceptionProvider interface. The wrapper is internal middleware: it
// neither adds an OpenAI Realtime event nor changes the wrapped descriptor.
package asrbuffer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

const (
	defaultMaxInputFrameBytes = 4 << 20
	maxChunkDuration          = time.Hour
	maxSampleRateHz           = 384_000
)

// Config binds a provider to a minimum chunk duration. MaximumChunk equal to
// zero selects fixed cadence. A larger value enables revision-adaptive
// backoff: changed ASR output resets to MinimumChunk, while an unchanged
// provider advance doubles the next chunk up to MaximumChunk. The policy uses
// only the presence of a typed revision, never transcript content.
// MaxInputFrameBytes is an admission bound; zero selects 4 MiB.
type Config struct {
	Provider           v1.PerceptionProvider
	MinimumChunk       time.Duration
	MaximumChunk       time.Duration
	MaxInputFrameBytes int
}

// Stats is a point-in-time, secret-free snapshot. ProviderChunks counts
// successful calls to the wrapped provider's PushFrame; ProviderAttempts and
// timing include failures. Finalization is measured separately.
type Stats struct {
	InputFrames          uint64 `json:"input_frames"`
	InputSamples         uint64 `json:"input_samples"`
	ProviderAttempts     uint64 `json:"provider_attempts"`
	ProviderChunks       uint64 `json:"provider_chunks"`
	ProviderFailures     uint64 `json:"provider_failures"`
	ProviderSamples      uint64 `json:"provider_samples"`
	ProviderElapsedNS    uint64 `json:"provider_elapsed_ns"`
	ProviderMaxElapsedNS uint64 `json:"provider_max_elapsed_ns"`
	FinalizeAttempts     uint64 `json:"finalize_attempts"`
	FinalizeFailures     uint64 `json:"finalize_failures"`
	FinalizeElapsedNS    uint64 `json:"finalize_elapsed_ns"`
	FinalizeMaxElapsedNS uint64 `json:"finalize_max_elapsed_ns"`
	PendingSamples       uint64 `json:"pending_samples"`
	MinimumChunkSamples  uint64 `json:"minimum_chunk_samples,omitempty"`
	MaximumChunkSamples  uint64 `json:"maximum_chunk_samples,omitempty"`
	CurrentChunkSamples  uint64 `json:"current_chunk_samples,omitempty"`
	Policy               string `json:"policy"`
	SampleRateHz         uint32 `json:"sample_rate_hz,omitempty"`
	Finalized            bool   `json:"finalized"`
}

// ProviderMetrics is the monotonic subset used by process-level benchmark
// telemetry. It contains no transcript or audio content.
type ProviderMetrics struct {
	AdvanceInvocations   uint64
	AdvanceFailures      uint64
	AdvanceElapsedNS     uint64
	AdvanceMaxElapsedNS  uint64
	FinalizeInvocations  uint64
	FinalizeFailures     uint64
	FinalizeElapsedNS    uint64
	FinalizeMaxElapsedNS uint64
}

// Buffer owns one utterance. Calls are serialized because the wrapped
// provider is stateful and a failed remote update makes retry ambiguous.
type Buffer struct {
	provider     v1.PerceptionProvider
	minimumChunk time.Duration
	maximumChunk time.Duration
	maxFrame     int

	mu sync.Mutex

	pending             []byte
	pendingStartSample  uint64
	sampleRateHz        uint32
	minimumChunkSamples uint64
	maximumChunkSamples uint64
	currentChunkSamples uint64
	nextInputIndex      uint64
	nextInputSample     uint64
	nextProviderIndex   uint64
	haveInput           bool
	finalized           bool
	terminalErr         error
	stats               Stats
}

// New validates middleware configuration and returns a fresh utterance
// buffer. MinimumChunk is explicit so a provider upgrade cannot silently
// change the scheduling experiment.
func New(config Config) (*Buffer, error) {
	if config.Provider == nil {
		return nil, errors.New("ASR cadence buffer requires a provider")
	}
	if config.MinimumChunk <= 0 || config.MinimumChunk > maxChunkDuration {
		return nil, fmt.Errorf("ASR minimum chunk must be in (0, %s]", maxChunkDuration)
	}
	if config.MaximumChunk < 0 || config.MaximumChunk > maxChunkDuration {
		return nil, fmt.Errorf("ASR maximum chunk must be zero or in (0, %s]", maxChunkDuration)
	}
	if config.MaximumChunk == 0 {
		config.MaximumChunk = config.MinimumChunk
	}
	if config.MaximumChunk < config.MinimumChunk {
		return nil, errors.New("ASR maximum chunk cannot be smaller than its minimum")
	}
	if config.MaxInputFrameBytes < 0 {
		return nil, errors.New("ASR maximum input frame size cannot be negative")
	}
	if config.MaxInputFrameBytes == 0 {
		config.MaxInputFrameBytes = defaultMaxInputFrameBytes
	}
	return &Buffer{
		provider: config.Provider, minimumChunk: config.MinimumChunk,
		maximumChunk: config.MaximumChunk,
		maxFrame:     config.MaxInputFrameBytes,
	}, nil
}

// Descriptor implements api/v1.PerceptionProvider. Buffering is deployment
// policy, not provider identity, so the wrapped descriptor is returned with a
// defensive copy of its capability map.
func (buffer *Buffer) Descriptor() v1.Descriptor {
	descriptor := buffer.provider.Descriptor()
	if descriptor.Capabilities != nil {
		capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
		for name, enabled := range descriptor.Capabilities {
			capabilities[name] = enabled
		}
		descriptor.Capabilities = capabilities
	}
	return descriptor
}

// ProviderInvocationCount returns the number of completed PushFrame attempts
// at the wrapped provider boundary. Benchmark drivers use before/after
// snapshots to distinguish cheap scheduler ticks from actual decode
// opportunities, including a terminal failed attempt.
func (buffer *Buffer) ProviderInvocationCount() uint64 {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.stats.ProviderAttempts
}

// ProviderRuntimeMetrics returns monotonic provider-boundary counters and
// timing for process-level aggregation.
func (buffer *Buffer) ProviderRuntimeMetrics() ProviderMetrics {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return ProviderMetrics{
		AdvanceInvocations:   buffer.stats.ProviderAttempts,
		AdvanceFailures:      buffer.stats.ProviderFailures,
		AdvanceElapsedNS:     buffer.stats.ProviderElapsedNS,
		AdvanceMaxElapsedNS:  buffer.stats.ProviderMaxElapsedNS,
		FinalizeInvocations:  buffer.stats.FinalizeAttempts,
		FinalizeFailures:     buffer.stats.FinalizeFailures,
		FinalizeElapsedNS:    buffer.stats.FinalizeElapsedNS,
		FinalizeMaxElapsedNS: buffer.stats.FinalizeMaxElapsedNS,
	}
}

// Stats returns an immutable snapshot.
func (buffer *Buffer) Stats() Stats {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	result := buffer.stats
	result.PendingSamples = uint64(len(buffer.pending) / 2)
	result.Finalized = buffer.finalized
	return result
}

// PushFrame accepts one scheduler frame and may make zero or more provider
// calls. Each provider frame reaches the current fixed or adaptive threshold,
// except the terminal remainder flushed by Finalize.
func (buffer *Buffer) PushFrame(ctx context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	if err := buffer.ready(); err != nil {
		return nil, err
	}
	if err := buffer.validateFrame(frame); err != nil {
		return nil, err
	}
	if !buffer.haveInput {
		minimumSamples, err := durationSamples(buffer.minimumChunk, frame.SampleRateHz)
		if err != nil {
			return nil, err
		}
		maximumSamples, err := durationSamples(buffer.maximumChunk, frame.SampleRateHz)
		if err != nil {
			return nil, err
		}
		buffer.sampleRateHz = frame.SampleRateHz
		buffer.minimumChunkSamples = minimumSamples
		buffer.maximumChunkSamples = maximumSamples
		buffer.currentChunkSamples = minimumSamples
		buffer.stats.SampleRateHz = frame.SampleRateHz
		buffer.stats.MinimumChunkSamples = minimumSamples
		buffer.stats.MaximumChunkSamples = maximumSamples
		buffer.stats.CurrentChunkSamples = minimumSamples
		buffer.stats.Policy = "fixed"
		if maximumSamples > minimumSamples {
			buffer.stats.Policy = "revision-adaptive"
		}
	}

	buffer.haveInput = true
	buffer.nextInputIndex = frame.Index + 1
	frameSamples := uint64(len(frame.PCM16LE) / 2)
	if frameSamples > math.MaxUint64-frame.SampleOffset {
		return nil, errors.New("ASR input frame end sample overflows")
	}
	buffer.nextInputSample = frame.SampleOffset + frameSamples
	buffer.stats.InputFrames++
	buffer.stats.InputSamples += frameSamples

	var revisions []v1.PerceptionRevision
	consumedSamples := uint64(0)
	for consumedSamples < frameSamples {
		if len(buffer.pending) == 0 {
			buffer.pendingStartSample = frame.SampleOffset + consumedSamples
		}
		pendingSamples := uint64(len(buffer.pending) / 2)
		needed := buffer.currentChunkSamples - pendingSamples
		available := frameSamples - consumedSamples
		take := min(needed, available)
		byteStart := int(consumedSamples * 2)
		byteEnd := int((consumedSamples + take) * 2)
		buffer.pending = append(buffer.pending, frame.PCM16LE[byteStart:byteEnd]...)
		consumedSamples += take
		if uint64(len(buffer.pending)/2) != buffer.currentChunkSamples {
			continue
		}
		produced, err := buffer.flush(ctx)
		if err != nil {
			return nil, buffer.fail(err)
		}
		revisions = append(revisions, produced...)
	}
	return revisions, nil
}

// Finalize flushes a short terminal remainder before finalizing the wrapped
// provider. A partial revision from that flush is superseded immediately by
// the returned final revision and is therefore not separately exposed.
func (buffer *Buffer) Finalize(ctx context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	if err := buffer.ready(); err != nil {
		return v1.PerceptionRevision{}, err
	}
	if !buffer.haveInput {
		return v1.PerceptionRevision{}, errors.New("ASR cadence buffer cannot finalize an empty utterance")
	}
	if sourceSample != buffer.nextInputSample {
		return v1.PerceptionRevision{}, fmt.Errorf("ASR final source sample is %d; expected %d", sourceSample, buffer.nextInputSample)
	}
	if len(buffer.pending) > 0 {
		if _, err := buffer.flush(ctx); err != nil {
			return v1.PerceptionRevision{}, buffer.fail(err)
		}
	}
	started := time.Now()
	result, err := buffer.provider.Finalize(ctx, sourceSample)
	elapsed := uint64(time.Since(started))
	buffer.stats.FinalizeAttempts++
	buffer.stats.FinalizeElapsedNS += elapsed
	buffer.stats.FinalizeMaxElapsedNS = max(buffer.stats.FinalizeMaxElapsedNS, elapsed)
	if err != nil {
		buffer.stats.FinalizeFailures++
		return v1.PerceptionRevision{}, buffer.fail(fmt.Errorf("finalize buffered ASR provider: %w", err))
	}
	buffer.finalized = true
	buffer.stats.Finalized = true
	return result, nil
}

func (buffer *Buffer) flush(ctx context.Context) ([]v1.PerceptionRevision, error) {
	if len(buffer.pending) == 0 {
		return nil, nil
	}
	audio := slices.Clone(buffer.pending)
	frame := v1.AudioFrame{
		Index: buffer.nextProviderIndex, SampleOffset: buffer.pendingStartSample,
		SampleRateHz: buffer.sampleRateHz, PCM16LE: audio,
	}
	started := time.Now()
	revisions, err := buffer.provider.PushFrame(ctx, frame)
	elapsed := uint64(time.Since(started))
	buffer.stats.ProviderAttempts++
	buffer.stats.ProviderElapsedNS += elapsed
	buffer.stats.ProviderMaxElapsedNS = max(buffer.stats.ProviderMaxElapsedNS, elapsed)
	if err != nil {
		buffer.stats.ProviderFailures++
		return nil, fmt.Errorf("push buffered ASR chunk %d: %w", buffer.nextProviderIndex, err)
	}
	chunkSamples := uint64(len(audio) / 2)
	buffer.nextProviderIndex++
	buffer.stats.ProviderChunks++
	buffer.stats.ProviderSamples += chunkSamples
	buffer.pending = buffer.pending[:0]
	buffer.pendingStartSample += chunkSamples
	if len(revisions) > 0 {
		buffer.currentChunkSamples = buffer.minimumChunkSamples
	} else {
		buffer.currentChunkSamples = min(buffer.currentChunkSamples*2, buffer.maximumChunkSamples)
	}
	buffer.stats.CurrentChunkSamples = buffer.currentChunkSamples
	return revisions, nil
}

func (buffer *Buffer) ready() error {
	if buffer.terminalErr != nil {
		return fmt.Errorf("ASR cadence buffer is unusable after an indeterminate provider update: %w", buffer.terminalErr)
	}
	if buffer.finalized {
		return errors.New("ASR cadence buffer is finalized")
	}
	return nil
}

func (buffer *Buffer) fail(err error) error {
	buffer.terminalErr = err
	return err
}

func (buffer *Buffer) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || frame.SampleRateHz > maxSampleRateHz {
		return fmt.Errorf("ASR input sample rate must be in [1, %d] Hz", maxSampleRateHz)
	}
	if len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("ASR input frame requires non-empty even-length PCM16")
	}
	if len(frame.PCM16LE) > buffer.maxFrame {
		return fmt.Errorf("ASR input frame has %d bytes; maximum is %d", len(frame.PCM16LE), buffer.maxFrame)
	}
	if buffer.haveInput {
		if frame.Index != buffer.nextInputIndex {
			return fmt.Errorf("ASR input frame index is %d; expected %d", frame.Index, buffer.nextInputIndex)
		}
		if frame.SampleOffset != buffer.nextInputSample {
			return fmt.Errorf("ASR input frame starts at sample %d; expected %d", frame.SampleOffset, buffer.nextInputSample)
		}
		if frame.SampleRateHz != buffer.sampleRateHz {
			return fmt.Errorf("ASR input sample rate changed from %d to %d", buffer.sampleRateHz, frame.SampleRateHz)
		}
	}
	return nil
}

func durationSamples(duration time.Duration, rate uint32) (uint64, error) {
	if rate == 0 || rate > maxSampleRateHz {
		return 0, fmt.Errorf("ASR input sample rate must be in [1, %d] Hz", maxSampleRateHz)
	}
	// Config validation bounds duration, so the product cannot overflow int64.
	product := int64(duration) * int64(rate)
	samples := (product + int64(time.Second) - 1) / int64(time.Second)
	if samples <= 0 {
		return 0, errors.New("ASR minimum chunk is shorter than one source sample")
	}
	return uint64(samples), nil
}
