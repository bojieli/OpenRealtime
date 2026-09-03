package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func meetingArtifact(id string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: id, Revision: "implementation:1"}
}

func meetingApplicationFixture(t testing.TB) (graphs.MeetingAssistantRegistrationConfig, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	ownership := legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
	capabilities := legacy.Capabilities{
		Video: true, Observations: true, ManualTurns: true, MaxOutputTokens: 512,
		Observers: []string{"audio", "screen"},
		Voice:     legacy.VoiceControl{InForce: "meeting-voice"},
		Stack: legacy.StackCapabilities{
			AudioInput: true, AudioOutput: true, VisualInput: true,
			Transcription: true, TurnGeneration: true, ConcurrentIO: true,
			TextInjection: true,
		},
	}
	foreground := continuation.Descriptor{
		Provider: "fixture", Model: "foreground", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
	background := foreground
	background.Model = "background"
	background.Phase = trajectory.PhaseSlow
	background.SpeechAuthority = continuation.SpeechAuthoritySilent
	return graphs.MeetingAssistantRegistrationConfig{
		ApplicationArtifact: meetingArtifact("go://openrealtime/graphs/meeting-assistant/application/v1"),
		ProviderArtifact:    meetingArtifact("go://openrealtime/meeting/session-provider/v2"),
		Session: meetinggraph.SessionPluginConfig{
			AdapterArtifact: meetingArtifact("go://openrealtime/meeting/session-adapter/v1"),
			Foreground: meetinggraph.ForegroundPlugin{
				Artifact:            meetingArtifact("plugin://openrealtime/meeting/foreground"),
				ProviderArtifact:    meetingArtifact("model://openrealtime/meeting/foreground"),
				RuntimeArtifact:     meetingArtifact("runtime://openrealtime/meeting/foreground"),
				WireAdapterArtifact: meetingArtifact("adapter://openrealtime/meeting/foreground-wire"),
				BindingName:         "meeting-fixture", Ownership: ownership,
				Capabilities: capabilities, Descriptor: foreground,
				Factory: func(context.Context, legacy.Options) (legacy.Binding, error) {
					calls.Add(1)
					return nil, errors.New("foreground must stay lazy")
				},
			},
			Visual: meetinggraph.VisualPlugin{
				Artifact: meetingArtifact("model://openrealtime/meeting/visual"),
				Descriptor: perceptionelements.VisualProviderDescriptor{
					Name: "meeting-visual", Revision: "implementation:1",
				},
				Factory: func(context.Context, legacy.Options) (perceptionelements.VisualProvider, error) {
					calls.Add(1)
					return nil, errors.New("visual must stay lazy")
				},
			},
			Background: meetinggraph.BackgroundPlugin{
				Artifact: meetingArtifact("model://openrealtime/meeting/background"), Descriptor: background,
				Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
					calls.Add(1)
					return nil, errors.New("background must stay lazy")
				},
			},
			TTS: meetinggraph.TTSPlugin{
				Artifact: meetingArtifact("model://openrealtime/meeting/tts"),
				Descriptor: v1.Descriptor{
					Name: "meeting-fixture-tts", Version: "implementation:1",
					Capabilities: v1.Capabilities{
						v1.CapabilityPCM16Output:     true,
						v1.CapabilityStreamingOutput: true,
					},
				},
				Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
					calls.Add(1)
					return nil, errors.New("TTS must stay lazy")
				},
			},
		},
	}, calls
}

func TestMeetingAssistantRegistrationResolvesExactGraphWithoutAcquiringProviders(t *testing.T) {
	config, calls := meetingApplicationFixture(t)
	var readinessCalls atomic.Int32
	config.Readiness = []graphlaunch.ReadinessCheck{{
		Name: "meeting-production-readiness",
		Check: func(context.Context) error {
			readinessCalls.Add(1)
			return nil
		},
	}}
	registration, err := graphs.MeetingAssistantApplicationRegistration(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Readiness[0] = graphlaunch.ReadinessCheck{
		Name: "mutated-after-registration",
		Check: func(context.Context) error {
			return errors.New("caller-owned readiness callback leaked into registration")
		},
	}
	application, err := graphs.MeetingAssistantApplicationConfig(registration, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(application.Plan.OptionalDependencies, []string{stateelements.TrajectoryStoreService}) {
		t.Fatalf("optional dependencies = %v", application.Plan.OptionalDependencies)
	}
	payload, err := json.Marshal(application)
	if err != nil {
		t.Fatal(err)
	}
	launchConfig, err := registration.Application.Factory(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := graphlaunch.New(context.Background(), launchConfig)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan == nil || prepared.Plan.Graph().ID != meetinggraph.GraphID ||
		prepared.Evidence.Graph != meetinggraph.GraphID || prepared.Evidence.Profiles == nil ||
		prepared.Plan.Graph().Fingerprint == "" || launchConfig.Adapter != registration.Adapter {
		t.Fatalf("prepared Meeting application = %+v", prepared.Plan)
	}
	if got := prepared.Binding.SessionAdapterProfile().Capabilities.Observers; !slices.Equal(got, []string{"audio", "screen"}) {
		t.Fatalf("Meeting observer selectors = %v, want exact foreground audio plus built-in screen", got)
	}
	if readinessCalls.Load() != 0 || len(prepared.Readiness) != 1 ||
		prepared.Readiness[0].Name != "meeting-production-readiness" {
		t.Fatalf("resource-free readiness composition = %+v, calls=%d", prepared.Readiness, readinessCalls.Load())
	}
	if err := prepared.Readiness[0].Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readinessCalls.Load() != 1 {
		t.Fatalf("explicit readiness calls = %d, want 1", readinessCalls.Load())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("resource-free application resolution acquired %d provider(s)", got)
	}
}

func TestMeetingAssistantEmbeddedArtifactsAreIndependentAndStrict(t *testing.T) {
	first, firstSecrets, err := graphs.MeetingAssistantArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	second, secondSecrets, err := graphs.MeetingAssistantArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Topology.Data) == 0 || len(first.Values.Data) == 0 || len(first.Lock.Data) == 0 ||
		len(first.Deployment.Data) == 0 || firstSecrets == nil || secondSecrets == nil {
		t.Fatal("embedded Meeting Assistant bundle is incomplete")
	}
	first.Topology.Data[0] ^= 0xff
	if slices.Equal(first.Topology.Data, second.Topology.Data) {
		t.Fatal("Meeting Assistant artifact bytes alias across calls")
	}
	if len(firstSecrets.Secrets) != 0 || len(secondSecrets.Secrets) != 0 {
		t.Fatal("Meeting Assistant graph unexpectedly embeds credentials")
	}
}
