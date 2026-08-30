package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

// Validate checks the complete review-record contract without trusting a
// provider or re-reading media. VerifyArtifacts additionally checks every
// retention-ready byte artifact returned by Evaluate.
func (record Record) Validate() error {
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
	for label, value := range map[string]string{
		"prompt": record.Prompt.SHA256, "schema": record.Schema.SHA256,
		"context": record.ContextSHA256, "request fingerprint": record.RequestFingerprint,
		"provider request":  record.ProviderRequestSHA256,
		"raw response":      record.RawResponseSHA256,
		"normalized output": record.NormalizedOutputSHA256,
	} {
		if err := validateDigest(value); err != nil {
			return fmt.Errorf("review record %s: %w", label, err)
		}
	}
	if record.ProviderRequestID != "" {
		if err := validateMachineIdentifier(
			"review record provider request ID", record.ProviderRequestID, 4096); err != nil {
			return err
		}
	}
	if record.ReportedModel != record.Provider.Model {
		return errors.New("review record reported model differs from its pinned provider")
	}
	if len(record.Media) == 0 || len(record.Media) > maximumMediaCount {
		return fmt.Errorf("review record needs 1..%d media artifacts", maximumMediaCount)
	}
	seen := make(map[string]struct{}, len(record.Media))
	for index, media := range record.Media {
		if err := validateMediaIdentity(media); err != nil {
			return fmt.Errorf("review record media %d: %w", index, err)
		}
		if _, duplicate := seen[media.Path]; duplicate {
			return fmt.Errorf("review record repeats media path %q", media.Path)
		}
		seen[media.Path] = struct{}{}
	}
	assessment, err := json.Marshal(record.Assessment)
	if err != nil {
		return fmt.Errorf("encode review record assessment: %w", err)
	}
	if _, _, err := normalizeAssessment(assessment); err != nil {
		return fmt.Errorf("review record assessment: %w", err)
	}
	return nil
}

// MarshalRecord emits the one canonical create-only JSON representation.
func MarshalRecord(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
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
	record Record, prompt, schema, contextJSON, rawResponse, normalizedOutput []byte,
) error {
	if err := record.Validate(); err != nil {
		return err
	}
	for label, artifact := range map[string]struct {
		bytes  []byte
		digest string
	}{
		"prompt":            {prompt, record.Prompt.SHA256},
		"schema":            {schema, record.Schema.SHA256},
		"context":           {contextJSON, record.ContextSHA256},
		"raw response":      {rawResponse, record.RawResponseSHA256},
		"normalized output": {normalizedOutput, record.NormalizedOutputSHA256},
	} {
		if digest(artifact.bytes) != artifact.digest {
			return fmt.Errorf("review %s digest does not match its record", label)
		}
	}
	if !bytes.Equal(schema, caseReviewSchema()) {
		return errors.New("review schema bytes differ from the declared schema version")
	}
	canonicalContext, err := canonicalJSON(contextJSON, maximumContextBytes)
	if err != nil || !bytes.Equal(contextJSON, canonicalContext) {
		return errors.New("review context is not canonical JSON")
	}
	promptContext, err := json.Marshal(struct {
		Suite   string          `json:"suite"`
		Case    string          `json:"case"`
		Trial   int             `json:"trial"`
		Context json.RawMessage `json:"deterministic_context"`
		Media   []Media         `json:"media"`
	}{record.Suite, record.Case, record.Trial, contextJSON, record.Media})
	if err != nil || !bytes.Equal(prompt, []byte(caseReviewPrompt(string(promptContext)))) {
		return errors.New("review prompt bytes differ from the declared prompt version and context")
	}
	fingerprintSource, err := json.Marshal(struct {
		AttemptID     string  `json:"attempt_id"`
		PromptVersion string  `json:"prompt_version"`
		PromptSHA256  string  `json:"prompt_sha256"`
		SchemaVersion string  `json:"schema_version"`
		SchemaSHA256  string  `json:"schema_sha256"`
		ContextSHA256 string  `json:"context_sha256"`
		Media         []Media `json:"media"`
	}{
		record.AttemptID, record.Prompt.Version, record.Prompt.SHA256,
		record.Schema.Version, record.Schema.SHA256, record.ContextSHA256, record.Media,
	})
	if err != nil || digest(fingerprintSource) != record.RequestFingerprint {
		return errors.New("review request fingerprint differs from its retained inputs")
	}
	assessment, canonicalOutput, err := normalizeAssessment(normalizedOutput)
	if err != nil || !bytes.Equal(normalizedOutput, canonicalOutput) ||
		!reflect.DeepEqual(assessment, record.Assessment) {
		return errors.New("review normalized output is not canonical or differs from its record")
	}
	if len(rawResponse) == 0 || len(rawResponse) > maximumResponseBytes {
		return errors.New("review raw response is empty or oversized")
	}
	return nil
}

func strictRecordJSON(payload []byte) error {
	if err := strictjson.Validate(payload); err != nil {
		return fmt.Errorf("decode review record: %w", err)
	}
	return nil
}
