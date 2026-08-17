// Package m2 runs the revision-aware microturn cadence ablation.
package m2

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/engine"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/fixture"
	"github.com/bojieli/OpenRealtime/internal/simtime"
	"github.com/bojieli/OpenRealtime/microturn"
)

type PolicyKind string

const (
	PolicyFixed       PolicyKind = "fixed"
	PolicyEventDriven PolicyKind = "event_driven"
	nanosecondsPerMS             = uint64(1_000_000)
)

type Policy struct {
	Name      string     `json:"name"`
	Kind      PolicyKind `json:"kind"`
	CadenceNS uint64     `json:"cadence_ns,omitempty"`
}

func DefaultPolicies() []Policy {
	return []Policy{
		{Name: "fixed_50ms", Kind: PolicyFixed, CadenceNS: 50 * nanosecondsPerMS},
		{Name: "fixed_100ms", Kind: PolicyFixed, CadenceNS: 100 * nanosecondsPerMS},
		{Name: "fixed_200ms", Kind: PolicyFixed, CadenceNS: 200 * nanosecondsPerMS},
		{Name: "fixed_400ms", Kind: PolicyFixed, CadenceNS: 400 * nanosecondsPerMS},
		{Name: "fixed_800ms", Kind: PolicyFixed, CadenceNS: 800 * nanosecondsPerMS},
		{Name: "revision_event", Kind: PolicyEventDriven},
	}
}

type Config struct {
	FixturePath string
	Manifest    reference.Manifest
	Trials      uint64
	Seed        uint64
	FrameMS     uint32
	Timing      simtime.Model
	Policies    []Policy
}

type Action struct {
	Sequence         uint64                  `json:"sequence"`
	AtNS             uint64                  `json:"at_ns"`
	Type             string                  `json:"type"`
	OpportunityID    *uint64                 `json:"opportunity_id,omitempty"`
	SourceRevisionID uint64                  `json:"source_revision_id,omitempty"`
	CandidateID      string                  `json:"candidate_id,omitempty"`
	ReplacementID    string                  `json:"replacement_candidate_id,omitempty"`
	Reason           string                  `json:"reason,omitempty"`
	Trigger          microturn.TriggerReason `json:"trigger,omitempty"`
	order            uint64
}

type Counters struct {
	Microturns      uint64 `json:"microturns"`
	Suppressed      uint64 `json:"suppressed"`
	PlanningJobs    uint64 `json:"planning_jobs"`
	Prepared        uint64 `json:"prepared"`
	Superseded      uint64 `json:"superseded"`
	Cancelled       uint64 `json:"cancelled"`
	StaleResults    uint64 `json:"stale_results"`
	EndpointCancels uint64 `json:"endpoint_cancels"`
}

type Trial struct {
	Index                        uint64   `json:"index"`
	Seed                         uint64   `json:"seed"`
	EndpointNS                   uint64   `json:"endpoint_ns"`
	CandidateReadyNS             uint64   `json:"candidate_ready_ns"`
	FirstOutputPlaybackNS        uint64   `json:"first_output_playback_ns"`
	BaselineLatencyNS            uint64   `json:"baseline_latency_ns"`
	ObservedLatencyNS            uint64   `json:"observed_latency_ns"`
	ResidualPlanningWaitNS       uint64   `json:"residual_planning_wait_ns"`
	PlanningOverlapNS            int64    `json:"planning_overlap_ns"`
	ObservedMinusBaselineNS      int64    `json:"observed_minus_baseline_ns"`
	ReconciliationErrorNS        uint64   `json:"reconciliation_error_ns"`
	CandidatePreparedPreEndpoint bool     `json:"candidate_prepared_pre_endpoint"`
	Counters                     Counters `json:"counters"`
	Actions                      []Action `json:"actions"`
}

type Condition struct {
	Policy                   Policy                                 `json:"policy"`
	Trials                   []Trial                                `json:"trials"`
	Distributions            map[string]analysis.Distribution       `json:"distributions"`
	SignedDistributions      map[string]analysis.SignedDistribution `json:"signed_distributions"`
	PreparedPreEndpointCount uint64                                 `json:"prepared_pre_endpoint_count"`
}

type Report struct {
	SchemaVersion string        `json:"schema_version"`
	Experiment    string        `json:"experiment"`
	TimingMode    string        `json:"timing_mode"`
	EvidenceScope string        `json:"evidence_scope"`
	FixtureSHA256 string        `json:"fixture_sha256"`
	Seed          uint64        `json:"seed"`
	FrameMS       uint32        `json:"frame_ms"`
	TimingModel   simtime.Model `json:"timing_model"`
	Conditions    []Condition   `json:"conditions"`
}

type revisionArrival struct {
	atNS     uint64
	revision engine.PerceptionRevision
}

type planningJob struct {
	opportunity microturn.Opportunity
	revision    engine.PerceptionRevision
	completeNS  uint64
}

type simulationEventKind uint8

const (
	eventRevision simulationEventKind = iota
	eventCandidateCompletion
	eventEndpoint
)

type simulationEvent struct {
	atNS     uint64
	kind     simulationEventKind
	revision engine.PerceptionRevision
	job      planningJob
}

func Run(ctx context.Context, config Config) (Report, error) {
	if config.FixturePath == "" || config.Trials == 0 || config.FrameMS == 0 || len(config.Policies) == 0 {
		return Report{}, errors.New("fixture, positive trials/frame duration, and at least one policy are required")
	}
	if err := validatePolicies(config.Policies); err != nil {
		return Report{}, err
	}
	digest, err := audio.HashFile(config.FixturePath)
	if err != nil {
		return Report{}, err
	}
	fixtureHash := hex.EncodeToString(digest[:])
	if fixtureHash != config.Manifest.FixtureSHA256 {
		return Report{}, fmt.Errorf("fixture SHA-256 %s does not match manifest %s", fixtureHash, config.Manifest.FixtureSHA256)
	}
	input, err := fixture.Load(config.FixturePath, config.FrameMS, "m2-input")
	if err != nil {
		return Report{}, err
	}
	revisions, finalRevision, err := perceive(ctx, config.Manifest, input)
	if err != nil {
		return Report{}, err
	}
	report := Report{
		SchemaVersion: "0.1.0", Experiment: "M2_microturn_cadence_ablation",
		TimingMode: "deterministic_simulation", EvidenceScope: "orchestration_only_no_provider_latency_or_quality",
		FixtureSHA256: fixtureHash, Seed: config.Seed, FrameMS: config.FrameMS,
		TimingModel: config.Timing, Conditions: make([]Condition, 0, len(config.Policies)),
	}
	for _, policy := range config.Policies {
		condition := Condition{Policy: policy, Trials: make([]Trial, 0, config.Trials)}
		for trialIndex := range config.Trials {
			if err := ctx.Err(); err != nil {
				return Report{}, err
			}
			trialSeed := config.Seed + trialIndex
			timing, err := simtime.Sample(config.Timing, trialSeed)
			if err != nil {
				return Report{}, err
			}
			trial, err := runTrial(ctx, trialIndex, trialSeed, policy, config.Manifest, input.EndpointNS, revisions, finalRevision, timing)
			if err != nil {
				return Report{}, fmt.Errorf("policy %s trial %d: %w", policy.Name, trialIndex, err)
			}
			condition.Trials = append(condition.Trials, trial)
		}
		if err := summarizeCondition(&condition); err != nil {
			return Report{}, err
		}
		report.Conditions = append(report.Conditions, condition)
	}
	return report, nil
}

func validatePolicies(policies []Policy) error {
	names := make(map[string]struct{}, len(policies))
	for _, policy := range policies {
		if policy.Name == "" {
			return errors.New("policy name must not be empty")
		}
		if _, exists := names[policy.Name]; exists {
			return fmt.Errorf("duplicate policy name %q", policy.Name)
		}
		names[policy.Name] = struct{}{}
		switch policy.Kind {
		case PolicyFixed:
			if policy.CadenceNS == 0 {
				return fmt.Errorf("fixed policy %q requires positive cadence", policy.Name)
			}
		case PolicyEventDriven:
			if policy.CadenceNS != 0 {
				return fmt.Errorf("event-driven policy %q must not declare cadence", policy.Name)
			}
		default:
			return fmt.Errorf("policy %q has unknown kind %q", policy.Name, policy.Kind)
		}
	}
	return nil
}

func perceive(
	ctx context.Context,
	manifest reference.Manifest,
	input fixture.Input,
) ([]engine.PerceptionRevision, engine.PerceptionRevision, error) {
	provider := reference.NewPerception(manifest)
	var revisions []engine.PerceptionRevision
	for _, frame := range input.Frames {
		produced, err := provider.PushFrame(ctx, frame)
		if err != nil {
			return nil, engine.PerceptionRevision{}, err
		}
		revisions = append(revisions, produced...)
	}
	finalRevision, err := provider.Finalize(ctx, input.SampleCount)
	if err != nil {
		return nil, engine.PerceptionRevision{}, err
	}
	return revisions, finalRevision, nil
}

func runTrial(
	ctx context.Context,
	index uint64,
	seed uint64,
	policy Policy,
	manifest reference.Manifest,
	endpointNS uint64,
	revisions []engine.PerceptionRevision,
	finalRevision engine.PerceptionRevision,
	timing simtime.Sampled,
) (Trial, error) {
	scheduler, err := newScheduler(policy)
	if err != nil {
		return Trial{}, err
	}
	arrivals := make([]revisionArrival, 0, len(revisions))
	for _, revision := range revisions {
		captureNS := revision.SourceSample * 1_000_000_000 / uint64(audio.OpenAIPCMSampleRate)
		arrivalNS, err := checkedAdd(captureNS, timing.StreamingRevisionNS)
		if err != nil {
			return Trial{}, err
		}
		if arrivalNS > endpointNS {
			return Trial{}, errors.New("M2 reference cue arrived after the endpoint")
		}
		arrivals = append(arrivals, revisionArrival{atNS: arrivalNS, revision: revision})
	}

	var actions []Action
	var actionOrder uint64
	addAction := func(action Action) {
		action.order = actionOrder
		actionOrder++
		actions = append(actions, action)
	}
	for _, arrival := range arrivals {
		addAction(Action{AtNS: arrival.atNS, Type: "perception.revision", SourceRevisionID: arrival.revision.RevisionID})
	}

	var opportunities []microturn.Opportunity
	for _, arrival := range arrivals {
		produced, err := scheduler.Observe(microturn.Observation{
			AtNS: arrival.atNS, RevisionID: arrival.revision.RevisionID, HasRevision: true,
		})
		if err != nil {
			return Trial{}, err
		}
		opportunities = append(opportunities, produced...)
	}
	produced, err := scheduler.Observe(microturn.Observation{AtNS: endpointNS, Endpoint: true})
	if err != nil {
		return Trial{}, err
	}
	opportunities = append(opportunities, produced...)

	revisionByID := make(map[uint64]engine.PerceptionRevision, len(revisions))
	for _, revision := range revisions {
		revisionByID[revision.RevisionID] = revision
	}
	requested := make(map[uint64]struct{}, len(revisions))
	jobs := make([]planningJob, 0, len(revisions))
	counters := Counters{}
	for _, opportunity := range opportunities {
		counters.Microturns++
		opportunityID := opportunity.ID
		addAction(Action{
			AtNS: opportunity.OpenedNS, Type: "microturn.opened", OpportunityID: &opportunityID,
			SourceRevisionID: opportunity.SourceRevisionID, Trigger: opportunity.Reason,
		})
		if opportunity.SourceRevisionID == 0 {
			counters.Suppressed++
			addAction(Action{AtNS: opportunity.OpenedNS, Type: "microturn.suppressed", OpportunityID: &opportunityID, Reason: "no_perception_revision"})
			continue
		}
		if _, exists := requested[opportunity.SourceRevisionID]; exists {
			counters.Suppressed++
			addAction(Action{AtNS: opportunity.OpenedNS, Type: "microturn.suppressed", OpportunityID: &opportunityID, SourceRevisionID: opportunity.SourceRevisionID, Reason: "revision_already_planned"})
			continue
		}
		revision, exists := revisionByID[opportunity.SourceRevisionID]
		if !exists {
			return Trial{}, fmt.Errorf("opportunity references unknown revision %d", opportunity.SourceRevisionID)
		}
		completeNS, err := checkedAdd(opportunity.OpenedNS, timing.Stages.PerceptionQueueNS, timing.Stages.CognitionNS)
		if err != nil {
			return Trial{}, err
		}
		requested[revision.RevisionID] = struct{}{}
		counters.PlanningJobs++
		jobs = append(jobs, planningJob{opportunity: opportunity, revision: revision, completeNS: completeNS})
		addAction(Action{AtNS: opportunity.OpenedNS, Type: "candidate.requested", OpportunityID: &opportunityID, SourceRevisionID: revision.RevisionID})
	}

	events := make([]simulationEvent, 0, len(arrivals)+len(jobs)+1)
	for _, arrival := range arrivals {
		events = append(events, simulationEvent{atNS: arrival.atNS, kind: eventRevision, revision: arrival.revision})
	}
	for _, job := range jobs {
		events = append(events, simulationEvent{atNS: job.completeNS, kind: eventCandidateCompletion, job: job})
	}
	events = append(events, simulationEvent{atNS: endpointNS, kind: eventEndpoint})
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].atNS != events[right].atNS {
			return events[left].atNS < events[right].atNS
		}
		return events[left].kind < events[right].kind
	})

	revisionLedger := microturn.NewRevisionLedger()
	candidateLedger := microturn.NewCandidateLedger(revisionLedger)
	cognition := reference.NewCognition(manifest.ResponseText)
	for _, event := range events {
		switch event.kind {
		case eventRevision:
			if err := revisionLedger.Append(event.revision); err != nil {
				return Trial{}, err
			}
		case eventCandidateCompletion:
			latest, exists := revisionLedger.Latest()
			if !exists || latest.RevisionID != event.job.revision.RevisionID {
				counters.Cancelled++
				counters.StaleResults++
				addAction(Action{AtNS: event.atNS, Type: "candidate.cancelled", SourceRevisionID: event.job.revision.RevisionID, Reason: "stale_result"})
				continue
			}
			candidate, err := cognition.Respond(ctx, event.job.revision)
			if err != nil {
				return Trial{}, err
			}
			active, hasActive := candidateLedger.Active()
			if !hasActive {
				if err := candidateLedger.Prepare(candidate, event.atNS); err != nil {
					return Trial{}, err
				}
				counters.Prepared++
				addAction(Action{AtNS: event.atNS, Type: "candidate.prepared", SourceRevisionID: candidate.SourceRevision, CandidateID: candidate.CandidateID})
				continue
			}
			if err := candidateLedger.Supersede(active.Candidate.CandidateID, candidate, event.atNS, "newer_stable_revision"); err != nil {
				return Trial{}, err
			}
			counters.Prepared++
			counters.Superseded++
			addAction(Action{AtNS: event.atNS, Type: "candidate.superseded", SourceRevisionID: active.Candidate.SourceRevision, CandidateID: active.Candidate.CandidateID, ReplacementID: candidate.CandidateID, Reason: "newer_stable_revision"})
			addAction(Action{AtNS: event.atNS, Type: "candidate.prepared", SourceRevisionID: candidate.SourceRevision, CandidateID: candidate.CandidateID})
		case eventEndpoint:
			addAction(Action{AtNS: event.atNS, Type: "turn.endpoint"})
			active, hasActive := candidateLedger.Active()
			if !hasActive {
				continue
			}
			source, _ := revisionLedger.Get(active.Candidate.SourceRevision)
			if source.StableText == finalRevision.StableText {
				continue
			}
			if err := candidateLedger.Cancel(active.Candidate.CandidateID, event.atNS, "candidate_not_valid_for_final_transcript"); err != nil {
				return Trial{}, err
			}
			counters.Cancelled++
			counters.EndpointCancels++
			addAction(Action{AtNS: event.atNS, Type: "candidate.cancelled", SourceRevisionID: active.Candidate.SourceRevision, CandidateID: active.Candidate.CandidateID, Reason: "candidate_not_valid_for_final_transcript"})
		}
	}

	active, exists := candidateLedger.Active()
	if !exists {
		return Trial{}, errors.New("no final-compatible response candidate was prepared")
	}
	source, exists := revisionLedger.Get(active.Candidate.SourceRevision)
	if !exists || source.StableText != finalRevision.StableText {
		return Trial{}, errors.New("active candidate is not valid for the final transcript")
	}
	speech := reference.NewSpeech(100)
	if _, err := speech.Synthesize(ctx, engine.SpeechPlan{CandidateID: active.Candidate.CandidateID, Text: active.Candidate.Text}); err != nil {
		return Trial{}, err
	}
	candidateReadyNS := active.PreparedNS
	residualWaitNS := uint64(0)
	if candidateReadyNS > endpointNS {
		residualWaitNS = candidateReadyNS - endpointNS
	}
	observedNS, err := checkedAdd(residualWaitNS, timing.Stages.CognitionQueueNS, timing.Stages.SpeechFirstChunkNS, timing.Stages.PlaybackQueueNS)
	if err != nil {
		return Trial{}, err
	}
	baselineNS := timing.Stages.Sum()
	b0PlanningNS, err := checkedAdd(timing.Stages.PerceptionFinalizeNS, timing.Stages.PerceptionQueueNS, timing.Stages.CognitionNS)
	if err != nil {
		return Trial{}, err
	}
	planningOverlapNS, err := signedDifference(b0PlanningNS, residualWaitNS)
	if err != nil {
		return Trial{}, err
	}
	observedMinusBaselineNS, err := signedDifference(observedNS, baselineNS)
	if err != nil {
		return Trial{}, err
	}
	if planningOverlapNS != -observedMinusBaselineNS {
		return Trial{}, errors.New("planning-overlap attribution does not reconcile")
	}
	firstOutputNS, err := checkedAdd(endpointNS, observedNS)
	if err != nil {
		return Trial{}, err
	}
	addAction(Action{AtNS: firstOutputNS, Type: "output.playback_started", SourceRevisionID: active.Candidate.SourceRevision, CandidateID: active.Candidate.CandidateID})
	sort.SliceStable(actions, func(left, right int) bool {
		if actions[left].AtNS != actions[right].AtNS {
			return actions[left].AtNS < actions[right].AtNS
		}
		return actions[left].order < actions[right].order
	})
	for sequence := range actions {
		actions[sequence].Sequence = uint64(sequence)
		actions[sequence].order = 0
	}
	return Trial{
		Index: index, Seed: seed, EndpointNS: endpointNS, CandidateReadyNS: candidateReadyNS,
		FirstOutputPlaybackNS: firstOutputNS, BaselineLatencyNS: baselineNS, ObservedLatencyNS: observedNS,
		ResidualPlanningWaitNS: residualWaitNS, PlanningOverlapNS: planningOverlapNS,
		ObservedMinusBaselineNS: observedMinusBaselineNS, ReconciliationErrorNS: 0,
		CandidatePreparedPreEndpoint: candidateReadyNS < endpointNS, Counters: counters, Actions: actions,
	}, nil
}

func newScheduler(policy Policy) (microturn.Scheduler, error) {
	switch policy.Kind {
	case PolicyFixed:
		return microturn.NewFixedScheduler(policy.CadenceNS)
	case PolicyEventDriven:
		return microturn.NewEventScheduler(), nil
	default:
		return nil, fmt.Errorf("unsupported policy kind %q", policy.Kind)
	}
}

func summarizeCondition(condition *Condition) error {
	metrics := map[string][]uint64{
		"baseline_latency_ns": {}, "observed_latency_ns": {}, "residual_planning_wait_ns": {},
		"reconciliation_error_ns": {}, "microturn_count": {}, "suppressed_count": {},
		"planning_job_count": {}, "candidate_cancelled_count": {},
	}
	signedMetrics := map[string][]int64{
		"planning_overlap_ns": {}, "observed_minus_baseline_ns": {}, "candidate_ready_relative_to_endpoint_ns": {},
	}
	for _, trial := range condition.Trials {
		metrics["baseline_latency_ns"] = append(metrics["baseline_latency_ns"], trial.BaselineLatencyNS)
		metrics["observed_latency_ns"] = append(metrics["observed_latency_ns"], trial.ObservedLatencyNS)
		metrics["residual_planning_wait_ns"] = append(metrics["residual_planning_wait_ns"], trial.ResidualPlanningWaitNS)
		metrics["reconciliation_error_ns"] = append(metrics["reconciliation_error_ns"], trial.ReconciliationErrorNS)
		metrics["microturn_count"] = append(metrics["microturn_count"], trial.Counters.Microturns)
		metrics["suppressed_count"] = append(metrics["suppressed_count"], trial.Counters.Suppressed)
		metrics["planning_job_count"] = append(metrics["planning_job_count"], trial.Counters.PlanningJobs)
		metrics["candidate_cancelled_count"] = append(metrics["candidate_cancelled_count"], trial.Counters.Cancelled)
		signedMetrics["planning_overlap_ns"] = append(signedMetrics["planning_overlap_ns"], trial.PlanningOverlapNS)
		signedMetrics["observed_minus_baseline_ns"] = append(signedMetrics["observed_minus_baseline_ns"], trial.ObservedMinusBaselineNS)
		readyRelative, err := signedDifference(trial.CandidateReadyNS, trial.EndpointNS)
		if err != nil {
			return err
		}
		signedMetrics["candidate_ready_relative_to_endpoint_ns"] = append(signedMetrics["candidate_ready_relative_to_endpoint_ns"], readyRelative)
		if trial.CandidatePreparedPreEndpoint {
			condition.PreparedPreEndpointCount++
		}
	}
	condition.Distributions = make(map[string]analysis.Distribution, len(metrics))
	for name, values := range metrics {
		distribution, err := analysis.Summarize(values)
		if err != nil {
			return err
		}
		condition.Distributions[name] = distribution
	}
	condition.SignedDistributions = make(map[string]analysis.SignedDistribution, len(signedMetrics))
	for name, values := range signedMetrics {
		distribution, err := analysis.SummarizeSigned(values)
		if err != nil {
			return err
		}
		condition.SignedDistributions[name] = distribution
	}
	return nil
}

func checkedAdd(values ...uint64) (uint64, error) {
	var sum uint64
	for _, value := range values {
		if value > math.MaxUint64-sum {
			return 0, errors.New("nanosecond timing overflow")
		}
		sum += value
	}
	return sum, nil
}

func signedDifference(left, right uint64) (int64, error) {
	if left >= right {
		difference := left - right
		if difference > math.MaxInt64 {
			return 0, errors.New("signed nanosecond difference overflow")
		}
		return int64(difference), nil
	}
	difference := right - left
	if difference > math.MaxInt64 {
		return 0, errors.New("signed nanosecond difference overflow")
	}
	return -int64(difference), nil
}
