package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	stable "github.com/bojieli/OpenRealtime/api/v1"
)

type ProviderProbe struct {
	Frames     []stable.AudioFrame
	EndSample  uint64
	Goal       stable.GoalSnapshot
	SpeechText string
}

type ProviderResult struct {
	Role             string            `json:"role"`
	Descriptor       stable.Descriptor `json:"descriptor"`
	DescriptorChecks uint64            `json:"descriptor_checks"`
}

type ProviderReport struct {
	Suite               string           `json:"suite"`
	APIVersion          string           `json:"api_version"`
	Passed              bool             `json:"passed"`
	Providers           []ProviderResult `json:"providers"`
	Frames              uint64           `json:"frames"`
	Revisions           uint64           `json:"revisions"`
	SynthesizedChunks   uint64           `json:"synthesized_chunks"`
	StreamedChunks      uint64           `json:"streamed_chunks"`
	DeliberationUpdates uint64           `json:"deliberation_updates"`
	CancellationChecks  uint64           `json:"cancellation_checks"`
	BehaviorChecks      uint64           `json:"behavior_checks"`
}

func RunProviders(ctx context.Context, providers stable.ProviderSet, probe ProviderProbe) (ProviderReport, error) {
	if ctx == nil {
		return ProviderReport{}, errors.New("provider suite requires a context")
	}
	if len(probe.Frames) == 0 || probe.EndSample == 0 || strings.TrimSpace(probe.SpeechText) == "" {
		return ProviderReport{}, errors.New("provider suite requires frames, endpoint, and speech text")
	}
	if err := probe.Goal.Validate(); err != nil {
		return ProviderReport{}, err
	}
	if isNil(providers.Perception) || isNil(providers.Cognition) || isNil(providers.Speech) || isNil(providers.Fast) || isNil(providers.Deliberation) {
		return ProviderReport{}, errors.New("provider suite requires all five stable provider roles")
	}
	report := ProviderReport{Suite: "openrealtime_api_v1_providers", APIVersion: stable.Version}
	roles := []struct {
		name       string
		descriptor stable.Descriptor
		required   []stable.Capability
	}{
		{"perception", providers.Perception.Descriptor(), []stable.Capability{stable.CapabilityCancellation, stable.CapabilityStreamingInput, stable.CapabilityRevisions}},
		{"cognition", providers.Cognition.Descriptor(), []stable.Capability{stable.CapabilityCancellation}},
		{"speech", providers.Speech.Descriptor(), []stable.Capability{stable.CapabilityCancellation, stable.CapabilityPCM16Output}},
		{"fast", providers.Fast.Descriptor(), []stable.Capability{stable.CapabilityCancellation}},
		{"deliberation", providers.Deliberation.Descriptor(), []stable.Capability{stable.CapabilityCancellation}},
	}
	for _, role := range roles {
		if err := role.descriptor.Validate(); err != nil {
			return ProviderReport{}, fmt.Errorf("%s descriptor: %w", role.name, err)
		}
		checks := uint64(1)
		for _, capability := range role.required {
			if !role.descriptor.Capabilities.Has(capability) {
				return ProviderReport{}, fmt.Errorf("%s provider lacks %s", role.name, capability)
			}
			checks++
		}
		report.Providers = append(report.Providers, ProviderResult{Role: role.name, Descriptor: role.descriptor, DescriptorChecks: checks})
	}
	firstFrame := probe.Frames[0]
	if err := expectCancelled(func(cancelled context.Context) error {
		_, err := providers.Perception.PushFrame(cancelled, firstFrame)
		return err
	}); err != nil {
		return ProviderReport{}, fmt.Errorf("perception cancellation: %w", err)
	}
	report.CancellationChecks++
	var revisions []stable.PerceptionRevision
	var previousEnd uint64
	var lastStable string
	var lastRevisionID uint64
	for index, frame := range probe.Frames {
		end, err := frame.EndSample()
		if err != nil {
			return ProviderReport{}, fmt.Errorf("probe frame %d: %w", index, err)
		}
		if frame.Index != uint64(index) || frame.SampleOffset != previousEnd {
			return ProviderReport{}, errors.New("probe frames are not contiguous and indexed")
		}
		produced, err := providers.Perception.PushFrame(ctx, cloneFrame(frame))
		if err != nil {
			return ProviderReport{}, fmt.Errorf("perception frame %d: %w", index, err)
		}
		for _, revision := range produced {
			if revision.RevisionID <= lastRevisionID || revision.SourceSample > end || revision.Final {
				return ProviderReport{}, errors.New("streaming perception revision violates order or source boundary")
			}
			if !strings.HasPrefix(revision.StableText, lastStable) {
				return ProviderReport{}, errors.New("stable perception prefix regressed")
			}
			lastStable, lastRevisionID = revision.StableText, revision.RevisionID
			revisions = append(revisions, revision)
		}
		previousEnd = end
	}
	if previousEnd != probe.EndSample {
		return ProviderReport{}, errors.New("probe endpoint does not match frames")
	}
	finalRevision, err := providers.Perception.Finalize(ctx, probe.EndSample)
	if err != nil {
		return ProviderReport{}, err
	}
	if !finalRevision.Final || finalRevision.RevisionID <= lastRevisionID || !strings.HasPrefix(finalRevision.StableText, lastStable) {
		return ProviderReport{}, errors.New("final perception revision is inconsistent")
	}
	revisions = append(revisions, finalRevision)
	report.Frames = uint64(len(probe.Frames))
	report.Revisions = uint64(len(revisions))
	report.BehaviorChecks += 3

	if err := expectCancelled(func(cancelled context.Context) error {
		_, err := providers.Cognition.Respond(cancelled, finalRevision)
		return err
	}); err != nil {
		return ProviderReport{}, fmt.Errorf("cognition cancellation: %w", err)
	}
	report.CancellationChecks++
	candidate, err := providers.Cognition.Respond(ctx, finalRevision)
	if err != nil {
		return ProviderReport{}, err
	}
	if candidate.CandidateID == "" || candidate.SourceRevision != finalRevision.RevisionID || strings.TrimSpace(candidate.Text) == "" || candidate.ValiditySummary == "" {
		return ProviderReport{}, errors.New("cognition candidate lacks identity, source, text, or validity")
	}
	report.BehaviorChecks++
	plan := stable.SpeechPlan{CandidateID: candidate.CandidateID, Text: probe.SpeechText}
	if err := expectCancelled(func(cancelled context.Context) error {
		_, err := providers.Speech.Synthesize(cancelled, plan)
		return err
	}); err != nil {
		return ProviderReport{}, fmt.Errorf("speech cancellation: %w", err)
	}
	report.CancellationChecks++
	chunks, err := providers.Speech.Synthesize(ctx, plan)
	if err != nil {
		return ProviderReport{}, err
	}
	if err := validateChunks(chunks, plan.CandidateID); err != nil {
		return ProviderReport{}, err
	}
	report.SynthesizedChunks = uint64(len(chunks))
	report.BehaviorChecks++
	if providers.Speech.Descriptor().Capabilities.Has(stable.CapabilityStreamingOutput) {
		streaming, ok := providers.Speech.(stable.StreamingSpeechProvider)
		if !ok {
			return ProviderReport{}, errors.New("speech provider advertises streaming without implementing it")
		}
		var cancelledChunks uint64
		cancellationErr := expectCancelled(func(cancelled context.Context) error {
			return streaming.Stream(cancelled, plan, func(stable.SpeechChunk) error {
				cancelledChunks++
				return nil
			})
		})
		if cancellationErr != nil {
			return ProviderReport{}, fmt.Errorf("streaming speech cancellation: %w", cancellationErr)
		}
		if cancelledChunks != 0 {
			return ProviderReport{}, fmt.Errorf("streaming speech cancellation emitted %d chunks", cancelledChunks)
		}
		report.CancellationChecks++
		var streamed []stable.SpeechChunk
		if err := streaming.Stream(ctx, plan, func(chunk stable.SpeechChunk) error {
			streamed = append(streamed, cloneChunk(chunk))
			return nil
		}); err != nil {
			return ProviderReport{}, err
		}
		if err := validateChunks(streamed, plan.CandidateID); err != nil {
			return ProviderReport{}, fmt.Errorf("streaming speech: %w", err)
		}
		report.StreamedChunks = uint64(len(streamed))
		report.BehaviorChecks++
	}
	if err := expectCancelled(func(cancelled context.Context) error {
		_, err := providers.Fast.Decide(cancelled, probe.Goal)
		return err
	}); err != nil {
		return ProviderReport{}, fmt.Errorf("fast cancellation: %w", err)
	}
	report.CancellationChecks++
	decision, err := providers.Fast.Decide(ctx, probe.Goal)
	if err != nil {
		return ProviderReport{}, err
	}
	if err := decision.ValidateFor(probe.Goal); err != nil {
		return ProviderReport{}, err
	}
	report.BehaviorChecks++
	var cancelledUpdates uint64
	cancellationErr := expectCancelled(func(cancelled context.Context) error {
		return providers.Deliberation.Deliberate(cancelled, probe.Goal, func(stable.DeliberationUpdate) error {
			cancelledUpdates++
			return nil
		})
	})
	if cancellationErr != nil {
		return ProviderReport{}, fmt.Errorf("deliberation cancellation: %w", cancellationErr)
	}
	if cancelledUpdates != 0 {
		return ProviderReport{}, fmt.Errorf("deliberation cancellation emitted %d updates", cancelledUpdates)
	}
	report.CancellationChecks++
	var updates []stable.DeliberationUpdate
	if err := providers.Deliberation.Deliberate(ctx, probe.Goal, func(update stable.DeliberationUpdate) error {
		updates = append(updates, update)
		return nil
	}); err != nil {
		return ProviderReport{}, err
	}
	if len(updates) == 0 {
		return ProviderReport{}, errors.New("deliberation emitted no updates")
	}
	for index, update := range updates {
		if update.Sequence != uint64(index) {
			return ProviderReport{}, errors.New("deliberation sequence is not contiguous")
		}
		if err := update.ValidateFor(probe.Goal); err != nil {
			return ProviderReport{}, err
		}
		terminal := update.Status == stable.DeliberationFinal || update.Status == stable.DeliberationFailed
		if terminal != (index == len(updates)-1) {
			return ProviderReport{}, errors.New("deliberation terminal update is not exactly last")
		}
	}
	report.DeliberationUpdates = uint64(len(updates))
	report.BehaviorChecks++
	report.Passed = true
	return report, nil
}

func validateChunks(chunks []stable.SpeechChunk, candidateID string) error {
	if len(chunks) == 0 {
		return errors.New("speech emitted no chunks")
	}
	var end uint64
	ids := make(map[string]struct{}, len(chunks))
	for index, chunk := range chunks {
		if err := chunk.Validate(); err != nil {
			return err
		}
		if chunk.CandidateID != candidateID || chunk.SampleOffset != end || chunk.Final != (index == len(chunks)-1) {
			return errors.New("speech chunks violate candidate, continuity, or finality")
		}
		if _, exists := ids[chunk.ChunkID]; exists {
			return errors.New("speech chunk IDs are not unique")
		}
		ids[chunk.ChunkID] = struct{}{}
		var err error
		end, err = chunk.EndSample()
		if err != nil {
			return err
		}
	}
	return nil
}

func expectCancelled(call func(context.Context) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := call(ctx)
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("got %v, want context canceled", err)
	}
	return nil
}

func cloneFrame(frame stable.AudioFrame) stable.AudioFrame {
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	return frame
}

func cloneChunk(chunk stable.SpeechChunk) stable.SpeechChunk {
	chunk.PCM16LE = slices.Clone(chunk.PCM16LE)
	return chunk
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
