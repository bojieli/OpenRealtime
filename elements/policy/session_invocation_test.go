package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const sessionInvocationGraph = `graph session_invocation_test {
    policy.SessionInvocation :: policy;
    input update = policy.update;
    input committed = policy.committed;
    input create = policy.create;
    input cancel = policy.cancel;
    output trigger = policy.trigger;
    output authority = policy.authority;
    output state = policy.state;
    output outcome = policy.outcome;
}
`

func TestSessionInvocationDescriptorOwnsDynamicSettingsAndActivation(t *testing.T) {
	descriptor := policyelements.SessionInvocationDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Revision != 1 || descriptor.Name != "policy.SessionInvocation" {
		t.Fatalf("descriptor identity = %s@%d", descriptor.Name, descriptor.Revision)
	}
	want := map[string]string{
		"update":    "Event<policy.SessionInvocationUpdate>",
		"committed": "Event<policy.SemanticGrant>",
		"create":    "Trigger<policy.ResponseCreate>",
		"cancel":    "Interrupt<policy.GenerationAddress>",
		"trigger":   "Trigger<cognition.Generate>",
		"authority": "Stream<authority.Candidate>",
		"state":     "State<policy.SessionInvocationState>",
		"outcome":   "Event<policy.SessionInvocationOutcome>",
	}
	for name, typeName := range want {
		port, found := descriptor.Port(name)
		if !found || port.Type.String() != typeName {
			t.Fatalf("descriptor port %s = %+v, want %s", name, port, typeName)
		}
	}
}

func TestSessionInvocationSnapshotsExactInstructionsToolsAndCommittedBasis(t *testing.T) {
	harness := mountSessionInvocation(t)
	defer harness.stop(t)
	startup := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if startup.InvocationRevision != 0 || startup.Role != "foreground" {
		t.Fatalf("startup state = %+v", startup)
	}
	parameters := json.RawMessage(`{"type":"object","properties":{"digit":{"type":"string"}},"required":["digit"]}`)
	update := policyelements.SessionInvocationUpdate{
		Revision: 7,
		Invocation: continuation.Invocation{
			Instruction: "Press the matching key and say nothing.",
			Tools: []continuation.ToolDefinition{{
				Name: "press_key", Description: "Send a keypad tone.", Parameters: parameters,
			}},
			MaxOutputTokens: 96,
		},
	}
	sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
		Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-7",
		SessionID: "session-policy", Payload: update,
	})
	updated := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if updated.Kind != policyelements.SessionInvocationUpdated || updated.InvocationRevision != 7 ||
		!strings.HasPrefix(updated.InvocationDigest, "sha256:") ||
		state.InvocationDigest != updated.InvocationDigest || state.Updated != 1 {
		t.Fatalf("update outcome=%+v state=%+v", updated, state)
	}
	update.Invocation.Instruction = "mutated"
	parameters[0] = '['

	commit := committedObservation(t, "microphone", "speech-final", "trajectory-user", "trajectory-state", 1, 9)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: policyelements.SemanticGrantType(), ItemID: "commit-user",
		SessionID: "session-policy", Payload: semanticGrant(commit, interaction.ActAnswer),
	})
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	authority := receivePolicy(t, harness.egress(t, "authority"))
	payload, ok := trigger.Payload.(cognitionelements.Generate)
	if !ok {
		t.Fatalf("trigger payload type = %T", trigger.Payload)
	}
	if payload.Invocation.Instruction != "Press the matching key and say nothing." ||
		payload.Invocation.SourceRevision != 9 || payload.Invocation.MaxOutputTokens != 96 ||
		len(payload.Invocation.Tools) != 1 || payload.Invocation.Tools[0].Name != "press_key" ||
		!json.Valid(payload.Invocation.Tools[0].Parameters) ||
		payload.ExpectedContextVersion == nil || *payload.ExpectedContextVersion != 1 ||
		payload.ExpectedContextItemID != "trajectory-state" || payload.CommittedContext == nil ||
		!reflect.DeepEqual(*payload.CommittedContext, commit.Context) {
		t.Fatalf("exact session invocation trigger = %+v", payload)
	}
	if trigger.RunID == "" || authority.RunID != trigger.RunID ||
		!slicesContain(trigger.CausalParents, "trajectory-user") {
		t.Fatalf("trigger=%+v authority=%+v", trigger, authority)
	}
	emitted := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	settled := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if emitted.Kind != policyelements.SessionInvocationEmitted || emitted.Operation != "committed" ||
		emitted.InvocationRevision != 7 || emitted.InvocationDigest != updated.InvocationDigest ||
		settled.Emitted != 1 || settled.ContextVersion != 1 {
		t.Fatalf("emitted=%+v settled=%+v", emitted, settled)
	}
	assertSessionInvocationLiveResolution(t, harness.mounted)
}

func TestSessionInvocationManualCreateIsVisibleAndCarriesNoObservationAuthority(t *testing.T) {
	harness := mountSessionInvocation(t)
	defer harness.stop(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	installSessionInvocation(t, harness, 1, "Continue after the tool result.", []continuation.ToolDefinition{{
		Name: "press_key", Description: "Send a keypad tone.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"digit":{"type":"string"}}}`),
	}})
	version := uint64(7)
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "create-1", SessionID: "session-policy",
		Payload: policyelements.ResponseCreate{
			ResponseID: "response-1", ExpectedContextVersion: &version,
			ExpectedContextItemID: "trajectory-state-7",
		},
	})
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	payload := trigger.Payload.(cognitionelements.Generate)
	if payload.Invocation.Instruction != "Continue after the tool result." ||
		payload.Invocation.SourceRevision != 0 || len(payload.Invocation.Capabilities) != 0 ||
		len(payload.Invocation.Tools) != 0 ||
		payload.ExpectedContextVersion == nil || *payload.ExpectedContextVersion != version ||
		payload.ExpectedContextItemID != "trajectory-state-7" ||
		payload.CommittedContext != nil {
		t.Fatalf("manual trigger = %+v", payload)
	}
	version = 99
	if *payload.ExpectedContextVersion != 7 {
		t.Fatalf("manual trigger retained a caller-owned context pointer: %+v", payload)
	}
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if outcome.Kind != policyelements.SessionInvocationEmitted || outcome.Operation != "create" ||
		outcome.GenerationID != trigger.RunID || outcome.ContextVersion != 7 ||
		state.ContextVersion != 7 || !slicesContain(trigger.CausalParents, "trajectory-state-7") {
		t.Fatalf("manual create outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
	staleVersion := uint64(6)
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "create-stale-context",
		SessionID: "session-policy", Payload: policyelements.ResponseCreate{
			ResponseID: "response-stale-context", ExpectedContextVersion: &staleVersion,
			ExpectedContextItemID: "trajectory-state-6",
		},
	})
	staleOutcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	staleState := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if staleOutcome.Kind != policyelements.SessionInvocationRefused ||
		staleOutcome.Code != "invalid_create" || staleState.ContextVersion != 7 {
		t.Fatalf("stale manual context outcome=%+v state=%+v", staleOutcome, staleState)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
}

func TestSessionInvocationExecutesTrustedPostCommitSilencePurpose(t *testing.T) {
	harness := mountSessionInvocation(t)
	defer harness.stop(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	installSessionInvocation(t, harness, 1, "Be concise and follow the user's standing requests.", nil)
	version := uint64(4)
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "quiet-create",
		SessionID: "session-policy", Payload: policyelements.ResponseCreate{
			ResponseID: "quiet-response", ExpectedContextVersion: &version,
			ExpectedContextItemID: "trajectory-state-4",
			TrustedPurpose:        policyelements.ResponseCreatePurposePostCommitSilence,
		},
	})
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	payload := trigger.Payload.(cognitionelements.Generate)
	if !strings.HasPrefix(payload.Invocation.Instruction,
		"Be concise and follow the user's standing requests.\n\nTrusted runtime purpose:") ||
		!strings.Contains(payload.Invocation.Instruction, "Execute that due standing action now") ||
		!strings.Contains(payload.Invocation.Instruction, "Do not merely acknowledge") ||
		payload.Invocation.SourceRevision != 0 || len(payload.Invocation.Tools) != 0 {
		t.Fatalf("trusted quiet invocation = %+v", payload.Invocation)
	}
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
	if outcome.Kind != policyelements.SessionInvocationEmitted || outcome.Operation != "create" ||
		state.Emitted != 1 || state.ContextVersion != version {
		t.Fatalf("trusted quiet outcome=%+v state=%+v", outcome, state)
	}
}

func TestSessionInvocationFailsClosedOnMissingStaleOrMalformedSettings(t *testing.T) {
	t.Run("commit before settings", func(t *testing.T) {
		harness := mountSessionInvocation(t)
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "microphone", "speech", "trajectory-user", "state", 1, 1)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: policyelements.SemanticGrantType(), ItemID: "commit",
			SessionID: "session-policy", Payload: semanticGrant(commit, interaction.ActAnswer),
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
		_ = receivePolicy(t, harness.egress(t, "state"))
		if outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != "invocation_unset" {
			t.Fatalf("unset outcome = %+v", outcome)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
	})

	t.Run("stale revision", func(t *testing.T) {
		harness := mountSessionInvocation(t)
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		installSessionInvocation(t, harness, 2, "first", nil)
		sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
			Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-stale",
			SessionID: "session-policy", Payload: policyelements.SessionInvocationUpdate{
				Revision: 2, Invocation: continuation.Invocation{Instruction: "replacement"},
			},
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SessionInvocationState)
		if outcome.Kind != policyelements.SessionInvocationIgnored || outcome.Code != "stale_update" ||
			state.InvocationRevision != 2 || state.Updated != 1 {
			t.Fatalf("stale outcome=%+v state=%+v", outcome, state)
		}
	})

	t.Run("duplicate tool", func(t *testing.T) {
		harness := mountSessionInvocation(t)
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		tool := continuation.ToolDefinition{Name: "press", Description: "press", Parameters: json.RawMessage(`{"type":"object"}`)}
		sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
			Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-duplicate",
			SessionID: "session-policy", Payload: policyelements.SessionInvocationUpdate{
				Revision: 1, Invocation: continuation.Invocation{Instruction: "answer", Tools: []continuation.ToolDefinition{tool, tool}},
			},
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
		_ = receivePolicy(t, harness.egress(t, "state"))
		if outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != "invalid_update" ||
			!strings.Contains(outcome.Message, "repeats tool") {
			t.Fatalf("duplicate tool outcome = %+v", outcome)
		}
	})

	t.Run("manual create without context binding", func(t *testing.T) {
		harness := mountSessionInvocation(t)
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		installSessionInvocation(t, harness, 1, "answer", nil)
		sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
			Type: policyelements.ResponseCreateType(), ItemID: "create-unbound",
			SessionID: "session-policy",
			Payload:   policyelements.ResponseCreate{ResponseID: "response-unbound"},
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
		_ = receivePolicy(t, harness.egress(t, "state"))
		if outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != "invalid_create" ||
			!strings.Contains(outcome.Message, "expected context version") {
			t.Fatalf("unbound manual create outcome = %+v", outcome)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
	})

	for _, testCase := range []struct {
		name    string
		context stateelements.CommittedContext
		want    string
	}{
		{
			name: "committed version disagrees", want: "disagrees",
			context: stateelements.CommittedContext{
				Prefix: trajectory.PrefixIdentity{
					Version: 6, Digest: "sha256:" + strings.Repeat("0", 64),
				},
				StateItemID: "trajectory-state-7",
			},
		},
		{
			name: "committed State identity disagrees", want: "disagrees",
			context: stateelements.CommittedContext{
				Prefix: trajectory.PrefixIdentity{
					Version: 7, Digest: "sha256:" + strings.Repeat("0", 64),
				},
				StateItemID: "trajectory-state-other",
			},
		},
		{
			name: "committed digest malformed", want: "canonical SHA-256",
			context: stateelements.CommittedContext{
				Prefix:      trajectory.PrefixIdentity{Version: 7, Digest: "sha256:not-a-digest"},
				StateItemID: "trajectory-state-7",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := mountSessionInvocation(t)
			defer harness.stop(t)
			_ = receivePolicy(t, harness.egress(t, "state"))
			installSessionInvocation(t, harness, 1, "answer", nil)
			version := uint64(7)
			sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
				Type: policyelements.ResponseCreateType(), ItemID: "create-bound-drift",
				SessionID: "session-policy", Payload: policyelements.ResponseCreate{
					ResponseID: "response-bound-drift", ExpectedContextVersion: &version,
					ExpectedContextItemID: "trajectory-state-7",
					CommittedContext:      &testCase.context,
				},
			})
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
			_ = receivePolicy(t, harness.egress(t, "state"))
			if outcome.Kind != policyelements.SessionInvocationRefused || outcome.Code != "invalid_create" ||
				!strings.Contains(outcome.Message, testCase.want) {
				t.Fatalf("bound-create drift outcome = %+v", outcome)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
		})
	}
}

func semanticGrant(
	commit stateelements.ObservationCommitOutcome, act interaction.Act,
) policyelements.SemanticGrant {
	return policyelements.SemanticGrant{
		Commit: commit, Act: act, DecisionItemID: "semantic-decision-for-" + commit.TriggerItemID,
	}
}

func TestSessionInvocationFactoryRejectsUnknownConfig(t *testing.T) {
	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if registration.Profile.Reference != "policy.SessionInvocation" {
			continue
		}
		validator := registration.Factory.(element.ConfigValidator)
		if err := validator.ValidateConfig(json.RawMessage(`{"role":"foreground","unknown":true}`)); err == nil {
			t.Fatal("session invocation config accepted an unknown field")
		}
		return
	}
	t.Fatal("session invocation factory is absent from the production registry")
}

func mountSessionInvocation(t *testing.T) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	configuration, err := json.Marshal(policyelements.SessionInvocationConfig{
		Role: "foreground", TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "session-invocation-test.ortg", []byte(sessionInvocationGraph)),
		Registry: registry, Values: map[string]json.RawMessage{"policy": configuration},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func installSessionInvocation(
	t *testing.T, harness policyHarness, revision uint64, instruction string,
	tools []continuation.ToolDefinition,
) {
	t.Helper()
	capabilities := make([]continuation.Capability, len(tools))
	for index := range tools {
		capabilities[index] = continuation.Capability{
			Name: tools[index].Name, Description: tools[index].Description, Available: true,
			ExecutionPhase: "fast",
		}
	}
	sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
		Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-install",
		SessionID: "session-policy", Payload: policyelements.SessionInvocationUpdate{
			Revision: revision, Invocation: continuation.Invocation{
				Instruction: instruction, Capabilities: capabilities, Tools: tools,
			},
		},
	})
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SessionInvocationOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))
	if outcome.Kind != policyelements.SessionInvocationUpdated {
		t.Fatalf("install outcome = %+v", outcome)
	}
}

func assertSessionInvocationLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes["policy"].Resolution
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "builtin://openrealtime/elements/policy.SessionInvocation" &&
			resolution.Runtime.Revision == "implementation:3" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session invocation live resolution = %+v", resolution)
		}
		time.Sleep(time.Millisecond)
	}
}

func slicesContain(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func TestSessionInvocationRunnerStopsCleanly(t *testing.T) {
	harness := mountSessionInvocation(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session invocation runner did not stop")
	}
}
