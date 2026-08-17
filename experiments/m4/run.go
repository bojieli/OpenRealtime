// Package m4 runs the deterministic fast/slow cognition reference study.
package m4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/engine"
)

type ConditionKind string

const (
	ConditionSingleBlocking ConditionKind = "single_blocking"
	ConditionFastOnly       ConditionKind = "fast_only"
	ConditionFastSlow       ConditionKind = "fast_slow"
)

type Config struct {
	WorkloadPath string
	Workload     reference.DifficultWorkload
	Trials       uint64
	Seed         uint64
}

type TimingModel struct {
	FastBaseNS   uint64 `json:"fast_base_ns"`
	FastJitterNS uint64 `json:"fast_jitter_ns"`
	SlowBaseNS   uint64 `json:"slow_base_ns"`
	SlowJitterNS uint64 `json:"slow_jitter_ns"`
	DeadlineNS   uint64 `json:"deadline_ns"`
}

func DefaultTimingModel() TimingModel {
	return TimingModel{
		FastBaseNS: 35_000_000, FastJitterNS: 10_000_000,
		SlowBaseNS: 400_000_000, SlowJitterNS: 100_000_000,
		DeadlineNS: 450_000_000,
	}
}

type Trial struct {
	Index                   uint64              `json:"index"`
	Seed                    uint64              `json:"seed"`
	TaskID                  string              `json:"task_id"`
	Condition               ConditionKind       `json:"condition"`
	FirstTruthfulProgressNS uint64              `json:"first_truthful_progress_ns"`
	FinalAnswerNS           uint64              `json:"final_answer_ns"`
	QualityScore            uint32              `json:"quality_score"`
	ComputeUnits            uint64              `json:"compute_units"`
	TaskSuccess             bool                `json:"task_success"`
	TruthfulForeground      bool                `json:"truthful_foreground"`
	DeadlineMissed          bool                `json:"deadline_missed"`
	ForegroundText          string              `json:"foreground_text"`
	FinalText               string              `json:"final_text"`
	SlowState               cognition.SlowState `json:"slow_state"`
	Coordinator             *cognition.Snapshot `json:"coordinator,omitempty"`
}

type Condition struct {
	Kind                  ConditionKind               `json:"kind"`
	Trials                []Trial                     `json:"trials"`
	FirstTruthfulProgress analysis.Distribution       `json:"first_truthful_progress_ns"`
	FinalAnswer           analysis.Distribution       `json:"final_answer_ns"`
	Quality               analysis.ScalarDistribution `json:"quality_score"`
	Compute               analysis.ScalarDistribution `json:"compute_units"`
	TaskSuccessCount      uint64                      `json:"task_success_count"`
	TruthViolationCount   uint64                      `json:"truth_violation_count"`
	DeadlineMissCount     uint64                      `json:"deadline_miss_count"`
}

type SafetyScenario struct {
	Name             string              `json:"name"`
	FinalState       cognition.SlowState `json:"final_state"`
	StaleRejectCount uint64              `json:"stale_reject_count"`
	StaleAccepted    bool                `json:"stale_accepted"`
	Closed           bool                `json:"closed"`
}

type Report struct {
	SchemaVersion              string           `json:"schema_version"`
	Experiment                 string           `json:"experiment"`
	TimingMode                 string           `json:"timing_mode"`
	EvidenceScope              string           `json:"evidence_scope"`
	WorkloadSHA256             string           `json:"workload_sha256"`
	Seed                       uint64           `json:"seed"`
	TrialsPerTask              uint64           `json:"trials_per_task"`
	TimingModel                TimingModel      `json:"timing_model"`
	Conditions                 []Condition      `json:"conditions"`
	SafetyScenarios            []SafetyScenario `json:"safety_scenarios"`
	FabricatedProgressRejected bool             `json:"fabricated_progress_rejected"`
}

func Run(ctx context.Context, config Config) (Report, error) {
	if config.WorkloadPath == "" || config.Trials == 0 || len(config.Workload.Tasks) == 0 {
		return Report{}, errors.New("M4 requires a workload path, tasks, and positive trial count")
	}
	digest, err := hashFile(config.WorkloadPath)
	if err != nil {
		return Report{}, err
	}
	timing := DefaultTimingModel()
	report := Report{
		SchemaVersion: "0.1.0", Experiment: "M4_fast_slow_cognition",
		TimingMode: "deterministic_simulation", EvidenceScope: "symbolic_quality_and_compute_units",
		WorkloadSHA256: digest, Seed: config.Seed, TrialsPerTask: config.Trials, TimingModel: timing,
		Conditions: make([]Condition, 0, 3),
	}
	for _, kind := range []ConditionKind{ConditionSingleBlocking, ConditionFastOnly, ConditionFastSlow} {
		condition := Condition{Kind: kind, Trials: make([]Trial, 0, config.Trials*uint64(len(config.Workload.Tasks)))}
		for taskIndex, task := range config.Workload.Tasks {
			for trialIndex := range config.Trials {
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				seed := config.Seed + trialIndex
				trial, err := runTrial(ctx, kind, trialIndex, seed, uint64(taskIndex), task, timing)
				if err != nil {
					return Report{}, fmt.Errorf("condition %s task %s trial %d: %w", kind, task.ID, trialIndex, err)
				}
				condition.Trials = append(condition.Trials, trial)
			}
		}
		if err := summarizeCondition(&condition); err != nil {
			return Report{}, err
		}
		report.Conditions = append(report.Conditions, condition)
	}
	report.SafetyScenarios, err = runSafetyScenarios(config.Workload.Tasks[0])
	if err != nil {
		return Report{}, err
	}
	goal := engine.GoalSnapshot{GoalID: "truth", RevisionID: 1, Question: "truth check"}
	fabricated := engine.FastDecision{
		GoalID: goal.GoalID, RevisionID: goal.RevisionID, Action: engine.FastAcknowledge,
		Text: "I have completed the external check.", ProgressClaim: engine.ProgressCompleted,
	}
	report.FabricatedProgressRejected = cognition.ValidateFastDecision(goal, fabricated, cognition.SlowRunning) != nil
	if !report.FabricatedProgressRejected {
		return Report{}, errors.New("fabricated progress claim was accepted")
	}
	return report, nil
}

func runTrial(
	ctx context.Context,
	kind ConditionKind,
	index uint64,
	seed uint64,
	taskIndex uint64,
	task reference.DifficultTask,
	timing TimingModel,
) (Trial, error) {
	fastNS := sampleDelay(timing.FastBaseNS, timing.FastJitterNS, seed, taskIndex^0x51ed270b)
	slowNS := sampleDelay(timing.SlowBaseNS, timing.SlowJitterNS, seed, taskIndex^0x94d049bb)
	goal := engine.GoalSnapshot{
		GoalID: task.ID, RevisionID: 1, Question: task.Question,
		CreatedNS: 0, DeadlineNS: timing.DeadlineNS,
	}
	trial := Trial{Index: index, Seed: seed, TaskID: task.ID, Condition: kind, TruthfulForeground: true}
	switch kind {
	case ConditionFastOnly:
		decision, err := reference.NewFast(task, reference.FastModeAnswer).Decide(ctx, goal)
		if err != nil {
			return Trial{}, err
		}
		if err := cognition.ValidateFastDecision(goal, decision, cognition.SlowIdle); err != nil {
			return Trial{}, err
		}
		trial.FirstTruthfulProgressNS = fastNS
		trial.FinalAnswerNS = fastNS
		trial.ForegroundText = decision.Text
		trial.FinalText = decision.Text
		trial.QualityScore = task.FastQualityScore
		trial.ComputeUnits = task.FastComputeUnits
		trial.SlowState = cognition.SlowIdle
	case ConditionSingleBlocking, ConditionFastSlow:
		coordinator := cognition.NewCoordinator()
		if err := coordinator.Begin(goal, 0); err != nil {
			return Trial{}, err
		}
		if kind == ConditionFastSlow {
			decision, err := reference.NewFast(task, reference.FastModeAcknowledge).Decide(ctx, goal)
			if err != nil {
				return Trial{}, err
			}
			if err := cognition.ValidateFastDecision(goal, decision, cognition.SlowRunning); err != nil {
				return Trial{}, err
			}
			trial.FirstTruthfulProgressNS = fastNS
			trial.ForegroundText = decision.Text
			trial.ComputeUnits += task.FastComputeUnits
		}
		updates, err := collectUpdates(ctx, reference.NewDeliberation(task, reference.DeliberationComplete), goal)
		if err != nil {
			return Trial{}, err
		}
		for updateIndex, update := range updates {
			atNS := slowNS
			if updateIndex == 0 {
				atNS = slowNS / 2
			}
			accepted, err := coordinator.Accept(update, atNS)
			if err != nil {
				return Trial{}, fmt.Errorf("reference slow update failed: %w", err)
			}
			if !accepted {
				return Trial{}, errors.New("reference slow update was stale")
			}
		}
		if err := coordinator.ValidateClosed(); err != nil {
			return Trial{}, err
		}
		snapshot := coordinator.Snapshot()
		if kind == ConditionSingleBlocking {
			trial.FirstTruthfulProgressNS = slowNS
			trial.ForegroundText = task.SlowAnswer
		}
		trial.FinalAnswerNS = slowNS
		trial.FinalText = task.SlowAnswer
		trial.QualityScore = task.SlowQualityScore
		trial.ComputeUnits += task.SlowComputeUnits
		trial.SlowState = snapshot.State
		trial.DeadlineMissed = snapshot.DeadlineMissed
		trial.Coordinator = &snapshot
	default:
		return Trial{}, errors.New("unknown M4 condition")
	}
	trial.TaskSuccess = trial.QualityScore >= 80
	return trial, nil
}

func collectUpdates(
	ctx context.Context,
	provider engine.DeliberationProvider,
	goal engine.GoalSnapshot,
) ([]engine.DeliberationUpdate, error) {
	var updates []engine.DeliberationUpdate
	err := provider.Deliberate(ctx, goal, func(update engine.DeliberationUpdate) error {
		updates = append(updates, update)
		return nil
	})
	return updates, err
}

func runSafetyScenarios(task reference.DifficultTask) ([]SafetyScenario, error) {
	goal1 := engine.GoalSnapshot{GoalID: task.ID, RevisionID: 1, Question: task.Question, DeadlineNS: 500_000_000}
	goal2 := goal1
	goal2.RevisionID = 2
	var scenarios []SafetyScenario

	complete := cognition.NewCoordinator()
	if err := complete.Begin(goal1, 0); err != nil {
		return nil, err
	}
	if _, err := complete.Accept(engine.DeliberationUpdate{GoalID: task.ID, RevisionID: 1, Sequence: 0, Status: engine.DeliberationFinal, Text: task.SlowAnswer}, 100); err != nil {
		return nil, err
	}
	scenarios = append(scenarios, safety("completed", complete, false))

	failed := cognition.NewCoordinator()
	if err := failed.Begin(goal1, 0); err != nil {
		return nil, err
	}
	if _, err := failed.Accept(engine.DeliberationUpdate{GoalID: task.ID, RevisionID: 1, Sequence: 0, Status: engine.DeliberationFailed, Error: "injected failure"}, 100); err != nil {
		return nil, err
	}
	scenarios = append(scenarios, safety("failed", failed, false))

	cancelled := cognition.NewCoordinator()
	if err := cancelled.Begin(goal1, 0); err != nil {
		return nil, err
	}
	if err := cancelled.Cancel(50, "user cancelled"); err != nil {
		return nil, err
	}
	scenarios = append(scenarios, safety("cancelled", cancelled, false))

	stale := cognition.NewCoordinator()
	if err := stale.Begin(goal1, 0); err != nil {
		return nil, err
	}
	if err := stale.Cancel(50, "goal revised"); err != nil {
		return nil, err
	}
	if err := stale.Begin(goal2, 51); err != nil {
		return nil, err
	}
	accepted, err := stale.Accept(engine.DeliberationUpdate{GoalID: task.ID, RevisionID: 1, Sequence: 0, Status: engine.DeliberationFinal, Text: "stale"}, 60)
	if err != nil {
		return nil, err
	}
	if _, err := stale.Accept(engine.DeliberationUpdate{GoalID: task.ID, RevisionID: 2, Sequence: 0, Status: engine.DeliberationFinal, Text: task.SlowAnswer}, 70); err != nil {
		return nil, err
	}
	scenarios = append(scenarios, safety("stale_revision_rejected", stale, accepted))
	return scenarios, nil
}

func safety(name string, coordinator *cognition.Coordinator, staleAccepted bool) SafetyScenario {
	snapshot := coordinator.Snapshot()
	return SafetyScenario{
		Name: name, FinalState: snapshot.State, StaleRejectCount: snapshot.StaleRejectCount,
		StaleAccepted: staleAccepted, Closed: coordinator.ValidateClosed() == nil,
	}
}

func summarizeCondition(condition *Condition) error {
	first := make([]uint64, 0, len(condition.Trials))
	final := make([]uint64, 0, len(condition.Trials))
	quality := make([]uint64, 0, len(condition.Trials))
	compute := make([]uint64, 0, len(condition.Trials))
	for _, trial := range condition.Trials {
		first = append(first, trial.FirstTruthfulProgressNS)
		final = append(final, trial.FinalAnswerNS)
		quality = append(quality, uint64(trial.QualityScore))
		compute = append(compute, trial.ComputeUnits)
		if trial.TaskSuccess {
			condition.TaskSuccessCount++
		}
		if !trial.TruthfulForeground {
			condition.TruthViolationCount++
		}
		if trial.DeadlineMissed {
			condition.DeadlineMissCount++
		}
	}
	var err error
	condition.FirstTruthfulProgress, err = analysis.Summarize(first)
	if err != nil {
		return err
	}
	condition.FinalAnswer, err = analysis.Summarize(final)
	if err != nil {
		return err
	}
	condition.Quality, err = analysis.SummarizeScalar(quality)
	if err != nil {
		return err
	}
	condition.Compute, err = analysis.SummarizeScalar(compute)
	return err
}

func sampleDelay(base, jitter, seed, stream uint64) uint64 {
	if jitter == 0 {
		return base
	}
	random := rand.New(rand.NewPCG(seed^0x9e3779b97f4a7c15, stream^0x243f6a8885a308d3))
	return base - jitter + random.Uint64N(jitter*2+1)
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
