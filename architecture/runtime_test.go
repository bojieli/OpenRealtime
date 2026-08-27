package architecture_test

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/binding"
)

type runtimeStub struct {
	binding.Runtime
	status binding.Status
	closed bool
}

func (runtime *runtimeStub) Status() binding.Status { return runtime.status }

func (runtime *runtimeStub) Close(context.Context, error) error {
	runtime.closed = true
	return nil
}

type bindingStub struct {
	definition architecture.Definition
	runtime    *runtimeStub
}

func runtimeStatus(definition architecture.Definition, stack binding.StackCapabilities) binding.Status {
	status := binding.Status{
		Ownership: definition.Ownership, Stack: stack,
		Interaction: binding.InteractionStatus{
			Evidence:          string(definition.Interaction.Evidence),
			Transport:         definition.Interaction.Transport,
			ProtocolVersion:   definition.Interaction.ProtocolVersion,
			ActHandoff:        string(definition.Interaction.Handoff),
			NativeSuppression: definition.Interaction.NativeSuppression,
		},
	}
	if definition.Interaction.Control != nil {
		status.Interaction.Control = *definition.Interaction.Control
	}
	if definition.Interaction.EvidenceCapabilities != nil {
		status.Interaction.EvidenceCapabilities = *definition.Interaction.EvidenceCapabilities
		if status.Interaction.EvidenceCapabilities.Transcript &&
			definition.Interaction.UsesTextPolicy() {
			status.Interaction.Recognizer = "test/asr"
			status.Interaction.RecognizerRevision = "asr-adapter-r1"
		}
	}
	return status
}

func (stub bindingStub) Name() string { return "shared-sidecar-runtime" }

func (stub bindingStub) Ownership() binding.Ownership { return stub.definition.Ownership }

func (stub bindingStub) Capabilities() binding.Capabilities {
	return binding.Capabilities{Stack: stub.runtime.status.Stack}
}

func (stub bindingStub) Start(context.Context, binding.Options) (binding.Runtime, error) {
	return stub.runtime, nil
}

func TestResolvedBindingAttestsTheExactDefinition(t *testing.T) {
	definition, err := architecture.Default().Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtimeStub{status: runtimeStatus(definition, definition.Requires)}
	resolved, err := architecture.Bind(definition, bindingStub{definition: definition, runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	session, err := resolved.Start(context.Background(), binding.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if session.Status().Architecture != definition.Identity() {
		t.Fatalf("runtime did not attest its definition: %+v", session.Status().Architecture)
	}
	if resolved.Name() != "shared-sidecar-runtime" {
		t.Fatalf("the adapter identity was erased: %q", resolved.Name())
	}
}

func TestResolvedBindingRefusesAHandshakeMissingRequirements(t *testing.T) {
	definition, err := architecture.Default().Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	available := definition.Requires
	available.InteractionActs = false
	runtime := &runtimeStub{status: runtimeStatus(definition, available)}
	resolved, err := architecture.Bind(definition, bindingStub{definition: definition, runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Start(context.Background(), binding.Options{}); err == nil {
		t.Fatal("a deficient ready frame started the architecture")
	}
	if !runtime.closed {
		t.Fatal("a deficient live runtime was not closed")
	}
}

func TestResolvedBindingRefusesAnUndeclaredEvidenceChannel(t *testing.T) {
	definition, err := architecture.Default().Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	status := runtimeStatus(definition, definition.Requires)
	status.Interaction.EvidenceCapabilities.DirectVisualInput = true
	runtime := &runtimeStub{status: status}
	resolved, err := architecture.Bind(definition, bindingStub{definition: definition, runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Start(context.Background(), binding.Options{}); err == nil {
		t.Fatal("direct vision hidden behind a transcript label was accepted")
	}
	if !runtime.closed {
		t.Fatal("a runtime with an undeclared evidence channel was not closed")
	}
}

func TestResolvedBindingRefusesDifferentControllerArbitration(t *testing.T) {
	definition, err := architecture.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	status := runtimeStatus(definition, definition.Requires)
	status.Interaction.Control = binding.InteractionControl{
		Selectors: binding.InteractionControllers{TextPolicy: true}, Arbitration: "single",
	}
	runtime := &runtimeStub{status: status}
	resolved, err := architecture.Bind(definition, bindingStub{definition: definition, runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Start(context.Background(), binding.Options{}); err == nil {
		t.Fatal("a pure controller started under a composed architecture identity")
	}
	if !runtime.closed {
		t.Fatal("a runtime with different controller arbitration was not closed")
	}
}
