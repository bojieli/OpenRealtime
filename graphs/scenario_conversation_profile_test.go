package graphs_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestScenarioConversationApplicationProfileResolvesExactGraphWithoutResources(t *testing.T) {
	fixture := newScenarioProfileFixture()
	registration, err := graphs.ScenarioConversationApplicationRegistration(fixture.registration)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Reference != scenarioconversation.ApplicationReference ||
		registration.Artifact != fixture.registration.ApplicationArtifact ||
		registration.ProviderArtifact != fixture.registration.ProviderArtifact {
		t.Fatalf("scenario application registration = %+v", registration)
	}
	payload := marshalScenarioApplication(t, fixture.application)

	previewConfig, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), previewConfig)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          "openrealtime.scenario-conversation.profile-test",
		Revision:      1,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: scenarioconversation.ApplicationReference,
				Artifact:  fixture.registration.ApplicationArtifact,
			},
			Configuration: payload,
		},
		Plan: preview.Plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference:       scenarioconversation.AdapterReference,
			RuntimeArtifact: fixture.registration.RuntimeArtifact,
			ProfileName:     scenarioconversation.ProfileName, ProfileRevision: scenarioconversation.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.scenario-conversation.profile-test", ProfileRevision: 1,
			ProviderArtifact: fixture.registration.ProviderArtifact,
			GatewayArtifact:  fixture.gatewayArtifact,
			Model:            "scenario-model", TranscriptionModel: "scenario-asr",
			ValidateWire: true, InspectionTokenTTLMS: 30_000, MaxAudioFrameBytes: 1 << 20,
			VideoLimits: openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := launchprofile.NewRegistry([]launchprofile.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := serverplugin.NewProfileGraphBundle(context.Background(), serverplugin.ProfileGraphBundleConfig{
		Profile: profile, Applications: registry, GatewayArtifact: fixture.gatewayArtifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.GraphPlan.Identity() != profile.Plan ||
		bundle.ServerBundle.Profile.Name != profile.Server.ProfileName {
		t.Fatal("scenario application profile lost its exact graph or server identity")
	}
	assertScenarioFactoriesUnopened(t, fixture)

	// Artifact reads are defensive and the custom media bounds enter the plan,
	// rather than living only in the session-side resolver bridge.
	first, err := graphs.ScenarioConversationArtifacts(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	first.Topology.Data[0] ^= 0xff
	second, err := graphs.ScenarioConversationArtifacts(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Topology.Data) == 0 || first.Topology.Data[0] == second.Topology.Data[0] {
		t.Fatal("embedded scenario artifacts exposed mutable storage")
	}
	values := bundle.GraphPlan.Values()
	var retention struct {
		MaxItems        int `json:"max_items"`
		MaxBytes        int `json:"max_bytes"`
		MaxItemBytes    int `json:"max_item_bytes"`
		MaxActiveLeases int `json:"max_active_leases"`
	}
	if err := json.Unmarshal(values["retention"], &retention); err != nil {
		t.Fatal(err)
	}
	if retention.MaxItems != fixture.application.Media.MaxItems ||
		retention.MaxBytes != fixture.application.Media.MaxBytes ||
		retention.MaxItemBytes != fixture.application.Media.MaxItemBytes ||
		retention.MaxActiveLeases != fixture.application.Media.MaxActiveLeases {
		t.Fatalf("selected media bounds did not enter plan: %+v", retention)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["unknown"] = true
	unknown, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload json.RawMessage
		want    string
	}{
		{name: "unknown field", payload: unknown, want: "unknown field"},
		{name: "ASR missing", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.ASR.Reference = "asr://test/uninstalled"
		}), want: "ASR registry is missing"},
		{name: "ASR artifact drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.ASR.Artifact.Revision = "build:drift"
		}), want: "artifact or descriptor drifted"},
		{name: "model descriptor drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Model.Descriptor.Model = "drifted"
		}), want: "artifact or descriptor drifted"},
		{name: "model authority escalation", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Model.Descriptor.ToolAuthority = continuation.ToolAuthorityExecute
		}), want: "proposal-only"},
		{name: "TTS voice drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.TTS.Voice = "different-voice"
		}), want: "voice drifted"},
		{name: "tool confirmation escalation", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Tools[0].Confirm = legacyaction.ConfirmAlways
		}), want: "unsupported confirmation"},
		{name: "tool target drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Tools[0].Target = "different-client"
		}), want: "want \"scenario-client\""},
		{name: "duplicate target source", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Target.Sources = []string{"message", "message"}
		}), want: "is repeated"},
		{name: "media bound escalation", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Media.MaxBytes = 2 << 30
		}), want: "max_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted := profile.Clone()
			drifted.Application.Configuration = test.payload
			drifted, err = launchprofile.Freeze(drifted)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serverplugin.NewProfileGraphBundle(context.Background(), serverplugin.ProfileGraphBundleConfig{
				Profile: drifted, Applications: registry, GatewayArtifact: fixture.gatewayArtifact,
			}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario profile drift error = %v, want %q", err, test.want)
			}
			assertScenarioFactoriesUnopened(t, fixture)
		})
	}
}

func TestScenarioConversationAdapterCarriesFullScenarioContractWithDistinctPlaybackReceipts(t *testing.T) {
	fixture := newScenarioProfileFixture()
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	config, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	var selected graphbinding.SessionAdapterProfile
	decorateScenarioAdapter(t, &config, func(profile *graphbinding.SessionAdapterProfile) error {
		selected = profile.Clone()
		return nil
	})
	launched, err := graphnative.New(context.Background(), graphnative.Config{Launch: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.ValidateProfile(selected, launched.Plan.Graph()); err != nil {
		t.Fatal(err)
	}
	want := map[graphbinding.AdapterOperation]string{
		graphbinding.AdapterOutputTurnBegin:   "gateway_turn_begin",
		graphbinding.AdapterOutputTurnEnd:     "gateway_turn_end",
		graphbinding.AdapterOutputSpeechBegin: "gateway_speech_begin",
		graphbinding.AdapterOutputSpeechText:  "gateway_speech_text",
		graphbinding.AdapterOutputSpeechAudio: "gateway_speech_audio",
		graphbinding.AdapterOutputSpeechEnd:   "gateway_speech_end",
	}
	seenBoundaries := make(map[string]struct{}, len(want))
	for _, boundary := range selected.Boundaries {
		expected, relevant := want[boundary.Operation]
		if !relevant {
			continue
		}
		if boundary.Boundary != expected {
			t.Fatalf("scenario operation %s maps %q, want %q",
				boundary.Operation, boundary.Boundary, expected)
		}
		if _, aliased := seenBoundaries[boundary.Boundary]; aliased {
			t.Fatalf("scenario playback receipt boundary %q is aliased", boundary.Boundary)
		}
		seenBoundaries[boundary.Boundary] = struct{}{}
		delete(want, boundary.Operation)
	}
	if len(want) != 0 {
		t.Fatalf("scenario profile omitted playback receipt operations: %v", want)
	}
	assertScenarioFactoriesUnopened(t, fixture)

	tests := []struct {
		name   string
		mutate func(*graphbinding.SessionAdapterProfile)
		want   string
	}{
		{
			name: "missing playback receipt",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				for index, boundary := range profile.Boundaries {
					if boundary.Operation == graphbinding.AdapterOutputSpeechAudio {
						profile.Boundaries = append(profile.Boundaries[:index], profile.Boundaries[index+1:]...)
						return
					}
				}
			},
			want: "missing required seams",
		},
		{
			name: "aliased playback receipts",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				var turnBegin graphbinding.AdapterBoundary
				for _, boundary := range profile.Boundaries {
					if boundary.Operation == graphbinding.AdapterOutputTurnBegin {
						turnBegin = boundary
					}
				}
				for index := range profile.Boundaries {
					if profile.Boundaries[index].Operation == graphbinding.AdapterOutputTurnEnd {
						profile.Boundaries[index].Boundary = turnBegin.Boundary
						profile.Boundaries[index].Type = turnBegin.Type.Clone()
					}
				}
			},
			want: "mapped more than once",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			drifted, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
			if err != nil {
				t.Fatal(err)
			}
			decorateScenarioAdapter(t, &drifted, func(profile *graphbinding.SessionAdapterProfile) error {
				test.mutate(profile)
				frozen, freezeErr := graphbinding.FreezeSessionAdapterProfile(*profile)
				if freezeErr != nil {
					return freezeErr
				}
				*profile = frozen
				return nil
			})
			if _, err := graphnative.New(context.Background(), graphnative.Config{Launch: drifted}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario playback projection error = %v, want %q", err, test.want)
			}
			assertScenarioFactoriesUnopened(t, fixture)
		})
	}
}

func decorateScenarioAdapter(
	t *testing.T, config *graphlaunch.Config,
	mutate func(*graphbinding.SessionAdapterProfile) error,
) {
	t.Helper()
	for index := range config.Catalog.Adapters {
		plugin := &config.Catalog.Adapters[index]
		if plugin.Reference != config.Adapter.Reference {
			continue
		}
		original := plugin.Bind
		plugin.Bind = func(
			ctx context.Context, plan *graphconfig.Plan,
		) (graphlaunch.BoundAdapter, error) {
			bound, err := original(ctx, plan)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			profile := bound.Profile.Clone()
			if err := mutate(&profile); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			bound.Profile = profile
			return bound, nil
		}
		return
	}
	t.Fatal("scenario launch config omitted selected adapter plugin")
}

type scenarioProfileFixture struct {
	application     scenarioconversation.ApplicationConfig
	registration    scenarioconversation.ApplicationRegistrationConfig
	gatewayArtifact inspect.ArtifactIdentity
	asrOpened       *atomic.Int32
	modelOpened     *atomic.Int32
	ttsOpened       *atomic.Int32
}

func newScenarioProfileFixture() scenarioProfileFixture {
	artifact := func(name string) inspect.ArtifactIdentity {
		return inspect.ArtifactIdentity{ID: "plugin://test/scenario/" + name, Revision: "build:1"}
	}
	asrSelection := scenarioconversation.ApplicationASRSelection{
		Reference: "asr://test/scenario/v1", Artifact: artifact("asr"),
		Descriptor: v1.Descriptor{Name: "scenario-asr", Version: "1", Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true, v1.CapabilityRevisions: true,
		}},
	}
	modelSelection := scenarioconversation.ApplicationModelSelection{
		Reference: "model://test/scenario/v1", Artifact: artifact("model"),
		Descriptor: continuation.Descriptor{
			Provider: "test", Model: "scenario-model", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortLow, Streaming: true, Vision: true,
			ToolAuthority:   continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice,
		},
	}
	ttsSelection := scenarioconversation.ApplicationTTSSelection{
		Reference: "tts://test/scenario/v1", Artifact: artifact("tts"), Voice: "scenario-voice",
		Descriptor: v1.Descriptor{Name: "scenario-tts", Version: "1", Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
		}},
	}
	application := scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		ASR:           asrSelection, Model: modelSelection, TTS: ttsSelection,
		Tools: []scenarioconversation.ToolDeclaration{{
			Name: "lookup.weather", Description: "Look up weather.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			Confirm:    legacyaction.ConfirmNever,
		}},
		Target: computeruse.Target{Name: "scenario-client", Sources: []string{"message"}, Width: 1, Height: 1},
		Gate: scenarioconversation.ApplicationGateSelection{
			Threshold: 0.5, PrefixPaddingMS: 300, SilenceDurationMS: 500, SpeechDurationMS: 120,
		},
		Media: scenarioconversation.MediaLimits{
			MaxItems: 7, MaxBytes: 4 << 20, MaxItemBytes: 2 << 20,
			MaxPending: 5, MaxActiveLeases: 9,
		},
		MaxOutputTokens: 4096,
	}
	asrOpened, modelOpened, ttsOpened := &atomic.Int32{}, &atomic.Int32{}, &atomic.Int32{}
	registration := scenarioconversation.ApplicationRegistrationConfig{
		ApplicationArtifact: artifact("application"), ProviderArtifact: artifact("provider"),
		RuntimeArtifact: artifact("adapter"), DependencyArtifact: artifact("session-services"),
		ASR: []scenarioconversation.ASRFactoryRegistration{{
			ApplicationASRSelection: asrSelection,
			Factory: func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				asrOpened.Add(1)
				return nil, nil
			},
		}},
		Models: []scenarioconversation.ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelOpened.Add(1)
				return nil, nil
			},
		}},
		TTS: []scenarioconversation.TTSFactoryRegistration{{
			ApplicationTTSSelection: ttsSelection,
			Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				ttsOpened.Add(1)
				return nil, nil
			},
		}},
	}
	return scenarioProfileFixture{
		application: application, registration: registration, gatewayArtifact: artifact("gateway"),
		asrOpened: asrOpened, modelOpened: modelOpened, ttsOpened: ttsOpened,
	}
}

func (fixture scenarioProfileFixture) pluginConfig() scenarioconversation.PluginConfig {
	return scenarioconversation.PluginConfig{
		RuntimeArtifact:    fixture.registration.RuntimeArtifact,
		DependencyArtifact: fixture.registration.DependencyArtifact,
		ASR: scenarioconversation.ASRPlugin{
			Reference: scenarioconversation.ASRReference, Artifact: fixture.application.ASR.Artifact,
			Descriptor: fixture.application.ASR.Descriptor, Factory: fixture.registration.ASR[0].Factory,
		},
		Model: scenarioconversation.ModelPlugin{
			Reference: scenarioconversation.ModelReference, Artifact: fixture.application.Model.Artifact,
			Descriptor: fixture.application.Model.Descriptor, Factory: fixture.registration.Models[0].Factory,
		},
		TTS: scenarioconversation.TTSPlugin{
			Reference: scenarioconversation.TTSReference, Artifact: fixture.application.TTS.Artifact,
			Descriptor: fixture.application.TTS.Descriptor, Voice: fixture.application.TTS.Voice,
			Factory: fixture.registration.TTS[0].Factory,
		},
		Tools: fixture.application.Tools, Target: fixture.application.Target,
		Gate: perception.GateConfig{
			Threshold:         fixture.application.Gate.Threshold,
			PrefixPaddingMS:   fixture.application.Gate.PrefixPaddingMS,
			SilenceDurationMS: fixture.application.Gate.SilenceDurationMS,
			SpeechDurationMS:  fixture.application.Gate.SpeechDurationMS,
		},
		Media: fixture.application.Media, MaxOutputTokens: fixture.application.MaxOutputTokens,
	}
}

func assertScenarioFactoriesUnopened(t *testing.T, fixture scenarioProfileFixture) {
	t.Helper()
	if fixture.asrOpened.Load() != 0 || fixture.modelOpened.Load() != 0 || fixture.ttsOpened.Load() != 0 {
		t.Fatalf("resource-free scenario resolution opened factories: asr=%d model=%d tts=%d",
			fixture.asrOpened.Load(), fixture.modelOpened.Load(), fixture.ttsOpened.Load())
	}
}

func marshalScenarioApplication(t *testing.T, config scenarioconversation.ApplicationConfig) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mutateScenarioApplication(
	t *testing.T, source scenarioconversation.ApplicationConfig,
	mutate func(*scenarioconversation.ApplicationConfig),
) json.RawMessage {
	t.Helper()
	payload := marshalScenarioApplication(t, source)
	var copied scenarioconversation.ApplicationConfig
	if err := json.Unmarshal(payload, &copied); err != nil {
		t.Fatal(err)
	}
	mutate(&copied)
	return marshalScenarioApplication(t, copied)
}
