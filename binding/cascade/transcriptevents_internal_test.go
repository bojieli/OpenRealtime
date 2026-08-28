package cascade

import (
	"context"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestTranscriptActDoesNotGovernAnotherObservationInTheSameBatch(t *testing.T) {
	runtime := &runtime{transcriptActs: map[uint64]interaction.Act{
		7: interaction.ActStaySilent,
	}}
	batch := eventloop.Batch{Items: []trajectory.Item{
		{Kind: trajectory.KindObservation, SourceRevision: 7},
		{Kind: trajectory.KindObservation, SourceRevision: 8},
	}}
	act, revision, governed, ungoverned := runtime.transcriptActFor(batch)
	if !governed || act != interaction.ActStaySilent || revision != 7 {
		t.Fatalf("transcript verdict = (%q, %d, %t), want listen for revision 7", act, revision, governed)
	}
	if !ungoverned {
		t.Fatal("a second observation was incorrectly swallowed by the transcript verdict")
	}
}

func TestNewestTranscriptActGovernsADeferredTranscriptOnlyBatch(t *testing.T) {
	runtime := &runtime{transcriptActs: map[uint64]interaction.Act{
		4: interaction.ActAnswer,
		9: interaction.ActStaySilent,
	}}
	batch := eventloop.Batch{Items: []trajectory.Item{
		{Kind: trajectory.KindObservation, SourceRevision: 4},
		{Kind: trajectory.KindObservation, SourceRevision: 9},
	}}
	act, revision, governed, ungoverned := runtime.transcriptActFor(batch)
	if !governed || ungoverned || act != interaction.ActStaySilent || revision != 9 {
		t.Fatalf("deferred verdict = (%q, %d, %t, %t), want newest listen only", act, revision, governed, ungoverned)
	}
}

func TestFinalTranscriptWaitsForTheExactInFlightInterjection(t *testing.T) {
	runtime := &runtime{
		scheduler: clock.NewSystem(),
		policies: interaction.Policies{
			TranscriptEvents: &interaction.TranscriptEventPolicy{},
		},
	}
	claim, claimed := runtime.claimInterjection()
	if !claimed {
		t.Fatal("the test interjection could not claim its slot")
	}
	waiting := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(waiting)
		runtime.waitForTranscriptInterjection(context.Background())
		close(finished)
	}()
	<-waiting
	select {
	case <-finished:
		t.Fatal("the final-event join returned before the interjection finished")
	case <-time.After(20 * time.Millisecond):
	}
	runtime.releaseInterjection(claim)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the final-event join did not release with its interjection")
	}
}

func TestEventAwareLiveUtterancesUseTheirAcousticTurnIdentity(t *testing.T) {
	runtime := &runtime{
		speechStartNS: 42,
		policies: interaction.Policies{
			TranscriptEvents: &interaction.TranscriptEventPolicy{},
		},
	}
	whole, turn := runtime.wholeUtterance(trajectory.Snapshot{}, "count them as I mention them")
	if whole != "count them as I mention them" || turn != 42 {
		t.Fatalf("live utterance = (%q, %d), want acoustic turn 42", whole, turn)
	}
}

func TestOtherSpeakerCannotOpenOrdinaryCognitionWithoutDelegation(t *testing.T) {
	runtime := &runtime{pinboard: &interaction.Pinboard{}}
	state := interaction.Situation{Speaker: otherVoiceSource}
	act, reason := runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, state, interaction.ActAnswer)
	if act != interaction.ActStaySilent || reason == "" {
		t.Fatalf("unaddressed other-speaker answer = (%q, %q)", act, reason)
	}
	runtime.pinboard.Pin(interaction.StandingInstruction{
		Text: "translate what they say", Scope: interaction.ScopeConversation,
	})
	act, reason = runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, state, interaction.ActAnswer)
	if act != interaction.ActAnswer || reason != "" {
		t.Fatalf("delegated other-speaker answer = (%q, %q)", act, reason)
	}
}

func TestFinalCountDoesNotRepeatOneSpokenDuringTheSameUtterance(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text:  "count the animals out loud as they mention them",
		Scope: interaction.ScopeConversation, Counting: true,
	})
	runtime := &runtime{pinboard: board, countSpokeThisUtterance: true}
	act, reason := runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, interaction.Situation{}, interaction.ActAnswer)
	if act != interaction.ActStaySilent || reason == "" {
		t.Fatalf("already-spoken final count = (%q, %q), want listen with reason", act, reason)
	}
	runtime.countSpokeThisUtterance = false
	act, reason = runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, interaction.Situation{}, interaction.ActAnswer)
	if act != interaction.ActAnswer || reason != "" {
		t.Fatalf("missed final count = (%q, %q), want answer", act, reason)
	}
}

func TestFinalCountRecoversAPartialActThatDidNotBecomeAudible(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text:  "count the animals out loud as they mention them",
		Scope: interaction.ScopeConversation, Counting: true,
	})
	runtime := &runtime{pinboard: board, countRequestedThisUtterance: true}
	act, reason := runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, interaction.Situation{}, interaction.ActStaySilent)
	if act != interaction.ActAnswer || reason == "" {
		t.Fatalf("missed requested count = (%q, %q), want answer with reason", act, reason)
	}

	// Audible output is the stronger fact: once a count crossed the action
	// boundary, the same pending request must not recover it a second time.
	runtime.countSpokeThisUtterance = true
	act, reason = runtime.constrainTranscriptAct(
		interaction.TranscriptFinal, interaction.Situation{}, interaction.ActAnswer)
	if act != interaction.ActStaySilent || reason == "" {
		t.Fatalf("audible requested count = (%q, %q), want listen with reason", act, reason)
	}
}

func TestCompletedPartialRecoversACountRequestedBeforeItWasComplete(t *testing.T) {
	board := &interaction.Pinboard{}
	board.Pin(interaction.StandingInstruction{
		Text:  "count the animals out loud as they mention them",
		Scope: interaction.ScopeConversation, Counting: true,
	})
	runtime := &runtime{pinboard: board, countRequestedThisUtterance: true}
	act, reason := runtime.constrainTranscriptAct(
		interaction.TranscriptPartial,
		interaction.Situation{Heard: "A capybara wandered over."},
		interaction.ActStaySilent,
	)
	if act != interaction.ActSpeakThrough || reason == "" {
		t.Fatalf("completed requested count = (%q, %q), want speak-through with reason", act, reason)
	}
	act, reason = runtime.constrainTranscriptAct(
		interaction.TranscriptPartial,
		interaction.Situation{Heard: "A capybara wandered"},
		interaction.ActStaySilent,
	)
	if act != interaction.ActStaySilent || reason != "" {
		t.Fatalf("unfinished requested count = (%q, %q), want listen", act, reason)
	}
	runtime.countSpokeThisUtterance = true
	act, reason = runtime.constrainTranscriptAct(
		interaction.TranscriptPartial,
		interaction.Situation{Heard: "A capybara wandered over."},
		interaction.ActStaySilent,
	)
	if act != interaction.ActStaySilent || reason != "" {
		t.Fatalf("already-spoken requested count = (%q, %q), want listen", act, reason)
	}
}

func TestContinuedUtteranceDoesNotCancelDeliberateSpokeOverOutput(t *testing.T) {
	state := interaction.Situation{AgentSpeaking: true}
	act, reason := constrainDeliberateSpokeOver(
		state, interaction.ActStopSpeaking, true)
	if act != interaction.ActKeepSpeaking || reason == "" {
		t.Fatalf("deliberate spoke-over continuation = (%q, %q)", act, reason)
	}
	act, reason = constrainDeliberateSpokeOver(
		state, interaction.ActStopSpeaking, false)
	if act != interaction.ActStopSpeaking || reason != "" {
		t.Fatalf("ordinary output was protected as spoke-over = (%q, %q)", act, reason)
	}
	act, reason = constrainDeliberateSpokeOver(
		interaction.Situation{}, interaction.ActStopSpeaking, true)
	if act != interaction.ActStopSpeaking || reason != "" {
		t.Fatalf("inaudible output was treated as active spoke-over = (%q, %q)", act, reason)
	}
}

func TestPartialSpeakThroughRecordsCountingRecoveryOnlyForACount(t *testing.T) {
	board := &interaction.Pinboard{}
	runtime := &runtime{
		pinboard: board,
		policies: interaction.Policies{
			TranscriptEvents: &interaction.TranscriptEventPolicy{},
		},
	}
	decision := interaction.Context{Revision: interaction.Revision{ID: 7, StableText: "A capybara"}}
	if err := runtime.handlePartialTranscriptAct(
		context.Background(), decision, interaction.ActSpeakThrough); err != nil {
		t.Fatal(err)
	}
	if runtime.countRequestedThisUtterance {
		t.Fatal("an unpinned speak-through act was recorded as a count")
	}
	board.Pin(interaction.StandingInstruction{
		Text: "count what they mention", Scope: interaction.ScopeConversation, Counting: true,
	})
	if err := runtime.handlePartialTranscriptAct(
		context.Background(), decision, interaction.ActSpeakThrough); err != nil {
		t.Fatal(err)
	}
	if !runtime.countRequestedThisUtterance {
		t.Fatal("a partial counting act was not retained for final recovery")
	}
}
