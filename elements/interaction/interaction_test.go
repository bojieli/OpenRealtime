package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	flowelements "github.com/bojieli/OpenRealtime/elements/flow"
	"github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const segmentGraph = `graph segment_test {
    interaction.SegmentPreparedText :: segment;
    input text = segment.text;
    input terminal = segment.terminal;
    input timeout = segment.timeout;
    input cancel = segment.cancel;
    output segments = segment.segments;
    output model_cancel = segment.model_cancel;
    output speech_cancel = segment.speech_cancel;
    output outcome = segment.outcome;
}
`

const arbiterGraph = `graph arbiter_test {
    interaction.SpeechArbiter :: arbiter;
    input fast_text = arbiter.text;
    input slow_text = arbiter.text;
    input fast_terminal = arbiter.terminal;
    input slow_terminal = arbiter.terminal;
    input selection = arbiter.selection;
    input timeout = arbiter.timeout;
    input cancel = arbiter.cancel;
    output selected = arbiter.selected;
    output cancel_upstream = arbiter.cancel_upstream;
    output outcome = arbiter.outcome;
}
`

const commitGraph = `graph model_commit_test {
    interaction.ModelResultCommit :: commit;
    state.TrajectoryStore :: store;
    commit.append -> store.append;
    store.committed -> commit.committed;
    store.rejected -> commit.rejected;
    input result = commit.result;
    output outcome = commit.outcome;
    output snapshot = store.snapshot;
}
`

const commitAdapterGraph = `graph model_commit_adapter_test {
    interaction.ModelResultCommit :: commit;
    input result = commit.result;
    input committed = commit.committed;
    input rejected = commit.rejected;
    output append = commit.append;
    output outcome = commit.outcome;
}
`

func TestInteractionDescriptorsExposePolicyWithoutModelRoles(t *testing.T) {
	for _, descriptor := range Descriptors() {
		if err := descriptor.Validate(); err != nil {
			t.Fatalf("%s: %v", descriptor.Name, err)
		}
		encoded, err := json.Marshal(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"fast", "slow", "speech_authority"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("descriptor %s embeds role policy %q: %s", descriptor.Name, forbidden, encoded)
			}
		}
	}
	arbiter := SpeechArbiterDescriptor()
	selected, found := arbiter.Port("selected")
	if !found || !selected.Type.Equal(cognitionelements.PreparedTextType()) {
		t.Fatalf("selected port = %+v", selected)
	}
	text, _ := arbiter.Port("text")
	terminal, _ := arbiter.Port("terminal")
	if text.Cardinality != element.Variadic || terminal.Cardinality != element.Variadic {
		t.Fatalf("arbiter stream ports = %+v / %+v", text, terminal)
	}
	segmentTerminal, _ := SegmentPreparedTextDescriptor().Port("terminal")
	if segmentTerminal.Cardinality != element.Variadic {
		t.Fatalf("segment terminal port = %+v", segmentTerminal)
	}
}

func TestInteractionElementsReportLiveManagementPlaneIdentity(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes["segment"].Resolution
		if resolution != nil && string(resolution.RuntimeEvidence) == "live" &&
			string(resolution.CapabilitiesEvidence) == "live" {
			if resolution.Runtime.ID == "" || len(resolution.Capabilities) != 0 {
				t.Fatalf("interaction live resolution = %+v", resolution)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("interaction did not report live resolution: %+v", mounted.Live().Nodes["segment"])
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStrictBoundedInteractionConfig(t *testing.T) {
	tests := []struct {
		name   string
		decode func(json.RawMessage) error
		value  string
	}{
		{"segment unknown", func(raw json.RawMessage) error { _, err := decodeSegmentConfig(raw); return err }, `{"mystery":1}`},
		{"segment duplicate", func(raw json.RawMessage) error { _, err := decodeSegmentConfig(raw); return err }, `{"max_segments":2,"max_segments":3}`},
		{"segment unbounded", func(raw json.RawMessage) error { _, err := decodeSegmentConfig(raw); return err }, `{"max_segments":999999}`},
		{"arbiter unknown", func(raw json.RawMessage) error { _, err := decodeArbiterConfig(raw); return err }, `{"priority":"slow"}`},
		{"arbiter duplicate", func(raw json.RawMessage) error { _, err := decodeArbiterConfig(raw); return err }, `{"max_pending_runs":2,"max_pending_runs":3}`},
		{"arbiter unbounded", func(raw json.RawMessage) error { _, err := decodeArbiterConfig(raw); return err }, `{"max_buffered_bytes":999999999}`},
		{"commit unknown", func(raw json.RawMessage) error { _, err := decodeCommitConfig(raw); return err }, `{"retry":true}`},
		{"commit duplicate", func(raw json.RawMessage) error { _, err := decodeCommitConfig(raw); return err }, `{"max_pending":2,"max_pending":3}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.decode(json.RawMessage(test.value)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestSegmentPreparedTextReleasesSafeUnitsAndTranslatesCancellation(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	text := ingress(t, mounted, "text")
	segments := egress(t, mounted, "segments")
	send(t, text, preparedEnvelope("begin", "slow-run", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	send(t, text, preparedEnvelope("delta", "slow-run", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1, Text: "Hello. remainder",
	}))
	spokenEnvelope := receive(t, segments)
	spoken, ok := spokenEnvelope.Payload.(speech.TextSegment)
	if !ok || spoken.Text != "Hello." || spoken.ID == "" || spokenEnvelope.RunID != "slow-run" {
		t.Fatalf("speech segment = %#v in %+v", spokenEnvelope.Payload, spokenEnvelope)
	}
	if spoken.SpeechAuthority != "" {
		t.Fatalf("graph-routed speech inherited legacy authority %q", spoken.SpeechAuthority)
	}

	send(t, ingress(t, mounted, "cancel"), element.Envelope{
		Type: ModelCancelType(), ItemID: "cancel-slow", RunID: "slow-run",
		Payload: cognitionelements.Cancel{RunID: "slow-run", Reason: "new user speech"},
	})
	modelCancel := receive(t, egress(t, mounted, "model_cancel"))
	if request := modelCancel.Payload.(cognitionelements.Cancel); request.RunID != "slow-run" {
		t.Fatalf("model cancellation = %+v", request)
	}
	speechCancel := receive(t, egress(t, mounted, "speech_cancel"))
	if request := speechCancel.Payload.(speech.Cancel); request.UtteranceID != spoken.ID {
		t.Fatalf("speech cancellation = %+v, segment = %+v", request, spoken)
	}
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeCanceled)
	if outcome.RunID != "slow-run" || outcome.Segments != 1 || outcome.Code != "canceled" {
		t.Fatalf("segmentation outcome = %+v", outcome)
	}

	// A late source end is harmless after the addressed terminal is retained.
	send(t, text, preparedEnvelope("late-end", "slow-run", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2, Interrupted: true,
	}))
	assertNoEnvelope(t, segments)
}

func TestSegmentPreparedTextLateCancelRevokesCompletedStreamExactlyOnce(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	text := ingress(t, mounted, "text")
	segments := egress(t, mounted, "segments")
	outcomes := egress(t, mounted, "outcome")
	speechCancels := egress(t, mounted, "speech_cancel")
	modelCancels := egress(t, mounted, "model_cancel")
	const runID = "completed-before-cancel"
	send(t, text, preparedEnvelope("late-begin", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	send(t, text, preparedEnvelope("late-delta", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1,
		Text: "First sentence. Second sentence.",
	}))
	first := receive(t, segments).Payload.(speech.TextSegment)
	send(t, text, preparedEnvelope("late-end", runID, cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2,
	}))
	second := receive(t, segments).Payload.(speech.TextSegment)
	completed := receiveSegmentationOutcome(t, outcomes, OutcomeCompleted)
	if first.ID == "" || second.ID == "" || first.ID == second.ID || completed.Segments != 2 {
		t.Fatalf("completed segmented stream = first=%+v second=%+v outcome=%+v",
			first, second, completed)
	}

	send(t, ingress(t, mounted, "cancel"), element.Envelope{
		Type: ModelCancelType(), ItemID: "late-cancel", RunID: runID,
		Payload: cognitionelements.Cancel{RunID: runID, Reason: "user interrupted playback"},
	})
	for index, utteranceID := range []string{first.ID, second.ID} {
		envelope := receive(t, speechCancels)
		request, ok := envelope.Payload.(speech.Cancel)
		if !ok || request.UtteranceID != utteranceID ||
			request.Reason != "user interrupted playback" || envelope.RunID != runID ||
			envelope.CancellationScope != utteranceID || envelope.Sequence != uint64(index+1) {
			t.Fatalf("late speech cancellation %d = %+v / %#v", index, envelope, envelope.Payload)
		}
	}
	late := receiveSegmentationOutcome(t, outcomes, OutcomeCanceled)
	if late.RunID != runID || late.Segments != 2 || late.Code != "canceled_after_stream" {
		t.Fatalf("late cancellation outcome = %+v", late)
	}
	assertNoEnvelope(t, modelCancels)

	// The retained utterance identities are consumed by the first late cancel,
	// so a replay cannot issue a second cancellation for the same speech.
	send(t, ingress(t, mounted, "cancel"), element.Envelope{
		Type: ModelCancelType(), ItemID: "late-cancel-replay", RunID: runID,
		Payload: cognitionelements.Cancel{RunID: runID, Reason: "replayed interruption"},
	})
	replayed := receiveSegmentationOutcome(t, outcomes, OutcomeIgnored)
	if replayed.Code != "already_terminal" {
		t.Fatalf("replayed late cancellation outcome = %+v", replayed)
	}
	assertNoEnvelope(t, speechCancels)
}

func TestSegmentPreparedTextRejectsMalformedAndUnboundedStreams(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(
			`{"minimum_runes":2,"max_segment_bytes":8,"max_run_bytes":16}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)

	text := ingress(t, mounted, "text")
	send(t, text, preparedEnvelope("missing-begin", "bad-frame", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 0, Text: "bad",
	}))
	_ = receive(t, egress(t, mounted, "model_cancel"))
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeRefused)
	if outcome.Code != "invalid_framing" {
		t.Fatalf("malformed outcome = %+v", outcome)
	}

	send(t, text, preparedEnvelope("bounded-begin", "bounded", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	send(t, text, preparedEnvelope("bounded-delta", "bounded", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1, Text: "abcdefghijk",
	}))
	_ = receive(t, egress(t, mounted, "model_cancel"))
	outcome = receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeFailed)
	if outcome.Code != "segment_too_large" {
		t.Fatalf("bounded outcome = %+v", outcome)
	}
}

func TestSegmentPreparedTextSourceFailureRevokesEmittedSpeech(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, segmentGraph,
		map[string]json.RawMessage{"segment": json.RawMessage(`{"minimum_runes":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)
	text := ingress(t, mounted, "text")
	send(t, text, preparedEnvelope("source-begin", "source-failure",
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
	send(t, text, preparedEnvelope("source-delta", "source-failure",
		cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 1, Text: "Spoken. unfinished",
		}))
	spoken := receive(t, egress(t, mounted, "segments")).Payload.(speech.TextSegment)
	send(t, ingress(t, mounted, "terminal"), element.Envelope{
		Type: ModelOutcomeType(), ItemID: "source-terminal", RunID: "source-failure",
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeFailed, Operation: "generate", RunID: "source-failure",
			Code: "provider_error", Message: "provider disconnected",
		},
	})
	speechCancel := receive(t, egress(t, mounted, "speech_cancel")).Payload.(speech.Cancel)
	if speechCancel.UtteranceID != spoken.ID || speechCancel.Reason != "provider disconnected" {
		t.Fatalf("source-failure speech cancellation = %+v", speechCancel)
	}
	outcome := receiveSegmentationOutcome(t, egress(t, mounted, "outcome"), OutcomeFailed)
	if outcome.Code != "source_failed" || outcome.RunID != "source-failure" {
		t.Fatalf("source-failure outcome = %+v", outcome)
	}
	// The source has already failed; the adapter does not bounce a redundant
	// cancellation back upstream.
	assertNoEnvelope(t, egress(t, mounted, "model_cancel"))
}

func TestSpeechArbiterNeverInterleavesConcurrentFastAndSlowStreams(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, arbiterGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	selection := ingress(t, mounted, "selection")
	outcomes := egress(t, mounted, "outcome")
	sendSelection(t, selection, "fast", SelectionPreempt, "select-fast")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "fast")
	sendSelection(t, selection, "slow", SelectionQueue, "queue-slow")
	assertArbitrationKind(t, outcomes, OutcomeQueued, "slow")

	fast := ingress(t, mounted, "fast_text")
	slow := ingress(t, mounted, "slow_text")
	var wait sync.WaitGroup
	errorsOut := make(chan error, 2)
	for _, source := range []struct {
		port element.OutputPort
		run  string
		text string
	}{
		{fast, "fast", "fast answer"}, {slow, "slow", "slow answer"},
	} {
		wait.Add(1)
		go func(source struct {
			port element.OutputPort
			run  string
			text string
		}) {
			defer wait.Done()
			for _, envelope := range preparedStream(source.run, source.text) {
				if err := broadcastTest(source.port, envelope); err != nil {
					errorsOut <- err
					return
				}
			}
		}(source)
	}
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Fatal(err)
	}

	selected := egress(t, mounted, "selected")
	var runs []string
	var texts []string
	for range 6 {
		envelope := receive(t, selected)
		runs = append(runs, envelope.RunID)
		delta := envelope.Payload.(cognitionelements.PreparedTextDelta)
		if delta.Boundary == cognitionelements.TextChunk {
			texts = append(texts, delta.Text)
		}
	}
	if !reflect.DeepEqual(runs, []string{"fast", "fast", "fast", "slow", "slow", "slow"}) ||
		!reflect.DeepEqual(texts, []string{"fast answer", "slow answer"}) {
		t.Fatalf("arbitrated stream runs/text = %v / %v", runs, texts)
	}
	assertArbitrationKind(t, outcomes, OutcomeCompleted, "fast")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "slow")
	assertArbitrationKind(t, outcomes, OutcomeCompleted, "slow")
}

func TestSpeechArbiterExplicitPreemptionClosesFramingAndCancelsOnlyTarget(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, arbiterGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)

	selection := ingress(t, mounted, "selection")
	outcomes := egress(t, mounted, "outcome")
	selected := egress(t, mounted, "selected")
	sendSelection(t, selection, "fast", SelectionPreempt, "select-fast")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "fast")
	fast := ingress(t, mounted, "fast_text")
	send(t, fast, preparedEnvelope("fast-begin", "fast", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextBegin, Index: 0,
	}))
	send(t, fast, preparedEnvelope("fast-delta", "fast", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextChunk, Index: 1, Text: "fast partial",
	}))
	_ = receive(t, selected)
	_ = receive(t, selected)

	sendSelection(t, selection, "slow", SelectionPreempt, "select-slow")
	end := receive(t, selected)
	endDelta := end.Payload.(cognitionelements.PreparedTextDelta)
	if end.RunID != "fast" || endDelta.Boundary != cognitionelements.TextEnd ||
		!endDelta.Interrupted || endDelta.Index != 2 {
		t.Fatalf("synthetic terminal = %+v / %+v", end, endDelta)
	}
	cancelEnvelope := receive(t, egress(t, mounted, "cancel_upstream"))
	cancelRequest := cancelEnvelope.Payload.(cognitionelements.Cancel)
	if cancelRequest.RunID != "fast" || cancelEnvelope.RunID != "fast" {
		t.Fatalf("preemption cancellation = %+v / %+v", cancelEnvelope, cancelRequest)
	}
	assertArbitrationKind(t, outcomes, OutcomePreempted, "fast")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "slow")

	for _, envelope := range preparedStream("slow", "deliberative answer") {
		send(t, ingress(t, mounted, "slow_text"), envelope)
	}
	for index := range 3 {
		envelope := receive(t, selected)
		if envelope.RunID != "slow" {
			t.Fatalf("selected item %d run = %q", index, envelope.RunID)
		}
	}
	assertArbitrationKind(t, outcomes, OutcomeCompleted, "slow")
	// The real terminal eventually releases bounded discard state and is not
	// forwarded after the synthetic close.
	send(t, fast, preparedEnvelope("fast-real-end", "fast", cognitionelements.PreparedTextDelta{
		Boundary: cognitionelements.TextEnd, Index: 2, Interrupted: true,
	}))
	assertNoEnvelope(t, selected)
}

func TestSpeechArbiterRejectsOneRunAcrossSeveralLanesAndConflictingAddress(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, arbiterGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)
	selection := ingress(t, mounted, "selection")
	outcomes := egress(t, mounted, "outcome")
	selected := egress(t, mounted, "selected")
	sendSelection(t, selection, "same", SelectionPreempt, "select-same")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "same")
	send(t, ingress(t, mounted, "fast_text"), preparedEnvelope("same-begin", "same",
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextBegin, Index: 0}))
	_ = receive(t, selected)
	send(t, ingress(t, mounted, "slow_text"), preparedEnvelope("same-other-lane", "same",
		cognitionelements.PreparedTextDelta{Boundary: cognitionelements.TextChunk, Index: 1, Text: "bad"}))
	terminal := receive(t, selected).Payload.(cognitionelements.PreparedTextDelta)
	if terminal.Boundary != cognitionelements.TextEnd || !terminal.Interrupted {
		t.Fatalf("lane conflict terminal = %+v", terminal)
	}
	_ = receive(t, egress(t, mounted, "cancel_upstream"))
	failed := receiveArbitrationKind(t, outcomes, OutcomeFailed, "same")
	if failed.Code != "multiple_source_lanes" {
		t.Fatalf("lane conflict outcome = %+v", failed)
	}

	send(t, ingress(t, mounted, "cancel"), element.Envelope{
		Type: ModelCancelType(), ItemID: "conflict", RunID: "one",
		Payload: cognitionelements.Cancel{RunID: "two"},
	})
	refused := receiveArbitrationKind(t, outcomes, OutcomeRefused, "one")
	if refused.Code != "conflicting_run_id" {
		t.Fatalf("conflicting cancel outcome = %+v", refused)
	}
}

func TestSpeechArbiterTimeoutIsExplicitAndBounded(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, arbiterGraph,
		map[string]json.RawMessage{"arbiter": json.RawMessage(`{"max_pending_runs":1}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)
	selection := ingress(t, mounted, "selection")
	outcomes := egress(t, mounted, "outcome")
	sendSelection(t, selection, "waiting", SelectionPreempt, "select-waiting")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "waiting")
	send(t, ingress(t, mounted, "timeout"), element.Envelope{
		Type: TimeoutType(), ItemID: "timeout-waiting", RunID: "waiting",
		Payload: Timeout{RunID: "waiting", Reason: "speech deadline"},
	})
	_ = receive(t, egress(t, mounted, "cancel_upstream"))
	timedOut := receiveArbitrationKind(t, outcomes, OutcomeFailed, "waiting")
	if timedOut.Code != "timeout" || timedOut.BufferedRuns != 1 {
		t.Fatalf("timeout outcome = %+v", timedOut)
	}
	// The timed-out run is a bounded tombstone until its source end arrives.
	sendSelection(t, selection, "another", SelectionPreempt, "select-another")
	refused := receiveArbitrationKind(t, outcomes, OutcomeRefused, "another")
	if refused.Code != "run_capacity" {
		t.Fatalf("bounded tombstone outcome = %+v", refused)
	}
}

func TestSpeechArbiterSourceTerminalReleasesPreemptedRunWithoutTextEnd(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, arbiterGraph,
		map[string]json.RawMessage{"arbiter": json.RawMessage(`{"max_pending_runs":2}`)}, nil)
	defer stopInteractionGraph(t, done, cancel)
	selection := ingress(t, mounted, "selection")
	outcomes := egress(t, mounted, "outcome")

	// The first run is selected and then preempted before it ever opens a text
	// stream. Its model cannot emit TextEnd, so the model terminal must release
	// the discard tombstone.
	sendSelection(t, selection, "first", SelectionPreempt, "select-first")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "first")
	sendSelection(t, selection, "second", SelectionPreempt, "select-second")
	_ = receive(t, egress(t, mounted, "cancel_upstream"))
	assertArbitrationKind(t, outcomes, OutcomePreempted, "first")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "second")
	send(t, ingress(t, mounted, "fast_terminal"), element.Envelope{
		Type: ModelOutcomeType(), ItemID: "first-canceled", RunID: "first",
		Payload: cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: "first",
		},
	})
	released := receiveArbitrationKind(t, outcomes, OutcomeIgnored, "first")
	if released.Code != "discard_terminal" || released.BufferedRuns != 1 {
		t.Fatalf("released tombstone outcome = %+v", released)
	}

	// With the old leak, the two-entry bound was still full and this explicit
	// replacement was incorrectly refused.
	sendSelection(t, selection, "third", SelectionPreempt, "select-third")
	_ = receive(t, egress(t, mounted, "cancel_upstream"))
	assertArbitrationKind(t, outcomes, OutcomePreempted, "second")
	assertArbitrationKind(t, outcomes, OutcomeSelected, "third")
}

func TestModelResultCommitCompareAndAppendAndRejectsStaleWork(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, commitGraph, nil, nil)
	defer stopInteractionGraph(t, done, cancel)
	snapshots := egress(t, mounted, "snapshot")
	if seed := receive(t, snapshots).Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("initial snapshot = %+v", seed)
	}
	results := ingress(t, mounted, "result")
	outcomes := egress(t, mounted, "outcome")

	first := modelResult("first", 0, "", false, false)
	send(t, results, resultEnvelopeForTest("first-result", first))
	snapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
	if snapshot.Version != 2 || len(snapshot.Items) != 2 ||
		snapshot.Items[0].Kind != trajectory.KindInstruction ||
		snapshot.Items[1].Kind != trajectory.KindAssistant ||
		snapshot.Items[1].Producer.SpeechAuthority != string(continuation.SpeechAuthoritySilent) {
		t.Fatalf("committed model snapshot = %+v", snapshot)
	}
	committed := receiveModelCommitKind(t, outcomes, ModelCommitted)
	if committed.RunID != "first" || committed.StoreVersion != 2 || len(committed.ItemIDs) != 2 {
		t.Fatalf("commit outcome = %+v", committed)
	}

	stale := modelResult("stale", 0, "", false, false)
	send(t, results, resultEnvelopeForTest("stale-result", stale))
	rejected := receiveModelCommitKind(t, outcomes, ModelRejected)
	if rejected.RunID != "stale" || rejected.Code != "version_conflict" || rejected.StoreVersion != 2 {
		t.Fatalf("stale result outcome = %+v", rejected)
	}
	assertNoEnvelope(t, snapshots)

	interrupted := modelResult("interrupted", snapshot.Version,
		snapshot.Items[len(snapshot.Items)-1].ID, true, true)
	send(t, results, resultEnvelopeForTest("interrupted-result", interrupted))
	snapshot = receive(t, snapshots).Payload.(trajectory.Snapshot)
	if snapshot.Version != 3 || snapshot.Items[len(snapshot.Items)-1].Kind != trajectory.KindInstruction {
		t.Fatalf("interrupted proposal snapshot = %+v", snapshot)
	}
	for _, item := range snapshot.Items {
		if item.InvocationID == "interrupted" && item.Kind == trajectory.KindToolProposal {
			t.Fatalf("interrupted proposal escaped into canonical state: %+v", item)
		}
	}
	_ = receiveModelCommitKind(t, outcomes, ModelCommitted)
}

func TestModelResultCommitRejectsReplyThatChangesAttestedItems(t *testing.T) {
	mounted, done, cancel := mountInteractionGraph(t, commitAdapterGraph, nil, nil)
	result := modelResult("forged-reply", 0, "", false, false)
	resultEnvelope := resultEnvelopeForTest("forged-result", result)
	resultEnvelope.SessionID = "session-a"
	send(t, ingress(t, mounted, "result"), resultEnvelope)
	request := receive(t, egress(t, mounted, "append"))
	appendRequest := request.Payload.(stateelements.Append)
	items := cloneTrajectoryItems(appendRequest.Items)
	items[len(items)-1].Content = "substituted assistant output"
	ids := make([]string, len(items))
	for index := range items {
		ids[index] = items[index].ID
	}
	send(t, ingress(t, mounted, "committed"), element.Envelope{
		Type: commitType, ItemID: request.ItemID + ":committed",
		SessionID: request.SessionID, RunID: request.RunID,
		CausalParents: []string{request.ItemID},
		Payload: stateelements.Commit{
			Version: uint64(len(items)), AppendedIDs: ids,
			Snapshot: trajectory.Snapshot{Version: uint64(len(items)), Items: items},
		},
	})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "changed appended item contents") {
			t.Fatalf("forged reply error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forged reply did not stop the graph")
	}
	cancel()
}

func TestReferenceRoutingGraphsCompileAndHaveNoHiddenSlowFastHandoff(t *testing.T) {
	for _, directory := range []string{
		"interaction-fast-only", "interaction-slow-only", "interaction-both",
	} {
		t.Run(directory, func(t *testing.T) {
			graph := compileArtifact(t, directory)
			if graph.Fingerprint == "" {
				t.Fatal("compiled graph has no fingerprint")
			}
			for _, edge := range graph.Edges {
				if edge.From.Node == "fast" && edge.To.Node == "slow" ||
					edge.From.Node == "slow" && edge.To.Node == "fast" {
					t.Fatalf("hidden model-to-model handoff: %+v", edge)
				}
			}
			valuesPath := filepath.Join("..", "..", "graphs", "components",
				directory, "agent.values.yaml")
			valuesSource, err := os.ReadFile(valuesPath)
			if err != nil {
				t.Fatal(err)
			}
			document, err := graphvalues.ParseYAML(valuesPath, valuesSource)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := graphvalues.Bind(graph, document); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacySilentSlowProviderSpeaksWhenGraphRoutesIt(t *testing.T) {
	descriptor := testModelDescriptor()
	descriptor.Phase = trajectory.PhaseSlow
	descriptor.SpeechAuthority = continuation.SpeechAuthoritySilent
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("legacy.silent", descriptor, func() (continuation.Provider, error) {
		return &oneAnswerProvider{descriptor: descriptor, answer: "The slow answer."}, nil
	}); err != nil {
		t.Fatal(err)
	}
	graph := compileArtifact(t, "interaction-slow-only")
	registry := testRegistry(t)
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(cognitionelements.ProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{
			"slow": json.RawMessage(`{"provider":"legacy.silent"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	defer stopInteractionGraph(t, done, cancel)
	_ = receive(t, egress(t, mounted, "model_resolution"))
	send(t, ingress(t, mounted, "context"), element.Envelope{
		Type: cognitionelements.ContextType(), ItemID: "empty-context",
		Payload: trajectory.Snapshot{},
	})
	version := uint64(0)
	send(t, ingress(t, mounted, "trigger"), element.Envelope{
		Type: cognitionelements.GenerateType(), ItemID: "slow-trigger", RunID: "slow-run",
		Payload: cognitionelements.Generate{
			ExpectedContextVersion: &version,
			Invocation:             continuation.Invocation{Instruction: "answer carefully"},
		},
	})
	spoken := receive(t, egress(t, mounted, "speech")).Payload.(speech.TextSegment)
	if spoken.Text != "The slow answer." || spoken.SpeechAuthority != "" {
		t.Fatalf("graph-routed legacy-silent output = %+v", spoken)
	}
}

func TestInteractionRunnersStopPromptlyUnderConcurrentInput(t *testing.T) {
	for iteration := range 20 {
		mounted, done, cancel := mountInteractionGraph(t, arbiterGraph, nil, nil)
		selection := ingress(t, mounted, "selection")
		go func(index int) {
			_ = broadcastTest(selection, element.Envelope{
				Type: SelectionType(), ItemID: "shutdown-selection", RunID: "run",
				Payload: SpeechSelection{RunID: "run", Mode: SelectionQueue},
			})
		}(iteration)
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("iteration %d stopped with %v", iteration, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d did not stop", iteration)
		}
		_ = mounted
	}
}

type oneAnswerProvider struct {
	descriptor continuation.Descriptor
	answer     string
}

func (provider *oneAnswerProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *oneAnswerProvider) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: provider.answer}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func mountInteractionGraph(
	t *testing.T, source string, values map[string]json.RawMessage,
	providers *cognitionelements.ProviderRegistry,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	graph := compileSource(t, "interaction-test.ortg", []byte(source))
	services := graphruntime.NewServiceSet()
	if providers != nil {
		if _, err := services.Set(cognitionelements.ProviderRegistryService, providers); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: testRegistry(t), Services: services, Values: values,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func stopInteractionGraph(t *testing.T, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("interaction graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("interaction graph did not stop")
	}
}

func testCatalog(t *testing.T) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	for _, register := range []func(*resolve.Catalog) error{
		RegisterDescriptors,
		cognitionelements.RegisterDescriptors,
		flowelements.RegisterDescriptors,
		stateelements.RegisterDescriptors,
	} {
		if err := register(catalog); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func testRegistry(t *testing.T) *graphruntime.Registry {
	t.Helper()
	registry := graphruntime.NewRegistry()
	for _, register := range []func(*graphruntime.Registry) error{
		RegisterFactories,
		cognitionelements.RegisterFactories,
		flowelements.RegisterFactories,
		stateelements.RegisterFactories,
	} {
		if err := register(registry); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func compileSource(t *testing.T, name string, source []byte) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse(name, source)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: testCatalog(t), ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func compileArtifact(t *testing.T, directory string) ir.Graph {
	t.Helper()
	path := filepath.Join("..", "..", "graphs", "components", directory, "agent.ortg")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return compileSource(t, path, source)
}

func testModelDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "model", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func modelResult(
	runID string, version uint64, tail string, interrupted, proposal bool,
) cognitionelements.Result {
	result := cognitionelements.Result{
		RunID: runID, ProviderReference: "test.provider", Descriptor: testModelDescriptor(),
		ContextVersion: version, ContextTailID: tail,
		Invocation:  continuation.Invocation{Instruction: "answer", SourceRevision: 7},
		Interrupted: interrupted, Completion: continuation.Completion{StopReason: "stop"},
	}
	if proposal {
		call := trajectory.ToolCall{
			CallID: "call-" + runID, Name: "lookup", Arguments: json.RawMessage(`{"query":"x"}`),
		}
		tool := continuation.ToolDefinition{
			Name: "lookup", Description: "look up data", Parameters: json.RawMessage(`{"type":"object"}`),
		}
		result.Invocation.Tools = []continuation.ToolDefinition{tool}
		prepared := cognitionelements.ToolProposal{
			Call: call, Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
		}
		result.Outputs = []cognitionelements.PreparedOutput{{
			Kind: cognitionelements.PreparedTool, Proposal: &prepared,
		}}
		result.ToolProposals = []cognitionelements.ToolProposal{prepared}
		return result
	}
	result.Outputs = []cognitionelements.PreparedOutput{{
		Kind: cognitionelements.PreparedAssistant, Text: "hello from " + runID,
	}}
	result.AssistantText = "hello from " + runID
	return result
}

func TestModelResultValidationTreatsZeroProposalRepresentationsEqually(t *testing.T) {
	result := modelResult("no-tools", 0, "", false, false)
	result.ToolProposals = make([]cognitionelements.ToolProposal, 0)
	if err := validateCognitionResult(result); err != nil {
		t.Fatalf("empty aggregate proposal slice: %v", err)
	}
	result.ToolProposals = nil
	if err := validateCognitionResult(result); err != nil {
		t.Fatalf("nil aggregate proposal slice: %v", err)
	}
}

func resultEnvelopeForTest(itemID string, result cognitionelements.Result) element.Envelope {
	return element.Envelope{
		Type: cognitionelements.ResultType(), ItemID: itemID, RunID: result.RunID, Payload: result,
	}
}

func preparedEnvelope(
	itemID, runID string, delta cognitionelements.PreparedTextDelta,
) element.Envelope {
	return element.Envelope{
		Type: PreparedTextType(), ItemID: itemID, RunID: runID,
		SourceID: "legacy-silent-model", Payload: delta,
	}
}

func preparedStream(runID, text string) []element.Envelope {
	return []element.Envelope{
		preparedEnvelope(runID+"-begin", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin, Index: 0,
		}),
		preparedEnvelope(runID+"-delta", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextChunk, Index: 1, Text: text,
		}),
		preparedEnvelope(runID+"-end", runID, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd, Index: 2,
		}),
	}
}

func sendSelection(
	t *testing.T, output element.OutputPort, runID string, mode SelectionMode, itemID string,
) {
	t.Helper()
	send(t, output, element.Envelope{
		Type: SelectionType(), ItemID: itemID, RunID: runID,
		Payload: SpeechSelection{RunID: runID, Mode: mode},
	})
}

func ingress(t *testing.T, mounted *graphruntime.Mounted, name string) element.OutputPort {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func egress(t *testing.T, mounted *graphruntime.Mounted, name string) element.InputPort {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func send(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	if err := broadcastTest(output, envelope); err != nil {
		t.Fatal(err)
	}
}

func broadcastTest(output element.OutputPort, envelope element.Envelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := output.Broadcast(ctx, envelope)
	return err
}

func receive(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertNoEnvelope(t *testing.T, input element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected envelope: %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receive empty output: %v", err)
	}
}

func receiveSegmentationOutcome(
	t *testing.T, input element.InputPort, kind OutcomeKind,
) SegmentationOutcome {
	t.Helper()
	outcome := receive(t, input).Payload.(SegmentationOutcome)
	if outcome.Kind != kind {
		t.Fatalf("segmentation kind = %q, want %q: %+v", outcome.Kind, kind, outcome)
	}
	return outcome
}

func assertArbitrationKind(
	t *testing.T, input element.InputPort, kind OutcomeKind, runID string,
) {
	t.Helper()
	_ = receiveArbitrationKind(t, input, kind, runID)
}

func receiveArbitrationKind(
	t *testing.T, input element.InputPort, kind OutcomeKind, runID string,
) ArbitrationOutcome {
	t.Helper()
	outcome := receive(t, input).Payload.(ArbitrationOutcome)
	if outcome.Kind != kind || outcome.RunID != runID {
		t.Fatalf("arbitration outcome = %+v, want %q for %q", outcome, kind, runID)
	}
	return outcome
}

func receiveModelCommitKind(
	t *testing.T, input element.InputPort, kind ModelCommitKind,
) ModelCommitOutcome {
	t.Helper()
	outcome := receive(t, input).Payload.(ModelCommitOutcome)
	if outcome.Kind != kind {
		t.Fatalf("model commit outcome = %+v, want %q", outcome, kind)
	}
	return outcome
}
