package policy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	internalclock "github.com/bojieli/OpenRealtime/internal/clock"
)

const intentDispositionRetryTestGraph = `graph intent_disposition_retry_test {
    policy.IntentDispositionRetry :: retry;
    input probe = retry.probe;
    input disposition = retry.disposition;
    input cancel = retry.cancel;
    input reset = retry.reset;
    output attempt = retry.attempt;
    output forwarded_cancel = retry.forwarded_cancel;
    output forwarded_reset = retry.forwarded_reset;
    output exhausted = retry.exhausted;
    output state = retry.state;
    output outcome = retry.outcome;
}
`

type intentDispositionRetryHarness struct {
	mounted   *graphruntime.Mounted
	scheduler *internalclock.Manual
	done      <-chan error
	cancel    context.CancelFunc
}

func TestIntentDispositionRetryForwardsOnceThenReplaysOnlyAfterIndeterminateDelay(t *testing.T) {
	harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
		InitialDelayMS: 10, BackoffFactor: 2, MaxDelayMS: 40, MaxRetries: 3,
		MaxElapsedMS: 1_000, MaxPending: 4, TerminalMemory: 8, CancelMemory: 8,
	})
	defer harness.stop(t)
	probe := intentDispositionRetryProbe(t, 1)
	original := intentDispositionProbeEnvelope(probe)
	original.SourceID = "settlement-gate"
	original.RunID = probe.Result.InvocationID
	original.TraceID = "trace-retry"
	original.CancellationScope = probe.DurableIntent.TrajectoryItemID

	harness.send(t, "probe", original)
	if attempt := harness.receive(t, "attempt"); !reflect.DeepEqual(attempt, original) {
		t.Fatalf("initial retry attempt = %#v, want exact envelope %#v", attempt, original)
	}
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryObserved ||
		outcome.Code != "initial_attempt" || outcome.Retry != 0 || outcome.DeadlineNS == 0 {
		t.Fatalf("initial retry outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Pending != 1 || state.Armed != 0 || state.Observed != 1 {
		t.Fatalf("initial retry state = %+v", state)
	}
	harness.assertNoEnvelope(t, "attempt")

	first := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "indeterminate-1", 200)
	harness.send(t, "disposition", first)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryScheduled ||
		outcome.Retry != 1 || outcome.DelayMS != 10 || outcome.DispositionItemID != first.ItemID {
		t.Fatalf("scheduled retry outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Pending != 1 || state.Armed != 1 ||
		state.Indeterminate != 1 || state.Scheduled != 1 {
		t.Fatalf("scheduled retry state = %+v", state)
	}
	harness.scheduler.AdvanceNS(uint64(9 * time.Millisecond))
	harness.assertNoEnvelope(t, "attempt")
	harness.scheduler.AdvanceNS(uint64(time.Millisecond))
	if attempt := harness.receive(t, "attempt"); !reflect.DeepEqual(attempt, original) {
		t.Fatalf("replayed retry attempt = %#v, want exact immutable envelope %#v", attempt, original)
	}
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryEmitted ||
		outcome.Retry != 1 || outcome.DispositionItemID != first.ItemID {
		t.Fatalf("emitted retry outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Armed != 0 || state.Retried != 1 {
		t.Fatalf("emitted retry state = %+v", state)
	}

	terminal := intentDispositionRetryDisposition(probe, IntentDispositionContinue, "continue-1", 300)
	harness.send(t, "disposition", terminal)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetrySettled ||
		outcome.Disposition != IntentDispositionContinue || outcome.Retry != 1 {
		t.Fatalf("terminal retry outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Pending != 0 || state.Armed != 0 || state.Settled != 1 {
		t.Fatalf("terminal retry state = %+v", state)
	}
	harness.scheduler.AdvanceNS(uint64(time.Second))
	harness.assertNoEnvelope(t, "attempt")
}

func TestIntentDispositionRetryUsesBoundedExponentialBackoffAndTypedExhaustion(t *testing.T) {
	harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
		InitialDelayMS: 5, BackoffFactor: 2, MaxDelayMS: 8, MaxRetries: 2,
		MaxElapsedMS: 100, MaxPending: 2, TerminalMemory: 4, CancelMemory: 4,
	})
	defer harness.stop(t)
	probe := intentDispositionRetryProbe(t, 1)
	harness.send(t, "probe", intentDispositionProbeEnvelope(probe))
	_ = harness.receive(t, "attempt")
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)

	for retry, delay := range []int{5, 8} {
		disposition := intentDispositionRetryDisposition(
			probe, IntentDispositionIndeterminate, "indeterminate-"+string(rune('a'+retry)),
			uint64(200+retry*20),
		)
		harness.send(t, "disposition", disposition)
		if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryScheduled ||
			outcome.Retry != retry+1 || outcome.DelayMS != delay {
			t.Fatalf("retry %d scheduled outcome = %+v", retry+1, outcome)
		}
		_ = harness.receiveState(t)
		harness.scheduler.AdvanceNS(uint64(time.Duration(delay) * time.Millisecond))
		_ = harness.receive(t, "attempt")
		if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryEmitted ||
			outcome.Retry != retry+1 {
			t.Fatalf("retry %d emitted outcome = %+v", retry+1, outcome)
		}
		_ = harness.receiveState(t)
	}

	last := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "indeterminate-final", 260)
	harness.send(t, "disposition", last)
	exhaustedEnvelope := harness.receive(t, "exhausted")
	exhausted, ok := exhaustedEnvelope.Payload.(IntentDispositionRetryExhaustion)
	if !ok || exhausted.Code != "max_retries" || exhausted.Retries != 2 ||
		!reflect.DeepEqual(exhausted.Probe, probe) || exhausted.LastDispositionItemID != last.ItemID ||
		!exhaustedEnvelope.Type.Equal(IntentDispositionRetryExhaustedType()) {
		t.Fatalf("typed retry exhaustion = %#v payload=%+v", exhaustedEnvelope, exhausted)
	}
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryExhausted ||
		outcome.Code != "max_retries" || outcome.Retry != 2 {
		t.Fatalf("retry exhaustion outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Pending != 0 || state.Armed != 0 || state.Exhausted != 1 {
		t.Fatalf("retry exhaustion state = %+v", state)
	}
	harness.scheduler.AdvanceNS(uint64(time.Second))
	harness.assertNoEnvelope(t, "attempt")
}

func TestIntentDispositionRetryCancellationAndResetQuiesceBeforeExactForwarding(t *testing.T) {
	for _, operation := range []string{"cancel", "reset"} {
		t.Run(operation, func(t *testing.T) {
			harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
				InitialDelayMS: 10, BackoffFactor: 2, MaxDelayMS: 20, MaxRetries: 2,
				MaxElapsedMS: 100, MaxPending: 2, TerminalMemory: 4, CancelMemory: 4,
			})
			defer harness.stop(t)
			probe := intentDispositionRetryProbe(t, 1)
			harness.send(t, "probe", intentDispositionProbeEnvelope(probe))
			_ = harness.receive(t, "attempt")
			_ = harness.receiveOutcome(t)
			_ = harness.receiveState(t)
			harness.send(t, "disposition", intentDispositionRetryDisposition(
				probe, IntentDispositionIndeterminate, operation+"-indeterminate", 200,
			))
			_ = harness.receiveOutcome(t)
			_ = harness.receiveState(t)

			var input, output string
			var control element.Envelope
			if operation == "cancel" {
				input, output = "cancel", "forwarded_cancel"
				payload := IntentSettlementCancellation{
					SessionID: probe.SessionID, DurableIntent: probe.DurableIntent, Reason: "user interrupted",
				}
				control = element.Envelope{
					Type: IntentSettlementCancelType(), ItemID: "cancel-retry", SessionID: probe.SessionID,
					CancellationScope: probe.DurableIntent.TrajectoryItemID,
					CausalParents:     []string{probe.ProbeID}, Payload: payload,
				}
			} else {
				input, output = "reset", "forwarded_reset"
				payload := IntentSettlementAddress{
					SessionID: probe.SessionID, DurableIntent: probe.DurableIntent, Reason: "operator reset",
				}
				control = element.Envelope{
					Type: IntentSettlementResetType(), ItemID: "reset-retry", SessionID: probe.SessionID,
					CausalParents: []string{probe.ProbeID}, Payload: payload,
				}
			}
			harness.send(t, input, control)
			if forwarded := harness.receive(t, output); !reflect.DeepEqual(forwarded, control) {
				t.Fatalf("forwarded %s = %#v, want exact control %#v", operation, forwarded, control)
			}
			outcome := harness.receiveOutcome(t)
			if operation == "cancel" && (outcome.Kind != IntentDispositionRetryCanceled || outcome.Code != "canceled") {
				t.Fatalf("cancellation outcome = %+v", outcome)
			}
			if operation == "reset" && (outcome.Kind != IntentDispositionRetryReset || outcome.Code != "reset") {
				t.Fatalf("reset outcome = %+v", outcome)
			}
			if state := harness.receiveState(t); state.Pending != 0 || state.Armed != 0 {
				t.Fatalf("%s did not quiesce retry state: %+v", operation, state)
			}
			harness.scheduler.AdvanceNS(uint64(time.Second))
			harness.assertNoEnvelope(t, "attempt")
		})
	}
}

func TestIntentDispositionRetryRefusesForgedAndReorderedEvidenceWithoutCorruptingTimer(t *testing.T) {
	harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
		InitialDelayMS: 10, BackoffFactor: 2, MaxDelayMS: 20, MaxRetries: 2,
		MaxElapsedMS: 100, MaxPending: 2, TerminalMemory: 4, CancelMemory: 4,
	})
	defer harness.stop(t)
	probe := intentDispositionRetryProbe(t, 1)

	unknown := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "unknown-disposition", 200)
	harness.send(t, "disposition", unknown)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryRefused || outcome.Code != "unknown_probe" {
		t.Fatalf("unknown disposition outcome = %+v", outcome)
	}
	_ = harness.receiveState(t)

	harness.send(t, "probe", intentDispositionProbeEnvelope(probe))
	_ = harness.receive(t, "attempt")
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	first := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "accepted-indeterminate", 220)
	harness.send(t, "disposition", first)
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)

	harness.send(t, "disposition", first)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryIgnored ||
		outcome.Code != "duplicate_disposition" {
		t.Fatalf("duplicate disposition outcome = %+v", outcome)
	}
	_ = harness.receiveState(t)

	forged := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "forged-disposition", 210)
	forgedPayload := forged.Payload.(IntentDisposition)
	forgedPayload.Probe.Result.CallID = "forged-call"
	forged.Payload = forgedPayload
	harness.send(t, "disposition", forged)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryRefused ||
		outcome.Code != "probe_mismatch" {
		t.Fatalf("forged disposition outcome = %+v", outcome)
	}
	_ = harness.receiveState(t)

	reordered := intentDispositionRetryDisposition(probe, IntentDispositionIndeterminate, "reordered-disposition", 215)
	harness.send(t, "disposition", reordered)
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryRefused ||
		outcome.Code != "non_monotonic_disposition" {
		t.Fatalf("reordered disposition outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Armed != 1 || state.Pending != 1 {
		t.Fatalf("refused evidence corrupted armed retry: %+v", state)
	}

	harness.scheduler.AdvanceNS(uint64(10 * time.Millisecond))
	_ = harness.receive(t, "attempt")
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryEmitted ||
		outcome.DispositionItemID != first.ItemID {
		t.Fatalf("retry used corrupted disposition lineage: %+v", outcome)
	}
	_ = harness.receiveState(t)
}

func TestIntentDispositionRetryBoundsPendingTerminalAndCancellationMemory(t *testing.T) {
	harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
		InitialDelayMS: 10, BackoffFactor: 1, MaxDelayMS: 10, MaxRetries: 1,
		MaxElapsedMS: 100, MaxPending: 1, TerminalMemory: 1, CancelMemory: 1,
	})
	defer harness.stop(t)
	first := intentDispositionRetryProbe(t, 1)
	second := intentDispositionRetryProbe(t, 2)
	harness.send(t, "probe", intentDispositionProbeEnvelope(first))
	_ = harness.receive(t, "attempt")
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	harness.send(t, "probe", intentDispositionProbeEnvelope(second))
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryRefused ||
		outcome.Code != "capacity_exhausted" {
		t.Fatalf("pending-capacity outcome = %+v", outcome)
	}
	if state := harness.receiveState(t); state.Pending != 1 || state.MaxPending != 1 {
		t.Fatalf("pending-capacity state = %+v", state)
	}

	harness.send(t, "disposition", intentDispositionRetryDisposition(
		first, IntentDispositionContinue, "first-terminal", 200,
	))
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	harness.send(t, "probe", intentDispositionProbeEnvelope(second))
	_ = harness.receive(t, "attempt")
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	harness.send(t, "disposition", intentDispositionRetryDisposition(
		second, IntentDispositionContinue, "second-terminal", 220,
	))
	_ = harness.receiveOutcome(t)
	if state := harness.receiveState(t); state.TerminalEntries != 1 {
		t.Fatalf("terminal memory is not bounded: %+v", state)
	}

	// The first tombstone was deliberately evicted at the configured bound, so
	// the exact old probe can be tracked again instead of growing memory forever.
	harness.send(t, "probe", intentDispositionProbeEnvelope(first))
	_ = harness.receive(t, "attempt")
	if outcome := harness.receiveOutcome(t); outcome.Kind != IntentDispositionRetryObserved {
		t.Fatalf("evicted terminal probe outcome = %+v", outcome)
	}
	_ = harness.receiveState(t)
}

func TestIntentDispositionRetryStopsPromptlyWithArmedTimer(t *testing.T) {
	harness := mountIntentDispositionRetry(t, IntentDispositionRetryConfig{
		InitialDelayMS: 10, BackoffFactor: 2, MaxDelayMS: 20, MaxRetries: 2,
		MaxElapsedMS: 100, MaxPending: 2, TerminalMemory: 4, CancelMemory: 4,
	})
	probe := intentDispositionRetryProbe(t, 1)
	harness.send(t, "probe", intentDispositionProbeEnvelope(probe))
	_ = harness.receive(t, "attempt")
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	harness.send(t, "disposition", intentDispositionRetryDisposition(
		probe, IntentDispositionIndeterminate, "armed-at-shutdown", 200,
	))
	_ = harness.receiveOutcome(t)
	_ = harness.receiveState(t)
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("armed retry stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("armed retry did not stop promptly")
	}
	harness.scheduler.AdvanceNS(uint64(time.Second))
}

func TestIntentDispositionRetryDescriptorAndConfigAreExplicitAndBounded(t *testing.T) {
	descriptor := IntentDispositionRetryDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Revision != 1 || !descriptor.Reaction.BreaksCycles ||
		!reflect.DeepEqual(descriptor.Reaction.Interrupts, []string{"cancel", "reset"}) ||
		descriptor.ConfigSchema != "schema://openrealtime/policy/intent-disposition-retry-config/v1" {
		t.Fatalf("retry descriptor = %+v", descriptor)
	}
	for _, source := range []string{
		`{"initial_delay_ms":0}`, `{"backoff_factor":17}`, `{"max_retries":0}`,
		`{"max_elapsed_ms":1,"initial_delay_ms":2}`, `{"max_pending":4097}`,
		`{"initial_delay_ms":null}`, `{"unexpected":true}`,
	} {
		if _, err := decodeIntentDispositionRetryConfig(json.RawMessage(source)); err == nil {
			t.Fatalf("invalid retry config accepted: %s", source)
		}
	}
	config, err := decodeIntentDispositionRetryConfig(json.RawMessage(`{}`))
	if err != nil || config != (IntentDispositionRetryConfig{
		InitialDelayMS: 100, BackoffFactor: 2, MaxDelayMS: 1_000, MaxRetries: 3,
		MaxElapsedMS: 5_000, MaxPending: 64, TerminalMemory: 512, CancelMemory: 256,
	}) {
		t.Fatalf("default retry config = %+v, error=%v", config, err)
	}
}

func mountIntentDispositionRetry(
	t *testing.T, config IntentDispositionRetryConfig,
) intentDispositionRetryHarness {
	t.Helper()
	graph := compileIntentDispositionRetryGraph(t)
	registry := graphruntime.NewRegistry()
	if err := registry.Register("policy.IntentDispositionRetry", intentDispositionRetryFactory{}); err != nil {
		t.Fatal(err)
	}
	scheduler := internalclock.NewManual(uint64(time.Millisecond))
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(IntentDispositionRetrySchedulerService, scheduler); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"retry": payload}, Now: scheduler.NowNS,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := intentDispositionRetryHarness{
		mounted: mounted, scheduler: scheduler, done: done, cancel: cancel,
	}
	startup := harness.receiveState(t)
	if startup.Revision != 0 || startup.Pending != 0 || startup.MaxPending != config.MaxPending {
		t.Fatalf("retry startup state = %+v", startup)
	}
	return harness
}

func compileIntentDispositionRetryGraph(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("intent-disposition-retry-test.ortg", []byte(intentDispositionRetryTestGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := catalog.Register(IntentDispositionRetryDescriptor()); err != nil {
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

func intentDispositionRetryProbe(t *testing.T, sequence uint64) IntentSettlementProbe {
	t.Helper()
	_, probe := intentDispositionProducerFixture(t, false)
	probe.Sequence = sequence
	probe.IssuedNS = 100 + sequence
	probe.ProbeID = ""
	probeID, err := intentSettlementProbeID(probe)
	if err != nil {
		t.Fatal(err)
	}
	probe.ProbeID = probeID
	return probe
}

func intentDispositionRetryDisposition(
	probe IntentSettlementProbe, kind IntentDispositionKind, itemID string, started uint64,
) element.Envelope {
	disposition := IntentDisposition{
		Probe: cloneIntentSettlementProbe(probe), Detector: probe.Detector, Kind: kind,
		DecisionStartedNS: started, DecisionFinishedNS: started + 5,
	}
	return element.Envelope{
		Type: IntentDispositionType(), ItemID: itemID, SessionID: probe.SessionID,
		SourceID: probe.Detector.Reference, RunID: probe.Result.InvocationID,
		CancellationScope: probe.DurableIntent.TrajectoryItemID,
		CausalParents:     []string{probe.ProbeID}, Payload: disposition,
	}
}

func (harness intentDispositionRetryHarness) send(
	t *testing.T, port string, envelope element.Envelope,
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

func (harness intentDispositionRetryHarness) receive(t *testing.T, port string) element.Envelope {
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

func (harness intentDispositionRetryHarness) receiveOutcome(t *testing.T) IntentDispositionRetryOutcome {
	t.Helper()
	envelope := harness.receive(t, "outcome")
	outcome, ok := envelope.Payload.(IntentDispositionRetryOutcome)
	if !ok || !envelope.Type.Equal(IntentDispositionRetryOutcomeType()) {
		t.Fatalf("retry outcome envelope = %#v", envelope)
	}
	return outcome
}

func (harness intentDispositionRetryHarness) receiveState(t *testing.T) IntentDispositionRetryState {
	t.Helper()
	envelope := harness.receive(t, "state")
	state, ok := envelope.Payload.(IntentDispositionRetryState)
	if !ok || !envelope.Type.Equal(IntentDispositionRetryStateType()) {
		t.Fatalf("retry state envelope = %#v", envelope)
	}
	return state
}

func (harness intentDispositionRetryHarness) assertNoEnvelope(t *testing.T, port string) {
	t.Helper()
	input, err := harness.mounted.Egress(port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected retry %s envelope: %#v", port, envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("retry %s no-envelope check failed: %v", port, err)
	}
}

func (harness intentDispositionRetryHarness) stop(t *testing.T) {
	t.Helper()
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("intent disposition retry stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("intent disposition retry did not stop")
	}
}
