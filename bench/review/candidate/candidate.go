// Package candidate defines the suite-neutral evidence plug-in boundary for
// new benchmark attempts. It deliberately has no representation of historical
// runs: trusted baseline numbers are acceptance targets, while this package
// records only media and deterministic results produced by the current run.
package candidate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"unicode"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	OriginProduction     = "production-shared-realtime"
	OriginHermetic       = "hermetic-test"
	MediaSharedSession   = "shared-session-callbacks"
	MediaExternalHarness = "external-harness-artifacts"

	maximumContextBytes  = 4 << 20
	maximumIdentityBytes = 1024
	maximumTrial         = 1_000_000
)

// RunOrigin binds an attempt to the shared Realtime API without retaining an
// endpoint, query parameter, credential, or presentation-client detail.
// Hermetic attempts remain useful diagnostics but cannot support publication.
type RunOrigin struct {
	Kind           string `json:"kind"`
	Live           bool   `json:"live"`
	Transport      string `json:"transport"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

// NewRunOrigin creates a public endpoint identity. User information, query
// parameters, and fragments are removed before hashing so credential-bearing
// URLs never create retained credential fingerprints.
func NewRunOrigin(kind, transport, endpoint string) (RunOrigin, error) {
	if kind != OriginProduction && kind != OriginHermetic {
		return RunOrigin{}, errors.New("candidate run origin kind is invalid")
	}
	transport = strings.ToLower(strings.TrimSpace(transport))
	if transport == "" {
		transport = bench.TransportWebSocket
	}
	if transport != bench.TransportWebSocket && transport != bench.TransportWebRTC {
		return RunOrigin{}, errors.New("candidate run origin transport is invalid")
	}
	publicEndpoint, err := endpointIdentityInput(endpoint)
	if err != nil {
		return RunOrigin{}, err
	}
	digest := sha256.Sum256([]byte(publicEndpoint))
	origin := RunOrigin{
		Kind: kind, Live: kind == OriginProduction, Transport: transport,
		EndpointSHA256: "sha256:" + hex.EncodeToString(digest[:]),
	}
	if err := origin.Validate(); err != nil {
		return RunOrigin{}, err
	}
	return origin, nil
}

func endpointIdentityInput(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || len(endpoint) > 16<<10 {
		return "", errors.New("candidate run endpoint is empty or oversized")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", errors.New("candidate run endpoint must be an absolute network URL")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "ws", "wss":
	default:
		return "", errors.New("candidate run endpoint scheme is unsupported")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String(), nil
}

// Validate checks a retained run origin without requiring the original
// endpoint to remain available.
func (origin RunOrigin) Validate() error {
	if origin.Kind != OriginProduction && origin.Kind != OriginHermetic {
		return errors.New("candidate run origin kind is invalid")
	}
	if origin.Live != (origin.Kind == OriginProduction) {
		return errors.New("candidate run origin live status is inconsistent")
	}
	if origin.Transport != bench.TransportWebSocket && origin.Transport != bench.TransportWebRTC {
		return errors.New("candidate run origin transport is invalid")
	}
	if len(origin.EndpointSHA256) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(origin.EndpointSHA256, "sha256:") {
		return errors.New("candidate run endpoint identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(origin.EndpointSHA256, "sha256:")); err != nil {
		return errors.New("candidate run endpoint identity is invalid")
	}
	return nil
}

// Attempt identifies one preregistered candidate attempt before a session is
// opened. Context is suite-owned deterministic JSON: task annotations and
// scorer criteria belong here, while secrets and provider configuration do
// not. ExecutionRequirement must be exactly the requirement in Cell.
type Attempt struct {
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	Cell                 bench.Cell                 `json:"cell"`
	Provenance           bench.Provenance           `json:"provenance"`
	Origin               RunOrigin                  `json:"run_origin"`
	MediaSource          string                     `json:"media_source"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
	Context              json.RawMessage            `json:"context"`
}

// NewAttempt snapshots a suite-owned context into the canonical candidate
// contract. Passing a struct is preferred; maps are accepted because the JSON
// encoder sorts their keys deterministically.
func NewAttempt(
	suite, caseID string, trial int, cell bench.Cell, provenance bench.Provenance,
	origin RunOrigin, contextValue any,
) (Attempt, error) {
	return newAttempt(
		suite, caseID, trial, cell, provenance, origin, MediaSharedSession, contextValue,
	)
}

// NewExternalAttempt snapshots an attempt whose client and media are owned by
// a pinned external harness. It never implies that OpenRealtime observed the
// session callbacks itself.
func NewExternalAttempt(
	suite, caseID string, trial int, cell bench.Cell, provenance bench.Provenance,
	origin RunOrigin, contextValue any,
) (Attempt, error) {
	return newAttempt(
		suite, caseID, trial, cell, provenance, origin, MediaExternalHarness, contextValue,
	)
}

func newAttempt(
	suite, caseID string, trial int, cell bench.Cell, provenance bench.Provenance,
	origin RunOrigin, mediaSource string, contextValue any,
) (Attempt, error) {
	contextJSON, err := canonicalContext(contextValue)
	if err != nil {
		return Attempt{}, err
	}
	attempt := Attempt{
		Suite: suite, Case: caseID, Trial: trial, Cell: cloneCell(cell),
		Provenance: provenance, Origin: origin, MediaSource: mediaSource,
		ExecutionRequirement: cloneRequirement(cell.Execution),
		Context:              contextJSON,
	}
	if err := attempt.Validate(); err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

// ID is the stable human-readable identity used by review-provider requests.
func (attempt Attempt) ID() string {
	return fmt.Sprintf("%s:%s#%d", attempt.Suite, attempt.Case, attempt.Trial)
}

// Validate checks the immutable candidate-attempt contract.
func (attempt Attempt) Validate() error {
	if err := validateIdentity("candidate suite", attempt.Suite); err != nil {
		return err
	}
	if err := validateIdentity("candidate case", attempt.Case); err != nil {
		return err
	}
	if attempt.Trial <= 0 || attempt.Trial > maximumTrial {
		return errors.New("candidate trial is outside the supported range")
	}
	if err := validateCell(attempt.Cell); err != nil {
		return err
	}
	if err := attempt.Origin.Validate(); err != nil {
		return err
	}
	if attempt.MediaSource != MediaSharedSession && attempt.MediaSource != MediaExternalHarness {
		return errors.New("candidate media source is invalid")
	}
	if err := attempt.ExecutionRequirement.Validate(); err != nil {
		return fmt.Errorf("candidate execution requirement: %w", err)
	}
	if !reflect.DeepEqual(
		cloneRequirement(attempt.Cell.Execution), cloneRequirement(attempt.ExecutionRequirement),
	) {
		return errors.New("candidate execution requirement differs from its cell")
	}
	canonical, err := canonicalContext(attempt.Context)
	if err != nil {
		return err
	}
	if !bytes.Equal(attempt.Context, canonical) {
		return errors.New("candidate context is not canonical JSON")
	}
	return nil
}

func validateIdentity(label, value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maximumIdentityBytes {
		return errors.New(label + " is empty, noncanonical, or oversized")
	}
	for _, symbol := range value {
		if unicode.IsControl(symbol) {
			return errors.New(label + " contains a control character")
		}
	}
	return nil
}

func validateCell(cell bench.Cell) error {
	if err := validateIdentity("candidate cell name", cell.Name); err != nil {
		return err
	}
	if len(cell.Levels) == 0 || len(cell.Levels) > 128 {
		return errors.New("candidate cell needs a bounded non-empty level set")
	}
	for factor, level := range cell.Levels {
		if err := validateIdentity("candidate cell factor", string(factor)); err != nil {
			return err
		}
		if err := validateIdentity("candidate cell level", level); err != nil {
			return err
		}
	}
	if err := cell.Execution.Validate(); err != nil {
		return fmt.Errorf("candidate cell execution requirement: %w", err)
	}
	return nil
}

func canonicalContext(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("candidate context must be bounded JSON: %w", err)
	}
	if len(payload) == 0 || len(payload) > maximumContextBytes {
		return nil, errors.New("candidate context must be bounded JSON")
	}
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumContextBytes, MaxDepth: 64, MaxTokens: 250_000,
		MaxObjectMembers: 50_000, MaxArrayElements: 100_000,
		MaxKeyBytes: 16 << 10, MaxTotalKeyBytes: maximumContextBytes,
		MaxWorkBytes: 16 << 20,
	}); err != nil {
		return nil, fmt.Errorf("candidate context is not strict JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, errors.New("candidate context cannot be decoded")
	}
	if _, object := decoded.(map[string]any); !object {
		return nil, errors.New("candidate context must be a JSON object")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || len(canonical) > maximumContextBytes {
		return nil, errors.New("candidate context cannot be canonicalized")
	}
	return json.RawMessage(canonical), nil
}

// Completion is the exact deterministic outcome and transcript delivered
// after scoring. A review plug-in may annotate it, but cannot change Passed.
type Completion struct {
	Attempt    Attempt           `json:"attempt"`
	Outcome    bench.TaskOutcome `json:"outcome"`
	Transcript bench.Transcript  `json:"transcript"`
}

// EvidenceError identifies which candidate-retention stage failed without
// conflating it with the deterministic scorer outcome.
type EvidenceError struct {
	Case  string
	Stage string
	cause error
}

func (failure *EvidenceError) Error() string {
	if failure == nil {
		return "candidate evidence failed"
	}
	message := "candidate evidence"
	if failure.Case != "" {
		message += " for " + failure.Case
	}
	if failure.Stage != "" {
		message += " during " + failure.Stage
	}
	if failure.cause == nil {
		return message + " failed"
	}
	return message + ": " + failure.cause.Error()
}

func (failure *EvidenceError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

// StageError wraps one non-nil plug-in failure with its attempt and lifecycle
// stage. Nil remains nil so callers can use it directly with errors.Join.
func StageError(caseID, stage string, err error) error {
	if err == nil {
		return nil
	}
	return &EvidenceError{Case: caseID, Stage: stage, cause: err}
}

// Validate checks that a terminal record still belongs to its preregistered
// attempt. Incomplete outcomes are valid evidence and must be retained too.
func (completion Completion) Validate() error {
	if err := completion.Attempt.Validate(); err != nil {
		return err
	}
	if completion.Outcome.ID != completion.Attempt.Case {
		return errors.New("candidate outcome belongs to a different case")
	}
	return nil
}

// CloneAttempt returns a deep copy safe to hand to an untrusted plug-in.
func CloneAttempt(source Attempt) (Attempt, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return Attempt{}, errors.New("snapshot candidate attempt")
	}
	var result Attempt
	if err := json.Unmarshal(payload, &result); err != nil {
		return Attempt{}, errors.New("snapshot candidate attempt")
	}
	if err := result.Validate(); err != nil {
		return Attempt{}, err
	}
	return result, nil
}

// CloneCompletion returns a deep copy safe to retain after the runner exits.
func CloneCompletion(source Completion) (Completion, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return Completion{}, errors.New("snapshot candidate completion")
	}
	var result Completion
	if err := json.Unmarshal(payload, &result); err != nil {
		return Completion{}, errors.New("snapshot candidate completion")
	}
	if err := result.Validate(); err != nil {
		return Completion{}, err
	}
	return result, nil
}

// CloneResult freezes the final deterministic cell before a plug-in sees it.
func CloneResult(source bench.Result) (bench.Result, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return bench.Result{}, errors.New("snapshot candidate result")
	}
	var result bench.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		return bench.Result{}, errors.New("snapshot candidate result")
	}
	return result, nil
}

func cloneCell(source bench.Cell) bench.Cell {
	result := source
	result.Levels = make(map[bench.Factor]string, len(source.Levels))
	for factor, level := range source.Levels {
		result.Levels[factor] = level
	}
	result.Varies = slices.Clone(source.Varies)
	result.Execution = cloneRequirement(source.Execution)
	return result
}

func cloneRequirement(source bench.ExecutionRequirement) bench.ExecutionRequirement {
	if !source.Required() {
		return source
	}
	payload, err := bench.MarshalExecutionRequirement(source)
	if err != nil {
		return bench.ExecutionRequirement{}
	}
	result, err := bench.ParseExecutionRequirement(payload)
	if err != nil {
		return bench.ExecutionRequirement{}
	}
	return result
}

// AttemptEvidence receives owned media for exactly one candidate attempt,
// followed by one Complete or Abort call. Session-driven suites call the
// audio/video methods; an external-harness attempt may additionally implement
// CapturedMediaEvidence for artifacts produced by that harness.
type AttemptEvidence interface {
	CaptureAudio(bench.SessionAudioCapture) error
	CaptureVideo(bench.SessionVideoCapture) error
	Complete(context.Context, Completion) error
	Abort() error
}

// Plugin is the provider-, storage-, encoder-, server-, and UI-neutral
// candidate evidence extension point. The runner remains the deterministic
// authority and supplies every external capability through this interface.
type Plugin interface {
	BeginAttempt(context.Context, Attempt) (AttemptEvidence, error)
	FinishSuite(context.Context, bench.Result) error
}
