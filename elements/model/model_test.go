package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/sidecar"
)

type testDelta struct {
	Boundary string `json:"boundary"`
	Text     string `json:"text,omitempty"`
}

type testOutcome struct {
	Kind  string `json:"kind"`
	RunID string `json:"run_id,omitempty"`
}

type fakeSession struct {
	ready     sidecar.Message
	frames    chan sidecar.Message
	sent      chan sidecar.Message
	err       atomic.Pointer[error]
	closeOnce sync.Once
	frameOnce sync.Once
	closes    atomic.Int32
}

func selectedPortCapability(
	selection sidecar.PortSelection, provider sidecar.ArtifactIdentity,
) sidecar.CapabilityIdentity {
	return sidecar.CapabilityIdentity{
		Name:     sidecar.PortCapabilityName(selection.Direction, selection.Name),
		Contract: selection.Type.String(), Provider: provider,
		Adapter: &sidecar.ArtifactIdentity{ID: "adapter/element-wire", Revision: "4"},
	}
}

func newFakeSession(hello sidecar.Message) *fakeSession {
	provider := sidecar.ArtifactIdentity{ID: "provider/test-model", Revision: "2026-08-29"}
	capabilities := make([]sidecar.CapabilityIdentity, 0,
		len(hello.SelectedPorts)+len(hello.RequiredCapabilities))
	for _, selection := range hello.SelectedPorts {
		capabilities = append(capabilities, selectedPortCapability(selection, provider))
	}
	for _, requirement := range hello.RequiredCapabilities {
		capabilities = append(capabilities, sidecar.CapabilityIdentity{
			Name: requirement.Name, Contract: requirement.Contract, Provider: provider,
		})
	}
	descriptor := hello.ElementDescriptor.Clone()
	ready := sidecar.Message{
		Type: sidecar.TypeReady, Version: sidecar.VersionElementGraph,
		ElementDescriptor:    &descriptor,
		AppliedConfigDigest:  sidecar.ElementConfigDigest(hello.ElementConfig),
		RuntimeArtifact:      sidecar.ArtifactIdentity{ID: "runtime/fake-sidecar", Revision: "4.1.0"},
		ResolvedCapabilities: capabilities,
	}
	for _, selection := range hello.SelectedPorts {
		ready.NegotiatedPorts = append(ready.NegotiatedPorts, sidecar.PortNegotiation{
			Name: selection.Name, Direction: selection.Direction, Format: selection.Formats[0],
		})
	}
	return &fakeSession{
		ready:  ready,
		frames: make(chan sidecar.Message, 32), sent: make(chan sidecar.Message, 32),
	}
}

func (session *fakeSession) Ready() sidecar.Message         { return session.ready }
func (session *fakeSession) Frames() <-chan sidecar.Message { return session.frames }
func (session *fakeSession) Err() error {
	if value := session.err.Load(); value != nil {
		return *value
	}
	return nil
}
func (session *fakeSession) Send(message sidecar.Message) error { session.sent <- message; return nil }
func (session *fakeSession) Close() error {
	session.closeOnce.Do(func() {
		session.closes.Add(1)
		session.endFrames()
	})
	return nil
}

func (session *fakeSession) endFrames() { session.frameOnce.Do(func() { close(session.frames) }) }

type mountedModel struct {
	mounted *graphruntime.Mounted
	cancel  context.CancelFunc
	done    chan error
	session *fakeSession
}

func mountModel(t *testing.T, source string, required []sidecar.CapabilityRequirement) mountedModel {
	t.Helper()
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptor(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("model-test.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	codec := NewJSONCodec()
	if err := codec.Register(PreparedTextType(), func() any { return &testDelta{} }); err != nil {
		t.Fatal(err)
	}
	if err := codec.Register(OutcomeType(), func() any { return &testOutcome{} }); err != nil {
		t.Fatal(err)
	}
	deployments := NewDeploymentRegistry()
	created := make(chan *fakeSession, 1)
	if err := deployments.Register("deployment.test", func(_ context.Context, hello sidecar.Message) (Session, error) {
		session := newFakeSession(hello)
		created <- session
		return session, nil
	}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(DeploymentRegistryService, deployments); err != nil {
		t.Fatal(err)
	}
	if _, err := services.Set(PayloadCodecService, codec); err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := registry.RegisterArtifact("", inspect.ArtifactIdentity{
		ID: "builtin/model-external", Revision: "test",
	}, Factory{}); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(Config{
		Deployment: "deployment.test", Settings: json.RawMessage(`{"voice":"test"}`),
		RequiredCapabilities: required,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"model": config},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	var session *fakeSession
	select {
	case session = <-created:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("model deployment was not dialed")
	}
	return mountedModel{mounted: mounted, cancel: cancel, done: done, session: session}
}

const modelGraph = `graph model_test {
    model.External :: model;
    input trigger = model.trigger;
    input cancel = model.cancel;
    output text = model.text_out;
    output outcome = model.outcome;
}`

func TestGraphNativeSessionPreservesFramingCancellationAndLiveResolution(t *testing.T) {
	harness := mountModel(t, modelGraph, []sidecar.CapabilityRequirement{{
		Name: "model.interruptible", Contract: "flow.RunID",
	}})
	defer stopModel(t, harness)

	eventually(t, func() bool {
		resolution := harness.mounted.Live().Nodes["model"].Resolution
		return resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "runtime/fake-sidecar" &&
			resolution.CapabilitiesEvidence == inspect.EvidenceLive
	})

	trigger := ingress(t, harness.mounted, "trigger")
	if _, err := trigger.Broadcast(context.Background(), element.Envelope{
		Type: GenerateType(), ItemID: "trigger-1", SessionID: "session-1",
		RunID: "run-1", TraceID: "trace-1", CaptureNS: 10, ReceiveNS: 12,
		CancellationScope: "run-1", Payload: map[string]any{"prompt": "hello"},
	}); err != nil {
		t.Fatal(err)
	}
	sent := receiveSent(t, harness.session)
	if sent.Port != "trigger" || sent.Envelope == nil || sent.Envelope.RunID != "run-1" ||
		sent.Envelope.TraceID != "trace-1" || sent.Envelope.CaptureNS != 10 ||
		!sent.Envelope.Type.Equal(GenerateType()) {
		t.Fatalf("trigger frame lost typed timing/correlation evidence: %+v", sent)
	}

	text := egress(t, harness.mounted, "text")
	for index, delta := range []testDelta{
		{Boundary: "begin"}, {Boundary: "delta", Text: "hello"}, {Boundary: "end"},
	} {
		harness.session.frames <- elementFrame(t, "text_out", PreparedTextType(),
			fmt.Sprintf("text-%d", index), "run-1", uint64(index+1), delta)
	}
	for index, boundary := range []string{"begin", "delta", "end"} {
		envelope := receiveEnvelope(t, text)
		payload, ok := envelope.Payload.(*testDelta)
		if !ok || payload.Boundary != boundary || envelope.Sequence != uint64(index+1) ||
			envelope.RunID != "run-1" {
			t.Fatalf("framed text %d = %#v (%+v)", index, envelope.Payload, envelope)
		}
	}

	cancel := ingress(t, harness.mounted, "cancel")
	if _, err := cancel.Broadcast(context.Background(), element.Envelope{
		Type: CancelType(), ItemID: "cancel-1", RunID: "run-1",
		CancellationScope: "run-1", Payload: map[string]any{"reason": "barge-in"},
	}); err != nil {
		t.Fatal(err)
	}
	sent = receiveSent(t, harness.session)
	if sent.Port != "cancel" || sent.Envelope == nil || sent.Envelope.RunID != "run-1" ||
		!sent.Envelope.Type.Equal(CancelType()) {
		t.Fatalf("cancel was not an addressed graph frame: %+v", sent)
	}
	outcomes := egress(t, harness.mounted, "outcome")
	harness.session.frames <- elementFrame(t, "outcome", OutcomeType(),
		"outcome-1", "run-1", 1, testOutcome{Kind: "cancelled", RunID: "run-1"})
	terminal := receiveEnvelope(t, outcomes)
	if payload, ok := terminal.Payload.(*testOutcome); !ok || payload.Kind != "cancelled" {
		t.Fatalf("explicit cancellation terminal = %#v", terminal.Payload)
	}
}

func TestLiveIdentityDriftStopsTheNodeAndClosesItsSession(t *testing.T) {
	harness := mountModel(t, modelGraph, nil)
	eventually(t, func() bool {
		resolution := harness.mounted.Live().Nodes["model"].Resolution
		return resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive
	})
	drifted := harness.session.ready
	drifted.ResolvedCapabilities = append([]sidecar.CapabilityIdentity(nil),
		drifted.ResolvedCapabilities...)
	drifted.ResolvedCapabilities[0].Provider.Revision = "changed"
	harness.session.frames <- drifted
	select {
	case err := <-harness.done:
		if err == nil || !strings.Contains(err.Error(), "identity changed after readiness") {
			t.Fatalf("identity drift error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("identity drift did not stop the graph")
	}
	if got := harness.session.closes.Load(); got != 1 {
		t.Fatalf("session closes after drift = %d, want 1", got)
	}
}

func TestPrematureRemoteCloseIsALifecycleFailure(t *testing.T) {
	harness := mountModel(t, modelGraph, nil)
	harness.session.endFrames()
	select {
	case err := <-harness.done:
		if err == nil || !strings.Contains(err.Error(), "closed before graph shutdown") {
			t.Fatalf("premature close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("premature close did not stop the graph")
	}
	if got := harness.session.closes.Load(); got != 1 {
		t.Fatalf("session closes = %d, want 1", got)
	}
}

func TestReactiveSelectionRequiresAnExplicitOutcomeConnection(t *testing.T) {
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptor(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("missing-outcome.ortg", []byte(`graph missing_outcome {
    model.External :: model;
    input trigger = model.trigger;
}`))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployments := NewDeploymentRegistry()
	if err := deployments.Register("deployment.test", func(context.Context, sidecar.Message) (Session, error) {
		return nil, errors.New("should not dial")
	}); err != nil {
		t.Fatal(err)
	}
	services := graphruntime.NewServiceSet()
	_, _ = services.Set(DeploymentRegistryService, deployments)
	_, _ = services.Set(PayloadCodecService, NewJSONCodec())
	registry := graphruntime.NewRegistry()
	if err := RegisterFactory(registry); err != nil {
		t.Fatal(err)
	}
	_, err = graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"model": json.RawMessage(`{"deployment":"deployment.test"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "explicit selected outcome") {
		t.Fatalf("missing outcome mount error = %v", err)
	}
}

func TestPayloadCodecRejectsFactoriesThatCannotBeDecodedInto(t *testing.T) {
	codec := NewJSONCodec()
	if err := codec.Register(OutcomeType(), func() any { return testOutcome{} }); err == nil ||
		!strings.Contains(err.Error(), "non-nil pointer") {
		t.Fatalf("non-pointer codec factory error = %v", err)
	}
}

func TestLossyExternalOutputDropIsAValidDeliveryDecision(t *testing.T) {
	output := droppingOutput{typeOf: OutcomeType()}
	runner := runner{
		codec: NewJSONCodec(),
		ports: boundPorts{outputs: map[string]element.OutputPort{"outcome": output}},
	}
	wire := sidecar.WireEnvelope{
		Type: OutcomeType(), ItemID: "outcome-dropped", JSON: json.RawMessage(`{"kind":"done"}`),
	}
	if err := runner.forwardOutput(context.Background(), sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "outcome", Envelope: &wire,
	}); err != nil {
		t.Fatalf("legal lossy drop stopped the model node: %v", err)
	}
}

type droppingOutput struct{ typeOf element.Type }

func (output droppingOutput) Name() string       { return "outcome" }
func (output droppingOutput) Type() element.Type { return output.typeOf.Clone() }
func (output droppingOutput) Lanes() []element.Sender {
	return []element.Sender{droppingSender{typeOf: output.typeOf}}
}
func (droppingOutput) Broadcast(context.Context, element.Envelope) (element.SendResult, error) {
	return element.SendResult{Dropped: 1}, nil
}

type droppingSender struct{ typeOf element.Type }

func (droppingSender) ID() string                { return "lossy" }
func (sender droppingSender) Type() element.Type { return sender.typeOf.Clone() }
func (droppingSender) Send(context.Context, element.Envelope) (element.DeliveryResult, error) {
	return element.Dropped, nil
}

func elementFrame(
	t *testing.T, port string, valueType element.Type, itemID, runID string,
	sequence uint64, payload any,
) sidecar.Message {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	wire := sidecar.WireEnvelope{
		Type: valueType, ItemID: itemID, RunID: runID, Sequence: sequence,
		TraceID: "trace-1", CausalParents: []string{"trigger-1"}, JSON: data,
	}
	return sidecar.Message{Type: sidecar.TypeElementFrame, Port: port, Envelope: &wire}
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

func receiveEnvelope(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func receiveSent(t *testing.T, session *fakeSession) sidecar.Message {
	t.Helper()
	select {
	case message := <-session.sent:
		return message
	case <-time.After(time.Second):
		t.Fatal("external model did not receive a selected input")
		return sidecar.Message{}
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func stopModel(t *testing.T, harness mountedModel) {
	t.Helper()
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("graph shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("graph did not stop")
	}
	if got := harness.session.closes.Load(); got != 1 {
		t.Errorf("session closes = %d, want 1", got)
	}
}
