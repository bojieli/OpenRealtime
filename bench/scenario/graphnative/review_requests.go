package graphnative

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	SourceReviewContextFormat        = "openrealtime.scenario-source-review-context"
	SourceReviewContextFormatVersion = 2
	maximumSourceReviewContext       = 4 << 20
)

// SourceReviewArchitecture is the attempt-local projection of the exact final
// architecture result. It deliberately excludes unrelated task records while
// retaining the authored cell, run provenance, deterministic task row, and
// matching live observation.
type SourceReviewArchitecture struct {
	ResultVersion   int                    `json:"result_version"`
	Experiment      string                 `json:"experiment"`
	ManifestID      string                 `json:"manifest_id"`
	FixtureRevision string                 `json:"fixture_revision"`
	Cell            archbench.Cell         `json:"cell"`
	Provenance      bench.Provenance       `json:"provenance"`
	Task            bench.TaskOutcome      `json:"task"`
	Observation     *archbench.Observation `json:"observation,omitempty"`
}

// SourceReviewContext is the public, self-contained context sent to an
// offline secondary reviewer. DeterministicAuthority is a fixed statement of
// precedence: no model assessment can rewrite Result or Attempt.
type SourceReviewContext struct {
	Format                 string                   `json:"format"`
	FormatVersion          int                      `json:"format_version"`
	DeterministicAuthority string                   `json:"deterministic_authority"`
	SourceReceiptSHA256    string                   `json:"source_receipt_sha256"`
	SourceManifestSHA256   string                   `json:"source_manifest_sha256"`
	SourceFileSetSHA256    string                   `json:"source_file_set_sha256"`
	ChecklistFingerprint   string                   `json:"checklist_fingerprint"`
	MediaDurationMS        int64                    `json:"media_duration_ms"`
	Attempt                AttemptRecord            `json:"attempt"`
	Result                 scenario.Result          `json:"deterministic_result"`
	Architecture           SourceReviewArchitecture `json:"architecture"`
}

// SourceReviewPopulation is one receipt-verified source snapshot and the
// exact provider-neutral requests derived from it. Returning both avoids an
// orchestration layer performing a redundant full source verification merely
// to recover checklist and manifest metadata already authenticated here.
type SourceReviewPopulation struct {
	Bundle   SourceBundle
	Requests []review.Request
}

// BuildSourceReviewRequests converts one externally anchored scenario source
// bundle into provider-neutral offline review requests. It performs no model,
// credential, or network work. The source tree is receipt-verified before and
// after request construction; Evaluate will independently reopen and validate
// every referenced media payload immediately before invoking a provider.
func BuildSourceReviewRequests(
	ctx context.Context,
	options SourceBundleOptions,
	expected SourceReceipt,
) ([]review.Request, error) {
	population, err := BuildSourceReviewPopulation(ctx, options, expected)
	if err != nil {
		return nil, err
	}
	return population.Requests, nil
}

// BuildSourceReviewPopulation verifies the externally anchored source before
// and after deriving every request, and returns the verified bundle metadata
// from that same operation. It performs no provider, credential, or network
// work.
func BuildSourceReviewPopulation(
	ctx context.Context,
	options SourceBundleOptions,
	expected SourceReceipt,
) (population SourceReviewPopulation, resultErr error) {
	if ctx == nil {
		return SourceReviewPopulation{}, errors.New("build scenario source reviews: nil context")
	}
	bundle, err := VerifySourceBundle(ctx, options, expected)
	if err != nil {
		return SourceReviewPopulation{}, err
	}
	root, rootInfo, err := openSourceRoot(options.Directory)
	if err != nil {
		return SourceReviewPopulation{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			population = SourceReviewPopulation{}
			resultErr = errors.Join(resultErr, errors.New("close scenario source review root"))
		}
	}()
	if len(bundle.Manifest.Attempts) != bundle.Checklist.Expected ||
		len(bundle.ArchitectureResult.Measurement.Tasks) != bundle.Checklist.Expected {
		return SourceReviewPopulation{}, errors.New("scenario source review population is incomplete")
	}
	observations := make(map[string]archbench.Observation, len(bundle.ArchitectureResult.Observed))
	for _, observation := range bundle.ArchitectureResult.Observed {
		if _, duplicate := observations[observation.TaskID]; duplicate {
			return SourceReviewPopulation{}, errors.New("scenario source architecture observations are duplicated")
		}
		observations[observation.TaskID] = observation
	}
	guard := sourceSensitiveValues(options.SensitiveValues)
	requests := make([]review.Request, 0, bundle.Checklist.Expected)
	for index, attempt := range bundle.Manifest.Attempts {
		if err := ctx.Err(); err != nil {
			return SourceReviewPopulation{}, err
		}
		resultPayload, err := readSourceFile(ctx, root, attempt.Result)
		if err != nil {
			return SourceReviewPopulation{}, err
		}
		deterministic, err := decodeSourceScenarioResult(resultPayload)
		if err != nil {
			return SourceReviewPopulation{}, fmt.Errorf("scenario source review %s: %w", attempt.Record.Key.TaskID, err)
		}
		if digest, err := FingerprintResult(deterministic); err != nil ||
			digest != attempt.Record.Execution.ResultSHA256 {
			return SourceReviewPopulation{}, errors.New("scenario source review result differs from the checklist")
		}
		audioPayload, err := readSourceFile(ctx, root, attempt.Audio)
		if err != nil {
			return SourceReviewPopulation{}, err
		}
		mediaDurationMS, err := sourceReviewStereoWAVDurationMS(audioPayload)
		if err != nil {
			return SourceReviewPopulation{}, fmt.Errorf(
				"scenario source review %s: %w", attempt.Record.Key.TaskID, err,
			)
		}
		architecture := SourceReviewArchitecture{
			ResultVersion:   bundle.ArchitectureResult.Version,
			Experiment:      bundle.ArchitectureResult.Experiment,
			ManifestID:      bundle.ArchitectureResult.ManifestID,
			FixtureRevision: bundle.ArchitectureResult.FixtureRevision,
			Cell:            bundle.ArchitectureResult.Cell,
			Provenance:      bundle.ArchitectureResult.Measurement.Provenance,
			Task:            bundle.ArchitectureResult.Measurement.Tasks[index],
		}
		if observation, found := observations[attempt.Record.Key.TaskID]; found {
			copy := observation
			architecture.Observation = &copy
		}
		contextPayload, err := marshalSourceIndented(SourceReviewContext{
			Format:                 SourceReviewContextFormat,
			FormatVersion:          SourceReviewContextFormatVersion,
			DeterministicAuthority: "the retained graph-native checklist and deterministic scorer result are authoritative",
			SourceReceiptSHA256:    expected.ReceiptSHA256,
			SourceManifestSHA256:   expected.ManifestSHA256,
			SourceFileSetSHA256:    expected.FileSetSHA256,
			ChecklistFingerprint:   bundle.Checklist.Fingerprint,
			MediaDurationMS:        mediaDurationMS,
			Attempt:                attempt.Record.Clone(), Result: deterministic, Architecture: architecture,
		}, maximumSourceReviewContext)
		if err != nil {
			return SourceReviewPopulation{}, err
		}
		if sourceContainsSensitive(contextPayload, guard) {
			return SourceReviewPopulation{}, errors.New("scenario source review context contains a declared sensitive value")
		}
		media := []review.Media{{
			Kind: "audio", Role: "stereo_room_and_agent", Path: attempt.Audio.Path,
			SHA256: attempt.Audio.SHA256, MediaType: "audio/wav",
		}}
		for submittedIndex, submitted := range attempt.Submitted {
			kind, mediaType, err := sourceReviewImageType(submitted.Receipt.MediaType)
			if err != nil {
				return SourceReviewPopulation{}, fmt.Errorf("scenario source review %s input %d: %w",
					attempt.Record.Key.TaskID, submittedIndex+1, err)
			}
			media = append(media, review.Media{
				Kind: kind, Role: fmt.Sprintf("submitted_visual_input_%02d", submittedIndex+1),
				Path: submitted.File.Path, SHA256: submitted.File.SHA256, MediaType: mediaType,
			})
		}
		requests = append(requests, review.Request{
			AttemptID: attempt.Record.Fingerprint,
			Suite:     SuiteName, Case: attempt.Record.Key.CaseName, Trial: attempt.Record.Key.Trial,
			RootDirectory: options.Directory, Context: contextPayload, Media: media,
			SensitiveValues: slices.Clone(options.SensitiveValues),
		})
	}
	if err := verifySourceRootIdentity(options.Directory, root, rootInfo); err != nil {
		return SourceReviewPopulation{}, err
	}
	reopened, err := VerifySourceBundle(ctx, options, expected)
	if err != nil || !reflect.DeepEqual(reopened.Receipt, bundle.Receipt) ||
		reopened.Checklist.Fingerprint != bundle.Checklist.Fingerprint {
		return SourceReviewPopulation{}, errors.New("scenario source bundle changed while building review requests")
	}
	return SourceReviewPopulation{Bundle: reopened, Requests: requests}, nil
}

// sourceReviewStereoWAVDurationMS validates the exact graph-native scenario
// recording shape and returns its inclusive integer timestamp ceiling. A
// fractional final millisecond rounds up so a finding at the last audio frame
// remains representable without admitting timestamps from a later second.
func sourceReviewStereoWAVDurationMS(payload []byte) (int64, error) {
	const (
		headerBytes = uint64(44)
		sampleRate  = uint64(24_000)
		blockAlign  = uint64(4)
		byteRate    = uint64(96_000)
	)
	size := uint64(len(payload))
	if size <= headerBytes ||
		string(payload[:4]) != "RIFF" || string(payload[8:12]) != "WAVE" ||
		string(payload[12:16]) != "fmt " || string(payload[36:40]) != "data" ||
		binary.LittleEndian.Uint32(payload[16:20]) != 16 ||
		binary.LittleEndian.Uint16(payload[20:22]) != 1 ||
		binary.LittleEndian.Uint16(payload[22:24]) != 2 ||
		uint64(binary.LittleEndian.Uint32(payload[24:28])) != sampleRate ||
		uint64(binary.LittleEndian.Uint32(payload[28:32])) != byteRate ||
		uint64(binary.LittleEndian.Uint16(payload[32:34])) != blockAlign ||
		binary.LittleEndian.Uint16(payload[34:36]) != 16 ||
		uint64(binary.LittleEndian.Uint32(payload[4:8])) != size-8 ||
		uint64(binary.LittleEndian.Uint32(payload[40:44])) != size-headerBytes ||
		(size-headerBytes)%blockAlign != 0 {
		return 0, errors.New("scenario source review audio is not exact 24 kHz stereo PCM16 WAV")
	}
	frames := (size - headerBytes) / blockAlign
	durationMS := (frames*1000 + sampleRate - 1) / sampleRate
	if durationMS == 0 {
		return 0, errors.New("scenario source review audio duration is invalid")
	}
	return int64(durationMS), nil
}

func decodeSourceScenarioResult(payload []byte) (scenario.Result, error) {
	if len(payload) == 0 || strictjson.Validate(payload) != nil {
		return scenario.Result{}, errors.New("deterministic scenario result is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result scenario.Result
	if err := decoder.Decode(&result); err != nil {
		return scenario.Result{}, errors.New("decode deterministic scenario result")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return scenario.Result{}, err
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, payload) {
		return scenario.Result{}, errors.New("deterministic scenario result is noncanonical")
	}
	return result, nil
}

func sourceReviewImageType(mediaType string) (string, string, error) {
	switch mediaType {
	case "image/png", "image/jpeg":
		return "image", mediaType, nil
	default:
		return "", "", fmt.Errorf("media type %q is not supported by the offline review API", mediaType)
	}
}
