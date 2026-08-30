package sidecar_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/sidecar"
)

type v4ProbeResult struct {
	Challenge string `json:"challenge"`
	Status    string `json:"status"`
}

func TestRunConformanceV4UsesDescriptorBackedElementProbe(t *testing.T) {
	hello := sidecar.StandardElementConformanceHello()
	if err := hello.Validate(); err != nil {
		t.Fatalf("standard v4 conformance hello: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := serveV4Probe(listener, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := sidecar.RunConformance(ctx, sidecar.ConformanceOptions{
		Config: sidecar.Config{
			Address: "tcp:" + listener.Addr().String(), ProtocolVersion: sidecar.VersionElementGraph,
		},
		TurnTimeout: time.Second,
	})
	if !report.Passed {
		t.Fatalf("descriptor-backed v4 conformance failed: %v", report.Failures)
	}
	if report.Version != sidecar.VersionElementGraph || report.Element == nil ||
		report.Element.Name != "sidecar.ConformanceProbe" || report.Runtime == nil ||
		report.Runtime.ID != "runtime/conformance-sidecar" || len(report.Ports) != 3 ||
		len(report.Resolved) != 3 || report.ConfigDigest !=
		sidecar.ElementConfigDigest(sidecar.StandardElementConformanceHello().ElementConfig) {
		t.Fatalf("v4 conformance evidence = %+v", report)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("v4 conformance server did not observe clean close")
	}
}

func TestRunConformanceV4AgainstBundledPythonSDK(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate bundled Python conformance fixture")
	}
	script := filepath.Join(filepath.Dir(source), "..", "sidecars", "v4_conformance_sidecar.py")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report := sidecar.RunConformance(ctx, sidecar.ConformanceOptions{
		Config: sidecar.Config{
			Command: []string{python, script}, ProtocolVersion: sidecar.VersionElementGraph,
		},
		TurnTimeout: 2 * time.Second,
	})
	if !report.Passed {
		t.Fatalf("bundled Python protocol-v4 conformance failed: %v", report.Failures)
	}
	if report.Runtime == nil || report.Runtime.ID != "openrealtime/python-sidecar" ||
		report.ConfigDigest != sidecar.ElementConfigDigest(
			sidecar.StandardElementConformanceHello().ElementConfig,
		) || len(report.Ports) != 3 {
		t.Fatalf("bundled Python v4 evidence = %+v", report)
	}
}

func TestRunConformanceV4RejectsSemanticallyInvalidResults(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*v4ProbeResult, *sidecar.WireEnvelope)
		want   string
	}{
		"wrong challenge": {
			mutate: func(result *v4ProbeResult, _ *sidecar.WireEnvelope) {
				result.Challenge = "replayed-challenge"
			},
			want: "challenge and success status",
		},
		"missing causal parent": {
			mutate: func(_ *v4ProbeResult, wire *sidecar.WireEnvelope) {
				wire.CausalParents = nil
			},
			want: "causal correlation",
		},
	} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			serverDone := serveV4Probe(listener, test.mutate)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			report := sidecar.RunConformance(ctx, sidecar.ConformanceOptions{
				Config: sidecar.Config{
					Address:         "tcp:" + listener.Addr().String(),
					ProtocolVersion: sidecar.VersionElementGraph,
				},
				TurnTimeout: time.Second,
			})
			if report.Passed || !strings.Contains(strings.Join(report.Failures, " "), test.want) {
				t.Fatalf("invalid v4 result report = passed %v failures %v", report.Passed, report.Failures)
			}
			select {
			case err := <-serverDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("adversarial v4 server did not observe clean close")
			}
		})
	}
}

func TestRunConformanceV4RejectsAnInvalidCancellationProof(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := serveV4Probe(listener, nil,
		func(_ *v4ProbeResult, wire *sidecar.WireEnvelope) {
			wire.CausalParents = wire.CausalParents[:1]
		})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	report := sidecar.RunConformance(ctx, sidecar.ConformanceOptions{
		Config: sidecar.Config{
			Address:         "tcp:" + listener.Addr().String(),
			ProtocolVersion: sidecar.VersionElementGraph,
		},
		TurnTimeout: time.Second,
	})
	if report.Passed || !strings.Contains(
		strings.Join(report.Failures, " "), "preserves cancellation scope and causes",
	) {
		t.Fatalf("invalid v4 cancellation report = passed %v failures %v", report.Passed, report.Failures)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("adversarial v4 cancellation server did not observe clean close")
	}
}

func serveV4Probe(
	listener net.Listener,
	mutate func(*v4ProbeResult, *sidecar.WireEnvelope),
	cancelMutate ...func(*v4ProbeResult, *sidecar.WireEnvelope),
) <-chan error {
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		reader, writer := sidecar.NewReader(connection), sidecar.NewWriter(connection)
		hello, err := reader.Read()
		if err != nil {
			done <- err
			return
		}
		if err := hello.Validate(); err != nil {
			done <- fmt.Errorf("v4 probe hello: %w", err)
			return
		}
		if hello.Version != sidecar.VersionElementGraph || hello.ElementDescriptor == nil ||
			hello.SampleRate != 0 || hello.OutputRate != 0 || hello.Model != "" ||
			hello.Instructions != "" || hello.Voice != "" {
			done <- fmt.Errorf("RunConformance sent a legacy hello into v4: %+v", hello)
			return
		}
		ready, err := (sidecar.ElementConformanceFixture{
			Runtime:  sidecar.ArtifactIdentity{ID: "runtime/conformance-sidecar", Revision: "4.0.0"},
			Provider: sidecar.ArtifactIdentity{ID: "provider/conformance-element", Revision: "1.0.0"},
			Adapter:  sidecar.ArtifactIdentity{ID: "adapter/conformance-wire", Revision: "4.0.0"},
		}).Ready(hello)
		if err != nil {
			done <- err
			return
		}
		if err := writer.Write(ready); err != nil {
			done <- err
			return
		}
		request, err := reader.Read()
		if err != nil {
			done <- err
			return
		}
		if request.Type != sidecar.TypeElementFrame || request.Port != "request" ||
			request.Envelope == nil || request.Envelope.RunID == "" {
			done <- fmt.Errorf("v4 conformance request = %+v", request)
			return
		}
		var incoming struct {
			Challenge string `json:"challenge"`
		}
		if err := json.Unmarshal(request.Envelope.JSON, &incoming); err != nil || incoming.Challenge == "" {
			done <- fmt.Errorf("v4 conformance challenge = %q, error = %v", incoming.Challenge, err)
			return
		}
		result := v4ProbeResult{Challenge: incoming.Challenge, Status: "ok"}
		resultType, found := hello.ElementDescriptor.Port("result")
		if !found {
			done <- fmt.Errorf("v4 conformance descriptor has no result port")
			return
		}
		wire := sidecar.WireEnvelope{
			Type: resultType.Type, ItemID: "conformance-result-1",
			SessionID: request.Envelope.SessionID, RunID: request.Envelope.RunID, Sequence: 1,
			TraceID: request.Envelope.TraceID, CancellationScope: request.Envelope.CancellationScope,
			CausalParents: []string{request.Envelope.ItemID},
		}
		if mutate != nil {
			mutate(&result, &wire)
		}
		wire.JSON, err = json.Marshal(result)
		if err != nil {
			done <- err
			return
		}
		if err := writer.Write(sidecar.Message{
			Type: sidecar.TypeElementFrame, Port: "result", Envelope: &wire,
		}); err != nil {
			done <- err
			return
		}
		pending, err := reader.Read()
		if err != nil {
			done <- err
			return
		}
		if pending.Type != sidecar.TypeElementFrame || pending.Port != "request" ||
			pending.Envelope == nil || pending.Envelope.RunID == "" {
			done <- fmt.Errorf("v4 pending conformance request = %+v", pending)
			return
		}
		var pendingValue struct {
			Challenge     string `json:"challenge"`
			WaitForCancel bool   `json:"wait_for_cancel"`
		}
		if err := json.Unmarshal(pending.Envelope.JSON, &pendingValue); err != nil ||
			pendingValue.Challenge == "" || !pendingValue.WaitForCancel {
			done <- fmt.Errorf("v4 pending conformance challenge = %+v, error = %v", pendingValue, err)
			return
		}
		cancel, err := reader.Read()
		if err != nil {
			done <- err
			return
		}
		if cancel.Type != sidecar.TypeElementFrame || cancel.Port != "cancel" ||
			cancel.Envelope == nil || cancel.Envelope.RunID != pending.Envelope.RunID ||
			cancel.Envelope.CancellationScope != pending.Envelope.CancellationScope {
			done <- fmt.Errorf("v4 conformance cancellation = %+v", cancel)
			return
		}
		canceledResult := v4ProbeResult{
			Challenge: pendingValue.Challenge, Status: "canceled",
		}
		canceled := sidecar.WireEnvelope{
			Type: resultType.Type, ItemID: "conformance-result-canceled",
			SessionID: pending.Envelope.SessionID, RunID: pending.Envelope.RunID, Sequence: 2,
			TraceID:           pending.Envelope.TraceID,
			CancellationScope: pending.Envelope.CancellationScope,
			CausalParents:     []string{pending.Envelope.ItemID, cancel.Envelope.ItemID},
		}
		if len(cancelMutate) != 0 && cancelMutate[0] != nil {
			cancelMutate[0](&canceledResult, &canceled)
		}
		canceled.JSON, err = json.Marshal(canceledResult)
		if err != nil {
			done <- err
			return
		}
		if err := writer.Write(sidecar.Message{
			Type: sidecar.TypeElementFrame, Port: "result", Envelope: &canceled,
		}); err != nil {
			done <- err
			return
		}
		bye, err := reader.Read()
		if err != nil {
			done <- err
			return
		}
		if bye.Type != sidecar.TypeBye {
			done <- fmt.Errorf("v4 conformance final frame = %s, want bye", bye.Type)
			return
		}
		done <- nil
	}()
	return done
}
