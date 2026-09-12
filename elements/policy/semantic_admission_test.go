package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const semanticAdmissionGraph = `graph semantic_admission_test {
    policy.SemanticAdmission :: admission;
    input context = admission.context;
    input update = admission.update;
    input agent_output = admission.agent_output;
    input release = admission.release;
    output safe_release = admission.safe_release;
    input committed = admission.committed;
    input create = admission.create;
    input quiet = admission.quiet;
    input cancel = admission.cancel;
    output voice_committed = admission.voice_committed;
    output voice_create = admission.voice_create;
    output decision = admission.decision;
    output state = admission.state;
    output outcome = admission.outcome;
    output resolved = admission.resolved;
}
`

var semanticTestDescriptor = policyelements.SemanticDeciderDescriptor{
	Provider: "test-policy", Model: "enumerated-v1", Protocol: "test-v1",
	Revision: "immutable-1", ConfigurationDigest: "sha256:" + strings.Repeat("0", 64),
	DecisionTimeoutMS: 1_000,
}

func TestSemanticAdmissionContractRejectsUnpinnedProvidersAndUnboundedValues(t *testing.T) {
	descriptor := policyelements.SemanticAdmissionDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "policy.SemanticAdmission" || descriptor.Revision != 10 ||
		descriptor.ConfigSchema != "schema://openrealtime/policy/semantic-admission-config/v4" {
		t.Fatalf("semantic admission descriptor = %+v", descriptor)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*policyelements.SemanticDeciderDescriptor)
	}{
		{name: "mutable revision", mutate: func(value *policyelements.SemanticDeciderDescriptor) { value.Revision = "latest" }},
		{name: "uppercase digest", mutate: func(value *policyelements.SemanticDeciderDescriptor) {
			value.ConfigurationDigest = "sha256:" + strings.Repeat("A", 64)
		}},
		{name: "missing provider", mutate: func(value *policyelements.SemanticDeciderDescriptor) { value.Provider = "" }},
		{name: "zero timeout", mutate: func(value *policyelements.SemanticDeciderDescriptor) { value.DecisionTimeoutMS = 0 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := semanticTestDescriptor
			testCase.mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatalf("invalid semantic descriptor was accepted: %+v", value)
			}
		})
	}
	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		if registration.Profile.Reference != "policy.SemanticAdmission" {
			continue
		}
		validator := registration.Factory.(element.ConfigValidator)
		for _, source := range []string{
			`{"decider":"semantic-primary","max_pending":0}`,
			`{"decider":"semantic-primary","terminal_memory":1000001}`,
			`{"decider":"semantic-primary","cancel_memory":0}`,
			`{"decider":"semantic-primary","standing_memory":4097}`,
			`{"decider":"semantic-primary","rules":"` + strings.Repeat("x", 1<<20+1) + `"}`,
			`{"decider":"semantic-primary","unknown":true}`,
		} {
			if err := validator.ValidateConfig(json.RawMessage(source)); err == nil {
				t.Fatalf("invalid semantic config was accepted: %s", source)
			}
		}
		return
	}
	t.Fatal("semantic admission factory is absent from the production registry")
}

func TestSemanticAdmissionDirectVisualInputIsExplicitAndResolvedAtTheSealedPrefix(t *testing.T) {
	visualDescriptor := semanticTestDescriptor
	visualDescriptor.Vision = true
	imageBytes := []byte("exact retained image bytes")
	decider := &semanticTestDecider{
		descriptor: visualDescriptor, answers: []string{"speak"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", DirectVisualInput: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := continuation.MediaResolver(func(handle string) (continuation.Media, error) {
		if handle != "retained-image-1" {
			return continuation.Media{}, errors.New("unexpected media handle")
		}
		return continuation.Media{MIMEType: "image/png", Bytes: imageBytes}, nil
	})
	mounted, err := mountSemanticAdmissionRegisteredWithMedia(
		t, visualDescriptor, decider, config, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	item := trajectory.Item{
		ID: "visual-observation", Kind: trajectory.KindObservation, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "submitted still image",
		Observation: &trajectory.ObservationMeta{
			Observer: "client", Source: "message", Authority: trajectory.AuthorityUser,
			Media: []trajectory.MediaRef{{
				Handle: "retained-image-1", MIMEType: "image/png", Source: "message",
				Width: 64, Height: 48, Bytes: len(imageBytes),
			}},
		},
	}
	snapshot := trajectory.Snapshot{Version: 2, Items: []trajectory.Item{item, {
		ID: "tool-result-after-visual", Kind: trajectory.KindToolResult, MonotonicNS: 2,
		InvocationID: "generation-visual", Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
		ToolResult: &trajectory.ToolResult{
			CallID: "visual-tool-1", Name: "inspect", Output: json.RawMessage(`{"ok":true}`),
		},
	}}}
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-2", snapshot)
	version := snapshot.Version
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "visual-create",
		SessionID: "semantic-session", Payload: policyelements.ResponseCreate{
			ResponseID: "visual-response", ExpectedContextVersion: &version,
			ExpectedContextItemID: "state-2", CommittedContext: &stateelements.CommittedContext{
				Prefix: identity, StateItemID: "state-2",
			},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	_ = receivePolicy(t, harness.egress(t, "decision"))
	_ = receivePolicy(t, harness.egress(t, "voice_create"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	captured := decider.captured()
	if len(captured) != 1 || len(captured[0].Images) != 1 ||
		captured[0].Images[0].MIMEType != "image/png" ||
		!reflect.DeepEqual(captured[0].Images[0].Bytes, imageBytes) {
		t.Fatalf("direct visual semantic decision = %+v", captured)
	}
	imageBytes[0] ^= 0xff
	if captured[0].Images[0].Bytes[0] == imageBytes[0] {
		t.Fatal("semantic decision retained mutable media resolver bytes")
	}
}

func TestSemanticAdmissionPinsStandingPolicyBeforeTheNextDecisionAndSuppressesItsSetup(t *testing.T) {
	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	decider := &semanticTestDecider{
		descriptor: descriptor,
		// Setup and its refinement are listen; the animal is speak. The policy
		// makes those calls itself now, with the standing instruction in view.
		answers: []string{"listen", "listen", "speak"},
		generationAnswers: []string{
			"pin conversation count the animals out loud as they mention them and say nothing else",
			"yes", "yes", "yes", "standing", "none",
			"pin conversation count the animals out loud as they mention them and say nothing else",
			"yes",
		},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true, RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	setup := trajectory.Item{
		ID: "observation-setup", Kind: trajectory.KindObservation, MonotonicNS: 1,
		SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: "I will tell you about my afternoon. Count the animals out loud as I mention them, and say nothing else.",
		Observation: &trajectory.ObservationMeta{
			Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		},
		Event: &trajectory.EventMetadata{EventID: "event-setup", Type: "asr.endpoint", Source: "asr", Channel: "microphone"},
	}
	first := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{setup}}
	firstPrefix, err := trajectory.IdentifyPrefix(first, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-1", first)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-setup", SessionID: "semantic-session",
		Payload: stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationCommitted, TriggerItemID: setup.Event.EventID,
			TrajectoryItemID: setup.ID, StreamID: "speech-setup", ObservationRevision: 1,
			SourceRevision: 1, StoreVersion: 1,
			Context: stateelements.CommittedContext{Prefix: firstPrefix, StateItemID: "state-1"},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if !decision.Choice.Idle() || decision.DecisionStage != "policy" || decision.StandingBefore != 0 ||
		decision.StandingAfter != 1 || decision.StandingPinned != 1 ||
		outcome.Kind != policyelements.SemanticAdmissionSuppressed || state.StandingPolicies != 1 {
		t.Fatalf("setup decision=%+v outcome=%+v state=%+v", decision, outcome, state)
	}
	assertNoSemanticGeneration(t, harness)

	refinement := trajectory.Item{
		ID: "observation-refinement", Kind: trajectory.KindObservation, MonotonicNS: 2,
		SourceRevision: 2, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: "And say the count out loud.",
		Observation: &trajectory.ObservationMeta{
			Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		},
		Event: &trajectory.EventMetadata{EventID: "event-refinement", Type: "asr.endpoint", Source: "asr", Channel: "microphone"},
	}
	second := trajectory.Snapshot{Version: 2, Items: []trajectory.Item{setup, refinement}}
	secondPrefix, err := trajectory.IdentifyPrefix(second, second.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-2", second)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-refinement", SessionID: "semantic-session",
		Payload: stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationCommitted, TriggerItemID: refinement.Event.EventID,
			TrajectoryItemID: refinement.ID, StreamID: "speech-refinement", ObservationRevision: 1,
			SourceRevision: 2, StoreVersion: 2,
			Context: stateelements.CommittedContext{Prefix: secondPrefix, StateItemID: "state-2"},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision = receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	outcome = receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state = receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if !decision.Choice.Idle() || decision.DecisionStage != "policy" ||
		decision.StandingBefore != 1 || decision.StandingAfter != 1 ||
		outcome.Kind != policyelements.SemanticAdmissionSuppressed || state.StandingPolicies != 1 {
		t.Fatalf("refinement decision=%+v outcome=%+v state=%+v", decision, outcome, state)
	}
	assertNoSemanticGeneration(t, harness)

	animal := trajectory.Item{
		ID: "observation-animal", Kind: trajectory.KindObservation, MonotonicNS: 3,
		SourceRevision: 3, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: "A capybara wandered over and sat down next to me.",
		Observation: &trajectory.ObservationMeta{
			Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		},
		Event: &trajectory.EventMetadata{EventID: "event-animal", Type: "asr.endpoint", Source: "asr", Channel: "microphone"},
	}
	third := trajectory.Snapshot{Version: 3, Items: []trajectory.Item{setup, refinement, animal}}
	thirdPrefix, err := trajectory.IdentifyPrefix(third, third.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-3", third)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-animal", SessionID: "semantic-session",
		Payload: stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationCommitted, TriggerItemID: animal.Event.EventID,
			TrajectoryItemID: animal.ID, StreamID: "speech-animal", ObservationRevision: 1,
			SourceRevision: 3, StoreVersion: 3,
			Context: stateelements.CommittedContext{Prefix: thirdPrefix, StateItemID: "state-3"},
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision = receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	outcome = receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state = receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if !decision.Choice.Speak || decision.DecisionStage != "policy" ||
		decision.StandingBefore != 1 || decision.StandingAfter != 1 ||
		outcome.Kind != policyelements.SemanticAdmissionAdmitted || state.StandingPolicies != 1 {
		t.Fatalf("animal decision=%+v outcome=%+v state=%+v", decision, outcome, state)
	}
	captured := decider.captured()
	if len(captured) != 3 || !strings.Contains(captured[1].Evidence, "Standing instructions:") ||
		!strings.Contains(captured[1].Evidence, "count the animals") ||
		!strings.Contains(captured[2].Evidence, "Standing instructions:") ||
		!strings.Contains(captured[2].Evidence, "count the animals") ||
		!reflect.DeepEqual(captured[2].Options, coreinteraction.ChoiceOptions(false)) {
		t.Fatalf("semantic decision inputs = %+v", captured)
	}
}

func TestSemanticAdmissionNeverStoresAnUngroundedStandingPolicy(t *testing.T) {
	tests := []struct {
		name       string
		utterance  string
		extraction string
	}{
		{
			name: "current rain observation", utterance: "Oh, it's starting to rain outside.",
			extraction: "pin conversation say something if it starts raining",
		},
		{
			name: "floor-taking question", utterance: "Hold on, what time is the meeting scheduled today?",
			extraction: "pin turn do not reply until they have asked what time the meeting is scheduled today",
		},
		{
			name: "topic change", utterance: "Hold that thought. Can we discuss cooking tips instead.",
			extraction: "pin turn do not reply until they have finished their thought and ask about cooking tips",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := semanticTestDescriptor
			descriptor.StandingExtraction = true
			decider := &semanticTestDecider{
				descriptor: descriptor,
				answers:    []string{coreinteraction.ChoiceSpeak},
				generationAnswers: []string{
					testCase.extraction, "no",
				},
			}
			config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
				Decider: "semantic-primary", StandingExtraction: true,
				RecentLines: 12, MaxPending: 8, TerminalMemory: 8,
				CancelMemory: 8, StandingMemory: 8,
			})
			if err != nil {
				t.Fatal(err)
			}
			mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- mounted.Run(ctx) }()
			harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
			defer harness.stop(t)
			consumeSemanticStartup(t, harness)
			installSemanticInvocation(t, harness, 1, false)

			item := semanticEndpointObservation("false-policy", 1, 1, testCase.utterance)
			snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{item}}
			prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
			if err != nil {
				t.Fatal(err)
			}
			sendSemanticContext(t, harness, "state-1", snapshot)
			sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
				Type:   stateelements.ObservationCommitOutcomeType(),
				ItemID: "commit-false-policy", SessionID: "semantic-session",
				Payload: semanticCommittedOutcome(item, "false-policy", prefix, "state-1", 1),
			})
			_ = receivePolicy(t, harness.egress(t, "state"))
			decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			_ = receivePolicy(t, harness.egress(t, "voice_committed"))
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if !decision.Choice.Speak || decision.DecisionStage != "policy" ||
				decision.StandingBefore != 0 || decision.StandingAfter != 0 || decision.StandingPinned != 0 ||
				outcome.Kind != policyelements.SemanticAdmissionAdmitted || state.StandingPolicies != 0 {
				t.Fatalf("ungrounded policy affected admission: decision=%+v outcome=%+v state=%+v",
					decision, outcome, state)
			}
			decider.mu.Lock()
			generations := append([]string(nil), decider.generations...)
			decider.mu.Unlock()
			if len(generations) != 2 ||
				!strings.Contains(generations[1], coreinteraction.StandingPolicyGroundingInstruction) ||
				strings.Contains(generations[1], coreinteraction.CountingInstruction) {
				t.Fatalf("ungrounded pin crossed the wrong model passes: %+v", generations)
			}
		})
	}
}

func TestSemanticAdmissionGroundsAStandingPolicyAcrossSplitEndpoints(t *testing.T) {
	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	decider := &semanticTestDecider{
		descriptor: descriptor,
		answers:    []string{"listen"},
		generationAnswers: []string{
			"pin conversation after 15s ask whether they are still there", "yes", "no", "no",
		},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true,
		RecentLines: 12, MaxPending: 8, TerminalMemory: 8,
		CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	first := semanticEndpointObservation(
		"quiet-prefix", 1, 1, "If I have not said anything for about fifteen seconds,",
	)
	last := semanticEndpointObservation(
		"quiet-tail", 2, 2, "Ask whether I am still there.",
	)
	snapshot := trajectory.Snapshot{Version: 2, Items: []trajectory.Item{first, last}}
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-2", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-quiet-tail",
		SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(
			last, "quiet", prefix, "state-2", snapshot.Version,
		),
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if !decision.Choice.Idle() || decision.DecisionStage != "policy" ||
		decision.StandingAfter != 1 || decision.StandingPinned != 1 ||
		outcome.Kind != policyelements.SemanticAdmissionSuppressed || state.StandingPolicies != 1 {
		t.Fatalf("split standing decision=%+v outcome=%+v state=%+v", decision, outcome, state)
	}
	decider.mu.Lock()
	generations := append([]string(nil), decider.generations...)
	decider.mu.Unlock()
	want := "If I have not said anything for about fifteen seconds, Ask whether I am still there."
	if len(generations) != 4 || !strings.Contains(generations[0], want) ||
		!strings.Contains(generations[1], want) {
		t.Fatalf("split standing extraction inputs = %+v", generations)
	}
	if strings.Count(generations[0], first.Content) != 1 {
		t.Fatalf("the extractor received the earlier clause twice: %s", generations[0])
	}
}

func TestSupersededStandingExtractionCannotLeakIntoNewerDecision(t *testing.T) {
	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	entered := make(chan int, 2)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: descriptor, entered: entered, release: release,
		answers: []string{"listen", "speak"},
		generationAnswers: []string{
			"pin conversation count the animals as they arrive", "yes", "yes", "no", "standing", "none",
		},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true,
		RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	setup := semanticEndpointObservation("shared-setup", 1, 1, "Count the animals as they arrive.")
	first := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{setup}}
	firstPrefix, err := trajectory.IdentifyPrefix(first, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-1", first)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-shared-1", SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(setup, "shared", firstPrefix, "state-1", 1),
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	if call := awaitSemanticCall(t, entered); call != 0 {
		t.Fatalf("coverage call = %d, want 0", call)
	}

	newerItem := semanticEndpointObservation("shared-newer", 2, 2, "What time is it?")
	second := trajectory.Snapshot{Version: 2, Items: []trajectory.Item{setup, newerItem}}
	secondPrefix, err := trajectory.IdentifyPrefix(second, second.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-2", second)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-shared-2", SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(newerItem, "shared", secondPrefix, "state-2", 2),
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	if canceled.Kind != policyelements.SemanticAdmissionCanceled || canceled.Code != "decision_superseded" {
		t.Fatalf("superseded setup outcome = %+v", canceled)
	}
	if call := awaitSemanticCall(t, entered); call != 1 {
		t.Fatalf("newer primary call = %d, want 1", call)
	}
	close(release)
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if decision.StandingBefore != 0 || decision.StandingAfter != 0 || state.StandingPolicies != 0 {
		t.Fatalf("superseded extraction leaked: decision=%+v state=%+v", decision, state)
	}
}

func TestSemanticAdmissionDirectVisualInputNeverReadsImagesPastTheBoundPrefix(t *testing.T) {
	visualDescriptor := semanticTestDescriptor
	visualDescriptor.Vision = true
	decider := &semanticTestDecider{
		descriptor: visualDescriptor, answers: []string{"speak"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", DirectVisualInput: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	var resolverMu sync.Mutex
	var resolved []string
	resolver := continuation.MediaResolver(func(handle string) (continuation.Media, error) {
		resolverMu.Lock()
		resolved = append(resolved, handle)
		resolverMu.Unlock()
		switch handle {
		case "sealed-image":
			return continuation.Media{MIMEType: "image/png", Bytes: []byte("sealed")}, nil
		case "future-image":
			return continuation.Media{MIMEType: "image/png", Bytes: []byte("future")}, nil
		default:
			return continuation.Media{}, errors.New("unexpected media handle")
		}
	})
	mounted, err := mountSemanticAdmissionRegisteredWithMedia(
		t, visualDescriptor, decider, config, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	sealed, commit := semanticVisualObservation(t, "inspect the sealed frame", "screen", 1, "sealed-image")
	futureItem := trajectory.Item{
		ID: "future-visual-observation", Kind: trajectory.KindObservation, SourceRevision: 2,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "future frame",
		Observation: &trajectory.ObservationMeta{
			Observer: "client", Source: "screen", Authority: trajectory.AuthorityUser,
			Media: []trajectory.MediaRef{{
				Handle: "future-image", MIMEType: "image/png", Source: "screen", Width: 64, Height: 48, Bytes: 6,
			}},
		},
	}
	later := trajectory.Snapshot{
		Version: 2, Items: append(append([]trajectory.Item(nil), sealed.Items...), futureItem),
	}
	sendSemanticContext(t, harness, "state-2", later)
	version := uint64(1)
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "create-bound-before-future-image",
		SessionID: "semantic-session", Payload: policyelements.ResponseCreate{
			ResponseID: "response-bound-before-future-image", ExpectedContextVersion: &version,
			ExpectedContextItemID: "state-1", CommittedContext: &commit.Context,
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	_ = receivePolicy(t, harness.egress(t, "decision"))
	_ = receivePolicy(t, harness.egress(t, "voice_create"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	captured := decider.captured()
	resolverMu.Lock()
	resolvedCopy := append([]string(nil), resolved...)
	resolverMu.Unlock()
	if len(captured) != 1 || len(captured[0].Images) != 1 ||
		string(captured[0].Images[0].Bytes) != "sealed" ||
		!reflect.DeepEqual(resolvedCopy, []string{"sealed-image"}) {
		t.Fatalf("bound visual decision=%+v resolved=%v", captured, resolvedCopy)
	}
}

func TestSemanticAdmissionDirectVisualInputDoesNotTouchMediaOnAnAudioTurn(t *testing.T) {
	visualDescriptor := semanticTestDescriptor
	visualDescriptor.Vision = true
	decider := &semanticTestDecider{
		descriptor: visualDescriptor, answers: []string{"speak"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", DirectVisualInput: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	resolver := continuation.MediaResolver(func(string) (continuation.Media, error) {
		calls.Add(1)
		return continuation.Media{}, errors.New("audio turn reached media resolver")
	})
	mounted, err := mountSemanticAdmissionRegisteredWithMedia(
		t, visualDescriptor, decider, config, resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, commit := semanticObservation(t, "answer this audio turn", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-audio-under-visual-policy",
		SessionID: "semantic-session", Payload: commit,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	_ = receivePolicy(t, harness.egress(t, "decision"))
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if calls.Load() != 0 {
		t.Fatalf("audio turn resolved %d media attachment(s)", calls.Load())
	}
	if captured := decider.captured(); len(captured) != 1 || len(captured[0].Images) != 0 {
		t.Fatalf("audio turn semantic decision = %+v", captured)
	}
}

func TestSemanticAdmissionDirectVisualInputRequiresBothDeclaredCapabilityAndResolver(t *testing.T) {
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", DirectVisualInput: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonvisual := &semanticTestDecider{descriptor: semanticTestDescriptor}
	if _, err := mountSemanticAdmissionRegisteredWithMedia(
		t, semanticTestDescriptor, nonvisual, config,
		continuation.MediaResolver(func(string) (continuation.Media, error) { return continuation.Media{}, nil }),
	); err == nil || !strings.Contains(err.Error(), "vision-capable") {
		t.Fatalf("non-visual semantic decider mount error = %v", err)
	}
	visualDescriptor := semanticTestDescriptor
	visualDescriptor.Vision = true
	visual := &semanticTestDecider{descriptor: visualDescriptor}
	if _, err := mountSemanticAdmissionRegisteredWithMedia(
		t, visualDescriptor, visual, config, nil,
	); err == nil || !strings.Contains(err.Error(), "media resolver") {
		t.Fatalf("missing semantic media resolver mount error = %v", err)
	}
}

func TestSemanticAdmissionStandingExtractionRequiresDeclaredProviderCapability(t *testing.T) {
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	decider := &semanticTestDecider{descriptor: semanticTestDescriptor}
	if _, err := mountSemanticAdmissionRegistered(t, semanticTestDescriptor, decider, config); err == nil ||
		!strings.Contains(err.Error(), "provider-declared extraction capability") {
		t.Fatalf("undeclared standing extraction mount error = %v", err)
	}

	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	dishonest := semanticDecisionOnlyDecider{descriptor: descriptor}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, dishonest, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := mounted.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "does not implement interaction.Generator") {
		t.Fatalf("dishonest standing extraction provider run error = %v", err)
	}
}

func TestSemanticAdmissionDirectVisualResolutionObeysDeadlineAndCancellation(t *testing.T) {
	directConfig, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", DirectVisualInput: true, RecentLines: 12,
		MaxPending: 8, TerminalMemory: 8, CancelMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("decision deadline", func(t *testing.T) {
		descriptor := semanticTestDescriptor
		descriptor.Vision = true
		descriptor.DecisionTimeoutMS = 10
		decider := &semanticTestDecider{descriptor: descriptor}
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		resolver := continuation.MediaResolver(func(string) (continuation.Media, error) {
			started <- struct{}{}
			<-release
			return continuation.Media{MIMEType: "image/png", Bytes: []byte("late")}, nil
		})
		mounted, err := mountSemanticAdmissionRegisteredWithMedia(
			t, descriptor, decider, directConfig, resolver,
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mounted.Run(ctx) }()
		harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
		defer harness.stop(t)
		consumeSemanticStartup(t, harness)
		installSemanticInvocation(t, harness, 1, false)
		snapshot, commit := semanticVisualObservation(t, "inspect it", "screen", 1, "blocked-image")
		sendSemanticContext(t, harness, "state-1", snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-media-timeout",
			SessionID: "semantic-session", Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("semantic media resolver was not called")
		}
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if outcome.Kind != policyelements.SemanticAdmissionFailed || outcome.Code != "decision_timeout" ||
			state.Failed != 1 || state.Active {
			t.Fatalf("media timeout outcome=%+v state=%+v", outcome, state)
		}
		if captured := decider.captured(); len(captured) != 0 {
			t.Fatalf("timed-out media resolution reached decider: %+v", captured)
		}
		unblock()
	})

	t.Run("stream cancellation", func(t *testing.T) {
		descriptor := semanticTestDescriptor
		descriptor.Vision = true
		decider := &semanticTestDecider{descriptor: descriptor}
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		resolver := continuation.MediaResolver(func(string) (continuation.Media, error) {
			started <- struct{}{}
			<-release
			return continuation.Media{MIMEType: "image/png", Bytes: []byte("canceled")}, nil
		})
		mounted, err := mountSemanticAdmissionRegisteredWithMedia(
			t, descriptor, decider, directConfig, resolver,
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mounted.Run(ctx) }()
		harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
		defer harness.stop(t)
		consumeSemanticStartup(t, harness)
		installSemanticInvocation(t, harness, 1, false)
		snapshot, commit := semanticVisualObservation(t, "inspect it", "screen", 1, "canceled-image")
		sendSemanticContext(t, harness, "state-1", snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-media-cancel",
			SessionID: "semantic-session", Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("semantic media resolver was not called")
		}
		sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
			Type: policyelements.GenerationCancelType(), ItemID: "cancel-media",
			SessionID: "semantic-session", Payload: policyelements.GenerationCancel{
				StreamID: "screen", Reason: "newer visual evidence arrived",
			},
		})
		recorded := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		_ = receivePolicy(t, harness.egress(t, "state"))
		canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if recorded.Operation != "cancel" || recorded.Code != "cancel_recorded" ||
			canceled.Kind != policyelements.SemanticAdmissionCanceled || canceled.Code != "decision_canceled" ||
			state.Canceled != 1 || state.Active {
			t.Fatalf("media cancel receipt=%+v outcome=%+v state=%+v", recorded, canceled, state)
		}
		if captured := decider.captured(); len(captured) != 0 {
			t.Fatalf("canceled media resolution reached decider: %+v", captured)
		}
		unblock()
	})

	t.Run("resolved media type drift", func(t *testing.T) {
		descriptor := semanticTestDescriptor
		descriptor.Vision = true
		decider := &semanticTestDecider{descriptor: descriptor}
		resolver := continuation.MediaResolver(func(string) (continuation.Media, error) {
			return continuation.Media{MIMEType: "text/plain", Bytes: []byte("not an image")}, nil
		})
		mounted, err := mountSemanticAdmissionRegisteredWithMedia(
			t, descriptor, decider, directConfig, resolver,
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mounted.Run(ctx) }()
		harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
		defer harness.stop(t)
		consumeSemanticStartup(t, harness)
		installSemanticInvocation(t, harness, 1, false)
		snapshot, commit := semanticVisualObservation(t, "inspect it", "screen", 1, "drifted-image")
		sendSemanticContext(t, harness, "state-1", snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-media-drift",
			SessionID: "semantic-session", Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if outcome.Kind != policyelements.SemanticAdmissionFailed || outcome.Code != "visual_evidence_failed" ||
			!strings.Contains(outcome.Message, "text/plain") || state.Failed != 1 || state.Active {
			t.Fatalf("media type drift outcome=%+v state=%+v", outcome, state)
		}
		if captured := decider.captured(); len(captured) != 0 {
			t.Fatalf("drifted visual evidence reached decider: %+v", captured)
		}
	})
}

type semanticTestDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor

	mu                 sync.Mutex
	answers            []string
	confidences        []float64
	generationAnswers  []string
	failure            error
	decisions          []coreinteraction.Decision
	generations        []string
	entered            chan int
	exited             chan int
	release            chan struct{}
	ignoreCancellation bool
	closed             atomic.Int32
	active             atomic.Int32
	closedWhileActive  atomic.Bool
	// step is the index of the current step's record; lastQuestion tells a
	// new step from the next question of the same one.
	step         int
	lastQuestion string
}

type semanticDecisionOnlyDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor
}

func (decider semanticDecisionOnlyDecider) Name() string { return "semantic-decision-only-test" }
func (decider semanticDecisionOnlyDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}
func (semanticDecisionOnlyDecider) Decide(
	context.Context, coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	return coreinteraction.Outcome{}, errors.New("unexpected decision")
}

func (decider *semanticTestDecider) Name() string { return "semantic-test" }

func (decider *semanticTestDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *semanticTestDecider) Decide(
	ctx context.Context, decision coreinteraction.Decision,
) (outcome coreinteraction.Outcome, err error) {
	decider.active.Add(1)
	defer decider.active.Add(-1)
	decider.mu.Lock()
	// The runner asks up to three yes/no questions about one step; the
	// fixture scripts the Choice it wants composed and answers each question
	// the way that choice would. A step begins with the stop question when
	// the agent is speaking and with the occurrence question otherwise; the
	// request question belongs to the step before it. A decision with other
	// options is not a step question and is answered as scripted.
	stepQuestion := decision.Question != ""
	newStep := !stepQuestion || decision.Question == coreinteraction.QuestionStop ||
		(decision.Question == coreinteraction.QuestionOccurrence && decider.lastQuestion != coreinteraction.QuestionStop)
	decider.lastQuestion = decision.Question
	if newStep {
		decider.step = len(decider.decisions)
		recorded := cloneSemanticTestDecision(decision)
		if stepQuestion {
			recorded.Options = coreinteraction.ChoiceOptions(decision.Speaking)
		}
		decider.decisions = append(decider.decisions, recorded)
	}
	call := decider.step
	// Unscripted, the fake invokes the voice: speak while the floor is free,
	// keep+speak while the agent is talking.
	answer := coreinteraction.ChoiceSpeak
	if decision.Speaking || (!stepQuestion && !slices.Contains(decision.Options, answer)) {
		answer = coreinteraction.ChoiceKeepSpeak
	}
	if len(decider.answers) != 0 {
		answer = decider.answers[min(call, len(decider.answers)-1)]
	}
	confidence := 0.9
	if len(decider.confidences) != 0 {
		confidence = decider.confidences[min(call, len(decider.confidences)-1)]
	}
	failure := decider.failure
	entered, exited, release := decider.entered, decider.exited, decider.release
	ignoreCancellation := decider.ignoreCancellation
	decider.mu.Unlock()
	if exited != nil {
		defer func() {
			select {
			case exited <- call:
			default:
			}
		}()
	}
	if newStep && entered != nil {
		select {
		case entered <- call:
		case <-ctx.Done():
			return coreinteraction.Outcome{}, ctx.Err()
		}
	}
	if newStep && release != nil {
		if ignoreCancellation {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return coreinteraction.Outcome{}, ctx.Err()
			}
		}
	}
	if failure != nil {
		return coreinteraction.Outcome{}, failure
	}
	if stepQuestion {
		choice, parseErr := coreinteraction.ParseChoice(answer, decision.Speaking)
		if parseErr != nil {
			return coreinteraction.Outcome{}, fmt.Errorf("scripted answer %q for a %s question: %w", answer, decision.Question, parseErr)
		}
		reply := coreinteraction.AnswerFor(decision.Question, choice)
		return coreinteraction.Outcome{
			Option: reply, Index: slices.Index(coreinteraction.YesNo(), reply), Confidence: confidence, Measured: true,
		}, nil
	}
	index := slices.Index(decision.Options, answer)
	return coreinteraction.Outcome{Option: answer, Index: index, Confidence: confidence, Measured: true}, nil
}

func (decider *semanticTestDecider) Generate(
	ctx context.Context, prompt, evidence string, _ int,
) (string, error) {
	if prompt == coreinteraction.AddressedElsewhereInstruction {
		// Nobody in these fixtures talks to a third party; the question is
		// answered by name so the scripted extraction answers keep their order.
		return "no", nil
	}
	decider.mu.Lock()
	call := len(decider.generations)
	decider.generations = append(decider.generations, prompt+"\n"+evidence)
	answer := "none"
	if len(decider.generationAnswers) != 0 {
		answer = decider.generationAnswers[min(call, len(decider.generationAnswers)-1)]
	}
	failure := decider.failure
	decider.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	if failure != nil {
		return "", failure
	}
	return answer, nil
}

func (decider *semanticTestDecider) Close() error {
	if decider.active.Load() > 0 {
		decider.closedWhileActive.Store(true)
	}
	decider.closed.Add(1)
	return nil
}

func (decider *semanticTestDecider) captured() []coreinteraction.Decision {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	result := make([]coreinteraction.Decision, len(decider.decisions))
	for index := range decider.decisions {
		result[index] = cloneSemanticTestDecision(decider.decisions[index])
	}
	return result
}

func cloneSemanticTestDecision(value coreinteraction.Decision) coreinteraction.Decision {
	value.Options = append([]string(nil), value.Options...)
	value.Images = append([]coreinteraction.Image(nil), value.Images...)
	for index := range value.Images {
		value.Images[index].Bytes = append([]byte(nil), value.Images[index].Bytes...)
	}
	return value
}

func semanticOptionIndex(options []string, token string) int {
	return slices.Index(options, token)
}

func TestSemanticAdmissionRoutesOnlyTheEnumeratedBranch(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		token     string
		tools     bool
		branch    string
		wantKind  policyelements.SemanticAdmissionOutcomeKind
		wantVoice uint64
	}{
		{name: "listen suppresses generation", token: coreinteraction.ChoiceListen,
			wantKind: policyelements.SemanticAdmissionSuppressed},
		{name: "speak admits the voice", token: coreinteraction.ChoiceSpeak, branch: "voice_committed",
			wantKind: policyelements.SemanticAdmissionAdmitted, wantVoice: 1},
		// Tools change nothing about routing: a tool call is the voice model's
		// decision, made in content, and there is no second lane for it.
		{name: "speak with tools still admits only the voice", token: coreinteraction.ChoiceSpeak,
			tools: true, branch: "voice_committed", wantKind: policyelements.SemanticAdmissionAdmitted,
			wantVoice: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			entered := make(chan int, 1)
			exited := make(chan int, 1)
			decider := &semanticTestDecider{
				descriptor: semanticTestDescriptor, answers: []string{testCase.token},
				entered: entered, exited: exited,
			}
			harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer harness.stop(t)
			consumeSemanticStartup(t, harness)
			installSemanticInvocation(t, harness, 1, testCase.tools)
			snapshot, commit := semanticObservation(t, "please help", "speech", 1)
			sendSemanticContext(t, harness, "state-1", snapshot)
			envelope := element.Envelope{
				Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-1",
				SessionID: "semantic-session", Payload: commit,
			}
			sendPolicy(t, harness.ingress(t, "committed"), envelope)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("semantic decider was not called")
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("semantic decider did not return")
			}
			active := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if !active.Active || active.Pending != 0 {
				t.Fatalf("active semantic state = %+v", active)
			}
			select {
			case err := <-harness.done:
				t.Fatalf("semantic admission stopped before its decision was observable: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			decision := receivePolicy(t, harness.egress(t, "decision"))
			payload, ok := decision.Payload.(policyelements.SemanticDecision)
			if !ok || payload.Choice.Token() != testCase.token || payload.EvidenceItemID != "commit-1" ||
				payload.StreamID != "speech" || payload.SourceRevision != 1 ||
				payload.ContextVersion != 1 || !strings.HasPrefix(payload.InvocationDigest, "sha256:") ||
				payload.Provider != semanticTestDescriptor.Provider || payload.Model != semanticTestDescriptor.Model {
				t.Fatalf("semantic decision = %+v", decision)
			}
			if !containsPolicy(decision.CausalParents, "commit-1") ||
				!containsPolicy(decision.CausalParents, "state-1") {
				t.Fatalf("semantic decision causal parents = %v", decision.CausalParents)
			}
			if testCase.branch != "" {
				branch := receivePolicy(t, harness.egress(t, testCase.branch))
				grant, ok := branch.Payload.(policyelements.SemanticGrant)
				if !ok || !reflect.DeepEqual(grant.Commit, commit) || grant.Choice.Token() != testCase.token ||
					grant.DecisionItemID != decision.ItemID ||
					!containsPolicy(branch.CausalParents, decision.ItemID) {
					t.Fatalf("semantic branch = %+v", branch)
				}
			}
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if outcome.Kind != testCase.wantKind || outcome.Choice == nil || outcome.Choice.Token() != testCase.token ||
				state.Active || state.Pending != 0 || state.AdmittedVoice != testCase.wantVoice {
				t.Fatalf("semantic outcome=%+v state=%+v", outcome, state)
			}
			for _, branch := range []string{"voice_committed", "voice_create"} {
				if branch != testCase.branch {
					assertNoPolicyEnvelope(t, harness.egress(t, branch))
				}
			}
			captured := decider.captured()
			if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "please help") ||
				semanticOptionIndex(captured[0].Options, testCase.token) < 0 {
				t.Fatalf("policy request = %+v", captured)
			}
		})
	}
}

func TestSemanticAdmissionWaitsForExactInputsAndSealsTheCommittedPrefix(t *testing.T) {
	decider := &semanticTestDecider{descriptor: semanticTestDescriptor, answers: []string{"speak"}}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	first, commit := semanticObservation(t, "first utterance", "speech", 1)
	commit.Context.StateItemID = "state-1"
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-before-inputs",
		SessionID: "semantic-session", Payload: commit,
	})
	assertNoPolicyEnvelope(t, harness.egress(t, "decision"))
	pending := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if pending.Pending != 1 || pending.Active {
		t.Fatalf("pending semantic state = %+v", pending)
	}
	installSemanticInvocation(t, harness, 1, false)
	assertNoPolicyEnvelope(t, harness.egress(t, "decision"))

	later := trajectory.Snapshot{Version: 2, Items: append(
		append([]trajectory.Item(nil), first.Items...),
		trajectory.Item{ID: "future-item", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "future secret"},
	)}
	sendSemanticContext(t, harness, "state-2", later)
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if decision.ContextVersion != 1 {
		t.Fatalf("decision context version = %d", decision.ContextVersion)
	}
	captured := decider.captured()
	if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "first utterance") ||
		strings.Contains(captured[0].Evidence, "future secret") {
		t.Fatalf("decision did not use the exact sealed prefix: %+v", captured)
	}
}

func TestSemanticAdmissionReconstructsBoundCreateFromLaterAppendOnlyState(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"},
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	first, _ := semanticObservation(t, "answer the exact first request", "speech", 1)
	identity, err := trajectory.IdentifyPrefix(first, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	later := trajectory.Snapshot{Version: 2, Items: append(
		append([]trajectory.Item(nil), first.Items...),
		trajectory.Item{ID: "future-observation", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "future secret"},
	)}
	sendSemanticContext(t, harness, "state-2", later)
	version := uint64(1)
	create := policyelements.ResponseCreate{
		ResponseID: "response-bound-to-state-1", ExpectedContextVersion: &version,
		ExpectedContextItemID: "state-1",
		CommittedContext: &stateelements.CommittedContext{
			Prefix: identity, StateItemID: "state-1",
		},
	}
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "request-bound-to-state-1",
		SessionID: "semantic-session", Payload: create,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision"))
	payload := decision.Payload.(policyelements.SemanticDecision)
	branch := receivePolicy(t, harness.egress(t, "voice_create"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if payload.ContextVersion != 1 || !reflect.DeepEqual(branch.Payload, create) ||
		!containsPolicy(decision.CausalParents, "state-2") {
		t.Fatalf("later-State bound decision=%+v branch=%+v", decision, branch)
	}
	captured := decider.captured()
	if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "answer the exact first request") ||
		strings.Contains(captured[0].Evidence, "future secret") {
		t.Fatalf("later State did not reconstruct the exact sealed prefix: %+v", captured)
	}
}

func TestSemanticAdmissionRejectsTamperedBoundCreatePrefix(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"},
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, _ := semanticObservation(t, "authenticated prefix", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	version := uint64(1)
	create := policyelements.ResponseCreate{
		ResponseID: "response-tampered-prefix", ExpectedContextVersion: &version,
		ExpectedContextItemID: "state-1",
		CommittedContext: &stateelements.CommittedContext{
			Prefix: trajectory.PrefixIdentity{
				Version: 1, Digest: "sha256:" + strings.Repeat("0", 64),
			},
			StateItemID: "state-1",
		},
	}
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "request-tampered-prefix",
		SessionID: "semantic-session", Payload: create,
	})
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))
	if outcome.Kind != policyelements.SemanticAdmissionRefused || outcome.Code != "invalid_context" ||
		!strings.Contains(outcome.Message, "digest mismatch") {
		t.Fatalf("tampered bound-create outcome = %+v", outcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "decision"))
	assertNoPolicyEnvelope(t, harness.egress(t, "voice_create"))
	if captured := decider.captured(); len(captured) != 0 {
		t.Fatalf("tampered bound create reached decider: %+v", captured)
	}
}

func TestSemanticAdmissionRoutesManualAndQuietCreatesThroughTheSamePolicy(t *testing.T) {
	for _, operation := range []string{"create", "quiet"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan int, 1)
			decider := &semanticTestDecider{
				descriptor: semanticTestDescriptor, answers: []string{"speak"},
				entered: entered,
			}
			harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
			defer harness.stop(t)
			consumeSemanticStartup(t, harness)
			installSemanticInvocation(t, harness, 1, false)
			snapshot, _ := semanticObservation(t, "If I go quiet, check on me.", "speech", 1)
			sendSemanticContext(t, harness, "state-1", snapshot)
			version := uint64(1)
			create := policyelements.ResponseCreate{
				ResponseID: "response-" + operation, ExpectedContextVersion: &version,
				ExpectedContextItemID: "state-1",
			}
			sendPolicy(t, harness.ingress(t, operation), element.Envelope{
				Type: policyelements.ResponseCreateType(), ItemID: "request-" + operation,
				SessionID: "semantic-session", Payload: create,
			})
			if operation == "create" {
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("semantic create did not reach the decider")
				}
			}
			active := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if !active.Active {
				t.Fatalf("semantic create active state = %+v", active)
			}
			decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			captured := decider.captured()
			if operation == "quiet" {
				if decision.Operation != operation || !decision.Choice.Idle() ||
					outcome.Kind != policyelements.SemanticAdmissionSuppressed || state.AdmittedVoice != 0 ||
					state.Active || len(captured) != 0 {
					t.Fatalf("unowned quiet decision=%+v outcome=%+v state=%+v captured=%+v",
						decision, outcome, state, captured)
				}
				assertNoPolicyEnvelope(t, harness.egress(t, "voice_create"))
				return
			}
			branch := receivePolicy(t, harness.egress(t, "voice_create"))
			if decision.Operation != operation || !decision.Choice.Speak ||
				!reflect.DeepEqual(branch.Payload, create) || outcome.Kind != policyelements.SemanticAdmissionAdmitted ||
				state.AdmittedVoice != 1 || state.Active || len(captured) != 1 ||
				!strings.Contains(captured[0].Evidence, "If I go quiet") {
				t.Fatalf("%s decision=%+v branch=%+v outcome=%+v state=%+v captured=%+v",
					operation, decision, branch, outcome, state, captured)
			}
		})
	}
}

func TestSemanticAdmissionUnownedQuietTickDoesNotDisturbActiveOutput(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"stop"},
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, _ := semanticObservation(t, "Count slowly from one to ten.", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)

	output := coreinteraction.AgentOutput{
		Revision: 2, Active: true, Audible: true, Saying: "One. Two. Three.",
		InFlight: "voice output active: playback=1",
	}
	sendPolicy(t, harness.ingress(t, "agent_output"), element.Envelope{
		Type: coreinteraction.AgentOutputType(), ItemID: "agent-output-active",
		SessionID: "semantic-session", Payload: output,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	version := uint64(1)
	create := policyelements.ResponseCreate{
		ResponseID: "response-unowned-quiet", ExpectedContextVersion: &version,
		ExpectedContextItemID: "state-1",
	}
	sendPolicy(t, harness.ingress(t, "quiet"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "request-unowned-quiet",
		SessionID: "semantic-session", Payload: create,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if decision.Operation != "quiet" || !decision.Choice.Idle() || !decision.Choice.Speaking || decision.Measured ||
		outcome.Kind != policyelements.SemanticAdmissionSuppressed || outcome.Code != "keep" ||
		state.AdmittedVoice != 0 || state.Failed != 0 || state.Active || len(decider.captured()) != 0 {
		t.Fatalf("active-output quiet decision=%+v outcome=%+v state=%+v captured=%+v",
			decision, outcome, state, decider.captured())
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "voice_create"))
}

func TestSemanticAdmissionQuietTickActsOnlyForAnExactDueStandingPolicy(t *testing.T) {
	descriptor := semanticTestDescriptor
	descriptor.StandingExtraction = true
	decider := &semanticTestDecider{
		descriptor: descriptor,
		answers:    []string{"listen", "speak"},
		generationAnswers: []string{
			"pin conversation after 15s ask whether they are still there", "yes", "no", "no",
		},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", StandingExtraction: true,
		RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	setup := semanticEndpointObservation(
		"quiet-setup", 1, 1,
		"If I have not said anything for fifteen seconds, ask whether I am still there.",
	)
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{setup}}
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-1", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-quiet-setup", SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(setup, "quiet-setup", identity, "state-1", 1),
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	setupDecision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if !setupDecision.Choice.Idle() || setupDecision.DecisionStage != "policy" ||
		setupDecision.StandingAfter != 1 {
		t.Fatalf("quiet setup decision = %+v", setupDecision)
	}

	version := uint64(1)
	create := policyelements.ResponseCreate{
		ResponseID: "response-due-quiet", ExpectedContextVersion: &version,
		ExpectedContextItemID: "state-1",
	}
	sendPolicy(t, harness.ingress(t, "quiet"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "request-due-quiet",
		SessionID: "semantic-session", Payload: create,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	branch := receivePolicy(t, harness.egress(t, "voice_create"))
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	trustedCreate := create
	trustedCreate.TrustedPurpose = policyelements.ResponseCreatePurposePostCommitSilence
	if decision.Operation != "quiet" || !decision.Choice.Speak ||
		!reflect.DeepEqual(branch.Payload, trustedCreate) || outcome.Kind != policyelements.SemanticAdmissionAdmitted ||
		state.AdmittedVoice != 1 || state.StandingPolicies != 1 {
		t.Fatalf("due quiet decision=%+v branch=%+v outcome=%+v state=%+v",
			decision, branch, outcome, state)
	}
	captured := decider.captured()
	if len(captured) != 2 || !strings.Contains(captured[1].Evidence, "after 15s of quiet") ||
		!strings.Contains(captured[1].Evidence, "silence: 15s") {
		t.Fatalf("due quiet evidence = %+v", captured)
	}
}

func TestSemanticAdmissionRejectsTransportForgedTrustedPurpose(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"},
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, _ := semanticObservation(t, "ordinary context", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	version := uint64(1)
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "forged-purpose",
		SessionID: "semantic-session", Payload: policyelements.ResponseCreate{
			ResponseID: "forged-response", ExpectedContextVersion: &version,
			ExpectedContextItemID: "state-1",
			TrustedPurpose:        policyelements.ResponseCreatePurposePostCommitSilence,
		},
	})
	outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if outcome.Kind != policyelements.SemanticAdmissionRefused || outcome.Code != "invalid_create" ||
		!strings.Contains(outcome.Message, "cannot supply a trusted runtime purpose") || state.Refused != 1 {
		t.Fatalf("forged trusted-purpose outcome=%+v state=%+v", outcome, state)
	}
	assertNoSemanticGeneration(t, harness)
	if captured := decider.captured(); len(captured) != 0 {
		t.Fatalf("forged trusted purpose reached decider: %+v", captured)
	}
}

func TestSemanticAdmissionExplicitCreateConsultsPolicyAfterToolResult(t *testing.T) {
	entered := make(chan int, 1)
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"},
		entered: entered,
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, true)
	first, _ := semanticObservation(t, "look up the weather", "speech", 1)
	snapshot := trajectory.Snapshot{
		Version: 2,
		Items: append(append([]trajectory.Item(nil), first.Items...), trajectory.Item{
			ID: "tool-result-1", Kind: trajectory.KindToolResult, MonotonicNS: 2,
			InvocationID: "generation-1", Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
			ToolResult: &trajectory.ToolResult{
				CallID: "weather-1", Name: "lookup.weather", Output: json.RawMessage(`{"temperature_c":21}`),
			},
		}),
	}
	sendSemanticContext(t, harness, "state-2", snapshot)
	version := uint64(2)
	create := policyelements.ResponseCreate{
		ResponseID: "response-after-tool", ExpectedContextVersion: &version,
		ExpectedContextItemID: "state-2",
	}
	sendPolicy(t, harness.ingress(t, "create"), element.Envelope{
		Type: policyelements.ResponseCreateType(), ItemID: "request-after-tool",
		SessionID: "semantic-session", Payload: create,
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("explicit create after a tool result did not reach the semantic decider")
	}
	_ = receivePolicy(t, harness.egress(t, "state"))
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	branch := receivePolicy(t, harness.egress(t, "voice_create"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if decision.Operation != "create" || !decision.Choice.Speak ||
		!reflect.DeepEqual(branch.Payload, create) {
		t.Fatalf("tool-result continuation decision=%+v branch=%+v", decision, branch)
	}
	if captured := decider.captured(); len(captured) != 1 ||
		!strings.Contains(captured[0].Evidence, "look up the weather") {
		t.Fatalf("tool-result continuation policy request = %+v", captured)
	}
}

func TestSemanticAdmissionNewerEvidenceCancelsOnlyTheOlderDecision(t *testing.T) {
	entered := make(chan int, 2)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor,
		answers:    []string{"speak", "speak"},
		entered:    entered, release: release,
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	first, firstCommit := semanticObservation(t, "first partial", "speech", 1)
	sendSemanticContext(t, harness, "state-1", first)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-1",
		SessionID: "semantic-session", Payload: firstCommit,
	})
	if call := awaitSemanticCall(t, entered); call != 0 {
		t.Fatalf("first semantic call = %d", call)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))

	secondItem := trajectory.Item{
		ID: "observation-speech-2", Kind: trajectory.KindObservation, SourceRevision: 2,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "corrected final",
		Event: &trajectory.EventMetadata{EventID: "event-speech-2"},
	}
	second := trajectory.Snapshot{Version: 2, Items: append(append([]trajectory.Item(nil), first.Items...), secondItem)}
	identity, err := trajectory.IdentifyPrefix(second, second.Version)
	if err != nil {
		t.Fatal(err)
	}
	secondCommit := stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: secondItem.Event.EventID,
		TrajectoryItemID: secondItem.ID, StreamID: "speech", ObservationRevision: 2,
		SourceRevision: 2, StoreVersion: 2,
		Context: stateelements.CommittedContext{Prefix: identity, StateItemID: "state-2"},
	}
	sendSemanticContext(t, harness, "state-2", second)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-2",
		SessionID: "semantic-session", Payload: secondCommit,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	old := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	if old.Kind != policyelements.SemanticAdmissionCanceled || old.Code != "decision_superseded" ||
		old.SourceRevision != 1 {
		t.Fatalf("superseded semantic outcome = %+v", old)
	}
	if call := awaitSemanticCall(t, entered); call != 1 {
		t.Fatalf("replacement semantic call = %d", call)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))
	close(release)
	decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	branch := receivePolicy(t, harness.egress(t, "voice_committed"))
	final := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	grant, ok := branch.Payload.(policyelements.SemanticGrant)
	if decision.SourceRevision != 2 || !ok || !reflect.DeepEqual(grant.Commit, secondCommit) ||
		!grant.Choice.Speak || strings.TrimSpace(grant.DecisionItemID) == "" ||
		final.Kind != policyelements.SemanticAdmissionAdmitted || state.Canceled != 1 ||
		state.AdmittedVoice != 1 {
		t.Fatalf("replacement decision=%+v branch=%+v outcome=%+v state=%+v",
			decision, branch, final, state)
	}
}

func TestSemanticAdmissionUsesVoiceLifecycleAcrossTranscriptRevisions(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor,
		// A correction spoken on its final, then a later revision of the same
		// stream and an unrelated partial, both while that output is active:
		// keep, keep.
		answers: []string{"speak", "keep", "keep"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
		Rules: "Classify this transcript.",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := mountSemanticAdmission(t, decider, config)
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	items := []trajectory.Item{
		semanticTranscriptObservation(
			"correction-first", "asr.endpoint", 1,
			"And then ship it by the thirteenth.",
		),
		semanticTranscriptObservation(
			"correction-final", "asr.endpoint", 2,
			"And then ship it by the thirteenth.",
		),
		semanticTranscriptObservation(
			"continued-partial", "asr.revision", 1,
			"Which gives us plenty of time to finish the documentation.",
		),
	}
	streams := []string{"correction-stream", "correction-stream", "continued-stream"}

	appendAndCommit := func(index int) policyelements.SemanticDecision {
		snapshot := trajectory.Snapshot{
			Version: uint64(index + 1), Items: append([]trajectory.Item(nil), items[:index+1]...),
		}
		stateID := fmt.Sprintf("state-%d", index+1)
		sendSemanticContext(t, harness, stateID, snapshot)
		prefix, identifyErr := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if identifyErr != nil {
			t.Fatal(identifyErr)
		}
		commit := semanticCommittedOutcome(items[index], streams[index], prefix, stateID, snapshot.Version)
		commit.ObservationRevision = items[index].SourceRevision
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type:   stateelements.ObservationCommitOutcomeType(),
			ItemID: fmt.Sprintf("commit-%d", index+1), SessionID: "semantic-session",
			Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		return receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	}

	first := appendAndCommit(0)
	grant := receivePolicy(t, harness.egress(t, "voice_committed")).Payload.(policyelements.SemanticGrant)
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	if !first.Choice.Speak || first.Event != coreinteraction.TranscriptFinal || !grant.Choice.Speak {
		t.Fatalf("correction decision=%+v grant=%+v", first, grant)
	}

	output := coreinteraction.AgentOutput{
		Active: true, Queued: true,
		InFlight:         "voice output active: model=1, segmentation=1, synthesis=0, playback=0",
		ProtectedStreams: []string{"correction-stream"},
	}
	acknowledgeSemanticVoice(t, harness, output, 2, 1)

	final := appendAndCommit(1)
	finalOutcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))
	continued := appendAndCommit(2)
	continuedOutcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))

	// The continued partial belongs to a stream with no standing instruction
	// in force. The policy is still asked, with the full speaking option set:
	// whether a half-sentence justifies speaking is its judgement, not a
	// filter in front of it.
	if !final.Choice.Idle() || !final.Choice.Speaking || final.Event != coreinteraction.TranscriptFinal ||
		!continued.Choice.Idle() || !continued.Choice.Speaking || continued.DecisionStage != "policy" ||
		finalOutcome.Code != "keep" || continuedOutcome.Code != "keep" {
		t.Fatalf("lifecycle decisions final=%+v/%+v continued=%+v/%+v",
			final, finalOutcome, continued, continuedOutcome)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "voice_committed"))
	captured := decider.captured()
	if len(captured) != 3 ||
		!reflect.DeepEqual(captured[0].Options, coreinteraction.ChoiceOptions(false)) ||
		!reflect.DeepEqual(captured[1].Options, coreinteraction.ChoiceOptions(true)) ||
		!reflect.DeepEqual(captured[2].Options, coreinteraction.ChoiceOptions(true)) ||
		!strings.Contains(captured[1].Evidence, "agent: voice output is active and still being prepared") ||
		!strings.Contains(captured[1].Evidence,
			"agent output was deliberately triggered by an earlier revision of this same transcript stream") ||
		!strings.Contains(captured[1].Evidence, output.InFlight) ||
		!strings.Contains(captured[2].Evidence, "Which gives us plenty of time") {
		t.Fatalf("transcript lifecycle policy requests = %+v", captured)
	}
}

func TestSemanticAdmissionSealsAgentOutputForEachStartedDecision(t *testing.T) {
	entered := make(chan int, 2)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor,
		answers:    []string{"speak", "keep"},
		entered:    entered, release: release,
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
		Rules: "Classify this transcript.",
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := mountSemanticAdmission(t, decider, config)
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	sendPolicy(t, harness.ingress(t, "agent_output"), element.Envelope{
		Type: coreinteraction.AgentOutputType(), ItemID: "agent-output-idle",
		SessionID: "semantic-session", Payload: coreinteraction.AgentOutput{Revision: 1},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	first := semanticTranscriptObservation(
		"first-stream", "asr.endpoint", 1, "The first live observation.",
	)
	firstSnapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{first}}
	firstPrefix, err := trajectory.IdentifyPrefix(firstSnapshot, firstSnapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-1", firstSnapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-first",
		SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(
			first, "first-stream", firstPrefix, "state-1", firstSnapshot.Version,
		),
	})
	_ = receivePolicy(t, harness.egress(t, "state"))
	if call := awaitSemanticCall(t, entered); call != 0 {
		t.Fatalf("first semantic call = %d, want 0", call)
	}

	laterOutput := coreinteraction.AgentOutput{
		Revision: 2, Active: true, Queued: true,
		Saying:           "the later lifecycle response",
		InFlight:         "later lifecycle work is active",
		ProtectedStreams: []string{"second-stream"},
	}
	sendPolicy(t, harness.ingress(t, "agent_output"), element.Envelope{
		Type: coreinteraction.AgentOutputType(), ItemID: "agent-output-active",
		SessionID: "semantic-session", Payload: laterOutput,
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	second := semanticTranscriptObservation(
		"second-stream", "asr.endpoint", 2, "The second live observation.",
	)
	secondSnapshot := trajectory.Snapshot{
		Version: 2, Items: []trajectory.Item{first, second},
	}
	secondPrefix, err := trajectory.IdentifyPrefix(secondSnapshot, secondSnapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	sendSemanticContext(t, harness, "state-2", secondSnapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-second",
		SessionID: "semantic-session",
		Payload: semanticCommittedOutcome(
			second, "second-stream", secondPrefix, "state-2", secondSnapshot.Version,
		),
	})
	pending := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if pending.Pending != 1 || !pending.Active {
		t.Fatalf("second request did not wait behind the active decision: %+v", pending)
	}

	close(release)
	firstDecision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	_ = receivePolicy(t, harness.egress(t, "outcome"))
	_ = receivePolicy(t, harness.egress(t, "state"))
	acknowledgeSemanticVoice(t, harness, laterOutput, 3, 1)
	if call := awaitSemanticCall(t, entered); call != 1 {
		t.Fatalf("second semantic call = %d, want 1", call)
	}
	secondDecision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
	secondOutcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))

	if !firstDecision.Choice.Speak ||
		!secondDecision.Choice.Idle() || !secondDecision.Choice.Speaking || secondOutcome.Code != "keep" {
		t.Fatalf("sealed lifecycle decisions first=%+v second=%+v outcome=%+v",
			firstDecision, secondDecision, secondOutcome)
	}
	captured := decider.captured()
	if len(captured) != 2 ||
		!reflect.DeepEqual(captured[0].Options, coreinteraction.ChoiceOptions(false)) ||
		strings.Contains(captured[0].Evidence, laterOutput.Saying) ||
		strings.Contains(captured[0].Evidence, laterOutput.InFlight) ||
		!reflect.DeepEqual(captured[1].Options, coreinteraction.ChoiceOptions(true)) ||
		!strings.Contains(captured[1].Evidence, laterOutput.Saying) ||
		!strings.Contains(captured[1].Evidence, laterOutput.InFlight) ||
		!strings.Contains(captured[1].Evidence,
			"agent output was deliberately triggered by an earlier revision of this same transcript stream") {
		t.Fatalf("sealed lifecycle policy requests = %+v", captured)
	}
}

func TestSemanticAdmissionCancelWinsWhenProviderReturnsAfterCancellation(t *testing.T) {
	entered := make(chan int, 1)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"},
		entered: entered, release: release, ignoreCancellation: true,
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, commit := semanticObservation(t, "do this", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-cancel",
		SessionID: "semantic-session", Payload: commit,
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("semantic decider was not called")
	}
	active := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if !active.Active {
		t.Fatalf("active semantic state = %+v", active)
	}
	sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-1", SessionID: "semantic-session",
		Payload: policyelements.GenerationCancel{StreamID: "speech", Reason: "user interrupted"},
	})
	recorded := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	if recorded.Operation != "cancel" || recorded.Code != "cancel_recorded" {
		t.Fatalf("cancel receipt = %+v", recorded)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))
	close(release)
	canceled := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if canceled.Kind != policyelements.SemanticAdmissionCanceled || canceled.Code != "decision_canceled" ||
		!strings.Contains(canceled.Message, "user interrupted") || state.Canceled != 1 || state.Active {
		t.Fatalf("canceled outcome=%+v state=%+v", canceled, state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "decision"))
	assertNoPolicyEnvelope(t, harness.egress(t, "voice_committed"))
}

func TestSemanticAdmissionShutdownOwnsCancellationIgnoringDecision(t *testing.T) {
	entered := make(chan int, 1)
	exited := make(chan int, 1)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor,
		answers:    []string{"speak"},
		entered:    entered, exited: exited, release: release,
		ignoreCancellation: true,
	}
	mounted, err := mountSemanticAdmissionRegisteredWithMediaAndShutdown(
		t, semanticTestDescriptor, decider, semanticConfig(8, 8, 8), nil, 25*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	snapshot, commit := semanticObservation(t, "answer me", "speech", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-shutdown",
		SessionID: "semantic-session", Payload: commit,
	})
	if call := awaitSemanticCall(t, entered); call != 0 {
		t.Fatalf("semantic call = %d, want 0", call)
	}
	_ = receivePolicy(t, harness.egress(t, "state"))

	cancel()
	select {
	case runErr := <-done:
		if runErr == nil ||
			!strings.Contains(runErr.Error(), "graph shutdown timed out after 25ms") ||
			!strings.Contains(runErr.Error(), "unresponsive elements: admission") {
			t.Fatalf("shutdown error = %v", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("semantic admission shutdown was not bounded")
	}
	if decider.closed.Load() != 1 || !decider.closedWhileActive.Load() {
		t.Fatalf("forced close state: closed=%d while_active=%t",
			decider.closed.Load(), decider.closedWhileActive.Load())
	}
	select {
	case call := <-exited:
		t.Fatalf("cancellation-ignoring decision exited before release: call=%d", call)
	default:
	}
	close(release)
	select {
	case call := <-exited:
		if call != 0 {
			t.Fatalf("exited semantic call = %d, want 0", call)
		}
	case <-time.After(time.Second):
		t.Fatal("released semantic decision did not exit")
	}
}

func TestSemanticAdmissionRejectsProviderDriftAndClosesIt(t *testing.T) {
	drifted := semanticTestDescriptor
	drifted.Model = "different-model"
	decider := &semanticTestDecider{descriptor: drifted}
	harness, err := mountSemanticAdmissionResult(t, decider, semanticConfig(8, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = harness.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "descriptor drifted") {
		t.Fatalf("provider drift error = %v", err)
	}
	if decider.closed.Load() != 1 {
		t.Fatalf("drifted provider close count = %d", decider.closed.Load())
	}
}

func TestSemanticAdmissionProviderFailureAndTimeoutNeverGenerate(t *testing.T) {
	t.Run("provider failure", func(t *testing.T) {
		decider := &semanticTestDecider{
			descriptor: semanticTestDescriptor, failure: errors.New("policy service unavailable"),
		}
		harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 8))
		defer harness.stop(t)
		consumeSemanticStartup(t, harness)
		installSemanticInvocation(t, harness, 1, false)
		snapshot, commit := semanticObservation(t, "answer me", "speech", 1)
		sendSemanticContext(t, harness, "state-1", snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-failure",
			SessionID: "semantic-session", Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if outcome.Kind != policyelements.SemanticAdmissionFailed || outcome.Code != "decider_failed" ||
			!strings.Contains(outcome.Message, "unavailable") || state.Failed != 1 {
			t.Fatalf("provider failure outcome=%+v state=%+v", outcome, state)
		}
		assertNoSemanticGeneration(t, harness)
	})

	t.Run("provider timeout", func(t *testing.T) {
		descriptor := semanticTestDescriptor
		descriptor.DecisionTimeoutMS = 10
		decider := &semanticTestDecider{
			descriptor: descriptor, release: make(chan struct{}),
		}
		mounted, err := mountSemanticAdmissionRegistered(t, descriptor, decider, semanticConfig(8, 8, 8))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- mounted.Run(ctx) }()
		harness := policyHarness{mounted: mounted, done: done, cancel: cancel}
		defer harness.stop(t)
		consumeSemanticStartup(t, harness)
		installSemanticInvocation(t, harness, 1, false)
		snapshot, commit := semanticObservation(t, "answer me", "speech", 1)
		sendSemanticContext(t, harness, "state-1", snapshot)
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-timeout",
			SessionID: "semantic-session", Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if outcome.Kind != policyelements.SemanticAdmissionFailed || outcome.Code != "decision_timeout" ||
			state.Failed != 1 {
			t.Fatalf("provider timeout outcome=%+v state=%+v", outcome, state)
		}
		assertNoSemanticGeneration(t, harness)
	})
}

func TestSemanticAdmissionCancellationMemoryIsBounded(t *testing.T) {
	entered := make(chan int, 1)
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, answers: []string{"speak"}, entered: entered,
	}
	harness := mountSemanticAdmission(t, decider, semanticConfig(8, 8, 2))
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)
	for index, stream := range []string{"evicted", "retained-a", "retained-b"} {
		sendPolicy(t, harness.ingress(t, "cancel"), element.Envelope{
			Type: policyelements.GenerationCancelType(), ItemID: "precancel-" + string(rune('a'+index)),
			SessionID: "semantic-session", Payload: policyelements.GenerationCancel{
				StreamID: stream, Reason: "cancel before evidence",
			},
		})
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
		if outcome.Code != "cancel_recorded" || state.CancellationMemory != 2 {
			t.Fatalf("pre-cancel outcome=%+v state=%+v", outcome, state)
		}
	}
	snapshot, commit := semanticObservation(t, "still current", "evicted", 1)
	sendSemanticContext(t, harness, "state-1", snapshot)
	sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: "commit-evicted",
		SessionID: "semantic-session", Payload: commit,
	})
	awaitSemanticCall(t, entered)
	_ = receivePolicy(t, harness.egress(t, "state"))
	_ = receivePolicy(t, harness.egress(t, "decision"))
	_ = receivePolicy(t, harness.egress(t, "voice_committed"))
	admitted := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
	_ = receivePolicy(t, harness.egress(t, "state"))
	if admitted.Kind != policyelements.SemanticAdmissionAdmitted {
		t.Fatalf("evicted pre-cancel still canceled evidence: %+v", admitted)
	}
}

func mountSemanticAdmission(
	t *testing.T, decider *semanticTestDecider, config json.RawMessage,
) policyHarness {
	t.Helper()
	mounted, err := mountSemanticAdmissionResult(t, decider, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return policyHarness{mounted: mounted, done: done, cancel: cancel}
}

func mountSemanticAdmissionResult(
	t *testing.T, decider *semanticTestDecider, config json.RawMessage,
) (*graphruntime.Mounted, error) {
	return mountSemanticAdmissionRegistered(t, semanticTestDescriptor, decider, config)
}

func mountSemanticAdmissionRegistered(
	t *testing.T, descriptor policyelements.SemanticDeciderDescriptor,
	decider policyelements.SemanticDecider, config json.RawMessage,
) (*graphruntime.Mounted, error) {
	return mountSemanticAdmissionRegisteredWithMedia(t, descriptor, decider, config, nil)
}

func mountSemanticAdmissionRegisteredWithMedia(
	t *testing.T, descriptor policyelements.SemanticDeciderDescriptor,
	decider policyelements.SemanticDecider, config json.RawMessage, media continuation.MediaResolver,
) (*graphruntime.Mounted, error) {
	return mountSemanticAdmissionRegisteredWithMediaAndShutdown(
		t, descriptor, decider, config, media, 0,
	)
}

func mountSemanticAdmissionRegisteredWithMediaAndShutdown(
	t *testing.T, descriptor policyelements.SemanticDeciderDescriptor,
	decider policyelements.SemanticDecider, config json.RawMessage, media continuation.MediaResolver,
	shutdownTimeout time.Duration,
) (*graphruntime.Mounted, error) {
	t.Helper()
	providers := policyelements.NewSemanticDeciderRegistry()
	if err := providers.Register("semantic-primary", descriptor,
		func() (policyelements.SemanticDecider, error) { return decider, nil }); err != nil {
		return nil, err
	}
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(policyelements.SemanticDeciderRegistryService, providers); err != nil {
		return nil, err
	}
	if media != nil {
		if _, err := services.Set(cognitionelements.MediaResolverService, media); err != nil {
			return nil, err
		}
	}
	registry := graphruntime.NewRegistry()
	if err := policyelements.RegisterFactories(registry); err != nil {
		return nil, err
	}
	var clock atomic.Uint64
	return graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compilePolicySource(t, "semantic-admission-test.ortg", []byte(semanticAdmissionGraph)), Registry: registry,
		Services: services, Values: map[string]json.RawMessage{"admission": config},
		Now: func() uint64 { return clock.Add(1) }, ShutdownTimeout: shutdownTimeout,
	})
}

func assertNoSemanticGeneration(t *testing.T, harness policyHarness) {
	t.Helper()
	for _, output := range []string{
		"decision", "voice_committed", "voice_create",
	} {
		assertNoPolicyEnvelope(t, harness.egress(t, output))
	}
}

func semanticConfig(maxPending, terminalMemory, cancelMemory int) json.RawMessage {
	payload, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: maxPending,
		TerminalMemory: terminalMemory, CancelMemory: cancelMemory,
	})
	if err != nil {
		panic(err)
	}
	return payload
}

func consumeSemanticStartup(t *testing.T, harness policyHarness) {
	t.Helper()
	resolved := receivePolicy(t, harness.egress(t, "resolved")).Payload.(policyelements.SemanticDeciderResolution)
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if resolved.Reference != "semantic-primary" ||
		resolved.Descriptor.Provider != semanticTestDescriptor.Provider ||
		resolved.Descriptor.Model != semanticTestDescriptor.Model ||
		resolved.Descriptor.ConfigurationDigest != semanticTestDescriptor.ConfigurationDigest ||
		!strings.HasPrefix(resolved.DescriptorDigest, "sha256:") ||
		state.TerminalMemory == 0 || state.CancellationMemory == 0 {
		t.Fatalf("semantic startup resolution=%+v state=%+v", resolved, state)
	}
}

func installSemanticInvocation(t *testing.T, harness policyHarness, revision uint64, withTool bool) {
	t.Helper()
	invocation := continuation.Invocation{Instruction: "Act only when this turn requires it.", MaxOutputTokens: 128}
	if withTool {
		invocation.Capabilities = []continuation.Capability{{
			Name: "press_key", Description: "Press one keypad key", Available: true, ExecutionPhase: "fast",
		}}
		invocation.Tools = []continuation.ToolDefinition{{
			Name: "press_key", Description: "Press one keypad key",
			Parameters: json.RawMessage(`{"type":"object","properties":{"digit":{"type":"string"}}}`),
		}}
	}
	sendPolicy(t, harness.ingress(t, "update"), element.Envelope{
		Type: policyelements.SessionInvocationUpdateType(), ItemID: "settings-1",
		SessionID: "semantic-session", Payload: policyelements.SessionInvocationUpdate{
			Revision: revision, Invocation: invocation,
		},
	})
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if state.InvocationRevision != revision || !strings.HasPrefix(state.InvocationDigest, "sha256:") {
		t.Fatalf("semantic invocation state = %+v", state)
	}
}

func sendSemanticContext(t *testing.T, harness policyHarness, itemID string, snapshot trajectory.Snapshot) {
	t.Helper()
	sendPolicy(t, harness.ingress(t, "context"), element.Envelope{
		Type: stateelements.SnapshotType(), ItemID: itemID, SessionID: "semantic-session", Payload: snapshot,
	})
	state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
	if state.ContextVersion != snapshot.Version {
		t.Fatalf("semantic context state = %+v", state)
	}
}

func awaitSemanticCall(t *testing.T, entered <-chan int) int {
	t.Helper()
	select {
	case call := <-entered:
		return call
	case <-time.After(time.Second):
		t.Fatal("semantic decider was not called")
		return -1
	}
}

func semanticObservation(
	t *testing.T, content, stream string, sourceRevision uint64,
) (trajectory.Snapshot, stateelements.ObservationCommitOutcome) {
	t.Helper()
	item := trajectory.Item{
		ID:   "observation-" + stream + "-" + string(rune('0'+sourceRevision)),
		Kind: trajectory.KindObservation, SourceRevision: sourceRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: content,
		Event: &trajectory.EventMetadata{EventID: "event-" + stream},
	}
	snapshot := trajectory.Snapshot{Version: 1, Items: []trajectory.Item{item}}
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: item.Event.EventID,
		TrajectoryItemID: item.ID, StreamID: stream, ObservationRevision: 1,
		SourceRevision: sourceRevision, StoreVersion: snapshot.Version,
		Context: stateelements.CommittedContext{Prefix: identity, StateItemID: "state-1"},
	}
}

func semanticEndpointObservation(
	stream string, revision, sourceRevision uint64, content string,
) trajectory.Item {
	return trajectory.Item{
		ID: "observation-" + stream, Kind: trajectory.KindObservation,
		MonotonicNS: revision, SourceRevision: sourceRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: content,
		Observation: &trajectory.ObservationMeta{
			Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		},
		Event: &trajectory.EventMetadata{
			EventID: "event-" + stream, Type: "asr.endpoint", Source: "asr", Channel: "microphone",
		},
	}
}

func semanticTranscriptObservation(
	stream, eventType string, sourceRevision uint64, content string,
) trajectory.Item {
	return trajectory.Item{
		ID: "observation-" + stream, Kind: trajectory.KindObservation,
		MonotonicNS: sourceRevision, SourceRevision: sourceRevision,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: content,
		Observation: &trajectory.ObservationMeta{
			Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		},
		Event: &trajectory.EventMetadata{
			EventID: "event-" + stream, Type: eventType, Source: "asr", Channel: "microphone",
		},
	}
}

func semanticCommittedOutcome(
	item trajectory.Item, stream string, prefix trajectory.PrefixIdentity,
	stateItem string, version uint64,
) stateelements.ObservationCommitOutcome {
	return stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: item.Event.EventID,
		TrajectoryItemID: item.ID, StreamID: stream, ObservationRevision: 1,
		SourceRevision: item.SourceRevision, StoreVersion: version,
		Context: stateelements.CommittedContext{Prefix: prefix, StateItemID: stateItem},
	}
}

func semanticVisualObservation(
	t *testing.T, content, stream string, sourceRevision uint64, handle string,
) (trajectory.Snapshot, stateelements.ObservationCommitOutcome) {
	t.Helper()
	snapshot, commit := semanticObservation(t, content, stream, sourceRevision)
	snapshot.Items[0].Observation = &trajectory.ObservationMeta{
		Observer: "client", Source: stream, Authority: trajectory.AuthorityUser,
		Media: []trajectory.MediaRef{{
			Handle: handle, MIMEType: "image/png", Source: stream, Width: 64, Height: 48, Bytes: 4,
		}},
	}
	identity, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	commit.Context.Prefix = identity
	return snapshot, commit
}

var _ policyelements.SemanticDecider = (*semanticTestDecider)(nil)

// A final transcript that lands while the agent is audibly speaking, in a
// is the FDB interruption case. The option set is derived from whether the
// agent is speaking, so a final landing mid-speech is always offered keep and
// stop, and choosing one resolves as a disposition rather than a failure.
func TestSemanticAdmissionOffersSpeechControlsWhileSpeaking(t *testing.T) {
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor,
		answers:    []string{"stop", "keep"},
	}
	config, err := json.Marshal(policyelements.SemanticAdmissionConfig{
		Decider: "semantic-primary", RecentLines: 12, MaxPending: 8,
		TerminalMemory: 8, CancelMemory: 8, StandingMemory: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := mountSemanticAdmission(t, decider, config)
	defer harness.stop(t)
	consumeSemanticStartup(t, harness)
	installSemanticInvocation(t, harness, 1, false)

	sendPolicy(t, harness.ingress(t, "agent_output"), element.Envelope{
		Type: coreinteraction.AgentOutputType(), ItemID: "agent-output-speaking",
		SessionID: "semantic-session", Payload: coreinteraction.AgentOutput{
			Revision: 1, Active: true, Audible: true, Saying: "You could make a stir fry tonight,",
			InFlight: "voice output active: model=1, segmentation=1, synthesis=1, playback=1",
		},
	})
	_ = receivePolicy(t, harness.egress(t, "state"))

	items := []trajectory.Item{
		semanticTranscriptObservation("interruption-final", "asr.endpoint", 1,
			"Oh, before I forget, do we need more coffee?"),
		semanticTranscriptObservation("continuation-final", "asr.endpoint", 2,
			"Actually never mind, keep going."),
	}
	commitFinal := func(index int) (policyelements.SemanticDecision, policyelements.SemanticAdmissionOutcome) {
		snapshot := trajectory.Snapshot{
			Version: uint64(index + 1), Items: append([]trajectory.Item(nil), items[:index+1]...),
		}
		stateID := fmt.Sprintf("state-%d", index+1)
		sendSemanticContext(t, harness, stateID, snapshot)
		prefix, identifyErr := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
		if identifyErr != nil {
			t.Fatal(identifyErr)
		}
		commit := semanticCommittedOutcome(items[index], fmt.Sprintf("stream-%d", index+1), prefix, stateID, snapshot.Version)
		commit.ObservationRevision = items[index].SourceRevision
		sendPolicy(t, harness.ingress(t, "committed"), element.Envelope{
			Type:   stateelements.ObservationCommitOutcomeType(),
			ItemID: fmt.Sprintf("commit-%d", index+1), SessionID: "semantic-session",
			Payload: commit,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
		decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
		outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
		_ = receivePolicy(t, harness.egress(t, "state"))
		return decision, outcome
	}

	stop, stopOutcome := commitFinal(0)
	if !stop.Choice.Stop || stop.Choice.Speak || stopOutcome.Kind != policyelements.SemanticAdmissionSuppressed ||
		stopOutcome.Code != "stop" {
		t.Fatalf("interruption mid-speech: decision=%+v outcome=%+v", stop, stopOutcome)
	}
	keep, keepOutcome := commitFinal(1)
	if !keep.Choice.Idle() || !keep.Choice.Speaking || keepOutcome.Kind != policyelements.SemanticAdmissionSuppressed ||
		keepOutcome.Code != "keep" {
		t.Fatalf("continuation mid-speech: decision=%+v outcome=%+v", keep, keepOutcome)
	}
	for _, outcome := range []policyelements.SemanticAdmissionOutcome{stopOutcome, keepOutcome} {
		if outcome.Kind == policyelements.SemanticAdmissionFailed || outcome.Code == "decider_failed" {
			t.Fatalf("a mid-speech final transcript failed the session: %+v", outcome)
		}
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "voice_committed"))
	captured := decider.captured()
	if len(captured) != 2 ||
		!reflect.DeepEqual(captured[0].Options, coreinteraction.ChoiceOptions(true)) ||
		!reflect.DeepEqual(captured[1].Options, coreinteraction.ChoiceOptions(true)) ||
		!strings.Contains(captured[0].Evidence, "You could make a stir fry tonight,") {
		t.Fatalf("speech controls were not what the policy was offered: %+v", captured)
	}
}

// acknowledgeSemanticVoice plays the output lifecycle's part of the lockstep:
// the generation the last decision admitted is seen running, then answered.
// Until it is, the runner decides nothing.
func acknowledgeSemanticVoice(
	t *testing.T, harness policyHarness, template coreinteraction.AgentOutput, revision, generation uint64,
) {
	t.Helper()
	for step, generating := range []int{1, 0} {
		output := template
		output.Revision = revision + uint64(step)
		output.Active = true
		output.Generating = generating
		output.GenerationsStarted = generation
		output.GenerationsFinished = generation - uint64(generating)
		if output.InFlight == "" {
			output.InFlight = "voice output active"
		}
		sendPolicy(t, harness.ingress(t, "agent_output"), element.Envelope{
			Type: coreinteraction.AgentOutputType(), ItemID: fmt.Sprintf("agent-output-%d", output.Revision),
			SessionID: "semantic-session", Payload: output,
		})
		_ = receivePolicy(t, harness.egress(t, "state"))
	}
}
