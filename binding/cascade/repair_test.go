package cascade_test

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// revisingASR answers a stable partial first and a different final second, so
// the endpoint genuinely contradicts what the agent already said. It is the
// only situation a repair exists for, and it is reachable only when the
// observation policy admits partials.
type revisingASR struct {
	partial string
	final   string
	pushes  int
}

func (asr *revisingASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "revising-asr", Version: "1", Capabilities: v1.Capabilities{}}
}

func (asr *revisingASR) PushFrame(_ context.Context, _ v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	asr.pushes++
	if asr.pushes > 1 {
		return nil, nil
	}
	return []v1.PerceptionRevision{{RevisionID: 1, StableText: asr.partial}}, nil
}

func (asr *revisingASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 2, StableText: asr.final, Final: true}, nil
}

func pushAudio(t *testing.T, runtime binding.Runtime, payload []byte, blocks int) {
	t.Helper()
	for index := 0; index < blocks; index++ {
		if err := runtime.Audio(context.Background(), perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24_000,
			PCM16LE: payload,
		}); err != nil {
			t.Fatalf("audio block %d: %v", index, err)
		}
	}
}

// partialPolicies is the coherent pair: a policy that admits partials needs a
// deferral policy willing to act on one before the endpoint.
func partialPolicies() interaction.Policies {
	policies := interaction.Defaults()
	policies.Deferral = interaction.NewDuplexDeferral(interaction.DeferralOptions{
		AllowWhileUserSpeaking: true,
	})
	return policies
}

func repairItems(snapshot trajectory.Snapshot, status trajectory.RepairStatus) []trajectory.Item {
	var found []trajectory.Item
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindRepair && item.Repair != nil && item.Repair.Status == status {
			found = append(found, item)
		}
	}
	return found
}

// Speaking from a partial is what makes an agent feel fast. Being wrong about
// it is the price, and audio that reached the user cannot be taken back - so
// the only honest outcome is an obligation the model is told to discharge.
func TestHeardContentInvalidatedByALaterRevisionOwesAnAudibleRepair(t *testing.T) {
	// One turn is fast, slow, then a fast step that voices what slow wrote, so
	// answering the partial and then answering the endpoint takes two of each.
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twelve dollars, right."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twelve dollars it is."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Sorry - twenty, not twelve."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twenty dollars, corrected."}},
	)
	slow := newSlow(
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "The caller asked to transfer twelve dollars.",
		}},
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta,
			Text: "Correction: the amount the caller asked for is twenty dollars, not twelve.",
		}},
	)
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twelve", final: "transfer twenty"}, nil
		},
		Fast: fast, Slow: slow, ObservationPolicy: cascade.ObservationStablePartial,
		Policies: partialPolicies(),
	}, binding.Settings{})

	// Speak until the partial has been answered and the answer has been heard.
	pushAudio(t, runtime, tone(2400, 8000), 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) > 0 }, "the partial was never answered aloud")
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return sink.frames > 0
	}, "no audio ever reached the world")

	// Now the endpoint contradicts it.
	pushAudio(t, runtime, silence(2400), 8)

	waitFor(t, func() bool {
		return len(repairItems(runtime.Trajectory(), trajectory.RepairRequired)) > 0
	}, "heard content contradicted by the endpoint owed no repair")

	required := repairItems(runtime.Trajectory(), trajectory.RepairRequired)[0]
	if required.Repair.PlayedAudioMS == 0 {
		t.Fatal("a required repair must record how much the user actually heard")
	}
	if required.SourceRevision <= 1 {
		t.Fatalf("a repair names the later revision that invalidated it, got %d", required.SourceRevision)
	}

	// The obligation reaches the model as an instruction, not as a hint.
	waitFor(t, func() bool {
		slow.mu.Lock()
		defer slow.mu.Unlock()
		for _, request := range slow.requests {
			if strings.Contains(request.Invocation.Instruction, cognition.RepairInstruction) {
				return true
			}
		}
		return false
	}, "the slow provider was never instructed to correct what was heard")

	// And it is discharged against the correction, rather than left open.
	waitFor(t, func() bool {
		return len(trajectory.PendingRepairs(runtime.Trajectory())) == 0 &&
			len(repairItems(runtime.Trajectory(), trajectory.RepairResolved)) > 0
	}, "the repair was never resolved against the correction that answered it")
}

// The other half of the same rule: content nobody heard is cancelled, and a
// cancellation owes nothing. A runtime that raised repairs for speech it never
// emitted would teach the model to apologise for things it never said.
func TestUnheardContentInvalidatedByALaterRevisionOwesNothing(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twelve dollars, right."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twelve it is."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twenty dollars."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Twenty, confirmed."}},
	)
	slow := newSlow(
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The amount is twelve dollars."}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The amount is twenty dollars."}},
	)
	runtime, _ := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &revisingASR{partial: "transfer twelve", final: "transfer twenty"}, nil
		},
		// A synthesiser that never returns audio means nothing is ever heard.
		Speech: toneSpeech{chunks: 0, silent: true},
		Fast:   fast, Slow: slow, ObservationPolicy: cascade.ObservationStablePartial,
		Policies: partialPolicies(),
	}, binding.Settings{})

	pushAudio(t, runtime, tone(2400, 8000), 3)
	pushAudio(t, runtime, silence(2400), 8)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindObservation && item.SourceRevision == 2 {
				return true
			}
		}
		return false
	}, "the endpoint observation never committed")

	if items := repairItems(runtime.Trajectory(), trajectory.RepairRequired); len(items) > 0 {
		t.Fatalf("nothing was heard, so nothing is owed: %d repairs raised", len(items))
	}
}
