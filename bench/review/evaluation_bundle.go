package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	EvaluationBundleFormat        = "openrealtime.review-evaluation-bundle"
	EvaluationBundleFormatVersion = 1

	evaluationBundleManifestName = "manifest.json"
	evaluationBundleReviewName   = "REVIEW.md"
	evaluationBundleScope        = "provider-verified in-process exchange and local digest retention"
	evaluationBundleCaveat       = "This bundle does not independently attest that the retained exchange originated at a remote service."
	maximumEvaluationManifest    = 4 << 20
	maximumEvaluationReview      = 2 << 20
	maximumEvaluationFiles       = maximumMediaCount + 10
)

// EvaluationBundleOptions names one final create-only directory and the exact
// in-memory sensitive values against which every retained byte is checked.
// SensitiveValues are never serialized. Reopen callers must provide the same
// values if Record.SensitiveValueCount is non-zero.
type EvaluationBundleOptions struct {
	Directory       string
	SensitiveValues []string
}

// EvaluationBundleFile is one exact non-manifest byte artifact. MediaOrdinal
// is one-based for reviewed media and zero for all other purposes.
type EvaluationBundleFile struct {
	Path         string `json:"path"`
	Purpose      string `json:"purpose"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
	MediaOrdinal int    `json:"media_ordinal,omitempty"`
	MediaType    string `json:"media_type,omitempty"`
}

// EvaluationBundleManifest is the create-only commit marker. Complete is
// always true in an accepted manifest; an interrupted writer leaves no
// manifest rather than publishing a partial evaluation.
type EvaluationBundleManifest struct {
	Format                       string                 `json:"format"`
	FormatVersion                int                    `json:"format_version"`
	Complete                     bool                   `json:"complete"`
	AttemptID                    string                 `json:"attempt_id"`
	Suite                        string                 `json:"suite"`
	Case                         string                 `json:"case"`
	Trial                        int                    `json:"trial"`
	EvidenceScope                string                 `json:"evidence_scope"`
	IndependentRemoteAttestation bool                   `json:"independent_remote_attestation"`
	AuthenticityCaveat           string                 `json:"authenticity_caveat"`
	RecordSHA256                 string                 `json:"record_sha256"`
	FileSetSHA256                string                 `json:"file_set_sha256"`
	Files                        []EvaluationBundleFile `json:"files"`
}

// EvaluationBundleReceipt is a portable digest receipt plus the local
// directory at which it was verified. ReceiptSHA256 excludes Directory so a
// byte-identical copy has the same receipt.
type EvaluationBundleReceipt struct {
	Directory      string `json:"directory"`
	ManifestSHA256 string `json:"manifest_sha256"`
	RecordSHA256   string `json:"record_sha256"`
	FileSetSHA256  string `json:"file_set_sha256"`
	ReceiptSHA256  string `json:"receipt_sha256"`
}

// EvaluationBundle is the bounded result of reopening and verifying a final
// bundle. Large artifact bytes are verified while opening but are not retained
// in this return value.
type EvaluationBundle struct {
	Manifest EvaluationBundleManifest
	Record   Record
	Receipt  EvaluationBundleReceipt
}

type evaluationBundlePayload struct {
	file    EvaluationBundleFile
	payload []byte
	maximum int64
}

// WriteEvaluationBundle retains one successful Evaluation at an exclusive
// final directory, publishes manifest.json last, reopens the result, and
// returns its verified portable receipt. It never replaces an existing path.
func WriteEvaluationBundle(
	ctx context.Context, options EvaluationBundleOptions, source Evaluation,
) (receipt EvaluationBundleReceipt, resultErr error) {
	if ctx == nil {
		return EvaluationBundleReceipt{}, errors.New("write evaluation bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	directory, guard, err := prepareEvaluationBundleOptions(ctx, options)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	evaluation, recordPayload, err := snapshotEvaluationForBundle(ctx, source, guard)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	payloads, err := evaluationBundlePayloads(ctx, evaluation, recordPayload)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	for _, payload := range payloads {
		if err := rejectEvaluationSensitive(ctx, guard, payload.payload); err != nil {
			return EvaluationBundleReceipt{}, err
		}
	}
	root, err := createEvaluationBundleRoot(directory)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	defer func() {
		if closeErr := root.Close(); resultErr == nil && closeErr != nil {
			resultErr = errors.New("close evaluation bundle directory")
			receipt = EvaluationBundleReceipt{}
		}
	}()

	for _, payload := range payloads {
		if err := writeEvaluationBundleFile(ctx, root, payload.file.Path, payload.payload); err != nil {
			return EvaluationBundleReceipt{}, fmt.Errorf(
				"retain evaluation bundle %s: %w", payload.file.Purpose, err)
		}
	}
	if err := verifyEvaluationBundleEntries(ctx, root, payloads, false); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	manifest, manifestPayload, err := buildEvaluationBundleManifest(evaluation.Record, payloads)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	if err := rejectEvaluationSensitive(ctx, guard, manifestPayload); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	if err := writeEvaluationBundleFile(
		ctx, root, evaluationBundleManifestName, manifestPayload,
	); err != nil {
		return EvaluationBundleReceipt{}, errors.New("publish evaluation bundle manifest")
	}
	published := true
	defer func() {
		if resultErr != nil && published {
			_ = root.Remove(evaluationBundleManifestName)
			_ = syncEvaluationBundleDirectory(root)
		}
	}()
	opened, err := OpenEvaluationBundle(ctx, options)
	if err != nil {
		return EvaluationBundleReceipt{}, fmt.Errorf("reopen retained evaluation bundle: %w", err)
	}
	if !reflect.DeepEqual(opened.Manifest, manifest) {
		return EvaluationBundleReceipt{}, errors.New("reopened evaluation bundle manifest changed")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	published = false
	return opened.Receipt, nil
}

// OpenEvaluationBundle strictly reopens one committed bundle. Missing, extra,
// symlinked, aliased, noncanonical, mutated, traversal-bearing, sensitive, and
// incomplete trees are rejected.
func OpenEvaluationBundle(
	ctx context.Context, options EvaluationBundleOptions,
) (EvaluationBundle, error) {
	if ctx == nil {
		return EvaluationBundle{}, errors.New("open evaluation bundle: nil context")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundle{}, err
	}
	directory, guard, err := prepareEvaluationBundleOptions(ctx, options)
	if err != nil {
		return EvaluationBundle{}, err
	}
	if err := validateEvaluationBundleDirectory(directory, true); err != nil {
		return EvaluationBundle{}, err
	}
	root, err := openValidatedRoot(directory, nil)
	if err != nil {
		return EvaluationBundle{}, err
	}
	defer root.Close()
	manifestPayload, err := readEvaluationBundleFile(
		ctx, root, evaluationBundleManifestName, maximumEvaluationManifest,
	)
	if err != nil {
		return EvaluationBundle{}, errors.New("evaluation bundle is incomplete or has no valid manifest")
	}
	if err := rejectEvaluationSensitive(ctx, guard, manifestPayload); err != nil {
		return EvaluationBundle{}, err
	}
	manifest, err := decodeEvaluationBundleManifest(manifestPayload)
	if err != nil {
		return EvaluationBundle{}, err
	}
	if !manifest.Complete {
		return EvaluationBundle{}, errors.New("evaluation bundle manifest is incomplete")
	}
	recordEntry, ok := evaluationBundleFileByPurpose(manifest.Files, "record")
	if !ok || recordEntry.Path != "record.json" {
		return EvaluationBundle{}, errors.New("evaluation bundle manifest has no canonical record entry")
	}
	recordPayload, err := readEvaluationBundleFile(ctx, root, recordEntry.Path, maximumRecordBytes)
	if err != nil {
		return EvaluationBundle{}, errors.New("read evaluation bundle record")
	}
	if err := rejectEvaluationSensitive(ctx, guard, recordPayload); err != nil {
		return EvaluationBundle{}, err
	}
	record, err := DecodeRecord(bytes.NewReader(recordPayload))
	if err != nil {
		return EvaluationBundle{}, err
	}
	if record.SensitiveValueCount != guard.count {
		return EvaluationBundle{}, errors.New("evaluation bundle sensitive-value set count differs from its record")
	}
	expectedLayout, err := evaluationBundleLayout(record)
	if err != nil {
		return EvaluationBundle{}, err
	}
	if err := validateEvaluationBundleManifest(manifest, record, expectedLayout); err != nil {
		return EvaluationBundle{}, err
	}
	payloads := make([]evaluationBundlePayload, len(manifest.Files))
	for index, file := range manifest.Files {
		maximum, err := maximumEvaluationBundlePurpose(file, record)
		if err != nil {
			return EvaluationBundle{}, err
		}
		payload, err := readEvaluationBundleFile(ctx, root, file.Path, maximum)
		if err != nil {
			return EvaluationBundle{}, errors.New("read retained evaluation bundle file")
		}
		if int64(len(payload)) != file.SizeBytes || digest(payload) != file.SHA256 {
			return EvaluationBundle{}, errors.New("retained evaluation bundle file identity changed")
		}
		if err := rejectEvaluationSensitive(ctx, guard, payload); err != nil {
			return EvaluationBundle{}, err
		}
		payloads[index] = evaluationBundlePayload{file: file, payload: payload, maximum: maximum}
	}
	if err := verifyEvaluationBundleEntries(ctx, root, payloads, true); err != nil {
		return EvaluationBundle{}, err
	}
	artifact := func(purpose string) ([]byte, error) {
		for _, payload := range payloads {
			if payload.file.Purpose == purpose {
				return payload.payload, nil
			}
		}
		return nil, errors.New("evaluation bundle is missing a required retained artifact")
	}
	implementation, err := artifact("provider_implementation")
	if err != nil {
		return EvaluationBundle{}, err
	}
	configuration, err := artifact("provider_configuration")
	if err != nil {
		return EvaluationBundle{}, err
	}
	providerRequest, err := artifact("provider_request")
	if err != nil {
		return EvaluationBundle{}, err
	}
	prompt, err := artifact("prompt")
	if err != nil {
		return EvaluationBundle{}, err
	}
	schema, err := artifact("schema")
	if err != nil {
		return EvaluationBundle{}, err
	}
	contextJSON, err := artifact("context")
	if err != nil {
		return EvaluationBundle{}, err
	}
	rawResponse, err := artifact("raw_response")
	if err != nil {
		return EvaluationBundle{}, err
	}
	normalizedOutput, err := artifact("normalized_output")
	if err != nil {
		return EvaluationBundle{}, err
	}
	if err := VerifyArtifacts(
		record, implementation, configuration, providerRequest, prompt, schema,
		contextJSON, rawResponse, normalizedOutput,
	); err != nil {
		return EvaluationBundle{}, fmt.Errorf("verify evaluation bundle artifacts: %w", err)
	}
	mediaFiles := make([]EvaluationBundleFile, 0, len(record.Media))
	for _, payload := range payloads {
		if payload.file.MediaOrdinal == 0 {
			continue
		}
		media := record.Media[payload.file.MediaOrdinal-1]
		if digest(payload.payload) != media.SHA256 ||
			int64(len(payload.payload)) != media.SizeBytes {
			return EvaluationBundle{}, errors.New("retained reviewed media differs from its record")
		}
		if err := validateMediaPayloadContext(
			ctx, media.Kind, media.MediaType, payload.payload,
		); err != nil {
			return EvaluationBundle{}, errors.New("retained reviewed media is structurally invalid")
		}
		mediaFiles = append(mediaFiles, payload.file)
	}
	reviewPayload, err := artifact("human_review")
	if err != nil {
		return EvaluationBundle{}, err
	}
	expectedReview, err := renderEvaluationBundleReview(record, mediaFiles)
	if err != nil || !bytes.Equal(reviewPayload, expectedReview) {
		return EvaluationBundle{}, errors.New("evaluation bundle human review is noncanonical or changed")
	}
	expectedManifest, expectedManifestPayload, err := buildEvaluationBundleManifest(record, payloads)
	if err != nil || !reflect.DeepEqual(manifest, expectedManifest) ||
		!bytes.Equal(manifestPayload, expectedManifestPayload) {
		return EvaluationBundle{}, errors.New("evaluation bundle manifest differs from its retained inputs")
	}
	receipt, err := evaluationBundleReceipt(directory, manifestPayload, manifest)
	if err != nil {
		return EvaluationBundle{}, err
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundle{}, err
	}
	return EvaluationBundle{Manifest: manifest, Record: record, Receipt: receipt}, nil
}

func snapshotEvaluationForBundle(
	ctx context.Context, source Evaluation, guard *declaredSensitiveGuard,
) (Evaluation, []byte, error) {
	recordPayload, err := MarshalRecord(source.Record)
	if err != nil {
		return Evaluation{}, nil, err
	}
	if err := rejectEvaluationSensitive(ctx, guard, recordPayload); err != nil {
		return Evaluation{}, nil, err
	}
	record, err := DecodeRecord(bytes.NewReader(recordPayload))
	if err != nil {
		return Evaluation{}, nil, err
	}
	if record.SensitiveValueCount != guard.count {
		return Evaluation{}, nil, errors.New("evaluation sensitive-value set count differs from its record")
	}
	evaluation := Evaluation{
		Record: record, Media: make([]PreparedMedia, len(source.Media)),
		ProviderImplementation: slices.Clone(source.ProviderImplementation),
		ProviderConfiguration:  slices.Clone(source.ProviderConfiguration),
		ProviderRequest:        slices.Clone(source.ProviderRequest),
		Prompt:                 slices.Clone(source.Prompt),
		Schema:                 slices.Clone(source.Schema),
		Context:                slices.Clone(source.Context),
		RawResponse:            slices.Clone(source.RawResponse),
		NormalizedOutput:       slices.Clone(source.NormalizedOutput),
	}
	for index := range source.Media {
		evaluation.Media[index] = source.Media[index]
		evaluation.Media[index].Bytes = slices.Clone(source.Media[index].Bytes)
	}
	if err := VerifyArtifacts(
		evaluation.Record, evaluation.ProviderImplementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	); err != nil {
		return Evaluation{}, nil, err
	}
	if len(evaluation.Media) != len(record.Media) {
		return Evaluation{}, nil, errors.New("evaluation reviewed media count differs from its record")
	}
	total := int64(0)
	for index := range evaluation.Media {
		item := evaluation.Media[index]
		if item.Media != record.Media[index] || item.SizeBytes != int64(len(item.Bytes)) ||
			len(item.Bytes) == 0 || digest(item.Bytes) != item.SHA256 {
			return Evaluation{}, nil, errors.New("evaluation reviewed media differs from its record")
		}
		if total > maximumMediaBytes-int64(len(item.Bytes)) {
			return Evaluation{}, nil, errors.New("evaluation reviewed media exceeds its aggregate bound")
		}
		total += int64(len(item.Bytes))
		if err := validateMediaPayloadContext(ctx, item.Kind, item.MediaType, item.Bytes); err != nil {
			return Evaluation{}, nil, errors.New("evaluation reviewed media is structurally invalid")
		}
		metadata, err := marshalCanonicalCompact(item.Media, maximumPreparedPublicBytes)
		if err != nil {
			return Evaluation{}, nil, err
		}
		if err := rejectEvaluationSensitive(ctx, guard, metadata); err != nil {
			return Evaluation{}, nil, err
		}
		if err := rejectEvaluationSensitive(ctx, guard, item.Bytes); err != nil {
			return Evaluation{}, nil, err
		}
	}
	return evaluation, recordPayload, ctx.Err()
}

func evaluationBundlePayloads(
	ctx context.Context, evaluation Evaluation, recordPayload []byte,
) ([]evaluationBundlePayload, error) {
	layout, err := evaluationBundleLayout(evaluation.Record)
	if err != nil {
		return nil, err
	}
	fixed := map[string][]byte{
		"record":                  recordPayload,
		"provider_implementation": evaluation.ProviderImplementation,
		"provider_configuration":  evaluation.ProviderConfiguration,
		"provider_request":        evaluation.ProviderRequest,
		"prompt":                  evaluation.Prompt,
		"schema":                  evaluation.Schema,
		"context":                 evaluation.Context,
		"raw_response":            evaluation.RawResponse,
		"normalized_output":       evaluation.NormalizedOutput,
	}
	payloads := make([]evaluationBundlePayload, 0, len(layout))
	for _, file := range layout {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var payload []byte
		if file.MediaOrdinal > 0 {
			payload = evaluation.Media[file.MediaOrdinal-1].Bytes
		} else if file.Purpose == "human_review" {
			media := make([]EvaluationBundleFile, 0, len(evaluation.Record.Media))
			for _, candidate := range layout {
				if candidate.MediaOrdinal > 0 {
					media = append(media, candidate)
				}
			}
			payload, err = renderEvaluationBundleReview(evaluation.Record, media)
			if err != nil {
				return nil, err
			}
		} else {
			payload = fixed[file.Purpose]
		}
		maximum, err := maximumEvaluationBundlePurpose(file, evaluation.Record)
		if err != nil || len(payload) == 0 || int64(len(payload)) > maximum {
			return nil, errors.New("evaluation bundle artifact is empty or oversized")
		}
		file.SizeBytes = int64(len(payload))
		file.SHA256 = digest(payload)
		payloads = append(payloads, evaluationBundlePayload{
			file: file, payload: slices.Clone(payload), maximum: maximum,
		})
	}
	return payloads, nil
}

func evaluationBundleLayout(record Record) ([]EvaluationBundleFile, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	files := []EvaluationBundleFile{
		{Path: "record.json", Purpose: "record"},
		{Path: "provider-implementation.bin", Purpose: "provider_implementation"},
		{Path: "provider-configuration.json", Purpose: "provider_configuration"},
		{Path: "provider-request.bin", Purpose: "provider_request"},
		{Path: "prompt.txt", Purpose: "prompt"},
		{Path: "schema.json", Purpose: "schema"},
		{Path: "context.json", Purpose: "context"},
		{Path: "raw-response.bin", Purpose: "raw_response"},
		{Path: "normalized-output.json", Purpose: "normalized_output"},
	}
	for index, media := range record.Media {
		extension, err := evaluationMediaExtension(media.MediaType)
		if err != nil {
			return nil, err
		}
		files = append(files, EvaluationBundleFile{
			Path:    fmt.Sprintf("media-%03d.%s", index+1, extension),
			Purpose: "reviewed_media", MediaOrdinal: index + 1, MediaType: media.MediaType,
		})
	}
	files = append(files, EvaluationBundleFile{Path: evaluationBundleReviewName, Purpose: "human_review"})
	return files, nil
}

func evaluationMediaExtension(mediaType string) (string, error) {
	switch mediaType {
	case "audio/wav":
		return "wav", nil
	case "audio/ogg":
		return "ogg", nil
	case "audio/m4a":
		return "m4a", nil
	case "audio/opus":
		return "opus", nil
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpg", nil
	case "image/gif":
		return "gif", nil
	case "video/mp4":
		return "mp4", nil
	case "video/mov":
		return "mov", nil
	case "video/3gpp":
		return "3gp", nil
	default:
		return "", errors.New("evaluation reviewed media type has no deterministic extension")
	}
}

func maximumEvaluationBundlePurpose(file EvaluationBundleFile, record Record) (int64, error) {
	switch file.Purpose {
	case "record":
		return maximumRecordBytes, nil
	case "provider_implementation":
		return maximumProviderImplementationBytes, nil
	case "provider_configuration":
		return maximumProviderConfigurationBytes, nil
	case "provider_request":
		return maximumProviderRequestBytes, nil
	case "prompt":
		return maximumPromptBytes, nil
	case "schema":
		return maximumSchemaBytes, nil
	case "context":
		return maximumContextBytes, nil
	case "raw_response":
		return maximumResponseBytes, nil
	case "normalized_output":
		return maximumNormalizedOutputBytes, nil
	case "human_review":
		return maximumEvaluationReview, nil
	case "reviewed_media":
		if file.MediaOrdinal < 1 || file.MediaOrdinal > len(record.Media) {
			return 0, errors.New("evaluation bundle media ordinal is invalid")
		}
		return maximumMediaBytes, nil
	default:
		return 0, errors.New("evaluation bundle file purpose is invalid")
	}
}

func buildEvaluationBundleManifest(
	record Record, payloads []evaluationBundlePayload,
) (EvaluationBundleManifest, []byte, error) {
	files := make([]EvaluationBundleFile, len(payloads))
	for index := range payloads {
		files[index] = payloads[index].file
	}
	fileSet, err := marshalCanonicalCompact(files, maximumEvaluationManifest)
	if err != nil {
		return EvaluationBundleManifest{}, nil, err
	}
	manifest := EvaluationBundleManifest{
		Format: EvaluationBundleFormat, FormatVersion: EvaluationBundleFormatVersion,
		Complete: true, AttemptID: record.AttemptID, Suite: record.Suite, Case: record.Case,
		Trial: record.Trial, EvidenceScope: evaluationBundleScope,
		IndependentRemoteAttestation: false, AuthenticityCaveat: evaluationBundleCaveat,
		RecordSHA256: digest(payloads[0].payload), FileSetSHA256: digest(fileSet), Files: files,
	}
	payload, err := marshalCanonicalIndented(manifest, maximumEvaluationManifest)
	if err != nil {
		return EvaluationBundleManifest{}, nil, err
	}
	return manifest, payload, nil
}

func validateEvaluationBundleManifest(
	manifest EvaluationBundleManifest, record Record, layout []EvaluationBundleFile,
) error {
	if manifest.Format != EvaluationBundleFormat ||
		manifest.FormatVersion != EvaluationBundleFormatVersion || !manifest.Complete {
		return errors.New("evaluation bundle manifest format or completion state is invalid")
	}
	if manifest.AttemptID != record.AttemptID || manifest.Suite != record.Suite ||
		manifest.Case != record.Case || manifest.Trial != record.Trial {
		return errors.New("evaluation bundle manifest identity differs from its record")
	}
	if manifest.EvidenceScope != evaluationBundleScope || manifest.IndependentRemoteAttestation ||
		manifest.AuthenticityCaveat != evaluationBundleCaveat {
		return errors.New("evaluation bundle manifest overstates or changes its provenance scope")
	}
	if err := validateDigest(manifest.RecordSHA256); err != nil {
		return errors.New("evaluation bundle record digest is invalid")
	}
	if err := validateDigest(manifest.FileSetSHA256); err != nil {
		return errors.New("evaluation bundle file-set digest is invalid")
	}
	if len(manifest.Files) != len(layout) || len(manifest.Files) > maximumEvaluationFiles {
		return errors.New("evaluation bundle manifest file count is invalid")
	}
	seenPaths := make(map[string]struct{}, len(manifest.Files))
	seenOrdinals := make(map[int]struct{}, len(record.Media))
	for index, file := range manifest.Files {
		if file.Path != layout[index].Path || file.Purpose != layout[index].Purpose ||
			file.MediaOrdinal != layout[index].MediaOrdinal || file.MediaType != layout[index].MediaType {
			return errors.New("evaluation bundle manifest layout is noncanonical")
		}
		if err := validateEvaluationBundleName(file.Path); err != nil {
			return err
		}
		if _, duplicate := seenPaths[file.Path]; duplicate {
			return errors.New("evaluation bundle manifest repeats a file path")
		}
		seenPaths[file.Path] = struct{}{}
		if file.MediaOrdinal > 0 {
			if _, duplicate := seenOrdinals[file.MediaOrdinal]; duplicate {
				return errors.New("evaluation bundle manifest repeats a media ordinal")
			}
			seenOrdinals[file.MediaOrdinal] = struct{}{}
		}
		maximum, err := maximumEvaluationBundlePurpose(file, record)
		if err != nil || file.SizeBytes <= 0 || file.SizeBytes > maximum ||
			validateDigest(file.SHA256) != nil {
			return errors.New("evaluation bundle manifest file identity is invalid")
		}
	}
	fileSet, err := marshalCanonicalCompact(manifest.Files, maximumEvaluationManifest)
	if err != nil || digest(fileSet) != manifest.FileSetSHA256 {
		return errors.New("evaluation bundle manifest file-set digest differs from its files")
	}
	return nil
}

func decodeEvaluationBundleManifest(payload []byte) (EvaluationBundleManifest, error) {
	if len(payload) == 0 || len(payload) > maximumEvaluationManifest {
		return EvaluationBundleManifest{}, errors.New("evaluation bundle manifest is empty or oversized")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumEvaluationManifest, MaxDepth: 5,
		MaxTokens: 32 + maximumEvaluationFiles*16, MaxObjectMembers: 20,
		MaxArrayElements: maximumEvaluationFiles, MaxKeyBytes: 64,
		MaxTotalKeyBytes: 1 << 20, MaxWorkBytes: 32 << 20,
	}); err != nil {
		return EvaluationBundleManifest{}, errors.New("evaluation bundle manifest is not strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest EvaluationBundleManifest
	if err := decoder.Decode(&manifest); err != nil {
		return EvaluationBundleManifest{}, errors.New("decode evaluation bundle manifest")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return EvaluationBundleManifest{}, errors.New("evaluation bundle manifest has trailing data")
	}
	canonical, err := marshalCanonicalIndented(manifest, maximumEvaluationManifest)
	if err != nil || !bytes.Equal(payload, canonical) {
		return EvaluationBundleManifest{}, errors.New("evaluation bundle manifest is not canonical")
	}
	return manifest, nil
}

func evaluationBundleReceipt(
	directory string, manifestPayload []byte, manifest EvaluationBundleManifest,
) (EvaluationBundleReceipt, error) {
	receipt := EvaluationBundleReceipt{
		Directory: directory, ManifestSHA256: digest(manifestPayload),
		RecordSHA256: manifest.RecordSHA256, FileSetSHA256: manifest.FileSetSHA256,
	}
	source, err := marshalCanonicalCompact(struct {
		Format         string `json:"format"`
		FormatVersion  int    `json:"format_version"`
		ManifestSHA256 string `json:"manifest_sha256"`
		RecordSHA256   string `json:"record_sha256"`
		FileSetSHA256  string `json:"file_set_sha256"`
	}{EvaluationBundleFormat, EvaluationBundleFormatVersion, receipt.ManifestSHA256,
		receipt.RecordSHA256, receipt.FileSetSHA256}, maximumEvaluationManifest)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	receipt.ReceiptSHA256 = digest(source)
	return receipt, nil
}

func renderEvaluationBundleReview(
	record Record, mediaFiles []EvaluationBundleFile,
) ([]byte, error) {
	if err := record.Validate(); err != nil || len(mediaFiles) != len(record.Media) {
		return nil, errors.New("cannot render human review from invalid evaluation")
	}
	var output strings.Builder
	output.WriteString("# Evaluation review\n\n")
	output.WriteString("This is a secondary multimodal review. Deterministic benchmark scoring remains authoritative.\n\n")
	output.WriteString("- Suite: ")
	output.WriteString(escapeEvaluationMarkdown(record.Suite))
	output.WriteString("\n- Case: ")
	output.WriteString(escapeEvaluationMarkdown(record.Case))
	output.WriteString("\n- Trial: ")
	output.WriteString(fmt.Sprintf("%d", record.Trial))
	output.WriteString("\n- Provider: ")
	output.WriteString(escapeEvaluationMarkdown(record.Provider.Provider))
	output.WriteString(" / ")
	output.WriteString(escapeEvaluationMarkdown(record.Provider.Model))
	output.WriteString("\n- Observed outcome: **")
	output.WriteString(record.Assessment.ObservedOutcome)
	output.WriteString("**\n- Media usable: ")
	output.WriteString(fmt.Sprintf("%t", record.Assessment.MediaUsable))
	output.WriteString("\n- Agrees with deterministic result: ")
	output.WriteString(fmt.Sprintf("%t", record.Assessment.AgreesWithDeterministic))
	output.WriteString("\n- Confidence: ")
	output.WriteString(fmt.Sprintf("%.6g", record.Assessment.Confidence))
	output.WriteString("\n\n## Summary\n\n")
	output.WriteString(escapeEvaluationMarkdown(record.Assessment.Summary))
	output.WriteString("\n\n## Significant problems\n\n")
	writeEvaluationFindings(&output, record.Assessment.SignificantProblems)
	output.WriteString("\n## Minor observations\n\n")
	writeEvaluationFindings(&output, record.Assessment.MinorObservations)
	output.WriteString("\n## Limitations\n\n")
	if len(record.Assessment.Limitations) == 0 {
		output.WriteString("- None reported.\n")
	} else {
		for _, limitation := range record.Assessment.Limitations {
			output.WriteString("- ")
			output.WriteString(escapeEvaluationMarkdown(limitation))
			output.WriteByte('\n')
		}
	}
	output.WriteString("\n## Reviewed media\n\n")
	for index, file := range mediaFiles {
		media := record.Media[index]
		output.WriteString("- [")
		output.WriteString(file.Path)
		output.WriteString("](")
		output.WriteString(file.Path)
		output.WriteString(") — ")
		output.WriteString(escapeEvaluationMarkdown(media.Kind))
		output.WriteString(" / ")
		output.WriteString(escapeEvaluationMarkdown(media.Role))
		output.WriteString(" / ")
		output.WriteString(escapeEvaluationMarkdown(media.MediaType))
		output.WriteString(" / ")
		output.WriteString(media.SHA256)
		output.WriteByte('\n')
	}
	output.WriteString("\n## Provenance boundary\n\n")
	output.WriteString(evaluationBundleCaveat)
	output.WriteString(" The retained provider request and response are bound to the successful in-process provider verification represented by the review record.\n")
	if output.Len() == 0 || output.Len() > maximumEvaluationReview {
		return nil, errors.New("evaluation human review is empty or oversized")
	}
	return []byte(output.String()), nil
}

func writeEvaluationFindings(output *strings.Builder, findings []Finding) {
	if len(findings) == 0 {
		output.WriteString("- None reported.\n")
		return
	}
	for _, finding := range findings {
		output.WriteString("- **")
		output.WriteString(escapeEvaluationMarkdown(finding.Category))
		output.WriteString("**")
		if finding.StartMS != nil || finding.EndMS != nil {
			output.WriteString(" (ms ")
			if finding.StartMS != nil {
				output.WriteString(fmt.Sprintf("%d", *finding.StartMS))
			} else {
				output.WriteByte('?')
			}
			output.WriteString("–")
			if finding.EndMS != nil {
				output.WriteString(fmt.Sprintf("%d", *finding.EndMS))
			} else {
				output.WriteByte('?')
			}
			output.WriteByte(')')
		}
		output.WriteString(": ")
		output.WriteString(escapeEvaluationMarkdown(finding.Evidence))
		output.WriteString(" Impact: ")
		output.WriteString(escapeEvaluationMarkdown(finding.Impact))
		output.WriteByte('\n')
	}
}

func escapeEvaluationMarkdown(value string) string {
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(value)
	var output strings.Builder
	output.Grow(len(value))
	for _, character := range value {
		switch character {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '<', '>', '#', '+', '-', '.', '!', '|':
			output.WriteByte('\\')
		}
		output.WriteRune(character)
	}
	return output.String()
}

func prepareEvaluationBundleOptions(
	ctx context.Context, options EvaluationBundleOptions,
) (string, *declaredSensitiveGuard, error) {
	values, err := canonicalSensitiveValues(options.SensitiveValues)
	if err != nil {
		return "", nil, err
	}
	guard, err := newDeclaredSensitiveGuard(ctx, values)
	if err != nil {
		return "", nil, err
	}
	if matched, err := guard.matcher.containsContext(ctx, []byte(options.Directory)); err != nil {
		return "", nil, err
	} else if matched {
		return "", nil, errors.New("evaluation bundle directory contains a declared sensitive value")
	}
	if err := validateEvaluationBundleDirectory(options.Directory, false); err != nil {
		return "", nil, err
	}
	return options.Directory, guard, nil
}

func rejectEvaluationSensitive(
	ctx context.Context, guard *declaredSensitiveGuard, payload []byte,
) error {
	if guard == nil || guard.count == 0 {
		return ctx.Err()
	}
	matched, err := guard.hasContext(ctx, payload)
	if err != nil {
		return err
	}
	if matched {
		return errors.New("evaluation bundle content contains a declared sensitive value")
	}
	return nil
}

func validateEvaluationBundleDirectory(path string, mustExist bool) error {
	if len(path) == 0 || len(path) > maximumRootDirectoryBytes || !utf8.ValidString(path) ||
		containsControl(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == filepath.Dir(path) {
		return errors.New("evaluation bundle directory must be a bounded clean absolute non-root path")
	}
	parent := filepath.Dir(path)
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("evaluation bundle directory parent must be a non-symlink directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	info, err := os.Lstat(path)
	if !mustExist {
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("evaluation bundle final path must not be a symlink")
			}
			return nil
		}
		if os.IsNotExist(err) {
			return nil
		}
		return errors.New("inspect evaluation bundle final path")
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("evaluation bundle final path must be a non-symlink directory")
	}
	return nil
}

func createEvaluationBundleRoot(directory string) (*os.Root, error) {
	if err := validateEvaluationBundleDirectory(directory, false); err != nil {
		return nil, err
	}
	parentPath, base := filepath.Dir(directory), filepath.Base(directory)
	parentBefore, err := os.Lstat(parentPath)
	if err != nil || parentBefore.Mode()&os.ModeSymlink != 0 || !parentBefore.IsDir() {
		return nil, errors.New("evaluation bundle parent is invalid")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return nil, errors.New("open evaluation bundle parent")
	}
	defer parent.Close()
	openedParent, openErr := parent.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if openErr != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentBefore, openedParent) ||
		!os.SameFile(openedParent, visibleParent) {
		return nil, errors.New("evaluation bundle parent changed while opening")
	}
	if err := parent.Mkdir(base, 0o700); err != nil {
		return nil, errors.New("create evaluation bundle directory exclusively")
	}
	if err := syncEvaluationBundleDirectory(parent); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("open created evaluation bundle directory")
	}
	opened, openErr := root.Stat(".")
	entry, entryErr := parent.Lstat(base)
	visible, visibleErr := os.Lstat(directory)
	parentAfter, parentAfterErr := os.Lstat(parentPath)
	if openErr != nil || entryErr != nil || visibleErr != nil || parentAfterErr != nil ||
		entry.Mode()&os.ModeSymlink != 0 || !entry.IsDir() ||
		!os.SameFile(parentBefore, parentAfter) || !os.SameFile(openedParent, parentAfter) ||
		!os.SameFile(opened, entry) || !os.SameFile(entry, visible) {
		_ = root.Close()
		return nil, errors.New("created evaluation bundle directory changed while opening")
	}
	return root, nil
}

func writeEvaluationBundleFile(
	ctx context.Context, root *os.Root, name string, payload []byte,
) error {
	if err := validateEvaluationBundleName(name); err != nil || len(payload) == 0 {
		return errors.New("evaluation bundle file request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = root.Remove(name)
		}
	}()
	created, err := file.Stat()
	if err != nil || !created.Mode().IsRegular() {
		return errors.New("created evaluation bundle artifact is not regular")
	}
	for offset := 0; offset < len(payload); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+(64<<10), len(payload))
		count, err := file.Write(payload[offset:end])
		if err != nil || count != end-offset {
			return io.ErrShortWrite
		}
		offset = end
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync evaluation bundle artifact")
	}
	afterWrite, err := file.Stat()
	if err != nil || !os.SameFile(created, afterWrite) ||
		afterWrite.Size() != int64(len(payload)) {
		return errors.New("evaluation bundle artifact changed while writing")
	}
	if err := file.Close(); err != nil {
		return errors.New("close evaluation bundle artifact")
	}
	visible, err := root.Lstat(name)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.Mode().IsRegular() ||
		!os.SameFile(afterWrite, visible) || visible.Size() != int64(len(payload)) {
		return errors.New("evaluation bundle artifact identity changed after close")
	}
	retained, err := readEvaluationBundleFile(ctx, root, name, int64(len(payload)))
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("evaluation bundle artifact bytes changed after close")
	}
	if err := syncEvaluationBundleDirectory(root); err != nil {
		return err
	}
	complete = true
	return nil
}

func readEvaluationBundleFile(
	ctx context.Context, root *os.Root, name string, maximum int64,
) ([]byte, error) {
	if root == nil || maximum <= 0 || validateEvaluationBundleName(name) != nil {
		return nil, errors.New("evaluation bundle file reader is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() <= 0 || before.Size() > maximum {
		return nil, errors.New("evaluation bundle artifact is not a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("open evaluation bundle artifact")
	}
	defer file.Close()
	opened, openErr := file.Stat()
	afterOpen, afterOpenErr := root.Lstat(name)
	if openErr != nil || afterOpenErr != nil || afterOpen.Mode()&os.ModeSymlink != 0 ||
		!afterOpen.Mode().IsRegular() || !os.SameFile(before, opened) ||
		!os.SameFile(opened, afterOpen) || opened.Size() != before.Size() {
		return nil, errors.New("evaluation bundle artifact changed while opening")
	}
	payload := make([]byte, 0, int(opened.Size()))
	buffer := make([]byte, 64<<10)
	for int64(len(payload)) <= maximum {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if len(payload) > int(maximum)-count {
				return nil, errors.New("evaluation bundle artifact exceeds its bound")
			}
			payload = append(payload, buffer[:count]...)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, errors.New("read evaluation bundle artifact")
		}
	}
	afterRead, statErr := file.Stat()
	visible, visibleErr := root.Lstat(name)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, afterRead) ||
		!os.SameFile(afterRead, visible) || visible.Size() != int64(len(payload)) ||
		len(payload) == 0 || int64(len(payload)) > maximum {
		return nil, errors.New("evaluation bundle artifact changed while reading")
	}
	return payload, ctx.Err()
}

func verifyEvaluationBundleEntries(
	ctx context.Context, root *os.Root, payloads []evaluationBundlePayload, includeManifest bool,
) error {
	expected := make(map[string]struct{}, len(payloads)+1)
	for _, payload := range payloads {
		if _, duplicate := expected[payload.file.Path]; duplicate {
			return errors.New("evaluation bundle repeats an expected path")
		}
		expected[payload.file.Path] = struct{}{}
		retained, err := readEvaluationBundleFile(
			ctx, root, payload.file.Path, payload.maximum,
		)
		if err != nil || !bytes.Equal(retained, payload.payload) ||
			digest(retained) != payload.file.SHA256 {
			return errors.New("evaluation bundle retained artifact verification failed")
		}
	}
	if includeManifest {
		expected[evaluationBundleManifestName] = struct{}{}
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open evaluation bundle directory listing")
	}
	defer directory.Close()
	identities := make([]os.FileInfo, 0, len(expected))
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDir(32)
		if err != nil && err != io.EOF {
			return errors.New("read evaluation bundle directory")
		}
		for _, entry := range entries {
			count++
			if count > len(expected) {
				return errors.New("evaluation bundle contains extra entries")
			}
			if _, ok := expected[entry.Name()]; !ok ||
				entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return errors.New("evaluation bundle contains an unexpected or non-regular entry")
			}
			info, infoErr := root.Lstat(entry.Name())
			if infoErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("evaluation bundle entry is not a regular non-symlink file")
			}
			for _, prior := range identities {
				if os.SameFile(prior, info) {
					return errors.New("evaluation bundle contains duplicate file identities")
				}
			}
			identities = append(identities, info)
		}
		if err == io.EOF {
			break
		}
	}
	if count != len(expected) {
		return errors.New("evaluation bundle is missing expected entries")
	}
	return nil
}

func syncEvaluationBundleDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open evaluation bundle directory for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync evaluation bundle directory")
	}
	return nil
}

func validateEvaluationBundleName(name string) error {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || containsControl(name) ||
		filepath.IsAbs(name) || filepath.Base(name) != name || filepath.Clean(name) != name ||
		name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return errors.New("evaluation bundle path must be one safe canonical filename")
	}
	return nil
}

func evaluationBundleFileByPurpose(
	files []EvaluationBundleFile, purpose string,
) (EvaluationBundleFile, bool) {
	var result EvaluationBundleFile
	found := false
	for _, file := range files {
		if file.Purpose != purpose {
			continue
		}
		if found {
			return EvaluationBundleFile{}, false
		}
		result, found = file, true
	}
	return result, found
}
