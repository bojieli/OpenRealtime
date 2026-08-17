package reference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bojieli/OpenRealtime/engine"
)

type DifficultTask struct {
	ID               string `json:"id"`
	Question         string `json:"question"`
	Acknowledgement  string `json:"acknowledgement"`
	FastAnswer       string `json:"fast_answer"`
	SlowAnswer       string `json:"slow_answer"`
	FastQualityScore uint32 `json:"fast_quality_score"`
	SlowQualityScore uint32 `json:"slow_quality_score"`
	FastComputeUnits uint64 `json:"fast_compute_units"`
	SlowComputeUnits uint64 `json:"slow_compute_units"`
}

type DifficultWorkload struct {
	SchemaVersion string          `json:"schema_version"`
	EvidenceMode  string          `json:"evidence_mode"`
	Tasks         []DifficultTask `json:"tasks"`
}

func LoadDifficultWorkload(path string) (DifficultWorkload, error) {
	file, err := os.Open(path)
	if err != nil {
		return DifficultWorkload{}, fmt.Errorf("open difficult workload: %w", err)
	}
	defer file.Close()
	const maximumBytes = int64(1 << 20)
	metadata, err := file.Stat()
	if err != nil {
		return DifficultWorkload{}, fmt.Errorf("stat difficult workload: %w", err)
	}
	if metadata.Size() > maximumBytes {
		return DifficultWorkload{}, errors.New("difficult workload exceeds 1 MiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maximumBytes+1))
	decoder.DisallowUnknownFields()
	var workload DifficultWorkload
	if err := decoder.Decode(&workload); err != nil {
		return DifficultWorkload{}, fmt.Errorf("decode difficult workload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return DifficultWorkload{}, errors.New("difficult workload must contain exactly one JSON value")
		}
		return DifficultWorkload{}, fmt.Errorf("decode trailing difficult workload data: %w", err)
	}
	if workload.SchemaVersion != "0.1.0" || workload.EvidenceMode != "symbolic_scored_reference" || len(workload.Tasks) == 0 {
		return DifficultWorkload{}, errors.New("unsupported or empty difficult workload")
	}
	ids := make(map[string]struct{}, len(workload.Tasks))
	for index, task := range workload.Tasks {
		if task.ID == "" || strings.TrimSpace(task.Question) == "" || strings.TrimSpace(task.Acknowledgement) == "" ||
			strings.TrimSpace(task.FastAnswer) == "" || strings.TrimSpace(task.SlowAnswer) == "" {
			return DifficultWorkload{}, fmt.Errorf("difficult task %d has empty identity or text", index)
		}
		if task.FastQualityScore > 100 || task.SlowQualityScore > 100 || task.FastComputeUnits == 0 || task.SlowComputeUnits == 0 {
			return DifficultWorkload{}, fmt.Errorf("difficult task %q has invalid scores or cost", task.ID)
		}
		if _, exists := ids[task.ID]; exists {
			return DifficultWorkload{}, fmt.Errorf("duplicate difficult task %q", task.ID)
		}
		ids[task.ID] = struct{}{}
	}
	return workload, nil
}

type FastMode string

const (
	FastModeAcknowledge FastMode = "acknowledge"
	FastModeAnswer      FastMode = "answer"
)

type Fast struct {
	task DifficultTask
	mode FastMode
}

func NewFast(task DifficultTask, mode FastMode) *Fast { return &Fast{task: task, mode: mode} }

func (provider *Fast) Name() string { return "reference.fast_decision" }

func (provider *Fast) Capabilities() engine.Capabilities {
	return engine.Capabilities{engine.CapabilityCancellation: true, engine.CapabilityDeterministic: true}
}

func (provider *Fast) Decide(ctx context.Context, goal engine.GoalSnapshot) (engine.FastDecision, error) {
	if err := ctx.Err(); err != nil {
		return engine.FastDecision{}, err
	}
	if goal.GoalID != provider.task.ID || strings.TrimSpace(goal.Question) != provider.task.Question {
		return engine.FastDecision{}, errors.New("reference fast provider received the wrong task")
	}
	decision := engine.FastDecision{GoalID: goal.GoalID, RevisionID: goal.RevisionID, ProgressClaim: engine.ProgressNone}
	switch provider.mode {
	case FastModeAcknowledge:
		decision.Action = engine.FastAcknowledge
		decision.Text = provider.task.Acknowledgement
		decision.StartSlowPath = true
	case FastModeAnswer:
		decision.Action = engine.FastAnswer
		decision.Text = provider.task.FastAnswer
	default:
		return engine.FastDecision{}, errors.New("unknown reference fast mode")
	}
	return decision, nil
}

type DeliberationOutcome string

const (
	DeliberationComplete DeliberationOutcome = "complete"
	DeliberationFail     DeliberationOutcome = "fail"
)

type Deliberation struct {
	task    DifficultTask
	outcome DeliberationOutcome
}

func NewDeliberation(task DifficultTask, outcome DeliberationOutcome) *Deliberation {
	return &Deliberation{task: task, outcome: outcome}
}

func (provider *Deliberation) Name() string { return "reference.scripted_deliberation" }

func (provider *Deliberation) Capabilities() engine.Capabilities {
	return engine.Capabilities{engine.CapabilityCancellation: true, engine.CapabilityDeterministic: true}
}

func (provider *Deliberation) Deliberate(
	ctx context.Context,
	goal engine.GoalSnapshot,
	consume func(engine.DeliberationUpdate) error,
) error {
	if consume == nil {
		return errors.New("reference deliberation requires an update consumer")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if goal.GoalID != provider.task.ID || goal.Question != provider.task.Question {
		return errors.New("reference deliberation received the wrong task")
	}
	if err := consume(engine.DeliberationUpdate{
		GoalID: goal.GoalID, RevisionID: goal.RevisionID, Sequence: 0,
		Status: engine.DeliberationProgress, Text: "reference deliberation is evaluating the symbolic evidence",
		ComputeUnits: provider.task.SlowComputeUnits / 2,
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch provider.outcome {
	case DeliberationComplete:
		return consume(engine.DeliberationUpdate{
			GoalID: goal.GoalID, RevisionID: goal.RevisionID, Sequence: 1,
			Status: engine.DeliberationFinal, Text: provider.task.SlowAnswer,
			ComputeUnits: provider.task.SlowComputeUnits, QualityScore: provider.task.SlowQualityScore,
		})
	case DeliberationFail:
		return consume(engine.DeliberationUpdate{
			GoalID: goal.GoalID, RevisionID: goal.RevisionID, Sequence: 1,
			Status: engine.DeliberationFailed, Error: "injected reference deliberation failure",
			ComputeUnits: provider.task.SlowComputeUnits / 2,
		})
	default:
		return errors.New("unknown reference deliberation outcome")
	}
}
