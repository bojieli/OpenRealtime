// Package action turns decisions into effects on the world.
//
// It is perception's dual. Perception turns a continuous external stream into
// discrete commitments by gating - discarding what does not matter. Action
// turns discrete commitments back into a continuous external stream by pacing
// - holding back what is not yet safe to emit. Same boundary, opposite
// directions.
//
// There is one commit boundary and it is the same for everything. Speech,
// assistant text, tool calls, and clicks are all irreversible once emitted: a
// spoken sentence cannot be unsaid, and a repair is damage limitation rather
// than reversal. So this package does not classify outputs by how recoverable
// they are - it holds a single line between decided and emitted, identically
// for all of them. Nothing crosses without authority, and everything that has
// not yet crossed can still be cancelled.
//
// That is simpler than a taxonomy of consequence and it is also stronger,
// because the guarantee does not depend on having classified a new action type
// correctly. What remains genuinely per-action is a confirmation requirement,
// and that is a policy a developer declares rather than one the runtime
// infers.
package action

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// Kind names what sort of effect a commitment will have. It exists for
// reporting and for confirmation policy - never to decide how carefully the
// commit boundary is held, which is identical for all of them.
type Kind string

const (
	KindSpeech   Kind = "speech"
	KindText     Kind = "text"
	KindToolCall Kind = "tool_call"
	// KindComputerAction is a computer-use action. It rides the same dispatch
	// path as any other tool call and gets no special handling here.
	KindComputerAction Kind = "computer_action"
)

// State is how far a commitment has crossed the boundary.
//
// The vocabulary matches the trajectory's assistant visibility deliberately:
// the ledger and the log are describing the same lifecycle, and two
// vocabularies for one lifecycle is how they drift apart.
type State string

const (
	// StatePrepared: decided, nothing has left the process.
	StatePrepared State = "prepared"
	// StateQueued: accepted for emission, still fully cancellable.
	StateQueued State = "queued"
	// StateEmitting: something has reached the world. From here nothing is
	// reversible; cancelling stops the remainder and nothing more.
	StateEmitting State = "emitting"
	// StatePlayed: emission completed.
	StatePlayed State = "played"
	// StateCancelled: stopped before anything reached the world.
	StateCancelled State = "cancelled"
)

// Terminal reports whether a state can still change.
func (state State) Terminal() bool { return state == StatePlayed || state == StateCancelled }

// Crossed reports whether any part of this commitment reached the world. It is
// the only distinction that matters for reversibility.
func (state State) Crossed() bool { return state == StateEmitting || state == StatePlayed }

// Confirm is a developer's declaration about consequence, carried on a tool
// definition. It is never inferred from the action's name or arguments.
type Confirm string

const (
	ConfirmNever  Confirm = "never"
	ConfirmPolicy Confirm = "policy"
	ConfirmAlways Confirm = "always"
)

// ParseConfirm validates a declared confirmation requirement. An unset value
// means never, which matches how an ordinary tool behaves today.
func ParseConfirm(value string) (Confirm, error) {
	switch confirm := Confirm(strings.ToLower(strings.TrimSpace(value))); confirm {
	case "", ConfirmNever:
		return ConfirmNever, nil
	case ConfirmPolicy, ConfirmAlways:
		return confirm, nil
	default:
		return "", fmt.Errorf("confirmation requirement must be never, policy, or always, got %q", value)
	}
}

// Commitment is one decided output on its way to the world.
type Commitment struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// AssistantItemIDs are the trajectory items this commitment carries, so a
	// cancellation can record the matching visibility transitions.
	AssistantItemIDs []string `json:"assistant_item_ids,omitempty"`
	// CallID is the idempotency key for a tool or computer-use action. The
	// same call ID may be dispatched at most once, so recovery can retry a
	// committed call without duplicating its external effect.
	CallID string `json:"call_id,omitempty"`
	// SourceRevision is the perception revision this commitment answers.
	SourceRevision uint64 `json:"source_revision,omitempty"`
	// Phase is the cognition phase that produced it.
	Phase trajectory.Phase `json:"phase,omitempty"`
	// SpeechAuthority is the producing provider's authority, read from the
	// committed log. Silent content must never be queued for speech.
	SpeechAuthority string `json:"speech_authority,omitempty"`
	// Confirm is the declared requirement for this action.
	Confirm Confirm `json:"confirm,omitempty"`

	State State `json:"state"`
	// PlayedMS is how much of a speech commitment the user actually heard.
	PlayedMS uint64 `json:"played_ms,omitempty"`
	// CancelReason records why a commitment stopped.
	CancelReason string `json:"cancel_reason,omitempty"`
}

// Obligation is an outstanding repair: content that reached the world and was
// later invalidated. It is what the ledger owes the conversation.
type Obligation struct {
	CommitmentID          string   `json:"commitment_id"`
	AssistantItemIDs      []string `json:"assistant_item_ids"`
	PlayedMS              uint64   `json:"played_ms"`
	InvalidatedByRevision uint64   `json:"invalidated_by_revision"`
	Resolved              bool     `json:"resolved"`
}

var (
	// ErrUnknownCommitment names a commitment the ledger never saw.
	ErrUnknownCommitment = errors.New("unknown commitment")
	// ErrAlreadyCrossed means the caller tried to reverse something that had
	// already reached the world.
	ErrAlreadyCrossed = errors.New("commitment has already crossed the boundary")
	// ErrInvalidTransition names a lifecycle step that does not exist.
	ErrInvalidTransition = errors.New("invalid commitment transition")
	// ErrDuplicateCall means a call ID was dispatched twice.
	ErrDuplicateCall = errors.New("duplicate call identity")
)

// Ledger is the single record of what has been decided, what has crossed the
// boundary, and what repair is owed. Every output kind goes through it.
type Ledger struct {
	mu          sync.Mutex
	commitments map[string]*Commitment
	order       []string
	callIDs     map[string]string
	obligations []Obligation
}

// NewLedger creates an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{commitments: make(map[string]*Commitment), callIDs: make(map[string]string)}
}

// Prepare records a decided output. It has not left the process.
func (ledger *Ledger) Prepare(commitment Commitment) error {
	if strings.TrimSpace(commitment.ID) == "" {
		return errors.New("commitment requires an ID")
	}
	switch commitment.Kind {
	case KindSpeech, KindText, KindToolCall, KindComputerAction:
	default:
		return fmt.Errorf("unknown commitment kind %q", commitment.Kind)
	}
	confirm, err := ParseConfirm(string(commitment.Confirm))
	if err != nil {
		return err
	}
	commitment.Confirm = confirm
	commitment.State = StatePrepared
	commitment.AssistantItemIDs = slices.Clone(commitment.AssistantItemIDs)

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if _, exists := ledger.commitments[commitment.ID]; exists {
		return fmt.Errorf("duplicate commitment %q", commitment.ID)
	}
	if commitment.CallID != "" {
		if previous, exists := ledger.callIDs[commitment.CallID]; exists {
			return fmt.Errorf("%w: call %q already committed as %q", ErrDuplicateCall, commitment.CallID, previous)
		}
		ledger.callIDs[commitment.CallID] = commitment.ID
	}
	stored := commitment
	ledger.commitments[commitment.ID] = &stored
	ledger.order = append(ledger.order, commitment.ID)
	return nil
}

// Queue accepts a commitment for emission. It is still fully cancellable.
func (ledger *Ledger) Queue(id string) error {
	return ledger.transition(id, StateQueued, func(current State) bool {
		return current == StatePrepared
	})
}

// Emit records that something reached the world. This is the irreversible
// step, and it is deliberately separate from Queue: the code that queues a
// decision and the code that puts bytes on the wire are not the same code, and
// only the second one knows.
func (ledger *Ledger) Emit(id string) error {
	return ledger.transition(id, StateEmitting, func(current State) bool {
		return current == StatePrepared || current == StateQueued || current == StateEmitting
	})
}

// Complete records that emission finished. playedMS is how much of a speech
// commitment was actually heard.
func (ledger *Ledger) Complete(id string, playedMS uint64) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownCommitment, id)
	}
	if commitment.State.Terminal() {
		return fmt.Errorf("%w: %s is already %s", ErrInvalidTransition, id, commitment.State)
	}
	commitment.State = StatePlayed
	commitment.PlayedMS = playedMS
	return nil
}

// Cancel stops a commitment. It reports whether anything had already crossed
// the boundary, which is the caller's signal that a repair may be owed rather
// than an error - cancelling something already heard is a normal race, not a
// programming mistake.
func (ledger *Ledger) Cancel(id, reason string) (crossed bool, err error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists {
		return false, fmt.Errorf("%w: %s", ErrUnknownCommitment, id)
	}
	if commitment.State == StateCancelled {
		return false, nil
	}
	if commitment.State == StatePlayed {
		return true, nil
	}
	crossed = commitment.State.Crossed()
	commitment.CancelReason = reason
	if crossed {
		// What was heard stays heard. The commitment ends where it ended.
		commitment.State = StatePlayed
		return true, nil
	}
	commitment.State = StateCancelled
	return false, nil
}

// Invalidate records that later evidence invalidated content that had already
// reached the world, and returns the obligation it created. Content that never
// crossed the boundary creates no obligation: there is nothing to repair.
func (ledger *Ledger) Invalidate(id string, byRevision uint64) (Obligation, bool) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists || !commitment.State.Crossed() {
		return Obligation{}, false
	}
	for _, existing := range ledger.obligations {
		if existing.CommitmentID == id && !existing.Resolved {
			return Obligation{}, false
		}
	}
	obligation := Obligation{
		CommitmentID: id, AssistantItemIDs: slices.Clone(commitment.AssistantItemIDs),
		PlayedMS: commitment.PlayedMS, InvalidatedByRevision: byRevision,
	}
	ledger.obligations = append(ledger.obligations, obligation)
	return obligation, true
}

// ResolveObligation marks a repair as discharged.
func (ledger *Ledger) ResolveObligation(commitmentID string) bool {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for index := range ledger.obligations {
		if ledger.obligations[index].CommitmentID == commitmentID && !ledger.obligations[index].Resolved {
			ledger.obligations[index].Resolved = true
			return true
		}
	}
	return false
}

// Obligations returns outstanding repairs in the order they were created.
func (ledger *Ledger) Obligations() []Obligation {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var outstanding []Obligation
	for _, obligation := range ledger.obligations {
		if !obligation.Resolved {
			outstanding = append(outstanding, obligation)
		}
	}
	return outstanding
}

// Lookup returns one commitment.
func (ledger *Ledger) Lookup(id string) (Commitment, bool) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists {
		return Commitment{}, false
	}
	return *commitment, true
}

// Crossed returns every commitment that reached the world and matches the
// supplied predicate.
//
// It is Cancellable's mirror, and the pair is the whole vocabulary the commit
// boundary offers: what can still be stopped, and what can only be repaired.
// Supersession needs both - it stops what nobody heard and owes a correction
// for what somebody did.
func (ledger *Ledger) Crossed(match func(Commitment) bool) []Commitment {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var result []Commitment
	for _, id := range ledger.order {
		commitment := ledger.commitments[id]
		if !commitment.State.Crossed() {
			continue
		}
		if match != nil && !match(*commitment) {
			continue
		}
		result = append(result, *commitment)
	}
	return result
}

// Truncated records what the client says was actually heard.
//
// The server knows what it sent and when; only the client knows where playback
// stopped. That makes the client authoritative on the one number a repair
// decision turns on, and a repair raised later must use the honest figure
// rather than the server's optimistic one.
func (ledger *Ledger) Truncated(id string, playedMS uint64) bool {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists || !commitment.State.Crossed() {
		return false
	}
	commitment.PlayedMS = playedMS
	for index := range ledger.obligations {
		if ledger.obligations[index].CommitmentID == id && !ledger.obligations[index].Resolved {
			ledger.obligations[index].PlayedMS = playedMS
		}
	}
	return true
}

// Cancellable returns every commitment that has not yet crossed the boundary
// and that matches the supplied predicate. It is how barge-in and supersession
// find what they may still stop.
func (ledger *Ledger) Cancellable(match func(Commitment) bool) []Commitment {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	var result []Commitment
	for _, id := range ledger.order {
		commitment := ledger.commitments[id]
		if commitment.State.Terminal() || commitment.State.Crossed() {
			continue
		}
		if match != nil && !match(*commitment) {
			continue
		}
		result = append(result, *commitment)
	}
	return result
}

// Outstanding returns every commitment that has not reached a terminal state.
func (ledger *Ledger) Outstanding() []Commitment {
	return ledger.Cancellable(nil)
}

// Snapshot is the whole ledger, for the health endpoint and for evidence.
func (ledger *Ledger) Snapshot() []Commitment {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	result := make([]Commitment, 0, len(ledger.order))
	for _, id := range ledger.order {
		result = append(result, *ledger.commitments[id])
	}
	return result
}

func (ledger *Ledger) transition(id string, next State, allowed func(State) bool) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	commitment, exists := ledger.commitments[id]
	if !exists {
		return fmt.Errorf("%w: %s", ErrUnknownCommitment, id)
	}
	if commitment.State == next {
		return nil
	}
	if !allowed(commitment.State) {
		if commitment.State.Crossed() {
			return fmt.Errorf("%w: %s is %s", ErrAlreadyCrossed, id, commitment.State)
		}
		return fmt.Errorf("%w: %s -> %s for %s", ErrInvalidTransition, commitment.State, next, id)
	}
	commitment.State = next
	return nil
}
