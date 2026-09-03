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
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
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

type silentComputerUseReference struct {
	graph ir.Graph
	bound graphvalues.Bound
}

func TestSilentComputerUseReferenceIsLockedCompleteAndHasNoAuthorityBypass(t *testing.T) {
	reference := loadSilentComputerUseReference(t)
	if findings := graphvalidate.Check(reference.bound.Graph, graphvalidate.ComputerUse); len(findings) != 0 {
		t.Fatalf("computer-use warnings-as-errors findings = %+v", findings)
	}

	wantNodes := map[string]string{
		"observation_commit":       "state.ObservationCommit",
		"trajectory":               "state.TrajectoryStore",
		"activation":               "policy.GenerateOnObservation",
		"model":                    "cognition.TextModel",
		"model_control_quarantine": "interaction.ControlSerializationQuarantine",
		"model_result_commit":      "interaction.ModelResultCommit",
		"provenance_join":          "authority.ProvenanceJoin",
		"proposal_admission":       "authority.ProposalAdmission",
		"tool_lookup":              "action.ToolLookup",
		"confirmation":             "authority.Confirmation",
		"target_fence":             "authority.TargetFence",
		"canonical_call_commit":    "action.AuthorizedCallCommit",
		"ledger_commit":            "action.LedgerCommit",
		"dispatch":                 "action.Dispatch",
		"tool_result_commit":       "action.ToolResultCommit",
		"action_cancel_copy":       "flow.Tee",
		"action_timeout_copy":      "flow.Tee",
		"model_proposal_copy":      "flow.Tee",
		"model_result_copy":        "flow.Tee",
		"trajectory_append_mux":    "flow.Mux",
		"trajectory_snapshot_copy": "flow.Tee",
	}
	for id, want := range wantNodes {
		if got := silentNodeElement(reference.graph, id); got != want {
			t.Errorf("node %s = %q, want %q", id, got, want)
		}
	}

	for _, edge := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"observation_outcome_copy", "out", "activation", "committed"},
		{"activation", "authority", "provenance_join", "candidate"},
		{"model", "text", "model_control_quarantine", "text"},
		{"model", "result", "model_control_quarantine", "result"},
		{"model_control_quarantine", "safe_result", "model_result_copy", "in"},
		{"model_result_copy", "out", "model_result_commit", "result"},
		{"model_result_commit", "append", "trajectory_append_mux", "in"},
		{"model_proposal_copy", "out", "provenance_join", "proposal"},
		{"model_result_copy", "out", "provenance_join", "result"},
		{"model_proposal_copy", "out", "proposal_admission", "proposal"},
		{"provenance_copy", "out", "proposal_admission", "provenance"},
		{"proposal_admission", "admitted", "tool_lookup", "proposal"},
		{"tool_lookup", "declared", "confirmation", "action"},
		{"confirmation", "confirmed", "target_fence", "action"},
		{"target_fence", "authorized", "canonical_call_commit", "action"},
		{"canonical_call_commit", "append", "trajectory_append_mux", "in"},
		{"canonical_call_commit", "canonical", "ledger_commit", "action"},
		{"ledger_commit", "executable", "dispatch", "execute"},
		{"dispatch_result_copy", "out", "tool_result_commit", "result"},
		{"tool_result_commit", "append", "trajectory_append_mux", "in"},
		{"trajectory_committed_copy", "out", "model_result_commit", "committed"},
		{"trajectory_committed_copy", "out", "canonical_call_commit", "committed"},
		{"trajectory_committed_copy", "out", "tool_result_commit", "committed"},
		{"trajectory_rejected_copy", "out", "model_result_commit", "rejected"},
		{"trajectory_rejected_copy", "out", "canonical_call_commit", "rejected"},
		{"trajectory_rejected_copy", "out", "tool_result_commit", "rejected"},
	} {
		if !silentHasEdge(reference.graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
			t.Errorf("missing trust-chain edge %s.%s -> %s.%s",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
		}
	}
	for _, target := range []string{"model", "canonical_call_commit", "tool_result_commit"} {
		delivery, found := silentEdgeDelivery(
			reference.graph, "trajectory_snapshot_copy", "out", target, "context",
		)
		if !found || delivery != ir.Lossless {
			t.Errorf("required trajectory context edge to %s = %q, found=%t; want lossless",
				target, delivery, found)
		}
	}

	for _, node := range []string{
		"provenance_join", "proposal_admission", "confirmation", "canonical_call_commit",
		"ledger_commit", "dispatch", "tool_result_commit",
	} {
		if !silentHasEdge(reference.graph, "action_cancel_copy", "out", node, "cancel") {
			t.Errorf("action cancellation is not fanned to %s", node)
		}
		if !silentHasEdge(reference.graph, "action_timeout_copy", "out", node, "timeout") {
			t.Errorf("action timeout is not fanned to %s", node)
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
		t.Fatalf("external effect nodes = %v, want dispatch behind executable authority", external)
	}
	for _, edge := range reference.graph.Edges {
		if edge.To.Node == "dispatch" && edge.To.Port == "execute" &&
			(edge.From.Node != "ledger_commit" || edge.From.Port != "executable") {
			t.Errorf("authority bypass into dispatch: %s", edge.ID)
		}
		if edge.From.Node == "model" && edge.To.Node == "dispatch" {
			t.Errorf("model-to-effect bypass: %s", edge.ID)
		}
		if edge.From.Node == "model_proposal_copy" &&
			edge.To.Node != "provenance_join" && edge.To.Node != "proposal_admission" {
			t.Errorf("model proposal bypasses exact join/admission fanout: %s", edge.ID)
		}
	}
	for _, boundary := range reference.graph.Boundaries {
		if strings.Contains(strings.ToLower(boundary.Name), "audio") ||
			strings.Contains(strings.ToLower(boundary.Type.String()), "audio") {
			t.Errorf("silent computer-use graph exports audio boundary %+v", boundary)
		}
	}
	for name, wantType := range map[string]element.Type{
		"prepared_text":                    interactionelements.SafePreparedTextType(),
		"model_result":                     interactionelements.SafeModelResultType(),
		"control_serialization_quarantine": interactionelements.ControlSerializationQuarantineType(),
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

func TestSilentComputerUseReferenceExecutesCanonicalPolicyToResultChain(t *testing.T) {
	dispatcher := &silentReferenceDispatcher{}
	fixture := mountSilentComputerUseReference(t, legacyaction.ConfirmNever,
		&silentReferenceConfirmation{}, dispatcher)
	defer fixture.stop(t)

	fixture.sendObservation(t, "session-success", "user-input-success")
	fixture.awaitActivation(t)
	canonicalEnvelope := fixture.receive(t, "canonical_result")
	canonical, ok := canonicalEnvelope.Payload.(actionelements.CanonicalResult)
	if !ok {
		t.Fatalf("canonical result payload = %T", canonicalEnvelope.Payload)
	}
	if canonical.Execution.CallID != "reference-call" || canonical.Execution.Name != computeruse.Click ||
		canonical.Execution.Result.CallID != "reference-call" || canonical.TrajectoryItemID == "" ||
		canonical.StoreVersion != fixture.store.Snapshot().Version || dispatcher.calls.Load() != 1 {
		t.Fatalf("canonical execution/result = %+v, calls=%d, snapshot=%+v",
			canonical, dispatcher.calls.Load(), fixture.store.Snapshot())
	}

	provenanceEnvelope := fixture.receive(t, "provenance_audit")
	provenance, ok := provenanceEnvelope.Payload.(actionelements.Provenance)
	if !ok || provenance.CallID != "reference-call" || provenance.SessionID != "session-success" ||
		provenance.CandidateItemID == "" || provenance.ResultItemID == "" ||
		provenance.ModelResultDigest == "" || provenance.ProviderReference != "deployment.computer-use" {
		t.Fatalf("exact joined provenance = %#v envelope %+v", provenanceEnvelope.Payload, provenanceEnvelope)
	}
	auditEnvelope := fixture.receive(t, "dispatch_audit")
	audit, ok := auditEnvelope.Payload.(actionelements.AuditRecord)
	if !ok || !audit.Crossed || !audit.Executed || audit.CallID != "reference-call" ||
		audit.SessionID != "session-success" || audit.ModelResultDigest != provenance.ModelResultDigest ||
		audit.ProviderReference != provenance.ProviderReference {
		t.Fatalf("dispatch audit = %#v", auditEnvelope.Payload)
	}

	snapshot := fixture.store.Snapshot()
	var proposal, call, result *trajectory.Item
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		switch item.Kind {
		case trajectory.KindToolProposal:
			proposal = item
		case trajectory.KindToolCall:
			call = item
		case trajectory.KindToolResult:
			result = item
		}
	}
	if proposal == nil || call == nil || result == nil || proposal.ID == call.ID ||
		proposal.ToolCall == nil || call.ToolCall == nil || result.ToolResult == nil ||
		proposal.ToolCall.CallID != "reference-call" || call.ToolCall.CallID != "reference-call" ||
		result.ToolResult.CallID != "reference-call" || !slices.Contains(call.CausalParentIDs, proposal.ID) ||
		!slices.Contains(result.CausalParentIDs, call.ID) {
		t.Fatalf("canonical proposal/call/result trajectory = %+v", snapshot.Items)
	}
	commitment, found := fixture.ledger.Lookup(canonical.Execution.CommitmentID)
	if !found || commitment.State != legacyaction.StatePlayed ||
		commitment.CallID == "reference-call" {
		// The ledger key deliberately contains the session/run scope, so a raw
		// provider call ID cannot alias another session's commitment.
		t.Fatalf("scoped final ledger commitment = %+v, found=%v", commitment, found)
	}
}

func TestSilentComputerUseReferenceRejectsCrossSessionCancellationThenCancelsInScope(t *testing.T) {
	dispatcher := &silentReferenceDispatcher{}
	confirmation := &silentReferenceConfirmation{entered: make(chan struct{}, 1), block: true}
	fixture := mountSilentComputerUseReference(t, legacyaction.ConfirmAlways, confirmation, dispatcher)
	defer fixture.stop(t)

	fixture.sendObservation(t, "session-authorized", "user-input-cancel")
	fixture.awaitActivation(t)
	proposalEnvelope := fixture.receive(t, "model_proposals")
	select {
	case <-confirmation.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("confirmation stage did not receive the admitted call")
	}

	fixture.sendActionInterrupt(t, "other-session", proposalEnvelope.RunID, "cross-session-cancel")
	wrong := fixture.receive(t, "confirmation_outcome").Payload.(actionelements.Outcome)
	if wrong.Kind != actionelements.OutcomeCanceled || wrong.Crossed {
		t.Fatalf("cross-session cancellation outcome = %+v", wrong)
	}
	if dispatcher.calls.Load() != 0 {
		t.Fatalf("cross-session cancellation raced an effect: calls=%d", dispatcher.calls.Load())
	}

	fixture.sendActionInterrupt(t, "session-authorized", proposalEnvelope.RunID, "in-scope-cancel")
	canceled := fixture.receive(t, "confirmation_outcome").Payload.(actionelements.Outcome)
	if canceled.Kind != actionelements.OutcomeCanceled || canceled.CallID != "reference-call" {
		t.Fatalf("in-scope cancellation outcome = %+v", canceled)
	}
	fixture.assertNoEnvelope(t, "dispatch_result", 75*time.Millisecond)
	if dispatcher.calls.Load() != 0 {
		t.Fatalf("canceled action reached dispatcher %d times", dispatcher.calls.Load())
	}
}

func BenchmarkCompileSilentComputerUseReference(b *testing.B) {
	directory := filepath.Join("components", "silent-computer-use")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		b.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		b.Fatal(err)
	}
	lockBody, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
	if err != nil {
		b.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockBody)
	if err != nil {
		b.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		compiled, compileErr := graphcompiler.Compile(parsed, graphcompiler.Options{
			Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
		})
		if compileErr != nil || compiled.Graph.Fingerprint == "" {
			b.Fatalf("compile = %v, fingerprint=%q", compileErr, compiled.Graph.Fingerprint)
		}
	}
}

func loadSilentComputerUseReference(t *testing.T) silentComputerUseReference {
	t.Helper()
	directory := filepath.Join("components", "silent-computer-use")
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
		t.Fatal("silent computer-use resolution lock is stale")
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
	return silentComputerUseReference{graph: locked.Graph, bound: bound}
}

type silentReferenceFixture struct {
	mounted *graphruntime.Mounted
	done    chan error
	cancel  context.CancelFunc
	store   *trajectory.Store
	ledger  *legacyaction.Ledger
}

func mountSilentComputerUseReference(
	t *testing.T, confirm legacyaction.Confirm, confirmation *silentReferenceConfirmation,
	dispatcher *silentReferenceDispatcher,
) *silentReferenceFixture {
	t.Helper()
	reference := loadSilentComputerUseReference(t)
	store := trajectory.NewStore()
	ledger := legacyaction.NewLedger()
	providers := cognitionelements.NewProviderRegistry()
	descriptor := silentReferenceModelDescriptor()
	if err := providers.Register("deployment.computer-use", descriptor, func() (continuation.Provider, error) {
		return &silentReferenceModel{descriptor: descriptor}, nil
	}); err != nil {
		t.Fatal(err)
	}
	definition, found := computeruse.Lookup(computeruse.Click)
	if !found {
		t.Fatal("standard click definition is absent")
	}
	tools := actionelements.NewToolRegistries()
	if err := tools.Register("deployment.tools", []legacyaction.ToolSpec{{
		Name: computeruse.Click, Description: definition.Description, Parameters: definition.Parameters,
		Confirm: confirm, Target: "browser", Dispatcher: dispatcher,
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
	for _, name := range []string{"trajectory_snapshot", "activation_state"} {
		port, portErr := mounted.Egress(name)
		if portErr != nil {
			cancel()
			t.Fatal(portErr)
		}
		go func(input element.InputPort) {
			for {
				if _, receiveErr := input.Receive(runContext); receiveErr != nil {
					return
				}
			}
		}(port)
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
			t.Fatalf("silent computer-use graph did not become ready: %+v", live.Nodes)
		}
		time.Sleep(time.Millisecond)
	}
	return &silentReferenceFixture{mounted: mounted, done: done, cancel: cancel, store: store, ledger: ledger}
}

func (fixture *silentReferenceFixture) sendObservation(t *testing.T, sessionID, itemID string) {
	t.Helper()
	input, err := fixture.mounted.Ingress("observations")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := input.Broadcast(ctx, element.Envelope{
		Type: stateelements.ObservationType(), ItemID: itemID, SourceID: "user-text-1",
		SessionID: sessionID, Payload: perception.Observation{
			Text: "click the visible control", Observer: "text", Source: "user",
			Authority: trajectory.AuthorityUser, Revision: 1, StableText: "click the visible control", Final: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *silentReferenceFixture) sendActionInterrupt(
	t *testing.T, sessionID, runID, itemID string,
) {
	t.Helper()
	input, err := fixture.mounted.Ingress("action_cancel")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := input.Broadcast(ctx, element.Envelope{
		Type: actionelements.InterruptType(), ItemID: itemID, SessionID: sessionID, RunID: runID,
		Payload: actionelements.Interrupt{CallID: "reference-call", Reason: itemID},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *silentReferenceFixture) awaitActivation(t *testing.T) {
	t.Helper()
	observation := fixture.receive(t, "observation_outcome").Payload.(stateelements.ObservationCommitOutcome)
	if observation.Kind != stateelements.ObservationCommitted {
		t.Fatalf("observation commit outcome = %+v", observation)
	}
	for range 4 {
		outcome := fixture.receive(t, "activation_outcome").Payload.(policyelements.GenerationOutcome)
		if outcome.Kind == policyelements.GenerationEmitted {
			return
		}
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "missing_session" {
			t.Fatalf("activation outcome before emission = %+v", outcome)
		}
	}
	t.Fatal("activation did not emit after the committed observation")
}

func (fixture *silentReferenceFixture) receive(t *testing.T, name string) element.Envelope {
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

func (fixture *silentReferenceFixture) assertNoEnvelope(t *testing.T, name string, wait time.Duration) {
	t.Helper()
	input, err := fixture.mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected %s envelope %+v", name, envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receive %s: %v", name, err)
	}
}

func (fixture *silentReferenceFixture) stop(t *testing.T) {
	t.Helper()
	fixture.cancel()
	select {
	case err := <-fixture.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("silent computer-use graph stopped with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("silent computer-use graph did not stop")
	}
}

type silentReferenceModel struct{ descriptor continuation.Descriptor }

func (provider *silentReferenceModel) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (*silentReferenceModel) Continue(
	_ context.Context, _ continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
		CallID: "reference-call", Name: computeruse.Click,
		Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
	}}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "tool_call"}, nil
}

func silentReferenceModelDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "computer-use-reference", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

type silentReferenceDispatcher struct{ calls atomic.Int32 }

func (*silentReferenceDispatcher) Name() string { return "computer:browser" }
func (dispatcher *silentReferenceDispatcher) Dispatch(
	_ context.Context, call trajectory.ToolCall,
) (trajectory.ToolResult, error) {
	dispatcher.calls.Add(1)
	return trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"clicked":true}`),
	}, nil
}

type silentReferenceConfirmation struct {
	block   bool
	entered chan struct{}
}

func (*silentReferenceConfirmation) Name() string { return "reference-confirmation" }
func (provider *silentReferenceConfirmation) Confirm(
	ctx context.Context, _ legacyaction.ConfirmationRequest,
) (bool, error) {
	if provider.entered != nil {
		provider.entered <- struct{}{}
	}
	if provider.block {
		<-ctx.Done()
		return false, context.Cause(ctx)
	}
	return true, nil
}

func silentNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func silentHasEdge(graph ir.Graph, fromNode, fromPort, toNode, toPort string) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func silentEdgeDelivery(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) (ir.Delivery, bool) {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return edge.Delivery, true
		}
	}
	return "", false
}
