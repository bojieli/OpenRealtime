package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// Validate checks the complete review-record contract without trusting a
// provider or re-reading media. VerifyArtifacts additionally checks every
// retention-ready byte artifact returned by Evaluate.
func (record Record) Validate() error {
	if len(record.Media) == 0 || len(record.Media) > maximumMediaCount {
		return fmt.Errorf("review record needs 1..%d media artifacts", maximumMediaCount)
	}
	if record.SensitiveValueCount < 0 || record.SensitiveValueCount > maximumSensitiveValues {
		return fmt.Errorf(
			"review record sensitive-value count must be 0..%d", maximumSensitiveValues)
	}
	if err := validateRecordAssessment(record.Assessment); err != nil {
		return fmt.Errorf("review record assessment: %w", err)
	}
	if record.Format != RecordFormat || record.FormatVersion != FormatVersion {
		return fmt.Errorf("review record must use %s version %d", RecordFormat, FormatVersion)
	}
	if err := validateHumanIdentifier("review record attempt ID", record.AttemptID, 1024); err != nil {
		return err
	}
	if err := validateHumanIdentifier("review record suite", record.Suite, 1024); err != nil {
		return err
	}
	if err := validateHumanIdentifier("review record case", record.Case, 1024); err != nil {
		return err
	}
	if record.Trial <= 0 {
		return errors.New("review record trial must be positive")
	}
	if err := record.Provider.Validate(); err != nil {
		return fmt.Errorf("review record provider: %w", err)
	}
	if record.Prompt.Version != CasePromptVersion || record.Schema.Version != CaseSchemaVersion {
		return errors.New("review record has unsupported prompt or schema versions")
	}
	if record.Sanitization != SanitizationVersion {
		return errors.New("review record has invalid sanitization metadata")
	}
	for _, field := range []struct{ label, value string }{
		{"prompt", record.Prompt.SHA256}, {"schema", record.Schema.SHA256},
		{"context", record.ContextSHA256}, {"request fingerprint", record.RequestFingerprint},
		{"provider request", record.ProviderRequestSHA256},
		{"raw response", record.RawResponseSHA256},
		{"normalized output", record.NormalizedOutputSHA256},
	} {
		if err := validateDigest(field.value); err != nil {
			return fmt.Errorf("review record %s: %w", field.label, err)
		}
	}
	switch record.ProviderRequestIDState {
	case ProviderRequestIDValue:
		if err := validateMachineIdentifier(
			"review record provider request ID", record.ProviderRequestID, 4096); err != nil {
			return err
		}
	case ProviderRequestIDMissing, ProviderRequestIDNull:
		if record.ProviderRequestID != "" {
			return errors.New("review record provider request ID state disagrees with its value")
		}
	default:
		return errors.New("review record provider request ID state is invalid")
	}
	if record.ReportedModel != record.Provider.Model {
		return errors.New("review record reported model differs from its pinned provider")
	}
	seen := make(map[string]struct{}, len(record.Media))
	for index, media := range record.Media {
		if err := validateMediaIdentity(media); err != nil {
			return fmt.Errorf("review record media %d: %w", index, err)
		}
		if media.Validation != MediaValidationVersion {
			return fmt.Errorf("review record media %d has invalid validation provenance", index)
		}
		if _, duplicate := seen[media.Path]; duplicate {
			return fmt.Errorf("review record repeats media path %q", media.Path)
		}
		seen[media.Path] = struct{}{}
	}
	assessment, err := marshalCanonicalCompact(record.Assessment, maximumNormalizedOutputBytes)
	if err != nil {
		return fmt.Errorf("encode review record assessment: %w", err)
	}
	if _, _, err := normalizeAssessment(assessment); err != nil {
		return fmt.Errorf("review record assessment: %w", err)
	}
	return nil
}

// validateRecordAssessment admits every caller-owned field before JSON
// encoding. In particular, it prevents large typed slices or strings from
// forcing an unbounded encoder allocation merely to discover that the public
// assessment contract rejects them.
func validateRecordAssessment(assessment Assessment) error {
	if assessment.SignificantProblems == nil || assessment.MinorObservations == nil ||
		assessment.Limitations == nil {
		return errors.New("findings and limitations must be non-null arrays")
	}
	if len(assessment.SignificantProblems) > 128 || len(assessment.MinorObservations) > 128 ||
		len(assessment.Limitations) > 128 {
		return errors.New("has more than 128 findings or limitations")
	}
	if assessment.ObservedOutcome != "pass" && assessment.ObservedOutcome != "fail" &&
		assessment.ObservedOutcome != "unclear" {
		return errors.New("observed_outcome must be pass, fail, or unclear")
	}
	if math.IsNaN(assessment.Confidence) || math.IsInf(assessment.Confidence, 0) ||
		assessment.Confidence < 0 || assessment.Confidence > 1 {
		return errors.New("confidence must be between zero and one")
	}
	if err := validateReviewText("summary", assessment.Summary, true, 16<<10); err != nil {
		return err
	}
	for _, group := range []struct {
		label    string
		findings []Finding
	}{
		{"significant problem", assessment.SignificantProblems},
		{"minor observation", assessment.MinorObservations},
	} {
		for index, finding := range group.findings {
			if err := validateFinding(fmt.Sprintf("%s %d", group.label, index), finding); err != nil {
				return err
			}
		}
	}
	for index, limitation := range assessment.Limitations {
		if err := validateReviewText(fmt.Sprintf("limitation %d", index), limitation, true, 4096); err != nil {
			return err
		}
	}
	return nil
}

// MarshalRecord emits the one canonical create-only JSON representation.
func MarshalRecord(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	payload, err := marshalCanonicalIndented(record, maximumRecordBytes)
	if err != nil {
		return nil, fmt.Errorf("encode review record: %w", err)
	}
	return payload, nil
}

// DecodeRecord accepts only the canonical representation produced by
// MarshalRecord, rejecting duplicate keys, unknown fields, and trailing data.
func DecodeRecord(reader io.Reader) (Record, error) {
	if reader == nil {
		return Record{}, errors.New("decode review record: reader is nil")
	}
	limited := &io.LimitedReader{R: reader, N: maximumResponseBytes + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return Record{}, fmt.Errorf("read review record: %w", err)
	}
	if len(payload) == 0 || len(payload) > maximumResponseBytes {
		return Record{}, fmt.Errorf("review record must be 1..%d bytes", maximumResponseBytes)
	}
	if err := strictRecordJSON(payload); err != nil {
		return Record{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode review record: %w", err)
	}
	canonical, err := MarshalRecord(record)
	if err != nil {
		return Record{}, err
	}
	if !bytes.Equal(payload, canonical) {
		return Record{}, errors.New("review record bytes are not canonical")
	}
	return record, nil
}

// VerifyArtifacts proves that the prompt, schema, context, raw provider
// envelope, and normalized assessment still match a validated record.
func VerifyArtifacts(
	record Record, implementation, configuration, providerRequest,
	prompt, schema, contextJSON, rawResponse, normalizedOutput []byte,
) error {
	if err := record.Validate(); err != nil {
		return err
	}
	artifacts := []struct {
		label   string
		payload []byte
		digest  string
		maximum int
	}{
		{"provider implementation", implementation, record.Provider.Implementation.SHA256, maximumProviderImplementationBytes},
		{"provider configuration", configuration, record.Provider.ConfigurationSHA256, maximumProviderConfigurationBytes},
		{"provider request", providerRequest, record.ProviderRequestSHA256, maximumProviderRequestBytes},
		{"prompt", prompt, record.Prompt.SHA256, maximumPromptBytes},
		{"schema", schema, record.Schema.SHA256, maximumSchemaBytes},
		{"context", contextJSON, record.ContextSHA256, maximumContextBytes},
		{"raw response", rawResponse, record.RawResponseSHA256, maximumResponseBytes},
		{"normalized output", normalizedOutput, record.NormalizedOutputSHA256, maximumNormalizedOutputBytes},
	}
	// Admit every retained artifact before hashing any of them. This keeps a
	// late oversized value from causing avoidable work over earlier buffers and
	// gives callers deterministic failures in the declared artifact order.
	for _, artifact := range artifacts {
		if len(artifact.payload) == 0 || len(artifact.payload) > artifact.maximum {
			return fmt.Errorf(
				"review %s must be 1..%d bytes", artifact.label, artifact.maximum)
		}
	}
	for _, artifact := range artifacts {
		if digest(artifact.payload) != artifact.digest {
			return fmt.Errorf("review %s digest does not match its record", artifact.label)
		}
	}
	canonicalConfiguration, err := canonicalJSON(configuration, maximumProviderConfigurationBytes)
	if err != nil || !bytes.Equal(configuration, canonicalConfiguration) {
		return errors.New("review provider configuration is not canonical JSON")
	}
	if !bytes.Equal(schema, caseReviewSchema()) {
		return errors.New("review schema bytes differ from the declared schema version")
	}
	canonicalContext, err := canonicalJSON(contextJSON, maximumContextBytes)
	if err != nil || !bytes.Equal(contextJSON, canonicalContext) {
		return errors.New("review context is not canonical JSON")
	}
	promptContext, err := marshalCanonicalCompact(struct {
		Suite   string          `json:"suite"`
		Case    string          `json:"case"`
		Trial   int             `json:"trial"`
		Context json.RawMessage `json:"deterministic_context"`
		Media   []Media         `json:"media"`
	}{record.Suite, record.Case, record.Trial, contextJSON, record.Media}, maximumPromptBytes)
	if err != nil || !bytes.Equal(prompt, []byte(caseReviewPrompt(string(promptContext)))) {
		return errors.New("review prompt bytes differ from the declared prompt version and context")
	}
	fingerprintSource, err := marshalCanonicalCompact(struct {
		AttemptID           string  `json:"attempt_id"`
		PromptVersion       string  `json:"prompt_version"`
		PromptSHA256        string  `json:"prompt_sha256"`
		SchemaVersion       string  `json:"schema_version"`
		SchemaSHA256        string  `json:"schema_sha256"`
		ContextSHA256       string  `json:"context_sha256"`
		Media               []Media `json:"media"`
		Sanitization        string  `json:"sanitization"`
		SensitiveValueCount int     `json:"sensitive_value_count"`
	}{
		record.AttemptID, record.Prompt.Version, record.Prompt.SHA256,
		record.Schema.Version, record.Schema.SHA256, record.ContextSHA256, record.Media,
		record.Sanitization, record.SensitiveValueCount,
	}, maximumPreparedPublicBytes)
	if err != nil || digest(fingerprintSource) != record.RequestFingerprint {
		return errors.New("review request fingerprint differs from its retained inputs")
	}
	assessment, canonicalOutput, err := normalizeAssessment(normalizedOutput)
	if err != nil || !bytes.Equal(normalizedOutput, canonicalOutput) ||
		!reflect.DeepEqual(assessment, record.Assessment) {
		return errors.New("review normalized output is not canonical or differs from its record")
	}
	return nil
}

func strictRecordJSON(payload []byte) error {
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumRecordBytes, MaxDepth: 8, MaxTokens: 8192,
		MaxObjectMembers: 32, MaxArrayElements: maximumMediaCount, MaxKeyBytes: 64,
		MaxTotalKeyBytes: 1 << 20, MaxWorkBytes: 64 << 20,
	}); err != nil {
		return fmt.Errorf("decode review record: %w", err)
	}
	return nil
}
