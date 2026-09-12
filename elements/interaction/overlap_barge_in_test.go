package interaction

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const overlapBargeInGraph = `graph overlap_barge_in_test {
    interaction.OverlapBargeIn :: overlap;
    input activity = overlap.activity;
    input transcript = overlap.transcript;
    input semantic = overlap.semantic;
    input speech = overlap.speech;
    input result = overlap.result;
    input invocation = overlap.invocation;
    input model = overlap.model;
    input segmentation = overlap.segmentation;
    input tts = overlap.tts;
    input playback = overlap.playback;
    input release = overlap.release;
    output model_cancel = overlap.model_cancel;
    output segmentation_cancel = overlap.segmentation_cancel;
    output tts_cancel = overlap.tts_cancel;
    output playback_cancel = overlap.playback_cancel;
    output safe_result = overlap.safe_result;
    output safe_release = overlap.safe_release;
    output decision = overlap.decision;
    output state = overlap.state;
    output agent_output = overlap.agent_output;
    output resolved = overlap.resolved;
}
`

const overlapBargeInSession = "overlap-session"

type overlapBargeInHarness struct {
	mounted   *graphruntime.Mounted
	scheduler *clock.Manual
	done      <-chan error
	cancel    context.CancelFunc
}

func TestOverlapBargeInDescriptorAndFactoryAreRegistered(t *testing.T) {
	descriptor := OverlapBargeInDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "interaction.OverlapBargeIn" || descriptor.Revision != 12 ||
		!descriptor.Reaction.BreaksCycles || descriptor.ConfigSchema !=
		"schema://openrealtime/interaction/overlap-barge-in-config/v1" ||
		descriptor.StateSchema != "schema://openrealtime/interaction/overlap-state/v4" {
		t.Fatalf("overlap descriptor = %+v", descriptor)
	}

	descriptorCount := 0
	for _, candidate := range Descriptors() {
		if candidate.Name == descriptor.Name {
			descriptorCount++
		}
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	factoryCount := 0
	for _, registration := range registrations {
		if registration.Profile.Reference != descriptor.Name {
			continue
		}
		factoryCount++
		if !reflect.DeepEqual(registration.Factory.Descriptor(), descriptor) {
			t.Fatalf("registered overlap descriptor drifted: %+v", registration.Factory.Descriptor())
		}
		if _, ok := registration.Factory.(element.ConfigValidator); !ok {
			t.Fatal("registered overlap factory has no exact config validator")
		}
	}
	if descriptorCount != 1 || factoryCount != 1 {
		t.Fatalf("overlap registration counts: descriptors=%d factories=%d",
			descriptorCount, factoryCount)
	}
}

func TestOverlapBargeInReleaseBarrierRetiresSpeechBeforeForwarding(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":10,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	const (
		runID       = "release-before-status-run"
		utteranceID = "release-before-status-utterance"
	)
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-done", runID))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segmentation-done", runID, OutcomeCompleted,
	))
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invocation-late", runID))
	harness.sendAndSync(t, "speech", overlapSpeechEnvelope(
		"speech", runID, utteranceID, "The visible response is complete.",
	))
	state := harness.sendAndSync(t, "tts", overlapTransitionEnvelope(
		"tts-generating", runID, utteranceID,
		speechelements.StageSynthesis, speechelements.StateGenerating,
	))
	state = harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playback-emitting", runID, utteranceID,
		speechelements.StagePlayback, speechelements.StateEmitting,
	))
	if state.ActiveTTS != 1 || state.ActivePlayback != 1 {
		t.Fatalf("release test did not open both speech horizons: %+v", state)
	}

	release := overlapReleaseEnvelope(
		"playback-released", runID, utteranceID, "The visible response is complete.",
		action.Outcome{Completed: true, PlayedMS: 40},
	)
	state = harness.sendAndSync(t, "release", release)
	if state.ActiveTTS != 0 || state.ActivePlayback != 0 || state.OverlapActive {
		t.Fatalf("release did not retire both speech horizons: %+v", state)
	}
	forwarded := receive(t, harness.output(t, "safe_release"))
	boundary, ok := forwarded.Payload.(speechelements.PlaybackRelease)
	receipt := boundary.Receipt
	if !ok || receipt.Kind != speechelements.PlaybackReleased ||
		receipt.Utterance.ID != utteranceID || !receipt.Outcome.Completed ||
		forwarded.ItemID == release.ItemID ||
		!slices.Contains(forwarded.CausalParents, release.ItemID) {
		t.Fatalf("ordered playback-release pass-through = %+v / %#v", forwarded, forwarded.Payload)
	}
	if !reflect.DeepEqual(boundary.AgentOutput, state.AgentOutput) ||
		boundary.AgentOutput.Active || boundary.AgentOutputItemID == "" ||
		!slices.Contains(forwarded.CausalParents, boundary.AgentOutputItemID) {
		t.Fatalf("release did not carry its exact retired output state: %+v", boundary)
	}

	// The gateway may submit new audio as soon as it observes safe_release.
	// Neither independently drained status lane may make that input an overlap
	// or resurrect the terminal utterance afterward.
	state = harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"new-speech", "new-stream", acousticelements.SpeechStarted,
	))
	if state.OverlapActive || state.ActiveTTS != 0 || state.ActivePlayback != 0 {
		t.Fatalf("release permit exposed stale speech work: %+v", state)
	}
	assertNoEnvelope(t, harness.output(t, "decision"))
	state = harness.sendAndSync(t, "tts", overlapTransitionEnvelope(
		"tts-generated-late", runID, utteranceID,
		speechelements.StageSynthesis, speechelements.StateGenerated,
	))
	state = harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playback-played-late", runID, utteranceID,
		speechelements.StagePlayback, speechelements.StatePlayed,
	))
	if state.OverlapActive || state.ActiveTTS != 0 || state.ActivePlayback != 0 ||
		len(decider.seenDecisions()) != 0 {
		t.Fatalf("late terminal statuses revived released speech: %+v decisions=%+v",
			state, decider.seenDecisions())
	}
}

func TestOverlapBargeInReleaseBeforeStatusTombstonesTheExactUtterance(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":10,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	const (
		runID       = "release-first-run"
		utteranceID = "release-first-utterance"
		spoken      = "Release wins every status-lane race."
	)
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-first", runID))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segmentation-first", runID, OutcomeCompleted,
	))
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invocation-last", runID))
	release := overlapReleaseEnvelope(
		"release-first", runID, utteranceID, spoken,
		action.Outcome{Completed: true, PlayedMS: 30},
	)
	state := harness.sendAndSync(t, "release", release)
	if state.ActiveTTS != 0 || state.ActivePlayback != 0 || state.OverlapActive {
		t.Fatalf("release-first barrier retained speech work: %+v", state)
	}
	forwarded := receive(t, harness.output(t, "safe_release"))
	if !slices.Contains(forwarded.CausalParents, release.ItemID) {
		t.Fatalf("release-first pass-through lost ancestry: %+v", forwarded)
	}

	state = harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"post-release-speech", "post-release-stream", acousticelements.SpeechStarted,
	))
	if state.OverlapActive {
		t.Fatalf("post-release speech became overlap: %+v", state)
	}
	for _, transition := range []struct {
		name  string
		stage speechelements.Stage
		state speechelements.State
	}{
		{"tts-generating-late", speechelements.StageSynthesis, speechelements.StateGenerating},
		{"playback-emitting-late", speechelements.StagePlayback, speechelements.StateEmitting},
		{"tts-generated-late", speechelements.StageSynthesis, speechelements.StateGenerated},
		{"playback-played-late", speechelements.StagePlayback, speechelements.StatePlayed},
	} {
		state = harness.sendAndSync(t, map[speechelements.Stage]string{
			speechelements.StageSynthesis: "tts", speechelements.StagePlayback: "playback",
		}[transition.stage], overlapTransitionEnvelope(
			transition.name, runID, utteranceID, transition.stage, transition.state,
		))
		if state.ActiveTTS != 0 || state.ActivePlayback != 0 || state.OverlapActive {
			t.Fatalf("late %s status revived released speech: %+v", transition.name, state)
		}
	}
	if len(decider.seenDecisions()) != 0 {
		t.Fatalf("release-first status reordering invoked classifier: %+v", decider.seenDecisions())
	}
	assertNoEnvelope(t, harness.output(t, "decision"))
	assertNoOverlapCancels(t, harness)
}

func TestOverlapBargeInReleaseAcceptsFailedZeroAudioPunctuationMarker(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":10,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	const (
		runID       = "failed-zero-audio-run"
		utteranceID = "failed-zero-audio-utterance"
	)
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-done", runID))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segmentation-done", runID, OutcomeCompleted,
	))
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invocation-late", runID))
	release := overlapReleaseEnvelope(
		"failed-release", runID, utteranceID, "...",
		action.Outcome{Completed: false, PlayedMS: 0, Reason: "Fish Audio returned no audio"},
	)
	state := harness.sendAndSync(t, "release", release)
	if state.ActiveTTS != 0 || state.ActivePlayback != 0 || state.OverlapActive {
		t.Fatalf("failed zero-audio release retained speech work: %+v", state)
	}
	forwarded := receive(t, harness.output(t, "safe_release"))
	boundary, ok := forwarded.Payload.(speechelements.PlaybackRelease)
	receipt := boundary.Receipt
	if !ok || receipt.Utterance.Text != "..." || receipt.Outcome.Completed ||
		receipt.Outcome.PlayedMS != 0 || receipt.Outcome.Reason == "" ||
		!slices.Contains(forwarded.CausalParents, release.ItemID) {
		t.Fatalf("failed zero-audio release pass-through = %+v / %#v",
			forwarded, forwarded.Payload)
	}
}

func TestOverlapBargeInReleaseRejectsUnprovedPunctuationMarker(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		outcome action.Outcome
	}{
		{name: "completed", text: "...", outcome: action.Outcome{Completed: true}},
		{name: "played audio", text: "...", outcome: action.Outcome{PlayedMS: 1, Reason: "failed later"}},
		{name: "missing failure", text: "...", outcome: action.Outcome{}},
		{name: "symbol rather than punctuation", text: "🙂", outcome: action.Outcome{Reason: "no audio"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &overlapBargeInRunner{
				config:             OverlapBargeInConfig{MaxUtterances: 8},
				runs:               make(map[string]*overlapRun),
				utterances:         make(map[string]*overlapUtterance),
				terminalUtterances: make(map[string]struct{}),
			}
			_, err := runner.acceptPlaybackRelease(overlapReleaseEnvelope(
				"release", "run", "utterance", test.text, test.outcome,
			))
			if err == nil {
				t.Fatal("punctuation-only release without failed zero-audio proof was accepted")
			}
		})
	}
}

func TestOverlapBargeInSafeResultBarrierRetiresSpeechlessHorizonBeforeForwarding(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":10,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	const runID = "speechless-result-reordered"
	// Model completion and invocation are deliberately delivered in the order
	// most hostile to lifecycle reconstruction: the model tombstone arrives
	// first, then invocation conservatively opens only the segmentation horizon.
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-first", runID))
	state := harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invocation-late", runID))
	if state.ActiveModels != 0 || state.ActiveSegmentations != 1 {
		t.Fatalf("reordered provisional horizon = %+v", state)
	}
	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "stream", acousticelements.SpeechStarted,
	))
	_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
	if observed.Kind != OverlapObserved {
		t.Fatalf("provisional overlap decision = %+v", observed)
	}

	result := modelResult(runID, 0, "", false, true)
	resultEnvelope := element.Envelope{
		Type: SafeModelResultType(), ItemID: "speechless-safe-result",
		SessionID: overlapBargeInSession, RunID: runID,
		CausalParents: []string{"speechless-safe-text-end"}, Payload: result,
	}
	state = harness.sendAndSync(t, "result", resultEnvelope)
	if state.ActiveModels != 0 || state.ActiveSegmentations != 0 || state.OverlapActive {
		t.Fatalf("speechless result did not retire provisional horizon: %+v", state)
	}
	forwarded := receive(t, harness.output(t, "safe_result"))
	forwardedResult, ok := forwarded.Payload.(cognitionelements.Result)
	if !ok || forwardedResult.RunID != runID || forwarded.ItemID == resultEnvelope.ItemID ||
		!containsString(forwarded.CausalParents, resultEnvelope.ItemID) ||
		!containsString(forwarded.CausalParents, "speechless-safe-text-end") {
		t.Fatalf("ordered safe-result pass-through = %+v payload %#v", forwarded, forwarded.Payload)
	}

	state = harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
		"post-result-transcript", "stream", 1, "please stop",
	))
	if state.OverlapActive || state.ClassificationOpen {
		t.Fatalf("post-result transcript revived overlap: %+v", state)
	}
	_, ignored := receiveOverlapDecision(t, harness.output(t, "decision"))
	if ignored.Kind != OverlapIgnored || len(decider.seenDecisions()) != 0 {
		t.Fatalf("post-result classification = %+v decisions=%+v",
			ignored, decider.seenDecisions())
	}
	harness.scheduler.AdvanceNS(uint64(20 * time.Millisecond))
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))
}

func TestOverlapBargeInSpeechlessResultBeforeModelOutcomeClosesBothHorizons(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":10,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	const runID = "speechless-result-before-outcome"
	state := harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invocation", runID))
	if state.ActiveModels != 1 || state.ActiveSegmentations != 1 {
		t.Fatalf("initial result barrier horizons = %+v", state)
	}
	result := modelResult(runID, 0, "", false, true)
	resultEnvelope := element.Envelope{
		Type: SafeModelResultType(), ItemID: "speechless-result-before-outcome",
		SessionID: overlapBargeInSession, RunID: runID,
		CausalParents: []string{"speechless-result-before-outcome:text-end"}, Payload: result,
	}
	state = harness.sendAndSync(t, "result", resultEnvelope)
	if state.ActiveModels != 0 || state.ActiveSegmentations != 0 || state.OverlapActive {
		t.Fatalf("final result retained active work before model outcome: %+v", state)
	}
	forwarded := receive(t, harness.output(t, "safe_result"))
	if !containsString(forwarded.CausalParents, resultEnvelope.ItemID) ||
		!containsString(forwarded.CausalParents, "speechless-result-before-outcome:text-end") {
		t.Fatalf("result barrier lost completion ancestry: %+v", forwarded)
	}

	// New input begins before the redundant model-outcome lane drains. The final
	// result must already have made both lifecycle horizons terminal.
	state = harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"new-speech", "new-stream", acousticelements.SpeechStarted,
	))
	if state.OverlapActive || state.ActiveModels != 0 || state.ActiveSegmentations != 0 {
		t.Fatalf("new speech observed phantom work before model outcome: %+v", state)
	}
	assertNoEnvelope(t, harness.output(t, "decision"))

	state = harness.sendAndSync(t, "model", overlapModelEnvelope("late-model-outcome", runID))
	if state.ActiveModels != 0 || state.ActiveSegmentations != 0 || state.OverlapActive {
		t.Fatalf("late model outcome revived completed result: %+v", state)
	}
	if len(decider.seenDecisions()) != 0 {
		t.Fatalf("completion barrier invoked overlap classifier: %+v", decider.seenDecisions())
	}
}

func TestOverlapBargeInDirectedSpeechCancelsOnlyExactIndependentAddresses(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapDirected)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":100,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	// Finish only segmentation for one run and only the model for another.
	// Prepared speech still requires segmentation cancellation after TextEnd;
	// model cancellation remains limited to the independently active model.
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke-model", "run-model"))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segment-model-done", "run-model", OutcomeCompleted,
	))
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke-segment", "run-segment"))
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-segment-done", "run-segment"))

	// Likewise, synthesis and playback are independently active and carry
	// utterance addresses unrelated to the model/segmentation run addresses.
	harness.sendAndSync(t, "tts", overlapTransitionEnvelope(
		"tts-active", "run-model", "utterance-tts", speechelements.StageSynthesis,
		speechelements.StateGenerating,
	))
	harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playback-active", "run-segment", "utterance-playback", speechelements.StagePlayback,
		speechelements.StateEmitting,
	))

	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "stream-user", acousticelements.SpeechStarted,
	))
	observedEnvelope, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
	if observed.Kind != OverlapObserved || observed.Trigger != "acoustic_start" ||
		observed.StreamID != "stream-user" || observedEnvelope.ItemID == "" {
		t.Fatalf("observed overlap = %+v / %+v", observedEnvelope, observed)
	}

	harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
		"directed-revision", "stream-user", 7, "please stop",
	))
	decisionEnvelope, decision := receiveOverlapDecision(t, harness.output(t, "decision"))
	if decision.Kind != OverlapCanceled || decision.Trigger != "semantic_transcript" ||
		decision.Evidence != coreinteraction.OverlapDirected || decision.SourceRevision != 7 ||
		decision.EvidenceItemID != "directed-revision" ||
		!reflect.DeepEqual(decision.ActiveRunIDs, []string{"run-model", "run-segment"}) ||
		!reflect.DeepEqual(decision.ActiveUtteranceIDs,
			[]string{"utterance-playback", "utterance-tts"}) ||
		decision.ModelCancels != 1 || decision.SegmentationCancels != 2 ||
		decision.TTSCancels != 1 || decision.PlaybackCancels != 1 {
		t.Fatalf("directed overlap decision = %+v", decision)
	}

	assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"),
		decisionEnvelope.ItemID, "run-model")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"),
		decisionEnvelope.ItemID, "run-model")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"),
		decisionEnvelope.ItemID, "run-segment")
	assertExactOverlapSpeechCancel(t, harness.output(t, "tts_cancel"),
		decisionEnvelope.ItemID, "utterance-tts")
	assertExactOverlapSpeechCancel(t, harness.output(t, "playback_cancel"),
		decisionEnvelope.ItemID, "utterance-playback")

	for _, name := range []string{
		"model_cancel", "segmentation_cancel", "tts_cancel", "playback_cancel",
	} {
		assertNoEnvelope(t, harness.output(t, name))
	}
}

func TestOverlapBargeInKeepsSpeakingForExplicitNonDirectedClassifications(t *testing.T) {
	for _, testCase := range []struct {
		evidence coreinteraction.OverlapEvidence
		text     string
	}{
		{evidence: coreinteraction.OverlapBackchannel, text: "mm-hm"},
		{evidence: coreinteraction.OverlapSide, text: "non-directed speech"},
	} {
		t.Run(string(testCase.evidence), func(t *testing.T) {
			decider := newOverlapTestDecider(testCase.evidence)
			harness := mountOverlapBargeIn(t,
				`{"decider":"overlap-test","hold_ms":25,"unclassified":"cancel"}`, decider)
			defer harness.stop(t)

			harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", "run"))
			harness.sendAndSync(t, "activity", overlapActivityEnvelope(
				"speech-start", "stream", acousticelements.SpeechStarted,
			))
			_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
			if observed.Kind != OverlapObserved {
				t.Fatalf("initial overlap decision = %+v", observed)
			}
			harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
				"classified", "stream", 1, testCase.text,
			))
			_, classified := receiveOverlapDecision(t, harness.output(t, "decision"))
			if classified.Kind != OverlapClassified || classified.Trigger != "semantic_transcript" ||
				classified.Evidence != testCase.evidence || classified.SourceRevision != 1 {
				t.Fatalf("non-directed overlap classification = %+v", classified)
			}
			assertNoOverlapCancels(t, harness)

			harness.scheduler.AdvanceNS(uint64(25 * time.Millisecond))
			_, kept := receiveOverlapDecision(t, harness.output(t, "decision"))
			if kept.Kind != OverlapKept || kept.Trigger != "semantic_deadline" ||
				kept.Evidence != testCase.evidence {
				t.Fatalf("non-directed keep-speaking decision = %+v", kept)
			}
			assertNoOverlapCancels(t, harness)
			assertNoEnvelope(t, harness.output(t, "decision"))
		})
	}
}

func TestOverlapBargeInSuppliesConnectedAgentSpeechToClassification(t *testing.T) {
	decider := newOverlapTestDecider(coreinteraction.OverlapSide)
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":100,"unclassified":"cancel"}`, decider)
	defer harness.stop(t)

	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", "run"))
	harness.sendAndSync(t, "speech", overlapSpeechEnvelope(
		"prepared", "run", "utterance", "I cannot turn on the study lamp.",
	))
	harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playing", "run", "utterance", speechelements.StagePlayback,
		speechelements.StateEmitting,
	))
	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "stream", acousticelements.SpeechStarted,
	))
	_, _ = receiveOverlapDecision(t, harness.output(t, "decision"))
	harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
		"revision", "stream", 1, "Oh, it's starting.",
	))
	_, classified := receiveOverlapDecision(t, harness.output(t, "decision"))
	if classified.Kind != OverlapClassified || classified.Evidence != coreinteraction.OverlapSide {
		t.Fatalf("contextual overlap classification = %+v", classified)
	}
	decisions := decider.seenDecisions()
	if len(decisions) != 1 ||
		!strings.Contains(decisions[0].Evidence, "I cannot turn on the study lamp.") ||
		!strings.Contains(decisions[0].Evidence, "Oh, it's starting.") {
		t.Fatalf("overlap classifier evidence = %+v", decisions)
	}
}

func TestOverlapBargeInShortOverlapDoesNotCancelAfterSpeechStops(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":20,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", "run"))
	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "short-stream", acousticelements.SpeechStarted,
	))
	_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
	if observed.Kind != OverlapObserved {
		t.Fatalf("short overlap start = %+v", observed)
	}
	harness.scheduler.AdvanceNS(uint64(19 * time.Millisecond))
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))

	state := harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-stop", "short-stream", acousticelements.SpeechStopped,
	))
	if state.UserSpeaking || state.OverlapActive || state.CancelIssued {
		t.Fatalf("short overlap terminal state = %+v", state)
	}
	harness.scheduler.AdvanceNS(uint64(100 * time.Millisecond))
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))
}

func TestOverlapBargeInAppliesExplicitDeadlineFallback(t *testing.T) {
	tests := []struct {
		name       string
		fallback   OverlapFallback
		wantKind   OverlapDecisionKind
		wantCancel bool
	}{
		{name: "cancel", fallback: OverlapFallbackCancel, wantKind: OverlapCanceled, wantCancel: true},
		{name: "keep speaking", fallback: OverlapFallbackKeep, wantKind: OverlapKept},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			harness := mountOverlapBargeIn(t, fmt.Sprintf(
				`{"hold_ms":10,"unclassified":%q}`, testCase.fallback,
			), nil)
			defer harness.stop(t)

			harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", "run"))
			harness.sendAndSync(t, "activity", overlapActivityEnvelope(
				"speech-start", "stream", acousticelements.SpeechStarted,
			))
			_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
			if observed.Kind != OverlapObserved {
				t.Fatalf("fallback overlap start = %+v", observed)
			}

			harness.scheduler.AdvanceNS(uint64(10 * time.Millisecond))
			decisionEnvelope, decision := receiveOverlapDecision(t, harness.output(t, "decision"))
			if decision.Kind != testCase.wantKind || decision.Trigger != "hold_timeout" ||
				decision.Policy != "sustained-10ms/"+string(testCase.fallback) {
				t.Fatalf("deadline fallback decision = %+v", decision)
			}
			if !testCase.wantCancel {
				assertNoOverlapCancels(t, harness)
				return
			}
			if decision.ModelCancels != 1 || decision.SegmentationCancels != 1 ||
				decision.TTSCancels != 0 || decision.PlaybackCancels != 0 {
				t.Fatalf("cancel fallback counts = %+v", decision)
			}
			assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"),
				decisionEnvelope.ItemID, "run")
			assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"),
				decisionEnvelope.ItemID, "run")
			assertNoEnvelope(t, harness.output(t, "tts_cancel"))
			assertNoEnvelope(t, harness.output(t, "playback_cancel"))
		})
	}
}

func TestOverlapBargeInDiscardsStaleClassification(t *testing.T) {
	firstRelease := make(chan struct{})
	secondRelease := make(chan struct{})
	decider := newOverlapTestDecider(
		coreinteraction.OverlapDirected,
		coreinteraction.OverlapBackchannel,
	)
	decider.releases = []<-chan struct{}{firstRelease, secondRelease}
	decider.entered = make(chan int, 2)
	decider.ignoreCancellation = true
	harness := mountOverlapBargeIn(t,
		`{"decider":"overlap-test","hold_ms":100,"unclassified":"cancel"}`, decider)
	defer func() {
		closeIfOpen(firstRelease)
		closeIfOpen(secondRelease)
		harness.stop(t)
	}()

	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke", "run"))
	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "stream", acousticelements.SpeechStarted,
	))
	_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
	if observed.Kind != OverlapObserved {
		t.Fatalf("stale overlap start = %+v", observed)
	}

	harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
		"revision-one", "stream", 1, "stop",
	))
	if call := receiveOverlapCall(t, decider.entered); call != 0 {
		t.Fatalf("first classifier call = %d", call)
	}
	harness.sendAndSync(t, "transcript", overlapTranscriptEnvelope(
		"revision-two", "stream", 2, "mm-hm",
	))
	// Provider calls are serialized. Returning the now-stale first result
	// retires its worker and starts only the single newest pending revision.
	close(firstRelease)
	if call := receiveOverlapCall(t, decider.entered); call != 1 {
		t.Fatalf("second classifier call = %d", call)
	}
	close(secondRelease)
	_, classified := receiveOverlapDecision(t, harness.output(t, "decision"))
	if classified.Kind != OverlapClassified ||
		classified.Evidence != coreinteraction.OverlapBackchannel ||
		classified.SourceRevision != 2 || classified.EvidenceItemID != "revision-two" {
		t.Fatalf("newest classification = %+v", classified)
	}

	// The canceled first provider call deliberately returns directed speech
	// after its revision was superseded. Its stale generation must not cancel
	// any path or publish a policy verdict.
	assertNoOverlapCancels(t, harness)
	harness.scheduler.AdvanceNS(uint64(100 * time.Millisecond))
	_, kept := receiveOverlapDecision(t, harness.output(t, "decision"))
	if kept.Kind != OverlapKept || kept.Trigger != "semantic_deadline" ||
		kept.Evidence != coreinteraction.OverlapBackchannel {
		t.Fatalf("newest classification deadline = %+v", kept)
	}
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))
}

func TestOverlapBargeInDoesNotResurrectTerminalRunWhenLifecycleLanesReorder(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	// The model and segment terminal lanes may drain before the invocation
	// outcome lane even though all three descend from the same trigger. Their
	// tombstones must meet the later invocation instead of being overwritten by
	// a guessed active lifecycle.
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-terminal", "run-reordered"))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segment-terminal", "run-reordered", OutcomeCompleted,
	))
	state := harness.sendAndSync(t, "invocation", overlapInvocationEnvelope(
		"invocation-late", "run-reordered",
	))
	if state.ActiveModels != 0 || state.ActiveSegmentations != 0 {
		t.Fatalf("reordered terminal run was resurrected: %+v", state)
	}

	state = harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-after-terminal", "stream-after-terminal", acousticelements.SpeechStarted,
	))
	if state.OverlapActive || state.CancelIssued {
		t.Fatalf("terminal-only history became actionable overlap: %+v", state)
	}
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))
}

func TestOverlapBargeInCancelsLateWorkForTheSameActiveSpeech(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke-first", "run-first"))
	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"speech-start", "stream", acousticelements.SpeechStarted,
	))
	_, observed := receiveOverlapDecision(t, harness.output(t, "decision"))
	if observed.Kind != OverlapObserved {
		t.Fatalf("initial overlap = %+v", observed)
	}
	harness.scheduler.AdvanceNS(uint64(10 * time.Millisecond))
	firstEnvelope, first := receiveOverlapDecision(t, harness.output(t, "decision"))
	if first.Kind != OverlapCanceled || first.ModelCancels != 1 || first.SegmentationCancels != 1 {
		t.Fatalf("initial cancellation = %+v", first)
	}
	assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"), firstEnvelope.ItemID, "run-first")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"), firstEnvelope.ItemID, "run-first")
	stateEnvelope := receive(t, harness.output(t, "state"))
	if state := stateEnvelope.Payload.(OverlapState); !state.CancelIssued {
		t.Fatalf("post-cancel state = %+v", state)
	}
	_ = receive(t, harness.output(t, "agent_output"))

	// Retire the initially visible work. The user is still speaking, so a
	// non-cooperative or separately queued run that arrives afterward must get
	// its own cancellation rather than escaping behind the first decision.
	harness.sendAndSync(t, "model", overlapModelEnvelope("model-first-done", "run-first"))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segment-first-done", "run-first", OutcomeCanceled,
	))
	harness.sendAndSync(t, "invocation", overlapInvocationEnvelope("invoke-late", "run-late"))
	lateRunEnvelope, lateRun := receiveOverlapDecision(t, harness.output(t, "decision"))
	if lateRun.Kind != OverlapCanceled || lateRun.Trigger != "late_invocation" ||
		!reflect.DeepEqual(lateRun.ActiveRunIDs, []string{"run-late"}) ||
		lateRun.ModelCancels != 1 || lateRun.SegmentationCancels != 1 {
		t.Fatalf("late run cancellation = %+v", lateRun)
	}
	assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"), lateRunEnvelope.ItemID, "run-late")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"), lateRunEnvelope.ItemID, "run-late")

	harness.sendAndSync(t, "tts", overlapTransitionEnvelope(
		"tts-late", "run-late", "utterance-late", speechelements.StageSynthesis,
		speechelements.StateGenerating,
	))
	lateTTSEnvelope, lateTTS := receiveOverlapDecision(t, harness.output(t, "decision"))
	if lateTTS.Kind != OverlapCanceled || lateTTS.Trigger != "late_speech_lifecycle" ||
		lateTTS.TTSCancels != 1 || lateTTS.PlaybackCancels != 0 ||
		!reflect.DeepEqual(lateTTS.ActiveUtteranceIDs, []string{"utterance-late"}) {
		t.Fatalf("late TTS cancellation = %+v", lateTTS)
	}
	assertExactOverlapSpeechCancel(t, harness.output(t, "tts_cancel"),
		lateTTSEnvelope.ItemID, "utterance-late")

	harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playback-late", "run-late", "utterance-late", speechelements.StagePlayback,
		speechelements.StateEmitting,
	))
	latePlaybackEnvelope, latePlayback := receiveOverlapDecision(t, harness.output(t, "decision"))
	if latePlayback.Kind != OverlapCanceled || latePlayback.Trigger != "late_speech_lifecycle" ||
		latePlayback.TTSCancels != 0 || latePlayback.PlaybackCancels != 1 ||
		!reflect.DeepEqual(latePlayback.ActiveUtteranceIDs, []string{"utterance-late"}) {
		t.Fatalf("late playback cancellation = %+v", latePlayback)
	}
	assertExactOverlapSpeechCancel(t, harness.output(t, "playback_cancel"),
		latePlaybackEnvelope.ItemID, "utterance-late")
	assertNoOverlapCancels(t, harness)
}

func TestOverlapBargeInProtectsDeliberateSameStreamUntilExplicitStop(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"activity-start", "stream-a", acousticelements.SpeechStarted,
	))
	state := harness.sendAndSync(t, "invocation", overlapCommittedInvocationEnvelope(
		"invocation-a", "run-a", "stream-a", 1, coreinteraction.Choice{Speak: true}, true,
	))
	if state.OverlapActive || state.ActiveModels != 1 || state.ActiveSegmentations != 1 {
		t.Fatalf("deliberate same-stream invocation armed generic overlap: %+v", state)
	}

	harness.sendAndSync(t, "semantic", overlapSemanticEnvelope(
		"semantic-listen", "stream-a", 2, coreinteraction.Choice{},
	))
	assertNoOverlapCancels(t, harness)

	harness.sendAndSync(t, "semantic", overlapSemanticEnvelope(
		"semantic-stop", "stream-a", 3, coreinteraction.Choice{Speaking: true, Stop: true},
	))
	decisionEnvelope, decision := receiveOverlapDecision(t, harness.output(t, "decision"))
	if decision.Kind != OverlapCanceled || decision.Trigger != "semantic_revision" ||
		decision.SourceRevision != 3 || !reflect.DeepEqual(decision.ActiveRunIDs, []string{"run-a"}) ||
		decision.ModelCancels != 1 || decision.SegmentationCancels != 1 {
		t.Fatalf("semantic stop cancellation = %+v", decision)
	}
	assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"), decisionEnvelope.ItemID, "run-a")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"), decisionEnvelope.ItemID, "run-a")
}

func TestOverlapBargeInSemanticKeepSpeakingClosesTheAcousticDeadline(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	harness.sendAndSync(t, "invocation", overlapCommittedInvocationEnvelope(
		"invocation", "run", "source-stream", 1, coreinteraction.Choice{Speak: true}, true,
	))
	state := harness.sendAndSync(t, "activity", overlapActivityEnvelope(
		"activity-start", "continuation-stream", acousticelements.SpeechStarted,
	))
	if state.OverlapActive || state.CancelIssued {
		t.Fatalf("deliberate output armed acoustic cancellation before semantic evidence: %+v", state)
	}
	harness.scheduler.AdvanceNS(uint64(50 * time.Millisecond))
	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))

	state = harness.sendAndSync(t, "semantic", overlapSemanticEnvelope(
		"semantic-keep", "continuation-stream", 1, coreinteraction.Choice{Speaking: true},
	))
	_, kept := receiveOverlapDecision(t, harness.output(t, "decision"))
	if kept.Kind != OverlapKept || kept.Trigger != "semantic_revision" ||
		kept.SourceRevision != 1 || state.OverlapActive || state.ClassificationOpen ||
		state.Kept != 1 || state.CancelIssued ||
		!reflect.DeepEqual(state.AgentOutput.ProtectedStreams,
			[]string{"continuation-stream", "source-stream"}) {
		t.Fatalf("semantic keep decision=%+v state=%+v", kept, state)
	}

	assertNoOverlapCancels(t, harness)
	assertNoEnvelope(t, harness.output(t, "decision"))
}

func TestOverlapBargeInPublishesExactVoiceOutputLifecycle(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	state := harness.sendAndSync(t, "invocation", overlapCommittedInvocationEnvelope(
		"invocation", "run", "stream", 1, coreinteraction.Choice{Speak: true}, true,
	))
	if !state.AgentOutput.Active || !state.AgentOutput.Queued || state.AgentOutput.Audible ||
		state.AgentOutput.Saying != "" ||
		state.AgentOutput.InFlight != "voice output active: model=1, segmentation=1, synthesis=0, playback=0" ||
		!reflect.DeepEqual(state.AgentOutput.ProtectedStreams, []string{"stream"}) {
		t.Fatalf("admitted voice output = %+v", state.AgentOutput)
	}

	const text = "Actually, the deadline is the third."
	harness.sendAndSync(t, "speech", overlapSpeechEnvelope("speech", "run", "utterance", text))
	state = harness.sendAndSync(t, "tts", overlapTransitionEnvelope(
		"tts-generating", "run", "utterance",
		speechelements.StageSynthesis, speechelements.StateGenerating,
	))
	if !state.AgentOutput.Active || !state.AgentOutput.Queued || state.AgentOutput.Audible ||
		state.AgentOutput.Saying != text ||
		!strings.Contains(state.AgentOutput.InFlight, "synthesis=1") ||
		!reflect.DeepEqual(state.AgentOutput.ProtectedStreams, []string{"stream"}) {
		t.Fatalf("synthesizing voice output = %+v", state.AgentOutput)
	}

	state = harness.sendAndSync(t, "playback", overlapTransitionEnvelope(
		"playback-emitting", "run", "utterance",
		speechelements.StagePlayback, speechelements.StateEmitting,
	))
	if !state.AgentOutput.Active || state.AgentOutput.Queued || !state.AgentOutput.Audible ||
		state.AgentOutput.Saying != text ||
		!strings.Contains(state.AgentOutput.InFlight, "playback=1") ||
		!reflect.DeepEqual(state.AgentOutput.ProtectedStreams, []string{"stream"}) {
		t.Fatalf("audible voice output = %+v", state.AgentOutput)
	}

	harness.sendAndSync(t, "model", overlapModelEnvelope("model-done", "run"))
	harness.sendAndSync(t, "segmentation", overlapSegmentationEnvelope(
		"segmentation-done", "run", OutcomeCompleted,
	))
	state = harness.sendAndSync(t, "release", overlapReleaseEnvelope(
		"released", "run", "utterance", text,
		action.Outcome{Completed: true, PlayedMS: 40},
	))
	if state.AgentOutput.Active || state.AgentOutput.Queued || state.AgentOutput.Audible ||
		state.AgentOutput.Saying != "" || state.AgentOutput.InFlight != "" ||
		len(state.AgentOutput.ProtectedStreams) != 0 {
		t.Fatalf("terminal voice output = %+v", state.AgentOutput)
	}
	_ = receive(t, harness.output(t, "safe_release"))
}

func TestOverlapBargeInAppliesDecisionBeforeReorderedInvocation(t *testing.T) {
	harness := mountOverlapBargeIn(t, `{"hold_ms":10,"unclassified":"cancel"}`, nil)
	defer harness.stop(t)

	harness.sendAndSync(t, "semantic", overlapSemanticEnvelope(
		"semantic-newer", "stream-a", 2, coreinteraction.Choice{},
	))
	harness.sendAndSync(t, "invocation", overlapCommittedInvocationEnvelope(
		"invocation-older", "run-older", "stream-a", 1, coreinteraction.Choice{Speak: true}, false,
	))
	decisionEnvelope, decision := receiveOverlapDecision(t, harness.output(t, "decision"))
	if decision.Kind != OverlapCanceled || decision.Trigger != "semantic_revision" ||
		!reflect.DeepEqual(decision.ActiveRunIDs, []string{"run-older"}) {
		t.Fatalf("decision-before-invocation cancellation = %+v", decision)
	}
	assertExactOverlapRunCancel(t, harness.output(t, "model_cancel"), decisionEnvelope.ItemID, "run-older")
	assertExactOverlapRunCancel(t, harness.output(t, "segmentation_cancel"), decisionEnvelope.ItemID, "run-older")
}

func mountOverlapBargeIn(
	t *testing.T, config string, decider *overlapTestDecider,
) *overlapBargeInHarness {
	t.Helper()
	scheduler := clock.NewManual(uint64(time.Second))
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(OverlapBargeInSchedulerService, scheduler); err != nil {
		t.Fatal(err)
	}
	if decider != nil {
		registry := policyelements.NewSemanticDeciderRegistry()
		if err := registry.Register("overlap-test", decider.descriptor, func() (
			policyelements.SemanticDecider, error,
		) {
			return decider, nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := services.Set(policyelements.SemanticDeciderRegistryService, registry); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compileSource(t, "overlap-barge-in-test.ortg", []byte(overlapBargeInGraph)),
		Registry: testRegistry(t), Services: services, Now: scheduler.NowNS,
		Values: map[string]json.RawMessage{"overlap": json.RawMessage(config)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := &overlapBargeInHarness{
		mounted: mounted, scheduler: scheduler, done: done, cancel: cancel,
	}
	resolvedEnvelope := receive(t, harness.output(t, "resolved"))
	resolved, ok := resolvedEnvelope.Payload.(OverlapPolicyResolution)
	if !ok || resolved.HoldMS < 1 || resolved.Unclassified == "" {
		t.Fatalf("overlap startup resolution = %#v", resolvedEnvelope.Payload)
	}
	startupEnvelope := receive(t, harness.output(t, "state"))
	startup, ok := startupEnvelope.Payload.(OverlapState)
	if !ok || startup.UserSpeaking || startup.OverlapActive || startup.CancelIssued {
		t.Fatalf("overlap startup state = %#v", startupEnvelope.Payload)
	}
	startupOutput := receive(t, harness.output(t, "agent_output"))
	if output, ok := startupOutput.Payload.(coreinteraction.AgentOutput); !ok ||
		output.Revision != 1 || output.Active || output.Queued || output.Audible {
		t.Fatalf("overlap startup agent output = %#v", startupOutput.Payload)
	}
	live := mounted.Live().Nodes["overlap"].Resolution
	wantCapabilities := 0
	if decider != nil {
		wantCapabilities = 1
	}
	if live == nil || string(live.RuntimeEvidence) != "live" ||
		string(live.CapabilitiesEvidence) != "live" ||
		live.Runtime.ID != overlapBargeInRuntimeID ||
		live.Runtime.Revision != overlapBargeInRuntimeRevision ||
		len(live.Capabilities) != wantCapabilities {
		t.Fatalf("overlap live resolution = %+v, want %d capabilities", live, wantCapabilities)
	}
	if decider != nil {
		capability := live.Capabilities[0]
		if capability.Name != "interaction.overlap-classification" ||
			capability.Contract != "openrealtime.interaction/OverlapClassifier-v1" ||
			capability.Provider.ID != "interaction-model://overlap-test" || capability.Adapter == nil {
			t.Fatalf("overlap classifier capability = %+v", capability)
		}
	}
	return harness
}

func (harness *overlapBargeInHarness) stop(t *testing.T) {
	t.Helper()
	stopInteractionGraph(t, harness.done, harness.cancel)
}

func (harness *overlapBargeInHarness) input(t *testing.T, name string) element.OutputPort {
	t.Helper()
	return ingress(t, harness.mounted, name)
}

func (harness *overlapBargeInHarness) output(t *testing.T, name string) element.InputPort {
	t.Helper()
	return egress(t, harness.mounted, name)
}

func (harness *overlapBargeInHarness) sendAndSync(
	t *testing.T, name string, envelope element.Envelope,
) OverlapState {
	t.Helper()
	send(t, harness.input(t, name), envelope)
	stateEnvelope := receive(t, harness.output(t, "state"))
	state, ok := stateEnvelope.Payload.(OverlapState)
	if !ok {
		t.Fatalf("overlap state payload = %T", stateEnvelope.Payload)
	}
	if !slices.Contains(stateEnvelope.CausalParents, envelope.ItemID) {
		t.Fatalf("state %+v does not descend from %q", stateEnvelope, envelope.ItemID)
	}
	outputEnvelope := receive(t, harness.output(t, "agent_output"))
	output, ok := outputEnvelope.Payload.(coreinteraction.AgentOutput)
	if !ok || !reflect.DeepEqual(output, state.AgentOutput) {
		t.Fatalf("agent output payload = %#v, state = %#v", outputEnvelope.Payload, state.AgentOutput)
	}
	return state
}

type overlapTestDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor

	mu                 sync.Mutex
	answers            []coreinteraction.OverlapEvidence
	releases           []<-chan struct{}
	entered            chan int
	ignoreCancellation bool
	calls              int
	decisions          []coreinteraction.Decision
}

func newOverlapTestDecider(answers ...coreinteraction.OverlapEvidence) *overlapTestDecider {
	return &overlapTestDecider{
		descriptor: policyelements.SemanticDeciderDescriptor{
			Provider: "overlap-test", Model: "enumerated", Protocol: "in-process", Revision: "1",
			ConfigurationDigest: "sha256:" + strings.Repeat("a", 64), DecisionTimeoutMS: 5_000,
		},
		answers: slices.Clone(answers),
	}
}

func (*overlapTestDecider) Name() string { return "overlap-test" }

func (decider *overlapTestDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *overlapTestDecider) Decide(
	ctx context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	// The element tests script the semantic classification returned by the
	// model-backed OverlapClassifier, not each internal provider turn used to
	// enforce that classification's label contract. A scripted backchannel
	// therefore satisfies the classifier's separate binary validation without
	// consuming another classification answer or release gate. Keeping those
	// layers distinct is important for the stale-revision test: its gates model
	// two concurrent transcript classifications, not an implementation detail
	// of validating one result.
	if index := slices.Index(decision.Options, "valid_backchannel"); index >= 0 {
		return coreinteraction.Outcome{Index: index, Option: decision.Options[index]}, nil
	}

	decider.mu.Lock()
	call := decider.calls
	decider.calls++
	decider.decisions = append(decider.decisions, decision)
	answer := coreinteraction.OverlapAmbiguous
	if len(decider.answers) != 0 {
		answer = decider.answers[min(call, len(decider.answers)-1)]
	}
	var release <-chan struct{}
	if call < len(decider.releases) {
		release = decider.releases[call]
	}
	entered := decider.entered
	ignoreCancellation := decider.ignoreCancellation
	decider.mu.Unlock()

	if entered != nil {
		select {
		case entered <- call:
		case <-ctx.Done():
			return coreinteraction.Outcome{}, ctx.Err()
		}
	}
	if release != nil {
		if ignoreCancellation {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return coreinteraction.Outcome{}, ctx.Err()
			}
		}
	}
	index := slices.Index(decision.Options, string(answer))
	if index < 0 {
		return coreinteraction.Outcome{}, fmt.Errorf("overlap option %q is unavailable", answer)
	}
	return coreinteraction.Outcome{Index: index, Option: string(answer)}, nil
}

func (decider *overlapTestDecider) seenDecisions() []coreinteraction.Decision {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	return slices.Clone(decider.decisions)
}

func overlapInvocationEnvelope(itemID, runID string) element.Envelope {
	return element.Envelope{
		Type: policyelements.SessionInvocationOutcomeType(), ItemID: itemID,
		SessionID: overlapBargeInSession, RunID: runID, CancellationScope: runID,
		Payload: policyelements.SessionInvocationOutcome{
			Kind: policyelements.SessionInvocationEmitted, Operation: "create", GenerationID: runID,
			Role: "assistant",
		},
	}
}

func overlapCommittedInvocationEnvelope(
	itemID, runID, streamID string, sourceRevision uint64, choice coreinteraction.Choice, spokeOver bool,
) element.Envelope {
	envelope := overlapInvocationEnvelope(itemID, runID)
	envelope.Payload = policyelements.SessionInvocationOutcome{
		Kind: policyelements.SessionInvocationEmitted, Operation: "committed",
		GenerationID: runID, Role: "foreground", StreamID: streamID,
		SourceRevision: sourceRevision, ObservationRevision: sourceRevision,
		Choice: &choice, SpokeOver: spokeOver,
	}
	return envelope
}

func overlapSemanticEnvelope(
	itemID, streamID string, sourceRevision uint64, choice coreinteraction.Choice,
) element.Envelope {
	return element.Envelope{
		Type: policyelements.SemanticDecisionType(), ItemID: itemID,
		SessionID: overlapBargeInSession, SourceID: streamID, CancellationScope: streamID,
		Payload: policyelements.SemanticDecision{
			Operation: "committed", Choice: choice, Policy: "transcript-policy",
			EvidenceItemID: "evidence-" + itemID, StreamID: streamID,
			SourceRevision: sourceRevision, ContextVersion: sourceRevision,
		},
	}
}

func overlapModelEnvelope(itemID, runID string) element.Envelope {
	return element.Envelope{
		Type: cognitionelements.OutcomeType(), ItemID: itemID,
		SessionID: overlapBargeInSession, RunID: runID, CancellationScope: runID,
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeSucceeded, Operation: "generate", RunID: runID,
		},
	}
}

func overlapSegmentationEnvelope(
	itemID, runID string, kind OutcomeKind,
) element.Envelope {
	return element.Envelope{
		Type: SegmentationOutcomeType(), ItemID: itemID,
		SessionID: overlapBargeInSession, RunID: runID, CancellationScope: runID,
		Payload: SegmentationOutcome{Kind: kind, RunID: runID},
	}
}

func overlapTransitionEnvelope(
	itemID, runID, utteranceID string, stage speechelements.Stage, state speechelements.State,
) element.Envelope {
	return element.Envelope{
		Type: speechelements.TransitionType(), ItemID: itemID,
		SessionID: overlapBargeInSession, SourceID: utteranceID,
		RunID: runID, CancellationScope: utteranceID,
		Payload: speechelements.Transition{UtteranceID: utteranceID, Stage: stage, State: state},
	}
}

func overlapReleaseEnvelope(
	itemID, runID, utteranceID, text string, outcome action.Outcome,
) element.Envelope {
	return element.Envelope{
		Type: speechelements.PlaybackReceiptType(), ItemID: itemID,
		SessionID: overlapBargeInSession, SourceID: utteranceID,
		RunID: runID, CancellationScope: utteranceID,
		Payload: speechelements.PlaybackReceipt{
			Kind: speechelements.PlaybackReleased, Sequence: 6,
			Utterance: action.Utterance{ID: utteranceID, Text: text}, Outcome: outcome,
		},
	}
}

func overlapSpeechEnvelope(itemID, runID, utteranceID, text string) element.Envelope {
	return element.Envelope{
		Type: speechelements.TextSegmentType(), ItemID: itemID,
		SessionID: overlapBargeInSession, RunID: runID, CancellationScope: utteranceID,
		Payload: speechelements.TextSegment{ID: utteranceID, Text: text},
	}
}

func overlapActivityEnvelope(
	itemID, streamID string, kind acousticelements.SpeechActivityKind,
) element.Envelope {
	return element.Envelope{
		Type: acousticelements.ActivityType(), ItemID: itemID,
		SessionID: overlapBargeInSession, SourceID: streamID, CancellationScope: streamID,
		Payload: acousticelements.SpeechActivity{
			Kind: kind, StreamID: streamID, Source: "microphone", SampleRateHz: 16_000,
		},
	}
}

func overlapTranscriptEnvelope(
	itemID, streamID string, revision uint64, text string,
) element.Envelope {
	return element.Envelope{
		Type: perceptionelements.ObservationType(), ItemID: itemID,
		SessionID: overlapBargeInSession, SourceID: streamID, CancellationScope: streamID,
		Payload: coreperception.Observation{
			Text: text, StableText: text, Observer: "asr", Source: "microphone",
			Authority: trajectory.AuthorityUser, Revision: revision,
			Supersedes: revision - 1, Provisional: true,
		},
	}
}

func receiveOverlapDecision(
	t *testing.T, input element.InputPort,
) (element.Envelope, OverlapDecision) {
	t.Helper()
	envelope := receive(t, input)
	decision, ok := envelope.Payload.(OverlapDecision)
	if !ok {
		t.Fatalf("overlap decision payload = %T", envelope.Payload)
	}
	return envelope, decision
}

func assertExactOverlapRunCancel(
	t *testing.T, input element.InputPort, decisionID, runID string,
) {
	t.Helper()
	envelope := receive(t, input)
	request, ok := envelope.Payload.(cognitionelements.Cancel)
	if !ok || request.RunID != runID || strings.TrimSpace(request.Reason) == "" ||
		envelope.RunID != runID || envelope.CancellationScope != runID ||
		!envelope.Type.Equal(cognitionelements.CancelType()) ||
		!slices.Contains(envelope.CausalParents, decisionID) {
		t.Fatalf("run cancellation for %q = %+v / %#v", runID, envelope, envelope.Payload)
	}
}

func assertExactOverlapSpeechCancel(
	t *testing.T, input element.InputPort, decisionID, utteranceID string,
) {
	t.Helper()
	envelope := receive(t, input)
	request, ok := envelope.Payload.(speechelements.Cancel)
	if !ok || request.UtteranceID != utteranceID || strings.TrimSpace(request.Reason) == "" ||
		envelope.SourceID != utteranceID || envelope.CancellationScope != utteranceID ||
		!envelope.Type.Equal(speechelements.CancelType()) ||
		!slices.Contains(envelope.CausalParents, decisionID) {
		t.Fatalf("speech cancellation for %q = %+v / %#v",
			utteranceID, envelope, envelope.Payload)
	}
}

func assertNoOverlapCancels(t *testing.T, harness *overlapBargeInHarness) {
	t.Helper()
	for _, name := range []string{
		"model_cancel", "segmentation_cancel", "tts_cancel", "playback_cancel",
	} {
		assertNoEnvelope(t, harness.output(t, name))
	}
}

func receiveOverlapCall(t *testing.T, calls <-chan int) int {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("overlap classifier was not called")
		return 0
	}
}

func closeIfOpen(channel chan struct{}) {
	select {
	case <-channel:
	default:
		close(channel)
	}
}

var _ policyelements.SemanticDecider = (*overlapTestDecider)(nil)
