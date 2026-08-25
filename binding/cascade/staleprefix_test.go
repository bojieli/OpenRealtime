package cascade_test

import (
	"context"
	"errors"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// stalingProvider reports the failure a runner reports when the trajectory
// moved under a continuation while it was being computed.
type stalingProvider struct{ descriptor continuation.Descriptor }

func (provider *stalingProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *stalingProvider) Continue(
	context.Context, continuation.Request, continuation.Emit,
) (continuation.Completion, error) {
	return continuation.Completion{}, errors.Join(
		continuation.ErrStalePrefix,
		errors.New("observation superseded output derived from version 0, current 1"))
}

// A safe point that refuses output answering a sentence the speaker has since
// moved past is the rule working, not a fault. It reached the client as a
// session error instead, which ends the call - and in a scenario where a
// recorded menu talks continuously, something arrives during almost every
// turn.
func TestBeingOvertakenIsNotASessionFailure(t *testing.T) {
	fast := &stalingProvider{descriptor: continuation.Descriptor{
		Provider: "staling", Model: "staling", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}}
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "what is my balance"}, nil
		},
		Fast: fast, Slow: newSlow(),
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(runtime.Trajectory().Items) > 0 }, "nothing was ever heard")
	time.Sleep(200 * time.Millisecond)

	sink.mu.Lock()
	failures := append([]binding.ErrorEvent(nil), sink.failures...)
	sink.mu.Unlock()
	for _, failure := range failures {
		t.Fatalf("being overtaken was reported to the client: %s %s", failure.Code, failure.Message)
	}
}
