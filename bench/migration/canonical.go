package migration

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
)

// ID returns the digest of the canonical manifest. Semantically identical
// slice orderings produce the same identity.
func (manifest Manifest) ID() string {
	return digestJSON(canonicalManifest(manifest))
}

func canonicalManifest(input Manifest) Manifest {
	result := Manifest{
		Version: input.Version, Campaign: canonicalCampaign(input.Campaign),
		Baseline: input.Baseline, Candidate: input.Candidate,
		FixedAxes: make([]Axis, len(input.FixedAxes)),
		Treatment: make([]TreatmentDelta, len(input.Treatment)),
		Suites:    make([]SuiteSpec, len(input.Suites)),
	}
	copy(result.FixedAxes, input.FixedAxes)
	copy(result.Treatment, input.Treatment)
	sort.Slice(result.FixedAxes, func(left, right int) bool {
		if result.FixedAxes[left].Name != result.FixedAxes[right].Name {
			return result.FixedAxes[left].Name < result.FixedAxes[right].Name
		}
		return result.FixedAxes[left].Value < result.FixedAxes[right].Value
	})
	sort.Slice(result.Treatment, func(left, right int) bool {
		if result.Treatment[left].Axis != result.Treatment[right].Axis {
			return result.Treatment[left].Axis < result.Treatment[right].Axis
		}
		if result.Treatment[left].Baseline != result.Treatment[right].Baseline {
			return result.Treatment[left].Baseline < result.Treatment[right].Baseline
		}
		return result.Treatment[left].Candidate < result.Treatment[right].Candidate
	})
	for index, suite := range input.Suites {
		result.Suites[index] = canonicalSuite(suite)
	}
	sort.Slice(result.Suites, func(left, right int) bool {
		return result.Suites[left].Name < result.Suites[right].Name
	})
	return result
}

func canonicalCampaign(input CampaignPlan) CampaignPlan {
	result := input
	result.Predecessors = canonicalReportReferences(input.Predecessors)
	result.DiagnosedFailures = append(
		make([]FailureReference, 0, len(input.DiagnosedFailures)), input.DiagnosedFailures...)
	sort.Slice(result.DiagnosedFailures, func(left, right int) bool {
		if result.DiagnosedFailures[left].ReportID != result.DiagnosedFailures[right].ReportID {
			return result.DiagnosedFailures[left].ReportID < result.DiagnosedFailures[right].ReportID
		}
		if result.DiagnosedFailures[left].Gate != result.DiagnosedFailures[right].Gate {
			return result.DiagnosedFailures[left].Gate < result.DiagnosedFailures[right].Gate
		}
		return result.DiagnosedFailures[left].FindingCode < result.DiagnosedFailures[right].FindingCode
	})
	return result
}

func canonicalReportReferences(input []ReportReference) []ReportReference {
	result := append(make([]ReportReference, 0, len(input)), input...)
	sort.Slice(result, func(left, right int) bool {
		if result[left].RunID != result[right].RunID {
			return result[left].RunID < result[right].RunID
		}
		if result[left].ReportID != result[right].ReportID {
			return result[left].ReportID < result[right].ReportID
		}
		return result[left].ArtifactSHA256 < result[right].ArtifactSHA256
	})
	return result
}

func canonicalSuite(input SuiteSpec) SuiteSpec {
	result := SuiteSpec{
		Name: input.Name, ExpectedCases: input.ExpectedCases,
		ExpectedAttempts: input.ExpectedAttempts, MinimumRepetitions: input.MinimumRepetitions,
		Populations: make([]Population, len(input.Populations)),
		Cases:       make([]CaseSpec, len(input.Cases)),
		Policy:      canonicalSuitePolicy(input.Policy),
	}
	for index, population := range input.Populations {
		result.Populations[index] = Population{
			Condition: population.Condition, ExpectedCases: population.ExpectedCases,
			ExpectedAttempts: population.ExpectedAttempts, RequirePolicy: population.RequirePolicy,
			Policy: canonicalPartitionPolicyPointer(population.Policy),
		}
	}
	sort.Slice(result.Populations, func(left, right int) bool {
		return result.Populations[left].Condition < result.Populations[right].Condition
	})
	for index, caseSpec := range input.Cases {
		result.Cases[index] = CaseSpec{
			Condition: caseSpec.Condition, ID: caseSpec.ID,
			Repetitions:   append(make([]string, 0, len(caseSpec.Repetitions)), caseSpec.Repetitions...),
			RequirePolicy: caseSpec.RequirePolicy,
			Policy:        canonicalPartitionPolicyPointer(caseSpec.Policy),
		}
		sort.Strings(result.Cases[index].Repetitions)
	}
	sort.Slice(result.Cases, func(left, right int) bool {
		if result.Cases[left].Condition != result.Cases[right].Condition {
			return result.Cases[left].Condition < result.Cases[right].Condition
		}
		return result.Cases[left].ID < result.Cases[right].ID
	})
	return result
}

func canonicalSuitePolicy(input SuitePolicy) SuitePolicy {
	result := input
	result.RequiredEvidenceKinds = append(
		make([]string, 0, len(input.RequiredEvidenceKinds)), input.RequiredEvidenceKinds...)
	sort.Strings(result.RequiredEvidenceKinds)
	result.Inference.Confidence, result.Inference.ConfidenceNonFinite = canonicalFinite(
		result.Inference.Confidence, result.Inference.ConfidenceNonFinite)
	result.Pass.Margin, result.Pass.MarginNonFinite = canonicalFinite(
		result.Pass.Margin, result.Pass.MarginNonFinite)
	result.Interaction = canonicalOutcomePolicy(input.Interaction)
	result.Deadline = canonicalOutcomePolicy(input.Deadline)
	result.Latencies = canonicalLatencyPolicies(input.Latencies)
	return result
}

func canonicalPartitionPolicyPointer(input *PartitionPolicy) *PartitionPolicy {
	if input == nil {
		return nil
	}
	result := canonicalPartitionPolicy(*input)
	return &result
}

func canonicalPartitionPolicy(input PartitionPolicy) PartitionPolicy {
	result := input
	result.Pass.Margin, result.Pass.MarginNonFinite = canonicalFinite(
		result.Pass.Margin, result.Pass.MarginNonFinite)
	result.Interaction = canonicalOutcomePolicy(input.Interaction)
	result.Deadline = canonicalOutcomePolicy(input.Deadline)
	result.Latencies = canonicalLatencyPolicies(input.Latencies)
	return result
}

func canonicalOutcomePolicy(input OutcomePolicy) OutcomePolicy {
	result := input
	result.NonInferiority = cloneRatePolicy(input.NonInferiority)
	if result.NonInferiority != nil {
		policy := result.NonInferiority
		policy.Margin, policy.MarginNonFinite = canonicalFinite(policy.Margin, policy.MarginNonFinite)
	}
	return result
}

func canonicalLatencyPolicies(input []LatencyPolicy) []LatencyPolicy {
	result := make([]LatencyPolicy, len(input))
	for index, latency := range input {
		result[index] = latency
		if latency.Gate != nil {
			gate := *latency.Gate
			gate.Limits = append(make([]LatencyLimit, 0, len(latency.Gate.Limits)), latency.Gate.Limits...)
			for limitIndex := range gate.Limits {
				limit := &gate.Limits[limitIndex]
				limit.MaximumIncrease, limit.MaximumIncreaseNonFinite = canonicalFinite(
					limit.MaximumIncrease, limit.MaximumIncreaseNonFinite)
			}
			sort.Slice(gate.Limits, func(left, right int) bool {
				return gate.Limits[left].Statistic < gate.Limits[right].Statistic
			})
			result[index].Gate = &gate
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

func cloneRatePolicy(input *RatePolicy) *RatePolicy {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func canonicalAttempt(input Attempt) Attempt {
	result := input
	result.Axes = append(make([]Axis, 0, len(input.Axes)), input.Axes...)
	result.Latencies = append(make([]Latency, 0, len(input.Latencies)), input.Latencies...)
	for index := range result.Latencies {
		latency := &result.Latencies[index]
		latency.Value, latency.NonFinite = canonicalFinite(latency.Value, latency.NonFinite)
	}
	result.Evidence = append(make([]EvidenceRef, 0, len(input.Evidence)), input.Evidence...)
	slices.SortFunc(result.Axes, func(left, right Axis) int {
		if order := cmp.Compare(left.Name, right.Name); order != 0 {
			return order
		}
		return cmp.Compare(left.Value, right.Value)
	})
	slices.SortFunc(result.Latencies, func(left, right Latency) int {
		if order := cmp.Compare(left.Name, right.Name); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Unit, right.Unit); order != 0 {
			return order
		}
		if order := cmp.Compare(left.NonFinite, right.NonFinite); order != 0 {
			return order
		}
		return cmp.Compare(left.Value, right.Value)
	})
	slices.SortFunc(result.Evidence, func(left, right EvidenceRef) int {
		if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Location, right.Location); order != 0 {
			return order
		}
		return cmp.Compare(left.SHA256, right.SHA256)
	})
	return result
}

func canonicalFloat(value float64) float64 {
	if value == 0 {
		return 0
	}
	return value
}

func canonicalFinite(value float64, marker string) (float64, string) {
	if marker == "" {
		switch {
		case math.IsNaN(value):
			marker = "nan"
		case math.IsInf(value, 1):
			marker = "positive_infinity"
		case math.IsInf(value, -1):
			marker = "negative_infinity"
		}
	}
	if marker != "" {
		return 0, marker
	}
	return canonicalFloat(value), ""
}

func canonicalObserved(arm Arm, attempts []Attempt) []ObservedAttempt {
	type sortableObserved struct {
		row     ObservedAttempt
		encoded string
	}
	result := make([]ObservedAttempt, len(attempts))
	for index, attempt := range attempts {
		result[index] = ObservedAttempt{Arm: arm, Attempt: canonicalAttempt(attempt)}
	}
	sort.SliceStable(result, func(left, right int) bool {
		a, b := result[left], result[right]
		if lessKey(a.Attempt.Key, b.Attempt.Key) {
			return true
		}
		if lessKey(b.Attempt.Key, a.Attempt.Key) {
			return false
		}
		if a.Attempt.ID != b.Attempt.ID {
			return a.Attempt.ID < b.Attempt.ID
		}
		return false
	})
	for start := 0; start < len(result); {
		end := start + 1
		for end < len(result) && result[end].Attempt.Key == result[start].Attempt.Key &&
			result[end].Attempt.ID == result[start].Attempt.ID {
			end++
		}
		if end-start > 1 {
			working := make([]sortableObserved, end-start)
			for index := range working {
				working[index].row = result[start+index]
				working[index].encoded = canonicalJSON(working[index].row.Attempt)
			}
			sort.Slice(working, func(left, right int) bool {
				return working[left].encoded < working[right].encoded
			})
			for index := range working {
				result[start+index] = working[index].row
			}
		}
		start = end
	}
	return result
}

func mergeObserved(left, right []ObservedAttempt) []ObservedAttempt {
	result := make([]ObservedAttempt, 0, len(left)+len(right))
	leftIndex, rightIndex := 0, 0
	for leftIndex < len(left) && rightIndex < len(right) {
		if observedLess(left[leftIndex], right[rightIndex]) {
			result = append(result, left[leftIndex])
			leftIndex++
		} else {
			result = append(result, right[rightIndex])
			rightIndex++
		}
	}
	result = append(result, left[leftIndex:]...)
	result = append(result, right[rightIndex:]...)
	return result
}

func observedLess(left, right ObservedAttempt) bool {
	if lessKey(left.Attempt.Key, right.Attempt.Key) {
		return true
	}
	if lessKey(right.Attempt.Key, left.Attempt.Key) {
		return false
	}
	if left.Arm != right.Arm {
		return left.Arm < right.Arm
	}
	if left.Attempt.ID != right.Attempt.ID {
		return left.Attempt.ID < right.Attempt.ID
	}
	return canonicalJSON(left.Attempt) < canonicalJSON(right.Attempt)
}

func lessKey(left, right AttemptKey) bool {
	if left.Suite != right.Suite {
		return left.Suite < right.Suite
	}
	if left.Condition != right.Condition {
		return left.Condition < right.Condition
	}
	if left.Case != right.Case {
		return left.Case < right.Case
	}
	return left.Repetition < right.Repetition
}

func keyScope(key AttemptKey) string {
	return pathIdentity(key.Suite, key.Condition, key.Case, key.Repetition)
}

// pathIdentity renders user-controlled identifiers into unambiguous logical
// paths used by findings and gate names. Percent must be escaped first so the
// encoding is injective.
func pathIdentity(parts ...string) string {
	length := 0
	if len(parts) > 1 {
		length = len(parts) - 1
	}
	for _, part := range parts {
		length += len(part) + 2*strings.Count(part, "%") + 2*strings.Count(part, "/")
	}
	var result strings.Builder
	result.Grow(length)
	for partIndex, part := range parts {
		if partIndex > 0 {
			result.WriteByte('/')
		}
		for index := 0; index < len(part); index++ {
			switch part[index] {
			case '%':
				result.WriteString("%25")
			case '/':
				result.WriteString("%2F")
			default:
				result.WriteByte(part[index])
			}
		}
	}
	return result.String()
}

func canonicalJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func digestJSON(value any) string {
	digest := sha256.New()
	encoder := json.NewEncoder(digest)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil))
}
