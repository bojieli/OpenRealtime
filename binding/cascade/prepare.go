package cascade

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/admission"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Preparation is the only policy that acts on evidence it knows may be wrong.
//
// It starts a continuation while the user is still speaking, against what
// perception has heard so far. Nothing it produces is committed, spoken, or
// dispatched: the work is private until the endpoint says the same thing the
// preparation was generated from, and is discarded otherwise. That is what
// makes it a latency policy - being wrong costs tokens and nothing else.
//
// Adoption is deliberately strict. A preparation is adopted only when the
// canonical observation matches the provisional text it answered, because an
// answer to a sentence the user did not finish saying is not a faster answer,
// it is a wrong one.

type preparation struct {
	handle *continuation.Prepared
	phase  trajectory.Phase
}

type preparations struct {
	mu      sync.Mutex
	byPhase map[trajectory.Phase]preparation
	cancel  context.CancelFunc
	running bool
}

func newPreparations() *preparations {
	return &preparations{byPhase: make(map[trajectory.Phase]preparation, 2)}
}

// prepare consults the preparation policy and speculates if it says to.
//
// It is latest-wins: a revision that changes what the user appears to be
// saying makes any preparation in flight answer the wrong sentence, so that
// one is cancelled rather than raced against. One preparation per revision,
// never one per frame.
func (runtime *runtime) prepare(ctx context.Context, decision interaction.Context) {
	outcome := runtime.policies.Preparation.Prepare(decision)
	if !outcome.Start || len(outcome.Phases) == 0 || decision.Revision.Empty() {
		return
	}
	text := strings.TrimSpace(decision.Revision.Text())
	if text == "" {
		return
	}
	phases := append([]trajectory.Phase(nil), outcome.Phases...)

	runtime.prepared.mu.Lock()
	if runtime.prepared.cancel != nil {
		runtime.prepared.cancel()
	}
	speculative, cancel := context.WithCancel(ctx)
	runtime.prepared.cancel = cancel
	runtime.prepared.byPhase = make(map[trajectory.Phase]preparation, len(phases))
	runtime.prepared.running = true
	runtime.prepared.mu.Unlock()

	provisional := trajectory.Item{
		ID: idFor("provisional", runtime.sequence.Add(1)), Kind: trajectory.KindObservation,
		MonotonicNS: decision.NowNS, SourceRevision: decision.Revision.ID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: text,
	}
	standing, interjecting, heard := runtime.cognitionExtras()
	request := cognition.Request{
		SourceRevision: decision.Revision.ID, Standing: standing, Interjecting: interjecting, Heard: heard,
		AllowFastTools: true,
	}

	go func() {
		defer cancel()
		var wait sync.WaitGroup
		for _, phase := range phases {
			wait.Add(1)
			go func(phase trajectory.Phase) {
				defer wait.Done()
				handle, err := runtime.speculate(speculative, phase, request, provisional)
				if err != nil || handle == nil {
					// A speculation that failed is a speculation that did not
					// happen. The endpoint runs the continuation itself, which
					// is what it would have done anyway.
					return
				}
				runtime.prepared.mu.Lock()
				if runtime.prepared.byPhase != nil {
					runtime.prepared.byPhase[phase] = preparation{handle: handle, phase: phase}
				}
				runtime.prepared.mu.Unlock()
			}(phase)
		}
		wait.Wait()
		runtime.prepared.mu.Lock()
		runtime.prepared.running = false
		runtime.prepared.mu.Unlock()
	}()
}

func (runtime *runtime) speculate(
	ctx context.Context, phase trajectory.Phase,
	request cognition.Request, provisional trajectory.Item,
) (*continuation.Prepared, error) {
	// Speculation competes for the same compute as the turn it is trying to
	// make faster, so it is admitted below the foreground and is preemptible.
	// A speculation that delayed the voice would have spent exactly the
	// latency it exists to save.
	if runtime.config.Governor != nil {
		lease, err := runtime.config.Governor.Acquire(ctx, admission.Request{
			Class: admission.ClassSpeculative, Cost: 1, Preemptible: true,
			Deadline: time.Now().Add(preparationDeadline), Label: "preparation:" + string(phase),
		})
		if err != nil {
			return nil, err
		}
		defer lease.Release()
		ctx = lease.Context()
	}
	if phase == trajectory.PhaseSlow {
		return runtime.engine.PrepareSlow(ctx, request, provisional)
	}
	return runtime.engine.PrepareFast(ctx, request, provisional)
}

// preparationDeadline bounds how long speculative work waits for capacity.
// A preparation that has not started by the time the user stops talking is a
// preparation the endpoint will never adopt.
const preparationDeadline = 2 * time.Second

// adopt uses a preparation whose provisional text the endpoint confirmed.
//
// The comparison is against the canonical observation rather than against the
// revision number, because the number says which revision was answered and the
// text says whether answering it was right. A recogniser that revised "twelve"
// to "twenty" produces a new revision with the same shape and a different
// meaning, and adopting on the shape would put an answer to the wrong sentence
// into the log with full authority.
func (runtime *runtime) adopt(phase trajectory.Phase, canonical string) (continuation.RunResult, bool) {
	canonical = strings.TrimSpace(canonical)
	if canonical == "" {
		return continuation.RunResult{}, false
	}
	runtime.prepared.mu.Lock()
	entry, exists := runtime.prepared.byPhase[phase]
	if exists {
		delete(runtime.prepared.byPhase, phase)
	}
	runtime.prepared.mu.Unlock()
	if !exists || !entry.handle.Ready() {
		return continuation.RunResult{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(entry.handle.ProvisionalText()), canonical) {
		return continuation.RunResult{}, false
	}
	result, err := runtime.engine.Adopt(entry.handle)
	if err != nil {
		// Adoption is best-effort by design. The safe point runs the
		// continuation itself and the turn is a little slower, which is
		// exactly the cost preparation was trying to avoid and never the cost
		// of being wrong.
		return continuation.RunResult{}, false
	}
	return result, true
}

// discardPreparations drops speculative work at a turn boundary.
func (runtime *runtime) discardPreparations() {
	runtime.prepared.mu.Lock()
	defer runtime.prepared.mu.Unlock()
	if runtime.prepared.cancel != nil {
		runtime.prepared.cancel()
		runtime.prepared.cancel = nil
	}
	runtime.prepared.byPhase = make(map[trajectory.Phase]preparation, 2)
}

// canonicalText is what the log says the user said at one revision.
func canonicalText(snapshot trajectory.Snapshot, revision uint64) string {
	text := ""
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindObservation && item.SourceRevision == revision {
			text = item.Content
		}
	}
	return text
}
