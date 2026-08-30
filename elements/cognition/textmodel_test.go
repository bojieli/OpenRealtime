package cognition_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const textModelGraph = `graph graph_native_text_model {
    cognition.TextModel :: model;
    input context = model.context;
    input trigger = model.trigger;
    input cancel = model.cancel;
    output text = model.text;
    output result = model.result;
    output tools = model.tools;
    output outcome = model.outcome;
    output resolved = model.resolved;
}
`

var testDescriptor = continuation.Descriptor{
	Provider: "test", Model: "prepared-model", Phase: trajectory.PhaseFast,
	Effort: continuation.EffortMinimal, Streaming: true,
	NativeStateType: "test.state", ToolAuthority: continuation.ToolAuthorityPropose,
	// Legacy speech authority remains resolution evidence; the element still
	// exposes prepared text so graph topology decides whether it is routed.
	SpeechAuthority: continuation.SpeechAuthoritySilent,
}

func TestTextModelDescriptorMakesPreparedAndControlBoundariesExplicit(t *testing.T) {
	descriptor := cognitionelements.TextModelDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"context":  "State<trajectory.Snapshot>",
		"trigger":  "Trigger<cognition.Generate>",
		"cancel":   "Interrupt<flow.RunID>",
		"text":     "Segmented<text.PreparedDelta, flow.RunID>",
		"result":   "Event<cognition.Result>",
		"tools":    "Stream<tool.Proposal>",
		"outcome":  "Event<cognition.Outcome>",
		"resolved": "State<cognition.ProviderResolution>",
	}
	for name, want := range checks {
		port, found := descriptor.Port(name)
		if !found || port.Type.String() != want {
			t.Fatalf("port %s = %+v, want %s", name, port, want)
		}
	}
	if !reflect.DeepEqual(descriptor.Reaction.SampledState, []string{"context"}) ||
		!reflect.DeepEqual(descriptor.Reaction.Triggers, []string{"trigger"}) ||
		!reflect.DeepEqual(descriptor.Reaction.Interrupts, []string{"cancel"}) {
		t.Fatalf("reaction = %+v", descriptor.Reaction)
	}
}

func TestTextModelSamplesContextAndEmitsPreparedTypedOutputs(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor, seen: make(chan continuation.Request, 1), release: make(chan struct{}),
		events: []continuation.Event{
			{Kind: continuation.EventReasoningDelta, Text: "checking"},
			{Kind: continuation.EventAssistantDelta, Text: "hello "},
			{Kind: continuation.EventAssistantDelta, Text: "world"},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"query":"weather"}`),
			}},
		},
		completion: continuation.Completion{
			StopReason: "stop", ProviderStateType: "test.state",
			ProviderState: json.RawMessage(`{"turn":"opaque"}`),
		},
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary","retain_reasoning":true}`))
	defer stopTextModel(t, mounted, done, stop)

	resolution := receive(t, mustEgress(t, mounted, "resolved")).Payload.(cognitionelements.ProviderResolution)
	if resolution.Reference != "primary" || !reflect.DeepEqual(resolution.Descriptor, testDescriptor) ||
		!strings.HasPrefix(resolution.DescriptorDigest, "sha256:") || len(resolution.DescriptorDigest) != 71 ||
		resolution.RegistryRevision == 0 {
		t.Fatalf("resolution = %+v", resolution)
	}
	assertTextModelLiveResolution(t, mounted, resolution.DescriptorDigest)

	observation := trajectory.Item{
		ID: "observation-1", Kind: trajectory.KindObservation, MonotonicNS: 10,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, SourceRevision: 7,
		Content: "what is the weather?", CausalParentIDs: []string{"root"},
		Event: &trajectory.EventMetadata{EventID: "event-1", Source: "microphone"},
	}
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{observation}}
	send(t, mustIngress(t, mounted, "context"), element.Envelope{
		Type: element.State(element.Named("trajectory.Snapshot")), ItemID: "context-1", Payload: snapshot,
	})
	generate := cognitionelements.Generate{
		ExpectedContextVersion: uint64Pointer(1),
		Invocation: continuation.Invocation{
			Instruction: "answer briefly", SourceRevision: 7,
			Tools: []continuation.ToolDefinition{{
				Name: "lookup", Description: "look up data", Parameters: json.RawMessage(`{"type":"object"}`),
			}},
		},
	}
	send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
		Type: element.Trigger(element.Named("cognition.Generate")), ItemID: "trigger-1",
		RunID: "run-1", TraceID: "trace-1", Payload: generate,
	})

	var request continuation.Request
	select {
	case request = <-provider.seen:
	case <-time.After(time.Second):
		t.Fatal("provider was not invoked")
	}
	// Both graph payloads are immutable by contract, but the model boundary
	// still takes defensive copies so a bad producer cannot race or corrupt a
	// provider request already in flight.
	snapshot.Items[0].Content = "mutated"
	snapshot.Items[0].CausalParentIDs[0] = "mutated"
	snapshot.Items[0].Event.Source = "mutated"
	generate.Invocation.Tools[0].Parameters[0] = '['
	if request.Trajectory.Items[0].Content != "what is the weather?" ||
		request.Trajectory.Items[0].CausalParentIDs[0] != "root" ||
		request.Trajectory.Items[0].Event.Source != "microphone" ||
		string(request.Invocation.Tools[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("provider request aliases graph payloads: %+v", request)
	}
	if request.InvocationID != "run-1" || request.Trajectory.Version != 1 ||
		request.Invocation.Instruction != "answer briefly" {
		t.Fatalf("provider request = %+v", request)
	}
	close(provider.release)

	textPort := mustEgress(t, mounted, "text")
	wantBoundaries := []cognitionelements.TextBoundary{
		cognitionelements.TextBegin, cognitionelements.TextChunk,
		cognitionelements.TextChunk, cognitionelements.TextEnd,
	}
	var text string
	for index, want := range wantBoundaries {
		envelope := receive(t, textPort)
		delta, ok := envelope.Payload.(cognitionelements.PreparedTextDelta)
		if !ok || delta.Boundary != want || delta.Index != uint64(index) || envelope.RunID != "run-1" {
			t.Fatalf("text segment %d = %#v, envelope %+v", index, envelope.Payload, envelope)
		}
		text += delta.Text
	}
	if text != "hello world" {
		t.Fatalf("streamed text = %q", text)
	}
	proposalEnvelope := receive(t, mustEgress(t, mounted, "tools"))
	proposal := proposalEnvelope.Payload.(cognitionelements.ToolProposal)
	if proposal.Call.Name != "lookup" || !proposal.Declared ||
		proposal.ProviderAuthority != continuation.ToolAuthorityPropose {
		t.Fatalf("tool proposal = %+v", proposal)
	}
	if !contains(proposalEnvelope.CausalParents, "observation-1") {
		t.Fatalf("tool proposal is not bound to context tail: %v", proposalEnvelope.CausalParents)
	}
	resultEnvelope := receive(t, mustEgress(t, mounted, "result"))
	result := resultEnvelope.Payload.(cognitionelements.Result)
	if result.RunID != "run-1" || result.ContextVersion != 1 || result.ContextTailID != "observation-1" ||
		result.AssistantText != "hello world" || result.ReasoningText != "checking" || result.Interrupted ||
		len(result.Outputs) != 3 || result.Outputs[0].Kind != cognitionelements.PreparedReasoning ||
		result.Outputs[1].Kind != cognitionelements.PreparedAssistant ||
		result.Outputs[2].Kind != cognitionelements.PreparedTool || len(result.ToolProposals) != 1 {
		t.Fatalf("prepared result = %+v", result)
	}
	if !contains(resultEnvelope.CausalParents, "trigger-1") ||
		!contains(resultEnvelope.CausalParents, "context-1") {
		t.Fatalf("result causal parents = %v", resultEnvelope.CausalParents)
	}
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if outcome.Kind != cognitionelements.OutcomeSucceeded || outcome.RunID != "run-1" ||
		outcome.ContextVersion != 1 || outcome.FinishedNS < outcome.StartedNS {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestTextModelAddressedInterruptCancelsOnlyTheActiveRunAndClosesProvider(t *testing.T) {
	provider := &blockingProvider{descriptor: testDescriptor, entered: make(chan continuation.Request, 1)}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer func() {
		stopTextModel(t, mounted, done, stop)
		if provider.closed.Load() != 1 {
			t.Errorf("provider close count = %d, want 1", provider.closed.Load())
		}
	}()
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "cancel-unaddressed",
		Payload: cognitionelements.Cancel{},
	})
	unaddressed := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if unaddressed.Kind != cognitionelements.OutcomeRefused || unaddressed.Code != "missing_run_id" {
		t.Fatalf("unaddressed interrupt = %+v", unaddressed)
	}
	sendContext(t, mounted, 1, "context-1")
	sendGenerate(t, mounted, "run-active", 1)
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not enter")
	}
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "cancel-conflict",
		RunID: "run-active", Payload: cognitionelements.Cancel{RunID: "run-other"},
	})
	conflict := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if conflict.Kind != cognitionelements.OutcomeRefused || conflict.Code != "conflicting_run_id" {
		t.Fatalf("conflicting interrupt = %+v", conflict)
	}

	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "cancel-other",
		RunID: "run-other", Payload: cognitionelements.Cancel{RunID: "run-other"},
	})
	ignored := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if ignored.Kind != cognitionelements.OutcomeIgnored || ignored.Code != "run_not_active" {
		t.Fatalf("mismatched interrupt = %+v", ignored)
	}
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "cancel-active",
		RunID: "run-active", Payload: cognitionelements.Cancel{Reason: "barge-in"},
	})
	result := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	if !result.Interrupted || result.RunID != "run-active" {
		t.Fatalf("canceled result = %+v", result)
	}
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if outcome.Kind != cognitionelements.OutcomeCanceled || outcome.Code != "canceled" ||
		outcome.Message != "barge-in" {
		t.Fatalf("cancel outcome = %+v", outcome)
	}
	select {
	case cause := <-provider.canceled:
		if cause == nil || !strings.Contains(cause.Error(), "barge-in") {
			t.Fatalf("provider cancel cause = %v", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("provider did not observe cancellation")
	}
}

func TestTextModelTurnsInvalidProviderEventsIntoTypedFailure(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		events:     []continuation.Event{{Kind: continuation.EventAssistantDelta}},
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 1, "context-1")
	sendGenerate(t, mounted, "run-invalid", 1)
	result := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if !result.Interrupted || outcome.Kind != cognitionelements.OutcomeFailed ||
		outcome.Code != "invalid_provider_event" {
		t.Fatalf("invalid event result/outcome = %+v / %+v", result, outcome)
	}
	assertNoEnvelope(t, mustEgress(t, mounted, "text"))
}

func TestTextModelCanBindGenerationToExplicitEmptyContext(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		events:     []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "hello"}},
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 0, "context-empty")
	sendGenerate(t, mounted, "run-empty", 0)
	for range 3 {
		_ = receive(t, mustEgress(t, mounted, "text"))
	}
	result := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if result.ContextVersion != 0 || outcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("empty-context result/outcome = %+v / %+v", result, outcome)
	}
}

func TestTextModelRefusesSameVersionFromDifferentContextEnvelope(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		seen:       make(chan continuation.Request, 1),
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 1, "context-authenticated")
	version := uint64(1)
	send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
		Type: element.Trigger(element.Named("cognition.Generate")), ItemID: "trigger-wrong-context",
		RunID: "run-wrong-context", Payload: cognitionelements.Generate{
			ExpectedContextVersion: &version,
			ExpectedContextItemID:  "context-forged",
			Invocation:             continuation.Invocation{Instruction: "answer"},
		},
	})
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if outcome.Kind != cognitionelements.OutcomeRefused ||
		outcome.Code != "context_identity_mismatch" || outcome.ContextVersion != 1 {
		t.Fatalf("identity mismatch outcome = %+v", outcome)
	}
	select {
	case request := <-provider.seen:
		t.Fatalf("provider received mismatched context request: %+v", request)
	case <-time.After(20 * time.Millisecond):
	}
	send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
		Type: element.Trigger(element.Named("cognition.Generate")), ItemID: "trigger-incomplete-context",
		RunID: "run-incomplete-context", Payload: cognitionelements.Generate{
			ExpectedContextItemID: "context-authenticated",
			Invocation:            continuation.Invocation{Instruction: "answer"},
		},
	})
	incomplete := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if incomplete.Kind != cognitionelements.OutcomeRefused ||
		incomplete.Code != "incomplete_context_constraint" {
		t.Fatalf("incomplete context constraint outcome = %+v", incomplete)
	}
	assertNoEnvelope(t, mustEgress(t, mounted, "result"))
}

func TestTextModelDoesNotRetainPlaintextReasoningByDefault(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		events:     []continuation.Event{{Kind: continuation.EventReasoningDelta, Text: "private chain"}},
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 0, "context-empty")
	sendGenerate(t, mounted, "run-private", 0)
	result := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if result.ReasoningText != "" || len(result.Outputs) != 0 ||
		outcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("reasoning retention result/outcome = %+v / %+v", result, outcome)
	}
	assertNoEnvelope(t, mustEgress(t, mounted, "text"))
}

func TestTextModelClosesEmitCallbackWhenProviderReturns(t *testing.T) {
	provider := &lateEmitProvider{
		descriptor: testDescriptor, release: make(chan struct{}), emitted: make(chan error, 1),
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 1, "context-1")
	sendGenerate(t, mounted, "run-late", 1)
	_ = receive(t, mustEgress(t, mounted, "result"))
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if outcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("outcome = %+v", outcome)
	}
	close(provider.release)
	select {
	case err := <-provider.emitted:
		if err == nil || !strings.Contains(err.Error(), "after completion") {
			t.Fatalf("late emit error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late provider callback did not return")
	}
	assertNoEnvelope(t, mustEgress(t, mounted, "text"))
}

func TestTextModelSerializesStateSamplingWhileContextAndCancelRace(t *testing.T) {
	provider := &versionProvider{
		descriptor: testDescriptor, versions: make(chan uint64, 2), firstEntered: make(chan struct{}),
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 1, "context-1")
	sendGenerate(t, mounted, "run-1", 1)
	select {
	case <-provider.firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first generation did not start")
	}

	updatesDone := make(chan error, 1)
	contextIngress := mustIngress(t, mounted, "context")
	go func() {
		for version := uint64(2); version <= 64; version++ {
			updateCtx, updateCancel := context.WithTimeout(context.Background(), time.Second)
			_, err := contextIngress.Broadcast(updateCtx, element.Envelope{
				Type:    element.State(element.Named("trajectory.Snapshot")),
				ItemID:  "context-" + time.Duration(version).String(),
				Payload: trajectory.Snapshot{Version: version},
			})
			updateCancel()
			if err != nil {
				updatesDone <- err
				return
			}
		}
		updatesDone <- nil
	}()
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: element.Interrupt(element.Named("flow.RunID")), ItemID: "cancel-1",
		RunID: "run-1", Payload: cognitionelements.Cancel{Reason: "replace"},
	})
	_ = receive(t, mustEgress(t, mounted, "result"))
	firstOutcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if firstOutcome.Kind != cognitionelements.OutcomeCanceled {
		t.Fatalf("first outcome = %+v", firstOutcome)
	}
	select {
	case err := <-updatesDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("context updates did not drain")
	}
	sendGenerate(t, mounted, "run-2", 64)
	textPort := mustEgress(t, mounted, "text")
	for range 3 {
		_ = receive(t, textPort)
	}
	second := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	secondOutcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if second.ContextVersion != 64 || secondOutcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("second result/outcome = %+v / %+v", second, secondOutcome)
	}
	if first, secondVersion := <-provider.versions, <-provider.versions; first != 1 || secondVersion != 64 {
		t.Fatalf("sampled versions = %d, %d", first, secondVersion)
	}
}

func TestTextModelStrictConfigAndLiveDescriptorDriftFailBeforeReadiness(t *testing.T) {
	graph := compileTextModelGraph(t)
	registry := graphruntime.NewRegistry()
	if err := cognitionelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	providers := cognitionelements.NewProviderRegistry()
	var created atomic.Int32
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		created.Add(1)
		return &scriptedProvider{descriptor: testDescriptor}, nil
	}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(cognitionelements.ProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]json.RawMessage{
		"unknown":          json.RawMessage(`{"provider":"primary","mystery":true}`),
		"missing":          json.RawMessage(`{}`),
		"duplicate":        json.RawMessage(`{"provider":"primary","provider":"other"}`),
		"unbounded bytes":  json.RawMessage(`{"provider":"primary","max_output_bytes":999999999}`),
		"unbounded events": json.RawMessage(`{"provider":"primary","max_events":999999999}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry, Services: services,
				Values: map[string]json.RawMessage{"model": value},
			}); err == nil {
				t.Fatal("invalid values mounted")
			}
		})
	}
	if created.Load() != 0 {
		t.Fatalf("config validation created %d providers", created.Load())
	}

	drifted := &scriptedProvider{descriptor: testDescriptor}
	drifted.descriptor.Model = "different"
	drifted.closed = &atomic.Int32{}
	driftProviders := cognitionelements.NewProviderRegistry()
	if err := driftProviders.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return drifted, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountTextModel(t, driftProviders, json.RawMessage(`{"provider":"primary"}`))
	defer cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "descriptor drifted") {
			t.Fatalf("graph error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("descriptor drift did not fail readiness")
	}
	closeContext, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := mounted.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	if drifted.closed.Load() != 1 {
		t.Fatalf("drifted provider close count = %d", drifted.closed.Load())
	}
}

func TestTextModelRefusesMutableLiveModelIdentity(t *testing.T) {
	descriptor := testDescriptor
	descriptor.Model = "latest"
	provider := &scriptedProvider{descriptor: descriptor, closed: &atomic.Int32{}}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", descriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, done, cancel := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	defer cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "mutable or placeholder selector") {
			t.Fatalf("mutable cognition identity error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("text model published readiness for a mutable provider identity")
	}
	if provider.closed.Load() != 1 {
		t.Fatalf("provider close count after refused identity = %d", provider.closed.Load())
	}
}

func TestTextModelBoundsUntrustedProviderOutput(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		events: []continuation.Event{{
			Kind: continuation.EventAssistantDelta, Text: "too many bytes",
		}},
	}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers,
		json.RawMessage(`{"provider":"primary","max_output_bytes":4}`))
	defer stopTextModel(t, mounted, done, stop)
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	sendContext(t, mounted, 0, "empty-context")
	sendGenerate(t, mounted, "bounded", 0)
	result := receive(t, mustEgress(t, mounted, "result")).Payload.(cognitionelements.Result)
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if !result.Interrupted || result.AssistantText != "" ||
		outcome.Kind != cognitionelements.OutcomeFailed ||
		outcome.Code != "invalid_provider_event" ||
		!strings.Contains(outcome.Message, "4-byte output bound") {
		t.Fatalf("bounded result/outcome = %+v / %+v", result, outcome)
	}
}

func TestTextModelReportsProviderCloseFailure(t *testing.T) {
	provider := &closeErrorProvider{scriptedProvider: scriptedProvider{descriptor: testDescriptor}}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, cancel := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "close test provider") {
			t.Fatalf("shutdown error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("graph did not report provider close failure")
	}
}

type scriptedProvider struct {
	descriptor continuation.Descriptor
	seen       chan continuation.Request
	release    chan struct{}
	events     []continuation.Event
	completion continuation.Completion
	closed     *atomic.Int32
}

type closeErrorProvider struct{ scriptedProvider }

func (*closeErrorProvider) Close() error { return errors.New("close test provider") }

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if provider.seen != nil {
		provider.seen <- request
	}
	if provider.release != nil {
		select {
		case <-provider.release:
		case <-ctx.Done():
			return continuation.Completion{}, context.Cause(ctx)
		}
	}
	for _, event := range provider.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return provider.completion, nil
}

func (provider *scriptedProvider) Close() error {
	if provider.closed != nil {
		provider.closed.Add(1)
	}
	return nil
}

type blockingProvider struct {
	descriptor continuation.Descriptor
	entered    chan continuation.Request
	canceled   chan error
	closed     atomic.Int32
	once       sync.Once
}

func (provider *blockingProvider) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider *blockingProvider) Continue(
	ctx context.Context, request continuation.Request, _ continuation.Emit,
) (continuation.Completion, error) {
	provider.entered <- request
	<-ctx.Done()
	cause := context.Cause(ctx)
	provider.once.Do(func() {
		provider.canceled = make(chan error, 1)
		provider.canceled <- cause
	})
	return continuation.Completion{}, cause
}
func (provider *blockingProvider) Close() error { provider.closed.Add(1); return nil }

type versionProvider struct {
	descriptor   continuation.Descriptor
	versions     chan uint64
	firstEntered chan struct{}
	calls        atomic.Int32
}

type lateEmitProvider struct {
	descriptor continuation.Descriptor
	release    chan struct{}
	emitted    chan error
}

func (provider *lateEmitProvider) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider *lateEmitProvider) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	go func() {
		<-provider.release
		provider.emitted <- emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "too late"})
	}()
	return continuation.Completion{StopReason: "stop"}, nil
}

func (provider *versionProvider) Descriptor() continuation.Descriptor { return provider.descriptor }
func (provider *versionProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	provider.versions <- request.Trajectory.Version
	if provider.calls.Add(1) == 1 {
		close(provider.firstEntered)
		<-ctx.Done()
		return continuation.Completion{}, context.Cause(ctx)
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: "ready"}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func mountTextModel(
	t *testing.T, providers *cognitionelements.ProviderRegistry, values json.RawMessage,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := cognitionelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(cognitionelements.ProviderRegistryService, providers); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compileTextModelGraph(t), Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"model": values},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func stopTextModel(
	t *testing.T, _ *graphruntime.Mounted, done <-chan error, cancel context.CancelFunc,
) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("text model graph stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("text model graph did not stop")
	}
}

func compileTextModelGraph(t *testing.T) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("text-model.ortg", []byte(textModelGraph))
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := cognitionelements.RegisterDescriptors(catalog); err != nil {
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

func sendContext(t *testing.T, mounted *graphruntime.Mounted, version uint64, itemID string) {
	t.Helper()
	send(t, mustIngress(t, mounted, "context"), element.Envelope{
		Type: element.State(element.Named("trajectory.Snapshot")), ItemID: itemID,
		Payload: trajectory.Snapshot{Version: version},
	})
}

func sendGenerate(t *testing.T, mounted *graphruntime.Mounted, runID string, version uint64) {
	t.Helper()
	send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
		Type: element.Trigger(element.Named("cognition.Generate")), ItemID: "trigger-" + runID,
		RunID: runID, Payload: cognitionelements.Generate{
			ExpectedContextVersion: uint64Pointer(version),
			Invocation:             continuation.Invocation{Instruction: "answer"},
		},
	})
}

func mustIngress(t *testing.T, mounted *graphruntime.Mounted, name string) element.OutputPort {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func mustEgress(t *testing.T, mounted *graphruntime.Mounted, name string) element.InputPort {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func send(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sendWithContext(t, ctx, output, envelope)
}

func sendWithContext(t *testing.T, ctx context.Context, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Errorf("send %s: %v", output.Name(), err)
	}
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

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func uint64Pointer(value uint64) *uint64 { return &value }

func assertTextModelLiveResolution(
	t *testing.T, mounted *graphruntime.Mounted, descriptorDigest string,
) {
	t.Helper()
	resolution := mounted.Live().Nodes["model"].Resolution
	if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
		resolution.Runtime.ID != "builtin://openrealtime/elements/cognition.TextModel" ||
		resolution.Runtime.Revision != "implementation:2" ||
		resolution.CapabilitiesEvidence != inspect.EvidenceLive {
		t.Fatalf("text model live resolution = %+v", resolution)
	}
	for _, capability := range resolution.Capabilities {
		if capability.Name == "cognition.continuation" &&
			capability.Provider.ID == "model://test" &&
			capability.Provider.Revision == "prepared-model" &&
			capability.Provider.Digest == descriptorDigest && capability.Adapter != nil &&
			capability.Adapter.ID == "builtin://openrealtime/adapters/cognition.TextModel-continuation" &&
			capability.Adapter.Revision == "implementation:2" {
			return
		}
	}
	t.Fatalf("text model provider and adapter artifacts are not independently attested: %+v",
		resolution.Capabilities)
}
