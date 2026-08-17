// Package speech implements the boundary between replaceable audio and played history.
package speech

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/engine"
)

type PlanState string

const (
	PlanActive    PlanState = "active"
	PlanCompleted PlanState = "completed"
	PlanCancelled PlanState = "cancelled"
)

type CancelMode string

const (
	CancelYield      CancelMode = "yield"
	CancelInvalidate CancelMode = "invalidate"
)

type Config struct {
	PlanID                  string
	CandidateID             string
	Text                    string
	SampleRateHz            uint32
	MaxPreparedAheadSamples uint64
	MaxQueuedAheadSamples   uint64
}

type Cancellation struct {
	Mode                     CancelMode `json:"mode"`
	PlayedSamples            uint64     `json:"played_samples"`
	DiscardedQueuedSamples   uint64     `json:"discarded_queued_samples"`
	DiscardedPreparedSamples uint64     `json:"discarded_prepared_samples"`
	RepairRequired           bool       `json:"repair_required"`
}

type Repair struct {
	AtNS          uint64 `json:"at_ns"`
	CandidateID   string `json:"candidate_id"`
	PlayedSamples uint64 `json:"played_samples"`
	Text          string `json:"text"`
}

type PlayedSpan struct {
	CandidateID   string `json:"candidate_id"`
	Text          string `json:"text"`
	PlayedSamples uint64 `json:"played_samples"`
}

type Operation struct {
	AtNS          uint64     `json:"at_ns"`
	Type          string     `json:"type"`
	ThroughSample uint64     `json:"through_sample,omitempty"`
	Mode          CancelMode `json:"mode,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

type Snapshot struct {
	PlanID          string        `json:"plan_id"`
	CandidateID     string        `json:"candidate_id"`
	Text            string        `json:"text"`
	SampleRateHz    uint32        `json:"sample_rate_hz"`
	State           PlanState     `json:"state"`
	PreparedSamples uint64        `json:"prepared_samples"`
	QueuedSamples   uint64        `json:"queued_samples"`
	PlayedSamples   uint64        `json:"played_samples"`
	FinalPrepared   bool          `json:"final_prepared"`
	PendingRepair   bool          `json:"pending_repair"`
	Cancellation    *Cancellation `json:"cancellation,omitempty"`
	PlayedHistory   []PlayedSpan  `json:"played_history"`
	Repairs         []Repair      `json:"repairs"`
	Operations      []Operation   `json:"operations"`
}

type Plan struct {
	mu sync.Mutex

	config        Config
	state         PlanState
	chunks        []engine.SpeechChunk
	prepared      uint64
	queued        uint64
	played        uint64
	finalPrepared bool
	pendingRepair bool
	cancellation  *Cancellation
	repairs       []Repair
	operations    []Operation
	hasTime       bool
	lastNS        uint64
}

func NewPlan(config Config) (*Plan, error) {
	if config.PlanID == "" || config.CandidateID == "" || strings.TrimSpace(config.Text) == "" {
		return nil, errors.New("speech plan ID, candidate ID, and text must not be empty")
	}
	if config.SampleRateHz == 0 || config.MaxPreparedAheadSamples == 0 || config.MaxQueuedAheadSamples == 0 {
		return nil, errors.New("speech sample rate and buffer bounds must be positive")
	}
	if config.MaxQueuedAheadSamples > config.MaxPreparedAheadSamples {
		return nil, errors.New("queued-audio bound cannot exceed prepared-audio bound")
	}
	return &Plan{config: config, state: PlanActive}, nil
}

func (plan *Plan) Prepare(chunk engine.SpeechChunk, atNS uint64) error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkActiveAndTime(atNS); err != nil {
		return err
	}
	if plan.finalPrepared {
		return errors.New("cannot prepare audio after the final speech chunk")
	}
	if chunk.ChunkID == "" || chunk.CandidateID != plan.config.CandidateID {
		return errors.New("speech chunk must identify the plan candidate")
	}
	if chunk.SampleRateHz != plan.config.SampleRateHz || len(chunk.PCM16LE) == 0 || len(chunk.PCM16LE)%2 != 0 {
		return errors.New("speech chunk format does not match the PCM16 plan")
	}
	if chunk.SampleOffset != plan.prepared {
		return fmt.Errorf("speech chunks must be contiguous: got offset %d, want %d", chunk.SampleOffset, plan.prepared)
	}
	chunkSamples := uint64(len(chunk.PCM16LE) / 2)
	if chunkSamples > math.MaxUint64-plan.prepared {
		return errors.New("prepared speech sample offset overflow")
	}
	if chunkSamples > plan.config.MaxPreparedAheadSamples-(plan.prepared-plan.played) {
		return errors.New("prepared speech exceeds the bounded lookahead")
	}
	chunk.PCM16LE = slices.Clone(chunk.PCM16LE)
	plan.chunks = append(plan.chunks, chunk)
	plan.prepared += chunkSamples
	plan.finalPrepared = chunk.Final
	plan.record(atNS, Operation{Type: "speech.prepared", ThroughSample: plan.prepared})
	return nil
}

func (plan *Plan) QueueThrough(sample uint64, atNS uint64) error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkActiveAndTime(atNS); err != nil {
		return err
	}
	if sample < plan.queued || sample > plan.prepared {
		return errors.New("queued audio must advance within prepared audio")
	}
	if sample-plan.played > plan.config.MaxQueuedAheadSamples {
		return errors.New("queued speech exceeds the commit horizon")
	}
	if sample != plan.queued && !plan.isChunkBoundary(sample) {
		return errors.New("audio may only be queued through a complete speech chunk")
	}
	plan.queued = sample
	plan.record(atNS, Operation{Type: "speech.queued", ThroughSample: sample})
	return nil
}

func (plan *Plan) MarkPlayedThrough(sample uint64, atNS uint64) error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkActiveAndTime(atNS); err != nil {
		return err
	}
	if sample < plan.played || sample > plan.queued {
		return errors.New("played audio must advance within queued audio")
	}
	plan.played = sample
	plan.record(atNS, Operation{Type: "speech.played", ThroughSample: sample})
	return nil
}

func (plan *Plan) Cancel(mode CancelMode, reason string, atNS uint64) (Cancellation, error) {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkActiveAndTime(atNS); err != nil {
		return Cancellation{}, err
	}
	if mode != CancelYield && mode != CancelInvalidate {
		return Cancellation{}, errors.New("unknown speech cancellation mode")
	}
	if strings.TrimSpace(reason) == "" {
		return Cancellation{}, errors.New("speech cancellation requires a reason")
	}
	cancellation := Cancellation{
		Mode: mode, PlayedSamples: plan.played,
		DiscardedQueuedSamples:   plan.queued - plan.played,
		DiscardedPreparedSamples: plan.prepared - plan.queued,
		RepairRequired:           mode == CancelInvalidate && plan.played > 0,
	}
	plan.prepared = plan.played
	plan.queued = plan.played
	plan.finalPrepared = plan.finalPrepared && cancellation.DiscardedQueuedSamples == 0 && cancellation.DiscardedPreparedSamples == 0
	plan.chunks = truncateChunks(plan.chunks, plan.played)
	plan.state = PlanCancelled
	plan.pendingRepair = cancellation.RepairRequired
	plan.cancellation = &cancellation
	plan.record(atNS, Operation{Type: "speech.cancelled", ThroughSample: plan.played, Mode: mode, Reason: reason})
	return cancellation, nil
}

func (plan *Plan) RecordRepair(text string, atNS uint64) error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkTime(atNS); err != nil {
		return err
	}
	if !plan.pendingRepair {
		return errors.New("speech plan has no pending repair")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("repair text must not be empty")
	}
	plan.repairs = append(plan.repairs, Repair{
		AtNS: atNS, CandidateID: plan.config.CandidateID, PlayedSamples: plan.played, Text: text,
	})
	plan.pendingRepair = false
	plan.record(atNS, Operation{Type: "speech.repair_recorded", ThroughSample: plan.played})
	return nil
}

func (plan *Plan) Complete(atNS uint64) error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if err := plan.checkActiveAndTime(atNS); err != nil {
		return err
	}
	if !plan.finalPrepared || plan.played != plan.prepared || plan.queued != plan.prepared {
		return errors.New("speech plan cannot complete before all final audio is played")
	}
	plan.state = PlanCompleted
	plan.record(atNS, Operation{Type: "speech.completed", ThroughSample: plan.played})
	return nil
}

func (plan *Plan) ValidateClosed() error {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.state == PlanActive {
		return errors.New("speech plan is still active")
	}
	if plan.pendingRepair {
		return errors.New("played invalidated audio requires an explicit repair")
	}
	if plan.played > plan.queued || plan.queued > plan.prepared {
		return errors.New("speech horizons are inconsistent")
	}
	if plan.state == PlanCompleted && (!plan.finalPrepared || plan.played != plan.prepared) {
		return errors.New("completed speech plan is missing played final audio")
	}
	if plan.state == PlanCancelled && (plan.prepared != plan.played || plan.queued != plan.played) {
		return errors.New("cancelled speech plan retained unplayed audio")
	}
	return nil
}

func (plan *Plan) Snapshot() Snapshot {
	plan.mu.Lock()
	defer plan.mu.Unlock()
	var cancellation *Cancellation
	if plan.cancellation != nil {
		copy := *plan.cancellation
		cancellation = &copy
	}
	history := []PlayedSpan{}
	if plan.played > 0 {
		history = append(history, PlayedSpan{
			CandidateID: plan.config.CandidateID, Text: plan.config.Text, PlayedSamples: plan.played,
		})
	}
	return Snapshot{
		PlanID: plan.config.PlanID, CandidateID: plan.config.CandidateID, Text: plan.config.Text,
		SampleRateHz: plan.config.SampleRateHz, State: plan.state,
		PreparedSamples: plan.prepared, QueuedSamples: plan.queued, PlayedSamples: plan.played,
		FinalPrepared: plan.finalPrepared, PendingRepair: plan.pendingRepair, Cancellation: cancellation,
		PlayedHistory: history, Repairs: append([]Repair{}, plan.repairs...),
		Operations: append([]Operation{}, plan.operations...),
	}
}

func (plan *Plan) checkActiveAndTime(atNS uint64) error {
	if plan.state != PlanActive {
		return fmt.Errorf("speech plan is %s", plan.state)
	}
	return plan.checkTime(atNS)
}

func (plan *Plan) checkTime(atNS uint64) error {
	if plan.hasTime && atNS < plan.lastNS {
		return errors.New("speech plan time moved backwards")
	}
	return nil
}

func (plan *Plan) record(atNS uint64, operation Operation) {
	plan.hasTime = true
	plan.lastNS = atNS
	operation.AtNS = atNS
	plan.operations = append(plan.operations, operation)
}

func (plan *Plan) isChunkBoundary(sample uint64) bool {
	for _, chunk := range plan.chunks {
		end := chunk.SampleOffset + uint64(len(chunk.PCM16LE)/2)
		if end == sample {
			return true
		}
		if end > sample {
			return false
		}
	}
	return false
}

func truncateChunks(chunks []engine.SpeechChunk, sample uint64) []engine.SpeechChunk {
	result := make([]engine.SpeechChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.SampleOffset >= sample {
			break
		}
		end := chunk.SampleOffset + uint64(len(chunk.PCM16LE)/2)
		if end > sample {
			byteCount := (sample - chunk.SampleOffset) * 2
			chunk.PCM16LE = slices.Clone(chunk.PCM16LE[:byteCount])
			chunk.Final = false
		} else {
			chunk.PCM16LE = slices.Clone(chunk.PCM16LE)
		}
		result = append(result, chunk)
	}
	return result
}
