package releasevalidation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	architecture "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const maximumBehavioralResultBytes int64 = 1 << 30

type BehavioralOutcome string

const (
	BehavioralPassed  BehavioralOutcome = "passed"
	BehavioralFailed  BehavioralOutcome = "failed"
	BehavioralBlocked BehavioralOutcome = "blocked"
)

type BehavioralClosureInput struct {
	ID   string
	Path string
}

// BehavioralResultInput remains as a source-compatible alias. Path now names
// a campaign closure, never a bare result; supplying result JSON fails closed.
type BehavioralResultInput = BehavioralClosureInput

type BehavioralAcceptanceReport struct {
	FormatVersion         int                     `json:"format_version"`
	TargetsSHA256         string                  `json:"targets_sha256"`
	FrozenCandidateSHA256 string                  `json:"frozen_candidate_sha256"`
	CandidateID           string                  `json:"candidate_id"`
	Revision              string                  `json:"revision"`
	ExecutableSHA256      string                  `json:"executable_sha256"`
	Machine               bench.Machine           `json:"machine"`
	Outcome               BehavioralOutcome       `json:"outcome"`
	Accepted              bool                    `json:"accepted"`
	BlockedBy             []string                `json:"blocked_by,omitempty"`
	Failures              []string                `json:"failures,omitempty"`
	Suites                []BehavioralSuiteReport `json:"suites"`
}

type BehavioralSuiteReport struct {
	ID                 string                     `json:"id"`
	Suite              string                     `json:"suite"`
	ResultKind         string                     `json:"result_kind"`
	Outcome            BehavioralOutcome          `json:"outcome"`
	ResultSHA256       string                     `json:"result_sha256,omitempty"`
	ClosureSHA256      string                     `json:"closure_sha256,omitempty"`
	ExpectedPopulation int                        `json:"expected_population"`
	ObservedPopulation int                        `json:"observed_population"`
	Completed          int                        `json:"completed"`
	Passed             int                        `json:"passed"`
	NotApplicable      int                        `json:"not_applicable,omitempty"`
	Execution          *BehavioralExecutionReport `json:"execution,omitempty"`
	Lineage            []RunLineage               `json:"lineage,omitempty"`
	BlockedBy          []string                   `json:"blocked_by,omitempty"`
	Failures           []string                   `json:"failures,omitempty"`
	Checks             []BehavioralCheck          `json:"checks,omitempty"`
}

type BehavioralExecutionReport struct {
	RequirementSHA256   string `json:"requirement_sha256"`
	GraphFingerprint    string `json:"graph_fingerprint"`
	ConfigurationID     string `json:"configuration_id"`
	ConfigurationSHA256 string `json:"configuration_sha256"`
	DeploymentID        string `json:"deployment_id"`
	DeploymentSHA256    string `json:"deployment_sha256"`
	RuntimeSetSHA256    string `json:"runtime_set_sha256"`
}

type BehavioralCheck struct {
	Domain     string  `json:"domain"`
	Name       string  `json:"name"`
	Passed     bool    `json:"passed"`
	Metric     string  `json:"metric,omitempty"`
	Statistic  string  `json:"statistic,omitempty"`
	Comparison string  `json:"comparison,omitempty"`
	Threshold  float64 `json:"threshold,omitempty"`
	Observed   float64 `json:"observed,omitempty"`
	Samples    int     `json:"samples,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// EvaluateBehavioralAcceptance creates one candidate-only acceptance report.
// Historical result files and bare final results are deliberately not inputs:
// checked target numbers are the comparison authority, while every observed
// row is reached through a verified final graph-native campaign closure.
func EvaluateBehavioralAcceptance(
	targets BehavioralTargets,
	targetsSHA256 string,
	candidate FrozenCandidate,
	candidateSHA256 string,
	inputs []BehavioralClosureInput,
) BehavioralAcceptanceReport {
	report := BehavioralAcceptanceReport{
		FormatVersion: BehavioralAcceptanceVersion,
		TargetsSHA256: targetsSHA256, FrozenCandidateSHA256: candidateSHA256,
		CandidateID: candidate.CandidateID, Revision: candidate.Revision,
		ExecutableSHA256: candidate.ExecutableSHA256, Machine: candidate.Machine,
	}
	if err := targets.Validate(); err != nil {
		report.Failures = append(report.Failures, "invalid behavioral targets: "+err.Error())
		return finishBehavioralReport(report)
	}
	if err := candidate.Validate(); err != nil {
		report.Failures = append(report.Failures, "invalid frozen candidate: "+err.Error())
		return finishBehavioralReport(report)
	}
	if !sha256Pattern.MatchString(targetsSHA256) || !sha256Pattern.MatchString(candidateSHA256) {
		report.Failures = append(report.Failures, "control-artifact digests are not canonical SHA-256")
		return finishBehavioralReport(report)
	}

	targetByID := make(map[string]BehavioralSuiteTarget, len(targets.Suites))
	for _, suite := range targets.Suites {
		targetByID[suite.ID] = suite
	}
	frozenByID := make(map[string]FrozenCandidateSuite, len(candidate.Suites))
	for _, suite := range candidate.Suites {
		frozenByID[suite.ID] = suite
		if _, found := targetByID[suite.ID]; !found {
			report.Failures = append(report.Failures,
				fmt.Sprintf("frozen candidate declares unrequired suite %q", suite.ID))
		}
	}
	inputByID := make(map[string]string, len(inputs))
	for _, input := range inputs {
		if !behavioralIDPattern.MatchString(input.ID) || strings.TrimSpace(input.Path) == "" {
			report.Failures = append(report.Failures, "campaign closure input has an invalid ID or empty path")
			continue
		}
		if _, duplicate := inputByID[input.ID]; duplicate {
			report.Failures = append(report.Failures,
				fmt.Sprintf("campaign closure input repeats suite %q", input.ID))
			continue
		}
		inputByID[input.ID] = input.Path
		if _, found := targetByID[input.ID]; !found {
			report.Failures = append(report.Failures,
				fmt.Sprintf("campaign closure input declares unrequired suite %q", input.ID))
		}
	}

	for _, target := range targets.Suites {
		frozen, frozenFound := frozenByID[target.ID]
		path, inputFound := inputByID[target.ID]
		suiteReport := BehavioralSuiteReport{
			ID: target.ID, Suite: target.Suite, ResultKind: target.ResultKind,
			ExpectedPopulation: target.ExpectedPopulation,
		}
		if frozenFound {
			suiteReport.Lineage = slices.Clone(frozen.Lineage)
		} else {
			suiteReport.Failures = append(suiteReport.Failures,
				"frozen candidate omits this required suite")
		}
		appendRegistrationBlockers(&suiteReport, target)
		if !inputFound {
			suiteReport.Failures = append(suiteReport.Failures,
				"final-candidate campaign closure was not supplied")
			report.Suites = append(report.Suites, finishBehavioralSuite(suiteReport))
			continue
		}

		verified, closureErr := VerifyCampaignClosure(path)
		if closureErr != nil {
			suiteReport.Failures = append(suiteReport.Failures,
				"verify final-candidate campaign closure: "+closureErr.Error())
			report.Suites = append(report.Suites, finishBehavioralSuite(suiteReport))
			continue
		}
		result := verified.Result
		resultDigest := verified.Closure.Result.ArtifactSHA256
		suiteReport.ResultSHA256 = resultDigest
		suiteReport.ClosureSHA256 = verified.Closure.ClosureSHA256
		suiteReport.ObservedPopulation = len(result.Tasks)
		suiteReport.Completed = result.Summary.Completed
		suiteReport.Passed = result.Summary.Passed
		suiteReport.NotApplicable = result.Summary.NotApplicable

		validateCampaignClosureControl(
			&suiteReport, target, frozen, frozenFound, candidate, candidateSHA256, verified,
		)
		validateBehavioralResult(&suiteReport, target, frozen, frozenFound, candidate, result)
		if target.Aggregate.Registration.Status == RegistrationRegistered {
			minimum := *target.Aggregate.MinimumPassed
			passed := result.Summary.Passed >= minimum
			suiteReport.Checks = append(suiteReport.Checks, BehavioralCheck{
				Domain: "aggregate", Name: "minimum-passed", Passed: passed,
				Metric: "passed", Statistic: StatisticSum, Comparison: ComparisonAtLeast,
				Threshold: float64(minimum), Observed: float64(result.Summary.Passed),
				Samples: len(result.Tasks),
			})
			if !passed {
				suiteReport.Failures = append(suiteReport.Failures, fmt.Sprintf(
					"aggregate passed %d tasks, below preregistered target %d",
					result.Summary.Passed, minimum))
			}
			if target.Aggregate.MinimumApplicable != nil {
				evaluateApplicabilityFloor(&suiteReport, "aggregate", "minimum-applicable",
					result.Summary.Completed-result.Summary.NotApplicable, *target.Aggregate.MinimumApplicable, len(result.Tasks))
			}
		}
		if target.Cases.Registration.Status == RegistrationRegistered {
			evaluateCaseTargets(&suiteReport, target, result.Tasks)
		}
		evaluateEvidenceTargets(&suiteReport, target, result.Tasks)
		report.Suites = append(report.Suites, finishBehavioralSuite(suiteReport))
	}
	return finishBehavioralReport(report)
}

func appendRegistrationBlockers(report *BehavioralSuiteReport, target BehavioralSuiteTarget) {
	registrations := []struct {
		name         string
		registration Registration
	}{
		{"aggregate", target.Aggregate.Registration},
		{"per-case", target.Cases.Registration},
		{"safety", target.Safety.Registration},
		{"deadline", target.Deadline.Registration},
		{"latency", target.Latency.Registration},
	}
	for _, item := range registrations {
		if item.registration.Status == RegistrationUnavailable {
			report.BlockedBy = append(report.BlockedBy,
				fmt.Sprintf("%s target unavailable: %s", item.name, item.registration.Reason))
		}
	}
}

func readBehavioralResult(path, kind string) (bench.Result, string, error) {
	payload, err := readBehavioralResultFile(path)
	if err != nil {
		return bench.Result{}, "", fmt.Errorf("read final-candidate result: %w", err)
	}
	return decodeBehavioralResult(payload, kind)
}

func decodeBehavioralResult(payload []byte, kind string) (bench.Result, string, error) {
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: int(maximumBehavioralResultBytes), MaxDepth: 512,
		MaxTokens: 64_000_000, MaxObjectMembers: 1_000_000,
		MaxArrayElements: 2_000_000, MaxTotalKeyBytes: 768 << 20,
		MaxWorkBytes: 4 << 30,
	}); err != nil {
		return bench.Result{}, "", fmt.Errorf("final-candidate result is not strict bounded JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result bench.Result
	switch kind {
	case ResultKindBench:
		if err := decoder.Decode(&result); err != nil {
			return bench.Result{}, "", fmt.Errorf("decode benchmark result: %w", err)
		}
	case ResultKindArchitecture:
		var wrapped architecture.Result
		if err := decoder.Decode(&wrapped); err != nil {
			return bench.Result{}, "", fmt.Errorf("decode architecture result: %w", err)
		}
		if err := wrapped.Reportable(); err != nil {
			return bench.Result{}, "", err
		}
		result = wrapped.Measurement
	default:
		return bench.Result{}, "", fmt.Errorf("unknown result kind %q", kind)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return bench.Result{}, "", errors.New("final-candidate result contains trailing JSON")
		}
		return bench.Result{}, "", fmt.Errorf("decode final-candidate trailing JSON: %w", err)
	}
	return result, digestBytes(payload), nil
}

func readBehavioralResultFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maximumBehavioralResultBytes {
		return nil, errors.New("result is not a bounded non-empty regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximumBehavioralResultBytes+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, errors.New("read complete result")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		return nil, errors.New("result changed while it was read")
	}
	return payload, nil
}

func validateCampaignClosureControl(
	report *BehavioralSuiteReport,
	target BehavioralSuiteTarget,
	frozen FrozenCandidateSuite,
	frozenFound bool,
	candidate FrozenCandidate,
	candidateSHA256 string,
	verified VerifiedCampaignClosure,
) {
	closure := verified.Closure
	if closure.SuiteID != target.ID {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"campaign closure suite is %q, want %q", closure.SuiteID, target.ID))
	}
	if closure.Result.Format != target.ResultKind {
		report.Failures = append(report.Failures,
			"campaign closure result kind differs from the preregistered target")
	}
	if closure.ExpectedPopulation != target.ExpectedPopulation {
		report.Failures = append(report.Failures,
			"campaign closure population differs from the preregistered target")
	}
	if closure.CandidateID != candidate.CandidateID ||
		closure.CandidateSHA256 != candidateSHA256 {
		report.Failures = append(report.Failures,
			"campaign closure belongs to a different frozen candidate")
	}
	if closure.Revision != candidate.Revision ||
		closure.ExecutableSHA256 != candidate.ExecutableSHA256 ||
		!reflect.DeepEqual(closure.Machine, candidate.Machine) {
		report.Failures = append(report.Failures,
			"campaign closure build or machine differs from the frozen candidate")
	}
	if !frozenFound {
		return
	}
	if closure.ExecutionRequirementSHA256 != frozen.ExecutionRequirementSHA256 {
		report.Failures = append(report.Failures,
			"campaign closure execution requirement differs from the frozen candidate")
	}
	if closure.RunSpec.ArtifactSHA256 != frozen.RunSpecSHA256 {
		report.Failures = append(report.Failures,
			"campaign closure run specification differs from the frozen candidate")
	}
	if closure.Inventory.ArtifactSHA256 != frozen.TaskInventorySHA256 {
		report.Failures = append(report.Failures,
			"campaign closure task inventory differs from the frozen candidate")
	}
	if closure.Scorer.ArtifactSHA256 != frozen.ScorerSHA256 {
		report.Failures = append(report.Failures,
			"campaign closure scorer differs from the frozen candidate")
	}
	if len(verified.SourceReceipts) != len(frozen.SourceReceipts) {
		report.Failures = append(report.Failures,
			"campaign closure source-receipt set differs from the frozen candidate")
	} else {
		actual := make([]string, len(verified.SourceReceipts))
		want := make([]string, len(frozen.SourceReceipts))
		for index, source := range verified.SourceReceipts {
			actual[index] = source.Kind + "\x00" + source.ArtifactFormat
		}
		for index, requirement := range frozen.SourceReceipts {
			want[index] = requirement.Kind + "\x00" + requirement.ArtifactFormat
		}
		sort.Strings(actual)
		sort.Strings(want)
		if !slices.Equal(actual, want) {
			report.Failures = append(report.Failures,
				"campaign closure source-receipt identity differs from the frozen candidate")
		}
	}
	if len(frozen.Lineage) == 0 {
		return
	}
	final := frozen.Lineage[len(frozen.Lineage)-1]
	if closure.CampaignID != final.CampaignID || closure.ExpectedPopulation != final.Population {
		report.Failures = append(report.Failures,
			"campaign closure does not close the declared final_full lineage row")
	}
	priorLineage := frozen.Lineage[:len(frozen.Lineage)-1]
	if len(verified.Predecessors) != len(priorLineage) ||
		len(closure.Predecessors) != len(priorLineage) {
		report.Failures = append(report.Failures,
			"campaign closure predecessor set does not close the repair lineage")
		return
	}
	for index, run := range priorLineage {
		prior := verified.Predecessors[index]
		artifact := closure.Predecessors[index]
		if prior.CampaignID != run.CampaignID || prior.ExpectedPopulation != run.Population ||
			artifact.ArtifactSHA256 != run.ArtifactSHA256 {
			report.Failures = append(report.Failures, fmt.Sprintf(
				"campaign closure predecessor %d does not match declared repair lineage", index))
		}
	}
}

func validateBehavioralResult(
	report *BehavioralSuiteReport,
	target BehavioralSuiteTarget,
	frozen FrozenCandidateSuite,
	frozenFound bool,
	candidate FrozenCandidate,
	result bench.Result,
) {
	if result.Suite != target.Suite {
		report.Failures = append(report.Failures,
			fmt.Sprintf("result suite is %q, want %q", result.Suite, target.Suite))
	}
	if result.Expected != target.ExpectedPopulation || len(result.Tasks) != target.ExpectedPopulation ||
		result.Summary.Completed != target.ExpectedPopulation || !result.Summary.Complete {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"population is incomplete: declared %d, rows %d, completed %d, want %d",
			result.Expected, len(result.Tasks), result.Summary.Completed, target.ExpectedPopulation))
	}
	seen := make(map[string]bool, len(result.Tasks))
	for _, task := range result.Tasks {
		if strings.TrimSpace(task.ID) == "" || seen[task.ID] {
			report.Failures = append(report.Failures,
				fmt.Sprintf("task identity %q is empty or duplicated", task.ID))
		}
		seen[task.ID] = true
		if !task.Completed {
			report.Failures = append(report.Failures,
				fmt.Sprintf("task %q did not complete", task.ID))
		}
		if target.Suite == "fdb-v1.5" && task.Applicability == "" {
			report.Failures = append(report.Failures,
				fmt.Sprintf("FDB task %q lacks explicit applicability; historical nominal passes cannot certify a final candidate", task.ID))
		}
	}
	derived := result
	derived.Finish()
	if !equalSummaries(result.Summary, derived.Summary) {
		report.Failures = append(report.Failures,
			"stored summary differs from the task-derived summary")
	}
	if err := result.Reportable(); err != nil {
		report.Failures = append(report.Failures, err.Error())
	}
	if result.Provenance.Revision != candidate.Revision {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"source revision is %q, want frozen %q", result.Provenance.Revision, candidate.Revision))
	}
	if "sha256:"+result.Provenance.ExecutableSHA256 != candidate.ExecutableSHA256 {
		report.Failures = append(report.Failures,
			"executable digest differs from the frozen candidate")
	}
	if !reflect.DeepEqual(result.Provenance.Machine, candidate.Machine) {
		report.Failures = append(report.Failures,
			"machine identity differs from the frozen candidate")
	}
	if result.Provenance.Modified {
		report.Failures = append(report.Failures,
			"result came from a modified worktree")
	}
	if !result.Cell.Execution.Required() || result.Cell.Execution.Kind != bench.ExecutionGraphNative {
		report.Failures = append(report.Failures,
			"result does not require graph-native execution")
		return
	}
	requirement, err := bench.MarshalExecutionRequirement(result.Cell.Execution)
	if err != nil {
		report.Failures = append(report.Failures,
			"invalid graph-native execution requirement: "+err.Error())
		return
	}
	requirementDigest := digestBytes(requirement)
	graph := result.Cell.Execution.Graph
	executionReport := &BehavioralExecutionReport{
		RequirementSHA256:   requirementDigest,
		GraphFingerprint:    graph.Graph.Fingerprint,
		ConfigurationID:     graph.Configuration.ID,
		ConfigurationSHA256: graph.Configuration.Digest,
		RuntimeSetSHA256:    digestJSON(graph.Nodes),
	}
	if graph.Deployment != nil {
		executionReport.DeploymentID = graph.Deployment.Public.ID
		executionReport.DeploymentSHA256 = graph.Deployment.Public.Digest
	}
	report.Execution = executionReport
	if frozenFound && requirementDigest != frozen.ExecutionRequirementSHA256 {
		report.Failures = append(report.Failures,
			"graph/config/deployment/runtime execution identity differs from the frozen candidate")
	}
	if frozenFound {
		final := frozen.Lineage[len(frozen.Lineage)-1]
		if final.Kind != RunFinalFull || final.Population != target.ExpectedPopulation {
			report.Failures = append(report.Failures, fmt.Sprintf(
				"lineage ends with %s population %d, not a full final population of %d",
				final.Kind, final.Population, target.ExpectedPopulation))
		}
		for index, run := range frozen.Lineage[:len(frozen.Lineage)-1] {
			if run.Kind == RunFocusedDiagnostic {
				if run.Population >= target.ExpectedPopulation {
					report.Failures = append(report.Failures, fmt.Sprintf(
						"lineage row %d labels a full population as a focused diagnostic", index))
				}
			} else if run.Population != target.ExpectedPopulation {
				report.Failures = append(report.Failures, fmt.Sprintf(
					"lineage row %d labels an incomplete population as a full run", index))
			}
		}
	}
}

func equalSummaries(left, right bench.Summary) bool {
	if left.Completed != right.Completed || left.Failed != right.Failed || left.Passed != right.Passed ||
		left.NotApplicable != right.NotApplicable ||
		left.PassRate != right.PassRate || left.Complete != right.Complete ||
		left.Incompleteness != right.Incompleteness || len(left.Distributions) != len(right.Distributions) {
		return false
	}
	for name, distribution := range left.Distributions {
		if distribution != right.Distributions[name] {
			return false
		}
	}
	return true
}

func evaluateCaseTargets(
	report *BehavioralSuiteReport,
	target BehavioralSuiteTarget,
	tasks []bench.TaskOutcome,
) {
	type observedCase struct{ attempts, applicable, passed int }
	observed := make(map[string]observedCase)
	for _, task := range tasks {
		key, err := behavioralCaseKey(target.CaseKey, task.ID)
		if err != nil {
			report.Failures = append(report.Failures, err.Error())
			continue
		}
		value := observed[key]
		value.attempts++
		if task.Completed && task.Applicability != bench.NotApplicable {
			value.applicable++
			if task.Passed {
				value.passed++
			}
		}
		observed[key] = value
	}
	for _, expected := range target.Cases.Targets {
		actual, found := observed[expected.Case]
		delete(observed, expected.Case)
		passed := found && actual.attempts == expected.ExpectedAttempts &&
			actual.passed >= expected.MinimumPassed
		reason := ""
		if !passed {
			reason = fmt.Sprintf("observed %d attempts and %d passes; want %d attempts and at least %d passes",
				actual.attempts, actual.passed, expected.ExpectedAttempts, expected.MinimumPassed)
			report.Failures = append(report.Failures,
				fmt.Sprintf("case %q: %s", expected.Case, reason))
		}
		report.Checks = append(report.Checks, BehavioralCheck{
			Domain: "case", Name: expected.Case, Passed: passed,
			Metric: "passed", Statistic: StatisticSum, Comparison: ComparisonAtLeast,
			Threshold: float64(expected.MinimumPassed), Observed: float64(actual.passed),
			Samples: actual.attempts, Reason: reason,
		})
		if expected.MinimumApplicable != nil {
			evaluateApplicabilityFloor(report, "case", expected.Case+"/minimum-applicable",
				actual.applicable, *expected.MinimumApplicable, actual.attempts)
		}
	}
	if len(observed) != 0 {
		keys := make([]string, 0, len(observed))
		for key := range observed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		report.Failures = append(report.Failures,
			"candidate has unregistered cases: "+strings.Join(keys, ", "))
	}
}

func evaluateApplicabilityFloor(report *BehavioralSuiteReport, domain, name string, actual, minimum, attempts int) {
	passed := actual >= minimum
	reason := ""
	if !passed {
		reason = fmt.Sprintf("%s has %d applicable attempts, below preregistered minimum %d", name, actual, minimum)
		report.Failures = append(report.Failures, reason)
	}
	report.Checks = append(report.Checks, BehavioralCheck{
		Domain: domain, Name: name, Passed: passed, Metric: "applicable", Statistic: StatisticSum,
		Comparison: ComparisonAtLeast, Threshold: float64(minimum), Observed: float64(actual),
		Samples: attempts, Reason: reason,
	})
}

func behavioralCaseKey(mode, taskID string) (string, error) {
	switch mode {
	case CaseKeyExact:
		return taskID, nil
	case CaseKeyBeforeFinalHash:
		index := strings.LastIndexByte(taskID, '#')
		if index <= 0 || index == len(taskID)-1 {
			return "", fmt.Errorf("task %q has no canonical #trial suffix", taskID)
		}
		trial, err := strconv.Atoi(taskID[index+1:])
		if err != nil || trial <= 0 {
			return "", fmt.Errorf("task %q has no canonical #trial suffix", taskID)
		}
		return taskID[:index], nil
	default:
		return "", fmt.Errorf("unknown case-key rule %q", mode)
	}
}

func evaluateEvidenceTargets(
	report *BehavioralSuiteReport,
	target BehavioralSuiteTarget,
	tasks []bench.TaskOutcome,
) {
	sets := []struct {
		domain string
		set    EvidenceTargetSet
	}{
		{"safety", target.Safety}, {"deadline", target.Deadline}, {"latency", target.Latency},
	}
	metricSamples := make(map[string][]float64)
	for _, task := range tasks {
		for metric, value := range task.Metrics {
			metricSamples[metric] = append(metricSamples[metric], value)
		}
	}
	for _, item := range sets {
		if item.set.Registration.Status == RegistrationRegistered {
			for _, rule := range item.set.Rules {
				evaluateMetricRule(report, item.domain, rule, metricSamples[rule.Metric], len(tasks))
			}
		}
		if item.domain == "latency" {
			checkMetricCoverage(report, item.domain, item.set, metricSamples, func(metric string) bool {
				return bench.UnitOf(metric) == "ms"
			})
		}
		if item.domain == "deadline" {
			checkMetricCoverage(report, item.domain, item.set, metricSamples, func(metric string) bool {
				return strings.Contains(strings.ToLower(metric), "deadline")
			})
		}
	}
}

func evaluateMetricRule(
	report *BehavioralSuiteReport,
	domain string,
	rule MetricRule,
	samples []float64,
	taskCount int,
) {
	check := BehavioralCheck{
		Domain: domain, Name: rule.Name, Metric: rule.Metric, Statistic: rule.Statistic,
		Comparison: rule.Comparison, Threshold: rule.Threshold, Samples: len(samples),
	}
	if len(samples) < rule.MinimumSamples {
		check.Reason = fmt.Sprintf("metric has %d samples, want at least %d", len(samples), rule.MinimumSamples)
	} else if rule.RequireEveryTask && len(samples) != taskCount {
		check.Reason = fmt.Sprintf("metric has %d samples for %d tasks", len(samples), taskCount)
	} else {
		check.Observed = metricStatistic(samples, rule.Statistic)
		check.Passed = rule.Comparison == ComparisonAtLeast && check.Observed >= rule.Threshold ||
			rule.Comparison == ComparisonAtMost && check.Observed <= rule.Threshold
		if !check.Passed {
			check.Reason = fmt.Sprintf("observed %g does not satisfy %s %g",
				check.Observed, rule.Comparison, rule.Threshold)
		}
	}
	if !check.Passed {
		report.Failures = append(report.Failures,
			fmt.Sprintf("%s evidence %q: %s", domain, rule.Name, check.Reason))
	}
	report.Checks = append(report.Checks, check)
}

func metricStatistic(samples []float64, statistic string) float64 {
	distribution := bench.Summarise(samples)
	switch statistic {
	case StatisticCount:
		return float64(len(samples))
	case StatisticMin:
		return distribution.Min
	case StatisticP50:
		return distribution.P50
	case StatisticP90:
		return distribution.P90
	case StatisticP95:
		return distribution.P95
	case StatisticP99:
		return distribution.P99
	case StatisticMax:
		return distribution.Max
	case StatisticMean:
		return distribution.Mean
	case StatisticSum:
		total := 0.0
		for _, value := range samples {
			total += value
		}
		return total
	default:
		return math.NaN()
	}
}

func checkMetricCoverage(
	report *BehavioralSuiteReport,
	domain string,
	set EvidenceTargetSet,
	samples map[string][]float64,
	belongs func(string) bool,
) {
	covered := make(map[string]bool)
	for _, rule := range set.Rules {
		covered[rule.Metric] = true
	}
	for _, exclusion := range set.Exclusions {
		covered[exclusion.Metric] = true
	}
	var missing []string
	for metric := range samples {
		if belongs(metric) && !covered[metric] {
			missing = append(missing, metric)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return
	}
	if set.Registration.Status == RegistrationNotApplicable {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"%s was declared not applicable but candidate reports metrics: %s",
			domain, strings.Join(missing, ", ")))
		return
	}
	if set.Registration.Status == RegistrationRegistered {
		report.Failures = append(report.Failures, fmt.Sprintf(
			"%s metrics lack preregistered targets or explicit exclusions: %s",
			domain, strings.Join(missing, ", ")))
	}
}

func finishBehavioralSuite(report BehavioralSuiteReport) BehavioralSuiteReport {
	switch {
	case len(report.Failures) != 0:
		report.Outcome = BehavioralFailed
	case len(report.BlockedBy) != 0:
		report.Outcome = BehavioralBlocked
	default:
		report.Outcome = BehavioralPassed
	}
	return report
}

func finishBehavioralReport(report BehavioralAcceptanceReport) BehavioralAcceptanceReport {
	for _, suite := range report.Suites {
		for _, blocker := range suite.BlockedBy {
			report.BlockedBy = append(report.BlockedBy, suite.ID+": "+blocker)
		}
		for _, failure := range suite.Failures {
			report.Failures = append(report.Failures, suite.ID+": "+failure)
		}
	}
	switch {
	case len(report.Failures) != 0:
		report.Outcome = BehavioralFailed
	case len(report.BlockedBy) != 0:
		report.Outcome = BehavioralBlocked
	default:
		report.Outcome = BehavioralPassed
		report.Accepted = true
	}
	return report
}

func digestJSON(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return digestBytes(payload)
}

func MarshalBehavioralAcceptanceReport(report BehavioralAcceptanceReport) ([]byte, error) {
	if report.FormatVersion != BehavioralAcceptanceVersion || !sha256Pattern.MatchString(report.TargetsSHA256) ||
		!sha256Pattern.MatchString(report.FrozenCandidateSHA256) ||
		(report.Outcome != BehavioralPassed && report.Outcome != BehavioralFailed && report.Outcome != BehavioralBlocked) ||
		report.Accepted != (report.Outcome == BehavioralPassed) {
		return nil, errors.New("behavioral acceptance report is structurally invalid")
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return nil, fmt.Errorf("encode behavioral acceptance report: %w", err)
	}
	return output.Bytes(), nil
}
