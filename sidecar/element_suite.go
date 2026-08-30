package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/element"
)

const (
	elementConformanceChallenge       = "openrealtime-sidecar-v4"
	elementConformanceRunID           = "conformance-run-1"
	elementConformanceRequestID       = "conformance-request-1"
	elementConformanceCancelChallenge = "openrealtime-sidecar-v4-cancel"
	elementConformanceCancelRunID     = "conformance-run-cancel"
	elementConformancePendingID       = "conformance-request-cancel"
	elementConformanceCancelID        = "conformance-cancel-1"
)

var (
	elementConformanceRequestType = element.Trigger(element.Named("sidecar.ConformanceRequest"))
	elementConformanceCancelType  = element.Interrupt(element.Named("flow.RunID"))
	elementConformanceResultType  = element.Event(element.Named("sidecar.ConformanceResult"))
)

// StandardElementConformanceHello returns the bounded descriptor-backed v4
// probe used by RunConformance. A sidecar running in conformance mode first
// returns an "ok" result for an ordinary typed request, then proves typed
// cancellation on a second request. Both results retain the request run and
// causal identities; the canceled result also names the interrupt frame.
func StandardElementConformanceHello() Message {
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "sidecar.ConformanceProbe",
		Revision:      1,
		Ports: []element.Port{
			{
				Name: "request", Direction: element.Input, Type: elementConformanceRequestType.Clone(),
				Cardinality: element.One, Required: true, MinConnections: 1, DefaultDepth: 1,
			},
			{
				Name: "cancel", Direction: element.Input, Type: elementConformanceCancelType.Clone(),
				Cardinality: element.One, Required: true, MinConnections: 1, DefaultDepth: 1,
			},
			{
				Name: "result", Direction: element.Output, Type: elementConformanceResultType.Clone(),
				Cardinality: element.One, Required: true, MinConnections: 1, DefaultDepth: 1,
			},
		},
		Reaction: element.Reaction{
			Triggers: []string{"request"}, Interrupts: []string{"cancel"}, Outcomes: []string{"result"},
		},
	}
	return Message{
		Type: TypeHello, Version: VersionElementGraph, ElementDescriptor: &descriptor,
		ElementConfig: json.RawMessage(`{"mode":"conformance"}`),
		SelectedPorts: []PortSelection{
			JSONPortSelection("request", element.Input, elementConformanceRequestType),
			JSONPortSelection("cancel", element.Input, elementConformanceCancelType),
			JSONPortSelection("result", element.Output, elementConformanceResultType),
		},
	}
}

type elementConformanceRequest struct {
	Challenge     string `json:"challenge"`
	WaitForCancel bool   `json:"wait_for_cancel,omitempty"`
}

type elementConformanceCancel struct {
	Reason string `json:"reason"`
}

type elementConformanceResult struct {
	Challenge string `json:"challenge"`
	Status    string `json:"status"`
}

func runElementConformance(ctx context.Context, options ConformanceOptions) ConformanceReport {
	if options.TurnTimeout <= 0 {
		options.TurnTimeout = 120 * time.Second
	}
	report := ConformanceReport{
		Suite: "sidecar-protocol-v4", Version: VersionElementGraph,
	}
	record := func(name string, required, passed bool, detail string) {
		report.Checks = append(report.Checks, ConformanceCheck{
			Name: name, Required: required, Passed: passed, Detail: detail,
		})
		if required && !passed {
			report.Failures = append(report.Failures, name+": "+detail)
		}
	}

	hello := StandardElementConformanceHello()
	if err := hello.Validate(); err != nil {
		record("standard element probe is valid", true, false, err.Error())
		return report
	}
	client, err := Dial(ctx, options.Config, hello)
	if err != nil {
		record("descriptor-backed handshake completes", true, false, err.Error())
		return report
	}
	defer client.Close()

	ready := client.Ready()
	resolved, _ := CanonicalCapabilities(ready.ResolvedCapabilities)
	ports, _ := canonicalNegotiations(ready.NegotiatedPorts)
	report.Version = ready.Version
	report.Model = ready.RuntimeArtifact.ID
	report.Capabilities = make([]string, 0, len(resolved))
	for _, capability := range resolved {
		report.Capabilities = append(report.Capabilities, capability.Name)
	}
	identity, identityErr := ready.ElementDescriptor.Identity()
	if identityErr == nil {
		report.Element = &identity
	}
	runtime := ready.RuntimeArtifact
	report.Runtime = &runtime
	report.ConfigDigest = ready.AppliedConfigDigest
	report.Resolved = resolved
	report.Ports = ports

	record("descriptor-backed handshake completes", true, true, ready.RuntimeArtifact.ID)
	record("declares protocol version 4", true, ready.Version == VersionElementGraph,
		fmt.Sprintf("declared %d", ready.Version))
	wantIdentity, wantIdentityErr := hello.ElementDescriptor.Identity()
	record("attests the exact probe descriptor", true,
		identityErr == nil && wantIdentityErr == nil && identity == wantIdentity,
		fmt.Sprintf("declared %+v", identity))
	record("attests an immutable runtime artifact", true, ready.RuntimeArtifact.Validate() == nil,
		ready.RuntimeArtifact.ID+"@"+ready.RuntimeArtifact.Revision)
	record("attests the exact applied element config", true,
		ready.AppliedConfigDigest == ElementConfigDigest(hello.ElementConfig),
		ready.AppliedConfigDigest)
	record("negotiates every selected port", true,
		len(ready.NegotiatedPorts) == len(hello.SelectedPorts),
		fmt.Sprintf("negotiated %d/%d", len(ready.NegotiatedPorts), len(hello.SelectedPorts)))

	requestJSON, _ := json.Marshal(elementConformanceRequest{Challenge: elementConformanceChallenge})
	wire := WireEnvelope{
		Type: elementConformanceRequestType.Clone(), ItemID: elementConformanceRequestID,
		SessionID: "sidecar-conformance", RunID: elementConformanceRunID, Sequence: 1,
		TraceID: "sidecar-conformance", CancellationScope: elementConformanceRunID,
		JSON: requestJSON,
	}
	if err := client.Send(Message{Type: TypeElementFrame, Port: "request", Envelope: &wire}); err != nil {
		record("accepts a typed element request", true, false, err.Error())
		return report
	}
	record("accepts a typed element request", true, true, elementConformanceRequestType.String())

	frame, receiveErr := awaitElementConformanceResult(
		ctx, client, options.TurnTimeout, elementConformanceRunID)
	if receiveErr != nil {
		record("produces a typed element result", true, false, receiveErr.Error())
	} else {
		record("produces a typed element result", true, true, frame.Envelope.Type.String())
		var result elementConformanceResult
		decodeErr := decodeElementConformanceResult(frame.Envelope.JSON, &result)
		semantic := decodeErr == nil && result.Challenge == elementConformanceChallenge && result.Status == "ok"
		detail := fmt.Sprintf("challenge=%q status=%q", result.Challenge, result.Status)
		if decodeErr != nil {
			detail = decodeErr.Error()
		}
		record("returns the probe challenge and success status", true, semantic, detail)
		correlated := frame.Envelope.RunID == elementConformanceRunID &&
			slices.Contains(frame.Envelope.CausalParents, elementConformanceRequestID)
		record("preserves run and causal correlation", true, correlated,
			fmt.Sprintf("run=%q parents=%v", frame.Envelope.RunID, frame.Envelope.CausalParents))
	}

	pendingJSON, _ := json.Marshal(elementConformanceRequest{
		Challenge: elementConformanceCancelChallenge, WaitForCancel: true,
	})
	pending := WireEnvelope{
		Type: elementConformanceRequestType.Clone(), ItemID: elementConformancePendingID,
		SessionID: "sidecar-conformance", RunID: elementConformanceCancelRunID, Sequence: 1,
		TraceID: "sidecar-conformance-cancel", CancellationScope: elementConformanceCancelRunID,
		JSON: pendingJSON,
	}
	if err := client.Send(Message{Type: TypeElementFrame, Port: "request", Envelope: &pending}); err != nil {
		record("accepts a pending typed request", true, false, err.Error())
		return report
	}
	record("accepts a pending typed request", true, true, elementConformanceCancelChallenge)
	cancelJSON, _ := json.Marshal(elementConformanceCancel{Reason: "conformance cancellation"})
	cancel := WireEnvelope{
		Type: elementConformanceCancelType.Clone(), ItemID: elementConformanceCancelID,
		SessionID: "sidecar-conformance", RunID: elementConformanceCancelRunID, Sequence: 2,
		TraceID: "sidecar-conformance-cancel", CancellationScope: elementConformanceCancelRunID,
		CausalParents: []string{elementConformancePendingID}, JSON: cancelJSON,
	}
	if err := client.Send(Message{Type: TypeElementFrame, Port: "cancel", Envelope: &cancel}); err != nil {
		record("accepts a typed cancellation", true, false, err.Error())
		return report
	}
	record("accepts a typed cancellation", true, true, elementConformanceCancelType.String())
	canceled, cancelErr := awaitElementConformanceResult(
		ctx, client, options.TurnTimeout, elementConformanceCancelRunID)
	if cancelErr != nil {
		record("acknowledges typed cancellation", true, false, cancelErr.Error())
	} else {
		var result elementConformanceResult
		decodeErr := decodeElementConformanceResult(canceled.Envelope.JSON, &result)
		semantic := decodeErr == nil && result.Challenge == elementConformanceCancelChallenge &&
			result.Status == "canceled"
		detail := fmt.Sprintf("challenge=%q status=%q", result.Challenge, result.Status)
		if decodeErr != nil {
			detail = decodeErr.Error()
		}
		record("acknowledges typed cancellation", true, semantic, detail)
		correlated := canceled.Envelope.RunID == elementConformanceCancelRunID &&
			canceled.Envelope.CancellationScope == elementConformanceCancelRunID &&
			slices.Contains(canceled.Envelope.CausalParents, elementConformancePendingID) &&
			slices.Contains(canceled.Envelope.CausalParents, elementConformanceCancelID)
		record("preserves cancellation scope and causes", true, correlated,
			fmt.Sprintf("scope=%q parents=%v", canceled.Envelope.CancellationScope,
				canceled.Envelope.CausalParents))
	}

	if err := client.Close(); err != nil {
		record("closes cleanly", true, false, err.Error())
	} else {
		record("closes cleanly", true, true, "")
	}
	report.Passed = len(report.Failures) == 0
	return report
}

func awaitElementConformanceResult(
	ctx context.Context, client *Client, timeout time.Duration, runID string,
) (Message, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-timer.C:
			return Message{}, fmt.Errorf("sidecar did not produce a v4 result within %s", timeout)
		case frame, open := <-client.Frames():
			if !open {
				if err := client.Err(); err != nil {
					return Message{}, err
				}
				return Message{}, errors.New("sidecar closed before producing a v4 result")
			}
			switch frame.Type {
			case TypeElementFrame:
				if frame.Port == "result" && frame.Envelope != nil && frame.Envelope.RunID == runID {
					return frame, nil
				}
			case TypeError:
				return Message{}, fmt.Errorf("sidecar reported: %s", frame.Message())
			}
		}
	}
}

func decodeElementConformanceResult(source json.RawMessage, destination *elementConformanceResult) error {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("v4 result contains a trailing JSON value")
		}
		return err
	}
	if strings.TrimSpace(destination.Challenge) == "" || strings.TrimSpace(destination.Status) == "" {
		return errors.New("v4 result requires challenge and status")
	}
	return nil
}
