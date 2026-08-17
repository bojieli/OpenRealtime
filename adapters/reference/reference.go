// Package reference provides deterministic, key-free adapters for instrumentation baselines.
package reference

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

type Cue struct {
	EndSample    uint64 `json:"end_sample"`
	StableText   string `json:"stable_text"`
	UnstableText string `json:"unstable_text"`
	Delta        string `json:"delta"`
}

type Manifest struct {
	SchemaVersion   string `json:"schema_version"`
	AnnotationMode  string `json:"annotation_mode"`
	FixtureSHA256   string `json:"fixture_sha256"`
	ItemID          string `json:"item_id"`
	FinalTranscript string `json:"final_transcript"`
	ResponseText    string `json:"response_text"`
	Cues            []Cue  `json:"cues"`
}

func LoadManifest(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read reference manifest: %w", err)
	}
	defer file.Close()
	const maximumManifestBytes = int64(1 << 20)
	metadata, err := file.Stat()
	if err != nil {
		return Manifest{}, fmt.Errorf("stat reference manifest: %w", err)
	}
	if metadata.Size() > maximumManifestBytes {
		return Manifest{}, errors.New("reference manifest exceeds 1 MiB")
	}
	var manifest Manifest
	decoder := json.NewDecoder(io.LimitReader(file, maximumManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode reference manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("reference manifest must contain exactly one JSON value")
		}
		return Manifest{}, fmt.Errorf("decode trailing reference manifest data: %w", err)
	}
	if manifest.SchemaVersion != "0.1.0" {
		return Manifest{}, fmt.Errorf("unsupported reference manifest schema %q", manifest.SchemaVersion)
	}
	if manifest.AnnotationMode != "symbolic_non_transcription" {
		return Manifest{}, fmt.Errorf("unsupported reference annotation mode %q", manifest.AnnotationMode)
	}
	if manifest.FixtureSHA256 == "" || manifest.ItemID == "" ||
		manifest.FinalTranscript == "" || manifest.ResponseText == "" {
		return Manifest{}, errors.New("reference manifest identities and texts must not be empty")
	}
	if len(manifest.Cues) == 0 {
		return Manifest{}, errors.New("reference manifest must contain at least one streaming cue")
	}
	digest, err := hex.DecodeString(manifest.FixtureSHA256)
	if err != nil || len(digest) != 32 {
		return Manifest{}, errors.New("reference fixture_sha256 must be a 64-character hexadecimal digest")
	}
	for index, cue := range manifest.Cues {
		if cue.EndSample == 0 || (cue.StableText == "" && cue.UnstableText == "" && cue.Delta == "") {
			return Manifest{}, fmt.Errorf("cue %d is empty", index)
		}
		if index > 0 && cue.EndSample <= manifest.Cues[index-1].EndSample {
			return Manifest{}, errors.New("reference cues must be strictly ordered by end_sample")
		}
	}
	return manifest, nil
}

type Perception struct {
	manifest   Manifest
	nextCue    int
	revision   uint64
	nextFrame  uint64
	nextSample uint64
	finalized  bool
}

func NewPerception(manifest Manifest) *Perception {
	return &Perception{manifest: manifest}
}

func (provider *Perception) Name() string { return "reference.manifest_perception" }

func (provider *Perception) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		engine.CapabilityStreamingInput: true,
		engine.CapabilityRevisions:      true,
		engine.CapabilityCancellation:   true,
		engine.CapabilityDeterministic:  true,
	}
}

func (provider *Perception) PushFrame(
	ctx context.Context,
	frame engine.AudioFrame,
) ([]engine.PerceptionRevision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if provider.finalized {
		return nil, errors.New("cannot push reference perception frames after finalization")
	}
	if frame.Index != provider.nextFrame || frame.SampleOffset != provider.nextSample {
		return nil, errors.New("reference perception frames must be contiguous and ordered")
	}
	if frame.SampleRateHz != audio.OpenAIPCMSampleRate || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return nil, errors.New("reference perception requires non-empty 24 kHz PCM16 frames")
	}
	provider.nextFrame++
	provider.nextSample = frame.EndSample()
	var revisions []engine.PerceptionRevision
	for provider.nextCue < len(provider.manifest.Cues) &&
		provider.manifest.Cues[provider.nextCue].EndSample <= frame.EndSample() {
		cue := provider.manifest.Cues[provider.nextCue]
		provider.revision++
		revisions = append(revisions, engine.PerceptionRevision{
			RevisionID:   provider.revision,
			SourceSample: cue.EndSample,
			StableText:   cue.StableText,
			UnstableText: cue.UnstableText,
			Delta:        cue.Delta,
		})
		provider.nextCue++
	}
	return revisions, nil
}

func (provider *Perception) Finalize(
	ctx context.Context,
	endSample uint64,
) (engine.PerceptionRevision, error) {
	if err := ctx.Err(); err != nil {
		return engine.PerceptionRevision{}, err
	}
	if provider.finalized {
		return engine.PerceptionRevision{}, errors.New("reference perception is already finalized")
	}
	if endSample != provider.nextSample || provider.nextCue != len(provider.manifest.Cues) {
		return engine.PerceptionRevision{}, errors.New("cannot finalize before all ordered audio frames and manifest cues")
	}
	provider.revision++
	provider.finalized = true
	return engine.PerceptionRevision{
		RevisionID:   provider.revision,
		SourceSample: endSample,
		StableText:   provider.manifest.FinalTranscript,
		Delta:        provider.manifest.FinalTranscript,
		Final:        true,
	}, nil
}

type Cognition struct {
	responseText string
}

func NewCognition(responseText string) *Cognition {
	return &Cognition{responseText: responseText}
}

func (provider *Cognition) Name() string { return "reference.fixed_cognition" }

func (provider *Cognition) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		engine.CapabilityCancellation:  true,
		engine.CapabilityDeterministic: true,
	}
}

func (provider *Cognition) Respond(
	ctx context.Context,
	revision engine.PerceptionRevision,
) (engine.ResponseCandidate, error) {
	if err := ctx.Err(); err != nil {
		return engine.ResponseCandidate{}, err
	}
	if revision.RevisionID == 0 || strings.TrimSpace(revision.StableText) == "" {
		return engine.ResponseCandidate{}, errors.New("reference cognition requires a stable perception revision")
	}
	if strings.TrimSpace(provider.responseText) == "" {
		return engine.ResponseCandidate{}, errors.New("reference response must not be empty")
	}
	return engine.ResponseCandidate{
		CandidateID:     fmt.Sprintf("candidate_%04d", revision.RevisionID),
		SourceRevision:  revision.RevisionID,
		Text:            provider.responseText,
		Semantic:        true,
		ValiditySummary: fmt.Sprintf("valid while stable prefix from revision %d remains applicable", revision.RevisionID),
	}, nil
}

type Speech struct {
	durationMS uint32
}

func NewSpeech(durationMS uint32) *Speech {
	return &Speech{durationMS: durationMS}
}

func (provider *Speech) Name() string { return "reference.signal_speech" }

func (provider *Speech) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		engine.CapabilityCancellation:    true,
		engine.CapabilityDeterministic:   true,
		engine.CapabilityPCM16Output:     true,
		engine.CapabilityStreamingOutput: true,
	}
}

func (provider *Speech) Stream(
	ctx context.Context,
	plan engine.SpeechPlan,
	consume func(engine.SpeechChunk) error,
) error {
	if consume == nil {
		return errors.New("streaming speech requires a chunk consumer")
	}
	if err := validateSpeechPlan(plan, provider.durationMS); err != nil {
		return err
	}
	const chunkMS = uint32(20)
	totalSamples := uint64(audio.OpenAIPCMSampleRate) * uint64(provider.durationMS) / 1_000
	chunkSamples := uint64(audio.OpenAIPCMSampleRate) * uint64(chunkMS) / 1_000
	for offset, index := uint64(0), uint64(0); offset < totalSamples; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+chunkSamples, totalSamples)
		pcm := referenceSignal(offset, end)
		if err := consume(engine.SpeechChunk{
			ChunkID: fmt.Sprintf("speech_%04d", index+1), CandidateID: plan.CandidateID,
			SampleOffset: offset, SampleRateHz: audio.OpenAIPCMSampleRate, PCM16LE: pcm,
			Final: end == totalSamples,
		}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func (provider *Speech) Synthesize(
	ctx context.Context,
	plan engine.SpeechPlan,
) ([]engine.SpeechChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateSpeechPlan(plan, provider.durationMS); err != nil {
		return nil, err
	}
	samples := uint64(audio.OpenAIPCMSampleRate) * uint64(provider.durationMS) / 1_000
	if samples > uint64(int(^uint(0)>>1))/2 {
		return nil, errors.New("reference speech duration is too large")
	}
	pcm := referenceSignal(0, samples)
	return []engine.SpeechChunk{{
		ChunkID:      "speech_0001",
		CandidateID:  plan.CandidateID,
		SampleRateHz: audio.OpenAIPCMSampleRate,
		PCM16LE:      pcm,
		Final:        true,
	}}, nil
}

func validateSpeechPlan(plan engine.SpeechPlan, durationMS uint32) error {
	if plan.CandidateID == "" || strings.TrimSpace(plan.Text) == "" {
		return errors.New("speech plan must identify a non-empty candidate")
	}
	if durationMS == 0 {
		return errors.New("reference speech duration must be positive")
	}
	if durationMS > 600_000 {
		return errors.New("reference speech duration must not exceed ten minutes")
	}
	return nil
}

func referenceSignal(startSample, endSample uint64) []byte {
	pcm := make([]byte, int(endSample-startSample)*2)
	for sample := startSample; sample < endSample; sample++ {
		value := int16(5_000)
		if (sample/24)%2 == 1 {
			value = -5_000
		}
		offset := (sample - startSample) * 2
		binary.LittleEndian.PutUint16(pcm[offset:offset+2], uint16(value))
	}
	return pcm
}
