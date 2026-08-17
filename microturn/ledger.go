package microturn

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/engine"
)

type RevisionLedger struct {
	revisions []engine.PerceptionRevision
	byID      map[uint64]engine.PerceptionRevision
	finalized bool
}

func NewRevisionLedger() *RevisionLedger {
	return &RevisionLedger{byID: make(map[uint64]engine.PerceptionRevision)}
}

func (ledger *RevisionLedger) Append(revision engine.PerceptionRevision) error {
	if ledger.finalized {
		return errors.New("cannot append a perception revision after finalization")
	}
	if revision.RevisionID == 0 {
		return errors.New("perception revision ID must be positive")
	}
	if strings.TrimSpace(revision.StableText) == "" && strings.TrimSpace(revision.UnstableText) == "" {
		return errors.New("perception revision must contain stable or unstable text")
	}
	if len(ledger.revisions) > 0 {
		previous := ledger.revisions[len(ledger.revisions)-1]
		if revision.RevisionID <= previous.RevisionID {
			return errors.New("perception revision IDs must increase")
		}
		if revision.SourceSample < previous.SourceSample {
			return errors.New("perception source samples must not move backwards")
		}
		if !strings.HasPrefix(revision.StableText, previous.StableText) {
			return errors.New("a declared stable prefix cannot be revised")
		}
	}
	ledger.revisions = append(ledger.revisions, revision)
	ledger.byID[revision.RevisionID] = revision
	ledger.finalized = revision.Final
	return nil
}

func (ledger *RevisionLedger) Get(id uint64) (engine.PerceptionRevision, bool) {
	revision, ok := ledger.byID[id]
	return revision, ok
}

func (ledger *RevisionLedger) Latest() (engine.PerceptionRevision, bool) {
	if len(ledger.revisions) == 0 {
		return engine.PerceptionRevision{}, false
	}
	return ledger.revisions[len(ledger.revisions)-1], true
}

type CandidateState string

const (
	CandidatePrepared   CandidateState = "prepared"
	CandidateSuperseded CandidateState = "superseded"
	CandidateCancelled  CandidateState = "cancelled"
)

type CandidateRecord struct {
	Candidate      engine.ResponseCandidate `json:"candidate"`
	State          CandidateState           `json:"state"`
	PreparedNS     uint64                   `json:"prepared_ns"`
	ResolvedNS     uint64                   `json:"resolved_ns,omitempty"`
	SupersededByID string                   `json:"superseded_by_id,omitempty"`
	Reason         string                   `json:"reason,omitempty"`
}

type CandidateLedger struct {
	revisions *RevisionLedger
	records   map[string]CandidateRecord
	activeID  string
}

func NewCandidateLedger(revisions *RevisionLedger) *CandidateLedger {
	return &CandidateLedger{revisions: revisions, records: make(map[string]CandidateRecord)}
}

func (ledger *CandidateLedger) Prepare(candidate engine.ResponseCandidate, atNS uint64) error {
	if ledger.activeID != "" {
		return fmt.Errorf("candidate %q is still active; supersede or cancel it first", ledger.activeID)
	}
	if err := ledger.validateNew(candidate); err != nil {
		return err
	}
	ledger.records[candidate.CandidateID] = CandidateRecord{
		Candidate: candidate, State: CandidatePrepared, PreparedNS: atNS,
	}
	ledger.activeID = candidate.CandidateID
	return nil
}

func (ledger *CandidateLedger) Supersede(
	activeID string,
	replacement engine.ResponseCandidate,
	atNS uint64,
	reason string,
) error {
	active, ok := ledger.records[activeID]
	if !ok || active.State != CandidatePrepared || ledger.activeID != activeID {
		return fmt.Errorf("candidate %q is not the active prepared candidate", activeID)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("candidate supersession requires a reason")
	}
	if atNS < active.PreparedNS {
		return errors.New("candidate supersession cannot move backwards in time")
	}
	if err := ledger.validateNew(replacement); err != nil {
		return err
	}
	if replacement.SourceRevision <= active.Candidate.SourceRevision {
		return errors.New("replacement candidate must use a newer perception revision")
	}
	active.State = CandidateSuperseded
	active.ResolvedNS = atNS
	active.SupersededByID = replacement.CandidateID
	active.Reason = reason
	ledger.records[activeID] = active
	ledger.records[replacement.CandidateID] = CandidateRecord{
		Candidate: replacement, State: CandidatePrepared, PreparedNS: atNS,
	}
	ledger.activeID = replacement.CandidateID
	return nil
}

func (ledger *CandidateLedger) Cancel(activeID string, atNS uint64, reason string) error {
	record, ok := ledger.records[activeID]
	if !ok || record.State != CandidatePrepared || ledger.activeID != activeID {
		return fmt.Errorf("candidate %q is not the active prepared candidate", activeID)
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("candidate cancellation requires a reason")
	}
	if atNS < record.PreparedNS {
		return errors.New("candidate cancellation cannot move backwards in time")
	}
	record.State = CandidateCancelled
	record.ResolvedNS = atNS
	record.Reason = reason
	ledger.records[activeID] = record
	ledger.activeID = ""
	return nil
}

func (ledger *CandidateLedger) Active() (CandidateRecord, bool) {
	if ledger.activeID == "" {
		return CandidateRecord{}, false
	}
	return ledger.records[ledger.activeID], true
}

func (ledger *CandidateLedger) Get(id string) (CandidateRecord, bool) {
	record, ok := ledger.records[id]
	return record, ok
}

func (ledger *CandidateLedger) validateNew(candidate engine.ResponseCandidate) error {
	if ledger.revisions == nil {
		return errors.New("candidate ledger requires a revision ledger")
	}
	if candidate.CandidateID == "" || strings.TrimSpace(candidate.Text) == "" || strings.TrimSpace(candidate.ValiditySummary) == "" {
		return errors.New("candidate ID, text, and validity summary must not be empty")
	}
	if _, exists := ledger.records[candidate.CandidateID]; exists {
		return fmt.Errorf("duplicate candidate ID %q", candidate.CandidateID)
	}
	if _, exists := ledger.revisions.Get(candidate.SourceRevision); !exists {
		return fmt.Errorf("candidate references unknown perception revision %d", candidate.SourceRevision)
	}
	return nil
}
