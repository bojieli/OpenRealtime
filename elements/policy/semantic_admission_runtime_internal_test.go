package policy

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestValidateSemanticOutcomeRequiresExactConsistentBoundedChoice(t *testing.T) {
	options := []string{"listen", "answer"}
	for _, test := range []struct {
		name    string
		outcome interaction.Outcome
	}{
		{name: "unknown option", outcome: interaction.Outcome{Option: "speak", Index: 0}},
		{name: "wrong index", outcome: interaction.Outcome{Option: "answer", Index: 0}},
		{name: "NaN confidence", outcome: interaction.Outcome{Option: "listen", Index: 0, Confidence: math.NaN(), Measured: true}},
		{name: "infinite confidence", outcome: interaction.Outcome{Option: "listen", Index: 0, Confidence: math.Inf(1), Measured: true}},
		{name: "negative confidence", outcome: interaction.Outcome{Option: "listen", Index: 0, Confidence: -0.1, Measured: true}},
		{name: "oversized confidence", outcome: interaction.Outcome{Option: "listen", Index: 0, Confidence: 1.1, Measured: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSemanticOutcome(test.outcome, options); err == nil {
				t.Fatalf("malformed outcome was accepted: %+v", test.outcome)
			}
		})
	}
	if err := validateSemanticOutcome(interaction.Outcome{
		Option: "answer", Index: 1, Confidence: 0.7, Measured: true,
	}, options); err != nil {
		t.Fatalf("valid exact outcome: %v", err)
	}
}

func TestSemanticExplicitCreateDoesNotAskProviderToChooseSingletonAct(t *testing.T) {
	runner := semanticAdmissionRunner{}
	act, outcome, err := runner.decideAct(context.Background(), "create", interaction.Situation{
		AllowedActs: []interaction.Act{interaction.ActStaySilent},
	})
	if err != nil || act != interaction.ActStaySilent || outcome.Index != 0 ||
		outcome.Option != string(interaction.ActStaySilent) || outcome.Measured {
		t.Fatalf("singleton explicit-create act = %q, %+v, %v", act, outcome, err)
	}
	_, _, err = runner.decideAct(context.Background(), "create", interaction.Situation{
		AgentSpeaking: true,
		AllowedActs:   []interaction.Act{interaction.ActAnswer},
	})
	if err == nil || !strings.Contains(err.Error(), "no executable act") {
		t.Fatalf("empty explicit-create act set error = %v", err)
	}
}

func TestSemanticRecentBeforeDoesNotEchoCurrentUtterance(t *testing.T) {
	items := []trajectory.Item{
		{ID: "previous", Kind: trajectory.KindObservation, Content: "the earlier clause",
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
		{ID: "current", Kind: trajectory.KindObservation, Content: "the current request",
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
		{ID: "beyond-prefix", Kind: trajectory.KindObservation, Content: "future evidence",
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}},
	}
	lines := semanticRecentBefore(items, "current", 12)
	if len(lines) != 1 || lines[0] != "user: the earlier clause" {
		t.Fatalf("recent extraction context = %v", lines)
	}
}

type semanticVariadicTestInput struct {
	envelope     element.Envelope
	delivered    atomic.Bool
	receiveCalls atomic.Int32
}

func BenchmarkSemanticAdmissionSituation(b *testing.B) {
	update := SessionInvocationUpdate{Invocation: continuation.Invocation{
		Instruction: "Answer only when the current evidence requires it.",
	}}
	audio := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "audio", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "a short audio turn",
	}}}
	b.Run("audio", func(b *testing.B) {
		runner := semanticAdmissionRunner{config: SemanticAdmissionConfig{RecentLines: 12}}
		b.ReportAllocs()
		for range b.N {
			state, err := runner.situation(context.Background(), semanticRequest{}, update, audio)
			if err != nil || len(state.Seeing) != 0 {
				b.Fatalf("audio situation=%+v error=%v", state, err)
			}
		}
	})

	image := make([]byte, 25<<10)
	visual := trajectory.Snapshot{Version: 3, Items: []trajectory.Item{
		{
			ID: "old-frame", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "old frame",
			Observation: &trajectory.ObservationMeta{
				Observer: "client", Source: "screen", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "old", MIMEType: "image/png", Source: "screen"}},
			},
		},
		{
			ID: "current-frame", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "current frame",
			Observation: &trajectory.ObservationMeta{
				Observer: "client", Source: "screen", Authority: trajectory.AuthorityUser,
				Media: []trajectory.MediaRef{{Handle: "current", MIMEType: "image/png", Source: "screen"}},
			},
		},
		{ID: "tool-result", Kind: trajectory.KindToolResult, Producer: trajectory.Producer{Phase: trajectory.PhaseTool}},
	}}
	b.Run("direct_visual_25KiB", func(b *testing.B) {
		runner := semanticAdmissionRunner{
			config: SemanticAdmissionConfig{RecentLines: 12, DirectVisualInput: true},
			media: continuation.MediaResolver(func(handle string) (continuation.Media, error) {
				if handle != "current" {
					return continuation.Media{}, errors.New("resolved stale visual evidence")
				}
				return continuation.Media{MIMEType: "image/png", Bytes: image}, nil
			}),
		}
		b.SetBytes(int64(len(image)))
		b.ReportAllocs()
		for range b.N {
			state, err := runner.situation(context.Background(), semanticRequest{}, update, visual)
			if err != nil || len(state.Seeing) != 1 || len(state.Seeing[0].Bytes) != len(image) {
				b.Fatalf("visual situation=%+v error=%v", state, err)
			}
		}
	})
}

func TestApplySemanticExtractionIsAtomicAndBounded(t *testing.T) {
	existing := []interaction.StandingInstruction{{
		Text: "tell me when the build finishes", Scope: interaction.ScopeConversation,
		Turn: 1, SetNS: 10,
	}}
	overflow := interaction.Extraction{Pins: []interaction.StandingInstruction{
		{Text: "count the animals", Scope: interaction.ScopeConversation},
		{Text: "translate as they speak", Scope: interaction.ScopeConversation},
	}}
	if after, _, _, err := applySemanticExtraction(existing, overflow, 2, 20, 1); err == nil || after != nil {
		t.Fatalf("overflow extraction = %+v, %v", after, err)
	}
	if existing[0].Text != "tell me when the build finishes" || existing[0].SetNS != 10 {
		t.Fatalf("failed extraction mutated caller state: %+v", existing)
	}

	replacement := interaction.Extraction{
		Revokes: []string{"build finishes"},
		Pins: []interaction.StandingInstruction{{
			Text: "count the animals", Scope: interaction.ScopeConversation,
		}},
	}
	after, pinned, revoked, err := applySemanticExtraction(existing, replacement, 2, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Text != "count the animals" || after[0].Turn != 2 ||
		after[0].SetNS != 20 || pinned != 1 || revoked != 1 {
		t.Fatalf("bounded extraction = %+v pinned=%d revoked=%d", after, pinned, revoked)
	}

	updated, pinned, revoked, err := applySemanticExtraction(after, interaction.Extraction{
		Pins: []interaction.StandingInstruction{{
			Text: "count the animals and say nothing else", Scope: interaction.ScopeConversation,
			Restricting: true,
		}},
	}, 2, 30, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || updated[0].Text != "count the animals and say nothing else" ||
		updated[0].SetNS != 20 || !updated[0].Restricting || pinned != 0 || revoked != 0 {
		t.Fatalf("same-turn replacement = %+v pinned=%d revoked=%d", updated, pinned, revoked)
	}
}

func TestApplySemanticExtractionExpiresOnlyPreviouslyActiveTurnPolicies(t *testing.T) {
	existing := []interaction.StandingInstruction{{
		Text: "wait until I finish", Scope: interaction.ScopeTurn, Turn: 1, SetNS: 10,
	}}
	after, pinned, revoked, err := applySemanticExtraction(
		existing, interaction.Extraction{Pins: []interaction.StandingInstruction{{
			Text: "wait for my next sentence", Scope: interaction.ScopeTurn,
		}}}, 2, 20, 4,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].Text != "wait for my next sentence" || after[0].Turn != 2 ||
		pinned != 1 || revoked != 0 {
		t.Fatalf("turn policy transition = %+v pinned=%d revoked=%d", after, pinned, revoked)
	}
	after, _, _, err = applySemanticExtraction(after, interaction.Extraction{}, 3, 30, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("turn-scoped policy survived its one governed turn: %+v", after)
	}
}

func (*semanticVariadicTestInput) Name() string { return "committed" }
func (*semanticVariadicTestInput) Type() element.Type {
	return stateelements.ObservationCommitOutcomeType()
}
func (*semanticVariadicTestInput) Lanes() []element.Receiver { return nil }
func (input *semanticVariadicTestInput) Receive(context.Context) (element.Envelope, error) {
	input.receiveCalls.Add(1)
	return element.Envelope{}, errors.New("singular Receive used for variadic semantic commits")
}
func (input *semanticVariadicTestInput) ReceiveAny(
	ctx context.Context,
) (element.Envelope, string, error) {
	if input.delivered.CompareAndSwap(false, true) {
		return input.envelope.Clone(), "committed/audio", nil
	}
	<-ctx.Done()
	return element.Envelope{}, "", context.Cause(ctx)
}

func TestReceiveSemanticAdmissionUsesVariadicArbitrationForCommittedLanes(t *testing.T) {
	input := &semanticVariadicTestInput{envelope: element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-a",
	}}
	ctx, cancel := context.WithCancelCause(context.Background())
	inputs := make(chan semanticAdmissionInput, 1)
	failures := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(1)
	go receiveSemanticAdmission(ctx, "committed", input, true, inputs, failures, &wait)
	select {
	case got := <-inputs:
		if got.kind != "committed" || got.envelope.ItemID != "commit-a" {
			t.Fatalf("variadic semantic input = %+v", got)
		}
	case err := <-failures:
		t.Fatalf("variadic semantic receiver failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("variadic semantic receiver did not deliver")
	}
	if calls := input.receiveCalls.Load(); calls != 0 {
		t.Fatalf("variadic semantic receiver called singular Receive %d time(s)", calls)
	}
	cancel(errors.New("test complete"))
	done := make(chan struct{})
	go func() {
		wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("variadic semantic receiver did not stop after cancellation")
	}
}
