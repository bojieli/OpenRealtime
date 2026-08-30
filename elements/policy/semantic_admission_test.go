package policy_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
    input committed = admission.committed;
    input create = admission.create;
    input quiet = admission.quiet;
    input cancel = admission.cancel;
    output voice_committed = admission.voice_committed;
    output silent_committed = admission.silent_committed;
    output voice_create = admission.voice_create;
    output silent_create = admission.silent_create;
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
	if descriptor.Name != "policy.SemanticAdmission" || descriptor.Revision != 2 ||
		descriptor.ConfigSchema != "schema://openrealtime/policy/semantic-admission-config/v2" {
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
		descriptor: visualDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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

func TestSemanticAdmissionDirectVisualInputNeverReadsImagesPastTheBoundPrefix(t *testing.T) {
	visualDescriptor := semanticTestDescriptor
	visualDescriptor.Vision = true
	decider := &semanticTestDecider{
		descriptor: visualDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
		descriptor: visualDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
	acts               []coreinteraction.Act
	failure            error
	decisions          []coreinteraction.Decision
	entered            chan int
	exited             chan int
	release            chan struct{}
	ignoreCancellation bool
	closed             atomic.Int32
}

func (decider *semanticTestDecider) Name() string { return "semantic-test" }

func (decider *semanticTestDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (decider *semanticTestDecider) Decide(
	ctx context.Context, decision coreinteraction.Decision,
) (outcome coreinteraction.Outcome, err error) {
	decider.mu.Lock()
	call := len(decider.decisions)
	decider.decisions = append(decider.decisions, cloneSemanticTestDecision(decision))
	act := coreinteraction.ActAnswer
	if len(decider.acts) != 0 {
		act = decider.acts[min(call, len(decider.acts)-1)]
	}
	failure := decider.failure
	entered, exited, release := decider.entered, decider.exited, decider.release
	ignoreCancellation := decider.ignoreCancellation
	decider.mu.Unlock()
	if exited != nil {
		defer func() { exited <- call }()
	}
	if entered != nil {
		select {
		case entered <- call:
		case <-ctx.Done():
			return coreinteraction.Outcome{}, ctx.Err()
		}
	}
	if release != nil {
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
	return coreinteraction.Outcome{Option: string(act), Index: semanticOptionIndex(decision.Options, act)}, nil
}

func (decider *semanticTestDecider) Close() error {
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

func semanticOptionIndex(options []string, act coreinteraction.Act) int {
	for index, option := range options {
		if option == string(act) {
			return index
		}
	}
	return -1
}

func TestSemanticAdmissionRoutesOnlyTheEnumeratedBranch(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		act        coreinteraction.Act
		tools      bool
		branch     string
		wantKind   policyelements.SemanticAdmissionOutcomeKind
		wantSilent uint64
		wantVoice  uint64
	}{
		{name: "listen suppresses generation", act: coreinteraction.ActStaySilent,
			wantKind: policyelements.SemanticAdmissionSuppressed},
		{name: "answer admits only voice", act: coreinteraction.ActAnswer, branch: "voice_committed",
			wantKind: policyelements.SemanticAdmissionAdmitted, wantVoice: 1},
		{name: "act silently admits only silent cognition", act: coreinteraction.ActActSilently,
			tools: true, branch: "silent_committed", wantKind: policyelements.SemanticAdmissionAdmitted,
			wantSilent: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			entered := make(chan int, 1)
			exited := make(chan int, 1)
			decider := &semanticTestDecider{
				descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{testCase.act},
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
			if !ok || payload.Act != testCase.act || payload.EvidenceItemID != "commit-1" ||
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
				if !reflect.DeepEqual(branch.Payload, commit) ||
					!containsPolicy(branch.CausalParents, decision.ItemID) {
					t.Fatalf("semantic branch = %+v", branch)
				}
			}
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if outcome.Kind != testCase.wantKind || outcome.Act != testCase.act ||
				state.Active || state.Pending != 0 || state.AdmittedVoice != testCase.wantVoice ||
				state.AdmittedSilent != testCase.wantSilent {
				t.Fatalf("semantic outcome=%+v state=%+v", outcome, state)
			}
			for _, branch := range []string{"voice_committed", "silent_committed", "voice_create", "silent_create"} {
				if branch != testCase.branch {
					assertNoPolicyEnvelope(t, harness.egress(t, branch))
				}
			}
			captured := decider.captured()
			if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "please help") ||
				semanticOptionIndex(captured[0].Options, testCase.act) < 0 {
				t.Fatalf("policy request = %+v", captured)
			}
		})
	}
}

func TestSemanticAdmissionWaitsForExactInputsAndSealsTheCommittedPrefix(t *testing.T) {
	decider := &semanticTestDecider{descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer}}
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
		descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
		descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
	assertNoPolicyEnvelope(t, harness.egress(t, "silent_create"))
	if captured := decider.captured(); len(captured) != 0 {
		t.Fatalf("tampered bound create reached decider: %+v", captured)
	}
}

func TestSemanticAdmissionRoutesManualAndQuietCreatesThroughTheSamePolicy(t *testing.T) {
	for _, operation := range []string{"create", "quiet"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan int, 1)
			decider := &semanticTestDecider{
				descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("semantic create did not reach the decider")
			}
			active := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if !active.Active {
				t.Fatalf("semantic create active state = %+v", active)
			}
			decision := receivePolicy(t, harness.egress(t, "decision")).Payload.(policyelements.SemanticDecision)
			branch := receivePolicy(t, harness.egress(t, "voice_create"))
			outcome := receivePolicy(t, harness.egress(t, "outcome")).Payload.(policyelements.SemanticAdmissionOutcome)
			state := receivePolicy(t, harness.egress(t, "state")).Payload.(policyelements.SemanticAdmissionState)
			if decision.Operation != operation || decision.Act != coreinteraction.ActAnswer ||
				!reflect.DeepEqual(branch.Payload, create) || outcome.Kind != policyelements.SemanticAdmissionAdmitted ||
				state.AdmittedVoice != 1 || state.Active {
				t.Fatalf("%s decision=%+v branch=%+v outcome=%+v state=%+v",
					operation, decision, branch, outcome, state)
			}
			captured := decider.captured()
			if len(captured) != 1 || !strings.Contains(captured[0].Evidence, "If I go quiet") {
				t.Fatalf("%s policy request = %+v", operation, captured)
			}
			if operation == "quiet" && !strings.Contains(captured[0].Evidence, "silence: 15s") {
				t.Fatalf("quiet policy omitted its exact timer evidence: %s", captured[0].Evidence)
			}
		})
	}
}

func TestSemanticAdmissionExplicitCreateConsultsPolicyAfterToolResult(t *testing.T) {
	entered := make(chan int, 1)
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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
	if decision.Operation != "create" || decision.Act != coreinteraction.ActAnswer ||
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
		acts:       []coreinteraction.Act{coreinteraction.ActAnswer, coreinteraction.ActAnswer},
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
	if decision.SourceRevision != 2 || !reflect.DeepEqual(branch.Payload, secondCommit) ||
		final.Kind != policyelements.SemanticAdmissionAdmitted || state.Canceled != 1 ||
		state.AdmittedVoice != 1 {
		t.Fatalf("replacement decision=%+v branch=%+v outcome=%+v state=%+v",
			decision, branch, final, state)
	}
	assertNoPolicyEnvelope(t, harness.egress(t, "silent_committed"))
}

func TestSemanticAdmissionCancelWinsWhenProviderReturnsAfterCancellation(t *testing.T) {
	entered := make(chan int, 1)
	release := make(chan struct{})
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer},
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

func TestSemanticAdmissionCancellationMemoryIsBoundedAndConsumed(t *testing.T) {
	entered := make(chan int, 1)
	decider := &semanticTestDecider{
		descriptor: semanticTestDescriptor, acts: []coreinteraction.Act{coreinteraction.ActAnswer}, entered: entered,
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
	decider *semanticTestDecider, config json.RawMessage,
) (*graphruntime.Mounted, error) {
	return mountSemanticAdmissionRegisteredWithMedia(t, descriptor, decider, config, nil)
}

func mountSemanticAdmissionRegisteredWithMedia(
	t *testing.T, descriptor policyelements.SemanticDeciderDescriptor,
	decider *semanticTestDecider, config json.RawMessage, media continuation.MediaResolver,
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
		Now: func() uint64 { return clock.Add(1) },
	})
}

func assertNoSemanticGeneration(t *testing.T, harness policyHarness) {
	t.Helper()
	for _, output := range []string{
		"decision", "voice_committed", "silent_committed", "voice_create", "silent_create",
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
