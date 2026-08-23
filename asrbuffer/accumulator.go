package asrbuffer

import "sync"

// Accumulator folds per-utterance counters into one process-level total.
//
// The recogniser is a factory and a Buffer owns exactly one utterance, so the
// counters a Buffer carries describe a few seconds of one session and then go
// out of scope with it. That is the right lifetime for the buffer and the
// wrong one for an operator, who is watching for a recogniser that is getting
// slower across every utterance rather than within one. This owns the number
// that survives the boundary.
//
// A tracked buffer folds itself in exactly once, when it closes. Until then it
// is read in place rather than waited for, because an utterance still in
// flight is the one most likely to be the slow one, and a signal that only
// appears after the endpoint would go quiet during precisely the stall it
// exists to report.
type Accumulator struct {
	mu         sync.Mutex
	folded     ProviderMetrics
	utterances uint64
	live       map[*Buffer]struct{}
}

// NewAccumulator returns an accumulator with no utterances folded in.
func NewAccumulator() *Accumulator {
	return &Accumulator{live: make(map[*Buffer]struct{})}
}

// New creates a Buffer whose counters fold into this accumulator when it
// closes.
//
// It is deliberately a second constructor rather than a Config field on the
// first. A benchmark builds a Buffer in order to read that one buffer's
// numbers, and enrolling it in a process-level total would leave the harness
// measuring a figure that other sessions also move. Deployment opts in; a
// measurement does not.
//
// A nil accumulator returns an ordinary untracked buffer, so a caller holding
// one that was never configured needs no branch of its own.
func (accumulator *Accumulator) New(config Config) (*Buffer, error) {
	buffer, err := New(config)
	if err != nil {
		return nil, err
	}
	if accumulator == nil {
		return buffer, nil
	}
	buffer.accumulator = accumulator
	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	accumulator.live[buffer] = struct{}{}
	return buffer, nil
}

// fold moves one closed buffer's final counters into the totals.
//
// Membership in live is what makes this exactly-once: a buffer is either
// pending in live or already folded, never both and never neither, so a second
// Close cannot count an utterance twice.
func (accumulator *Accumulator) fold(buffer *Buffer, metrics ProviderMetrics) {
	accumulator.mu.Lock()
	defer accumulator.mu.Unlock()
	if _, tracked := accumulator.live[buffer]; !tracked {
		return
	}
	delete(accumulator.live, buffer)
	accumulator.folded.add(metrics)
	accumulator.utterances++
}

// RuntimeSnapshot is a process-level view of the recogniser boundary. It
// contains counters and timing only: no transcript, no audio, no identity.
type RuntimeSnapshot struct {
	ProviderMetrics
	// Utterances is how many buffers have closed and folded in.
	Utterances uint64
	// InFlight is how many are still open and contributing in place.
	InFlight int
}

// Snapshot totals every utterance this process has run: those already folded,
// plus those still open.
func (accumulator *Accumulator) Snapshot() RuntimeSnapshot {
	if accumulator == nil {
		return RuntimeSnapshot{}
	}
	accumulator.mu.Lock()
	total, utterances := accumulator.folded, accumulator.utterances
	live := make([]*Buffer, 0, len(accumulator.live))
	for buffer := range accumulator.live {
		live = append(live, buffer)
	}
	accumulator.mu.Unlock()

	// The accumulator lock is released before any buffer lock is taken. Close
	// holds a buffer's lock and then takes this one, so holding this one while
	// reaching into a buffer would invert the order and the two could meet in
	// the middle. Nothing here holds both at once, so they cannot.
	//
	// Reading a live buffer just after the copy is what makes the total exact
	// rather than approximate: the pair above is consistent, so a buffer that
	// closes in the gap is absent from `total` and read here instead, and one
	// that closed before the copy is in `total` and absent from `live`. Each
	// is counted once either way, and a later read only returns a larger
	// value, so the total never goes backwards.
	for _, buffer := range live {
		total.add(buffer.ProviderRuntimeMetrics())
	}
	return RuntimeSnapshot{ProviderMetrics: total, Utterances: utterances, InFlight: len(live)}
}

// add accumulates one buffer's counters. Elapsed totals sum; a maximum does
// not, and summing two peaks would report a latency no call ever took.
func (metrics *ProviderMetrics) add(other ProviderMetrics) {
	metrics.AdvanceInvocations += other.AdvanceInvocations
	metrics.AdvanceFailures += other.AdvanceFailures
	metrics.AdvanceElapsedNS += other.AdvanceElapsedNS
	metrics.AdvanceMaxElapsedNS = max(metrics.AdvanceMaxElapsedNS, other.AdvanceMaxElapsedNS)
	metrics.FinalizeInvocations += other.FinalizeInvocations
	metrics.FinalizeFailures += other.FinalizeFailures
	metrics.FinalizeElapsedNS += other.FinalizeElapsedNS
	metrics.FinalizeMaxElapsedNS = max(metrics.FinalizeMaxElapsedNS, other.FinalizeMaxElapsedNS)
}

// MeanAdvanceNS is the average time one provider advance took. It is the
// degradation signal: a maximum moves on one bad call and stays there, while
// a mean that is climbing says the recogniser is slowing down generally.
// Zero invocations report zero rather than dividing.
func (metrics ProviderMetrics) MeanAdvanceNS() uint64 {
	if metrics.AdvanceInvocations == 0 {
		return 0
	}
	return metrics.AdvanceElapsedNS / metrics.AdvanceInvocations
}

// MeanFinalizeNS is the same average for finalisation, which is measured
// separately because it is one call per utterance rather than one per chunk.
func (metrics ProviderMetrics) MeanFinalizeNS() uint64 {
	if metrics.FinalizeInvocations == 0 {
		return 0
	}
	return metrics.FinalizeElapsedNS / metrics.FinalizeInvocations
}
