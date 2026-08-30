package sidecar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		sidecar.JSONPortSelection("trigger", element.Input, descriptor.Ports[0].Type),
		sidecar.JSONPortSelection("outcome", element.Output, descriptor.Ports[1].Type),
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
		AppliedConfigDigest:  sidecar.ElementConfigDigest(hello.ElementConfig),
		RuntimeArtifact:      sidecar.ArtifactIdentity{ID: "runtime/python-wheel", Revision: "1.4.2"},
		ResolvedCapabilities: capabilities,
	}
	for _, selection := range selections {
		ready.NegotiatedPorts = append(ready.NegotiatedPorts, sidecar.PortNegotiation{
			Name: selection.Name, Direction: selection.Direction, Format: selection.Formats[0],
		})
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
	declared := client.Ready()
	declared.RuntimeArtifact.Revision = "mutated-by-caller"
	declared.ElementDescriptor.Ports[0].Type.Arguments[0].Name = "test.Mutated"
	if retained := client.Ready(); retained.RuntimeArtifact.Revision != ready.RuntimeArtifact.Revision ||
		retained.ElementDescriptor.Ports[0].Type.Arguments[0].Name == "test.Mutated" {
		t.Fatalf("client readiness aliases caller-visible state: %+v", retained)
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

func TestV4ClientRejectsFramesBeforeReadiness(t *testing.T) {
	hello, _ := validElementHandshake()
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
		if _, err := reader.Read(); err != nil {
			serverDone <- err
			return
		}
		triggerType := graphDescriptor().Ports[0].Type
		wire := sidecar.WireEnvelope{
			Type: triggerType, ItemID: "pre-ready", JSON: json.RawMessage(`{"prompt":"too early"}`),
		}
		serverDone <- writer.Write(sidecar.Message{
			Type: sidecar.TypeElementFrame, Port: "trigger", Envelope: &wire,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = sidecar.Dial(ctx, sidecar.Config{
		Address: "tcp:" + listener.Addr().String(), ProtocolVersion: sidecar.VersionElementGraph,
	}, hello)
	if err == nil || !strings.Contains(err.Error(), "before readiness") {
		t.Fatalf("pre-readiness frame error = %v", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("pre-readiness server did not finish")
	}
}

func TestV4ClientRejectsPostReadinessIdentityDrift(t *testing.T) {
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
		if _, err := reader.Read(); err != nil {
			serverDone <- err
			return
		}
		if err := writer.Write(ready); err != nil {
			serverDone <- err
			return
		}
		drifted := ready.Clone()
		drifted.RuntimeArtifact.Revision = "1.4.3"
		serverDone <- writer.Write(drifted)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := sidecar.Dial(ctx, sidecar.Config{
		Address: "tcp:" + listener.Addr().String(), ProtocolVersion: sidecar.VersionElementGraph,
	}, hello)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case _, open := <-client.Frames():
		if open {
			t.Fatal("drifted readiness escaped as an application frame")
		}
	case <-ctx.Done():
		t.Fatal("post-readiness drift did not close the frame stream")
	}
	if err := client.Err(); err == nil || !strings.Contains(err.Error(), "changed after readiness") {
		t.Fatalf("post-readiness drift error = %v", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("post-readiness server did not finish")
	}
}

func TestV4TransportLossIsTerminalAndNeverSilentlyReconnects(t *testing.T) {
	hello, _ := validElementHandshake()
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
		reader, writer := sidecar.NewReader(connection), sidecar.NewWriter(connection)
		received, err := reader.Read()
		if err != nil {
			_ = connection.Close()
			serverDone <- err
			return
		}
		ready, err := (sidecar.ElementConformanceFixture{
			Runtime:  sidecar.ArtifactIdentity{ID: "runtime/reconnect-test", Revision: "4"},
			Provider: sidecar.ArtifactIdentity{ID: "provider/reconnect-test", Revision: "1"},
			Adapter:  sidecar.ArtifactIdentity{ID: "adapter/reconnect-test", Revision: "4"},
		}).Ready(received)
		if err == nil {
			err = writer.Write(ready)
		}
		_ = connection.Close()
		if err != nil {
			serverDone <- err
			return
		}

		// A v4 stream contains stateful, non-replayable media and action
		// envelopes. Reconnecting it inside Client would silently replace the
		// attested session and lose state; only an outer supervisor may start a
		// fresh session. Prove that no second connection is attempted.
		tcp := listener.(*net.TCPListener)
		if err := tcp.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			serverDone <- err
			return
		}
		unexpected, err := tcp.Accept()
		if err == nil {
			_ = unexpected.Close()
			serverDone <- errors.New("v4 client silently reconnected after transport loss")
			return
		}
		if networkError, ok := err.(net.Error); !ok || !networkError.Timeout() {
			serverDone <- fmt.Errorf("wait for forbidden reconnect: %w", err)
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
	defer client.Close()
	select {
	case _, open := <-client.Frames():
		if open {
			t.Fatal("transport loss produced an application frame")
		}
	case <-ctx.Done():
		t.Fatal("transport loss did not terminate the v4 session")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("timed out checking the no-reconnect policy")
	}
}

func TestV4DialContextCancellationClosesTheOwnedTransport(t *testing.T) {
	hello, _ := validElementHandshake()
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
		incoming, err := reader.Read()
		if err != nil {
			serverDone <- err
			return
		}
		ready, err := (sidecar.ElementConformanceFixture{
			Runtime:  sidecar.ArtifactIdentity{ID: "runtime/context-owned", Revision: "4"},
			Provider: sidecar.ArtifactIdentity{ID: "provider/context-owned", Revision: "1"},
			Adapter:  sidecar.ArtifactIdentity{ID: "adapter/context-owned", Revision: "4"},
		}).Ready(incoming)
		if err == nil {
			err = writer.Write(ready)
		}
		if err != nil {
			serverDone <- err
			return
		}
		last, err := reader.Read()
		if err != nil {
			serverDone <- err
			return
		}
		if last.Type != sidecar.TypeBye {
			serverDone <- fmt.Errorf("context-canceled session ended with %s, want bye", last.Type)
			return
		}
		serverDone <- nil
	}()

	ctx, cancel := context.WithCancel(context.Background())
	client, err := sidecar.Dial(ctx, sidecar.Config{
		Address: "tcp:" + listener.Addr().String(), ProtocolVersion: sidecar.VersionElementGraph,
	}, hello)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case _, open := <-client.Frames():
		if open {
			t.Fatal("context cancellation exposed an application frame")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not close the v4 frame stream")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("context cancellation did not close the owned peer transport")
	}
}
