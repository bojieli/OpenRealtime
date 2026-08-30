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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	RecordFormat              = "openrealtime.multimodal-review"
	FormatVersion             = 5
	CasePromptVersion         = "openrealtime.case-media-review.prompt.v7"
	CaseSchemaVersion         = "openrealtime.case-media-review.schema.v3"
	SanitizationVersion       = "openrealtime.review-sanitization.v4"
	MediaValidationVersion    = "openrealtime.media-container-validation.v4"
	maximumContextBytes       = 4 << 20
	maximumMediaBytes         = 128 << 20
	maximumResponseBytes      = 8 << 20
	maximumMediaCount         = 256
	maximumMediaPathBytes     = 4096
	maximumSensitiveValues    = 256
	maximumSensitiveDepth     = 32
	maximumSensitiveQueue     = 65_536
	maximumSensitiveWork      = 512 << 20
	maximumSensitiveQueued    = 128 << 20
	maximumProviderErrorBytes = 64 << 10
	maximumFindingTimestampMS = int64(24 * 60 * 60 * 1000)

	ProviderRequestIDMissing = "missing"
	ProviderRequestIDNull    = "null"
	ProviderRequestIDValue   = "value"
)

// ProviderDescriptor is immutable review-provider provenance. APIRevision
// pins the wire contract separately from Model so a protocol migration cannot
// masquerade as another run of the same evaluator.
type ProviderDescriptor struct {
	Provider            string          `json:"provider"`
	Model               string          `json:"model"`
	API                 string          `json:"api"`
	APIRevision         string          `json:"api_revision"`
	Implementation      ContentIdentity `json:"implementation"`
	ConfigurationSHA256 string          `json:"configuration_sha256"`
	CapabilitiesSHA256  string          `json:"capabilities_sha256"`
}

// Provider is a replaceable offline evaluator plugin. Every method must be
// concurrency-safe, and returned errors must never contain credentials.
// Review receives only content-addressed media and public benchmark context;
// credentials remain private to the provider implementation.
type Provider interface {
	Descriptor() ProviderDescriptor
	Capabilities() ProviderCapabilities
	Implementation() []byte
	Configuration() []byte
	// Claim transfers exclusive lifetime ownership to one ProviderLease. It
	// must succeed at most once for a provider instance. On failure ownership
	// remains with the factory and Registry will not call Close.
	Claim() error
	Review(context.Context, PreparedRequest) (ProviderResponse, error)
	VerifyResponse(context.Context, PreparedRequest, ProviderResponse) error
	Close() error
}

// Media identifies one retained artifact within RootDirectory. Path is always
// a clean relative path; SHA256 is checked against the bytes before a provider
// can observe them.
type Media struct {
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	MediaType  string `json:"media_type"`
	Validation string `json:"validation"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
}

// Request is one deterministic benchmark attempt to review. Context must be a
// self-contained JSON object containing scorer outcomes, errors, transcripts,
// action traces, and any suite-specific criteria intended for the reviewer.
type Request struct {
	AttemptID string `json:"attempt_id"`
	Suite     string `json:"suite"`
	Case      string `json:"case"`
	Trial     int    `json:"trial"`
	// FindingTimestampMaximumMS is the inclusive end of the sealed media
	// timeline. Zero selects the contract-wide 24-hour ceiling for callers that
	// have no more precise, attested media duration.
	FindingTimestampMaximumMS int64           `json:"finding_timestamp_maximum_ms,omitempty"`
	RootDirectory             string          `json:"-"`
	Context                   json.RawMessage `json:"context"`
	Media                     []Media         `json:"media"`
	// SensitiveValues are exact in-memory secrets that must not occur in the
	// public context or retained media. Values are checked before a provider
	// can observe bytes and are never serialized or fingerprinted themselves.
	SensitiveValues []string `json:"-"`
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
	AttemptID                 string          `json:"attempt_id"`
	Suite                     string          `json:"suite"`
	Case                      string          `json:"case"`
	Trial                     int             `json:"trial"`
	FindingTimestampMaximumMS int64           `json:"finding_timestamp_maximum_ms"`
	PromptVersion             string          `json:"prompt_version"`
	Prompt                    string          `json:"prompt"`
	SchemaVersion             string          `json:"schema_version"`
	Schema                    json.RawMessage `json:"schema"`
	Context                   json.RawMessage `json:"context"`
	Media                     []PreparedMedia `json:"media"`
	RequestFingerprint        string          `json:"request_fingerprint"`
	Sanitization              string          `json:"sanitization"`
	SensitiveValueCount       int             `json:"sensitive_value_count"`
	sensitiveGuard            *declaredSensitiveGuard
	validationSeal            *preparedValidationSeal
}

// declaredSensitiveGuard is an opaque, immutable capability. The closure owns
// the only copy retained after PrepareContext returns and reveals only a
// boolean containment result. Its formatters deliberately expose no captured
// bytes if a provider logs or reflects a PreparedRequest.
type declaredSensitiveGuard struct {
	count   int
	matcher *sensitiveMatcher
}

func newDeclaredSensitiveGuard(
	ctx context.Context, values [][]byte,
) (*declaredSensitiveGuard, error) {
	matcher, err := newSensitiveMatcherContext(ctx, values)
	if err != nil {
		return nil, err
	}
	return &declaredSensitiveGuard{count: len(values), matcher: matcher}, nil
}

func (guard *declaredSensitiveGuard) has(payload []byte) bool {
	matched, err := guard.hasContext(context.Background(), payload)
	return matched || err != nil
}

func (guard *declaredSensitiveGuard) hasContext(
	ctx context.Context, payload []byte,
) (bool, error) {
	if guard == nil {
		if ctx == nil {
			return false, errors.New("declared-sensitive scan requires a context")
		}
		return false, ctx.Err()
	}
	return secretInJSONContext(ctx, payload, guard.matcher)
}

func (guard *declaredSensitiveGuard) hasLiteral(payload []byte) bool {
	return guard != nil && guard.matcher != nil && guard.matcher.contains(payload)
}

func (guard *declaredSensitiveGuard) String() string {
	if guard == nil {
		return "<nil declared-sensitive guard>"
	}
	return fmt.Sprintf("<opaque declared-sensitive guard count=%d>", guard.count)
}

func (guard *declaredSensitiveGuard) GoString() string { return guard.String() }

// redactedReviewFailure deliberately has a message shorter than the minimum
// accepted declared secret, so replacing an unsafe error cannot reproduce the
// same secret. It preserves only standard cancellation classification and
// retains no provider-controlled cause or text.
type redactedReviewFailure struct {
	canceled bool
	deadline bool
}

func (failure redactedReviewFailure) Error() string { return "failed" }

func (failure redactedReviewFailure) Is(target error) bool {
	return (failure.canceled && target == context.Canceled) ||
		(failure.deadline && target == context.DeadlineExceeded)
}

func newRedactedReviewFailure(cause error) error {
	return redactedReviewFailure{
		canceled: errors.Is(cause, context.Canceled),
		deadline: errors.Is(cause, context.DeadlineExceeded),
	}
}

// ProviderResponse retains the raw provider envelope and the structured JSON
// text extracted from it. ReportedModel is checked against Descriptor.Model.
type ProviderResponse struct {
	Raw            []byte
	Output         json.RawMessage
	ReportedModel  string
	RequestID      string
	RequestIDState string
	Request        []byte
	// VerificationEvidence is a provider-private, non-retained capability that
	// can bind one transport exchange to VerifyResponse. Core treats it as
	// opaque and copies only the interface value between the two calls.
	VerificationEvidence any
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
	Format                    string               `json:"format"`
	FormatVersion             int                  `json:"format_version"`
	AttemptID                 string               `json:"attempt_id"`
	Suite                     string               `json:"suite"`
	Case                      string               `json:"case"`
	Trial                     int                  `json:"trial"`
	FindingTimestampMaximumMS int64                `json:"finding_timestamp_maximum_ms"`
	Provider                  ProviderDescriptor   `json:"provider"`
	ProviderCapabilities      ProviderCapabilities `json:"provider_capabilities"`
	Prompt                    ContentIdentity      `json:"prompt"`
	Schema                    ContentIdentity      `json:"schema"`
	ContextSHA256             string               `json:"context_sha256"`
	Media                     []Media              `json:"media"`
	RequestFingerprint        string               `json:"request_fingerprint"`
	Sanitization              string               `json:"sanitization"`
	SensitiveValueCount       int                  `json:"sensitive_value_count"`
	ProviderRequestID         string               `json:"provider_request_id,omitempty"`
	ProviderRequestIDState    string               `json:"provider_request_id_state"`
	ProviderRequestSHA256     string               `json:"provider_request_sha256"`
	ReportedModel             string               `json:"reported_model"`
	RawResponseSHA256         string               `json:"raw_response_sha256"`
	NormalizedOutputSHA256    string               `json:"normalized_output_sha256"`
	Assessment                Assessment           `json:"assessment"`
}

type Evaluation struct {
	Record Record
	// Media owns the exact immutable media bytes passed to the provider. The
	// paths in Record identify the source artifacts, while this snapshot lets a
	// create-only retention layer preserve what was actually reviewed even if
	// those source paths later change.
	Media                  []PreparedMedia
	ProviderImplementation []byte
	ProviderConfiguration  []byte
	ProviderRequest        []byte
	Prompt                 []byte
	Schema                 []byte
	Context                []byte
	RawResponse            []byte
	NormalizedOutput       []byte
	// retentionSeal is an opaque, one-use capability minted only after the
	// provider exchange and every retention byte have passed validation. Public
	// copies share the capability, so at most one exact snapshot can be
	// published as provider-verified evidence.
	retentionSeal *evaluationRetentionSeal
}

// Evaluate prepares a content-addressed request, invokes one provider plugin,
// validates its structured output strictly, and returns retention-ready bytes.
func Evaluate(
	ctx context.Context, lease *ProviderLease, request Request,
) (evaluation Evaluation, resultErr error) {
	var prepared PreparedRequest
	defer func() {
		if ctx != nil {
			if cause := ctx.Err(); cause != nil {
				evaluation = Evaluation{}
				if resultErr == nil || !errors.Is(resultErr, cause) {
					resultErr = cause
				}
			}
		}
		if resultErr != nil && prepared.ContainsDeclaredSensitiveValue([]byte(resultErr.Error())) {
			resultErr = newRedactedReviewFailure(resultErr)
		}
	}()
	var err error
	prepared, err = PrepareContext(ctx, request)
	if err != nil {
		return Evaluation{}, err
	}
	if lease == nil {
		return Evaluation{}, errors.New("review evaluation requires an opened provider lease")
	}
	if err := lease.validateCapabilities(prepared.Media); err != nil {
		return Evaluation{}, err
	}
	capabilities := lease.Capabilities()
	if err := capabilities.Validate(); err != nil {
		return Evaluation{}, errors.New("review provider lease capabilities are invalid")
	}
	descriptor, implementation, configuration, err := lease.provenance()
	if err != nil {
		return Evaluation{}, err
	}
	descriptorJSON, err := marshalCanonicalCompact(descriptor, maximumSchemaBytes)
	if err != nil || prepared.ContainsDeclaredSensitiveValue(descriptorJSON) ||
		prepared.ContainsDeclaredSensitiveValue(implementation) ||
		prepared.ContainsDeclaredSensitiveValue(configuration) {
		return Evaluation{}, errors.New("review provider provenance contains a declared sensitive value")
	}
	provider, err := lease.acquire()
	if err != nil {
		return Evaluation{}, err
	}
	defer lease.release()
	if err := verifyLiveProviderIdentity(
		provider, descriptor, capabilities, implementation, configuration, "before evaluation",
	); err != nil {
		return Evaluation{}, err
	}
	if cause := ctx.Err(); cause != nil {
		return Evaluation{}, cause
	}
	response, err := provider.Review(ctx, clonePreparedRequest(prepared))
	if cause := ctx.Err(); cause != nil {
		// A provider error is untrusted and may contain a credential or another
		// declared secret. Cancellation is the complete canonical outcome.
		return Evaluation{}, cause
	}
	if err != nil {
		return Evaluation{}, guardedProviderError(prepared, "review provider invocation", err)
	}
	if err := preflightProviderResponse(response); err != nil {
		return Evaluation{}, err
	}
	// The provider owns the returned buffers. Snapshot them immediately after
	// scalar admission and use only the owned copy for every subsequent check,
	// digest, verification call, and retained artifact.
	response = cloneProviderResponse(response)
	if err := verifyLiveProviderIdentity(
		provider, descriptor, capabilities, implementation, configuration, "during evaluation",
	); err != nil {
		return Evaluation{}, err
	}
	for _, payload := range [][]byte{
		response.Request, response.Raw, response.Output, []byte(response.RequestID),
		[]byte(response.RequestIDState), []byte(response.ReportedModel),
	} {
		if prepared.ContainsDeclaredSensitiveValue(payload) {
			return Evaluation{}, errors.New("review provider exchange contains a declared sensitive value")
		}
	}
	if response.ReportedModel != descriptor.Model {
		return Evaluation{}, errors.New("review provider reported model differs from the pinned model")
	}
	switch response.RequestIDState {
	case ProviderRequestIDValue:
		if err := validateMachineIdentifier("review provider request ID", response.RequestID, 4096); err != nil {
			return Evaluation{}, err
		}
	case ProviderRequestIDMissing, ProviderRequestIDNull:
		if response.RequestID != "" {
			return Evaluation{}, errors.New("review provider request ID state disagrees with its value")
		}
	default:
		return Evaluation{}, errors.New("review provider request ID state is invalid")
	}
	if cause := ctx.Err(); cause != nil {
		return Evaluation{}, cause
	}
	verifyErr := provider.VerifyResponse(
		ctx, clonePreparedRequest(prepared), cloneProviderResponse(response),
	)
	if cause := ctx.Err(); cause != nil {
		return Evaluation{}, cause
	}
	if verifyErr != nil {
		return Evaluation{}, guardedProviderError(
			prepared, "verify review provider exchange", verifyErr)
	}
	if err := verifyLiveProviderIdentity(
		provider, descriptor, capabilities, implementation, configuration, "during response verification",
	); err != nil {
		return Evaluation{}, err
	}
	if cause := ctx.Err(); cause != nil {
		return Evaluation{}, cause
	}
	assessment, normalized, err := normalizeAssessment(response.Output)
	if err != nil {
		return Evaluation{}, fmt.Errorf("validate review provider output: %w", err)
	}
	if err := validateAssessmentTimestampMaximum(
		assessment, prepared.FindingTimestampMaximumMS,
	); err != nil {
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
		FindingTimestampMaximumMS: prepared.FindingTimestampMaximumMS,
		ProviderCapabilities:      capabilities.Clone(),
		Prompt:                    ContentIdentity{Version: prepared.PromptVersion, SHA256: promptDigest},
		Schema:                    ContentIdentity{Version: prepared.SchemaVersion, SHA256: schemaDigest},
		ContextSHA256:             contextDigest, Media: media,
		RequestFingerprint: prepared.RequestFingerprint,
		Sanitization:       prepared.Sanitization, SensitiveValueCount: prepared.SensitiveValueCount,
		ProviderRequestID: response.RequestID, ProviderRequestIDState: response.RequestIDState,
		ProviderRequestSHA256: digest(response.Request), ReportedModel: descriptor.Model,
		RawResponseSHA256: rawDigest, NormalizedOutputSHA256: normalizedDigest,
		Assessment: assessment,
	}
	evaluation = Evaluation{
		Record: record, Media: clonePreparedRequest(prepared).Media,
		ProviderImplementation: implementation,
		ProviderConfiguration:  configuration, ProviderRequest: slices.Clone(response.Request),
		Prompt: []byte(prepared.Prompt), Schema: slices.Clone(prepared.Schema),
		Context: slices.Clone(prepared.Context), RawResponse: slices.Clone(response.Raw),
		NormalizedOutput: slices.Clone(normalized),
	}
	if err := VerifyArtifactsContext(
		ctx, evaluation.Record, evaluation.ProviderImplementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	); err != nil {
		return Evaluation{}, fmt.Errorf("construct review evaluation: %w", err)
	}
	recordJSON, err := MarshalRecord(evaluation.Record)
	if err != nil {
		return Evaluation{}, fmt.Errorf("construct review record: %w", err)
	}
	for _, artifact := range [][]byte{
		recordJSON, evaluation.ProviderImplementation, evaluation.ProviderConfiguration,
		evaluation.ProviderRequest, evaluation.Prompt, evaluation.Schema, evaluation.Context,
		evaluation.RawResponse, evaluation.NormalizedOutput,
	} {
		if prepared.ContainsDeclaredSensitiveValue(artifact) {
			return Evaluation{}, errors.New("retained review evaluation contains a declared sensitive value")
		}
	}
	for index := range evaluation.Media {
		metadata, err := marshalCanonicalCompact(
			evaluation.Media[index].Media, maximumPreparedPublicBytes,
		)
		if err != nil {
			return Evaluation{}, errors.New("retained review media metadata is invalid")
		}
		metadataSensitive, scanErr := prepared.ContainsDeclaredSensitiveValueContext(ctx, metadata)
		if scanErr != nil {
			return Evaluation{}, scanErr
		}
		mediaSensitive, scanErr := prepared.sensitiveGuard.matcher.containsContext(
			ctx, evaluation.Media[index].Bytes,
		)
		if scanErr != nil {
			return Evaluation{}, scanErr
		}
		if metadataSensitive || mediaSensitive {
			return Evaluation{}, errors.New("retained review media contains a declared sensitive value")
		}
	}
	if cause := ctx.Err(); cause != nil {
		return Evaluation{}, cause
	}
	evaluation.retentionSeal, err = newEvaluationRetentionSealContext(
		ctx, evaluation, prepared.sensitiveGuard,
	)
	if err != nil {
		return Evaluation{}, fmt.Errorf("seal review evaluation for retention: %w", err)
	}
	return evaluation, nil
}

func guardedProviderError(prepared PreparedRequest, operation string, err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if len(message) > maximumProviderErrorBytes || !utf8.ValidString(message) ||
		containsControl(message) {
		return newRedactedReviewFailure(err)
	}
	if prepared.ContainsDeclaredSensitiveValue([]byte(message)) {
		return newRedactedReviewFailure(err)
	}
	// Do not retain or wrap the provider-controlled error: Error methods can be
	// mutable and a later formatting call could expand or reveal different
	// content. Preserve only this admitted bounded snapshot.
	return errors.New(operation + ": " + message)
}

func verifyLiveProviderIdentity(
	provider Provider, descriptor ProviderDescriptor, capabilities ProviderCapabilities,
	implementation, configuration []byte,
	phase string,
) error {
	if nilInterface(provider) {
		return errors.New("review provider is nil")
	}
	if current := provider.Descriptor(); current != descriptor {
		return fmt.Errorf("review provider descriptor changed %s", phase)
	}
	liveCapabilities := provider.Capabilities()
	if err := liveCapabilities.Validate(); err != nil || !reflect.DeepEqual(liveCapabilities, capabilities) {
		return fmt.Errorf("review provider capabilities changed %s", phase)
	}
	liveImplementation := provider.Implementation()
	if len(liveImplementation) != len(implementation) ||
		len(liveImplementation) > maximumProviderImplementationBytes ||
		!bytes.Equal(liveImplementation, implementation) ||
		digest(implementation) != descriptor.Implementation.SHA256 {
		return fmt.Errorf("review provider implementation changed %s", phase)
	}
	liveConfiguration := provider.Configuration()
	if len(liveConfiguration) != len(configuration) ||
		len(liveConfiguration) > maximumProviderConfigurationBytes ||
		!bytes.Equal(liveConfiguration, configuration) ||
		digest(configuration) != descriptor.ConfigurationSHA256 {
		return fmt.Errorf("review provider configuration changed %s", phase)
	}
	return nil
}

// Prepare snapshots and verifies all local evidence before external provider
// work. Symlinks, traversal, digest drift, duplicate artifacts, and oversized
// inputs fail closed.
func Prepare(request Request) (PreparedRequest, error) {
	return PrepareContext(context.Background(), request)
}

// CanonicalContextSHA256 returns the same content identity Evaluate records
// for a public review context, without opening or validating any media. This
// lets retention and aggregation plug-ins rebind an already verified source
// request without repeating expensive evidence reads. The input must still be
// one strict, bounded JSON object.
func CanonicalContextSHA256(
	ctx context.Context, source json.RawMessage,
) (string, error) {
	canonical, err := canonicalJSONContext(ctx, source, maximumContextBytes)
	if err != nil {
		return "", fmt.Errorf("canonical review context: %w", err)
	}
	return digestContext(ctx, canonical)
}

// PrepareContext is Prepare with cancellation checks before and during every
// potentially large evidence read.
func PrepareContext(
	ctx context.Context, request Request,
) (result PreparedRequest, resultErr error) {
	var guard *declaredSensitiveGuard
	defer func() {
		if ctx != nil {
			if cause := ctx.Err(); cause != nil {
				result = PreparedRequest{}
				// Cancellation can occur before the declared-secret matcher has
				// been compiled. Preserve errors.Is semantics without exposing the
				// canonical error text, which may itself be a declared secret.
				resultErr = newRedactedReviewFailure(cause)
			}
		}
		if resultErr != nil && guard != nil && guard.has([]byte(resultErr.Error())) {
			resultErr = newRedactedReviewFailure(resultErr)
		}
	}()
	if ctx == nil {
		return PreparedRequest{}, errors.New("prepare review request: nil context")
	}
	if cause := ctx.Err(); cause != nil {
		return PreparedRequest{}, newRedactedReviewFailure(cause)
	}
	secrets, err := canonicalSensitiveValues(request.SensitiveValues)
	if err != nil {
		return PreparedRequest{}, err
	}
	if cause := ctx.Err(); cause != nil {
		return PreparedRequest{}, newRedactedReviewFailure(cause)
	}
	guard, err = newDeclaredSensitiveGuard(ctx, secrets)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return PreparedRequest{}, newRedactedReviewFailure(cause)
		}
		return PreparedRequest{}, err
	}
	if cause := ctx.Err(); cause != nil {
		return PreparedRequest{}, cause
	}
	// Install the opaque secret guard before any validation that can mention a
	// request-controlled identity or media scalar in its diagnostic.
	if err := preflightRequest(request); err != nil {
		return PreparedRequest{}, err
	}
	findingTimestampMaximumMS := request.FindingTimestampMaximumMS
	if findingTimestampMaximumMS == 0 {
		findingTimestampMaximumMS = maximumFindingTimestampMS
	}
	if err := validateHumanIdentifier("review attempt ID", request.AttemptID, 1024); err != nil {
		return PreparedRequest{}, err
	}
	if err := validateHumanIdentifier("review suite", request.Suite, 1024); err != nil {
		return PreparedRequest{}, err
	}
	if err := validateHumanIdentifier("review case", request.Case, 1024); err != nil {
		return PreparedRequest{}, err
	}
	containsSensitive, scanErr := guard.hasContext(ctx, request.Context)
	if scanErr != nil {
		return PreparedRequest{}, scanErr
	}
	if containsSensitive {
		return PreparedRequest{}, errors.New("review context contains a declared sensitive value")
	}
	contextJSON, err := canonicalJSONContext(ctx, request.Context, maximumContextBytes)
	if err != nil {
		if guard.has([]byte(err.Error())) {
			return PreparedRequest{}, errors.New("review context is invalid and contains a declared sensitive value")
		}
		return PreparedRequest{}, fmt.Errorf("review context: %w", err)
	}
	containsSensitive, scanErr = guard.hasContext(ctx, contextJSON)
	if scanErr != nil {
		return PreparedRequest{}, scanErr
	}
	if containsSensitive {
		return PreparedRequest{}, errors.New("review context contains a declared sensitive value")
	}
	for label, value := range map[string]string{
		"attempt ID": request.AttemptID, "suite": request.Suite, "case": request.Case,
		"root directory": request.RootDirectory,
	} {
		if guard.has([]byte(value)) {
			return PreparedRequest{}, fmt.Errorf("review %s contains a declared sensitive value", label)
		}
	}
	for index, media := range request.Media {
		for label, value := range map[string]string{
			"kind": media.Kind, "role": media.Role, "path": media.Path,
			"media type": media.MediaType, "digest": media.SHA256,
			"validation": media.Validation,
		} {
			if guard.has([]byte(value)) {
				return PreparedRequest{}, fmt.Errorf(
					"review media %d %s contains a declared sensitive value", index, label)
			}
		}
	}
	rootHandle, err := openValidatedRoot(request.RootDirectory, nil)
	if err != nil {
		return PreparedRequest{}, err
	}
	defer rootHandle.Close()
	type mediaMetadata struct {
		media Media
		size  int64
	}
	metadata := make([]mediaMetadata, len(request.Media))
	total := int64(0)
	for index, media := range request.Media {
		file, info, openErr := openRegularNoSymlink(rootHandle, media.Path, nil)
		if openErr != nil {
			return PreparedRequest{}, fmt.Errorf("review media %d: %w", index, openErr)
		}
		closeErr := file.Close()
		if closeErr != nil {
			return PreparedRequest{}, fmt.Errorf("review media %d: close metadata handle", index)
		}
		if info.Size() < 1 || info.Size() > maximumMediaBytes {
			return PreparedRequest{}, fmt.Errorf(
				"review media %d path must be 1..%d bytes", index, maximumMediaBytes)
		}
		if total > maximumMediaBytes-info.Size() {
			return PreparedRequest{}, fmt.Errorf("review media exceeds %d total bytes", maximumMediaBytes)
		}
		total += info.Size()
		metadata[index] = mediaMetadata{media: media, size: info.Size()}
	}
	preparedMedia := make([]PreparedMedia, 0, len(metadata))
	validatedMedia := make([]Media, 0, len(metadata))
	remaining := int64(maximumMediaBytes)
	for index, item := range metadata {
		validated, payload, loadErr := loadMediaContext(
			ctx, rootHandle, item.media, item.size, remaining, nil,
		)
		if loadErr != nil {
			return PreparedRequest{}, fmt.Errorf("review media %d: %w", index, loadErr)
		}
		remaining -= int64(len(payload))
		containsSensitive, scanErr = guard.matcher.containsContext(ctx, payload)
		if scanErr != nil {
			return PreparedRequest{}, scanErr
		}
		if containsSensitive {
			return PreparedRequest{}, fmt.Errorf("review media %d contains a declared sensitive value", index)
		}
		preparedMedia = append(preparedMedia, PreparedMedia{Media: validated, Bytes: payload})
		validatedMedia = append(validatedMedia, validated)
	}

	schema := caseReviewSchema()
	contextEnvelope, err := marshalCanonicalCompact(struct {
		Suite                     string          `json:"suite"`
		Case                      string          `json:"case"`
		Trial                     int             `json:"trial"`
		FindingTimestampMaximumMS int64           `json:"finding_timestamp_maximum_ms"`
		Context                   json.RawMessage `json:"deterministic_context"`
		Media                     []Media         `json:"media"`
	}{
		request.Suite, request.Case, request.Trial, findingTimestampMaximumMS,
		contextJSON, validatedMedia,
	}, maximumPromptBytes)
	if err != nil {
		return PreparedRequest{}, fmt.Errorf("encode review prompt context: %w", err)
	}
	prompt := caseReviewPrompt(string(contextEnvelope))
	if len(prompt) == 0 || len(prompt) > maximumPromptBytes {
		return PreparedRequest{}, fmt.Errorf("review prompt exceeds %d bytes", maximumPromptBytes)
	}
	// The prompt is the fixed, trusted instruction prefix followed by the exact
	// contextEnvelope scanned below. Scan the assembled wire text literally so
	// a raw secret or future formatter drift is still rejected, but do not
	// recursively decode the same large, nested JSON a second time. Replaying a
	// valid envelope through the embedded-JSON scanner can exhaust its bounded
	// work budget and falsely classify ordinary retained evidence as sensitive.
	promptSensitive, scanErr := guard.matcher.containsContext(ctx, []byte(prompt))
	if scanErr != nil {
		return PreparedRequest{}, scanErr
	}
	envelopeSensitive, scanErr := guard.hasContext(ctx, contextEnvelope)
	if scanErr != nil {
		return PreparedRequest{}, scanErr
	}
	if promptSensitive || envelopeSensitive {
		return PreparedRequest{}, errors.New("review prompt envelope contains a declared sensitive value")
	}
	prepared := PreparedRequest{
		AttemptID: request.AttemptID, Suite: request.Suite, Case: request.Case, Trial: request.Trial,
		FindingTimestampMaximumMS: findingTimestampMaximumMS,
		PromptVersion:             CasePromptVersion, Prompt: prompt,
		SchemaVersion: CaseSchemaVersion, Schema: schema, Context: contextJSON,
		Media: preparedMedia, Sanitization: SanitizationVersion,
		SensitiveValueCount: len(secrets),
		sensitiveGuard:      guard,
	}
	prepared.RequestFingerprint, err = prepared.fingerprintContext(ctx)
	if err != nil {
		return PreparedRequest{}, err
	}
	prepared.validationSeal, err = sealPreparedContext(ctx, prepared)
	if err != nil {
		return PreparedRequest{}, err
	}
	if err := prepared.ValidateContext(ctx); err != nil {
		return PreparedRequest{}, fmt.Errorf("prepare review request: %w", err)
	}
	return prepared, nil
}

// Validate recomputes the complete standard provider input contract. Exported
// providers must call it so a syntactically valid but forged PreparedRequest
// cannot bypass PrepareContext.
func (prepared PreparedRequest) Validate() (resultErr error) {
	return prepared.ValidateContext(context.Background())
}

// ValidateContext is Validate with cooperative cancellation checkpoints for
// every large hash and declared-sensitive-value scan.
func (prepared PreparedRequest) ValidateContext(ctx context.Context) (resultErr error) {
	defer func() {
		if resultErr != nil && prepared.sensitiveGuard != nil &&
			prepared.sensitiveGuard.has([]byte(resultErr.Error())) {
			resultErr = newRedactedReviewFailure(resultErr)
		}
	}()
	if ctx == nil {
		return errors.New("prepared review validation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := preflightPrepared(prepared, true); err != nil {
		return err
	}
	if err := validateHumanIdentifier("prepared review attempt ID", prepared.AttemptID, 1024); err != nil {
		return err
	}
	if err := validateHumanIdentifier("prepared review suite", prepared.Suite, 1024); err != nil {
		return err
	}
	if err := validateHumanIdentifier("prepared review case", prepared.Case, 1024); err != nil {
		return err
	}
	if prepared.Trial <= 0 || prepared.PromptVersion != CasePromptVersion ||
		prepared.SchemaVersion != CaseSchemaVersion || prepared.Sanitization != SanitizationVersion ||
		prepared.SensitiveValueCount < 0 || prepared.sensitiveGuard == nil ||
		prepared.sensitiveGuard.matcher == nil ||
		prepared.sensitiveGuard.count != prepared.SensitiveValueCount {
		return errors.New("prepared review has invalid trial, version, or sanitization metadata")
	}
	if err := validatePreparedSealContext(ctx, prepared); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	publicJSON, err := marshalCanonicalCompact(prepared, maximumPreparedPublicBytes)
	if err != nil {
		return errors.New("prepared review cannot encode its public contract")
	}
	// The public encoding repeats Context both directly and inside Prompt. Scan
	// the exact wire representation literally here; a canonical validation
	// envelope below performs the recursive structured scan with each untrusted
	// field represented once.
	contains, err := prepared.sensitiveGuard.matcher.containsContext(ctx, publicJSON)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("prepared review public contract contains a declared sensitive value")
	}
	for _, item := range prepared.Media {
		contains, err := prepared.sensitiveGuard.matcher.containsContext(ctx, item.Bytes)
		if err != nil {
			return err
		}
		if contains {
			return errors.New("prepared review media contains a declared sensitive value")
		}
	}
	contextJSON, err := canonicalJSONContext(ctx, prepared.Context, maximumContextBytes)
	if err != nil || !bytes.Equal(contextJSON, prepared.Context) {
		return errors.New("prepared review context is not canonical JSON")
	}
	if !bytes.Equal(prepared.Schema, caseReviewSchema()) {
		return errors.New("prepared review schema differs from the standard schema version")
	}
	media := make([]Media, len(prepared.Media))
	for index, item := range prepared.Media {
		media[index] = item.Media
	}
	contextEnvelope, err := marshalCanonicalCompact(struct {
		Suite                     string          `json:"suite"`
		Case                      string          `json:"case"`
		Trial                     int             `json:"trial"`
		FindingTimestampMaximumMS int64           `json:"finding_timestamp_maximum_ms"`
		Context                   json.RawMessage `json:"deterministic_context"`
		Media                     []Media         `json:"media"`
	}{
		prepared.Suite, prepared.Case, prepared.Trial, prepared.FindingTimestampMaximumMS,
		prepared.Context, media,
	}, maximumPromptBytes)
	if err != nil || prepared.Prompt != caseReviewPrompt(string(contextEnvelope)) {
		return errors.New("prepared review prompt differs from its context or media manifest")
	}
	validationEnvelope, err := marshalCanonicalCompact(struct {
		AttemptID           string          `json:"attempt_id"`
		PromptVersion       string          `json:"prompt_version"`
		SchemaVersion       string          `json:"schema_version"`
		Schema              json.RawMessage `json:"schema"`
		PromptContext       json.RawMessage `json:"prompt_context"`
		RequestFingerprint  string          `json:"request_fingerprint"`
		Sanitization        string          `json:"sanitization"`
		SensitiveValueCount int             `json:"sensitive_value_count"`
	}{
		prepared.AttemptID, prepared.PromptVersion, prepared.SchemaVersion, prepared.Schema,
		contextEnvelope, prepared.RequestFingerprint, prepared.Sanitization,
		prepared.SensitiveValueCount,
	}, maximumPreparedPublicBytes)
	if err != nil {
		return errors.New("prepared review cannot encode its validation envelope")
	}
	contains, err = prepared.ContainsDeclaredSensitiveValueContext(ctx, validationEnvelope)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("prepared review public contract contains a declared sensitive value")
	}
	want, err := prepared.fingerprintContext(ctx)
	if err != nil {
		return err
	}
	if prepared.RequestFingerprint != want {
		return errors.New("prepared review request fingerprint differs from its inputs")
	}
	contains, err = prepared.ContainsDeclaredSensitiveValueContext(ctx, []byte(want))
	if err != nil {
		return err
	}
	if contains {
		return errors.New("prepared review fingerprint contains a declared sensitive value")
	}
	return ctx.Err()
}

// ContainsDeclaredSensitiveValue checks final encoded provider bytes against
// the opaque guard created by PrepareContext. The guarded values are never
// serialized, fingerprinted, or exposed directly. Provider implementations
// must call this on the final wire payload before any transport side effect.
func (prepared PreparedRequest) ContainsDeclaredSensitiveValue(payload []byte) bool {
	return prepared.sensitiveGuard.has(payload)
}

// ContainsDeclaredSensitiveValueContext is the cancellable form of
// ContainsDeclaredSensitiveValue for provider work over bounded large inputs.
func (prepared PreparedRequest) ContainsDeclaredSensitiveValueContext(
	ctx context.Context, payload []byte,
) (bool, error) {
	return prepared.sensitiveGuard.hasContext(ctx, payload)
}

func (prepared PreparedRequest) fingerprint() (string, error) {
	return prepared.fingerprintContext(context.Background())
}

func (prepared PreparedRequest) fingerprintContext(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", errors.New("review fingerprint requires a context")
	}
	promptDigest, err := digestContext(ctx, []byte(prepared.Prompt))
	if err != nil {
		return "", err
	}
	schemaDigest, err := digestContext(ctx, prepared.Schema)
	if err != nil {
		return "", err
	}
	contextDigest, err := digestContext(ctx, prepared.Context)
	if err != nil {
		return "", err
	}
	media := make([]Media, len(prepared.Media))
	for index := range prepared.Media {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		media[index] = prepared.Media[index].Media
	}
	source, err := marshalCanonicalCompact(struct {
		AttemptID                 string  `json:"attempt_id"`
		PromptVersion             string  `json:"prompt_version"`
		PromptSHA256              string  `json:"prompt_sha256"`
		SchemaVersion             string  `json:"schema_version"`
		SchemaSHA256              string  `json:"schema_sha256"`
		ContextSHA256             string  `json:"context_sha256"`
		Media                     []Media `json:"media"`
		FindingTimestampMaximumMS int64   `json:"finding_timestamp_maximum_ms"`
		Sanitization              string  `json:"sanitization"`
		SensitiveValueCount       int     `json:"sensitive_value_count"`
	}{
		prepared.AttemptID, prepared.PromptVersion, promptDigest,
		prepared.SchemaVersion, schemaDigest, contextDigest, media,
		prepared.FindingTimestampMaximumMS, prepared.Sanitization,
		prepared.SensitiveValueCount,
	}, maximumPreparedPublicBytes)
	if err != nil {
		return "", fmt.Errorf("fingerprint review request: %w", err)
	}
	return digestContext(ctx, source)
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
	if err := validateMachineIdentifier(
		"review provider implementation version", descriptor.Implementation.Version, 256); err != nil {
		return err
	}
	if err := validateDigest(descriptor.Implementation.SHA256); err != nil {
		return fmt.Errorf("review provider implementation: %w", err)
	}
	if err := validateDigest(descriptor.ConfigurationSHA256); err != nil {
		return fmt.Errorf("review provider configuration: %w", err)
	}
	if err := validateDigest(descriptor.CapabilitiesSHA256); err != nil {
		return fmt.Errorf("review provider capabilities: %w", err)
	}
	return nil
}

func normalizeAssessment(source json.RawMessage) (Assessment, []byte, error) {
	if err := strictjson.ValidateWithLimits(source, strictjson.Limits{
		MaxInputBytes: maximumResponseBytes, MaxDepth: 8, MaxTokens: 4096,
		MaxObjectMembers: 9, MaxArrayElements: 129, MaxKeyBytes: 64,
		MaxTotalKeyBytes: 256 << 10, MaxWorkBytes: 64 << 20,
	}); err != nil {
		return Assessment{}, nil, err
	}
	if err := validateExactAssessmentShape(source); err != nil {
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
	normalized, err := marshalCanonicalIndented(assessment, maximumNormalizedOutputBytes)
	if err != nil {
		return Assessment{}, nil, err
	}
	return assessment, normalized, nil
}

func validateExactAssessmentShape(source json.RawMessage) error {
	top, err := exactJSONObject(
		source, "review assessment",
		[]string{
			"media_usable", "observed_outcome", "agrees_with_deterministic",
			"confidence", "summary", "significant_problems", "minor_observations",
			"limitations",
		}, nil,
	)
	if err != nil {
		return err
	}
	for _, name := range []string{"significant_problems", "minor_observations"} {
		var findings []json.RawMessage
		if err := json.Unmarshal(top[name], &findings); err != nil || findings == nil {
			return fmt.Errorf("review assessment field %q must be a non-null array", name)
		}
		for index, rawFinding := range findings {
			finding, err := exactJSONObject(
				rawFinding, fmt.Sprintf("review assessment %s %d", name, index),
				[]string{"category", "evidence", "impact"}, []string{"start_ms", "end_ms"},
			)
			if err != nil {
				return err
			}
			for _, timestamp := range []string{"start_ms", "end_ms"} {
				if rawTimestamp, exists := finding[timestamp]; exists {
					if err := validateAssessmentTimestamp(
						fmt.Sprintf("review assessment %s %d %s", name, index, timestamp),
						rawTimestamp,
					); err != nil {
						return err
					}
				}
			}
		}
	}
	var limitations []json.RawMessage
	if err := json.Unmarshal(top["limitations"], &limitations); err != nil || limitations == nil {
		return errors.New("review assessment field \"limitations\" must be a non-null array")
	}
	return nil
}

func validateAssessmentTimestamp(label string, source []byte) error {
	value, err := strconv.ParseInt(string(source), 10, 64)
	if err != nil {
		if len(source) > 0 && source[0] == '-' {
			return fmt.Errorf("%s must not be negative", label)
		}
		return fmt.Errorf("%s must be an integer in the supported range", label)
	}
	if value < 0 {
		return fmt.Errorf("%s must not be negative", label)
	}
	if value > maximumFindingTimestampMS {
		return fmt.Errorf("%s exceeds the supported timestamp range", label)
	}
	return nil
}

func exactJSONObject(
	source []byte, label string, required, optional []string,
) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(source, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", label)
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = false
	}
	for key, value := range object {
		requiredField, exists := allowed[key]
		if !exists {
			return nil, fmt.Errorf("%s contains unknown exact-case field %q", label, key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if requiredField {
				return nil, fmt.Errorf("%s field %q is required and must not be null", label, key)
			}
			return nil, fmt.Errorf("%s optional field %q must not be null when present", label, key)
		}
	}
	for _, key := range required {
		if _, exists := object[key]; !exists {
			return nil, fmt.Errorf("%s is missing required exact-case field %q", label, key)
		}
	}
	return object, nil
}

func validateFinding(label string, finding Finding) error {
	if err := validateMachineIdentifier(label+" category", finding.Category, 256); err != nil {
		return err
	}
	if !lowerSnakeCase(finding.Category) {
		return fmt.Errorf("%s category must be lowercase snake_case", label)
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
	if (finding.StartMS != nil && *finding.StartMS > maximumFindingTimestampMS) ||
		(finding.EndMS != nil && *finding.EndMS > maximumFindingTimestampMS) {
		return fmt.Errorf("%s timestamps exceed the supported range", label)
	}
	if finding.StartMS != nil && finding.EndMS != nil && *finding.EndMS < *finding.StartMS {
		return fmt.Errorf("%s end_ms precedes start_ms", label)
	}
	return nil
}

func validateAssessmentTimestampMaximum(
	assessment Assessment, maximumMS int64,
) error {
	if maximumMS <= 0 || maximumMS > maximumFindingTimestampMS {
		return errors.New("review assessment timestamp maximum is invalid")
	}
	for _, findings := range [][]Finding{
		assessment.SignificantProblems, assessment.MinorObservations,
	} {
		for _, finding := range findings {
			for _, timestamp := range []*int64{finding.StartMS, finding.EndMS} {
				if timestamp != nil && (*timestamp < 0 || *timestamp > maximumMS) {
					return errors.New("review assessment finding timestamp exceeds the sealed media timeline")
				}
				if timestamp != nil && !evidenceContainsExactTimestampMS(finding.Evidence, *timestamp) {
					return errors.New(
						"review assessment finding timestamp is not repeated exactly with ms in its evidence",
					)
				}
			}
		}
	}
	return nil
}

func evidenceContainsExactTimestampMS(evidence string, wanted int64) bool {
	if wanted < 0 {
		return false
	}
	for index := 0; index < len(evidence); {
		if evidence[index] < '0' || evidence[index] > '9' {
			index++
			continue
		}
		if index >= 2 && (evidence[index-1] == '.' || evidence[index-1] == ',') &&
			evidence[index-2] >= '0' && evidence[index-2] <= '9' {
			for index < len(evidence) && evidence[index] >= '0' && evidence[index] <= '9' {
				index++
			}
			continue
		}
		first, next, ok := reviewTimestampDecimal(evidence, index)
		if !ok {
			index = next
			continue
		}
		if reviewTimestampHasMSUnit(evidence, next) && first == wanted {
			return true
		}
		rangeIndex := reviewTimestampSpaces(evidence, next)
		switch {
		case rangeIndex < len(evidence) && evidence[rangeIndex] == '-':
			rangeIndex++
		case rangeIndex+3 <= len(evidence) &&
			(evidence[rangeIndex:rangeIndex+3] == "–" || evidence[rangeIndex:rangeIndex+3] == "—"):
			rangeIndex += 3
		default:
			index = next
			continue
		}
		rangeIndex = reviewTimestampSpaces(evidence, rangeIndex)
		second, rangeEnd, rangeOK := reviewTimestampDecimal(evidence, rangeIndex)
		if rangeOK && reviewTimestampHasMSUnit(evidence, rangeEnd) &&
			(first == wanted || second == wanted) {
			return true
		}
		index = next
	}
	return false
}

func reviewTimestampDecimal(source string, offset int) (int64, int, bool) {
	value := int64(0)
	index := offset
	valid := false
	for index < len(source) && source[index] >= '0' && source[index] <= '9' {
		digit := int64(source[index] - '0')
		if value > (math.MaxInt64-digit)/10 {
			for index < len(source) && source[index] >= '0' && source[index] <= '9' {
				index++
			}
			return 0, index, false
		}
		value = value*10 + digit
		valid = true
		index++
	}
	return value, index, valid
}

func reviewTimestampHasMSUnit(source string, offset int) bool {
	index := reviewTimestampSpaces(source, offset)
	if index+2 > len(source) || (source[index] != 'm' && source[index] != 'M') ||
		(source[index+1] != 's' && source[index+1] != 'S') {
		return false
	}
	index += 2
	return index == len(source) || !reviewTimestampWordByte(source[index])
}

func reviewTimestampSpaces(source string, offset int) int {
	for offset < len(source) {
		switch source[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func reviewTimestampWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}

func lowerSnakeCase(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' || value[len(value)-1] == '_' {
		return false
	}
	previousUnderscore := false
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			previousUnderscore = false
		case character == '_' && !previousUnderscore:
			previousUnderscore = true
		default:
			return false
		}
	}
	return true
}

func loadMediaContext(
	ctx context.Context, root *os.Root, media Media, expectedSize, maximumRead int64,
	between func(string),
) (Media, []byte, error) {
	if err := validateMediaIdentity(media); err != nil {
		return Media{}, nil, err
	}
	file, info, err := openRegularNoSymlink(root, media.Path, between)
	if err != nil {
		return Media{}, nil, err
	}
	defer file.Close()
	if expectedSize < 1 || maximumRead < 1 || expectedSize > maximumRead ||
		info.Size() != expectedSize {
		return Media{}, nil, fmt.Errorf("media path %q changed after metadata admission", media.Path)
	}
	payload, err := readBoundedContext(ctx, file, maximumRead)
	if err != nil {
		return Media{}, nil, err
	}
	if int64(len(payload)) != expectedSize {
		return Media{}, nil, fmt.Errorf(
			"media path %q changed after its admitted size", media.Path)
	}
	got, err := digestContext(ctx, payload)
	if err != nil {
		return Media{}, nil, err
	}
	if got != media.SHA256 {
		return Media{}, nil, fmt.Errorf("media path %q digest is %s, want %s", media.Path, got, media.SHA256)
	}
	if err := validateMediaPayloadContext(ctx, media.Kind, media.MediaType, payload); err != nil {
		return Media{}, nil, err
	}
	media.Validation = MediaValidationVersion
	media.SizeBytes = int64(len(payload))
	return media, payload, nil
}

// validateMediaPayload performs bounded structural validation. Capture
// plugins still retain deeper decoder/ffprobe attestations, but common images
// are decoded, WAV chunks and Ogg pages are parsed, and ISO-BMFF declarations
// are bound to track/codec structure instead of trusting a leading signature.
// Formats without a bounded structural parser are rejected here even if a
// downstream provider happens to accept their MIME type.
func validateMediaPayload(kind, mediaType string, payload []byte) error {
	return validateMediaPayloadContext(context.Background(), kind, mediaType, payload)
}

func validateMediaPayloadContext(
	ctx context.Context, kind, mediaType string, payload []byte,
) error {
	if ctx == nil {
		return errors.New("media payload validation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !supportedReviewMedia(kind, mediaType) {
		return fmt.Errorf("media type %q has no bounded payload validator", mediaType)
	}
	valid := false
	switch mediaType {
	case "audio/wav":
		valid = validWAVContext(ctx, payload)
	case "audio/ogg":
		valid = inspectOggContext(ctx, payload).valid
	case "audio/m4a":
		iso := inspectISOBMFFContext(ctx, payload)
		valid = iso.valid && iso.audio && !iso.video &&
			iso.hasBrand("M4A ", "M4B ", "isom", "mp42")
	case "audio/opus":
		ogg := inspectOggContext(ctx, payload)
		valid = ogg.valid && ogg.codec == "opus"
	case "image/png", "image/jpeg", "image/gif":
		valid = validDecodedImageContext(ctx, mediaType, payload)
	case "video/mp4":
		iso := inspectISOBMFFContext(ctx, payload)
		valid = iso.valid && iso.video
	case "video/mov":
		iso := inspectISOBMFFContext(ctx, payload)
		valid = iso.valid && iso.video && iso.hasBrand("qt  ")
	case "video/3gpp":
		iso := inspectISOBMFFContext(ctx, payload)
		valid = iso.valid && iso.video && iso.hasBrand("3gp1", "3gp2", "3g2a", "3g2b", "3gr6")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("media payload does not match declared type %q and kind %q", mediaType, kind)
	}
	return nil
}

type oggSummary struct {
	valid bool
	codec string
}

// inspectOgg validates a complete, single-logical-stream Ogg file. It checks
// page boundaries, sequence numbers, continuation flags, the non-reflected Ogg
// CRC-32, a completed identification packet, and the final end-of-stream page.
// Restricting this common validator to one stream keeps its evidence claim
// precise; a capture plugin may attest more elaborate multiplexed containers.
func inspectOgg(payload []byte) oggSummary {
	return inspectOggContext(context.Background(), payload)
}

func inspectOggContext(ctx context.Context, payload []byte) oggSummary {
	if ctx == nil || ctx.Err() != nil {
		return oggSummary{}
	}
	const (
		minimumHeaderBytes = 27
		maximumFirstPacket = 64 << 10
		maximumPages       = 1 << 20
		continuedFlag      = 0x01
		beginningFlag      = 0x02
		endFlag            = 0x04
	)
	if len(payload) < minimumHeaderBytes {
		return oggSummary{}
	}
	var packet []byte
	codec := ""
	var serial, nextSequence uint32
	packetCount := 0
	firstPage, packetContinues, sawEnd := true, false, false
	pageCount := 0
	for offset := 0; offset < len(payload); {
		if ctx.Err() != nil {
			return oggSummary{}
		}
		pageCount++
		if pageCount > maximumPages {
			return oggSummary{}
		}
		if sawEnd || len(payload)-offset < minimumHeaderBytes ||
			!bytes.Equal(payload[offset:offset+4], []byte("OggS")) || payload[offset+4] != 0 {
			return oggSummary{}
		}
		headerType := payload[offset+5]
		if headerType&^byte(continuedFlag|beginningFlag|endFlag) != 0 {
			return oggSummary{}
		}
		pageSerial := binary.LittleEndian.Uint32(payload[offset+14 : offset+18])
		sequence := binary.LittleEndian.Uint32(payload[offset+18 : offset+22])
		segments := int(payload[offset+26])
		if segments == 0 {
			return oggSummary{}
		}
		headerEnd := offset + minimumHeaderBytes + segments
		if headerEnd > len(payload) {
			return oggSummary{}
		}
		bodyBytes := 0
		for _, size := range payload[offset+minimumHeaderBytes : headerEnd] {
			bodyBytes += int(size)
		}
		pageEnd := headerEnd + bodyBytes
		if pageEnd > len(payload) {
			return oggSummary{}
		}
		checksum, checksumOK := oggChecksumContext(ctx, payload[offset:pageEnd])
		if !checksumOK ||
			checksum != binary.LittleEndian.Uint32(payload[offset+22:offset+26]) {
			return oggSummary{}
		}
		if firstPage {
			if headerType&beginningFlag == 0 || headerType&continuedFlag != 0 || sequence != 0 {
				return oggSummary{}
			}
			serial, nextSequence, firstPage = pageSerial, 1, false
		} else {
			if pageSerial != serial || sequence != nextSequence ||
				(headerType&continuedFlag != 0) != packetContinues || headerType&beginningFlag != 0 {
				return oggSummary{}
			}
			nextSequence++
		}
		bodyOffset := headerEnd
		for _, sizeByte := range payload[offset+minimumHeaderBytes : headerEnd] {
			size := int(sizeByte)
			if packetCount < 4 {
				if len(packet)+size > maximumFirstPacket {
					return oggSummary{}
				}
				packet = append(packet, payload[bodyOffset:bodyOffset+size]...)
			}
			bodyOffset += size
			packetContinues = size == 255
			if !packetContinues {
				packetCount++
				switch packetCount {
				case 1:
					codec = identifyOggCodec(packet)
					if codec == "" {
						return oggSummary{}
					}
				case 2:
					if (codec == "opus" && !validOpusTags(packet)) ||
						(codec == "vorbis" && !bytes.HasPrefix(packet, []byte("\x03vorbis"))) {
						return oggSummary{}
					}
				case 3:
					if (codec == "opus" && len(packet) == 0) ||
						(codec == "vorbis" && !bytes.HasPrefix(packet, []byte("\x05vorbis"))) {
						return oggSummary{}
					}
				case 4:
					if codec == "vorbis" && len(packet) == 0 {
						return oggSummary{}
					}
				}
				if packetCount < 4 {
					packet = packet[:0]
				} else {
					packet = nil
				}
			}
		}
		if headerType&endFlag != 0 {
			sawEnd = true
		}
		offset = pageEnd
	}
	minimumPackets := 3
	if codec == "vorbis" {
		minimumPackets = 4
	}
	if firstPage || !sawEnd || packetContinues || codec == "" || packetCount < minimumPackets {
		return oggSummary{}
	}
	return oggSummary{valid: true, codec: codec}
}

func validOpusTags(packet []byte) bool {
	if len(packet) < 16 || !bytes.Equal(packet[:8], []byte("OpusTags")) {
		return false
	}
	vendorBytes := uint64(binary.LittleEndian.Uint32(packet[8:12]))
	offset := uint64(12) + vendorBytes
	if offset+4 > uint64(len(packet)) {
		return false
	}
	comments := binary.LittleEndian.Uint32(packet[offset : offset+4])
	offset += 4
	for index := uint32(0); index < comments; index++ {
		if offset+4 > uint64(len(packet)) {
			return false
		}
		commentBytes := uint64(binary.LittleEndian.Uint32(packet[offset : offset+4]))
		offset += 4
		if offset+commentBytes > uint64(len(packet)) {
			return false
		}
		offset += commentBytes
	}
	return offset == uint64(len(packet))
}

func identifyOggCodec(packet []byte) string {
	if validOpusIdentification(packet) {
		return "opus"
	}
	if len(packet) >= 30 && bytes.Equal(packet[:7], []byte("\x01vorbis")) &&
		binary.LittleEndian.Uint32(packet[7:11]) == 0 && packet[11] > 0 &&
		binary.LittleEndian.Uint32(packet[12:16]) > 0 && packet[29]&1 == 1 {
		return "vorbis"
	}
	return ""
}

func validOpusIdentification(packet []byte) bool {
	if len(packet) < 19 || !bytes.Equal(packet[:8], []byte("OpusHead")) ||
		packet[8] != 1 || packet[9] == 0 {
		return false
	}
	channels, mapping := int(packet[9]), packet[18]
	if mapping == 0 {
		return channels <= 2 && len(packet) == 19
	}
	if len(packet) != 21+channels || packet[19] == 0 || packet[20] > packet[19] {
		return false
	}
	maximumChannel := int(packet[19]) + int(packet[20])
	for _, channel := range packet[21:] {
		if channel != 255 && int(channel) >= maximumChannel {
			return false
		}
	}
	return true
}

var oggChecksumTable = func() [256]uint32 {
	var table [256]uint32
	for index := range table {
		value := uint32(index) << 24
		for bit := 0; bit < 8; bit++ {
			if value&0x80000000 != 0 {
				value = value<<1 ^ 0x04c11db7
			} else {
				value <<= 1
			}
		}
		table[index] = value
	}
	return table
}()

func oggChecksum(page []byte) uint32 {
	checksum, _ := oggChecksumContext(context.Background(), page)
	return checksum
}

func oggChecksumContext(ctx context.Context, page []byte) (uint32, bool) {
	if ctx == nil || ctx.Err() != nil {
		return 0, false
	}
	var checksum uint32
	for index, value := range page {
		if index&65535 == 0 && ctx.Err() != nil {
			return 0, false
		}
		if index >= 22 && index < 26 {
			value = 0
		}
		checksum = checksum<<8 ^ oggChecksumTable[byte(checksum>>24)^value]
	}
	return checksum, ctx.Err() == nil
}

func validWAV(payload []byte) bool {
	return validWAVContext(context.Background(), payload)
}

func validWAVContext(ctx context.Context, payload []byte) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	const maximumChunks = 1 << 20
	if len(payload) < 44 || !bytes.Equal(payload[:4], []byte("RIFF")) ||
		!bytes.Equal(payload[8:12], []byte("WAVE")) ||
		uint64(binary.LittleEndian.Uint32(payload[4:8]))+8 != uint64(len(payload)) {
		return false
	}
	seenFormat, validFormat, seenData, validData := false, false, false, false
	var formatBlockAlign uint16
	dataSize := 0
	offset := 12
	chunks := 0
	for offset < len(payload) {
		if ctx.Err() != nil {
			return false
		}
		chunks++
		if chunks > maximumChunks {
			return false
		}
		if len(payload)-offset < 8 {
			return false
		}
		size := int(binary.LittleEndian.Uint32(payload[offset+4 : offset+8]))
		start := offset + 8
		if size < 0 || start > len(payload) || size > len(payload)-start {
			return false
		}
		switch string(payload[offset : offset+4]) {
		case "fmt ":
			if seenFormat {
				return false
			}
			seenFormat = true
			formatBlockAlign, validFormat = validWAVFormat(payload[start : start+size])
			if !validFormat {
				return false
			}
		case "data":
			if seenData || !validFormat || size == 0 {
				return false
			}
			seenData, validData = true, true
			dataSize = size
		}
		next := start + size
		if size%2 != 0 {
			next++
		}
		if next <= offset || next > len(payload) {
			return false
		}
		offset = next
	}
	return offset == len(payload) && seenFormat && validFormat && seenData && validData && formatBlockAlign > 0 &&
		dataSize%int(formatBlockAlign) == 0
}

func validWAVFormat(format []byte) (uint16, bool) {
	if len(format) < 16 {
		return 0, false
	}
	if len(format) != 16 {
		if len(format) < 18 || int(binary.LittleEndian.Uint16(format[16:18]))+18 != len(format) {
			return 0, false
		}
	}
	formatTag := binary.LittleEndian.Uint16(format[:2])
	channels := binary.LittleEndian.Uint16(format[2:4])
	rate := binary.LittleEndian.Uint32(format[4:8])
	byteRate := binary.LittleEndian.Uint32(format[8:12])
	blockAlign := binary.LittleEndian.Uint16(format[12:14])
	bits := binary.LittleEndian.Uint16(format[14:16])
	effectiveFormat := formatTag
	if formatTag == 0xfffe {
		if len(format) != 40 || binary.LittleEndian.Uint16(format[16:18]) != 22 {
			return 0, false
		}
		validBits := binary.LittleEndian.Uint16(format[18:20])
		if validBits == 0 || validBits > bits {
			return 0, false
		}
		pcmGUID := []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
		floatGUID := slices.Clone(pcmGUID)
		floatGUID[0] = 3
		subformat := format[24:40]
		switch {
		case bytes.Equal(subformat, pcmGUID):
			effectiveFormat = 1
		case bytes.Equal(subformat, floatGUID):
			effectiveFormat = 3
		default:
			return 0, false
		}
	} else if len(format) != 16 && binary.LittleEndian.Uint16(format[16:18]) != 0 {
		return 0, false
	}
	validBits := false
	switch effectiveFormat {
	case 1:
		validBits = bits == 8 || bits == 16 || bits == 24 || bits == 32
	case 3:
		validBits = bits == 32 || bits == 64
	default:
		return 0, false
	}
	bytesPerSample := (uint64(bits) + 7) / 8
	if !validBits || channels == 0 || channels > 64 || rate == 0 || rate > 768_000 ||
		uint64(blockAlign) != uint64(channels)*bytesPerSample ||
		uint64(byteRate) != uint64(rate)*uint64(blockAlign) {
		return 0, false
	}
	return blockAlign, true
}

const maximumDecodedImageBytes = 256 << 20

func validDecodedImage(mediaType string, payload []byte) bool {
	return validDecodedImageContext(context.Background(), mediaType, payload)
}

type cancellationReader struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (reader *cancellationReader) Read(destination []byte) (int, error) {
	if reader == nil || reader.ctx == nil || reader.reader == nil {
		return 0, errors.New("media decoder reader is unavailable")
	}
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(destination) > 64<<10 {
		destination = destination[:64<<10]
	}
	read, err := reader.reader.Read(destination)
	if cause := reader.ctx.Err(); cause != nil {
		return read, cause
	}
	return read, err
}

func validDecodedImageContext(
	ctx context.Context, mediaType string, payload []byte,
) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	config, format, err := image.DecodeConfig(&cancellationReader{ctx, bytes.NewReader(payload)})
	want := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif"}[mediaType]
	if err != nil || format != want || config.Width <= 0 || config.Height <= 0 ||
		config.Width > 16_384 || config.Height > 16_384 ||
		decodedImageBytes(config.Width, config.Height) > maximumDecodedImageBytes {
		return false
	}
	complete := false
	switch mediaType {
	case "image/png":
		complete = validPNGStructureContext(ctx, payload)
	case "image/jpeg":
		complete = validJPEGStructureContext(ctx, payload)
	case "image/gif":
		complete = payload[len(payload)-1] == 0x3b
	default:
		return false
	}
	if !complete {
		return false
	}
	if mediaType == "image/gif" {
		frames, totalPixels, ok := boundedGIFStructureContext(ctx, payload)
		if !ok {
			return false
		}
		decoded, err := gif.DecodeAll(&cancellationReader{ctx, bytes.NewReader(payload)})
		if err != nil || decoded == nil || len(decoded.Image) != frames ||
			decoded.Config.Width != config.Width || decoded.Config.Height != config.Height {
			return false
		}
		decodedBytes := uint64(0)
		canvas := image.Rect(0, 0, config.Width, config.Height)
		for index, frame := range decoded.Image {
			if index&255 == 0 && ctx.Err() != nil {
				return false
			}
			if frame == nil || !frame.Bounds().In(canvas) || frame.Bounds().Empty() {
				return false
			}
			bounds := frame.Bounds()
			frameBytes := decodedImageBytes(bounds.Dx(), bounds.Dy())
			if frameBytes > maximumDecodedImageBytes-decodedBytes {
				return false
			}
			decodedBytes += frameBytes
		}
		return decodedBytes == totalPixels*8
	}
	decoded, decodedFormat, err := image.Decode(&cancellationReader{ctx, bytes.NewReader(payload)})
	return err == nil && decodedFormat == want && decoded != nil &&
		decoded.Bounds().Dx() == config.Width && decoded.Bounds().Dy() == config.Height
}

func decodedImageBytes(width, height int) uint64 {
	if width <= 0 || height <= 0 {
		return maximumDecodedImageBytes + 1
	}
	// Eight bytes per pixel is the largest native representation used by the
	// standard image decoders (for example RGBA64). This bounds allocation in
	// bytes independently of color depth and decoder-selected concrete type.
	return uint64(width) * uint64(height) * 8
}

const maximumImageStructureEntries = 1 << 20

func validPNGStructure(payload []byte) bool {
	return validPNGStructureContext(context.Background(), payload)
}

func validPNGStructureContext(ctx context.Context, payload []byte) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if len(payload) < 8 || !bytes.Equal(payload[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}
	offset := 8
	chunks := 0
	sawHeader, sawImageData, imageDataEnded := false, false, false
	for offset < len(payload) {
		if ctx.Err() != nil {
			return false
		}
		chunks++
		if chunks > maximumImageStructureEntries || len(payload)-offset < 12 {
			return false
		}
		length := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		end64 := uint64(offset) + 12 + length
		if end64 > uint64(len(payload)) {
			return false
		}
		dataEnd := offset + 8 + int(length)
		end := int(end64)
		kind := payload[offset+4 : offset+8]
		for _, character := range kind {
			if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
				return false
			}
		}
		if kind[2]&0x20 != 0 {
			// PNG reserves the third type-code bit; it must remain zero (uppercase).
			return false
		}
		checksum, ok := checksumIEEEContext(ctx, payload[offset+4:dataEnd])
		if !ok || checksum !=
			binary.BigEndian.Uint32(payload[dataEnd:end]) {
			return false
		}
		switch string(kind) {
		case "IHDR":
			if sawHeader || chunks != 1 || length != 13 {
				return false
			}
			sawHeader = true
		case "IDAT":
			if !sawHeader || imageDataEnded {
				return false
			}
			sawImageData = true
		case "IEND":
			return sawHeader && sawImageData && length == 0 && end == len(payload)
		default:
			if !sawHeader {
				return false
			}
			if kind[0]&0x20 == 0 {
				// The decoder cannot safely infer semantics for an unknown
				// critical chunk. Known critical PLTE remains decoder-checked.
				if !bytes.Equal(kind, []byte("PLTE")) {
					return false
				}
			}
		}
		if sawImageData && !bytes.Equal(kind, []byte("IDAT")) {
			imageDataEnded = true
		}
		offset = end
	}
	return false
}

func checksumIEEEContext(ctx context.Context, payload []byte) (uint32, bool) {
	if ctx == nil || ctx.Err() != nil {
		return 0, false
	}
	hasher := crc32.NewIEEE()
	for offset := 0; offset < len(payload); {
		if ctx.Err() != nil {
			return 0, false
		}
		end := min(offset+(64<<10), len(payload))
		_, _ = hasher.Write(payload[offset:end])
		offset = end
	}
	return hasher.Sum32(), ctx.Err() == nil
}

func validJPEGStructure(payload []byte) bool {
	return validJPEGStructureContext(context.Background(), payload)
}

func validJPEGStructureContext(ctx context.Context, payload []byte) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if len(payload) < 4 || payload[0] != 0xff || payload[1] != 0xd8 {
		return false
	}
	offset := 2
	markers := 1
	inEntropy, sawFrame, sawScan := false, false, false
	for offset < len(payload) {
		if ctx.Err() != nil {
			return false
		}
		var marker byte
		if inEntropy {
			for offset < len(payload) && payload[offset] != 0xff {
				if offset&65535 == 0 && ctx.Err() != nil {
					return false
				}
				offset++
			}
			if offset >= len(payload) {
				return false
			}
			for offset < len(payload) && payload[offset] == 0xff {
				offset++
			}
			if offset >= len(payload) {
				return false
			}
			marker = payload[offset]
			offset++
			switch {
			case marker == 0x00:
				continue
			case marker >= 0xd0 && marker <= 0xd7:
				continue
			default:
				inEntropy = false
			}
		} else {
			if payload[offset] != 0xff {
				return false
			}
			for offset < len(payload) && payload[offset] == 0xff {
				offset++
			}
			if offset >= len(payload) {
				return false
			}
			marker = payload[offset]
			offset++
			if marker == 0x00 {
				return false
			}
		}
		markers++
		if markers > maximumImageStructureEntries {
			return false
		}
		switch {
		case marker == 0xd9:
			return sawFrame && sawScan && offset == len(payload)
		case marker == 0xd8, marker == 0x01,
			marker >= 0xd0 && marker <= 0xd7:
			return false
		case marker == 0xda:
			end, ok := jpegSegmentEnd(payload, offset)
			if !ok || !sawFrame {
				return false
			}
			offset, inEntropy, sawScan = end, true, true
		default:
			end, ok := jpegSegmentEnd(payload, offset)
			if !ok {
				return false
			}
			if jpegStartOfFrame(marker) {
				sawFrame = true
			}
			offset = end
		}
	}
	return false
}

func jpegSegmentEnd(payload []byte, offset int) (int, bool) {
	if len(payload)-offset < 2 {
		return 0, false
	}
	length := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
	if length < 2 || length > len(payload)-offset {
		return 0, false
	}
	return offset + length, true
}

func jpegStartOfFrame(marker byte) bool {
	switch marker {
	case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7,
		0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
		return true
	default:
		return false
	}
}

const (
	maximumGIFFrames          = 10_000
	maximumGIFStructureBlocks = 1 << 20
)

// boundedGIFStructure walks every GIF block before gif.DecodeAll can allocate
// frame images. It caps both frame count and aggregate decoded pixels, rejects
// trailing bytes, and makes a valid first frame insufficient evidence for a
// corrupt later frame.
func boundedGIFStructure(payload []byte) (int, uint64, bool) {
	return boundedGIFStructureContext(context.Background(), payload)
}

func boundedGIFStructureContext(
	ctx context.Context, payload []byte,
) (int, uint64, bool) {
	if ctx == nil || ctx.Err() != nil {
		return 0, 0, false
	}
	if len(payload) < 14 ||
		(!bytes.Equal(payload[:6], []byte("GIF87a")) &&
			!bytes.Equal(payload[:6], []byte("GIF89a"))) {
		return 0, 0, false
	}
	screenWidth := int(binary.LittleEndian.Uint16(payload[6:8]))
	screenHeight := int(binary.LittleEndian.Uint16(payload[8:10]))
	if screenWidth <= 0 || screenHeight <= 0 ||
		uint64(screenWidth)*uint64(screenHeight) > 32_000_000 {
		return 0, 0, false
	}
	offset := 13
	if payload[10]&0x80 != 0 {
		colors := 1 << ((payload[10] & 0x07) + 1)
		tableBytes := 3 * colors
		if tableBytes > len(payload)-offset {
			return 0, 0, false
		}
		offset += tableBytes
	}
	frames := 0
	totalPixels := uint64(0)
	blocks := 0
	pendingGraphicControl := false
	for offset < len(payload) {
		if ctx.Err() != nil {
			return 0, 0, false
		}
		blocks++
		if blocks > maximumGIFStructureBlocks {
			return 0, 0, false
		}
		switch payload[offset] {
		case 0x3b:
			if frames == 0 || pendingGraphicControl || offset+1 != len(payload) {
				return 0, 0, false
			}
			return frames, totalPixels, true
		case 0x2c:
			if len(payload)-offset < 10 {
				return 0, 0, false
			}
			left := int(binary.LittleEndian.Uint16(payload[offset+1 : offset+3]))
			top := int(binary.LittleEndian.Uint16(payload[offset+3 : offset+5]))
			width := int(binary.LittleEndian.Uint16(payload[offset+5 : offset+7]))
			height := int(binary.LittleEndian.Uint16(payload[offset+7 : offset+9]))
			packed := payload[offset+9]
			if width <= 0 || height <= 0 || left > screenWidth-width || top > screenHeight-height ||
				packed&0x18 != 0 ||
				frames >= maximumGIFFrames {
				return 0, 0, false
			}
			pixels := uint64(width) * uint64(height)
			if pixels > 32_000_000 || totalPixels > 32_000_000-pixels {
				return 0, 0, false
			}
			totalPixels += pixels
			frames++
			pendingGraphicControl = false
			offset += 10
			if packed&0x80 != 0 {
				colors := 1 << ((packed & 0x07) + 1)
				tableBytes := 3 * colors
				if tableBytes > len(payload)-offset {
					return 0, 0, false
				}
				offset += tableBytes
			}
			if offset >= len(payload) || payload[offset] < 2 || payload[offset] > 8 {
				return 0, 0, false
			}
			offset++
			var ok bool
			offset, ok = skipGIFSubBlocksContext(ctx, payload, offset, &blocks)
			if !ok {
				return 0, 0, false
			}
		case 0x21:
			if len(payload)-offset < 3 {
				return 0, 0, false
			}
			label := payload[offset+1]
			offset += 2
			switch label {
			case 0xf9:
				if pendingGraphicControl || len(payload)-offset < 6 || payload[offset] != 4 ||
					payload[offset+1]&0xe0 != 0 || (payload[offset+1]>>2)&0x07 > 3 ||
					payload[offset+5] != 0 {
					return 0, 0, false
				}
				pendingGraphicControl = true
				offset += 6
			case 0x01:
				if len(payload)-offset < 13 || payload[offset] != 12 {
					return 0, 0, false
				}
				offset += 13
				pendingGraphicControl = false
				var ok bool
				offset, ok = skipGIFSubBlocksContext(ctx, payload, offset, &blocks)
				if !ok {
					return 0, 0, false
				}
			case 0xff:
				if len(payload)-offset < 12 || payload[offset] != 11 {
					return 0, 0, false
				}
				offset += 12
				var ok bool
				offset, ok = skipGIFSubBlocksContext(ctx, payload, offset, &blocks)
				if !ok {
					return 0, 0, false
				}
			case 0xfe:
				var ok bool
				offset, ok = skipGIFSubBlocksContext(ctx, payload, offset, &blocks)
				if !ok {
					return 0, 0, false
				}
			default:
				return 0, 0, false
			}
		default:
			return 0, 0, false
		}
	}
	return 0, 0, false
}

func skipGIFSubBlocks(payload []byte, offset int, blocks *int) (int, bool) {
	return skipGIFSubBlocksContext(context.Background(), payload, offset, blocks)
}

func skipGIFSubBlocksContext(
	ctx context.Context, payload []byte, offset int, blocks *int,
) (int, bool) {
	if ctx == nil || ctx.Err() != nil {
		return 0, false
	}
	for offset < len(payload) {
		if ctx.Err() != nil {
			return 0, false
		}
		if blocks == nil || *blocks >= maximumGIFStructureBlocks {
			return 0, false
		}
		(*blocks)++
		size := int(payload[offset])
		offset++
		if size == 0 {
			return offset, true
		}
		if size > len(payload)-offset {
			return 0, false
		}
		offset += size
	}
	return 0, false
}

type isoBMFFSummary struct {
	valid     bool
	fileType  bool
	mediaData bool
	video     bool
	audio     bool
	meta      bool
	brands    map[string]struct{}
}

func (summary isoBMFFSummary) hasBrand(values ...string) bool {
	for _, value := range values {
		if _, exists := summary.brands[value]; exists {
			return true
		}
	}
	return false
}

type isoBox struct {
	kind string
	data []byte
}

func inspectISOBMFF(payload []byte) isoBMFFSummary {
	return inspectISOBMFFContext(context.Background(), payload)
}

func inspectISOBMFFContext(ctx context.Context, payload []byte) isoBMFFSummary {
	boxes, ok := parseISOBoxesContext(ctx, payload)
	summary := isoBMFFSummary{brands: make(map[string]struct{})}
	if ctx == nil || ctx.Err() != nil || !ok {
		return summary
	}
	foundFileType, foundMovie, foundMediaData := false, false, false
	for index, box := range boxes {
		if index&1023 == 0 && ctx.Err() != nil {
			return summary
		}
		switch box.kind {
		case "ftyp":
			if foundFileType || len(box.data) < 8 || (len(box.data)-8)%4 != 0 ||
				(len(box.data)-8)/4 > 1024 {
				return summary
			}
			foundFileType = true
			summary.brands[string(box.data[:4])] = struct{}{}
			for offset := 8; offset+4 <= len(box.data); offset += 4 {
				summary.brands[string(box.data[offset:offset+4])] = struct{}{}
			}
		case "meta":
			summary.meta = true
		case "mdat":
			foundMediaData = foundMediaData || len(box.data) > 0
		case "moov":
			if foundMovie {
				return summary
			}
			foundMovie = true
			tracks, trackOK := parseISOBoxesContext(ctx, box.data)
			if !trackOK {
				return summary
			}
			for trackIndex, track := range tracks {
				if trackIndex&1023 == 0 && ctx.Err() != nil {
					return summary
				}
				if track.kind != "trak" {
					continue
				}
				handler, codec, validTrack := inspectISOTrackContext(ctx, track.data)
				if !validTrack {
					return summary
				}
				if handler == "vide" && map[string]bool{
					"avc1": true, "avc3": true, "hvc1": true, "hev1": true,
					"vp09": true, "av01": true, "mp4v": true,
				}[codec] {
					summary.video = true
				}
				if handler == "soun" && map[string]bool{
					"mp4a": true, "Opus": true, "alac": true, "ac-3": true, "ec-3": true,
				}[codec] {
					summary.audio = true
				}
			}
		}
	}
	summary.fileType = foundFileType
	summary.mediaData = foundMediaData
	summary.valid = foundFileType && foundMovie && foundMediaData && (summary.video || summary.audio)
	return summary
}

func inspectISOTrack(payload []byte) (string, string, bool) {
	return inspectISOTrackContext(context.Background(), payload)
}

func inspectISOTrackContext(ctx context.Context, payload []byte) (string, string, bool) {
	trackBoxes, ok := parseISOBoxesContext(ctx, payload)
	if !ok {
		return "", "", false
	}
	foundMedia := false
	resultHandler, resultCodec := "", ""
	for index, box := range trackBoxes {
		if index&1023 == 0 && (ctx == nil || ctx.Err() != nil) {
			return "", "", false
		}
		if box.kind != "mdia" {
			continue
		}
		if foundMedia {
			return "", "", false
		}
		foundMedia = true
		mediaBoxes, ok := parseISOBoxesContext(ctx, box.data)
		if !ok {
			return "", "", false
		}
		handler, codec := "", ""
		foundHandler, foundMediaInfo := false, false
		for mediaIndex, mediaBox := range mediaBoxes {
			if mediaIndex&1023 == 0 && ctx.Err() != nil {
				return "", "", false
			}
			switch mediaBox.kind {
			case "hdlr":
				if foundHandler || len(mediaBox.data) < 12 {
					return "", "", false
				}
				foundHandler = true
				handler = string(mediaBox.data[8:12])
			case "minf":
				if foundMediaInfo {
					return "", "", false
				}
				foundMediaInfo = true
				codec = inspectISOSampleEntryContext(ctx, mediaBox.data)
			}
		}
		if !foundHandler || !foundMediaInfo || handler == "" || codec == "" {
			return "", "", false
		}
		resultHandler, resultCodec = handler, codec
	}
	return resultHandler, resultCodec, foundMedia && resultHandler != "" && resultCodec != ""
}

func inspectISOSampleEntry(payload []byte) string {
	return inspectISOSampleEntryContext(context.Background(), payload)
}

func inspectISOSampleEntryContext(ctx context.Context, payload []byte) string {
	minimum, ok := parseISOBoxesContext(ctx, payload)
	if !ok {
		return ""
	}
	foundSampleTable := false
	codec := ""
	for index, box := range minimum {
		if index&1023 == 0 && (ctx == nil || ctx.Err() != nil) {
			return ""
		}
		if box.kind != "stbl" {
			continue
		}
		if foundSampleTable {
			return ""
		}
		foundSampleTable = true
		tables, ok := parseISOBoxesContext(ctx, box.data)
		if !ok {
			return ""
		}
		foundDescription := false
		for tableIndex, table := range tables {
			if tableIndex&1023 == 0 && ctx.Err() != nil {
				return ""
			}
			if table.kind != "stsd" {
				continue
			}
			if foundDescription || len(table.data) < 8 {
				return ""
			}
			foundDescription = true
			entryCount := binary.BigEndian.Uint32(table.data[4:8])
			if entryCount == 0 || entryCount > maximumISOBoxes {
				return ""
			}
			entries, ok := parseISOBoxesContext(ctx, table.data[8:])
			if !ok || len(entries) != int(entryCount) {
				return ""
			}
			codec = entries[0].kind
		}
		if !foundDescription || codec == "" {
			return ""
		}
	}
	if !foundSampleTable {
		return ""
	}
	return codec
}

const maximumISOBoxes = 1 << 20

func parseISOBoxes(payload []byte) ([]isoBox, bool) {
	return parseISOBoxesContext(context.Background(), payload)
}

func parseISOBoxesContext(ctx context.Context, payload []byte) ([]isoBox, bool) {
	if ctx == nil || ctx.Err() != nil {
		return nil, false
	}
	var result []isoBox
	for offset := 0; offset < len(payload); {
		if len(result)&1023 == 0 && ctx.Err() != nil {
			return nil, false
		}
		if len(result) >= maximumISOBoxes {
			return nil, false
		}
		if len(payload)-offset < 8 {
			return nil, false
		}
		size := uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		header := uint64(8)
		if size == 1 {
			if len(payload)-offset < 16 {
				return nil, false
			}
			size = binary.BigEndian.Uint64(payload[offset+8 : offset+16])
			header = 16
		} else if size == 0 {
			size = uint64(len(payload) - offset)
		}
		if size < header || size > uint64(len(payload)-offset) {
			return nil, false
		}
		end := offset + int(size)
		result = append(result, isoBox{
			kind: string(payload[offset+4 : offset+8]), data: payload[offset+int(header) : end],
		})
		offset = end
	}
	return result, len(result) > 0
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
		!utf8.ValidString(media.Path) || containsControl(media.Path) ||
		filepath.IsAbs(media.Path) || strings.Contains(media.Path, `\`) ||
		filepath.Clean(media.Path) != media.Path || media.Path == "." || media.Path == ".." ||
		strings.HasPrefix(media.Path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("media path %q must be a clean relative path without traversal", media.Path)
	}
	return nil
}

func openValidatedRoot(path string, between func()) (*os.Root, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("review root directory must be a clean absolute path")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect review root directory: %w", err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("review root directory must be a non-symlink directory")
	}
	if between != nil {
		between()
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open review root directory: %w", err)
	}
	opened, openErr := root.Stat(".")
	after, afterErr := os.Lstat(path)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, errors.New("review root directory changed while it was opened")
	}
	return root, nil
}

func openRegularNoSymlink(
	root *os.Root, path string, between func(string),
) (*os.File, os.FileInfo, error) {
	components := strings.Split(path, string(filepath.Separator))
	if len(components) == 0 {
		return nil, nil, errors.New("media path has no components")
	}
	current := root
	var children []*os.Root
	defer func() {
		for index := len(children) - 1; index >= 0; index-- {
			_ = children[index].Close()
		}
	}()
	prefix := ""
	for _, component := range components[:len(components)-1] {
		if prefix == "" {
			prefix = component
		} else {
			prefix = filepath.Join(prefix, component)
		}
		before, err := current.Lstat(component)
		if err != nil {
			return nil, nil, err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			return nil, nil, fmt.Errorf("path component %q is not a non-symlink directory", prefix)
		}
		if between != nil {
			between(prefix)
		}
		child, err := current.OpenRoot(component)
		if err != nil {
			return nil, nil, err
		}
		opened, openErr := child.Stat(".")
		after, afterErr := current.Lstat(component)
		if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) {
			_ = child.Close()
			return nil, nil, fmt.Errorf("path component %q changed while it was opened", prefix)
		}
		children = append(children, child)
		current = child
	}
	name := components[len(components)-1]
	if prefix == "" {
		prefix = name
	} else {
		prefix = filepath.Join(prefix, name)
	}
	before, err := current.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("media path %q is not a regular non-symlink file", path)
	}
	if between != nil {
		between(prefix)
	}
	file, err := current.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, openErr := file.Stat()
	after, afterErr := current.Lstat(name)
	if openErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("media path %q changed while it was opened", path)
	}
	if err := fileidentity.RequireSingleLink(file); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("media path %q is not exclusively retained", path)
	}
	return file, opened, nil
}

func readBoundedContext(ctx context.Context, reader io.Reader, maximum int64) ([]byte, error) {
	var output bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		count, err := reader.Read(chunk)
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		if count > 0 {
			if int64(output.Len()) > maximum-int64(count) {
				return nil, fmt.Errorf("media exceeds %d bytes while reading", maximum)
			}
			_, _ = output.Write(chunk[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if output.Len() == 0 {
		return nil, errors.New("media is empty")
	}
	if cause := ctx.Err(); cause != nil {
		return nil, cause
	}
	return output.Bytes(), nil
}

func canonicalJSON(source []byte, maximum int) ([]byte, error) {
	return canonicalJSONContext(context.Background(), source, maximum)
}

func canonicalJSONContext(
	ctx context.Context, source []byte, maximum int,
) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("canonical JSON requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(source) == 0 || len(source) > maximum {
		return nil, fmt.Errorf("JSON must be 1..%d bytes", maximum)
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(&cancellationReader{ctx: ctx, reader: bytes.NewReader(source)})
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("JSON must be an object")
	}
	result, err := marshalCanonicalCompact(value, maximum)
	if err != nil {
		return nil, err
	}
	return result, ctx.Err()
}

func canonicalSensitiveValues(values []string) ([][]byte, error) {
	if len(values) > maximumSensitiveValues {
		return nil, fmt.Errorf(
			"review sensitive values exceed the %d-value limit", maximumSensitiveValues)
	}
	result := make([][]byte, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	total := 0
	for index, value := range values {
		if len(value) < 8 || len(value) > 4096 || !utf8.ValidString(value) ||
			strings.TrimSpace(value) != value || containsControl(value) {
			return nil, fmt.Errorf("review sensitive value %d is empty, short, oversized, or noncanonical", index)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		if total > maximumSensitiveAggregateBytes-len(value) {
			return nil, fmt.Errorf(
				"review sensitive values exceed the %d-byte aggregate limit", maximumSensitiveAggregateBytes)
		}
		total += len(value)
		seen[value] = struct{}{}
		result = append(result, []byte(value))
	}
	return result, nil
}

func secretInJSON(payload []byte, matcher *sensitiveMatcher) bool {
	matched, err := secretInJSONContext(context.Background(), payload, matcher)
	return matched || err != nil
}

func secretInJSONContext(
	ctx context.Context, payload []byte, matcher *sensitiveMatcher,
) (bool, error) {
	if ctx == nil {
		return false, errors.New("review JSON secret scan requires a context")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if matcher != nil {
		matched, err := matcher.containsContext(ctx, payload)
		if err != nil || matched {
			return matched, err
		}
	}
	if len(payload) == 0 || matcher == nil || matcher.width == 0 {
		return false, nil
	}
	type scanItem struct {
		payload []byte
		depth   int
	}
	queue := []scanItem{{payload: payload}}
	workRemaining := int64(maximumSensitiveWork)
	if int64(len(payload)) <= (int64(maximumSensitiveWork)-(1<<20))/8 {
		workRemaining = int64(len(payload))*8 + 1<<20
	}
	queuedBytes := int64(0)
	decodedAllState, decodedKeyState, decodedValueState := uint32(0), uint32(0), uint32(0)
	advanceDecoded := func(payload []byte, key bool) (bool, error) {
		matched, err := matcher.advanceContext(ctx, payload, &decodedAllState)
		if err != nil || matched {
			return matched, err
		}
		state := &decodedValueState
		if key {
			state = &decodedKeyState
		}
		return matcher.advanceContext(ctx, payload, state)
	}
	escapedAllStates := make([]uint32, maximumSensitiveDepth)
	escapedKeyStates := make([]uint32, maximumSensitiveDepth)
	escapedValueStates := make([]uint32, maximumSensitiveDepth)
	escapedAllActive := make([]bool, maximumSensitiveDepth)
	escapedKeyActive := make([]bool, maximumSensitiveDepth)
	escapedValueActive := make([]bool, maximumSensitiveDepth)
	advanceEscaped := func(
		payload []byte, key bool, baseAll, baseRole uint32,
	) (bool, error) {
		roleStates, roleActive := escapedValueStates, escapedValueActive
		if key {
			roleStates, roleActive = escapedKeyStates, escapedKeyActive
		}
		current := payload
		for depth := 0; depth < maximumSensitiveDepth; depth++ {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			decoded, changed, decodeErr := decodeJSONEscapesAnywhereContext(ctx, current)
			if decodeErr != nil {
				return false, decodeErr
			}
			if changed {
				workRemaining -= int64(len(decoded)) * 2
				if workRemaining < 0 {
					return true, nil
				}
				if !escapedAllActive[depth] {
					escapedAllStates[depth] = baseAll
				}
				matched, err := matcher.advanceContext(ctx, decoded, &escapedAllStates[depth])
				if err != nil || matched {
					return matched, err
				}
				escapedAllActive[depth] = escapedAllStates[depth] != 0
				if !roleActive[depth] {
					roleStates[depth] = baseRole
				}
				matched, err = matcher.advanceContext(ctx, decoded, &roleStates[depth])
				if err != nil || matched {
					return matched, err
				}
				roleActive[depth] = roleStates[depth] != 0
				current = decoded
				continue
			}
			for activeDepth := depth; activeDepth < maximumSensitiveDepth; activeDepth++ {
				for stream := 0; stream < 2; stream++ {
					states, active := escapedAllStates, escapedAllActive
					if stream == 1 {
						states, active = roleStates, roleActive
					}
					if !active[activeDepth] {
						continue
					}
					workRemaining -= int64(len(current))
					if workRemaining < 0 {
						return true, nil
					}
					matched, err := matcher.advanceContext(ctx, current, &states[activeDepth])
					if err != nil || matched {
						return matched, err
					}
					active[activeDepth] = states[activeDepth] != 0
				}
			}
			return false, nil
		}
		return true, nil
	}
	enqueue := func(payload []byte, depth int) bool {
		if len(queue) >= maximumSensitiveQueue || int64(len(payload)) > maximumSensitiveQueued-queuedBytes {
			return false
		}
		queue = append(queue, scanItem{payload: payload, depth: depth})
		queuedBytes += int64(len(payload))
		return true
	}
	if workRemaining > maximumSensitiveWork {
		workRemaining = maximumSensitiveWork
	}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		current := queue[0]
		queue = queue[1:]
		if current.depth > 0 {
			queuedBytes -= int64(len(current.payload))
		}
		if current.depth > maximumSensitiveDepth {
			// A payload that deliberately exceeds the inspection contract is
			// unsafe to retain even when no decoded secret has been reached yet.
			return true, nil
		}
		workRemaining -= int64(len(current.payload))
		if workRemaining < 0 {
			return true, nil
		}
		decodedStrings, completeJSON, bounded, err := completeJSONStringsContext(
			ctx, current.payload, maximumSensitiveQueued,
		)
		if err != nil {
			return false, err
		}
		if !bounded {
			return true, nil
		}
		if completeJSON {
			for _, decoded := range decodedStrings {
				baseRole := decodedValueState
				if decoded.key {
					baseRole = decodedKeyState
				}
				matched, err := advanceEscaped(
					decoded.payload, decoded.key, decodedAllState, baseRole,
				)
				if err != nil || matched {
					return matched, err
				}
				matched, err = advanceDecoded(decoded.payload, decoded.key)
				if err != nil || matched {
					return matched, err
				}
				if decoded.encoded && possiblyEncodedJSON(decoded.payload) {
					if !enqueue(decoded.payload, current.depth+1) {
						return true, nil
					}
				}
			}
			continue
		}
		embedded, bounded, err := embeddedJSONCandidatesContext(
			ctx, current.payload, maximumSensitiveQueue,
		)
		if err != nil {
			return false, err
		}
		if !bounded {
			return true, nil
		}
		for _, candidate := range embedded {
			if !enqueue(candidate, current.depth+1) {
				return true, nil
			}
		}
		if decodedEscapes, changed, decodeErr := decodeJSONEscapesAnywhereContext(
			ctx, current.payload,
		); decodeErr != nil {
			return false, decodeErr
		} else if changed {
			matched, err := advanceEscaped(
				current.payload, false, decodedAllState, decodedValueState,
			)
			if err != nil || matched {
				return matched, err
			}
			matched, err = advanceDecoded(decodedEscapes, false)
			if err != nil || matched {
				return matched, err
			}
			if possiblyEncodedJSON(decodedEscapes) {
				if !enqueue(decodedEscapes, current.depth+1) {
					return true, nil
				}
			}
		}
		for start := 0; start < len(current.payload); start++ {
			if start&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if current.payload[start] != '"' {
				continue
			}
			end, scanned, ok := embeddedJSONStringEndContext(ctx, current.payload, start)
			workRemaining -= int64(scanned)
			if workRemaining < 0 {
				return true, nil
			}
			if !ok {
				continue
			}
			var decoded string
			if json.Unmarshal(current.payload[start:end], &decoded) != nil {
				continue
			}
			decodedBytes := []byte(decoded)
			matched, err := advanceEscaped(
				decodedBytes, false, decodedAllState, decodedValueState,
			)
			if err != nil || matched {
				return matched, err
			}
			matched, err = advanceDecoded(decodedBytes, false)
			if err != nil || matched {
				return matched, err
			}
			if possiblyEncodedJSON(decodedBytes) {
				if !enqueue(decodedBytes, current.depth+1) {
					return true, nil
				}
			}
		}
	}
	return false, ctx.Err()
}

type decodedJSONString struct {
	payload []byte
	key     bool
	encoded bool
}

func completeJSONStringsContext(
	ctx context.Context, payload []byte, maximumDecoded int64,
) ([]decodedJSONString, bool, bool, error) {
	if ctx == nil {
		return nil, false, false, errors.New("review JSON token scan requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var result []decodedJSONString
	type containerState struct {
		object       bool
		expectingKey bool
	}
	var stack []containerState
	seen := false
	decodedBytes := int64(0)
	tokens := 0
	for {
		tokens++
		if tokens&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, false, err
			}
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return result, seen, true, ctx.Err()
		}
		if err != nil {
			return nil, false, true, nil
		}
		seen = true
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if len(stack) > 0 && stack[len(stack)-1].object &&
					!stack[len(stack)-1].expectingKey {
					stack[len(stack)-1].expectingKey = true
				}
				stack = append(stack, containerState{
					object: delimiter == '{', expectingKey: delimiter == '{',
				})
			case '}', ']':
				if len(stack) == 0 {
					return nil, false, true, nil
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		isKey := len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectingKey
		if isKey {
			stack[len(stack)-1].expectingKey = false
		} else if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectingKey = true
		}
		decoded := decodedJSONString{key: isKey}
		switch value := token.(type) {
		case string:
			decoded.payload = []byte(value)
			decoded.encoded = true
		case json.Number:
			decoded.payload = []byte(value.String())
		case bool:
			decoded.payload = []byte(strconv.FormatBool(value))
		case nil:
			decoded.payload = []byte("null")
		default:
			continue
		}
		if len(result) >= maximumSensitiveQueue ||
			int64(len(decoded.payload)) > maximumDecoded-decodedBytes {
			return nil, false, false, nil
		}
		decodedBytes += int64(len(decoded.payload))
		result = append(result, decoded)
	}
}

func possiblyEncodedJSON(payload []byte) bool {
	return bytes.IndexByte(payload, '"') >= 0 || bytes.IndexByte(payload, '\\') >= 0 ||
		bytes.IndexAny(payload, "[{") >= 0
}

func decodeJSONEscapesAnywhere(source []byte) ([]byte, bool) {
	decoded, changed, _ := decodeJSONEscapesAnywhereContext(context.Background(), source)
	return decoded, changed
}

func decodeJSONEscapesAnywhereContext(
	ctx context.Context, source []byte,
) ([]byte, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("review JSON escape scan requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if bytes.IndexByte(source, '\\') < 0 {
		return nil, false, nil
	}
	result := make([]byte, 0, len(source))
	changed := false
	for index := 0; index < len(source); {
		if index&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if source[index] != '\\' || index+1 >= len(source) {
			result = append(result, source[index])
			index++
			continue
		}
		switch source[index+1] {
		case '"', '\\', '/':
			result = append(result, source[index+1])
			index += 2
			changed = true
		case 'b', 'f', 'n', 'r', 't':
			decoded := byte('\t')
			switch source[index+1] {
			case 'b':
				decoded = '\b'
			case 'f':
				decoded = '\f'
			case 'n':
				decoded = '\n'
			case 'r':
				decoded = '\r'
			}
			result = append(result, decoded)
			index += 2
			changed = true
		case 'u':
			first, ok := jsonHexRune(source, index+2)
			if !ok {
				result = append(result, source[index])
				index++
				continue
			}
			consumed := 6
			decoded := rune(first)
			if utf16.IsSurrogate(decoded) {
				if index+12 > len(source) || source[index+6] != '\\' || source[index+7] != 'u' {
					result = append(result, source[index])
					index++
					continue
				}
				second, secondOK := jsonHexRune(source, index+8)
				if !secondOK {
					result = append(result, source[index])
					index++
					continue
				}
				decoded = utf16.DecodeRune(rune(first), rune(second))
				if decoded == unicode.ReplacementChar {
					result = append(result, source[index])
					index++
					continue
				}
				consumed = 12
			}
			result = utf8.AppendRune(result, decoded)
			index += consumed
			changed = true
		default:
			result = append(result, source[index])
			index++
		}
	}
	if !changed {
		return nil, false, ctx.Err()
	}
	return result, true, ctx.Err()
}

func jsonHexRune(source []byte, offset int) (uint16, bool) {
	if offset < 0 || len(source)-offset < 4 {
		return 0, false
	}
	var value uint16
	for _, character := range source[offset : offset+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func embeddedJSONStringEnd(payload []byte, start int) (int, int, bool) {
	return embeddedJSONStringEndContext(context.Background(), payload, start)
}

func embeddedJSONStringEndContext(
	ctx context.Context, payload []byte, start int,
) (int, int, bool) {
	if ctx == nil || ctx.Err() != nil {
		return 0, 0, false
	}
	if start < 0 || start >= len(payload) || payload[start] != '"' {
		return 0, 0, false
	}
	escaped := false
	for index := start + 1; index < len(payload); index++ {
		if index&4095 == 0 && ctx.Err() != nil {
			return 0, index - start + 1, false
		}
		character := payload[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			return index + 1, index - start + 1, true
		}
	}
	return 0, len(payload) - start, false
}

func embeddedJSONCandidatesContext(
	ctx context.Context, payload []byte, maximumCandidates int,
) ([][]byte, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("embedded JSON scan requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	var result [][]byte
	workRemaining := int64(len(payload))*4 + 1
	for start := 0; start < len(payload); start++ {
		if start&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if payload[start] != '{' && payload[start] != '[' {
			continue
		}
		end, scanned, ok := embeddedJSONValueEndContext(ctx, payload, start)
		workRemaining -= int64(scanned)
		if workRemaining < 0 {
			return nil, false, nil
		}
		if !ok || !json.Valid(payload[start:end]) {
			continue
		}
		if len(result) >= maximumCandidates {
			return nil, false, nil
		}
		result = append(result, payload[start:end])
		start = end - 1
	}
	return result, true, ctx.Err()
}

func embeddedJSONValueEndContext(
	ctx context.Context, payload []byte, start int,
) (int, int, bool) {
	if ctx == nil || start < 0 || start >= len(payload) ||
		(payload[start] != '{' && payload[start] != '[') {
		return 0, 0, false
	}
	stack := []byte{'}'}
	if payload[start] == '[' {
		stack[0] = ']'
	}
	inString, escaped := false, false
	for index := start + 1; index < len(payload); index++ {
		if index&4095 == 0 && ctx.Err() != nil {
			return 0, index - start, false
		}
		character := payload[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{':
			if len(stack) >= maximumSensitiveDepth {
				return 0, index - start + 1, false
			}
			stack = append(stack, '}')
		case '[':
			if len(stack) >= maximumSensitiveDepth {
				return 0, index - start + 1, false
			}
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || character != stack[len(stack)-1] {
				return 0, index - start + 1, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return index + 1, index - start + 1, true
			}
		}
	}
	return 0, len(payload) - start, false
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
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

func digestContext(ctx context.Context, payload []byte) (string, error) {
	if ctx == nil {
		return "", errors.New("SHA-256 digest requires a context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hasher := sha256.New()
	for offset := 0; offset < len(payload); {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		end := min(offset+(64<<10), len(payload))
		_, _ = hasher.Write(payload[offset:end])
		offset = end
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
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

func cloneByteSlices(source [][]byte) [][]byte {
	result := make([][]byte, len(source))
	for index := range source {
		result[index] = slices.Clone(source[index])
	}
	return result
}

func cloneProviderResponse(source ProviderResponse) ProviderResponse {
	result := source
	result.Raw = slices.Clone(source.Raw)
	result.Output = slices.Clone(source.Output)
	result.Request = slices.Clone(source.Request)
	return result
}
