package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const generationPolicyGraph = `graph generation_policy_test {
    policy.GenerateOnObservation :: activation;
    input committed = activation.committed;
    input cancel = activation.cancel;
    output trigger = activation.trigger;
    output authority = activation.authority;
    output state = activation.state;
    output outcome = activation.outcome;
}
`

func TestGenerateOnObservationDescriptorUsesSelfContainedCommitBasis(t *testing.T) {
	descriptor := policyelements.GenerateOnObservationDescriptor()
	if descriptor.Revision != 3 {
		t.Fatalf("descriptor revision = %d, want immutable successor 3", descriptor.Revision)
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, found := descriptor.Port("context"); found {
		t.Fatal("revision 3 retained the racy trajectory context input")
	}
	wantPorts := map[string]string{
		"committed": "Event<trajectory.ObservationCommitOutcome>",
		"cancel":    "Interrupt<policy.GenerationAddress>",
		"trigger":   "Trigger<cognition.Generate>",
		"authority": "Stream<authority.Candidate>",
		"state":     "State<policy.GenerationState>",
		"outcome":   "Event<policy.GenerationOutcome>",
	}
	for name, want := range wantPorts {
		port, found := descriptor.Port(name)
		if !found || port.Type.String() != want {
			t.Fatalf("port %s = %+v, want %s", name, port, want)
		}
		if name == "committed" && port.LossAllowed {
			t.Fatal("committed activation evidence allows loss")
		}
	}
	if !reflect.DeepEqual(descriptor.Reaction.Triggers, []string{"committed"}) ||
		!reflect.DeepEqual(descriptor.Reaction.Interrupts, []string{"cancel"}) ||
		!descriptor.Reaction.BreaksCycles || descriptor.Reaction.MaxConcurrency != 1 {
		t.Fatalf("reaction = %+v", descriptor.Reaction)
	}
	if len(descriptor.Effects) != 1 || descriptor.Effects[0].Name != "policy.activation.memory" ||
		strings.Contains(descriptor.Effects[0].Name, "pending") {
		t.Fatalf("descriptor effects still claim a pending join: %+v", descriptor.Effects)
	}
}

func TestGenerateOnObservationImmediatelyActivatesIndependentRoles(t *testing.T) {
	commit := committedObservation(t, "microphone", "observation-trigger",
		"trajectory-observation-7", "trajectory-state-1", 1, 7)
	type activation struct {
		role      string
		harness   policyHarness
		trigger   element.Envelope
		candidate element.Envelope
		payload   cognitionelements.Generate
	}
	activations := []activation{
		{role: "fast", harness: mountPolicy(t, validPolicyConfig("fast"))},
		{role: "deliberative", harness: mountPolicy(t, validPolicyConfig("deliberative"))},
	}
	for index := range activations {
		current := &activations[index]
		defer current.harness.stop(t)
		startup := receivePolicy(t, current.harness.egress(t, "state")).Payload.(policyelements.GenerationState)
		if startup.Role != current.role || startup.ContextVersion != 0 ||
			startup.TerminalMemory != 8 || startup.CancellationMemory != 8 {
			t.Fatalf("%s startup state = %+v", current.role, startup)
		}
		source := commit
		sendPolicy(t, current.harness.ingress(t, "committed"), commitEnvelope(
			"commit-envelope", "policy-test-session", &source,
		))
		current.trigger = receivePolicy(t, current.harness.egress(t, "trigger"))
		current.candidate = receivePolicy(t, current.harness.egress(t, "authority"))
		payload, ok := current.trigger.Payload.(cognitionelements.Generate)
		if !ok {
			t.Fatalf("%s trigger payload type = %T", current.role, current.trigger.Payload)
		}
		current.payload = payload
		assertActivation(t, current.role, current.trigger, current.candidate, payload, commit)
		outcome := receivePolicy(t, current.harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationEmitted ||
			outcome.GenerationID != current.trigger.RunID || outcome.Role != current.role ||
			outcome.ContextVersion != 1 || outcome.SourceRevision != 7 ||
			outcome.TriggerItemID != "observation-trigger" {
			t.Fatalf("%s outcome = %+v", current.role, outcome)
		}
		settled := receivePolicy(t, current.harness.egress(t, "state")).Payload.(policyelements.GenerationState)
		if settled.Emitted != 1 || settled.ContextVersion != 1 {
			t.Fatalf("%s settled state = %+v", current.role, settled)
		}
		assertStateHasNoPendingSurface(t, settled)
		assertPolicyLiveResolution(t, current.harness.mounted)
	}
	if activations[0].trigger.RunID == activations[1].trigger.RunID {
		t.Fatalf("independent roles shared generation identity %q", activations[0].trigger.RunID)
	}
	if !reflect.DeepEqual(*activations[0].payload.CommittedContext,
		*activations[1].payload.CommittedContext) {
		t.Fatalf("roles selected different committed bases: fast=%+v slow=%+v",
			activations[0].payload.CommittedContext, activations[1].payload.CommittedContext)
	}
}

func TestGenerateOnObservationRefusesTamperedOrMissingCompactBasis(t *testing.T) {
	valid := committedObservation(t, "camera", "frame-trigger",
		"trajectory-frame", "trajectory-state-3", 3, 11)
	tests := []struct {
		name   string
		mutate func(*stateelements.ObservationCommitOutcome)
		want   string
	}{
		{name: "zero store revision", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.StoreVersion = 0
		}, want: "positive store"},
		{name: "zero source revision", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.SourceRevision = 0
		}, want: "positive store"},
		{name: "zero observation revision", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.ObservationRevision = 0
		}, want: "positive store"},
		{name: "prefix version drift", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.Prefix.Version--
		}, want: "does not match store version"},
		{name: "missing digest", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.Prefix.Digest = ""
		}, want: "canonical SHA-256"},
		{name: "uppercase digest", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.Prefix.Digest = strings.ToUpper(value.Context.Prefix.Digest)
		}, want: "canonical SHA-256"},
		{name: "nonhex digest", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.Prefix.Digest = "sha256:" + strings.Repeat("z", 64)
		}, want: "canonical SHA-256"},
		{name: "missing State item ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.StateItemID = ""
		}, want: "State item ID is required"},
		{name: "noncanonical State item ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.Context.StateItemID = " state-item"
		}, want: "surrounding whitespace"},
		{name: "missing trajectory tail ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.TrajectoryItemID = ""
		}, want: "trajectory item ID is required"},
		{name: "noncanonical trajectory tail ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.TrajectoryItemID = "tail id"
		}, want: "whitespace"},
		{name: "missing trigger ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.TriggerItemID = ""
		}, want: "trigger item ID is required"},
		{name: "missing stream ID", mutate: func(value *stateelements.ObservationCommitOutcome) {
			value.StreamID = ""
		}, want: "stream ID is required"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			harness := mountPolicy(t, validPolicyConfig("fast"))
			defer harness.stop(t)
			_ = receivePolicy(t, harness.egress(t, "state"))
			commit := valid
			testCase.mutate(&commit)
			sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
				"commit-tampered", "policy-test-session", commit,
			))
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
			if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "invalid_commit" ||
				!strings.Contains(outcome.Message, testCase.want) {
				t.Fatalf("tampered outcome = %+v, want message containing %q", outcome, testCase.want)
			}
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
			if state.Refused != 1 || state.Emitted != 0 {
				t.Fatalf("tampered state = %+v", state)
			}
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
			assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
		})
	}
}

func TestGenerateOnObservationCopiesCompactBasisWithoutTrajectoryPayload(t *testing.T) {
	harness := mountPolicy(t, validPolicyConfig("fast"))
	defer harness.stop(t)
	_ = receivePolicy(t, harness.egress(t, "state"))
	source := committedObservation(t, "microphone", "observation-trigger",
		"trajectory-item", "trajectory-state", 2, 13)
	wantContext := source.Context
	sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
		"commit-stable", "policy-test-session", &source,
	))
	trigger := receivePolicy(t, harness.egress(t, "trigger"))
	payload := trigger.Payload.(cognitionelements.Generate)
	if payload.CommittedContext == nil || payload.CommittedContext == &source.Context ||
		!reflect.DeepEqual(*payload.CommittedContext, wantContext) {
		t.Fatalf("trigger did not defensively copy compact context: payload=%+v source=%+v",
			payload.CommittedContext, source.Context)
	}
	source.Context.StateItemID = "mutated-source"
	source.Context.Prefix.Digest = "mutated-source"
	if !reflect.DeepEqual(*payload.CommittedContext, wantContext) {
		t.Fatalf("retained producer mutation changed trigger context: %+v", payload.CommittedContext)
	}
	payload.CommittedContext.StateItemID = "mutated-trigger"
	if source.Context.StateItemID != "mutated-source" {
		t.Fatalf("trigger context aliases producer context: %+v", source.Context)
	}
	wire, err := json.Marshal(trigger.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"items"`) ||
		strings.Contains(string(wire), "private trajectory content") {
		t.Fatalf("activation retained a trajectory payload: %s", wire)
	}
	_ = receivePolicy(t, harness.egress(t, "authority"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
}

func TestGenerateOnObservationDuplicateAndTerminalMemoryAreDeterministic(t *testing.T) {
	t.Run("duplicate is ignored", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "microphone", "trigger-one",
			"trajectory-one", "state-one", 1, 1)
		envelope := commitEnvelope("commit-one", "session-one", commit)
		sendPolicy(t, harness.ingress(t, "committed"), envelope)
		first := receiveActivation(t, harness)
		sendPolicy(t, harness.ingress(t, "committed"), envelope)
		duplicate := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if duplicate.Kind != policyelements.GenerationIgnored || duplicate.Code != "duplicate_commit" ||
			duplicate.GenerationID != first.trigger.RunID {
			t.Fatalf("duplicate outcome = %+v", duplicate)
		}
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
		if state.Emitted != 1 || state.Ignored != 1 {
			t.Fatalf("duplicate state = %+v", state)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
		assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
	})

	t.Run("oldest terminal identity is evicted first", func(t *testing.T) {
		harness := mountPolicy(t, policyConfig("fast", 1, 1, 8))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		first := committedObservation(t, "stream-one", "trigger-one",
			"trajectory-one", "state-one", 1, 1)
		second := committedObservation(t, "stream-two", "trigger-two",
			"trajectory-two", "state-two", 2, 2)
		for _, input := range []struct {
			id     string
			commit stateelements.ObservationCommitOutcome
		}{
			{id: "commit-one", commit: first},
			{id: "commit-two", commit: second},
			{id: "commit-one-replayed-after-bounded-eviction", commit: first},
		} {
			sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
				input.id, "bounded-session", input.commit,
			))
			_ = receiveActivation(t, harness)
		}
	})

	t.Run("session is part of duplicate identity", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "microphone", "same-trigger",
			"same-tail", "same-state", 1, 1)
		var runIDs []string
		for _, sessionID := range []string{"session-a", "session-b"} {
			sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
				"commit-"+sessionID, sessionID, commit,
			))
			runIDs = append(runIDs, receiveActivation(t, harness).trigger.RunID)
		}
		if runIDs[0] == runIDs[1] {
			t.Fatalf("cross-session activations shared ID %q", runIDs[0])
		}
	})
}

func TestGenerateOnObservationPreCancellationIsBoundedAndSessionScoped(t *testing.T) {
	t.Run("matching stream is canceled before activation", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendCancellation(t, harness, "cancel-before-commit", "session-a",
			policyelements.GenerationCancel{StreamID: "microphone", Reason: "barge-in"})
		commit := committedObservation(t, "microphone", "observation-trigger",
			"trajectory-item", "state-item", 1, 3)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
			"commit-after-cancel", "session-a", commit,
		))
		canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if canceled.Kind != policyelements.GenerationCanceled || canceled.Message != "barge-in" {
			t.Fatalf("pre-canceled outcome = %+v", canceled)
		}
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
		if state.Canceled != 1 {
			t.Fatalf("pre-canceled state = %+v", state)
		}
		assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
		assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
	})

	t.Run("pre-cancel cannot cross session", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendCancellation(t, harness, "cancel-session-a", "session-a",
			policyelements.GenerationCancel{StreamID: "microphone", Reason: "session-a correction"})
		commit := committedObservation(t, "microphone", "observation-trigger",
			"trajectory-item", "state-item", 1, 3)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
			"commit-session-b", "session-b", commit,
		))
		if activation := receiveActivation(t, harness); activation.trigger.SessionID != "session-b" {
			t.Fatalf("session-b activation = %+v", activation.trigger)
		}
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
			"commit-session-a", "session-a", commit,
		))
		canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if canceled.Kind != policyelements.GenerationCanceled {
			t.Fatalf("session-a pre-cancel = %+v", canceled)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})

	t.Run("oldest cancellation is evicted first", func(t *testing.T) {
		harness := mountPolicy(t, policyConfig("fast", 1, 8, 1))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendCancellation(t, harness, "cancel-a", "session",
			policyelements.GenerationCancel{StreamID: "stream-a", Reason: "first"})
		sendCancellation(t, harness, "cancel-b", "session",
			policyelements.GenerationCancel{StreamID: "stream-b", Reason: "second"})
		first := committedObservation(t, "stream-a", "trigger-a",
			"tail-a", "state-a", 1, 1)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit-a", "session", first))
		_ = receiveActivation(t, harness)
		second := committedObservation(t, "stream-b", "trigger-b",
			"tail-b", "state-b", 2, 2)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit-b", "session", second))
		canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if canceled.Kind != policyelements.GenerationCanceled || canceled.Message != "second" {
			t.Fatalf("bounded pre-cancel outcome = %+v", canceled)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})
}

func TestGenerateOnObservationValidatesSessionEnvelopeAndIgnoresRejection(t *testing.T) {
	t.Run("missing session", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "stream", "trigger", "tail", "state", 1, 1)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit", "", commit))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "missing_session" {
			t.Fatalf("missing-session outcome = %+v", outcome)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})
	for name, sessionID := range map[string]string{
		"surrounding whitespace": " session",
		"embedded control":       "session\n",
		"overlong":               strings.Repeat("s", 257),
		"invalid UTF-8":          string([]byte{0xff}),
	} {
		t.Run("invalid session/"+name, func(t *testing.T) {
			harness := mountPolicy(t, validPolicyConfig("fast"))
			defer harness.stop(t)
			_ = receivePolicy(t, harness.egress(t, "state"))
			commit := committedObservation(t, "stream", "trigger", "tail", "state", 1, 1)
			sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit", sessionID, commit))
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
			if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "invalid_commit_envelope" {
				t.Fatalf("invalid-session outcome = %+v", outcome)
			}
			_ = receivePolicy(t, harness.egress(t, "state"))
			assertNoPolicyEnvelope(t, harness.egress(t, "trigger"))
			assertNoPolicyEnvelope(t, harness.egress(t, "authority"))
		})
	}
	t.Run("invalid cancel session is not retained", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
			Type: policyelements.GenerationCancelType(), ItemID: "hostile-cancel",
			SessionID: strings.Repeat("s", 257),
			Payload:   policyelements.GenerationCancel{StreamID: "stream", Reason: "hostile"},
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "invalid_cancel" {
			t.Fatalf("invalid cancellation session = %+v", outcome)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "stream", "trigger", "tail", "state", 1, 1)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit", "session", commit))
		_ = receiveActivation(t, harness)
	})
	t.Run("invalid envelope item", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		commit := committedObservation(t, "stream", "trigger", "tail", "state", 1, 1)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("commit item", "session", commit))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "invalid_commit_envelope" {
			t.Fatalf("invalid-envelope outcome = %+v", outcome)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})
	t.Run("rejected store outcome needs no committed context", func(t *testing.T) {
		harness := mountPolicy(t, validPolicyConfig("fast"))
		defer harness.stop(t)
		_ = receivePolicy(t, harness.egress(t, "state"))
		rejected := stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationRejected, TriggerItemID: "trigger",
			Code: "version_conflict", Message: "stale",
		}
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope("rejection", "", rejected))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
		if outcome.Kind != policyelements.GenerationIgnored || outcome.Code != "observation_not_committed" {
			t.Fatalf("rejected commit outcome = %+v", outcome)
		}
		_ = receivePolicy(t, harness.egress(t, "state"))
	})
}

func TestGenerateOnObservationDeprecatedMaxPendingDoesNotCreateBuffer(t *testing.T) {
	harness := mountPolicy(t, policyConfig("fast", 1, 8, 8))
	defer harness.stop(t)
	startup := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	assertStateHasNoPendingSurface(t, startup)
	for index := uint64(1); index <= 3; index++ {
		commit := committedObservation(t,
			"stream-"+string(rune('a'+index-1)),
			"trigger-"+string(rune('a'+index-1)),
			"tail-"+string(rune('a'+index-1)),
			"state-"+string(rune('a'+index-1)),
			index, index,
		)
		sendPolicy(t, harness.ingress(t, "committed"), commitEnvelope(
			"commit-"+string(rune('a'+index-1)), "session", commit,
		))
		activation := receiveActivation(t, harness)
		if activation.state.Emitted != index {
			t.Fatalf("activation %d state = %+v", index, activation.state)
		}
		assertStateHasNoPendingSurface(t, activation.state)
	}
}

func TestGenerateOnObservationConfigIsStrictAndBounded(t *testing.T) {
	graph := compilePolicySource(t, "policy-config.ortg", []byte(generationPolicyGraph))
	for name, value := range map[string]json.RawMessage{
		"missing role":      json.RawMessage(`{"invocation":{"instruction":"answer"}}`),
		"unknown field":     json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"mystery":true}`),
		"derived revision":  json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer","source_revision":9}}`),
		"empty instruction": json.RawMessage(`{"role":"fast","invocation":{"instruction":""}}`),
		"unbounded pending": json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"max_pending":1000001}`),
		"terminal zero":     json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"terminal_memory":0}`),
		"cancel unbounded":  json.RawMessage(`{"role":"fast","invocation":{"instruction":"answer"},"cancel_memory":1000001}`),
	} {
		t.Run(name, func(t *testing.T) {
			registry := graphruntime.NewRegistry()
			if err := policyelements.RegisterFactories(registry); err != nil {
				t.Fatal(err)
			}
			if _, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry,
				Values: map[string]json.RawMessage{"activation": value},
			}); err == nil {
				t.Fatal("invalid policy config mounted")
			}
		})
	}
}

func TestGenerationPolicyReferenceCompilesFromExactLock(t *testing.T) {
	directory := filepath.Join("..", "..", "graphs", "components", "generation-policy")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lockPayload, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(lockPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !lock.Equal(updated.Lock) {
		want, marshalErr := updated.Lock.Marshal()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		t.Fatalf("generation-policy lock is stale; generated lock:\n%s", want)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	valuesPayload, err := os.ReadFile(filepath.Join(directory, "agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML("agent.values.yaml", valuesPayload)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(compiled.Graph, document)
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range bound.Graph.Boundaries {
		if strings.Contains(boundary.Type.String(), "audio.") ||
			boundary.Name == "context" {
			t.Fatalf("generation policy retained an unrelated presentation/context boundary: %+v", boundary)
		}
	}
}

type policyHarness struct {
	mounted *graphruntime.Mounted
	done    <-chan error
	cancel  context.CancelFunc
}

type receivedActivation struct {
	trigger   element.Envelope
	authority element.Envelope
	outcome   policyelements.GenerationOutcome
	state     policyelements.GenerationState
}

func mountPolicy(t *testing.T, config json.RawMessage) policyHarness {
	t.Helper()
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph:    compilePolicySource(t, "policy-test.ortg", []byte(generationPolicyGraph)),
		Registry: registry, Values: map[string]json.RawMessage{"activation": config},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func (harness policyHarness) ingress(t *testing.T, name string) element.OutputPort {
	t.Helper()
	port, err := harness.mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (harness policyHarness) egress(t *testing.T, name string) element.InputPort {
	t.Helper()
	port, err := harness.mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (harness policyHarness) stop(t *testing.T) {
	t.Helper()
	harness.cancel()
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("generation policy stopped with %v", err)
		}
	case <-time.After(time.Second):
		t.Error("generation policy did not stop")
	}
}

func compilePolicySource(t *testing.T, name string, source []byte) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse(name, source)
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func validPolicyConfig(role string) json.RawMessage {
	return policyConfig(role, 4, 8, 8)
}

func policyConfig(role string, maxPending, terminalMemory, cancelMemory int) json.RawMessage {
	payload, err := json.Marshal(policyelements.GenerateOnObservationConfig{
		Role: role, Invocation: continuation.Invocation{
			Instruction: "answer", MaxOutputTokens: 128,
		},
		MaxPending: maxPending, TerminalMemory: terminalMemory, CancelMemory: cancelMemory,
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func committedObservation(
	t *testing.T, stream, trigger, item, stateItem string, version, sourceRevision uint64,
) stateelements.ObservationCommitOutcome {
	t.Helper()
	items := make([]trajectory.Item, version)
	for index := uint64(0); index < version; index++ {
		items[index] = trajectory.Item{
			ID: "prefix-item-" + string(rune('a'+index)), Kind: trajectory.KindInstruction,
			Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "private trajectory content",
		}
	}
	items[version-1] = trajectory.Item{
		ID: item, Kind: trajectory.KindObservation, SourceRevision: sourceRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Event:    &trajectory.EventMetadata{EventID: trigger},
		Content:  "private trajectory content",
	}
	identity, err := trajectory.IdentifyPrefix(trajectory.Snapshot{Version: version, Items: items}, version)
	if err != nil {
		t.Fatal(err)
	}
	return stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: trigger,
		TrajectoryItemID: item, StreamID: stream, ObservationRevision: 1,
		SourceRevision: sourceRevision, StoreVersion: version,
		Context: stateelements.CommittedContext{Prefix: identity, StateItemID: stateItem},
	}
}

func commitEnvelope(itemID, sessionID string, commit any) element.Envelope {
	return element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: itemID,
		SessionID: sessionID, Payload: commit,
	}
}

func sendCancellation(
	t *testing.T, harness policyHarness, itemID, sessionID string,
	cancel policyelements.GenerationCancel,
) {
	t.Helper()
	sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: itemID,
		SessionID: sessionID, Payload: cancel,
	})
	recorded := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
	if recorded.Kind != policyelements.GenerationIgnored || recorded.Code != "cancel_recorded" {
		t.Fatalf("recorded cancellation = %+v", recorded)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))
}

func receiveActivation(t *testing.T, harness policyHarness) receivedActivation {
	t.Helper()
	result := receivedActivation{
		trigger:   receivePolicy(t, harness.egress(t, "trigger")),
		authority: receivePolicy(t, harness.egress(t, "authority")),
	}
	result.outcome = receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.GenerationOutcome)
	result.state = receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.GenerationState)
	if result.outcome.Kind != policyelements.GenerationEmitted ||
		result.outcome.GenerationID != result.trigger.RunID {
		t.Fatalf("activation outcome = %+v trigger = %+v", result.outcome, result.trigger)
	}
	return result
}

func assertActivation(
	t *testing.T, role string, triggerEnvelope, candidateEnvelope element.Envelope,
	payload cognitionelements.Generate, commit stateelements.ObservationCommitOutcome,
) {
	t.Helper()
	version := payload.ExpectedContextVersion
	if version == nil || *version != commit.StoreVersion ||
		payload.ExpectedContextItemID != commit.Context.StateItemID ||
		payload.CommittedContext == nil || !reflect.DeepEqual(*payload.CommittedContext, commit.Context) ||
		payload.Invocation.SourceRevision != commit.SourceRevision ||
		payload.Invocation.Instruction != "answer" ||
		triggerEnvelope.RunID == "" ||
		triggerEnvelope.CancellationScope != triggerEnvelope.RunID ||
		!strings.HasSuffix(triggerEnvelope.ItemID, ":trigger") ||
		!containsPolicy(triggerEnvelope.CausalParents, "commit-envelope") ||
		!containsPolicy(triggerEnvelope.CausalParents, commit.Context.StateItemID) ||
		!containsPolicy(triggerEnvelope.CausalParents, commit.TrajectoryItemID) {
		t.Fatalf("%s generation trigger = %+v payload %+v", role, triggerEnvelope, payload)
	}
	candidate, ok := candidateEnvelope.Payload.(authority.Candidate)
	if !ok {
		t.Fatalf("%s authority candidate payload type = %T", role, candidateEnvelope.Payload)
	}
	if candidate.RunID != triggerEnvelope.RunID ||
		candidate.SessionID != "policy-test-session" ||
		candidate.ActivationItemID != triggerEnvelope.ItemID ||
		candidate.ActivationCauseItemID != "commit-envelope" ||
		candidate.ObservationItemID != commit.TrajectoryItemID ||
		candidate.ObservationTriggerItemID != commit.TriggerItemID ||
		candidate.SourceRevision != commit.SourceRevision ||
		candidate.ContextVersion != commit.StoreVersion ||
		candidate.ContextEnvelopeItemID != commit.Context.StateItemID ||
		candidate.ContextTailItem != commit.TrajectoryItemID ||
		candidateEnvelope.RunID != triggerEnvelope.RunID ||
		candidateEnvelope.SessionID != triggerEnvelope.SessionID {
		t.Fatalf("%s authority candidate = %+v envelope %+v", role, candidate, candidateEnvelope)
	}
}

func assertStateHasNoPendingSurface(t *testing.T, state policyelements.GenerationState) {
	t.Helper()
	wire, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "pending") || strings.Contains(string(wire), "max_pending") {
		t.Fatalf("revision 3 state claims a live pending buffer: %s", wire)
	}
}

func sendPolicy(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
}

func receivePolicy(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func assertNoPolicyEnvelope(t *testing.T, input element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected envelope: %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty receive: %v", err)
	}
}

func assertPolicyLiveResolution(t *testing.T, mounted *graphruntime.Mounted) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes["activation"].Resolution
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "builtin://openrealtime/elements/policy.GenerateOnObservation" &&
			resolution.Runtime.Revision == "implementation:4" &&
			resolution.CapabilitiesEvidence == inspect.EvidenceLive && len(resolution.Capabilities) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy live resolution = %+v", resolution)
		}
		time.Sleep(time.Millisecond)
	}
}

func containsPolicy(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
