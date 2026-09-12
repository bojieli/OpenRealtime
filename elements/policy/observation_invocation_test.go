package policy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func mountObservationInvocation(t *testing.T, automatic bool) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(policyelements.ObservationInvocationConfig{
		SessionInvocationConfig: policyelements.SessionInvocationConfig{
			Role: "text", TerminalMemory: 8, CancelMemory: 8,
		}, GenerateOnCommit: automatic,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compilePolicySource(t, "observation-invocation.ortg", []byte(strings.ReplaceAll(
			sessionInvocationGraph, "policy.SessionInvocation", "policy.ObservationInvocation"))),
		Registry: registry, Values: map[string]json.RawMessage{"policy": configuration},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	t.Cleanup(func() { harness.stop(t) })
	_ = receivePolicy(t, harness.egress(t, "state"))
	return harness
}

func observationInvocationOutcome(t *testing.T, harness policyHarness) policyelements.SessionInvocationOutcome {
	t.Helper()
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))
	return outcome
}

func sendObservationCreate(t *testing.T, harness policyHarness, id string, commit stateelements.ObservationCommitOutcome) {
	t.Helper()
	textCreate := policyelements.ResponseCreate{
		ResponseID: id, ExpectedContextVersion: &commit.StoreVersion,
		ExpectedContextItemID: commit.Context.StateItemID, CommittedContext: &commit.Context,
	}
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: id, SessionID: "session-policy", Payload: textCreate,
	})
}

func TestObservationInvocationManualModeUsesUpdatedSettingsAndExactCommittedContext(t *testing.T) {
	harness := mountObservationInvocation(t, false)
	installSessionInvocation(t, harness, 1, "first instruction", nil)
	commit := committedObservation(t, "files", "uploaded", "user-file", "snapshot-2", 2, 5)
	sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit-2", "session-policy", commit))
	if outcome := observationInvocationOutcome(t, harness); outcome.Code != "explicit_response_required" ||
		outcome.ContextVersion != 2 || outcome.Kind != policyelements.SessionInvocationIgnored {
		t.Fatalf("manual commit outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
	assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
	installSessionInvocation(t, harness, 2, "summarize the committed report", []continuation.ToolDefinition{
		{Name: "tool", Parameters: json.RawMessage(`{"type":"object"}`)},
	})
	sendObservationCreate(t, harness, "response-1", commit)
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	generate := trigger.Payload.(cognitionelements.Generate)
	if generate.Invocation.Instruction != "summarize the committed report" ||
		generate.Invocation.SourceRevision != 0 || len(generate.Invocation.Tools) != 0 ||
		len(generate.Invocation.Capabilities) != 0 || generate.SpokeOver ||
		generate.CommittedContext == nil || *generate.CommittedContext != commit.Context ||
		generate.ExpectedContextVersion == nil || *generate.ExpectedContextVersion != 2 ||
		generate.ExpectedContextItemID != "snapshot-2" {
		t.Fatalf("manual model invocation = %+v", generate)
	}
	if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationEmitted ||
		outcome.InvocationRevision != 2 || (outcome.Choice == nil || !outcome.Choice.Speak) {
		t.Fatalf("manual response outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
	sendObservationCreate(t, harness, "response-1", commit)
	if outcome := observationInvocationOutcome(t, harness); outcome.Code != "duplicate_trigger" {
		t.Fatalf("repeated response outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
}

func TestObservationInvocationManualContextDoesNotRegressOnDelayedCommit(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint("automatic=", automatic), func(t *testing.T) {
			harness := mountObservationInvocation(t, automatic)
			installSessionInvocation(t, harness, 1, "answer", nil)
			for _, version := range []uint64{3, 1} {
				commit := committedObservation(t, "messages", "typed", "user", fmt.Sprint("snapshot-", version), version, 1)
				sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(fmt.Sprint("commit-", version), "session-policy", commit))
				if automatic {
					_ = receivePolicy(t, harness.egress(t, "trigger"))
					_ = receivePolicy(t, harness.egress(t, "authority"))
				}
				_ = observationInvocationOutcome(t, harness)
			}
			stale := committedObservation(t, "messages", "typed", "user", "snapshot-1", 1, 1)
			sendObservationCreate(t, harness, "stale-response", stale)
			if outcome := observationInvocationOutcome(t, harness); outcome.Code != "invalid_create" ||
				!strings.Contains(outcome.Message, "moved backwards") {
				t.Fatalf("stale response outcome = %+v", outcome)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
		})
	}
}

func TestObservationInvocationAutomaticModeHasObservationAuthorityAndDurableCancellation(t *testing.T) {
	harness := mountObservationInvocation(t, true)
	installSessionInvocation(t, harness, 1, "answer from the file", nil)
	commit := committedObservation(t, "files", "uploaded", "user-file", "snapshot-1", 1, 5)
	sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit-1", "session-policy", commit))
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	generate := trigger.Payload.(cognitionelements.Generate)
	_ = receivePolicy(t, harness.egress(t, "authority"))
	if generate.Invocation.SourceRevision != 5 || generate.CommittedContext == nil ||
		*generate.CommittedContext != commit.Context || generate.SpokeOver {
		t.Fatalf("automatic model invocation = %+v", generate)
	}
	if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationEmitted || (outcome.Choice == nil || !outcome.Choice.Speak) {
		t.Fatalf("automatic outcome = %+v", outcome)
	}
	sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "withdraw-file", SessionID: "session-policy",
		Payload: policyelements.GenerationCancel{StreamID: "files", Reason: "withdrawn"},
	})
	_ = observationInvocationOutcome(t, harness)
	for _, revision := range []uint64{6, 7} {
		commit := committedObservation(t, "files", fmt.Sprint("uploaded-", revision), "user-file", "snapshot", revision, revision)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(fmt.Sprint("commit-", revision), "session-policy", commit))
		if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationCanceled {
			t.Fatalf("revision %d outcome = %+v", revision, outcome)
		}
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
	assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
}

func TestObservationInvocationRefusesUntrustedPurposeAndMalformedOrUnconfiguredRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*policyelements.ResponseCreate)
		install bool
		code    string
	}{
		{"settings absent", func(*policyelements.ResponseCreate) {}, false, "invocation_unset"},
		{"trusted semantic purpose", func(c *policyelements.ResponseCreate) {
			c.TrustedPurpose = policyelements.ResponseCreatePurposePostCommitSilence
		}, true, "invalid_create"},
		{"context absent", func(c *policyelements.ResponseCreate) { c.ExpectedContextVersion = nil }, true, "invalid_create"},
		{"context drift", func(c *policyelements.ResponseCreate) { c.ExpectedContextItemID = "different-snapshot" }, true, "invalid_create"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := mountObservationInvocation(t, false)
			if test.install {
				installSessionInvocation(t, harness, 1, "answer", nil)
			}
			commit := committedObservation(t, "files", "uploaded", "user-file", "snapshot-1", 1, 1)
			create := policyelements.ResponseCreate{ResponseID: "response", ExpectedContextVersion: &commit.StoreVersion,
				ExpectedContextItemID: commit.Context.StateItemID, CommittedContext: &commit.Context}
			test.mutate(&create)
			sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
				Type: policyelements.ResponseCreateType(), ItemID: "create", SessionID: "session-policy", Payload: create,
			})
			if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != test.code {
				t.Fatalf("refusal = %+v", outcome)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
		})
	}
}

func TestObservationInvocationRejectsMalformedCommitsBeforeAutomaticActivation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*element.Envelope)
	}{
		{"semantic grant", func(e *element.Envelope) { e.Payload = policyelements.SemanticGrant{} }},
		{"missing session", func(e *element.Envelope) { e.SessionID = "" }},
		{"malformed prefix", func(e *element.Envelope) {
			commit := e.Payload.(stateelements.ObservationCommitOutcome)
			commit.Context.Prefix.Digest = "sha256:bad"
			e.Payload = commit
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := mountObservationInvocation(t, true)
			installSessionInvocation(t, harness, 1, "answer", nil)
			commit := committedObservation(t, "files", "uploaded", "user-file", "snapshot", 1, 1)
			envelope := commitEnvelope("commit", "session-policy", commit)
			test.mutate(&envelope)
			sendPolicy(t, harness.ingress(t, "committed"), envelope)
			if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != "invalid_commit" {
				t.Fatalf("invalid observation admitted: %+v", outcome)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
			assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
		})
	}
}

func TestObservationInvocationRejectsReplayedSettingsAndUnknownConfiguration(t *testing.T) {
	harness := mountObservationInvocation(t, false)
	installSessionInvocation(t, harness, 2, "accepted instruction", nil)
	sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
		Type: policyelements.SessionInvocationUpdateType(), ItemID: "replayed-settings", SessionID: "session-policy",
		Payload: policyelements.SessionInvocationUpdate{Revision: 1, Invocation: continuation.Invocation{Instruction: "stale instruction"}},
	})
	if outcome := observationInvocationOutcome(t, harness); outcome.Kind != policyelements.SessionInvocationIgnored || outcome.Code != "stale_update" {
		t.Fatalf("replayed settings outcome = %+v", outcome)
	}
	commit := committedObservation(t, "messages", "typed", "user", "snapshot", 1, 1)
	sendObservationCreate(t, harness, "response", commit)
	generate := receivePolicy(t, harness.egress(t, "trigger")).Payload.(cognitionelements.Generate)
	if generate.Invocation.Instruction != "accepted instruction" {
		t.Fatalf("replay changed instruction: %+v", generate.Invocation)
	}
	_ = observationInvocationOutcome(t, harness)
	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if registration.Profile.Reference != "policy.ObservationInvocation" {
			continue
		}
		validator := registration.Factory.(element.ConfigValidator)
		for _, config := range []string{`{"role":"text","unknown":true}`, `{"role":"text","cancel_memory":-1}`, `{"role":""}`, `{"role":"text","generate_on_commit":"yes"}`} {
			if err := validator.ValidateConfig(json.RawMessage(config)); err == nil {
				t.Fatalf("accepted config %s", config)
			}
		}
		return
	}
	t.Fatal("observation invocation factory is absent")
}
