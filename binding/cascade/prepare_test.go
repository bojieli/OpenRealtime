package cascade_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// --- preparation ------------------------------------------------------------

// answeringProvider replies to what it was actually asked rather than to a
// script position, so a test cannot pass by counting calls in the right order.
type answeringProvider struct {
	descriptor continuation.Descriptor
	reply      func(heard string) string

	mu    sync.Mutex
	calls int
	heard []string
}

func (provider *answeringProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *answeringProvider) Continue(
	_ context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	heard := ""
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindObservation && item.Producer.Phase == trajectory.PhaseUser {
			heard = item.Content
		}
	}
	provider.mu.Lock()
	provider.calls++
	provider.heard = append(provider.heard, heard)
	provider.mu.Unlock()
	if err := emit(continuation.Event{
		Kind: continuation.EventAssistantDelta, Text: provider.reply(heard),
	}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func (provider *answeringProvider) invocations() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls
}

func answeringFast(reply func(string) string) *answeringProvider {
	return &answeringProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
		},
		reply: reply,
	}
}

func answeringSlow(reply func(string) string) *answeringProvider {
	return &answeringProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
			SpeechAuthority: continuation.SpeechAuthoritySilent,
		},
		reply: reply,
	}
}

// Preparation is the only policy that acts on evidence it knows may be wrong,
// so the test that matters is that it saves a call when it was right.
func TestPreparationIsAdoptedWhenTheEndpointConfirmsIt(t *testing.T) {
	policies := interaction.Defaults()
	policies.Preparation = interaction.NewContinuousPreparation(0)
	fast := answeringFast(func(heard string) string { return "Answering: " + heard })
	slow := answeringSlow(func(heard string) string { return "Recorded: " + heard })
	runtime, sink := startSession(t, cascade.Config{
		// The partial and the final say the same thing, which is the case
		// preparation exists for: the last partial was already right.
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twenty", final: "transfer twenty"}, nil
		},
		Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})

	pushAudio(t, runtime, tone(2400, 8000), 2)
	// Give the speculation time to finish before the endpoint asks for it.
	waitFor(t, func() bool { return fast.invocations() > 0 }, "nothing was ever prepared")
	preparedCalls := fast.invocations()

	pushAudio(t, runtime, silence(2400), 8)
	waitFor(t, func() bool { return len(sink.spokenTexts()) > 0 }, "the turn was never answered")

	// One fast call before the endpoint prepared the answer; the endpoint adds
	// only the voicing step, never a second answer.
	if got := fast.invocations(); got > preparedCalls+1 {
		t.Fatalf("an adopted preparation must not be regenerated: %d calls before the endpoint, %d after",
			preparedCalls, got)
	}
	spoken := strings.Join(sink.spokenTexts(), " ")
	if !strings.Contains(spoken, "Answering: transfer twenty") {
		t.Fatalf("the adopted answer never reached the world, got %q", spoken)
	}
}

// And the case that makes it safe: a preparation the endpoint contradicts is
// discarded rather than adopted, whatever it cost to produce.
func TestPreparationIsDiscardedWhenTheEndpointContradictsIt(t *testing.T) {
	policies := interaction.Defaults()
	policies.Preparation = interaction.NewContinuousPreparation(0)
	fast := answeringFast(func(heard string) string { return "Answering: " + heard })
	slow := answeringSlow(func(heard string) string { return "Recorded: " + heard })
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twelve", final: "transfer twenty"}, nil
		},
		Fast: fast, Slow: slow, Policies: policies,
	}, binding.Settings{})

	pushAudio(t, runtime, tone(2400, 8000), 2)
	waitFor(t, func() bool { return fast.invocations() > 0 }, "nothing was ever prepared")
	pushAudio(t, runtime, silence(2400), 8)
	waitFor(t, func() bool { return len(sink.spokenTexts()) > 0 }, "the turn was never answered")

	for _, spoken := range sink.spokenTexts() {
		if strings.Contains(spoken, "twelve") {
			t.Fatalf("an answer to a sentence the user did not finish saying was adopted: %q", spoken)
		}
	}
	// The provisional observation the speculation was shown must not have
	// reached the canonical log either. Only the endpoint's own words are the
	// record of what the user said.
	for _, item := range runtime.Trajectory().Items {
		if item.Kind == trajectory.KindObservation && strings.Contains(item.Content, "twelve") {
			t.Fatalf("a provisional observation reached the canonical log: %q", item.Content)
		}
	}
}
