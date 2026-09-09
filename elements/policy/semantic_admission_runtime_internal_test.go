package policy

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
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
	act, outcome, err := runner.decideAct(context.Background(), semanticRequest{operation: "create"}, interaction.Situation{
		AllowedActs: []interaction.Act{interaction.ActStaySilent},
	})
	if err != nil || act != interaction.ActStaySilent || outcome.Index != 0 ||
		outcome.Option != string(interaction.ActStaySilent) || outcome.Measured {
		t.Fatalf("singleton explicit-create act = %q, %+v, %v", act, outcome, err)
	}
	_, _, err = runner.decideAct(context.Background(), semanticRequest{operation: "create"}, interaction.Situation{
		AgentSpeaking: true,
		AllowedActs:   []interaction.Act{interaction.ActAnswer},
	})
	if err == nil || !strings.Contains(err.Error(), "no executable act") {
		t.Fatalf("empty explicit-create act set error = %v", err)
	}
}

func TestSemanticControlDispositionSuppressesCommittedControlWithoutOpeningCreate(t *testing.T) {
	for _, testCase := range []struct {
		act      interaction.Act
		wantCode string
	}{
		{act: interaction.ActKeepSpeaking, wantCode: "keep_speaking"},
		{act: interaction.ActStopSpeaking, wantCode: "stop_speaking"},
	} {
		code, message, refused := semanticControlDisposition("committed", testCase.act)
		if refused || code != testCase.wantCode || strings.TrimSpace(message) == "" {
			t.Fatalf("committed %s disposition = %q, %q, %t", testCase.act, code, message, refused)
		}
		code, message, refused = semanticControlDisposition("create", testCase.act)
		if !refused || code != "unsupported_act" || !strings.Contains(message, "cannot claim") {
			t.Fatalf("create %s disposition = %q, %q, %t", testCase.act, code, message, refused)
		}
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

func TestSemanticHeardSinceUsesOnlyCompletedSpeechAfterTheLastAudibleBoundary(t *testing.T) {
	endpoint := func(id, source, text string) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindObservation, Content: text,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Observation: &trajectory.ObservationMeta{
				Observer: "asr", Source: source, Authority: trajectory.AuthorityUser,
			},
			Event: &trajectory.EventMetadata{
				EventID: "event-" + id, Type: "asr.endpoint", Source: "asr", Channel: source,
			},
		}
	}
	voice := func(id, text string, visibility trajectory.Visibility) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindAssistant, Content: text, Visibility: visibility,
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast, SpeechAuthority: "voice"},
		}
	}
	played := func(id, target string) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindAssistantState,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: target, Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 100,
			},
		}
	}
	canceled := func(id, target string) trajectory.Item {
		return trajectory.Item{
			ID: id, Kind: trajectory.KindAssistantState,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
			AssistantState: &trajectory.AssistantState{
				AssistantItemID: target, Visibility: trajectory.VisibilityCancelled,
			},
		}
	}

	tests := []struct {
		name    string
		items   []trajectory.Item
		current string
		speaker string
		maximum int
		want    string
	}{
		{
			name: "split endpoints and silent cognition remain new",
			items: []trajectory.Item{
				endpoint("old", "microphone", "old speech"),
				voice("count-one", "One.", trajectory.VisibilityPrepared), played("played-one", "count-one"),
				endpoint("heron", "microphone", "Then, a heron."),
				{ID: "background", Kind: trajectory.KindAssistant, Content: "internal state",
					Visibility: trajectory.VisibilityPlayed,
					Producer:   trajectory.Producer{Phase: trajectory.PhaseSlow, SpeechAuthority: "silent"}},
				endpoint("landed", "microphone", "Landed on the far bank."),
			},
			current: "landed", speaker: "user", maximum: 12,
			want: "Then, a heron. Landed on the far bank.",
		},
		{
			name: "played transition excludes speech that preceded playback",
			items: []trajectory.Item{
				voice("answer", "An answer.", trajectory.VisibilityPrepared),
				endpoint("during", "microphone", "speech before playback ended"),
				played("played-answer", "answer"),
				endpoint("after", "microphone", "speech after playback"),
			},
			current: "after", speaker: "user", maximum: 12, want: "speech after playback",
		},
		{
			name: "canceled prepared voice does not claim a turn",
			items: []trajectory.Item{
				endpoint("first", "microphone", "first clause"),
				voice("unheard", "unheard answer", trajectory.VisibilityPrepared), canceled("canceled", "unheard"),
				endpoint("second", "microphone", "second clause"),
			},
			current: "second", speaker: "user", maximum: 12, want: "first clause second clause",
		},
		{
			name: "another speaker bounds the evidence",
			items: []trajectory.Item{
				endpoint("user-before", "microphone", "user before"),
				endpoint("waiter", "recorded-menu", "another speaker"),
				endpoint("user-after", "microphone", "user after"),
			},
			current: "user-after", speaker: "user", maximum: 12, want: "user after",
		},
		{
			name: "recent-line bound is exact",
			items: []trajectory.Item{
				endpoint("one", "microphone", "one"), endpoint("two", "microphone", "two"),
				endpoint("three", "microphone", "three"),
			},
			current: "three", speaker: "user", maximum: 2, want: "two three",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := semanticHeardSince(
				testCase.items, testCase.current, testCase.speaker, testCase.maximum,
			); got != testCase.want {
				t.Fatalf("heard since = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestSemanticStandingUtteranceUsesTheWholeUnansweredEndpointStretch(t *testing.T) {
	current := trajectory.Item{Content: "ask whether I am still there"}
	situation := interaction.Situation{
		HeardSince: "If I have been quiet for fifteen seconds, ask whether I am still there",
	}
	if got := semanticStandingUtterance(situation, current); got != situation.HeardSince {
		t.Fatalf("standing utterance = %q, want %q", got, situation.HeardSince)
	}
	situation.HeardSince = ""
	if got := semanticStandingUtterance(situation, current); got != current.Content {
		t.Fatalf("standing utterance fallback = %q, want %q", got, current.Content)
	}
}

func TestSemanticVoiceActivationReceivesExactVisualEvidenceWithoutMutableBytes(t *testing.T) {
	imageBytes := []byte("sealed current frame")
	wantImage := append([]byte(nil), imageBytes...)
	decider := &semanticCaptureDecider{answer: semanticVoiceConditionMet, mutateImages: true}
	runner := semanticAdmissionRunner{decider: decider}
	situation := interaction.Situation{
		Contract: "Tell the user when the build finishes.",
		Pins:     []string{"until revoked: say only when the build has finished"},
		Seeing:   []interaction.Image{{MIMEType: "image/png", Bytes: imageBytes}},
		AllowedActs: []interaction.Act{
			interaction.ActStaySilent, interaction.ActAnswer,
		},
	}
	outcome, err := runner.verifyVoiceActivation(context.Background(), situation)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Option != semanticVoiceConditionMet || len(decider.decisions) != 1 {
		t.Fatalf("visual activation = %+v decisions=%+v", outcome, decider.decisions)
	}
	decision := decider.decisions[0]
	if !strings.Contains(decision.Evidence, "build has finished") || len(decision.Images) != 1 ||
		decision.Images[0].MIMEType != "image/png" || !reflect.DeepEqual(decision.Images[0].Bytes, wantImage) {
		t.Fatalf("visual activation evidence = %+v", decision)
	}
	if !reflect.DeepEqual(imageBytes, wantImage) {
		t.Fatal("visual activation retained mutable Situation bytes")
	}
}

func TestSemanticActivationEvidenceAdmitsOnlyTypedCurrentConditionInputs(t *testing.T) {
	tests := []struct {
		name      string
		situation interaction.Situation
		want      bool
	}{
		{name: "empty"},
		{name: "partial transcript", situation: interaction.Situation{TranscriptEvent: interaction.TranscriptPartial}},
		{name: "final transcript", situation: interaction.Situation{TranscriptEvent: interaction.TranscriptFinal}, want: true},
		{name: "textual visual observation", situation: interaction.Situation{Seen: "build complete"}, want: true},
		{name: "direct visual observation", situation: interaction.Situation{
			Seeing: []interaction.Image{{MIMEType: "image/png", Bytes: []byte("pixels")}},
		}, want: true},
		{name: "due quiet policy", situation: interaction.Situation{Quiet: true}, want: true},
		{name: "elapsed but unreserved silence", situation: interaction.Situation{Silence: "15s"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := semanticActivationEvidence(testCase.situation); got != testCase.want {
				t.Fatalf("activation evidence = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestSemanticSituationSealsAgentOutputBeforeAConcurrentUpdate(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var clockCalls atomic.Uint64
	runner := semanticAdmissionRunner{
		config: SemanticAdmissionConfig{RecentLines: 12},
		clock: graphruntime.ClockFunc(func() uint64 {
			call := clockCalls.Add(1)
			if call == 1 {
				close(entered)
				<-release
			}
			return call
		}),
		agentOutput: interaction.AgentOutput{
			Revision: 1, Active: true, Queued: true,
			Saying: "the sealed response", InFlight: "sealed lifecycle work",
			ProtectedStreams: []string{"speech-stream"},
		},
	}
	request := semanticRequest{streamID: "speech-stream"}
	update := SessionInvocationUpdate{Invocation: continuation.Invocation{
		Instruction: "Use the exact lifecycle sample for this decision.",
	}}
	prefix := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
		ID: "observation-1", Kind: trajectory.KindObservation, Content: "current evidence",
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
	}}}
	type situationResult struct {
		situation interaction.Situation
		err       error
	}
	firstResult := make(chan situationResult, 1)
	go func() {
		situation, err := runner.situation(context.Background(), request, update, prefix)
		firstResult <- situationResult{situation: situation, err: err}
	}()
	<-entered
	later := interaction.AgentOutput{
		Revision: 2, Active: true, Queued: true,
		Saying: "the later response", InFlight: "later lifecycle work",
		ProtectedStreams: []string{"another-stream"},
	}
	if err := runner.acceptAgentOutput(context.Background(), element.Envelope{Payload: later}); err != nil {
		t.Fatal(err)
	}
	close(release)
	first := <-firstResult
	if first.err != nil {
		t.Fatal(first.err)
	}
	second, err := runner.situation(context.Background(), request, update, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !first.situation.AgentSpeaking || first.situation.AgentSaying != "the sealed response" ||
		first.situation.InFlight != "sealed lifecycle work" || !first.situation.AgentOutputProtected {
		t.Fatalf("in-flight decision observed mutable agent output: %+v", first.situation)
	}
	if !second.AgentSpeaking || second.AgentSaying != later.Saying || second.InFlight != later.InFlight ||
		second.AgentOutputProtected {
		t.Fatalf("next decision did not observe later agent output: %+v", second)
	}
}

type semanticCaptureDecider struct {
	answer       string
	mutateImages bool
	decisions    []interaction.Decision
}

func (*semanticCaptureDecider) Name() string { return "semantic-capture" }

func (*semanticCaptureDecider) Descriptor() SemanticDeciderDescriptor {
	return SemanticDeciderDescriptor{}
}

func (decider *semanticCaptureDecider) Decide(
	_ context.Context, decision interaction.Decision,
) (interaction.Outcome, error) {
	captured := decision
	captured.Options = append([]string(nil), decision.Options...)
	captured.Images = cloneSemanticImages(decision.Images)
	decider.decisions = append(decider.decisions, captured)
	if decider.mutateImages && len(decision.Images) > 0 && len(decision.Images[0].Bytes) > 0 {
		decision.Images[0].Bytes[0] ^= 0xff
	}
	for index, option := range decision.Options {
		if option == decider.answer {
			return interaction.Outcome{Index: index, Option: option}, nil
		}
	}
	return interaction.Outcome{}, errors.New("answer is not an available option")
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

func TestVoiceActivationRetainsReplyContextWithoutEarlierCallerConditions(t *testing.T) {
	s := interaction.Situation{Heard: "I can try that, but it is a hassle.", Recent: []string{
		"user: ship by the thirteenth", "agent: An earlier answer.",
		"agent: Would you try restarting the router?", "user: I can try that, but it is a hassle.",
	}}
	evidence := semanticVoiceActivationEvidence(s)
	if !strings.Contains(evidence, "Would you try restarting the router?") || !strings.Contains(evidence, s.Heard) {
		t.Fatalf("missing current reply context: %s", evidence)
	}
	if strings.Contains(evidence, "thirteenth") || strings.Contains(evidence, "An earlier answer.") || strings.Contains(evidence, "Recent conversation:") {
		t.Fatalf("activation reused historical observations: %s", evidence)
	}
}
