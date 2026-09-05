package graphnative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

const (
	ChecklistFormatVersion        uint64 = 1
	MinimumReportableRepetitions         = 15
	maximumChecklistRepetitions          = 1_000
	maximumChecklistTextBytes            = 64 << 10
	maximumBehaviorFailures              = 256
	maximumInfrastructureFailures        = 64
	maximumVideoSources                  = 32
)

const (
	MediaPolicyRequired   = "required"
	MediaPolicyUnattested = "unattested"

	BehaviorPassed   = "passed"
	BehaviorFailed   = "failed"
	BehaviorUnscored = "unscored"
)

// AttemptKey is the canonical identity shared by the executor, attestor,
// media verifier, and checklist sink. It deliberately matches the public
// scenario client's task scope so live inspection cannot be attributed to a
// different repetition.
type AttemptKey struct {
	CaseOrdinal int    `json:"case_ordinal"`
	CaseName    string `json:"case_name"`
	Trial       int    `json:"trial"`
	TaskID      string `json:"task_id"`
}

// SubmittedInputReceipt identifies bytes actually submitted by the harness.
// The verifier, not this package, establishes the digest from retained bytes.
// SightID and CueMS bind the receipt to the authored scenario timeline.
type SubmittedInputReceipt struct {
	SightID   string `json:"sight_id"`
	CueMS     int    `json:"cue_ms"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

// MediaReference is an opaque verifier-owned handle plus the externally
// retained completion digest. The checklist never opens a directory or
// assumes a media implementation.
type MediaReference struct {
	Handle         string                  `json:"handle"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	Submitted      []SubmittedInputReceipt `json:"submitted_inputs,omitempty"`
}

// VerifiedMedia is the bounded projection returned by the selected media
// verifier after it has independently checked the referenced bundle. Audio
// and submitted stills remain separate: a still-image message is not silently
// reclassified as a continuous video source.
type VerifiedMedia struct {
	Handle         string                  `json:"handle"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	Audio          bool                    `json:"audio"`
	VideoSources   []string                `json:"video_sources,omitempty"`
	Submitted      []SubmittedInputReceipt `json:"submitted_inputs,omitempty"`
}

// AttemptObservation is returned by the caller-selected executor. Result is
// the repository scenario scorer's output; Media only names evidence for a
// verifier and is never accepted by assertion.
type AttemptObservation struct {
	Result scenario.Result
	Media  *MediaReference
}

type AttemptExecutor func(
	context.Context, AttemptKey, scenario.Scenario,
) (AttemptObservation, error)

type MediaVerifier func(
	context.Context, AttemptKey, CaseRequirement, MediaReference,
) (VerifiedMedia, error)

// ErrorSanitizer may retain a bounded, redacted detail for infrastructure
// diagnosis. With no sanitizer, stable failure codes are retained without
// copying provider or transport errors into the checklist.
type ErrorSanitizer func(string) string

// ChecklistSink is a caller-selected retention plugin. Attempt is invoked
// after each independently fingerprinted record; Finalize receives the exact
// final checklist. Supplying only one callback is rejected before execution.
type ChecklistSink struct {
	Attempt  func(context.Context, AttemptRecord) error
	Finalize func(context.Context, Checklist) error
}

type ChecklistConfig struct {
	Contract                  Contract
	Profile                   launchprofile.Document
	AdapterProfileFingerprint string
	ExecutionRequirement      bench.ExecutionRequirement
	Repetitions               int
	RequireMedia              bool
	Executor                  AttemptExecutor
	VerifyMedia               MediaVerifier
	SanitizeError             ErrorSanitizer
	Sink                      ChecklistSink
}

type AttemptFailure struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// AttemptExecution makes each row independently attributable to the complete
// frozen selection. ResultSHA256 binds the retained scorer result, while
// EvidenceFingerprint refers to the separately retained bench.ExecutionEvidence
// whose exact content was matched before Validated was set. Expected
// plan/profile identities are not inferred from either digest.
type AttemptExecution struct {
	ContractFingerprint       string `json:"contract_fingerprint"`
	LaunchProfileFingerprint  string `json:"launch_profile_fingerprint"`
	PlanFingerprint           string `json:"plan_fingerprint"`
	GraphFingerprint          string `json:"graph_fingerprint"`
	AdapterProfileFingerprint string `json:"adapter_profile_fingerprint"`
	ResultSHA256              string `json:"result_sha256,omitempty"`
	EvidenceFingerprint       string `json:"evidence_fingerprint,omitempty"`
	Validated                 bool   `json:"validated"`
}

type AttemptRecord struct {
	FormatVersion  uint64           `json:"format_version"`
	Fingerprint    string           `json:"fingerprint"`
	Key            AttemptKey       `json:"key"`
	Behavior       string           `json:"behavior"`
	Failures       []string         `json:"behavior_failures,omitempty"`
	Infrastructure []AttemptFailure `json:"infrastructure_failures,omitempty"`
	Execution      AttemptExecution `json:"execution"`
	Media          *VerifiedMedia   `json:"media,omitempty"`
	Reportable     bool             `json:"reportable"`
}

type CaseChecklist struct {
	Ordinal              int    `json:"ordinal"`
	Name                 string `json:"name"`
	Expected             int    `json:"expected"`
	Executed             int    `json:"executed"`
	Reportable           int    `json:"reportable"`
	Passed               int    `json:"passed"`
	Failed               int    `json:"failed"`
	InfrastructureFailed int    `json:"infrastructure_failed"`
}

// Checklist is the deterministic case-by-case review index. Complete means
// every configured attempt returned a record. Reportable additionally needs
// the complete twelve-case contract, the reviewed repetition floor, exact
// graph evidence, and verifier-backed media for every row. Passed is only the
// behavioral result after those evidence conditions hold.
type Checklist struct {
	FormatVersion              uint64               `json:"format_version"`
	Fingerprint                string               `json:"fingerprint"`
	Suite                      string               `json:"suite"`
	ContractFingerprint        string               `json:"contract_fingerprint"`
	LaunchProfileFingerprint   string               `json:"launch_profile_fingerprint"`
	Plan                       graphconfig.Identity `json:"plan"`
	AdapterProfileFingerprint  string               `json:"adapter_profile_fingerprint"`
	ExecutionRequirementSHA256 string               `json:"execution_requirement_sha256"`
	MediaPolicy                string               `json:"media_policy"`
	Repetitions                int                  `json:"repetitions"`
	Expected                   int                  `json:"expected"`
	Executed                   int                  `json:"executed"`
	ReportableAttempts         int                  `json:"reportable_attempts"`
	PassedAttempts             int                  `json:"passed_attempts"`
	FailedAttempts             int                  `json:"failed_attempts"`
	InfrastructureFailures     int                  `json:"infrastructure_failures"`
	FullSuite                  bool                 `json:"full_suite"`
	Complete                   bool                 `json:"complete"`
	Reportable                 bool                 `json:"reportable"`
	Passed                     bool                 `json:"passed"`
	Cases                      []CaseChecklist      `json:"cases"`
	Attempts                   []AttemptRecord      `json:"attempts"`
}

type frozenChecklistConfig struct {
	contract                  Contract
	profile                   launchprofile.Document
	adapterProfileFingerprint string
	requirement               bench.ExecutionRequirement
	requirementSHA256         string
	repetitions               int
	requireMedia              bool
	executor                  AttemptExecutor
	verifyMedia               MediaVerifier
	sanitize                  ErrorSanitizer
	sink                      ChecklistSink
	fullSuite                 bool
}

// ValidateChecklistConfig verifies and freezes the complete checklist
// selection without invoking the executor, media verifier, or retention sink.
// Hosts can therefore reject a drifted contract/profile/evidence composition
// before they open credentials, storage, speech, or Realtime resources.
func ValidateChecklistConfig(config ChecklistConfig) error {
	_, err := freezeChecklistConfig(config)
	return err
}

// RunChecklist executes the canonical case/trial order. Behavioral and
// infrastructure failures are records, not early returns, so a bad first case
// cannot erase the other ten. Context cancellation and retention-plugin
// failure stop the run because continuing could not produce honest evidence.
func RunChecklist(ctx context.Context, config ChecklistConfig) (Checklist, error) {
	if ctx == nil {
		return Checklist{}, errors.New("run scenario checklist: nil context")
	}
	frozen, err := freezeChecklistConfig(config)
	if err != nil {
		return Checklist{}, err
	}
	checklist := newChecklist(frozen)
	mediaHandles := make(map[string]AttemptKey, checklist.Expected)
	mediaManifests := make(map[string]AttemptKey, checklist.Expected)
	for caseIndex, requirement := range frozen.contract.Cases {
		item := scenarioForChecklist(requirement.Name)
		for trial := 1; trial <= frozen.repetitions; trial++ {
			if cause := context.Cause(ctx); cause != nil {
				return finishChecklist(ctx, frozen, checklist, cause)
			}
			key := AttemptKey{
				CaseOrdinal: caseIndex + 1, CaseName: requirement.Name, Trial: trial,
				TaskID: requirement.Name + "#" + strconv.Itoa(trial),
			}
			observation, runErr := frozen.executor(ctx, key, cloneScenario(item))
			record := buildAttemptRecord(ctx, frozen, key, requirement, item, observation, runErr)
			if record.Media != nil {
				duplicate := false
				if previous, reused := mediaHandles[record.Media.Handle]; reused {
					record.Infrastructure = append(record.Infrastructure, AttemptFailure{
						Code: "media.reused",
						Detail: checklistErrorDetail(frozen.sanitize, fmt.Errorf(
							"media handle was already retained for %s", previous.TaskID,
						)),
					})
					duplicate = true
				}
				if previous, reused := mediaManifests[record.Media.ManifestSHA256]; reused {
					record.Infrastructure = append(record.Infrastructure, AttemptFailure{
						Code: "media.manifest_reused",
						Detail: checklistErrorDetail(frozen.sanitize, fmt.Errorf(
							"media manifest was already retained for %s", previous.TaskID,
						)),
					})
					duplicate = true
				}
				if duplicate {
					record.Media = nil
					record.Reportable = false
					record.Fingerprint = fingerprintAttempt(record)
				} else {
					mediaHandles[record.Media.Handle] = key
					mediaManifests[record.Media.ManifestSHA256] = key
				}
			}
			checklist.Attempts = append(checklist.Attempts, record)
			checklist = summarizeChecklist(checklist)
			if frozen.sink.Attempt != nil {
				if sinkErr := frozen.sink.Attempt(ctx, record.Clone()); sinkErr != nil {
					sealed, freezeErr := sealChecklist(checklist)
					return sealed, errors.Join(
						fmt.Errorf("retain scenario checklist attempt %s: %w", key.TaskID, sinkErr),
						freezeErr,
					)
				}
			}
			if cause := context.Cause(ctx); cause != nil {
				return finishChecklist(ctx, frozen, checklist, cause)
			}
		}
	}
	return finishChecklist(ctx, frozen, checklist, nil)
}

func freezeChecklistConfig(config ChecklistConfig) (frozenChecklistConfig, error) {
	if config.Executor == nil {
		return frozenChecklistConfig{}, errors.New("scenario checklist requires an executor plugin")
	}
	if config.Repetitions < 1 || config.Repetitions > maximumChecklistRepetitions {
		return frozenChecklistConfig{}, fmt.Errorf(
			"scenario checklist repetitions must be between 1 and %d", maximumChecklistRepetitions,
		)
	}
	if config.RequireMedia && config.VerifyMedia == nil {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist required media needs a verifier plugin",
		)
	}
	if (config.Sink.Attempt == nil) != (config.Sink.Finalize == nil) {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist sink must supply both attempt and finalize callbacks",
		)
	}
	contract := config.Contract.Clone()
	if err := contract.Validate(); err != nil {
		return frozenChecklistConfig{}, fmt.Errorf("scenario checklist contract: %w", err)
	}
	profile := config.Profile.Clone()
	if err := profile.Validate(); err != nil {
		return frozenChecklistConfig{}, fmt.Errorf("scenario checklist launch profile: %w", err)
	}
	profileContract, _, err := decodeApplicationConfiguration(profile.Application.Configuration)
	if err != nil {
		return frozenChecklistConfig{}, fmt.Errorf("scenario checklist application contract: %w", err)
	}
	if !reflect.DeepEqual(profileContract, contract) {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist contract differs from the frozen application profile",
		)
	}
	if !validChecklistDigest(config.AdapterProfileFingerprint) {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist adapter profile fingerprint is not canonical SHA-256",
		)
	}
	requirementPayload, err := bench.MarshalExecutionRequirement(config.ExecutionRequirement)
	if err != nil {
		return frozenChecklistConfig{}, fmt.Errorf("scenario checklist execution requirement: %w", err)
	}
	requirement, err := bench.ParseExecutionRequirement(requirementPayload)
	if err != nil {
		return frozenChecklistConfig{}, fmt.Errorf("scenario checklist execution requirement: %w", err)
	}
	if requirement.Kind != bench.ExecutionGraphNative || requirement.Graph == nil {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist requires exact graph-native execution evidence",
		)
	}
	expectedGraph := requirement.Graph.Graph
	if expectedGraph.ID != profile.Plan.GraphID || expectedGraph.Revision != profile.Plan.GraphRevision ||
		expectedGraph.Fingerprint != profile.Plan.GraphFingerprint {
		return frozenChecklistConfig{}, errors.New(
			"scenario checklist execution requirement differs from the frozen plan graph",
		)
	}
	full, err := BuildContract()
	if err != nil {
		return frozenChecklistConfig{}, err
	}
	digest := sha256.Sum256(requirementPayload)
	return frozenChecklistConfig{
		contract: contract, profile: profile,
		adapterProfileFingerprint: config.AdapterProfileFingerprint,
		requirement:               requirement,
		requirementSHA256:         "sha256:" + hex.EncodeToString(digest[:]),
		repetitions:               config.Repetitions, requireMedia: config.RequireMedia,
		executor: config.Executor, verifyMedia: config.VerifyMedia,
		sanitize: config.SanitizeError, sink: config.Sink,
		fullSuite: reflect.DeepEqual(contract, full),
	}, nil
}

func newChecklist(config frozenChecklistConfig) Checklist {
	policy := MediaPolicyUnattested
	if config.requireMedia {
		policy = MediaPolicyRequired
	}
	cases := make([]CaseChecklist, len(config.contract.Cases))
	for index, requirement := range config.contract.Cases {
		cases[index] = CaseChecklist{
			Ordinal: index + 1, Name: requirement.Name, Expected: config.repetitions,
		}
	}
	return summarizeChecklist(Checklist{
		FormatVersion: ChecklistFormatVersion, Suite: SuiteName,
		ContractFingerprint:        config.contract.Fingerprint,
		LaunchProfileFingerprint:   config.profile.Fingerprint,
		Plan:                       config.profile.Plan,
		AdapterProfileFingerprint:  config.adapterProfileFingerprint,
		ExecutionRequirementSHA256: config.requirementSHA256,
		MediaPolicy:                policy, Repetitions: config.repetitions,
		Expected:  len(config.contract.Cases) * config.repetitions,
		FullSuite: config.fullSuite, Cases: cases,
	})
}

func buildAttemptRecord(
	ctx context.Context,
	config frozenChecklistConfig,
	key AttemptKey,
	requirement CaseRequirement,
	item scenario.Scenario,
	observation AttemptObservation,
	runErr error,
) AttemptRecord {
	record := AttemptRecord{
		FormatVersion: ChecklistFormatVersion, Key: key, Behavior: BehaviorUnscored,
		Execution: AttemptExecution{
			ContractFingerprint:       config.contract.Fingerprint,
			LaunchProfileFingerprint:  config.profile.Fingerprint,
			PlanFingerprint:           config.profile.Plan.PlanFingerprint,
			GraphFingerprint:          config.profile.Plan.GraphFingerprint,
			AdapterProfileFingerprint: config.adapterProfileFingerprint,
		},
	}
	addFailure := func(code string, err error) {
		record.Infrastructure = append(record.Infrastructure, AttemptFailure{
			Code: code, Detail: checklistErrorDetail(config.sanitize, err),
		})
	}

	result := observation.Result
	resultSHA256, resultErr := FingerprintResult(result)
	if resultErr != nil {
		addFailure("result.encoding", resultErr)
	} else {
		record.Execution.ResultSHA256 = resultSHA256
	}
	validBehaviorIdentity := result.Scenario == key.CaseName
	if !validBehaviorIdentity {
		addFailure("scenario.identity", fmt.Errorf("result scenario is %q", result.Scenario))
	}
	if runErr != nil {
		addFailure("execution.run", runErr)
	}
	if strings.TrimSpace(result.Transcript.Failure) != "" {
		addFailure("execution.protocol", errors.New(result.Transcript.Failure))
	}
	validFailureCount := len(result.Failures) <= maximumBehaviorFailures
	if !validFailureCount {
		addFailure("behavior.output_limit", fmt.Errorf(
			"result has %d behavior failures, limit is %d",
			len(result.Failures), maximumBehaviorFailures,
		))
	}
	consistentScore := result.Passed == (len(result.Failures) == 0)
	if !consistentScore {
		addFailure("behavior.inconsistent", errors.New("passed flag disagrees with failure list"))
	}
	if resultErr == nil && validBehaviorIdentity && validFailureCount && consistentScore && runErr == nil &&
		strings.TrimSpace(result.Transcript.Failure) == "" {
		record.Failures = cloneChecklistFailures(result.Failures)
		if result.Passed {
			record.Behavior = BehaviorPassed
		} else {
			record.Behavior = BehaviorFailed
		}
	}

	validateAttemptExecution(config, key, result, &record, addFailure)
	validateAttemptMedia(ctx, config, key, requirement, item, observation.Media, &record, addFailure)
	record.Reportable = record.Behavior != BehaviorUnscored && record.Execution.Validated &&
		config.requireMedia && record.Media != nil && len(record.Infrastructure) == 0
	record.Fingerprint = fingerprintAttempt(record)
	return record
}

func validateAttemptExecution(
	config frozenChecklistConfig,
	key AttemptKey,
	result scenario.Result,
	record *AttemptRecord,
	addFailure func(string, error),
) {
	if strings.TrimSpace(result.Transcript.ExecutionError) != "" {
		addFailure("evidence.attestor", errors.New(result.Transcript.ExecutionError))
	}
	if result.Transcript.Runtime == nil {
		addFailure("evidence.runtime_missing", errors.New("session runtime status is missing"))
	} else {
		status := *result.Transcript.Runtime
		if err := config.requirement.MatchStatus(status); err != nil {
			addFailure("evidence.runtime_graph", err)
		}
		if status.Binding != config.profile.Adapter.ProfileName {
			addFailure("evidence.binding", fmt.Errorf(
				"runtime binding is %q", status.Binding,
			))
		}
		if status.Profile != config.adapterProfileFingerprint {
			addFailure("evidence.adapter_profile", fmt.Errorf(
				"runtime adapter profile is %q", status.Profile,
			))
		}
	}
	if result.Transcript.Execution == nil {
		addFailure("evidence.execution_missing", errors.New("execution evidence is missing"))
		return
	}
	evidence := result.Transcript.Execution
	record.Execution.EvidenceFingerprint = evidence.Fingerprint
	if evidence.Scope != key.TaskID {
		addFailure("evidence.scope", fmt.Errorf("execution scope is %q", evidence.Scope))
	}
	if err := config.requirement.Match(evidence); err != nil {
		addFailure("evidence.requirement", err)
	}
	if len(record.Infrastructure) == 0 {
		record.Execution.Validated = true
		return
	}
	// Media and behavior failures are appended later. At this point only the
	// execution path has had an opportunity to add infrastructure failures.
	for _, failure := range record.Infrastructure {
		if strings.HasPrefix(failure.Code, "evidence.") {
			return
		}
	}
	record.Execution.Validated = true
}

func validateAttemptMedia(
	ctx context.Context,
	config frozenChecklistConfig,
	key AttemptKey,
	requirement CaseRequirement,
	item scenario.Scenario,
	reference *MediaReference,
	record *AttemptRecord,
	addFailure func(string, error),
) {
	if !config.requireMedia {
		return
	}
	if reference == nil {
		addFailure("media.missing", errors.New("attempt media completion receipt is missing"))
		return
	}
	if err := validateMediaReference(*reference, item); err != nil {
		addFailure("media.reference", err)
		return
	}
	copy := cloneMediaReference(*reference)
	verified, err := config.verifyMedia(
		ctx, key, cloneCaseRequirement(requirement), cloneMediaReference(copy),
	)
	if err != nil {
		addFailure("media.verification", err)
		return
	}
	if err := validateVerifiedMedia(copy, verified, item); err != nil {
		addFailure("media.receipt", err)
		return
	}
	receipt := cloneVerifiedMedia(verified)
	record.Media = &receipt
}

func validateMediaReference(reference MediaReference, item scenario.Scenario) error {
	if !canonicalChecklistText(reference.Handle) || !validChecklistDigest(reference.ManifestSHA256) {
		return errors.New("media reference has a non-canonical handle or manifest digest")
	}
	return validateSubmittedInputs(reference.Submitted, item)
}

func validateVerifiedMedia(reference MediaReference, verified VerifiedMedia, item scenario.Scenario) error {
	if len(verified.Submitted) != len(reference.Submitted) {
		return errors.New("verified media submitted-input count differs from the reference")
	}
	if len(verified.VideoSources) > maximumVideoSources {
		return errors.New("verified media has too many video sources")
	}
	if verified.Handle != reference.Handle || verified.ManifestSHA256 != reference.ManifestSHA256 ||
		!verified.Audio || !reflect.DeepEqual(verified.Submitted, reference.Submitted) {
		return errors.New("verified media does not exactly attest the supplied audio/input receipt")
	}
	if err := validateSubmittedInputs(verified.Submitted, item); err != nil {
		return err
	}
	for index, source := range verified.VideoSources {
		if !canonicalChecklistText(source) || (index > 0 && source <= verified.VideoSources[index-1]) {
			return errors.New("verified media video sources are not canonical ordered identities")
		}
	}
	return nil
}

func validateSubmittedInputs(receipts []SubmittedInputReceipt, item scenario.Scenario) error {
	if len(receipts) != len(item.Sees) {
		return fmt.Errorf("submitted still receipts = %d, want %d", len(receipts), len(item.Sees))
	}
	for index, receipt := range receipts {
		wantID := "scenario.sight." + strconv.Itoa(index+1)
		if receipt.SightID != wantID || receipt.CueMS != item.Sees[index].AtMS ||
			!validChecklistDigest(receipt.SHA256) || receipt.SizeBytes <= 0 ||
			(receipt.MediaType != "image/png" && receipt.MediaType != "image/jpeg") {
			return fmt.Errorf("submitted still receipt %d does not match its authored cue", index+1)
		}
	}
	return nil
}

func finishChecklist(
	ctx context.Context,
	config frozenChecklistConfig,
	checklist Checklist,
	cause error,
) (Checklist, error) {
	checklist, err := sealChecklist(checklist)
	if err != nil {
		return Checklist{}, fmt.Errorf("freeze scenario checklist: %w", err)
	}
	if config.sink.Finalize != nil {
		if err := config.sink.Finalize(ctx, checklist.Clone()); err != nil {
			return checklist, fmt.Errorf("finalize scenario checklist retention: %w", err)
		}
	}
	return checklist, cause
}

func sealChecklist(checklist Checklist) (Checklist, error) {
	checklist = summarizeChecklist(checklist)
	checklist.Fingerprint = fingerprintChecklist(checklist)
	if err := checklist.Validate(); err != nil {
		return Checklist{}, err
	}
	return checklist, nil
}

func summarizeChecklist(checklist Checklist) Checklist {
	for index := range checklist.Cases {
		checklist.Cases[index].Executed = 0
		checklist.Cases[index].Reportable = 0
		checklist.Cases[index].Passed = 0
		checklist.Cases[index].Failed = 0
		checklist.Cases[index].InfrastructureFailed = 0
	}
	checklist.Executed = len(checklist.Attempts)
	checklist.ReportableAttempts = 0
	checklist.PassedAttempts = 0
	checklist.FailedAttempts = 0
	checklist.InfrastructureFailures = 0
	for _, attempt := range checklist.Attempts {
		if attempt.Key.CaseOrdinal < 1 || attempt.Key.CaseOrdinal > len(checklist.Cases) {
			continue
		}
		item := &checklist.Cases[attempt.Key.CaseOrdinal-1]
		item.Executed++
		if attempt.Reportable {
			item.Reportable++
			checklist.ReportableAttempts++
		}
		switch attempt.Behavior {
		case BehaviorPassed:
			item.Passed++
			checklist.PassedAttempts++
		case BehaviorFailed:
			item.Failed++
			checklist.FailedAttempts++
		}
		if len(attempt.Infrastructure) > 0 {
			item.InfrastructureFailed++
			checklist.InfrastructureFailures++
		}
	}
	checklist.Complete = checklist.Executed == checklist.Expected
	checklist.Reportable = checklist.FullSuite &&
		checklist.Repetitions >= MinimumReportableRepetitions &&
		checklist.MediaPolicy == MediaPolicyRequired && checklist.Complete &&
		checklist.ReportableAttempts == checklist.Expected
	checklist.Passed = checklist.Reportable && checklist.PassedAttempts == checklist.Expected
	return checklist
}

// Validate proves deterministic ordering, summaries, per-attempt identities,
// and the content fingerprint. It intentionally cannot re-verify external
// execution/media evidence after those separately retained artifacts are no
// longer supplied.
func (checklist Checklist) Validate() error {
	if checklist.FormatVersion != ChecklistFormatVersion || checklist.Suite != SuiteName {
		return errors.New("scenario checklist has an unsupported format or suite")
	}
	if !validChecklistDigest(checklist.ContractFingerprint) ||
		!validChecklistDigest(checklist.LaunchProfileFingerprint) ||
		!validChecklistDigest(checklist.AdapterProfileFingerprint) ||
		!validChecklistDigest(checklist.ExecutionRequirementSHA256) {
		return errors.New("scenario checklist contains a non-canonical identity digest")
	}
	if err := checklist.Plan.Validate(); err != nil {
		return fmt.Errorf("scenario checklist plan: %w", err)
	}
	if checklist.Repetitions < 1 || checklist.Repetitions > maximumChecklistRepetitions ||
		len(checklist.Cases) < 1 || len(checklist.Cases) > len(scenario.Catalog()) ||
		(checklist.MediaPolicy != MediaPolicyRequired && checklist.MediaPolicy != MediaPolicyUnattested) {
		return errors.New("scenario checklist repetition or media policy is invalid")
	}
	names := make([]string, len(checklist.Cases))
	for index, item := range checklist.Cases {
		if item.Ordinal != index+1 || !canonicalChecklistText(item.Name) ||
			item.Expected != checklist.Repetitions {
			return errors.New("scenario checklist case order or expected count is invalid")
		}
		names[index] = item.Name
	}
	contract, err := BuildContract(names...)
	if err != nil || contract.Fingerprint != checklist.ContractFingerprint {
		return errors.New("scenario checklist case contract or fingerprint is invalid")
	}
	full, err := BuildContract()
	if err != nil || checklist.FullSuite != reflect.DeepEqual(contract, full) {
		return errors.New("scenario checklist full-suite identity is invalid")
	}
	if checklist.Expected != len(checklist.Cases)*checklist.Repetitions ||
		len(checklist.Attempts) > checklist.Expected {
		return errors.New("scenario checklist expected or attempt count is invalid")
	}
	mediaHandles := make(map[string]AttemptKey, len(checklist.Attempts))
	mediaManifests := make(map[string]AttemptKey, len(checklist.Attempts))
	for index, attempt := range checklist.Attempts {
		caseIndex := index / checklist.Repetitions
		trial := index%checklist.Repetitions + 1
		if caseIndex >= len(checklist.Cases) {
			return errors.New("scenario checklist attempt exceeds its case inventory")
		}
		want := AttemptKey{
			CaseOrdinal: caseIndex + 1, CaseName: checklist.Cases[caseIndex].Name,
			Trial: trial, TaskID: checklist.Cases[caseIndex].Name + "#" + strconv.Itoa(trial),
		}
		if attempt.Key != want || attempt.FormatVersion != ChecklistFormatVersion ||
			attempt.Execution.ContractFingerprint != checklist.ContractFingerprint ||
			attempt.Execution.LaunchProfileFingerprint != checklist.LaunchProfileFingerprint ||
			attempt.Execution.PlanFingerprint != checklist.Plan.PlanFingerprint ||
			attempt.Execution.GraphFingerprint != checklist.Plan.GraphFingerprint ||
			attempt.Execution.AdapterProfileFingerprint != checklist.AdapterProfileFingerprint {
			return errors.New("scenario checklist attempt attribution is invalid")
		}
		if err := validateAttemptRecord(attempt, checklist.MediaPolicy); err != nil {
			return fmt.Errorf("scenario checklist attempt %s: %w", attempt.Key.TaskID, err)
		}
		if attempt.Media != nil {
			if previous, duplicate := mediaHandles[attempt.Media.Handle]; duplicate {
				return fmt.Errorf(
					"scenario checklist attempts %s and %s reuse a media handle",
					previous.TaskID, attempt.Key.TaskID,
				)
			}
			if previous, duplicate := mediaManifests[attempt.Media.ManifestSHA256]; duplicate {
				return fmt.Errorf(
					"scenario checklist attempts %s and %s reuse a media manifest",
					previous.TaskID, attempt.Key.TaskID,
				)
			}
			mediaHandles[attempt.Media.Handle] = attempt.Key
			mediaManifests[attempt.Media.ManifestSHA256] = attempt.Key
		}
	}
	want := summarizeChecklist(checklist.Clone())
	want.Fingerprint = ""
	actual := checklist.Clone()
	actual.Fingerprint = ""
	if !reflect.DeepEqual(actual, want) {
		return errors.New("scenario checklist summaries are inconsistent with attempts")
	}
	wantFingerprint := fingerprintChecklist(checklist)
	if checklist.Fingerprint != wantFingerprint {
		return fmt.Errorf("scenario checklist fingerprint is %q, want %q",
			checklist.Fingerprint, wantFingerprint)
	}
	return nil
}

func validateAttemptRecord(attempt AttemptRecord, mediaPolicy string) error {
	if attempt.Behavior != BehaviorPassed && attempt.Behavior != BehaviorFailed &&
		attempt.Behavior != BehaviorUnscored {
		return errors.New("behavior status is invalid")
	}
	if attempt.Behavior == BehaviorPassed && len(attempt.Failures) != 0 ||
		attempt.Behavior == BehaviorFailed && len(attempt.Failures) == 0 ||
		attempt.Behavior == BehaviorUnscored && len(attempt.Failures) != 0 {
		return errors.New("behavior status disagrees with failure details")
	}
	if len(attempt.Failures) > maximumBehaviorFailures ||
		len(attempt.Infrastructure) > maximumInfrastructureFailures {
		return errors.New("attempt contains too many failure records")
	}
	for _, failure := range attempt.Failures {
		if !canonicalChecklistText(failure) {
			return errors.New("behavior failure is not canonical bounded text")
		}
	}
	for _, failure := range attempt.Infrastructure {
		if !canonicalChecklistCode(failure.Code) ||
			(failure.Detail != "" && !canonicalChecklistText(failure.Detail)) {
			return errors.New("infrastructure failure is not canonical")
		}
	}
	if attempt.Execution.Validated && !validChecklistDigest(attempt.Execution.EvidenceFingerprint) {
		return errors.New("validated execution lacks an evidence fingerprint")
	}
	validResultDigest := validChecklistDigest(attempt.Execution.ResultSHA256)
	resultEncodingFailed := hasAttemptFailureCode(attempt.Infrastructure, "result.encoding")
	if validResultDigest == resultEncodingFailed ||
		(!validResultDigest && attempt.Execution.ResultSHA256 != "") ||
		(resultEncodingFailed && attempt.Behavior != BehaviorUnscored) {
		return errors.New("attempt scorer-result digest disagrees with its encoding outcome")
	}
	if attempt.Execution.EvidenceFingerprint != "" &&
		!validChecklistDigest(attempt.Execution.EvidenceFingerprint) {
		return errors.New("execution evidence fingerprint is invalid")
	}
	if attempt.Media != nil {
		if !canonicalChecklistText(attempt.Media.Handle) ||
			!validChecklistDigest(attempt.Media.ManifestSHA256) || !attempt.Media.Audio {
			return errors.New("media receipt is invalid")
		}
		if err := validateSubmittedInputs(
			attempt.Media.Submitted, scenarioForChecklist(attempt.Key.CaseName),
		); err != nil {
			return fmt.Errorf("media submitted inputs: %w", err)
		}
		if len(attempt.Media.VideoSources) > maximumVideoSources {
			return errors.New("media receipt has too many video sources")
		}
		for index, source := range attempt.Media.VideoSources {
			if !canonicalChecklistText(source) ||
				(index > 0 && source <= attempt.Media.VideoSources[index-1]) {
				return errors.New("media video sources are not canonical ordered identities")
			}
		}
	}
	wantReportable := attempt.Behavior != BehaviorUnscored && attempt.Execution.Validated &&
		mediaPolicy == MediaPolicyRequired && attempt.Media != nil && len(attempt.Infrastructure) == 0
	if attempt.Reportable != wantReportable {
		return errors.New("reportable status disagrees with evidence")
	}
	if attempt.Fingerprint != fingerprintAttempt(attempt) {
		return errors.New("fingerprint is stale")
	}
	return nil
}

// MarshalChecklist emits deterministic, newline-terminated JSON for a
// plugin-owned review/persistence layer.
func MarshalChecklist(checklist Checklist) ([]byte, error) {
	if err := checklist.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(checklist, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode scenario checklist: %w", err)
	}
	return append(payload, '\n'), nil
}

// FingerprintResult binds a checklist row to the complete scorer result,
// including its transcript and execution evidence. Callers retaining result
// artifacts can recompute this digest without depending on checklist internals.
func FingerprintResult(result scenario.Result) (string, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode scenario scorer result: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (record AttemptRecord) Clone() AttemptRecord {
	record.Failures = slices.Clone(record.Failures)
	record.Infrastructure = slices.Clone(record.Infrastructure)
	if record.Media != nil {
		copy := cloneVerifiedMedia(*record.Media)
		record.Media = &copy
	}
	return record
}

func (checklist Checklist) Clone() Checklist {
	checklist.Cases = slices.Clone(checklist.Cases)
	attempts := checklist.Attempts
	checklist.Attempts = make([]AttemptRecord, len(attempts))
	for index, attempt := range attempts {
		checklist.Attempts[index] = attempt.Clone()
	}
	return checklist
}

func cloneMediaReference(reference MediaReference) MediaReference {
	reference.Submitted = slices.Clone(reference.Submitted)
	return reference
}

func cloneCaseRequirement(requirement CaseRequirement) CaseRequirement {
	requirement.Seams = slices.Clone(requirement.Seams)
	requirement.Operations = slices.Clone(requirement.Operations)
	return requirement
}

func cloneScenario(item scenario.Scenario) scenario.Scenario {
	item.Tools = slices.Clone(item.Tools)
	for index := range item.Tools {
		item.Tools[index].Parameters = slices.Clone(item.Tools[index].Parameters)
	}
	item.Script = slices.Clone(item.Script)
	item.Sees = slices.Clone(item.Sees)
	item.Checks = slices.Clone(item.Checks)
	for index := range item.Checks {
		item.Checks[index].Any = slices.Clone(item.Checks[index].Any)
		if item.Checks[index].Count != nil {
			count := *item.Checks[index].Count
			item.Checks[index].Count = &count
		}
	}
	return item
}

func cloneVerifiedMedia(receipt VerifiedMedia) VerifiedMedia {
	receipt.VideoSources = slices.Clone(receipt.VideoSources)
	receipt.Submitted = slices.Clone(receipt.Submitted)
	return receipt
}

func cloneChecklistFailures(source []string) []string {
	result := make([]string, 0, len(source))
	for _, failure := range source {
		if canonicalChecklistText(failure) {
			result = append(result, failure)
			continue
		}
		result = append(result, "scenario scorer returned a non-canonical failure detail")
	}
	return result
}

func hasAttemptFailureCode(failures []AttemptFailure, code string) bool {
	for _, failure := range failures {
		if failure.Code == code {
			return true
		}
	}
	return false
}

func checklistErrorDetail(sanitize ErrorSanitizer, err error) string {
	if sanitize == nil || err == nil {
		return ""
	}
	detail := sanitize(err.Error())
	if !canonicalChecklistText(detail) {
		return ""
	}
	return detail
}

func scenarioForChecklist(name string) scenario.Scenario {
	for _, item := range scenario.Catalog() {
		if item.Name == name {
			return item
		}
	}
	panic("validated scenario contract has no matching scenario")
}

func fingerprintAttempt(record AttemptRecord) string {
	record.Fingerprint = ""
	payload, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func fingerprintChecklist(checklist Checklist) string {
	checklist.Fingerprint = ""
	payload, err := json.Marshal(checklist)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validChecklistDigest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil && value == strings.ToLower(value)
}

func canonicalChecklistText(value string) bool {
	if value == "" || value != strings.TrimSpace(value) ||
		len(value) > maximumChecklistTextBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func canonicalChecklistCode(value string) bool {
	if !canonicalChecklistText(value) || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '.' || character == '_' {
			continue
		}
		return false
	}
	return true
}
