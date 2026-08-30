// Package review defines the provider-neutral, offline multimodal review
// boundary for benchmark artifacts. Deterministic benchmark scorers remain the
// authority for pass/fail; a review provider can surface media-quality and
// behavioral problems for a human without becoming part of the realtime
// server or presentation clients.
package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	RecordFormat          = "openrealtime.multimodal-review"
	FormatVersion         = 1
	CasePromptVersion     = "openrealtime.case-media-review.prompt.v1"
	CaseSchemaVersion     = "openrealtime.case-media-review.schema.v1"
	maximumContextBytes   = 4 << 20
	maximumMediaBytes     = 128 << 20
	maximumResponseBytes  = 8 << 20
	maximumMediaCount     = 256
	maximumMediaPathBytes = 4096
)

// ProviderDescriptor is immutable review-provider provenance. APIRevision
// pins the wire contract separately from Model so a protocol migration cannot
// masquerade as another run of the same evaluator.
type ProviderDescriptor struct {
	Provider            string `json:"provider"`
	Model               string `json:"model"`
	API                 string `json:"api"`
	APIRevision         string `json:"api_revision"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
}

// Provider is a replaceable offline evaluator plugin. Review receives only
// content-addressed media and public benchmark context; credentials remain
// private to the provider implementation.
type Provider interface {
	Descriptor() ProviderDescriptor
	Review(context.Context, PreparedRequest) (ProviderResponse, error)
}

// Media identifies one retained artifact within RootDirectory. Path is always
// a clean relative path; SHA256 is checked against the bytes before a provider
// can observe them.
type Media struct {
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

// Request is one deterministic benchmark attempt to review. Context must be a
// self-contained JSON object containing scorer outcomes, errors, transcripts,
// action traces, and any suite-specific criteria intended for the reviewer.
type Request struct {
	AttemptID     string          `json:"attempt_id"`
	Suite         string          `json:"suite"`
	Case          string          `json:"case"`
	Trial         int             `json:"trial"`
	RootDirectory string          `json:"-"`
	Context       json.RawMessage `json:"context"`
	Media         []Media         `json:"media"`
}

// PreparedMedia is an immutable copy of one digest-verified artifact. Bytes is
// deliberately omitted from JSON and copied for every provider invocation.
type PreparedMedia struct {
	Media
	Bytes []byte `json:"-"`
}

// PreparedRequest is the exact provider input produced by Prepare. Prompt,
// schema, context, and media identities are all versioned and fingerprinted.
type PreparedRequest struct {
	AttemptID          string          `json:"attempt_id"`
	Suite              string          `json:"suite"`
	Case               string          `json:"case"`
	Trial              int             `json:"trial"`
	PromptVersion      string          `json:"prompt_version"`
	Prompt             string          `json:"prompt"`
	SchemaVersion      string          `json:"schema_version"`
	Schema             json.RawMessage `json:"schema"`
	Context            json.RawMessage `json:"context"`
	Media              []PreparedMedia `json:"media"`
	RequestFingerprint string          `json:"request_fingerprint"`
}

// ProviderResponse retains the raw provider envelope and the structured JSON
// text extracted from it. ReportedModel is checked against Descriptor.Model.
type ProviderResponse struct {
	Raw           []byte
	Output        json.RawMessage
	ReportedModel string
	RequestID     string
	RequestSHA256 string
}

type Finding struct {
	Category string `json:"category"`
	StartMS  *int64 `json:"start_ms,omitempty"`
	EndMS    *int64 `json:"end_ms,omitempty"`
	Evidence string `json:"evidence"`
	Impact   string `json:"impact"`
}

// Assessment is the normalized secondary review. ObservedOutcome never
// overrides the deterministic benchmark result retained in Request.Context.
type Assessment struct {
	MediaUsable             bool      `json:"media_usable"`
	ObservedOutcome         string    `json:"observed_outcome"`
	AgreesWithDeterministic bool      `json:"agrees_with_deterministic"`
	Confidence              float64   `json:"confidence"`
	Summary                 string    `json:"summary"`
	SignificantProblems     []Finding `json:"significant_problems"`
	MinorObservations       []Finding `json:"minor_observations"`
	Limitations             []string  `json:"limitations"`
}

type ContentIdentity struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// Record is the immutable provenance index for one successful model review.
// RawResponse and NormalizedOutput are returned separately so the bundle can
// retain them as create-only files and verify these digests.
type Record struct {
	Format                 string             `json:"format"`
	FormatVersion          int                `json:"format_version"`
	AttemptID              string             `json:"attempt_id"`
	Suite                  string             `json:"suite"`
	Case                   string             `json:"case"`
	Trial                  int                `json:"trial"`
	Provider               ProviderDescriptor `json:"provider"`
	Prompt                 ContentIdentity    `json:"prompt"`
	Schema                 ContentIdentity    `json:"schema"`
	ContextSHA256          string             `json:"context_sha256"`
	Media                  []Media            `json:"media"`
	RequestFingerprint     string             `json:"request_fingerprint"`
	ProviderRequestID      string             `json:"provider_request_id,omitempty"`
	ProviderRequestSHA256  string             `json:"provider_request_sha256"`
	ReportedModel          string             `json:"reported_model"`
	RawResponseSHA256      string             `json:"raw_response_sha256"`
	NormalizedOutputSHA256 string             `json:"normalized_output_sha256"`
	Assessment             Assessment         `json:"assessment"`
}

type Evaluation struct {
	Record           Record
	Prompt           []byte
	Schema           []byte
	Context          []byte
	RawResponse      []byte
	NormalizedOutput []byte
}

// Evaluate prepares a content-addressed request, invokes one provider plugin,
// validates its structured output strictly, and returns retention-ready bytes.
func Evaluate(ctx context.Context, provider Provider, request Request) (Evaluation, error) {
	if ctx == nil {
		return Evaluation{}, errors.New("review evaluation requires a context")
	}
	if err := context.Cause(ctx); err != nil {
		return Evaluation{}, err
	}
	if nilInterface(provider) {
		return Evaluation{}, errors.New("review evaluation requires a provider plugin")
	}
	descriptor := provider.Descriptor()
	if err := descriptor.Validate(); err != nil {
		return Evaluation{}, fmt.Errorf("review provider descriptor: %w", err)
	}
	prepared, err := Prepare(request)
	if err != nil {
		return Evaluation{}, err
	}
	response, err := provider.Review(ctx, clonePreparedRequest(prepared))
	if err != nil {
		return Evaluation{}, fmt.Errorf("review with %s/%s: %w", descriptor.Provider, descriptor.Model, err)
	}
	if after := provider.Descriptor(); after != descriptor {
		return Evaluation{}, errors.New("review provider descriptor changed during evaluation")
	}
	if len(response.Raw) == 0 || len(response.Raw) > maximumResponseBytes {
		return Evaluation{}, fmt.Errorf("review provider raw response must be 1..%d bytes", maximumResponseBytes)
	}
	if response.ReportedModel != descriptor.Model {
		return Evaluation{}, fmt.Errorf("review provider reported model %q, want pinned %q", response.ReportedModel, descriptor.Model)
	}
	if response.RequestID != "" {
		if err := validateMachineIdentifier("review provider request ID", response.RequestID, 4096); err != nil {
			return Evaluation{}, err
		}
	}
	if err := validateDigest(response.RequestSHA256); err != nil {
		return Evaluation{}, fmt.Errorf("review provider request: %w", err)
	}
	assessment, normalized, err := normalizeAssessment(response.Output)
	if err != nil {
		return Evaluation{}, fmt.Errorf("validate review provider output: %w", err)
	}
	promptDigest := digest([]byte(prepared.Prompt))
	schemaDigest := digest(prepared.Schema)
	contextDigest := digest(prepared.Context)
	rawDigest := digest(response.Raw)
	normalizedDigest := digest(normalized)
	media := make([]Media, len(prepared.Media))
	for index := range prepared.Media {
		media[index] = prepared.Media[index].Media
	}
	record := Record{
		Format: RecordFormat, FormatVersion: FormatVersion,
		AttemptID: prepared.AttemptID, Suite: prepared.Suite, Case: prepared.Case,
		Trial: prepared.Trial, Provider: descriptor,
		Prompt:        ContentIdentity{Version: prepared.PromptVersion, SHA256: promptDigest},
		Schema:        ContentIdentity{Version: prepared.SchemaVersion, SHA256: schemaDigest},
		ContextSHA256: contextDigest, Media: media,
		RequestFingerprint:    prepared.RequestFingerprint,
		ProviderRequestID:     response.RequestID,
		ProviderRequestSHA256: response.RequestSHA256, ReportedModel: descriptor.Model,
		RawResponseSHA256: rawDigest, NormalizedOutputSHA256: normalizedDigest,
		Assessment: assessment,
	}
	evaluation := Evaluation{
		Record: record, Prompt: []byte(prepared.Prompt), Schema: slices.Clone(prepared.Schema),
		Context: slices.Clone(prepared.Context), RawResponse: slices.Clone(response.Raw),
		NormalizedOutput: slices.Clone(normalized),
	}
	if err := VerifyArtifacts(
		evaluation.Record, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	); err != nil {
		return Evaluation{}, fmt.Errorf("construct review evaluation: %w", err)
	}
	return evaluation, nil
}

// Prepare snapshots and verifies all local evidence before external provider
// work. Symlinks, traversal, digest drift, duplicate artifacts, and oversized
// inputs fail closed.
func Prepare(request Request) (PreparedRequest, error) {
	if err := validateHumanIdentifier("review attempt ID", request.AttemptID, 1024); err != nil {
		return PreparedRequest{}, err
	}
	if err := validateHumanIdentifier("review suite", request.Suite, 1024); err != nil {
		return PreparedRequest{}, err
	}
	if err := validateHumanIdentifier("review case", request.Case, 1024); err != nil {
		return PreparedRequest{}, err
	}
	if request.Trial <= 0 {
		return PreparedRequest{}, errors.New("review trial must be positive")
	}
	contextJSON, err := canonicalJSON(request.Context, maximumContextBytes)
	if err != nil {
		return PreparedRequest{}, fmt.Errorf("review context: %w", err)
	}
	root, err := validateRoot(request.RootDirectory)
	if err != nil {
		return PreparedRequest{}, err
	}
	if len(request.Media) == 0 || len(request.Media) > maximumMediaCount {
		return PreparedRequest{}, fmt.Errorf(
			"review request needs 1..%d media artifacts", maximumMediaCount)
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return PreparedRequest{}, fmt.Errorf("open review root directory: %w", err)
	}
	defer rootHandle.Close()
	preparedMedia := make([]PreparedMedia, 0, len(request.Media))
	validatedMedia := make([]Media, 0, len(request.Media))
	seen := make(map[string]struct{}, len(request.Media))
	total := int64(0)
	for index, media := range request.Media {
		validated, payload, err := loadMedia(rootHandle, media)
		if err != nil {
			return PreparedRequest{}, fmt.Errorf("review media %d: %w", index, err)
		}
		if _, duplicate := seen[validated.Path]; duplicate {
			return PreparedRequest{}, fmt.Errorf("review media repeats path %q", validated.Path)
		}
		seen[validated.Path] = struct{}{}
		total += int64(len(payload))
		if total > maximumMediaBytes {
			return PreparedRequest{}, fmt.Errorf("review media exceeds %d total bytes", maximumMediaBytes)
		}
		preparedMedia = append(preparedMedia, PreparedMedia{Media: validated, Bytes: payload})
		validatedMedia = append(validatedMedia, validated)
	}

	schema := caseReviewSchema()
	contextEnvelope, err := json.Marshal(struct {
		Suite   string          `json:"suite"`
		Case    string          `json:"case"`
		Trial   int             `json:"trial"`
		Context json.RawMessage `json:"deterministic_context"`
		Media   []Media         `json:"media"`
	}{request.Suite, request.Case, request.Trial, contextJSON, validatedMedia})
	if err != nil {
		return PreparedRequest{}, fmt.Errorf("encode review prompt context: %w", err)
	}
	prompt := caseReviewPrompt(string(contextEnvelope))
	fingerprintSource, err := json.Marshal(struct {
		AttemptID     string  `json:"attempt_id"`
		PromptVersion string  `json:"prompt_version"`
		PromptSHA256  string  `json:"prompt_sha256"`
		SchemaVersion string  `json:"schema_version"`
		SchemaSHA256  string  `json:"schema_sha256"`
		ContextSHA256 string  `json:"context_sha256"`
		Media         []Media `json:"media"`
	}{
		request.AttemptID, CasePromptVersion, digest([]byte(prompt)),
		CaseSchemaVersion, digest(schema), digest(contextJSON), validatedMedia,
	})
	if err != nil {
		return PreparedRequest{}, fmt.Errorf("fingerprint review request: %w", err)
	}
	return PreparedRequest{
		AttemptID: request.AttemptID, Suite: request.Suite, Case: request.Case, Trial: request.Trial,
		PromptVersion: CasePromptVersion, Prompt: prompt,
		SchemaVersion: CaseSchemaVersion, Schema: schema, Context: contextJSON,
		Media: preparedMedia, RequestFingerprint: digest(fingerprintSource),
	}, nil
}

func (descriptor ProviderDescriptor) Validate() error {
	for _, field := range []struct{ label, value string }{
		{"provider", descriptor.Provider}, {"model", descriptor.Model},
		{"API", descriptor.API}, {"API revision", descriptor.APIRevision},
	} {
		if err := validateMachineIdentifier("review provider "+field.label, field.value, 256); err != nil {
			return err
		}
	}
	if err := validateDigest(descriptor.ConfigurationSHA256); err != nil {
		return fmt.Errorf("review provider configuration: %w", err)
	}
	return nil
}

func normalizeAssessment(source json.RawMessage) (Assessment, []byte, error) {
	if err := strictjson.Validate(source); err != nil {
		return Assessment{}, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	var wire struct {
		MediaUsable             *bool      `json:"media_usable"`
		ObservedOutcome         *string    `json:"observed_outcome"`
		AgreesWithDeterministic *bool      `json:"agrees_with_deterministic"`
		Confidence              *float64   `json:"confidence"`
		Summary                 *string    `json:"summary"`
		SignificantProblems     *[]Finding `json:"significant_problems"`
		MinorObservations       *[]Finding `json:"minor_observations"`
		Limitations             *[]string  `json:"limitations"`
	}
	if err := decoder.Decode(&wire); err != nil {
		return Assessment{}, nil, err
	}
	if wire.MediaUsable == nil || wire.ObservedOutcome == nil ||
		wire.AgreesWithDeterministic == nil || wire.Confidence == nil ||
		wire.Summary == nil || wire.SignificantProblems == nil ||
		wire.MinorObservations == nil || wire.Limitations == nil {
		return Assessment{}, nil, errors.New("review assessment is missing a required non-null field")
	}
	assessment := Assessment{
		MediaUsable: *wire.MediaUsable, ObservedOutcome: *wire.ObservedOutcome,
		AgreesWithDeterministic: *wire.AgreesWithDeterministic,
		Confidence:              *wire.Confidence, Summary: *wire.Summary,
		SignificantProblems: *wire.SignificantProblems,
		MinorObservations:   *wire.MinorObservations, Limitations: *wire.Limitations,
	}
	if assessment.ObservedOutcome != "pass" && assessment.ObservedOutcome != "fail" &&
		assessment.ObservedOutcome != "unclear" {
		return Assessment{}, nil, errors.New("observed_outcome must be pass, fail, or unclear")
	}
	if math.IsNaN(assessment.Confidence) || math.IsInf(assessment.Confidence, 0) ||
		assessment.Confidence < 0 || assessment.Confidence > 1 {
		return Assessment{}, nil, errors.New("confidence must be between zero and one")
	}
	if err := validateReviewText("summary", assessment.Summary, true, 16<<10); err != nil {
		return Assessment{}, nil, err
	}
	if len(assessment.SignificantProblems) > 128 || len(assessment.MinorObservations) > 128 ||
		len(assessment.Limitations) > 128 {
		return Assessment{}, nil, errors.New("review assessment has more than 128 findings or limitations")
	}
	for label, findings := range map[string][]Finding{
		"significant problem": assessment.SignificantProblems,
		"minor observation":   assessment.MinorObservations,
	} {
		for index, finding := range findings {
			if err := validateFinding(fmt.Sprintf("%s %d", label, index), finding); err != nil {
				return Assessment{}, nil, err
			}
		}
	}
	for index, limitation := range assessment.Limitations {
		if err := validateReviewText(fmt.Sprintf("limitation %d", index), limitation, true, 4096); err != nil {
			return Assessment{}, nil, err
		}
	}
	normalized, err := json.MarshalIndent(assessment, "", "  ")
	if err != nil {
		return Assessment{}, nil, err
	}
	return assessment, append(normalized, '\n'), nil
}

func validateFinding(label string, finding Finding) error {
	if err := validateMachineIdentifier(label+" category", finding.Category, 256); err != nil {
		return err
	}
	if err := validateReviewText(label+" evidence", finding.Evidence, true, 4096); err != nil {
		return err
	}
	if err := validateReviewText(label+" impact", finding.Impact, true, 4096); err != nil {
		return err
	}
	if (finding.StartMS != nil && *finding.StartMS < 0) ||
		(finding.EndMS != nil && *finding.EndMS < 0) {
		return fmt.Errorf("%s timestamps must not be negative", label)
	}
	if finding.StartMS != nil && finding.EndMS != nil && *finding.EndMS < *finding.StartMS {
		return fmt.Errorf("%s end_ms precedes start_ms", label)
	}
	return nil
}

func loadMedia(root *os.Root, media Media) (Media, []byte, error) {
	if err := validateMediaIdentity(media); err != nil {
		return Media{}, nil, err
	}
	if err := rejectRelativeSymlinks(root, media.Path); err != nil {
		return Media{}, nil, err
	}
	file, err := root.Open(media.Path)
	if err != nil {
		return Media{}, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Media{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return Media{}, nil, fmt.Errorf("media path %q is not a regular non-symlink file", media.Path)
	}
	if info.Size() < 1 || info.Size() > maximumMediaBytes {
		return Media{}, nil, fmt.Errorf("media path %q must be 1..%d bytes", media.Path, maximumMediaBytes)
	}
	reader := &io.LimitedReader{R: file, N: maximumMediaBytes + 1}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return Media{}, nil, err
	}
	if len(payload) < 1 || len(payload) > maximumMediaBytes {
		return Media{}, nil, fmt.Errorf(
			"media path %q changed outside the 1..%d byte limit", media.Path, maximumMediaBytes)
	}
	if got := digest(payload); got != media.SHA256 {
		return Media{}, nil, fmt.Errorf("media path %q digest is %s, want %s", media.Path, got, media.SHA256)
	}
	return media, payload, nil
}

func validateMediaIdentity(media Media) error {
	if media.Kind != "audio" && media.Kind != "video" && media.Kind != "image" {
		return fmt.Errorf("kind %q is not audio, video, or image", media.Kind)
	}
	if err := validateMachineIdentifier("media role", media.Role, 256); err != nil {
		return err
	}
	if err := validateMediaType(media.Kind, media.MediaType); err != nil {
		return err
	}
	if err := validateDigest(media.SHA256); err != nil {
		return err
	}
	if strings.TrimSpace(media.Path) == "" || len(media.Path) > maximumMediaPathBytes ||
		filepath.IsAbs(media.Path) || strings.Contains(media.Path, `\`) ||
		filepath.Clean(media.Path) != media.Path || media.Path == "." ||
		strings.HasPrefix(media.Path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("media path %q must be a clean relative path without traversal", media.Path)
	}
	return nil
}

func validateRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("review root directory must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect review root directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("review root directory must be a non-symlink directory")
	}
	return path, nil
}

func rejectRelativeSymlinks(root *os.Root, path string) error {
	current := ""
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if current == "" {
			current = component
		} else {
			current = filepath.Join(current, component)
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symlink", current)
		}
	}
	return nil
}

func canonicalJSON(source []byte, maximum int) ([]byte, error) {
	if len(source) == 0 || len(source) > maximum {
		return nil, fmt.Errorf("JSON must be 1..%d bytes", maximum)
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("JSON must be an object")
	}
	return json.Marshal(value)
}

func validateMediaType(kind, value string) error {
	if err := validateMachineIdentifier("media type", value, 256); err != nil {
		return err
	}
	if value != strings.ToLower(value) || strings.Contains(value, ";") {
		return errors.New("media type must be a lowercase type/subtype without parameters")
	}
	prefix := kind + "/"
	if !strings.HasPrefix(value, prefix) {
		return fmt.Errorf("media type %q does not match kind %q", value, kind)
	}
	return nil
}

func validateDigest(value string) error {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") ||
		value != strings.ToLower(value) {
		return errors.New("digest must be canonical SHA-256")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:")); err != nil {
		return errors.New("digest must be canonical SHA-256")
	}
	return nil
}

func validateHumanIdentifier(label, value string, maximum int) error {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		len(value) > maximum || !utf8.ValidString(value) {
		return fmt.Errorf("%s must be non-empty canonical UTF-8 no larger than %d bytes", label, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", label)
		}
	}
	return nil
}

func validateMachineIdentifier(label, value string, maximum int) error {
	if err := validateHumanIdentifier(label, value, maximum); err != nil {
		return err
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			return fmt.Errorf("%s contains whitespace", label)
		}
	}
	return nil
}

func validateReviewText(label, value string, required bool, maximum int) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("review %s is required", label)
	}
	if len(value) > maximum || !utf8.ValidString(value) {
		return fmt.Errorf("review %s must be valid UTF-8 no larger than %d bytes", label, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\t' {
			return fmt.Errorf("review %s contains unsupported control characters", label)
		}
	}
	return nil
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func clonePreparedRequest(source PreparedRequest) PreparedRequest {
	result := source
	result.Schema = slices.Clone(source.Schema)
	result.Context = slices.Clone(source.Context)
	result.Media = make([]PreparedMedia, len(source.Media))
	for index := range source.Media {
		result.Media[index] = source.Media[index]
		result.Media[index].Bytes = slices.Clone(source.Media[index].Bytes)
	}
	return result
}
