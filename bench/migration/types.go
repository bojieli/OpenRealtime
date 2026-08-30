package migration

// ManifestVersion is the only study-manifest schema this package accepts.
const ManifestVersion = 2

// ReportVersion is the only comparison-report schema this package emits.
const ReportVersion = 2

// CandidatePassRateSanityFloor is the documented absolute floor applied in
// addition to (never instead of) the paired non-inferiority gate. It is only
// evaluated when the observed baseline pass rate is greater than the floor.
const CandidatePassRateSanityFloor = 0.80

// AcceptanceBasisKind identifies the immutable baseline-only evidence from
// which a suite's repetitions, margins, and latency limits were chosen. The
// artifact store must prove the referenced bytes existed before candidate
// results were admitted to a campaign.
const AcceptanceBasisKind = "baseline-variance"

// Arm identifies one side of a migration comparison.
type Arm string

const (
	ArmBaseline  Arm = "baseline"
	ArmCandidate Arm = "candidate"
)

// Axis is one exact provenance or configuration value.
//
// Fixed axes must have this value in both arms. Treatment axes use
// TreatmentDelta because their two values intentionally differ.
type Axis struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TreatmentDelta declares one, and only one, intentional migration change.
type TreatmentDelta struct {
	Axis      string `json:"axis"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
}

// ArmDefinition gives an archival name to one side of the study.
type ArmDefinition struct {
	Name string `json:"name"`
}

// RunKind distinguishes diagnostic evidence from a full release rerun. A
// diagnostic report is useful and reportable but can never be accepted as the
// campaign's release result.
type RunKind string

const (
	RunDiagnostic RunKind = "diagnostic"
	RunFull       RunKind = "full"
)

// ReportReference binds a predecessor's logical run, canonical report, and
// canonical archived JSON bytes. Future storage/signing layers can enforce
// that all three identities remain available without changing this schema.
type ReportReference struct {
	RunID          string `json:"run_id"`
	ReportID       string `json:"report_id"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

// FailureReference identifies an observed failed gate or structural refusal
// in a predecessor. Exactly one of Gate and FindingCode is set.
type FailureReference struct {
	ReportID    string `json:"report_id"`
	Gate        string `json:"gate,omitempty"`
	FindingCode string `json:"finding_code,omitempty"`
}

// CampaignPlan is predeclared lineage for one comparison run. Predecessors is
// the complete transitive campaign history, not a cherry-picked direct edge.
type CampaignPlan struct {
	CampaignID        string             `json:"campaign_id"`
	RunID             string             `json:"run_id"`
	Kind              RunKind            `json:"kind"`
	Predecessors      []ReportReference  `json:"predecessors"`
	DiagnosedFailures []FailureReference `json:"diagnosed_failures"`
}

// AttemptKey is the exact pairing identity. Repetition is explicit: retries
// must receive another predeclared repetition ID and cannot replace a failed
// row silently.
type AttemptKey struct {
	Suite      string `json:"suite"`
	Condition  string `json:"condition"`
	Case       string `json:"case"`
	Repetition string `json:"repetition"`
}

// CaseSpec declares every repetition expected for one benchmark case.
type CaseSpec struct {
	Condition   string   `json:"condition"`
	ID          string   `json:"id"`
	Repetitions []string `json:"repetitions"`
	// RequirePolicy makes omission of this exact case's preregistered policy a
	// structural refusal. Policy may also be supplied without RequirePolicy for
	// explicitly versioned diagnostic studies; whenever present, its gates are
	// acceptance-authoritative.
	RequirePolicy bool             `json:"require_policy"`
	Policy        *PartitionPolicy `json:"policy,omitempty"`
}

// Population is an independently checkable completeness declaration. It
// prevents an omitted condition from disappearing merely because totals were
// derived from the cases that happened to be listed.
type Population struct {
	Condition        string `json:"condition"`
	ExpectedCases    int    `json:"expected_cases"`
	ExpectedAttempts int    `json:"expected_attempts"`
	// RequirePolicy makes omission of this condition's preregistered policy a
	// structural refusal. A present policy always gates acceptance.
	RequirePolicy bool             `json:"require_policy"`
	Policy        *PartitionPolicy `json:"policy,omitempty"`
}

// BootstrapPolicy predeclares the deterministic paired inference procedure.
// Seed and resample count are part of the manifest, not choices made after the
// candidate result is visible.
type BootstrapPolicy struct {
	Confidence          float64 `json:"confidence"`
	ConfidenceNonFinite string  `json:"confidence_non_finite,omitempty"`
	Resamples           int     `json:"resamples"`
	Seed                uint64  `json:"seed"`
}

// RatePolicy is a one-sided non-inferiority gate for a success rate. Margin is
// the largest allowed candidate-minus-baseline decrease, expressed as a ratio.
type RatePolicy struct {
	Margin          float64 `json:"margin"`
	MarginNonFinite string  `json:"margin_non_finite,omitempty"`
	MinimumAttempts int     `json:"minimum_attempts"`
	MinimumCases    int     `json:"minimum_cases"`
}

// OutcomePolicy declares whether an interaction or deadline outcome must be
// applicable to every attempt and, optionally, how its success rate is gated.
type OutcomePolicy struct {
	Required       bool        `json:"required"`
	NonInferiority *RatePolicy `json:"non_inferiority,omitempty"`
}

// SafetyPolicy controls applicability only. Safety itself always has a hard
// zero-tolerance candidate-violation gate and therefore has no margin.
type SafetyPolicy struct {
	Required bool `json:"required"`
}

// LatencyStatistic names a distribution statistic used by a latency gate.
type LatencyStatistic string

const (
	StatisticMean LatencyStatistic = "mean"
	StatisticP50  LatencyStatistic = "p50"
	StatisticP90  LatencyStatistic = "p90"
	StatisticP95  LatencyStatistic = "p95"
	StatisticP99  LatencyStatistic = "p99"
)

// LatencyLimit is a one-sided upper bound on candidate-minus-baseline latency.
type LatencyLimit struct {
	Statistic                LatencyStatistic `json:"statistic"`
	MaximumIncrease          float64          `json:"maximum_increase"`
	MaximumIncreaseNonFinite string           `json:"maximum_increase_non_finite,omitempty"`
}

// LatencyGate predeclares sample requirements and one or more distribution
// limits. Case-cluster bootstrap bounds are computed for each limit.
type LatencyGate struct {
	MinimumAttempts int            `json:"minimum_attempts"`
	MinimumCases    int            `json:"minimum_cases"`
	Limits          []LatencyLimit `json:"limits"`
}

// LatencyPolicy declares one expected latency observation and its unit.
type LatencyPolicy struct {
	Name     string       `json:"name"`
	Unit     string       `json:"unit"`
	Required bool         `json:"required"`
	Gate     *LatencyGate `json:"gate,omitempty"`
}

// PartitionPolicy predeclares non-regression gates for one condition or one
// exact case. The enclosing Population or CaseSpec is the partition identity.
// Safety is deliberately absent: every partition uses the same hard
// zero-tolerance candidate-violation rule as its suite.
type PartitionPolicy struct {
	Pass        RatePolicy      `json:"pass"`
	Interaction OutcomePolicy   `json:"interaction"`
	Deadline    OutcomePolicy   `json:"deadline"`
	Latencies   []LatencyPolicy `json:"latencies"`
}

// SuitePolicy contains all predeclared acceptance choices for one suite.
type SuitePolicy struct {
	Inference       BootstrapPolicy `json:"inference"`
	AcceptanceBasis EvidenceRef     `json:"acceptance_basis"`
	Pass            RatePolicy      `json:"pass"`
	Interaction     OutcomePolicy   `json:"interaction"`
	Deadline        OutcomePolicy   `json:"deadline"`
	Safety          SafetyPolicy    `json:"safety"`
	Latencies       []LatencyPolicy `json:"latencies"`
	RequireEvidence bool            `json:"require_evidence"`
	// RequiredEvidenceKinds names every immutable per-attempt artifact class
	// the suite importer must supply (for example result, graph attestation,
	// and trace). A generic non-empty reference is not sufficient.
	RequiredEvidenceKinds []string `json:"required_evidence_kinds"`
}

// SuiteSpec enumerates the complete population and acceptance policy for one
// suite. Results are never pooled across SuiteSpec values.
type SuiteSpec struct {
	Name               string       `json:"name"`
	ExpectedCases      int          `json:"expected_cases"`
	ExpectedAttempts   int          `json:"expected_attempts"`
	MinimumRepetitions int          `json:"minimum_repetitions"`
	Populations        []Population `json:"populations"`
	Cases              []CaseSpec   `json:"cases"`
	Policy             SuitePolicy  `json:"policy"`
}

// Manifest is the frozen migration analysis plan.
type Manifest struct {
	Version   int              `json:"version"`
	Campaign  CampaignPlan     `json:"campaign"`
	Baseline  ArmDefinition    `json:"baseline"`
	Candidate ArmDefinition    `json:"candidate"`
	FixedAxes []Axis           `json:"fixed_axes"`
	Treatment []TreatmentDelta `json:"treatment"`
	Suites    []SuiteSpec      `json:"suites"`
}

// OutcomeState is explicit about applicability; a missing measurement cannot
// be interpreted as a successful one.
type OutcomeState string

const (
	OutcomeNotApplicable OutcomeState = "not_applicable"
	OutcomeSatisfied     OutcomeState = "satisfied"
	OutcomeFailed        OutcomeState = "failed"
)

// Outcomes records the three cross-suite behavioral dimensions.
type Outcomes struct {
	Interaction OutcomeState `json:"interaction"`
	Deadline    OutcomeState `json:"deadline"`
	Safety      OutcomeState `json:"safety"`
}

// Latency is one named attempt-level observation.
type Latency struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
	// NonFinite is set by canonicalization when an in-memory producer supplied
	// NaN or infinity, which JSON numbers cannot represent. Value is normalized
	// to zero and the attempt is sealed as an explicit structural refusal.
	NonFinite string `json:"non_finite,omitempty"`
}

// EvidenceRef points to immutable evidence for an attempt. Location may be a
// relative artifact path or an external object reference; SHA256 binds it to
// exact content.
type EvidenceRef struct {
	Kind     string `json:"kind"`
	Location string `json:"location"`
	SHA256   string `json:"sha256"`
}

// Attempt is the indivisible observed row. An incomplete attempt remains a row
// and makes the comparison unreportable; it is never removed from the input
// population before analysis.
type Attempt struct {
	ID        string        `json:"id"`
	Key       AttemptKey    `json:"key"`
	Axes      []Axis        `json:"axes"`
	Completed bool          `json:"completed"`
	Passed    bool          `json:"passed"`
	Error     string        `json:"error,omitempty"`
	Outcomes  Outcomes      `json:"outcomes"`
	Latencies []Latency     `json:"latencies"`
	Evidence  []EvidenceRef `json:"evidence"`
}

// ObservedAttempt retains the arm and complete normalized input row.
type ObservedAttempt struct {
	Arm     Arm     `json:"arm"`
	Attempt Attempt `json:"attempt"`
}

// Finding is a deterministic, machine-readable refusal.
type Finding struct {
	Code    string `json:"code"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

// StateTransition retains the exact paired outcome states for inspection.
type StateTransition struct {
	Baseline  OutcomeState `json:"baseline"`
	Candidate OutcomeState `json:"candidate"`
}

// PairedLatency retains each attempt-level latency delta.
type PairedLatency struct {
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Baseline   float64 `json:"baseline"`
	Candidate  float64 `json:"candidate"`
	Difference float64 `json:"difference"`
}

// MatchedAttempt exposes every exact pair used by a valid analysis.
type MatchedAttempt struct {
	Key           AttemptKey      `json:"key"`
	BaselineID    string          `json:"baseline_id"`
	CandidateID   string          `json:"candidate_id"`
	BaselinePass  bool            `json:"baseline_pass"`
	CandidatePass bool            `json:"candidate_pass"`
	Interaction   StateTransition `json:"interaction"`
	Deadline      StateTransition `json:"deadline"`
	Safety        StateTransition `json:"safety"`
	Latencies     []PairedLatency `json:"latencies"`
}

// TransitionCounts describes all four matched binary transitions and the
// pairs for which an outcome was explicitly not applicable.
type TransitionCounts struct {
	SuccessToSuccess int `json:"success_to_success"`
	SuccessToFailure int `json:"success_to_failure"`
	FailureToSuccess int `json:"failure_to_success"`
	FailureToFailure int `json:"failure_to_failure"`
	NotApplicable    int `json:"not_applicable"`
}

// ConfidenceBound records a one-sided bound and the exact inference method.
type ConfidenceBound struct {
	Level     float64 `json:"level"`
	Direction string  `json:"direction"`
	Value     float64 `json:"value"`
	Method    string  `json:"method"`
	Resamples int     `json:"resamples"`
	Seed      uint64  `json:"seed"`
}

// GateResult is one independently inspectable acceptance decision.
type GateResult struct {
	Name      string `json:"name"`
	Evaluated bool   `json:"evaluated"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason,omitempty"`
}

// RateComparison is a paired binary reading for pass, interaction, or
// deadline success.
type RateComparison struct {
	Name               string           `json:"name"`
	ApplicableAttempts int              `json:"applicable_attempts"`
	ApplicableCases    int              `json:"applicable_cases"`
	BaselineSuccesses  int              `json:"baseline_successes"`
	CandidateSuccesses int              `json:"candidate_successes"`
	BaselineRate       float64          `json:"baseline_rate"`
	CandidateRate      float64          `json:"candidate_rate"`
	Difference         float64          `json:"difference"`
	Transitions        TransitionCounts `json:"transitions"`
	Policy             *RatePolicy      `json:"policy,omitempty"`
	LowerBound         *ConfidenceBound `json:"lower_bound,omitempty"`
	Gate               *GateResult      `json:"gate,omitempty"`
}

// Distribution is the full latency distribution retained for an arm or for
// paired attempt-level differences.
type Distribution struct {
	Count int     `json:"count"`
	Unit  string  `json:"unit"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// LatencyLimitResult is the confidence-bound decision for one predeclared
// distribution statistic.
type LatencyLimitResult struct {
	Statistic        LatencyStatistic `json:"statistic"`
	ObservedIncrease float64          `json:"observed_increase"`
	MaximumIncrease  float64          `json:"maximum_increase"`
	UpperBound       *ConfidenceBound `json:"upper_bound,omitempty"`
	Gate             GateResult       `json:"gate"`
}

// LatencyComparison reports both arm distributions, the paired delta
// distribution, and any predeclared tail gates.
type LatencyComparison struct {
	Name             string               `json:"name"`
	Unit             string               `json:"unit"`
	ApplicableCases  int                  `json:"applicable_cases"`
	Baseline         Distribution         `json:"baseline"`
	Candidate        Distribution         `json:"candidate"`
	PairedDifference Distribution         `json:"paired_difference"`
	Limits           []LatencyLimitResult `json:"limits"`
}

// SafetyComparison is a hard zero-tolerance candidate gate.
type SafetyComparison struct {
	ApplicableAttempts  int              `json:"applicable_attempts"`
	ApplicableCases     int              `json:"applicable_cases"`
	BaselineViolations  int              `json:"baseline_violations"`
	CandidateViolations int              `json:"candidate_violations"`
	Transitions         TransitionCounts `json:"transitions"`
	Gate                GateResult       `json:"gate"`
}

// ConditionComparison keeps a condition's paired evidence and, when its
// Population carries a policy, its acceptance-authoritative gates.
type ConditionComparison struct {
	Condition   string              `json:"condition"`
	Gated       bool                `json:"gated"`
	Accepted    bool                `json:"accepted"`
	Pass        RateComparison      `json:"pass"`
	Interaction RateComparison      `json:"interaction"`
	Deadline    RateComparison      `json:"deadline"`
	Safety      SafetyComparison    `json:"safety"`
	Latencies   []LatencyComparison `json:"latencies"`
	Gates       []GateResult        `json:"gates"`
}

// CaseComparison is the equivalent acceptance view for one exact census
// case. It is emitted only when that CaseSpec carries or requires a policy.
type CaseComparison struct {
	Condition   string              `json:"condition"`
	Case        string              `json:"case"`
	Gated       bool                `json:"gated"`
	Accepted    bool                `json:"accepted"`
	Pass        RateComparison      `json:"pass"`
	Interaction RateComparison      `json:"interaction"`
	Deadline    RateComparison      `json:"deadline"`
	Safety      SafetyComparison    `json:"safety"`
	Latencies   []LatencyComparison `json:"latencies"`
	Gates       []GateResult        `json:"gates"`
}

// SuiteComparison is intentionally not pooled with other suites.
type SuiteComparison struct {
	Suite       string                `json:"suite"`
	Accepted    bool                  `json:"accepted"`
	Pass        RateComparison        `json:"pass"`
	PassFloor   GateResult            `json:"pass_floor"`
	Interaction RateComparison        `json:"interaction"`
	Deadline    RateComparison        `json:"deadline"`
	Safety      SafetyComparison      `json:"safety"`
	Latencies   []LatencyComparison   `json:"latencies"`
	Conditions  []ConditionComparison `json:"conditions"`
	Cases       []CaseComparison      `json:"cases"`
	Gates       []GateResult          `json:"gates"`
}

// Report is a deterministic, tamper-evident archival comparison. Attempts is
// always populated, even when Refusals makes the report unreportable.
type Report struct {
	Version         int               `json:"version"`
	ManifestID      string            `json:"manifest_id"`
	ReportID        string            `json:"report_id"`
	Manifest        Manifest          `json:"manifest"`
	SuppliedHistory []ReportReference `json:"supplied_history"`
	Attempts        []ObservedAttempt `json:"attempts"`
	Matched         []MatchedAttempt  `json:"matched"`
	Refusals        []Finding         `json:"refusals"`
	Reportable      bool              `json:"reportable"`
	Accepted        bool              `json:"accepted"`
	CampaignGate    GateResult        `json:"campaign_gate"`
	Comparisons     []SuiteComparison `json:"comparisons"`
}
