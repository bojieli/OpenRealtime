package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type visualReflexPlannerReference struct {
	graph ir.Graph
	bound graphvalues.Bound
}

func TestVisualReflexPlannerReferenceIsLockedAndHasOneArbitratedEffectPath(t *testing.T) {
	reference := loadVisualReflexPlannerReference(t)
	if findings := graphvalidate.Check(reference.bound.Graph, graphvalidate.ComputerUse); len(findings) != 0 {
		t.Fatalf("visual reflex/planner warnings-as-errors findings = %+v", findings)
	}
	wantNodes := map[string]string{
		"reflex_activation":          "policy.GenerateOnObservation",
		"planner_activation":         "policy.GenerateOnObservation",
		"reflex_model":               "cognition.TextModel",
		"planner_model":              "cognition.TextModel",
		"reflex_control_quarantine":  "interaction.ControlSerializationQuarantine",
		"planner_control_quarantine": "interaction.ControlSerializationQuarantine",
		"reflex_provenance":          "authority.ProvenanceJoin",
		"planner_provenance":         "authority.ProvenanceJoin",
		"reflex_admission":           "authority.ProposalAdmission",
		"planner_admission":          "authority.ProposalAdmission",
		"action_arbiter":             "authority.ActionArbiter",
		"tool_lookup":                "action.ToolLookup",
		"dispatch":                   "action.Dispatch",
		"tool_result_commit":         "action.ToolResultCommit",
	}
	for id, want := range wantNodes {
		if got := silentNodeElement(reference.graph, id); got != want {
			t.Errorf("node %s = %q, want %q", id, got, want)
		}
	}
	for _, edge := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"reflex_model", "text", "reflex_control_quarantine", "text"},
		{"reflex_model", "result", "reflex_control_quarantine", "result"},
		{"planner_model", "text", "planner_control_quarantine", "text"},
		{"planner_model", "result", "planner_control_quarantine", "result"},
		{"reflex_control_quarantine", "safe_result", "reflex_result_copy", "in"},
		{"planner_control_quarantine", "safe_result", "planner_result_copy", "in"},
		{"reflex_candidate_copy", "out", "action_arbiter", "candidate"},
		{"planner_candidate_copy", "out", "action_arbiter", "candidate"},
		{"reflex_result_copy", "out", "action_arbiter", "result"},
		{"planner_result_copy", "out", "action_arbiter", "result"},
		{"reflex_admission", "admitted", "action_arbiter", "proposal"},
		{"planner_admission", "admitted", "action_arbiter", "proposal"},
		{"action_arbiter", "selected", "selected_action_copy", "in"},
		{"selected_action_copy", "out", "tool_lookup", "proposal"},
		{"action_arbiter", "cancel_upstream", "arbitration_cancel_copy", "in"},
		{"model_cancel_copy", "out", "reflex_model", "cancel"},
		{"model_cancel_copy", "out", "planner_model", "cancel"},
		{"dispatch_result_copy", "out", "tool_result_commit", "result"},
		{"tool_result_commit", "append", "trajectory_append_mux", "in"},
	} {
		if !silentHasEdge(reference.graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
			t.Errorf("missing multi-planner edge %s.%s -> %s.%s",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
		}
	}
	for _, edge := range reference.graph.Edges {
		if edge.To.Node == "tool_lookup" && edge.To.Port == "proposal" &&
			edge.From.Node != "selected_action_copy" {
			t.Errorf("action lane bypasses arbiter into tool lookup: %s", edge.ID)
		}
		if (edge.From.Node == "reflex_model" || edge.From.Node == "planner_model" ||
			edge.From.Node == "reflex_admission" || edge.From.Node == "planner_admission") &&
			edge.To.Node == "dispatch" {
			t.Errorf("cognition lane bypasses authority into dispatch: %s", edge.ID)
		}
	}
	external := make([]string, 0, 1)
	for _, node := range reference.graph.Nodes {
		for _, effect := range node.Effects {
			if effect.External {
				external = append(external, node.ID+":"+effect.Authority)
			}
		}
	}
	if !slices.Equal(external, []string{"dispatch:tool.ExecutableAction"}) {
		t.Fatalf("multi-planner external effects = %v", external)
	}
	for _, boundary := range reference.graph.Boundaries {
		if strings.Contains(strings.ToLower(boundary.Name), "audio") ||
			strings.Contains(strings.ToLower(boundary.Type.String()), "audio") {
			t.Errorf("visual reflex/planner graph exports audio boundary %+v", boundary)
		}
	}
	for name, wantType := range map[string]element.Type{
		"reflex_text":    interactionelements.SafePreparedTextType(),
		"planner_text":   interactionelements.SafePreparedTextType(),
		"reflex_result":  interactionelements.SafeModelResultType(),
		"planner_result": interactionelements.SafeModelResultType(),
		"reflex_control_serialization_quarantine":  interactionelements.ControlSerializationQuarantineType(),
		"planner_control_serialization_quarantine": interactionelements.ControlSerializationQuarantineType(),
	} {
		var found bool
		for _, boundary := range reference.graph.Boundaries {
			if boundary.Name == name && boundary.Direction == ir.OutputBoundary {
				found = true
				if !boundary.Type.Equal(wantType) {
					t.Errorf("boundary %s type = %s, want %s",
						name, boundary.Type.String(), wantType.String())
				}
			}
		}
		if !found {
			t.Errorf("missing quarantined model output boundary %s", name)
		}
	}
}

func TestVisualReflexPlannerReferenceExecutesRaceAuthorityAndVisualFeedback(t *testing.T) {
	reflex := newVisualLaneProvider("visual-reflex", trajectory.PhaseFast, true)
	planner := newVisualLaneProvider("visual-planner", trajectory.PhaseSlow, false)
	dispatcher := &silentReferenceDispatcher{}
	fixture := mountVisualReflexPlannerReference(t, reflex, planner, dispatcher)
	defer fixture.stop(t)

	fixture.sendObservation(t, "session-visual-race", "user-visual-task",
		trajectory.AuthorityUser, "user", "click the visible control", 1)
	reflex.awaitEntered(t, 1)
	planner.awaitEntered(t, 1)
	firstReflexRequest := reflex.receiveRequest(t)
	firstPlannerRequest := planner.receiveRequest(t)
	if firstReflexRequest.InvocationID == firstPlannerRequest.InvocationID ||
		firstReflexRequest.Descriptor.Phase != trajectory.PhaseFast ||
		firstPlannerRequest.Descriptor.Phase != trajectory.PhaseSlow {
		t.Fatalf("independent cognition lanes = reflex %+v planner %+v",
			firstReflexRequest.Descriptor, firstPlannerRequest.Descriptor)
	}
	close(reflex.releaseFirst)

	selected := fixture.receive(t, "selected_action").Payload.(actionelements.AdmittedProposal)
	if selected.ProviderReference != "deployment.visual-reflex" ||
		selected.ModelProducer.Phase != trajectory.PhaseFast ||
		selected.Proposal.Call.CallID != "visual-reflex-call" {
		t.Fatalf("selected reflex action = %+v", selected)
	}
	canceled := fixture.receive(t, "arbitration_model_cancel")
	if value, ok := canceled.Payload.(cognitionelements.Cancel); !ok ||
		value.RunID == "" || value.RunID == selected.ModelRunID {
		t.Fatalf("planner cancellation = %#v envelope=%+v", canceled.Payload, canceled)
	}
	planner.awaitCanceled(t)

	canonicalEnvelope := fixture.receive(t, "canonical_result")
	canonical, ok := canonicalEnvelope.Payload.(actionelements.CanonicalResult)
	if !ok || canonical.Execution.CallID != "visual-reflex-call" ||
		canonical.TrajectoryItemID == "" || canonical.StoreVersion == 0 || dispatcher.calls.Load() != 1 {
		t.Fatalf("canonical arbitrated effect = %#v calls=%d", canonicalEnvelope.Payload, dispatcher.calls.Load())
	}
	fixture.awaitArbitrationOutcome(t, "first_complete")

	// A subsequent screen observation is committed after the exact action
	// result. Both lanes sample that updated prefix and deliberately produce no
	// second action; this is the visual feedback loop, not a one-shot race.
	fixture.sendObservation(t, "session-visual-race", "screen-after-click",
		trajectory.AuthorityObserver, "screen", "the control is now selected", 2)
	reflex.awaitEntered(t, 2)
	planner.awaitEntered(t, 2)
	secondReflexRequest := reflex.receiveRequest(t)
	secondPlannerRequest := planner.receiveRequest(t)
	for name, request := range map[string]continuation.Request{
		"reflex": secondReflexRequest, "planner": secondPlannerRequest,
	} {
		var sawResult, sawFeedback bool
		for _, item := range request.Trajectory.Items {
			sawResult = sawResult || item.Kind == trajectory.KindToolResult
			sawFeedback = sawFeedback || (item.Kind == trajectory.KindObservation &&
				item.Observation != nil && item.Observation.Source == "screen")
		}
		if !sawResult || !sawFeedback {
			t.Fatalf("%s second prefix omitted action result or visual feedback: %+v",
				name, request.Trajectory.Items)
		}
	}
	fixture.awaitArbitrationOutcome(t, "no_action")
	if dispatcher.calls.Load() != 1 {
		t.Fatalf("visual feedback opened a second effect: calls=%d", dispatcher.calls.Load())
	}
}

func loadVisualReflexPlannerReference(t *testing.T) visualReflexPlannerReference {
	t.Helper()
	directory := filepath.Join("components", "visual-reflex-planner")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lockBody, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockBody)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Lock.Equal(lock) {
		t.Fatal("visual reflex/planner resolution lock is stale")
	}
	locked, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesBody, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("agent.values.yaml", valuesBody)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(locked.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	return visualReflexPlannerReference{graph: locked.Graph, bound: bound}
}

type visualReflexPlannerFixture struct {
	mounted *graphruntime.Mounted
	done    chan error
	cancel  context.CancelFunc
	store   *trajectory.Store
}

func mountVisualReflexPlannerReference(
	t *testing.T, reflex, planner *visualLaneProvider, dispatcher *silentReferenceDispatcher,
) *visualReflexPlannerFixture {
	t.Helper()
	reference := loadVisualReflexPlannerReference(t)
	store := trajectory.NewStore()
	ledger := legacyaction.NewLedger()
	providers := cognitionelements.NewProviderRegistry()
	for _, registration := range []struct {
		reference string
		provider  *visualLaneProvider
	}{
		{reference: "deployment.visual-reflex", provider: reflex},
		{reference: "deployment.visual-planner", provider: planner},
	} {
		if err := providers.Register(registration.reference, registration.provider.descriptor,
			func() (continuation.Provider, error) { return registration.provider, nil }); err != nil {
			t.Fatal(err)
		}
	}
	definition, found := computeruse.Lookup(computeruse.Click)
	if !found {
		t.Fatal("standard click definition is absent")
	}
	tools := actionelements.NewToolRegistries()
	if err := tools.Register("deployment.tools", []legacyaction.ToolSpec{{
		Name: computeruse.Click, Description: definition.Description, Parameters: definition.Parameters,
		Confirm: legacyaction.ConfirmNever, Target: "browser", Dispatcher: dispatcher,
	}}); err != nil {
		t.Fatal(err)
	}
	targets := actionelements.NewTargetRegistries()
	if err := targets.Register("deployment.browser", computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 720,
	}); err != nil {
		t.Fatal(err)
	}
	ledgers := actionelements.NewLedgerRegistries()
	if err := ledgers.Register("deployment.ledger", ledger); err != nil {
		t.Fatal(err)
	}
	confirmation := &silentReferenceConfirmation{}
	confirmations := actionelements.NewConfirmationProviders()
	if err := confirmations.Register("deployment.confirmation", confirmation.Name(),
		func() (actionelements.ConfirmationProvider, error) { return confirmation, nil }); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	for name, service := range map[string]any{
		cognitionelements.ProviderRegistryService:  providers,
		actionelements.TrajectoryStoreService:      store,
		stateelements.TrajectoryStoreService:       &stateelements.TrajectoryStoreServiceValue{Store: store},
		actionelements.ToolRegistryService:         tools,
		actionelements.TargetRegistryService:       targets,
		actionelements.LedgerRegistryService:       ledgers,
		actionelements.ConfirmationRegistryService: confirmations,
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: reference.bound.Graph, Registry: registry, Services: services,
		Values: reference.bound.Values, Now: func() uint64 { return now.Add(1) },
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	for _, name := range []string{
		"trajectory_snapshot", "reflex_activation_state", "planner_activation_state",
	} {
		port, portErr := mounted.Egress(name)
		if portErr != nil {
			cancel()
			t.Fatal(portErr)
		}
		go drainVisualReference(runContext, port)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		live := mounted.Live()
		ready := len(live.Nodes) == len(reference.bound.Graph.Nodes)
		for _, node := range live.Nodes {
			ready = ready && node.Resolution != nil &&
				string(node.Resolution.RuntimeEvidence) == "live" &&
				string(node.Resolution.CapabilitiesEvidence) == "live"
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("visual reflex/planner graph did not become ready: %+v", live.Nodes)
		}
		time.Sleep(time.Millisecond)
	}
	return &visualReflexPlannerFixture{
		mounted: mounted, done: done, cancel: cancel, store: store,
	}
}

func drainVisualReference(ctx context.Context, input element.InputPort) {
	for {
		if _, err := input.Receive(ctx); err != nil {
			return
		}
	}
}

func (fixture *visualReflexPlannerFixture) sendObservation(
	t *testing.T, sessionID, itemID string, authority trajectory.Authority,
	source, text string, revision uint64,
) {
	t.Helper()
	input, err := fixture.mounted.Ingress("observations")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := input.Broadcast(ctx, element.Envelope{
		Type: stateelements.ObservationType(), ItemID: itemID, SourceID: source,
		SessionID: sessionID, Payload: perception.Observation{
			Text: text, StableText: text, Observer: source, Source: source,
			Authority: authority, Revision: revision, Final: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *visualReflexPlannerFixture) receive(t *testing.T, name string) element.Envelope {
	t.Helper()
	input, err := fixture.mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		select {
		case runErr := <-fixture.done:
			t.Fatalf("receive %s: %v (graph stopped: %v)", name, err, runErr)
		default:
		}
		t.Fatalf("receive %s: %v", name, err)
	}
	return envelope
}

func (fixture *visualReflexPlannerFixture) awaitArbitrationOutcome(t *testing.T, code string) {
	t.Helper()
	for range 8 {
		outcome := fixture.receive(t, "arbitration_outcome").Payload.(actionelements.Outcome)
		if outcome.Code == code {
			return
		}
	}
	t.Fatalf("action arbitration never emitted outcome %q", code)
}

func (fixture *visualReflexPlannerFixture) stop(t *testing.T) {
	t.Helper()
	fixture.cancel()
	select {
	case err := <-fixture.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("visual reflex/planner graph stopped with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("visual reflex/planner graph did not stop")
	}
}

type visualLaneProvider struct {
	descriptor   continuation.Descriptor
	emitFirst    bool
	releaseFirst chan struct{}
	entered      chan int
	canceled     chan struct{}
	requests     chan continuation.Request
	calls        atomic.Int32
}

func newVisualLaneProvider(
	model string, phase trajectory.Phase, emitFirst bool,
) *visualLaneProvider {
	return &visualLaneProvider{
		descriptor: continuation.Descriptor{
			Provider: "test", Model: model, Phase: phase, Effort: continuation.EffortMinimal,
			Streaming: true, ToolAuthority: continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthoritySilent,
		},
		emitFirst: emitFirst, releaseFirst: make(chan struct{}), entered: make(chan int, 4),
		canceled: make(chan struct{}, 1), requests: make(chan continuation.Request, 4),
	}
}

func (provider *visualLaneProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *visualLaneProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	call := int(provider.calls.Add(1))
	provider.requests <- request
	provider.entered <- call
	if call != 1 {
		return continuation.Completion{StopReason: "no_action"}, nil
	}
	if !provider.emitFirst {
		<-ctx.Done()
		select {
		case provider.canceled <- struct{}{}:
		default:
		}
		return continuation.Completion{}, context.Cause(ctx)
	}
	select {
	case <-provider.releaseFirst:
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	}
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
		CallID: provider.descriptor.Model + "-call", Name: computeruse.Click,
		Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
	}}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

func (provider *visualLaneProvider) awaitEntered(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-provider.entered:
		if got != want {
			t.Fatalf("%s entered invocation %d, want %d", provider.descriptor.Model, got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not enter invocation %d", provider.descriptor.Model, want)
	}
}

func (provider *visualLaneProvider) awaitCanceled(t *testing.T) {
	t.Helper()
	select {
	case <-provider.canceled:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s was not canceled after losing arbitration", provider.descriptor.Model)
	}
}

func (provider *visualLaneProvider) receiveRequest(t *testing.T) continuation.Request {
	t.Helper()
	select {
	case request := <-provider.requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatalf("%s request was not retained", provider.descriptor.Model)
		return continuation.Request{}
	}
}
