package video

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const testOpenedNS uint64 = 1_000_000_000

func TestFixedCadenceUsesOnlyExplicitTicksAndPreservesCausality(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceFixed, FixedIntervalMS: 100,
		MinIntervalMS: 50, MaxIntervalMS: 500, ChangeThreshold: 0.1,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 8,
	})
	source := sourceEnvelope("source-1", "session-a", 1, testOpenedNS)
	if err := runner.handleSource(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	frame := frameEnvelope("frame-1", "session-a", 1, 1, testOpenedNS+1, []byte{0, 1, 2, 3})
	if err := runner.handleFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	// The policy owns its retained frame even if an ingress producer violates
	// the graph's immutable-payload convention after the send completes.
	frame.Payload.(InlineFrame).Frame.Image[0] = 99
	if len(ports.observe.values) != 0 {
		t.Fatal("frame arrival advanced cadence without a tick")
	}
	early := tickEnvelope("tick-early", "session-a", 1, testOpenedNS+99_000_000)
	if err := runner.handleTick(context.Background(), early); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 0 || ports.decision.last().Payload.(ObservationDecision).Reason != "not_due" {
		t.Fatalf("early tick emitted observation: decision=%+v", ports.decision.last().Payload)
	}
	due := tickEnvelope("tick-due", "session-a", 1, testOpenedNS+100_000_000)
	if err := runner.handleTick(context.Background(), due); err != nil {
		t.Fatal(err)
	}
	observed := ports.observe.last()
	batch := observed.Payload.(perceptionelements.ImageBatch)
	if len(batch.Frames) != 1 || batch.Frames[0].Index != 1 || batch.Frames[0].Image[0] != 0 || observed.CaptureNS != frame.Payload.(InlineFrame).Frame.CapturedNS {
		t.Fatalf("observed batch = %+v envelope=%+v", batch, observed)
	}
	wantParents := []string{source.ItemID, frame.ItemID, due.ItemID}
	if !slices.Equal(observed.CausalParents, wantParents) {
		t.Fatalf("observation parents = %v, want %v", observed.CausalParents, wantParents)
	}
	decision := ports.decision.last().Payload.(ObservationDecision)
	if decision.Kind != DecisionObserve || decision.Reason != "fixed_due" || decision.FrameItemID != frame.ItemID {
		t.Fatalf("fixed decision = %+v", decision)
	}

	// The last accepted source index and capture time fence replay even when a
	// lossy edge skipped intermediate indices.
	stale := frameEnvelope("frame-stale", "session-a", 1, 1, testOpenedNS+2, []byte{9})
	if err := runner.handleFrame(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || outcome.Code != "invalid_frame" {
		t.Fatalf("stale frame outcome = %+v", outcome)
	}
	crossSession := frameEnvelope("frame-cross-session", "session-b", 1, 2, testOpenedNS+3, []byte{9})
	if err := runner.handleFrame(context.Background(), crossSession); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "session") {
		t.Fatalf("cross-session frame outcome = %+v", outcome)
	}

	reference := referenceEnvelope("reference-2", "session-a", 1, 2, testOpenedNS+4, "sha256:two")
	if err := runner.handleReference(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	secondDue := tickEnvelope("tick-second", "session-a", 1, testOpenedNS+200_000_000)
	if err := runner.handleTick(context.Background(), secondDue); err != nil {
		t.Fatal(err)
	}
	referenceBatch := ports.observeReference.last().Payload.(ReferenceBatch)
	if len(referenceBatch.References) != 1 || referenceBatch.References[0].Reference != "youtube://video/frame/2" {
		t.Fatalf("reference observation = %+v", referenceBatch)
	}
}

func TestSourceEndClosesExactGenerationAndFencesLateFrames(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceAdaptive, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.2,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 8,
	})
	if err := runner.handleSource(context.Background(), sourceEnvelope("end-source", "session-a", 1, testOpenedNS)); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleFrame(context.Background(), frameEnvelope("end-frame", "session-a", 1, 1, testOpenedNS+10, []byte{1})); err != nil {
		t.Fatal(err)
	}

	wrong := element.Envelope{
		Type: sourceEndType, ItemID: "wrong-end", SessionID: "session-b", SourceID: "screen",
		Payload: SourceEnd{Source: "screen", StreamID: "screen-stream", SourceRevision: 1,
			EndedNS: testOpenedNS + 20, Reason: "wrong session"},
	}
	if err := runner.handleEnd(context.Background(), wrong); err != nil {
		t.Fatal(err)
	}
	if !runner.active || len(ports.observerClose.values) != 0 {
		t.Fatalf("wrong-scope end changed lifecycle: active=%v closes=%d", runner.active, len(ports.observerClose.values))
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || outcome.Code != "invalid_scope" {
		t.Fatalf("wrong-scope end outcome = %+v", outcome)
	}

	end := element.Envelope{
		Type: sourceEndType, ItemID: "valid-end", SessionID: "session-a", SourceID: "screen",
		Payload: SourceEnd{Source: "screen", StreamID: "screen-stream", SourceRevision: 1,
			EndedNS: testOpenedNS + 20, Reason: "source closed"},
	}
	if err := runner.handleEnd(context.Background(), end); err != nil {
		t.Fatal(err)
	}
	if runner.active || runner.latest != nil || runner.phase != PhaseEnded {
		t.Fatalf("end left live state: active=%v latest=%+v phase=%s", runner.active, runner.latest, runner.phase)
	}
	closed := ports.observerClose.last()
	if source := closed.Payload.(perceptionelements.VisualSourceClose).Source; source != "screen" || !slices.Equal(closed.CausalParents, []string{end.ItemID}) {
		t.Fatalf("observer close = %+v", closed)
	}
	key := sourceKey("session-a", "screen", "screen-stream", 1)
	if terminal, found := runner.terminated[key]; !found || terminal.phase != PhaseEnded {
		t.Fatalf("end tombstone = %+v, found=%v", terminal, found)
	}
	if state := ports.state.last().Payload.(ObservationPolicyState); state.Phase != PhaseEnded {
		t.Fatalf("terminal state = %+v", state)
	}

	late := frameEnvelope("end-late", "session-a", 1, 2, testOpenedNS+30, []byte{2})
	if err := runner.handleFrame(context.Background(), late); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "ended") {
		t.Fatalf("late ended frame outcome = %+v", outcome)
	}
}

func TestAdaptiveDueTimeSaturatesAtTimestampLimit(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceAdaptive, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 8,
	})
	opened := uint64(math.MaxUint64 - 200_000_000)
	if err := runner.handleSource(context.Background(), sourceEnvelope("overflow-source", "session-a", 1, opened)); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleFrame(context.Background(), frameEnvelope("overflow-frame", "session-a", 1, 1, opened+1, []byte{1})); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("overflow-tick", "session-a", 1, math.MaxUint64-50_000_000)); err != nil {
		t.Fatal(err)
	}
	if runner.nextDueNS != math.MaxUint64 {
		t.Fatalf("overflow next due = %d, want saturation", runner.nextDueNS)
	}
}

func TestSourceDeclarationFencesMIMEAndReferenceNamespace(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceManual, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.2,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 8,
	})
	start := sourceEnvelope("fenced-source", "session-a", 1, testOpenedNS)
	request := start.Payload.(SourceStart)
	request.ExpectedMIMEType = "image/jpeg"
	request.ExternalReferenceURI = "youtube://video/"
	start.Payload = request
	if err := runner.handleSource(context.Background(), start); err != nil {
		t.Fatal(err)
	}

	wrongMIME := frameEnvelope("wrong-mime", "session-a", 1, 1, testOpenedNS+1, []byte{1})
	if err := runner.handleFrame(context.Background(), wrongMIME); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "MIME") {
		t.Fatalf("wrong MIME outcome = %+v", outcome)
	}

	outside := referenceEnvelope("outside-reference", "session-a", 1, 1, testOpenedNS+1, "sha256:one")
	reference := outside.Payload.(FrameReference)
	reference.Reference = "youtube://video-archive/frame/1"
	outside.Payload = reference
	if err := runner.handleReference(context.Background(), outside); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "locator") {
		t.Fatalf("outside reference outcome = %+v", outcome)
	}

	inside := referenceEnvelope("inside-reference", "session-a", 1, 1, testOpenedNS+1, "sha256:one")
	if err := runner.handleReference(context.Background(), inside); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeSucceeded {
		t.Fatalf("inside reference outcome = %+v", outcome)
	}
}

func TestInlineAndReferenceMIMEMetadataAreCanonicalAndBounded(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceManual, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.2,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 256,
		TerminalMemory: 8,
	})
	if err := runner.handleSource(context.Background(), sourceEnvelope("metadata-source", "session-a", 1, testOpenedNS)); err != nil {
		t.Fatal(err)
	}

	invalidMIME := frameEnvelope("metadata-nul", "session-a", 1, 1, testOpenedNS+1, []byte{1})
	frame := invalidMIME.Payload.(InlineFrame)
	frame.Frame.MIMEType = "image/png\x00"
	invalidMIME.Payload = frame
	if err := runner.handleFrame(context.Background(), invalidMIME); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "canonical") {
		t.Fatalf("non-canonical inline MIME outcome = %+v", outcome)
	}

	oversized := frameEnvelope("metadata-large", "session-a", 1, 1, testOpenedNS+1, []byte{1})
	frame = oversized.Payload.(InlineFrame)
	frame.Frame.MIMEType = strings.Repeat("x", 256)
	oversized.Payload = frame
	if err := runner.handleFrame(context.Background(), oversized); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "metadata") {
		t.Fatalf("oversized inline metadata outcome = %+v", outcome)
	}

	reference := referenceEnvelope("metadata-reference-nul", "session-a", 1, 1, testOpenedNS+1, "sha256:one")
	value := reference.Payload.(FrameReference)
	value.MIMEType = "image/jpeg\x00"
	reference.Payload = value
	if err := runner.handleReference(context.Background(), reference); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "canonical MIME") {
		t.Fatalf("non-canonical reference MIME outcome = %+v", outcome)
	}
}

func TestAdaptiveCadenceUsesBoundedChangeStateAndMaximumInterval(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceAdaptive, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.2,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 8,
	})
	if err := runner.handleSource(context.Background(), sourceEnvelope("source-adaptive", "session-a", 1, testOpenedNS)); err != nil {
		t.Fatal(err)
	}
	first := frameEnvelope("adaptive-1", "session-a", 1, 1, testOpenedNS+1, []byte{0, 0, 0, 0})
	if err := runner.handleFrame(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("adaptive-first", "session-a", 1, testOpenedNS+100_000_000)); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 1 {
		t.Fatalf("initial changed frame was not observed: %d", len(ports.observe.values))
	}
	identical := frameEnvelope("adaptive-identical", "session-a", 1, 2, testOpenedNS+2, []byte{0, 0, 0, 0})
	if err := runner.handleFrame(context.Background(), identical); err != nil {
		t.Fatal(err)
	}
	if runner.latest.change != 0 || len(runner.latest.sample) > runner.config.MaxChangeSamples {
		t.Fatalf("bounded change state = change %f sample %d", runner.latest.change, len(runner.latest.sample))
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("adaptive-unchanged", "session-a", 1, testOpenedNS+200_000_000)); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 1 || ports.decision.last().Payload.(ObservationDecision).Reason != "unchanged" {
		t.Fatalf("unchanged adaptive frame decision = %+v", ports.decision.last().Payload)
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("adaptive-max", "session-a", 1, testOpenedNS+600_000_000)); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 2 || ports.decision.last().Payload.(ObservationDecision).Reason != "max_interval" {
		t.Fatalf("maximum interval decision = %+v observations=%d", ports.decision.last().Payload, len(ports.observe.values))
	}
	changed := frameEnvelope("adaptive-changed", "session-a", 1, 3, testOpenedNS+3, []byte{255, 255, 255, 255})
	if err := runner.handleFrame(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("adaptive-changed-tick", "session-a", 1, testOpenedNS+700_000_000)); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 3 || ports.decision.last().Payload.(ObservationDecision).Reason != "changed" {
		t.Fatalf("changed adaptive frame decision = %+v observations=%d", ports.decision.last().Payload, len(ports.observe.values))
	}
}

func TestManualRefreshCancellationAndTombstonesAreExplicitAndBounded(t *testing.T) {
	ports := newPolicyTestPorts()
	runner := newPolicyTestRunner(ports, AdaptiveObservationConfig{
		Source: "screen", Mode: CadenceManual, FixedIntervalMS: 100,
		MinIntervalMS: 100, MaxIntervalMS: 500, ChangeThreshold: 0.2,
		MaxFrameBytes: 1024, MaxChangeSamples: 16, MaxMetadataBytes: 1024,
		TerminalMemory: 2,
	})
	if err := runner.handleSource(context.Background(), sourceEnvelope("manual-source", "session-a", 1, testOpenedNS)); err != nil {
		t.Fatal(err)
	}
	frame := frameEnvelope("manual-frame", "session-a", 1, 1, testOpenedNS+1, []byte{1, 2, 3})
	if err := runner.handleFrame(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if err := runner.handleTick(context.Background(), tickEnvelope("manual-wait", "session-a", 1, testOpenedNS+100)); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 0 || ports.decision.last().Payload.(ObservationDecision).Reason != "manual_wait" {
		t.Fatalf("manual cadence advanced without refresh: %+v", ports.decision.last().Payload)
	}
	refresh := element.Envelope{
		Type: refreshType, ItemID: "manual-refresh", SessionID: "session-a", SourceID: "screen",
		Payload: Refresh{Source: "screen", StreamID: "screen-stream", SourceRevision: 1,
			RequestedNS: testOpenedNS + 101, Reason: "tool completed"},
	}
	if err := runner.handleRefresh(context.Background(), refresh); err != nil {
		t.Fatal(err)
	}
	visualRefresh := ports.observerRefresh.last()
	if visualRefresh.Payload.(perceptionelements.VisualRefresh).Reason != "tool completed" || !runner.refreshArmed {
		t.Fatalf("visual refresh = %+v state=%+v", visualRefresh, runner.refreshArmed)
	}
	forcedTick := tickEnvelope("manual-force", "session-a", 1, testOpenedNS+102)
	if err := runner.handleTick(context.Background(), forcedTick); err != nil {
		t.Fatal(err)
	}
	if len(ports.observe.values) != 1 || !ports.decision.last().Payload.(ObservationDecision).Forced {
		t.Fatalf("manual refresh did not force observation: %+v", ports.decision.last().Payload)
	}
	parents := ports.observe.last().CausalParents
	if !slices.Contains(parents, refresh.ItemID) || !slices.Contains(parents, forcedTick.ItemID) {
		t.Fatalf("forced observation lost refresh/tick provenance: %v", parents)
	}

	cancel := element.Envelope{
		Type: cancelType, ItemID: "manual-cancel", SessionID: "session-a", SourceID: "screen",
		Payload: Cancel{Source: "screen", StreamID: "screen-stream", SourceRevision: 1,
			CanceledNS: testOpenedNS + 103, Reason: "watch ended"},
	}
	if err := runner.handleCancel(context.Background(), cancel); err != nil {
		t.Fatal(err)
	}
	if runner.active || runner.latest != nil || runner.phase != PhaseCanceled {
		t.Fatalf("cancel left live state: active=%v latest=%+v phase=%s", runner.active, runner.latest, runner.phase)
	}
	visualCancel := ports.observerCancel.last().Payload.(perceptionelements.VisualCancel)
	if visualCancel.StreamID != "screen-stream" || visualCancel.Reason != "watch ended" {
		t.Fatalf("observer cancel = %+v", visualCancel)
	}
	late := frameEnvelope("late-frame", "session-a", 1, 2, testOpenedNS+104, []byte{9})
	if err := runner.handleFrame(context.Background(), late); err != nil {
		t.Fatal(err)
	}
	if outcome := ports.outcome.last().Payload.(PolicyOutcome); outcome.Kind != OutcomeRefused || !strings.Contains(outcome.Message, "canceled") {
		t.Fatalf("late canceled frame outcome = %+v", outcome)
	}

	// Addressed pre-cancellation uses the same bounded tombstone memory.
	for revision := uint64(2); revision <= 4; revision++ {
		envelope := element.Envelope{
			Type: cancelType, ItemID: fmt.Sprintf("pre-cancel-%d", revision),
			SessionID: "session-a", SourceID: "screen",
			Payload: Cancel{Source: "screen", StreamID: fmt.Sprintf("stream-%d", revision),
				SourceRevision: revision, CanceledNS: testOpenedNS + 200 + revision},
		}
		if err := runner.handleCancel(context.Background(), envelope); err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.terminated) != runner.config.TerminalMemory || len(runner.terminatedOrder) != runner.config.TerminalMemory {
		t.Fatalf("tombstone bound = %d / %d", len(runner.terminated), len(runner.terminatedOrder))
	}
}

func sourceEnvelope(itemID, session string, revision, opened uint64) element.Envelope {
	return element.Envelope{
		Type: sourceStartType, ItemID: itemID, SessionID: session, SourceID: "screen",
		Payload: SourceStart{Source: "screen", StreamID: "screen-stream", Kind: SourceScreen,
			SourceRevision: revision, OpenedNS: opened, NonBackpressurable: true},
	}
}

func frameEnvelope(itemID, session string, revision, index, captured uint64, content []byte) element.Envelope {
	return element.Envelope{
		Type: inlineFrameType, ItemID: itemID, SessionID: session, SourceID: "screen",
		CaptureNS: captured,
		Payload: InlineFrame{StreamID: "screen-stream", SourceRevision: revision,
			Frame: coreperception.Frame{Kind: coreperception.FrameImage, Source: "screen",
				CapturedNS: captured, Index: index, Image: content, MIMEType: "image/png", Width: 2, Height: 2}},
	}
}

func referenceEnvelope(itemID, session string, revision, index, captured uint64, fingerprint string) element.Envelope {
	return element.Envelope{
		Type: frameRefType, ItemID: itemID, SessionID: session, SourceID: "screen", CaptureNS: captured,
		Payload: FrameReference{Source: "screen", StreamID: "screen-stream", SourceRevision: revision,
			Index: index, CapturedNS: captured, Reference: "youtube://video/frame/2",
			MIMEType: "image/jpeg", Width: 2, Height: 2, Bytes: 10, Fingerprint: fingerprint},
	}
}

func tickEnvelope(itemID, session string, revision, now uint64) element.Envelope {
	return element.Envelope{
		Type: timingTickType, ItemID: itemID, SessionID: session, SourceID: "screen",
		Payload: TimingTick{Source: "screen", StreamID: "screen-stream", SourceRevision: revision, NowNS: now},
	}
}

type policyCaptureOutput struct {
	name   string
	typeOf element.Type
	mu     sync.Mutex
	values []element.Envelope
}

func (output *policyCaptureOutput) Name() string            { return output.name }
func (output *policyCaptureOutput) Type() element.Type      { return output.typeOf.Clone() }
func (output *policyCaptureOutput) Lanes() []element.Sender { return nil }
func (output *policyCaptureOutput) Broadcast(_ context.Context, envelope element.Envelope) (element.SendResult, error) {
	output.mu.Lock()
	output.values = append(output.values, envelope.Clone())
	output.mu.Unlock()
	return element.SendResult{Delivered: 1}, nil
}
func (output *policyCaptureOutput) last() element.Envelope {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.values[len(output.values)-1]
}

type policyTestPorts struct {
	observe, observeReference, observerRefresh, observerClose *policyCaptureOutput
	observerCancel, state, decision, outcome                  *policyCaptureOutput
}

func newPolicyTestPorts() *policyTestPorts {
	return &policyTestPorts{
		observe:          &policyCaptureOutput{name: "observe", typeOf: imageBatchType},
		observeReference: &policyCaptureOutput{name: "observe_reference", typeOf: referenceBatchType},
		observerRefresh:  &policyCaptureOutput{name: "observer_refresh", typeOf: visualRefreshType},
		observerClose:    &policyCaptureOutput{name: "observer_close", typeOf: visualCloseType},
		observerCancel:   &policyCaptureOutput{name: "observer_cancel", typeOf: visualCancelType},
		state:            &policyCaptureOutput{name: "state", typeOf: policyStateType},
		decision:         &policyCaptureOutput{name: "decision", typeOf: decisionType},
		outcome:          &policyCaptureOutput{name: "outcome", typeOf: policyOutcomeType},
	}
}

func newPolicyTestRunner(ports *policyTestPorts, config AdaptiveObservationConfig) *adaptiveObservationRunner {
	return &adaptiveObservationRunner{
		instance: "policy", config: config, sequences: graphruntime.NewSequenceAllocator(),
		ports: policyPorts{
			observe: ports.observe, observeReference: ports.observeReference,
			observerRefresh: ports.observerRefresh, observerClose: ports.observerClose,
			observerCancel: ports.observerCancel, state: ports.state,
			decision: ports.decision, outcome: ports.outcome,
		},
		phase: PhaseIdle, terminated: make(map[string]terminalSource),
		terminalItems: make(map[string]struct{}),
	}
}
