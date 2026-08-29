package sidecar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/sidecar"
)

func graphDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.GraphModel", Revision: 1,
		Ports: []element.Port{
			{Name: "trigger", Direction: element.Input, Type: element.Trigger(element.Named("test.Generate")), Cardinality: element.One},
			{Name: "outcome", Direction: element.Output, Type: element.Event(element.Named("test.Outcome")), Cardinality: element.One},
		},
		Reaction: element.Reaction{Triggers: []string{"trigger"}, Outcomes: []string{"outcome"}},
	}
}

func validElementHandshake() (sidecar.Message, sidecar.Message) {
	descriptor := graphDescriptor()
	selections := []sidecar.PortSelection{
		{Name: "trigger", Direction: element.Input, Type: descriptor.Ports[0].Type},
		{Name: "outcome", Direction: element.Output, Type: descriptor.Ports[1].Type},
	}
	provider := sidecar.ArtifactIdentity{ID: "provider/model", Revision: "2026-08-29"}
	capabilities := make([]sidecar.CapabilityIdentity, 0, len(selections)+1)
	for _, selection := range selections {
		capabilities = append(capabilities, sidecar.CapabilityIdentity{
			Name:     sidecar.PortCapabilityName(selection.Direction, selection.Name),
			Contract: selection.Type.String(), Provider: provider,
			Adapter: &sidecar.ArtifactIdentity{ID: "adapter/wire", Revision: "4.0.0"},
		})
	}
	capabilities = append(capabilities, sidecar.CapabilityIdentity{
		Name: "model.interruptible", Contract: "flow.RunID", Provider: provider,
	})
	helloDescriptor := descriptor.Clone()
	readyDescriptor := descriptor.Clone()
	hello := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &helloDescriptor, ElementConfig: json.RawMessage(`{"effort":"low"}`),
		SelectedPorts:        selections,
		RequiredCapabilities: []sidecar.CapabilityRequirement{{Name: "model.interruptible", Contract: "flow.RunID"}},
	}
	ready := sidecar.Message{
		Type: sidecar.TypeReady, Version: sidecar.VersionElementGraph,
		ElementDescriptor:    &readyDescriptor,
		RuntimeArtifact:      sidecar.ArtifactIdentity{ID: "runtime/python-wheel", Revision: "1.4.2"},
		ResolvedCapabilities: capabilities,
	}
	return hello, ready
}

func TestElementHandshakeBindsDescriptorSelectedPortsAndLiveIdentities(t *testing.T) {
	hello, ready := validElementHandshake()
	if err := hello.Validate(); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := ready.Validate(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := sidecar.ValidateElementReady(hello, ready); err != nil {
		t.Fatalf("readiness: %v", err)
	}

	missing := ready
	missing.ResolvedCapabilities = missing.ResolvedCapabilities[1:]
	if err := sidecar.ValidateElementReady(hello, missing); err == nil ||
		!strings.Contains(err.Error(), "selected port") {
		t.Fatalf("missing selected-port capability error = %v", err)
	}

	withoutAdapter := ready
	withoutAdapter.ResolvedCapabilities = append([]sidecar.CapabilityIdentity(nil), ready.ResolvedCapabilities...)
	withoutAdapter.ResolvedCapabilities[0].Adapter = nil
	if err := sidecar.ValidateElementReady(hello, withoutAdapter); err == nil ||
		!strings.Contains(err.Error(), "adapter identity") {
		t.Fatalf("missing selected-port adapter error = %v", err)
	}

	drifted := ready
	descriptor := ready.ElementDescriptor.Clone()
	descriptor.Revision++
	drifted.ElementDescriptor = &descriptor
	if err := sidecar.ValidateElementReady(hello, drifted); err == nil ||
		!strings.Contains(err.Error(), "descriptor drifted") {
		t.Fatalf("descriptor drift error = %v", err)
	}
}

func TestElementFramesPreserveTypedEnvelopeFramingAndBinaryBody(t *testing.T) {
	typeOf := element.Segmented(element.Named("test.Delta"), element.Named("flow.RunID"))
	source := element.Envelope{
		Type: typeOf, ItemID: "delta-2", SessionID: "session-1", SourceID: "model",
		OpportunityID: "op-1", RunID: "run-1", Sequence: 2, CaptureNS: 10, ReceiveNS: 12,
		TraceID: "trace-1", CancellationScope: "run-1", CausalParents: []string{"trigger-1"},
	}
	wire := sidecar.FromEnvelope(source, json.RawMessage(`{"boundary":"delta","text":"hi"}`))
	message := sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "text_out", Envelope: &wire,
		Payload: []byte{1, 2, 3, 4},
	}
	var stream bytes.Buffer
	if err := sidecar.NewWriter(&stream).Write(message); err != nil {
		t.Fatal(err)
	}
	decoded, err := sidecar.NewReader(&stream).Read()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Envelope == nil || !decoded.Envelope.Type.Equal(typeOf) ||
		decoded.Envelope.ItemID != source.ItemID || decoded.Envelope.RunID != source.RunID ||
		decoded.Envelope.Sequence != source.Sequence ||
		!bytes.Equal(decoded.Envelope.JSON, wire.JSON) || !bytes.Equal(decoded.Payload, message.Payload) {
		t.Fatalf("decoded frame lost envelope evidence: %+v", decoded)
	}
	envelope := decoded.Envelope.Envelope(struct{}{})
	if envelope.TraceID != source.TraceID || len(envelope.CausalParents) != 1 ||
		envelope.CausalParents[0] != "trigger-1" {
		t.Fatalf("reconstructed envelope = %+v", envelope)
	}
}

func TestElementProtocolDoesNotRequireAnAudioRateOrLegacyModelLabel(t *testing.T) {
	_, ready := validElementHandshake()
	ready.Model = ""
	ready.OutputRate = 0
	if err := ready.Validate(); err != nil {
		t.Fatalf("non-audio graph-ready frame was rejected: %v", err)
	}
}

func TestV4HelloRequiresStrictConfigurationJSON(t *testing.T) {
	hello, _ := validElementHandshake()
	hello.ElementConfig = json.RawMessage(`{"effort":"low","effort":"high"}`)
	if err := hello.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate config error = %v", err)
	}
}

func TestReaderRejectsDuplicateHeaderFields(t *testing.T) {
	reader := sidecar.NewReader(strings.NewReader("{\"type\":\"bye\",\"type\":\"log\"}\n"))
	if _, err := reader.Read(); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate sidecar header error = %v", err)
	}
}

func TestClientCompletesV4HandshakeAndCarriesGenericFrames(t *testing.T) {
	hello, ready := validElementHandshake()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		reader, writer := sidecar.NewReader(connection), sidecar.NewWriter(connection)
		incomingHello, err := reader.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if err := incomingHello.Validate(); err != nil {
			serverDone <- fmt.Errorf("server observed invalid hello: %w", err)
			return
		}
		if err := writer.Write(ready); err != nil {
			serverDone <- err
			return
		}
		input, err := reader.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if input.Type != sidecar.TypeElementFrame || input.Port != "trigger" {
			serverDone <- fmt.Errorf("input frame = %+v", input)
			return
		}
		outcomeType := graphDescriptor().Ports[1].Type
		wire := sidecar.WireEnvelope{
			Type: outcomeType, ItemID: "outcome-1", RunID: "run-1", JSON: json.RawMessage(`{"kind":"done"}`),
		}
		if err := writer.Write(sidecar.Message{
			Type: sidecar.TypeElementFrame, Port: "outcome", Envelope: &wire,
		}); err != nil {
			serverDone <- err
			return
		}
		bye, err := reader.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if bye.Type != sidecar.TypeBye {
			serverDone <- fmt.Errorf("last frame = %s, want bye", bye.Type)
			return
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := sidecar.Dial(ctx, sidecar.Config{
		Address: "tcp:" + listener.Addr().String(), ProtocolVersion: sidecar.VersionElementGraph,
	}, hello)
	if err != nil {
		t.Fatal(err)
	}
	triggerType := graphDescriptor().Ports[0].Type
	wire := sidecar.WireEnvelope{
		Type: triggerType, ItemID: "trigger-1", RunID: "run-1", JSON: json.RawMessage(`{"prompt":"hello"}`),
	}
	if err := client.Send(sidecar.Message{
		Type: sidecar.TypeElementFrame, Port: "trigger", Envelope: &wire,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-client.Frames():
		if frame.Port != "outcome" || frame.Envelope == nil || frame.Envelope.RunID != "run-1" {
			t.Fatalf("output frame = %+v", frame)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for v4 output")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("v4 server did not observe clean lifecycle close")
	}
}
