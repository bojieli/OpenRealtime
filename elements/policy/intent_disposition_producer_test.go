package policy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const intentDispositionProducerTestGraph = `graph intent_disposition_producer_test {
    policy.IntentDispositionProducer :: producer;
    input probe = producer.probe;
    input cancel = producer.cancel;
    output disposition = producer.disposition;
    output state = producer.state;
    output outcome = producer.outcome;
    output resolved = producer.resolved;
}
`

var intentDispositionTestDetector = IntentDetectorIdentity{
	Reference: "settlement-primary", Revision: "immutable-1",
	ConfigurationDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
}

type intentDispositionTestDecider struct {
	descriptor SemanticDeciderDescriptor
	outcome    coreinteraction.Outcome
	err        error
	decide     func(context.Context, coreinteraction.Decision) (coreinteraction.Outcome, error)

	mu       sync.Mutex
	requests []coreinteraction.Decision
	closed   atomic.Bool
}

func (decider *intentDispositionTestDecider) Name() string { return "intent-disposition-test" }

func (decider *intentDispositionTestDecider) Descriptor() SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *intentDispositionTestDecider) Decide(
	ctx context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	copy := decision
	copy.Options = slices.Clone(decision.Options)
	copy.Images = make([]coreinteraction.Image, len(decision.Images))
	for index := range decision.Images {
		copy.Images[index] = decision.Images[index]
		copy.Images[index].Bytes = slices.Clone(decision.Images[index].Bytes)
	}
	decider.mu.Lock()
	decider.requests = append(decider.requests, copy)
	decider.mu.Unlock()
	if decider.decide != nil {
		return decider.decide(ctx, decision)
	}
	return decider.outcome, decider.err
}

func (decider *intentDispositionTestDecider) Close() error {
	decider.closed.Store(true)
	return nil
}

func (decider *intentDispositionTestDecider) recordedRequests() []coreinteraction.Decision {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	return slices.Clone(decider.requests)
}

type intentDispositionProducerHarness struct {
	mounted *graphruntime.Mounted
	done    <-chan error
	cancel  context.CancelFunc
}

func TestIntentDispositionProducerResolvesExactVisualEvidenceAndBindsDecision(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, true)
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		outcome: coreinteraction.Outcome{
			Index: 1, Option: string(IntentDispositionSucceeded), Confidence: .9, Measured: true,
		},
	}
	var opens atomic.Int32
	resolvedHandles := make(chan string, 1)
	harness := mountIntentDispositionProducer(t, store, decider, &opens,
		continuation.MediaResolver(func(handle string) (continuation.Media, error) {
			resolvedHandles <- handle
			return continuation.Media{MIMEType: "image/png", Bytes: []byte{1, 2, 3, 4}}, nil
		}), true, nil)
	defer harness.stop(t)

	resolution := receiveIntentDispositionProducer(t, harness, "resolved")
	resolved, ok := resolution.Payload.(IntentDispositionProducerResolution)
	if !ok || resolved.Detector != intentDispositionTestDetector ||
		!reflect.DeepEqual(resolved.Descriptor, decider.descriptor) ||
		!strings.HasPrefix(resolved.DescriptorDigest, "sha256:") ||
		!resolved.DirectVisualInput || opens.Load() != 1 {
		t.Fatalf("producer resolution = %+v payload type %T opens=%d", resolved, resolution.Payload, opens.Load())
	}
	startup := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if startup.Active || startup.Pending != 0 || startup.MaxPending == 0 || startup.TerminalMemory == 0 {
		t.Fatalf("producer startup state = %+v", startup)
	}

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	activeState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if !activeState.Active {
		t.Fatalf("producer did not expose active decision state: %+v", activeState)
	}
	dispositionEnvelope := receiveIntentDispositionProducer(t, harness, "disposition")
	disposition, ok := dispositionEnvelope.Payload.(IntentDisposition)
	if !ok {
		t.Fatalf("disposition payload type = %T", dispositionEnvelope.Payload)
	}
	if disposition.Kind != IntentDispositionSucceeded ||
		!reflect.DeepEqual(disposition.Probe, probe) || disposition.Detector != probe.Detector ||
		disposition.DecisionStartedNS < probe.IssuedNS ||
		disposition.DecisionFinishedNS < disposition.DecisionStartedNS ||
		dispositionEnvelope.SourceID != probe.Detector.Reference ||
		dispositionEnvelope.SessionID != probe.SessionID ||
		dispositionEnvelope.RunID != probe.Result.InvocationID ||
		dispositionEnvelope.CancellationScope != probe.DurableIntent.TrajectoryItemID ||
		!reflect.DeepEqual(dispositionEnvelope.CausalParents, []string{probe.ProbeID}) ||
		!strings.HasPrefix(dispositionEnvelope.ItemID, "intent-disposition-disposition:sha256:") {
		t.Fatalf("disposition envelope = %+v payload=%+v", dispositionEnvelope, disposition)
	}
	if err := validateIntentDisposition(disposition, intentDispositionTestDetector); err != nil {
		t.Fatalf("produced disposition: %v", err)
	}
	select {
	case handle := <-resolvedHandles:
		if handle != "post-screen-media" {
			t.Fatalf("resolved handle = %q", handle)
		}
	default:
		t.Fatal("exact trigger media was not resolved")
	}
	requests := decider.recordedRequests()
	if len(requests) != 1 || !reflect.DeepEqual(requests[0].Options, intentDispositionOptions) ||
		len(requests[0].Images) != 1 || requests[0].Images[0].MIMEType != "image/png" ||
		!reflect.DeepEqual(requests[0].Images[0].Bytes, []byte{1, 2, 3, 4}) ||
		!strings.Contains(requests[0].Evidence, "Click Save") ||
		!strings.Contains(requests[0].Evidence, `{"ok":true}`) ||
		!strings.Contains(requests[0].Evidence, "Saved successfully") {
		t.Fatalf("semantic decision request = %+v", requests)
	}
	outcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if outcome.Kind != IntentDispositionProducerProduced ||
		outcome.Disposition != IntentDispositionSucceeded ||
		outcome.DispositionItemID != dispositionEnvelope.ItemID ||
		outcome.DecisionStartedNS != disposition.DecisionStartedNS ||
		outcome.DecisionFinishedNS != disposition.DecisionFinishedNS {
		t.Fatalf("producer outcome = %+v", outcome)
	}
	state := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if state.Active || state.Pending != 0 || state.Produced != 1 || state.Succeeded != 1 ||
		state.TerminalEntries != 1 {
		t.Fatalf("producer final state = %+v", state)
	}
	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	duplicate := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if duplicate.Kind != IntentDispositionProducerIgnored || duplicate.Code != "duplicate_terminal_probe" {
		t.Fatalf("duplicate terminal probe outcome = %+v", duplicate)
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	assertNoIntentDispositionProducerEnvelope(t, harness, "disposition", 20*time.Millisecond)
	if requests := decider.recordedRequests(); len(requests) != 1 {
		t.Fatalf("terminal probe was classified %d times", len(requests))
	}
}

func TestIntentDispositionProducerStalledExactMediaFailsClosedAsIndeterminate(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, true)
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(20 * time.Millisecond),
		outcome:    coreinteraction.Outcome{Index: 1, Option: string(IntentDispositionSucceeded)},
	}
	resolverEntered := make(chan struct{}, 1)
	releaseResolver := make(chan struct{})
	defer close(releaseResolver)
	harness := mountIntentDispositionProducer(t, store, decider, nil,
		continuation.MediaResolver(func(handle string) (continuation.Media, error) {
			if handle != "post-screen-media" {
				return continuation.Media{}, errors.New("wrong handle")
			}
			resolverEntered <- struct{}{}
			<-releaseResolver
			return continuation.Media{MIMEType: "image/png", Bytes: []byte{1, 2, 3, 4}}, nil
		}), true, nil)
	defer harness.stop(t)
	consumeIntentDispositionProducerStartup(t, harness)

	started := time.Now()
	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	activeState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if !activeState.Active {
		t.Fatalf("producer did not expose active media resolution: %+v", activeState)
	}
	select {
	case <-resolverEntered:
	case <-time.After(time.Second):
		t.Fatal("media resolver was not entered")
	}
	dispositionEnvelope := receiveIntentDispositionProducer(t, harness, "disposition")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stalled media delayed indeterminate disposition for %v", elapsed)
	}
	disposition := dispositionEnvelope.Payload.(IntentDisposition)
	if disposition.Kind != IntentDispositionIndeterminate {
		t.Fatalf("stalled media disposition = %+v", disposition)
	}
	outcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if outcome.Kind != IntentDispositionProducerFailed || outcome.Code != "decision_timeout" ||
		outcome.Disposition != IntentDispositionIndeterminate {
		t.Fatalf("stalled media outcome = %+v", outcome)
	}
	finalState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if finalState.Active || finalState.Indeterminate != 1 || finalState.ProviderFailures != 1 {
		t.Fatalf("stalled media state = %+v", finalState)
	}
	if requests := decider.recordedRequests(); len(requests) != 0 {
		t.Fatalf("decider was called without exact media: %+v", requests)
	}
}

func TestIntentDispositionProducerEnforcesProviderDeadlineWithoutConcurrentRetry(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, false)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(20 * time.Millisecond),
		decide: func(context.Context, coreinteraction.Decision) (coreinteraction.Outcome, error) {
			entered <- struct{}{}
			<-release
			return coreinteraction.Outcome{Index: 1, Option: string(IntentDispositionSucceeded)}, nil
		},
	}
	harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, nil)
	defer harness.stop(t)
	defer close(release)
	consumeIntentDispositionProducerStartup(t, harness)

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	_ = receiveIntentDispositionProducer(t, harness, "state")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("semantic decider was not entered")
	}
	first := receiveIntentDispositionProducer(t, harness, "disposition").Payload.(IntentDisposition)
	firstOutcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	firstState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if first.Kind != IntentDispositionIndeterminate || firstOutcome.Code != "decision_timeout" ||
		!firstState.DeciderUnavailable {
		t.Fatalf("deadline disposition=%+v outcome=%+v state=%+v", first, firstOutcome, firstState)
	}

	// Indeterminate is retryable, but the timed-out call may still be running.
	// The same client is therefore not called concurrently; the retry receives
	// another explicit indeterminate classification from the failed boundary.
	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	_ = receiveIntentDispositionProducer(t, harness, "state")
	second := receiveIntentDispositionProducer(t, harness, "disposition").Payload.(IntentDisposition)
	secondOutcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	_ = receiveIntentDispositionProducer(t, harness, "state")
	if second.Kind != IntentDispositionIndeterminate || secondOutcome.Code != "decider_unavailable" ||
		second.DecisionStartedNS <= first.DecisionFinishedNS {
		t.Fatalf("deadline retry disposition=%+v outcome=%+v", second, secondOutcome)
	}
	select {
	case <-entered:
		t.Fatal("producer started a concurrent call on its deadline-violating client")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestIntentDispositionProducerProviderFailuresEmitOnlyIndeterminate(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		outcome coreinteraction.Outcome
		err     error
		code    string
	}{
		{
			name: "provider error", err: errors.New("provider unavailable"),
			code: "decision_failed",
		},
		{
			name:    "non-enumerated output",
			outcome: coreinteraction.Outcome{Index: 99, Option: "probably-succeeded"},
			code:    "invalid_decision",
		},
		{
			name:    "option and index mismatch",
			outcome: coreinteraction.Outcome{Index: 0, Option: string(IntentDispositionSucceeded)},
			code:    "invalid_decision",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, probe := intentDispositionProducerFixture(t, false)
			decider := &intentDispositionTestDecider{
				descriptor: intentDispositionProducerTestDescriptor(time.Second),
				outcome:    testCase.outcome, err: testCase.err,
			}
			harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, nil)
			defer harness.stop(t)
			consumeIntentDispositionProducerStartup(t, harness)
			sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
			_ = receiveIntentDispositionProducer(t, harness, "state")
			dispositionEnvelope := receiveIntentDispositionProducer(t, harness, "disposition")
			disposition := dispositionEnvelope.Payload.(IntentDisposition)
			if disposition.Kind != IntentDispositionIndeterminate ||
				disposition.Detector != intentDispositionTestDetector ||
				!reflect.DeepEqual(disposition.Probe, probe) {
				t.Fatalf("provider failure disposition = %+v", disposition)
			}
			outcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
			if outcome.Kind != IntentDispositionProducerFailed || outcome.Code != testCase.code ||
				outcome.Disposition != IntentDispositionIndeterminate ||
				outcome.DispositionItemID != dispositionEnvelope.ItemID {
				t.Fatalf("provider failure outcome = %+v", outcome)
			}
			state := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
			if state.Indeterminate != 1 || state.ProviderFailures != 1 || state.Produced != 1 {
				t.Fatalf("provider failure state = %+v", state)
			}
		})
	}
}

func TestIntentDispositionProducerIndeterminateProbeCanBeRetriedMonotonically(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, false)
	var calls atomic.Int32
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		decide: func(context.Context, coreinteraction.Decision) (coreinteraction.Outcome, error) {
			if calls.Add(1) == 1 {
				return coreinteraction.Outcome{Index: 3, Option: string(IntentDispositionIndeterminate)}, nil
			}
			return coreinteraction.Outcome{Index: 1, Option: string(IntentDispositionSucceeded)}, nil
		},
	}
	// A constant clock exercises the producer's serialized timing floor. The
	// settlement gate requires a retry to begin after the previous disposition.
	harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, func() uint64 { return 100 })
	defer harness.stop(t)
	consumeIntentDispositionProducerStartup(t, harness)

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	_ = receiveIntentDispositionProducer(t, harness, "state")
	first := receiveIntentDispositionProducer(t, harness, "disposition").Payload.(IntentDisposition)
	_ = receiveIntentDispositionProducer(t, harness, "outcome")
	firstState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if first.Kind != IntentDispositionIndeterminate || firstState.TerminalEntries != 0 {
		t.Fatalf("first disposition=%+v state=%+v", first, firstState)
	}

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	_ = receiveIntentDispositionProducer(t, harness, "state")
	second := receiveIntentDispositionProducer(t, harness, "disposition").Payload.(IntentDisposition)
	_ = receiveIntentDispositionProducer(t, harness, "outcome")
	secondState := receiveIntentDispositionProducer(t, harness, "state").Payload.(IntentDispositionProducerState)
	if second.Kind != IntentDispositionSucceeded ||
		second.DecisionStartedNS <= first.DecisionFinishedNS ||
		second.DecisionFinishedNS < second.DecisionStartedNS ||
		secondState.TerminalEntries != 1 || secondState.Produced != 2 ||
		secondState.Indeterminate != 1 || secondState.Succeeded != 1 {
		t.Fatalf("retry first=%+v second=%+v state=%+v", first, second, secondState)
	}
}

func TestIntentDispositionProducerExactCancellationSuppressesInflightResult(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, false)
	entered := make(chan struct{}, 1)
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		decide: func(ctx context.Context, _ coreinteraction.Decision) (coreinteraction.Outcome, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return coreinteraction.Outcome{}, context.Cause(ctx)
		},
	}
	harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, nil)
	defer harness.stop(t)
	consumeIntentDispositionProducerStartup(t, harness)

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("decider was not entered")
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	cancellation := IntentSettlementCancellation{
		SessionID: probe.SessionID, DurableIntent: probe.DurableIntent, Reason: "user canceled",
	}
	sendIntentDispositionProducer(t, harness, "cancel", element.Envelope{
		Type: IntentSettlementCancelType(), ItemID: "cancel-intent-1",
		SessionID: probe.SessionID, CancellationScope: probe.DurableIntent.TrajectoryItemID,
		Payload: cancellation,
	})
	_ = receiveIntentDispositionProducer(t, harness, "state")
	outcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if outcome.Kind != IntentDispositionProducerCanceled || outcome.Code != "canceled" ||
		outcome.DurableIntentItemID != probe.DurableIntent.TrajectoryItemID {
		t.Fatalf("cancellation outcome = %+v", outcome)
	}
	// The final state retires the canceled model result. It may not be
	// accompanied by a disposition for the obsolete probe.
	_ = receiveIntentDispositionProducer(t, harness, "state")
	assertNoIntentDispositionProducerEnvelope(t, harness, "disposition", 50*time.Millisecond)

	// Replaying the canceled exact probe is explicitly suppressed by the
	// bounded durable-intent tombstone and cannot restart model work.
	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	replayed := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if replayed.Kind != IntentDispositionProducerCanceled || replayed.Code != "canceled" {
		t.Fatalf("canceled probe replay outcome = %+v", replayed)
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	assertNoIntentDispositionProducerEnvelope(t, harness, "disposition", 20*time.Millisecond)
	if requests := decider.recordedRequests(); len(requests) != 1 {
		t.Fatalf("canceled probe replay reached decider: %d requests", len(requests))
	}
}

func TestIntentDispositionProducerCancellationWaitsForDecisionQuiescenceAndParentsCancel(t *testing.T) {
	store, probe := intentDispositionProducerFixture(t, false)
	entered := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	release := make(chan struct{})
	released := false
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		decide: func(ctx context.Context, _ coreinteraction.Decision) (coreinteraction.Outcome, error) {
			entered <- struct{}{}
			<-ctx.Done()
			canceled <- struct{}{}
			// Model transports are not entitled to claim cancellation merely
			// because their context closed. This deliberately stubborn client
			// does not become quiescent until its call actually returns.
			<-release
			return coreinteraction.Outcome{}, context.Cause(ctx)
		},
	}
	harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, nil)
	defer harness.stop(t)
	defer func() {
		if !released {
			close(release)
		}
	}()
	consumeIntentDispositionProducerStartup(t, harness)

	sendIntentDispositionProducer(t, harness, "probe", intentDispositionProbeEnvelope(probe))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("decider was not entered")
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	cancelEnvelope := element.Envelope{
		Type: IntentSettlementCancelType(), ItemID: "cancel-await-quiescence",
		SessionID: probe.SessionID, CancellationScope: probe.DurableIntent.TrajectoryItemID,
		Payload: IntentSettlementCancellation{
			SessionID: probe.SessionID, DurableIntent: probe.DurableIntent, Reason: "user canceled",
		},
	}
	sendIntentDispositionProducer(t, harness, "cancel", cancelEnvelope)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("decider did not receive cancellation")
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	assertNoIntentDispositionProducerEnvelope(t, harness, "outcome", 50*time.Millisecond)

	close(release)
	released = true
	envelope := receiveIntentDispositionProducer(t, harness, "outcome")
	outcome := envelope.Payload.(IntentDispositionProducerOutcome)
	if outcome.Kind != IntentDispositionProducerCanceled || outcome.Code != "canceled" ||
		outcome.ProbeID != probe.ProbeID ||
		outcome.DurableIntentItemID != probe.DurableIntent.TrajectoryItemID {
		t.Fatalf("quiescent cancellation outcome = %+v", outcome)
	}
	if !slices.Contains(envelope.CausalParents, cancelEnvelope.ItemID) {
		t.Fatalf("quiescent cancellation parents = %v, want exact cancel %q",
			envelope.CausalParents, cancelEnvelope.ItemID)
	}
	assertNoIntentDispositionProducerEnvelope(t, harness, "disposition", 20*time.Millisecond)
}

func TestIntentDispositionProducerLifecycleClosesItsFreshClient(t *testing.T) {
	store, _ := intentDispositionProducerFixture(t, false)
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		outcome:    coreinteraction.Outcome{Index: 0, Option: string(IntentDispositionContinue)},
	}
	var opens atomic.Int32
	harness := mountIntentDispositionProducer(t, store, decider, &opens, nil, false, nil)
	consumeIntentDispositionProducerStartup(t, harness)
	if opens.Load() != 1 || decider.closed.Load() {
		t.Fatalf("client before stop: opens=%d closed=%t", opens.Load(), decider.closed.Load())
	}
	harness.stop(t)
	if !decider.closed.Load() {
		t.Fatal("producer did not close its independently owned decider")
	}
}

func TestIntentDispositionProducerMountRejectsDetectorDescriptorDrift(t *testing.T) {
	store, _ := intentDispositionProducerFixture(t, false)
	descriptor := intentDispositionProducerTestDescriptor(time.Second)
	descriptor.Revision = "different-revision"
	decider := &intentDispositionTestDecider{descriptor: descriptor}
	semanticRegistry := NewSemanticDeciderRegistry()
	var opens atomic.Int32
	if err := semanticRegistry.Register(intentDispositionTestDetector.Reference, descriptor,
		func() (SemanticDecider, error) {
			opens.Add(1)
			return decider, nil
		}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store, SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set(SemanticDeciderRegistryService, semanticRegistry); err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := registry.Register("policy.IntentDispositionProducer", intentDispositionProducerFactory{}); err != nil {
		t.Fatal(err)
	}
	_, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileIntentDispositionProducerGraph(t), Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"producer": intentDispositionProducerConfigJSON(t, false)},
	})
	if err == nil || !strings.Contains(err.Error(), "detector identity differs") {
		t.Fatalf("descriptor drift mount error = %v", err)
	}
	if opens.Load() != 0 {
		t.Fatalf("descriptor-drift mount opened %d provider clients", opens.Load())
	}
}

func TestIntentDispositionProducerMemoriesRemainBounded(t *testing.T) {
	runner := &intentDispositionProducerRunner{
		config:        IntentDispositionProducerConfig{TerminalMemory: 2, CancelMemory: 2},
		terminal:      make(map[string]struct{}),
		cancellations: make(map[intentDispositionCancellationAddress]struct{}),
	}
	for _, probeID := range []string{"probe-a", "probe-b", "probe-c"} {
		runner.rememberTerminal(probeID)
	}
	if len(runner.terminal) != 2 || len(runner.terminalOrder) != 2 {
		t.Fatalf("terminal memory = %v order=%v", runner.terminal, runner.terminalOrder)
	}
	for index := 0; index < 3; index++ {
		runner.rememberCancellation(intentDispositionCancellationAddress{
			session: "session-a",
			intent: TemporalEvidenceItemIdentity{
				TrajectoryItemID: "intent-" + string(rune('a'+index)),
			},
		})
	}
	if len(runner.cancellations) != 2 || len(runner.cancelOrder) != 2 {
		t.Fatalf("cancellation memory = %v order=%v", runner.cancellations, runner.cancelOrder)
	}
}

func TestIntentDispositionProducerConfigAndEnvelopeBoundsFailClosed(t *testing.T) {
	factory := intentDispositionProducerFactory{}
	valid := intentDispositionProducerConfigJSON(t, true)
	if err := factory.ValidateConfig(valid); err != nil {
		t.Fatalf("valid producer config: %v", err)
	}
	for _, source := range []string{
		`{}`,
		`{"expected_settlement":null}`,
		`{"expected_settlement":{"expected_admission":{"mode":"after_intent","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"immutable-1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},"max_media_items":0}`,
		`{"expected_settlement":{"expected_admission":{"mode":"after_intent","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"immutable-1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}},"max_pending":null}`,
	} {
		if err := factory.ValidateConfig(json.RawMessage(source)); err == nil {
			t.Errorf("invalid producer config accepted: %s", source)
		}
	}

	store, probe := intentDispositionProducerFixture(t, false)
	decider := &intentDispositionTestDecider{
		descriptor: intentDispositionProducerTestDescriptor(time.Second),
		outcome:    coreinteraction.Outcome{Index: 0, Option: string(IntentDispositionContinue)},
	}
	harness := mountIntentDispositionProducer(t, store, decider, nil, nil, false, nil)
	defer harness.stop(t)
	consumeIntentDispositionProducerStartup(t, harness)
	foreign := probe
	foreign.SessionID = strings.Repeat("x", maximumPolicyIdentifierBytes+1)
	sendIntentDispositionProducer(t, harness, "probe", element.Envelope{
		Type: IntentSettlementProbeType(), ItemID: "foreign-probe", SessionID: "session-a",
		Payload: foreign,
	})
	outcome := receiveIntentDispositionProducer(t, harness, "outcome").Payload.(IntentDispositionProducerOutcome)
	if outcome.Kind != IntentDispositionProducerRefused || outcome.Code != "invalid_probe" ||
		len(outcome.Message) > maximumPolicyReasonBytes {
		t.Fatalf("hostile probe outcome = %+v", outcome)
	}
	_ = receiveIntentDispositionProducer(t, harness, "state")
	assertNoIntentDispositionProducerEnvelope(t, harness, "disposition", 20*time.Millisecond)
	if len(decider.recordedRequests()) != 0 {
		t.Fatal("hostile probe reached the semantic decider")
	}
}

func intentDispositionProducerTestDescriptor(timeout time.Duration) SemanticDeciderDescriptor {
	return SemanticDeciderDescriptor{
		Provider: "test-provider", Model: "test-model", Protocol: "enum-v1",
		Revision:            intentDispositionTestDetector.Revision,
		ConfigurationDigest: intentDispositionTestDetector.ConfigurationDigest,
		Vision:              true, DecisionTimeoutMS: timeout.Milliseconds(),
	}
}

func intentDispositionProducerConfigJSON(t *testing.T, visual bool) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(IntentDispositionProducerConfig{
		ExpectedSettlement: intentDispositionProducerExpectedSettlement(),
		DirectVisualInput:  visual, MaxEvidenceBytes: 4096, MaxMediaBytes: 64,
		MaxMediaItems: 1, MaxPending: 4, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func intentDispositionProducerExpectedSettlement() IntentSettlementConfig {
	return IntentSettlementConfig{
		ExpectedAdmission: TemporalEvidenceAdmissionConfig{
			Mode:      TemporalEvidenceAdmissionAfterIntent,
			SourceSet: TemporalEvidenceSourceSetExplicit,
			Required:  []TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		},
		CandidateSources: []TemporalEvidenceRequirement{{Observer: "vision", Source: "screen"}},
		Detector:         intentDispositionTestDetector,
	}
}

func intentDispositionProducerFixture(
	t *testing.T, withMedia bool,
) (*trajectory.Store, IntentSettlementProbe) {
	t.Helper()
	store := trajectory.NewStore()
	postObservation := &trajectory.ObservationMeta{
		Observer: "vision", Source: "screen", Authority: trajectory.AuthorityObserver,
	}
	if withMedia {
		postObservation.Media = []trajectory.MediaRef{{
			Handle: "post-screen-media", MIMEType: "image/png", Source: "screen",
			Width: 2, Height: 2, Bytes: 4, CapturedNS: 30,
		}}
	}
	items := []trajectory.Item{
		{
			ID: "old-screen", Kind: trajectory.KindObservation, MonotonicNS: 1,
			SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "vision"},
			Content:     "Save is available",
			Observation: &trajectory.ObservationMeta{Observer: "vision", Source: "screen", Authority: trajectory.AuthorityObserver},
			Event:       &trajectory.EventMetadata{EventID: "old-screen-event", Type: "vision.endpoint", Source: "vision", Channel: "screen", OccurredNS: 10},
		},
		{
			ID: "intent-1", Kind: trajectory.KindObservation, MonotonicNS: 2,
			SourceRevision: 2, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Click Save",
			Event: &trajectory.EventMetadata{EventID: "intent-event", Type: "participant.endpoint", Source: "participant", Channel: "text", OccurredNS: 20},
		},
		{
			ID: "call-item-1", Kind: trajectory.KindToolCall, MonotonicNS: 3,
			CausalParentIDs: []string{"intent-1"}, InvocationID: "generation-1",
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
			ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "computer.click", Arguments: json.RawMessage(`{"x":1}`)},
		},
		{
			ID: "result-item-1", Kind: trajectory.KindToolResult, MonotonicNS: 4,
			CausalParentIDs: []string{"call-item-1"}, InvocationID: "generation-1",
			Producer:   trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{CallID: "call-1", Name: "computer.click", Output: json.RawMessage(`{"ok":true}`)},
		},
		{
			ID: "post-screen", Kind: trajectory.KindObservation, MonotonicNS: 5,
			CausalParentIDs: []string{"intent-1", "result-item-1"}, SourceRevision: 5,
			Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "vision"},
			Content:  "Saved successfully", Observation: postObservation,
			Event: &trajectory.EventMetadata{EventID: "post-screen-event", Type: "vision.endpoint", Source: "vision", Channel: "screen", OccurredNS: 30},
		},
	}
	if err := store.AppendBatch(items); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	intentIdentity, err := temporalEvidenceIdentity(snapshot.Items[1], 1)
	if err != nil {
		t.Fatal(err)
	}
	triggerIdentity, err := temporalEvidenceIdentity(snapshot.Items[4], 4)
	if err != nil {
		t.Fatal(err)
	}
	commit := stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: "post-screen-event",
		TrajectoryItemID: "post-screen", StreamID: "vision:screen",
		ObservationRevision: 5, SourceRevision: 5, StoreVersion: snapshot.Version,
		Context: stateelements.CommittedContext{Prefix: prefix, StateItemID: "trajectory-state-post-screen"},
	}
	evidence := AdmittedTemporalEvidence{
		Mode: TemporalEvidenceAdmissionAfterIntent, SourceSet: TemporalEvidenceSourceSetExplicit,
		TriggerCommit: commit, TriggerObservation: triggerIdentity,
		DurableIntent: &intentIdentity, QualifyingObservations: []TemporalEvidenceItemIdentity{triggerIdentity},
		Prefix: prefix,
	}
	probe := IntentSettlementProbe{
		Issuer: "settlement", Sequence: 1, SessionID: "session-a",
		Evidence: evidence, DurableIntent: intentIdentity, TriggerObservation: triggerIdentity,
		Result: IntentSettlementResultIdentity{
			TrajectoryItemID: "result-item-1", StoreVersion: 4,
			InvocationID: "generation-1", CallID: "call-1", Tool: "computer.click",
		},
		Prefix: prefix, Detector: intentDispositionTestDetector, IssuedNS: 100,
	}
	probe.ProbeID, err = intentSettlementProbeID(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyIntentSettlementProbe(snapshot, probe, intentDispositionProducerExpectedSettlement()); err != nil {
		t.Fatalf("fixture probe: %v", err)
	}
	return store, probe
}

func intentDispositionProbeEnvelope(probe IntentSettlementProbe) element.Envelope {
	return element.Envelope{
		Type: IntentSettlementProbeType(), ItemID: probe.ProbeID,
		SessionID: probe.SessionID, Sequence: probe.Sequence,
		CausalParents: []string{"held-evidence"}, Payload: probe,
	}
}

func mountIntentDispositionProducer(
	t *testing.T, store *trajectory.Store, decider *intentDispositionTestDecider,
	opens *atomic.Int32, media continuation.MediaResolver, visual bool, now func() uint64,
) intentDispositionProducerHarness {
	t.Helper()
	semanticRegistry := NewSemanticDeciderRegistry()
	if err := semanticRegistry.Register(intentDispositionTestDetector.Reference, decider.descriptor,
		func() (SemanticDecider, error) {
			if opens != nil {
				opens.Add(1)
			}
			return decider, nil
		}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store, SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set(SemanticDeciderRegistryService, semanticRegistry); err != nil {
		t.Fatal(err)
	}
	if media != nil {
		if _, err := services.Set(cognitionelements.MediaResolverService, media); err != nil {
			t.Fatal(err)
		}
	}
	registry := graphruntime.NewRegistry()
	if err := registry.Register("policy.IntentDispositionProducer", intentDispositionProducerFactory{}); err != nil {
		t.Fatal(err)
	}
	if now == nil {
		var clock atomic.Uint64
		clock.Store(1000)
		now = func() uint64 { return clock.Add(10) }
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileIntentDispositionProducerGraph(t), Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"producer": intentDispositionProducerConfigJSON(t, visual)},
		Now:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return intentDispositionProducerHarness{mounted: mounted, done: done, cancel: cancel}
}

func compileIntentDispositionProducerGraph(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("intent-disposition-producer-test.ortg", []byte(intentDispositionProducerTestGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := catalog.Register(IntentDispositionProducerDescriptor()); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func (harness intentDispositionProducerHarness) stop(t *testing.T) {
	t.Helper()
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("intent disposition producer stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("intent disposition producer did not stop")
	}
}

func sendIntentDispositionProducer(
	t *testing.T, harness intentDispositionProducerHarness, port string, envelope element.Envelope,
) {
	t.Helper()
	output, err := harness.mounted.Ingress(port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
}

func receiveIntentDispositionProducer(
	t *testing.T, harness intentDispositionProducerHarness, port string,
) element.Envelope {
	t.Helper()
	input, err := harness.mounted.Egress(port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertNoIntentDispositionProducerEnvelope(
	t *testing.T, harness intentDispositionProducerHarness, port string, timeout time.Duration,
) {
	t.Helper()
	input, err := harness.mounted.Egress(port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected %s envelope: %+v", port, envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) {
		t.Fatalf("empty %s receive: %v", port, err)
	}
}

func consumeIntentDispositionProducerStartup(t *testing.T, harness intentDispositionProducerHarness) {
	t.Helper()
	_ = receiveIntentDispositionProducer(t, harness, "resolved")
	_ = receiveIntentDispositionProducer(t, harness, "state")
}
