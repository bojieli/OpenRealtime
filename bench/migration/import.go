package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
)

const ImportProfileVersion = 1

type ResultFormat string

const (
	FormatBenchResult        ResultFormat = "bench-result"
	FormatArchitectureResult ResultFormat = "architecture-result"
)

type StateRuleKind string

const (
	RuleNotApplicable StateRuleKind = "not-applicable"
	RuleTaskPassed    StateRuleKind = "task-passed"
	RuleCompleted     StateRuleKind = "completed"
	RuleMetricAtMost  StateRuleKind = "metric-at-most"
	RuleMetricAtLeast StateRuleKind = "metric-at-least"
)

// ImportProfile is a preregistered compatibility contract. It maps existing
// suite rows into the common behavioral dimensions without changing their
// source JSON. Metric thresholds live here, not in post-candidate importer
// flags, so a missing suite-specific definition is a hard preregistration gap.
type ImportProfile struct {
	Version                int                 `json:"version"`
	ProfileID              string              `json:"profile_id"`
	Suite                  string              `json:"suite"`
	Format                 ResultFormat        `json:"format"`
	BaselineExecutionKind  bench.ExecutionKind `json:"baseline_execution_kind"`
	CandidateExecutionKind bench.ExecutionKind `json:"candidate_execution_kind"`
	Interaction            StateRule           `json:"interaction"`
	Deadline               StateRule           `json:"deadline"`
	Safety                 StateRule           `json:"safety"`
	Latencies              []LatencyMapping    `json:"latencies"`
	Axes                   []Axis              `json:"axes"`
	Evidence               []EvidenceRef       `json:"evidence"`
}

type StateRule struct {
	Kind      StateRuleKind `json:"kind"`
	Metric    string        `json:"metric,omitempty"`
	Threshold float64       `json:"threshold,omitempty"`
}

type LatencyMapping struct {
	Name   string `json:"name"`
	Metric string `json:"metric"`
	Unit   string `json:"unit"`
}

type ResultImport struct {
	Suite      string
	Reference  EvidenceRef
	Repetition string
	Outcome    EvidenceRef
}

var standardProvenanceAxes = map[string]bool{
	"source_revision": true, "source_modified": true, "executable_sha256": true,
	"machine_sha256": true, "cell_sha256": true, "execution_kind": true,
	"graph_sha256": true, "configuration_sha256": true,
	"deployment_sha256": true, "runtime_sha256": true,
	"legacy_runtime_sha256": true,
}

var notApplicableIdentitySHA256 = digestJSON("not-applicable")

func (profile ImportProfile) ID() string {
	profile = canonicalImportProfile(profile)
	profile.ProfileID = ""
	return digestJSON(profile)
}

func SealImportProfile(profile ImportProfile) (ImportProfile, error) {
	profile = canonicalImportProfile(profile)
	profile.ProfileID = ""
	profile.ProfileID = digestJSON(profile)
	if err := profile.Validate(); err != nil {
		return ImportProfile{}, err
	}
	return profile, nil
}

func canonicalImportProfile(input ImportProfile) ImportProfile {
	result := input
	result.Axes = append([]Axis(nil), input.Axes...)
	result.Evidence = append([]EvidenceRef(nil), input.Evidence...)
	result.Latencies = append([]LatencyMapping(nil), input.Latencies...)
	sort.Slice(result.Axes, func(left, right int) bool {
		if result.Axes[left].Name != result.Axes[right].Name {
			return result.Axes[left].Name < result.Axes[right].Name
		}
		return result.Axes[left].Value < result.Axes[right].Value
	})
	sort.Slice(result.Evidence, func(left, right int) bool {
		if result.Evidence[left].Kind != result.Evidence[right].Kind {
			return result.Evidence[left].Kind < result.Evidence[right].Kind
		}
		if result.Evidence[left].Location != result.Evidence[right].Location {
			return result.Evidence[left].Location < result.Evidence[right].Location
		}
		return result.Evidence[left].SHA256 < result.Evidence[right].SHA256
	})
	sort.Slice(result.Latencies, func(left, right int) bool {
		return result.Latencies[left].Name < result.Latencies[right].Name
	})
	return result
}

func (profile ImportProfile) Validate() error {
	if profile.Version != ImportProfileVersion {
		return fmt.Errorf("import profile version must be %d, got %d", ImportProfileVersion, profile.Version)
	}
	if strings.TrimSpace(profile.Suite) == "" || !validSHA256(profile.ProfileID) || profile.ID() != profile.ProfileID {
		return errors.New("import profile needs a suite and matching profile digest")
	}
	if profile.Format != FormatBenchResult && profile.Format != FormatArchitectureResult {
		return fmt.Errorf("import profile %q has unsupported result format %q", profile.Suite, profile.Format)
	}
	for arm, kind := range map[Arm]bench.ExecutionKind{
		ArmBaseline: profile.BaselineExecutionKind, ArmCandidate: profile.CandidateExecutionKind,
	} {
		if kind != bench.ExecutionGraphNative && kind != bench.ExecutionLegacy {
			return fmt.Errorf("import profile %q %s arm must require graph-native or legacy execution",
				profile.Suite, arm)
		}
	}
	if profile.CandidateExecutionKind != bench.ExecutionGraphNative {
		return fmt.Errorf("import profile %q candidate must require graph-native execution", profile.Suite)
	}
	if profile.Suite == SuiteScenario && profile.Format != FormatArchitectureResult {
		return errors.New("scenario migration imports must use architecture results")
	}
	for name, rule := range map[string]StateRule{
		"interaction": profile.Interaction, "deadline": profile.Deadline, "safety": profile.Safety,
	} {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("import profile %q %s rule: %w", profile.Suite, name, err)
		}
	}
	seenLatency := map[string]bool{}
	if len(profile.Latencies) > maxLatenciesPerSuite {
		return fmt.Errorf("import profile %q has too many latency mappings", profile.Suite)
	}
	for _, latency := range profile.Latencies {
		if strings.TrimSpace(latency.Name) == "" || strings.TrimSpace(latency.Metric) == "" || strings.TrimSpace(latency.Unit) == "" {
			return fmt.Errorf("import profile %q contains an incomplete latency mapping", profile.Suite)
		}
		if seenLatency[latency.Name] {
			return fmt.Errorf("import profile %q repeats latency %q", profile.Suite, latency.Name)
		}
		seenLatency[latency.Name] = true
	}
	seenAxes := map[string]bool{}
	if len(profile.Axes) > maxFixedAndTreatmentAxes {
		return fmt.Errorf("import profile %q has too many axes", profile.Suite)
	}
	for _, axis := range profile.Axes {
		if strings.TrimSpace(axis.Name) == "" || strings.TrimSpace(axis.Value) == "" || standardProvenanceAxes[axis.Name] {
			return fmt.Errorf("import profile %q has invalid or reserved axis %q", profile.Suite, axis.Name)
		}
		if seenAxes[axis.Name] {
			return fmt.Errorf("import profile %q repeats axis %q", profile.Suite, axis.Name)
		}
		seenAxes[axis.Name] = true
		var findings []Finding
		validateNamedDigest(&findings, "profile.axis_digest", "profile/axes", axis.Name, axis.Value)
		if len(findings) > 0 {
			return errors.New(findings[0].Message)
		}
	}
	seenEvidence := map[string]bool{}
	if len(profile.Evidence) == 0 {
		return fmt.Errorf("import profile %q needs immutable suite/fixture/scorer evidence", profile.Suite)
	}
	if len(profile.Evidence) > maxFixedAndTreatmentAxes {
		return fmt.Errorf("import profile %q has too many evidence references", profile.Suite)
	}
	for _, evidence := range profile.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Location) == "" || !validSHA256(evidence.SHA256) {
			return fmt.Errorf("import profile %q has invalid immutable evidence", profile.Suite)
		}
		identity := evidence.Kind + "\x00" + evidence.Location
		if seenEvidence[identity] {
			return fmt.Errorf("import profile %q repeats evidence %q", profile.Suite, evidence.Location)
		}
		seenEvidence[identity] = true
	}
	return nil
}

func (rule StateRule) validate() error {
	switch rule.Kind {
	case RuleNotApplicable, RuleTaskPassed, RuleCompleted:
		if rule.Metric != "" || rule.Threshold != 0 {
			return errors.New("non-metric rule carries a metric or threshold")
		}
	case RuleMetricAtMost, RuleMetricAtLeast:
		if strings.TrimSpace(rule.Metric) == "" || !finite(rule.Threshold) {
			return errors.New("metric rule needs a metric and finite threshold")
		}
	default:
		return fmt.Errorf("unknown state rule %q", rule.Kind)
	}
	return nil
}

func MarshalImportProfile(profile ImportProfile) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalImportProfile(profile), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeImportProfile(reader io.Reader) (ImportProfile, error) {
	if reader == nil {
		return ImportProfile{}, errors.New("decode import profile: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "migration import profile")
	if err != nil {
		return ImportProfile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var profile ImportProfile
	if err := decoder.Decode(&profile); err != nil {
		return ImportProfile{}, fmt.Errorf("decode import profile: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return ImportProfile{}, errors.New("decode import profile: trailing JSON value")
		}
		return ImportProfile{}, fmt.Errorf("decode import profile trailing content: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return ImportProfile{}, err
	}
	profile = canonicalImportProfile(profile)
	canonical, err := MarshalImportProfile(profile)
	if err != nil || !bytes.Equal(payload, canonical) {
		return ImportProfile{}, errors.New("decode import profile: artifact bytes are not canonical")
	}
	return profile, nil
}

// ImportBenchResult adapts an immutable existing result artifact into exact
// comparator attempts. It never drops an incomplete row. Structural evidence
// errors are retained on that attempt and cause the comparator to seal a
// refusal; unreadable source/profile artifacts remain hard import errors.
func ImportBenchResult(
	store LocalStore, census Census, profile ImportProfile, profileReference EvidenceRef,
	arm Arm, input ResultImport,
) ([]Attempt, error) {
	if arm != ArmBaseline && arm != ArmCandidate {
		return nil, fmt.Errorf("import arm must be baseline or candidate, got %q", arm)
	}
	if err := census.Validate(); err != nil {
		return nil, err
	}
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if profile.Suite != input.Suite {
		return nil, fmt.Errorf("import profile suite %q does not match input suite %q", profile.Suite, input.Suite)
	}
	if profileReference.Kind != EvidenceKindImportProfile {
		return nil, errors.New("import profile reference does not bind the decoded profile")
	}
	profilePayload, err := store.Resolve(profileReference)
	if err != nil {
		return nil, err
	}
	decodedProfile, err := DecodeImportProfile(bytes.NewReader(profilePayload))
	if err != nil || decodedProfile.ProfileID != profile.ProfileID {
		return nil, errors.New("registered import profile bytes differ from the supplied profile")
	}
	for _, evidence := range profile.Evidence {
		if _, err := store.Resolve(evidence); err != nil {
			return nil, fmt.Errorf("resolve import-profile evidence %q: %w", evidence.Location, err)
		}
	}
	if input.Reference.Kind != EvidenceKindResult {
		return nil, fmt.Errorf("result reference kind must be %q", EvidenceKindResult)
	}
	payload, err := store.Resolve(input.Reference)
	if err != nil {
		return nil, err
	}
	result, reportability, err := decodeCompatibleResult(payload, profile.Format)
	if err != nil {
		return nil, err
	}
	if len(result.Tasks) > maxAttemptsPerSuite {
		return nil, fmt.Errorf("result contains %d tasks, limit is %d", len(result.Tasks), maxAttemptsPerSuite)
	}
	if !compatibleSuiteName(profile.Suite, result.Suite) {
		return nil, fmt.Errorf("result suite %q does not match migration suite %q", result.Suite, profile.Suite)
	}
	censusSuite, exists := censusSuiteNamed(census, profile.Suite)
	if !exists {
		return nil, fmt.Errorf("census has no suite %q", profile.Suite)
	}
	caseByID := make(map[string]CensusCase, len(censusSuite.Cases))
	for _, item := range censusSuite.Cases {
		caseByID[item.ID] = item
	}
	baseEvidence := append([]EvidenceRef(nil), profile.Evidence...)
	baseEvidence = append(baseEvidence, profileReference, input.Reference)
	attempts := make([]Attempt, 0, len(result.Tasks))
	seenKeys := map[AttemptKey]bool{}
	for index, task := range result.Tasks {
		caseID, repetition, splitErr := importedTaskKey(profile.Suite, task.ID, input.Repetition)
		item, known := caseByID[caseID]
		if !known {
			item = CensusCase{Condition: "unmapped", ID: caseID}
		}
		key := AttemptKey{Suite: profile.Suite, Condition: item.Condition,
			Case: item.ID, Repetition: repetition}
		attempt := Attempt{
			ID:  fmt.Sprintf("%s/%d/%s", input.Reference.SHA256, index, task.ID),
			Key: key, Completed: task.Completed, Passed: task.Passed, Error: task.Error,
			Evidence: append([]EvidenceRef(nil), baseEvidence...),
		}
		if splitErr != nil {
			markImportFailure(&attempt, splitErr)
		}
		if !known {
			markImportFailure(&attempt, fmt.Errorf("task %q is absent from the authoritative census", caseID))
		}
		if seenKeys[key] {
			markImportFailure(&attempt, fmt.Errorf("result repeats attempt key %s", keyScope(key)))
		}
		seenKeys[key] = true
		attempt.Outcomes.Interaction, err = profile.Interaction.evaluate(task)
		if err != nil {
			markImportFailure(&attempt, fmt.Errorf("interaction outcome: %w", err))
		}
		attempt.Outcomes.Deadline, err = profile.Deadline.evaluate(task)
		if err != nil {
			markImportFailure(&attempt, fmt.Errorf("deadline outcome: %w", err))
		}
		attempt.Outcomes.Safety, err = profile.Safety.evaluate(task)
		if err != nil {
			markImportFailure(&attempt, fmt.Errorf("safety outcome: %w", err))
		}
		for _, mapping := range profile.Latencies {
			value, present := task.Metrics[mapping.Metric]
			if !present {
				markImportFailure(&attempt, fmt.Errorf("latency metric %q is missing", mapping.Metric))
				continue
			}
			attempt.Latencies = append(attempt.Latencies,
				Latency{Name: mapping.Name, Unit: mapping.Unit, Value: value})
		}
		attempt.Axes, err = importedAxes(result, task, profile, arm)
		if err != nil {
			markImportFailure(&attempt, err)
		}
		if task.Execution != nil {
			attempt.Evidence = append(attempt.Evidence, EvidenceRef{
				Kind:     EvidenceKindExecution,
				Location: input.Reference.Location + "#task=" + escapeFragment(task.ID),
				SHA256:   input.Reference.SHA256,
			})
		}
		attempts = append(attempts, attempt)
	}
	if reportability != nil || !ResultSummaryIsDerived(result) {
		message := "stored result summary is not derived from its task rows"
		if reportability != nil {
			message = reportability.Error()
		}
		// A result-level refusal is retained as an additional unexpected row.
		// Exact task rows remain unchanged, while Compare seals the artifact as
		// unreportable instead of allowing an importer error to erase it.
		attempts = append(attempts, Attempt{
			ID: input.Reference.SHA256 + "/result-refusal",
			Key: AttemptKey{Suite: profile.Suite, Condition: "import-refusal",
				Case: "result-refusal", Repetition: "result-refusal"},
			Completed: false, Error: "migration import: " + message,
			Outcomes: Outcomes{Interaction: OutcomeNotApplicable,
				Deadline: OutcomeNotApplicable, Safety: OutcomeNotApplicable},
			Evidence: append([]EvidenceRef(nil), baseEvidence...),
		})
	}
	return attempts, nil
}

func decodeCompatibleResult(payload []byte, format ResultFormat) (bench.Result, error, error) {
	switch format {
	case FormatBenchResult:
		var result bench.Result
		if err := decodeStrictResult(payload, &result); err != nil {
			return bench.Result{}, nil, fmt.Errorf("decode benchmark result: %w", err)
		}
		return result, result.Reportable(), nil
	case FormatArchitectureResult:
		var result archbench.Result
		if err := decodeStrictResult(payload, &result); err != nil {
			return bench.Result{}, nil, fmt.Errorf("decode architecture result: %w", err)
		}
		return result.Measurement, result.Reportable(), nil
	default:
		return bench.Result{}, nil, fmt.Errorf("unsupported result format %q", format)
	}
}

func decodeStrictResult(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func importedTaskKey(suite, taskID, defaultRepetition string) (string, string, error) {
	switch suite {
	case SuiteScenario:
		caseID, suffix, found := strings.Cut(taskID, "#")
		trial, err := strconv.Atoi(suffix)
		if !found || err != nil || trial <= 0 || caseID == "" {
			return taskID, "invalid", fmt.Errorf("scenario task ID %q is not case#trial", taskID)
		}
		return caseID, "trial-" + strconv.Itoa(trial), nil
	case SuiteTauControl, SuiteTauRegular:
		marker := strings.LastIndex(taskID, "/trial-")
		if marker <= 0 {
			return taskID, "invalid", fmt.Errorf("tau task ID %q has no trial suffix", taskID)
		}
		trial, err := strconv.Atoi(taskID[marker+len("/trial-"):])
		if err != nil || trial <= 0 {
			return taskID[:marker], "invalid", fmt.Errorf("tau task ID %q has an invalid trial", taskID)
		}
		return taskID[:marker], "trial-" + strconv.Itoa(trial), nil
	default:
		if strings.TrimSpace(defaultRepetition) == "" {
			return taskID, "invalid", errors.New("one-shot benchmark result needs an explicit repetition ID")
		}
		return taskID, defaultRepetition, nil
	}
}

func (rule StateRule) evaluate(task bench.TaskOutcome) (OutcomeState, error) {
	switch rule.Kind {
	case RuleNotApplicable:
		return OutcomeNotApplicable, nil
	case RuleTaskPassed:
		if task.Passed {
			return OutcomeSatisfied, nil
		}
		return OutcomeFailed, nil
	case RuleCompleted:
		if task.Completed {
			return OutcomeSatisfied, nil
		}
		return OutcomeFailed, nil
	case RuleMetricAtMost, RuleMetricAtLeast:
		value, present := task.Metrics[rule.Metric]
		if !present || !finite(value) {
			return OutcomeNotApplicable, fmt.Errorf("metric %q is missing or non-finite", rule.Metric)
		}
		passed := value <= rule.Threshold
		if rule.Kind == RuleMetricAtLeast {
			passed = value >= rule.Threshold
		}
		if passed {
			return OutcomeSatisfied, nil
		}
		return OutcomeFailed, nil
	default:
		return OutcomeNotApplicable, fmt.Errorf("unknown rule %q", rule.Kind)
	}
}

func importedAxes(
	result bench.Result, task bench.TaskOutcome, profile ImportProfile, arm Arm,
) ([]Axis, error) {
	executionKind, err := profile.executionKind(arm)
	if err != nil {
		return nil, err
	}
	axes := append([]Axis(nil), profile.Axes...)
	axes = append(axes,
		Axis{Name: "source_revision", Value: result.Provenance.Revision},
		Axis{Name: "source_modified", Value: strconv.FormatBool(result.Provenance.Modified)},
		Axis{Name: "executable_sha256", Value: result.Provenance.ExecutableSHA256},
		Axis{Name: "machine_sha256", Value: digestJSON(result.Provenance.Machine)},
		Axis{Name: "cell_sha256", Value: digestJSON(result.Cell)},
		Axis{Name: "execution_kind", Value: string(executionKind)},
	)
	if task.Execution == nil {
		return axes, errors.New("task has no immutable execution evidence")
	}
	if err := task.Execution.Validate(); err != nil {
		return axes, fmt.Errorf("task execution evidence: %w", err)
	}
	if task.Execution.Kind != executionKind {
		return axes, fmt.Errorf("task execution kind is %q, profile requires %q",
			task.Execution.Kind, executionKind)
	}
	if task.Execution.Scope != "" && task.Execution.Scope != task.ID {
		return axes, fmt.Errorf("task execution evidence scope is %q, want %q", task.Execution.Scope, task.ID)
	}
	if err := result.Cell.Execution.Match(task.Execution); err != nil {
		return axes, fmt.Errorf("task execution does not match the cell: %w", err)
	}
	switch task.Execution.Kind {
	case bench.ExecutionGraphNative:
		if task.Execution.Graph == nil {
			return axes, errors.New("graph-native execution has no graph evidence")
		}
		graph := task.Execution.Graph
		axes = append(axes,
			Axis{Name: "graph_sha256", Value: digestJSON(graph.Graph)},
			Axis{Name: "configuration_sha256", Value: digestJSON(graph.Configuration)},
			Axis{Name: "deployment_sha256", Value: digestJSON(graph.Nodes)},
			Axis{Name: "runtime_sha256", Value: stableRuntimeDigest(*task.Execution)},
			Axis{Name: "legacy_runtime_sha256", Value: notApplicableIdentitySHA256},
		)
	case bench.ExecutionLegacy:
		if task.Execution.Legacy == nil {
			return axes, errors.New("legacy execution has no runtime evidence")
		}
		axes = append(axes,
			Axis{Name: "graph_sha256", Value: digestJSON(result.Cell.Execution.Legacy)},
			Axis{Name: "configuration_sha256", Value: digestJSON(result.Cell)},
			Axis{Name: "deployment_sha256", Value: digestJSON(task.Execution.Legacy)},
			Axis{Name: "runtime_sha256", Value: stableRuntimeDigest(*task.Execution)},
			Axis{Name: "legacy_runtime_sha256", Value: digestJSON(task.Execution.Legacy)})
	}
	sort.Slice(axes, func(left, right int) bool { return axes[left].Name < axes[right].Name })
	return axes, nil
}

func (profile ImportProfile) executionKind(arm Arm) (bench.ExecutionKind, error) {
	switch arm {
	case ArmBaseline:
		return profile.BaselineExecutionKind, nil
	case ArmCandidate:
		return profile.CandidateExecutionKind, nil
	default:
		return "", fmt.Errorf("unknown migration arm %q", arm)
	}
}

func stableRuntimeDigest(evidence bench.ExecutionEvidence) string {
	copy := evidence.Clone()
	copy.Scope = ""
	copy.Fingerprint = ""
	if copy.Graph != nil {
		copy.Graph.Paths = nil
	}
	return digestJSON(copy)
}

func markImportFailure(attempt *Attempt, err error) {
	if attempt == nil || err == nil {
		return
	}
	attempt.Completed = false
	attempt.Passed = false
	message := strings.TrimSpace(err.Error())
	if attempt.Error == "" {
		attempt.Error = "migration import: " + message
	} else if !strings.Contains(attempt.Error, message) {
		attempt.Error += "; migration import: " + message
	}
	if len(attempt.Error) > maxAttemptErrorBytes {
		attempt.Error = truncateUTF8(attempt.Error, maxAttemptErrorBytes)
	}
}

func compatibleSuiteName(want, got string) bool {
	if want == got {
		return true
	}
	return (want == SuiteTauControl || want == SuiteTauRegular) && got == "tau-voice"
}

func censusSuiteNamed(census Census, name string) (CensusSuite, bool) {
	for _, suite := range census.Suites {
		if suite.Name == name {
			return suite, true
		}
	}
	return CensusSuite{}, false
}

func escapeFragment(value string) string {
	return strings.NewReplacer("%", "%25", "#", "%23", "=", "%3D").Replace(value)
}

// ResultSummaryIsDerived is used by integration tests and import diagnostics
// to detect legacy artifacts whose stored summary was edited independently of
// their rows. It preserves the original finish timestamp while re-deriving.
func ResultSummaryIsDerived(result bench.Result) bool {
	copy := result
	finished := copy.Provenance.FinishedAt
	copy.Finish()
	copy.Provenance.FinishedAt = finished
	return reflect.DeepEqual(copy.Summary, result.Summary)
}
