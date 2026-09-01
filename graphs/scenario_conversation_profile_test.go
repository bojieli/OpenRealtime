package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graphs"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestScenarioConversationApplicationProfileResolvesExactGraphWithoutResources(t *testing.T) {
	fixture := newScenarioProfileFixture(t)
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
	if previewConfig.Evidence.Graph != "scenario_conversation" || previewConfig.Evidence.Profiles == nil {
		t.Fatalf("scenario launch evidence = %+v", previewConfig.Evidence)
	}
	unsupported, err := projectarch.Default().Resolve("cascade.text-policy@3")
	if err != nil {
		t.Fatal(err)
	}
	pluginArchitectureTests := []struct {
		name   string
		mutate func(*scenarioconversation.PluginConfig)
		want   string
	}{
		{name: "missing", mutate: func(config *scenarioconversation.PluginConfig) {
			config.Architecture = projectarch.Definition{}
		}, want: "requires an exact architecture identity"},
		{name: "forged", mutate: func(config *scenarioconversation.PluginConfig) {
			config.Architecture.Summary += " forged"
		}, want: "fingerprint differs"},
		{name: "unsupported mode", mutate: func(config *scenarioconversation.PluginConfig) {
			config.Architecture = unsupported
		}, want: "not the exact composed semantic-policy controller"},
	}
	for _, test := range pluginArchitectureTests {
		t.Run("plugin architecture "+test.name, func(t *testing.T) {
			config := fixture.pluginConfig()
			test.mutate(&config)
			if _, err := graphs.ScenarioConversationLaunchConfig(config); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario plugin architecture error = %v, want %q", err, test.want)
			}
		})
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
	var semanticAdmission struct {
		StandingExtraction          bool    `json:"standing_extraction"`
		VerifyVoiceActivation       bool    `json:"verify_voice_activation"`
		VerifySilentAction          bool    `json:"verify_silent_action"`
		MinimumActivationConfidence float64 `json:"minimum_activation_confidence"`
		StandingMemory              int     `json:"standing_memory"`
	}
	if err := json.Unmarshal(values["semantic_admission"], &semanticAdmission); err != nil {
		t.Fatal(err)
	}
	if !semanticAdmission.StandingExtraction || !semanticAdmission.VerifyVoiceActivation ||
		!semanticAdmission.VerifySilentAction ||
		semanticAdmission.MinimumActivationConfidence != 0.7 || semanticAdmission.StandingMemory != 17 {
		t.Fatalf("selected semantic admission controls did not enter plan: %+v", semanticAdmission)
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
		{name: "architecture missing", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Architecture = legacy.ArchitectureIdentity{}
		}), want: "requires an exact architecture identity"},
		{name: "architecture fingerprint drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Architecture.Fingerprint = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		}), want: "fingerprint differs"},
		{name: "unsupported architecture mode", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			definition, resolveErr := projectarch.Default().Resolve("cascade.text-policy@3")
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			config.Architecture = definition.Identity()
		}), want: "not the exact composed semantic-policy controller"},
		{name: "ASR missing", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.ASR.Reference = "asr://test/uninstalled"
		}), want: "ASR registry is missing"},
		{name: "ASR artifact drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.ASR.Artifact.Revision = "build:drift"
		}), want: "artifact or descriptor drifted"},
		{name: "semantic policy missing", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Policy.Reference = "policy://test/uninstalled"
		}), want: "semantic policy registry is missing"},
		{name: "semantic policy descriptor drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Policy.Descriptor.Model = "drifted-policy"
		}), want: "artifact or descriptor drifted"},
		{name: "undeclared standing extraction", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Policy.Descriptor.StandingExtraction = false
		}), want: "does not declare it"},
		{name: "semantic activation confidence invalid", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.SemanticAdmission.MinimumActivationConfidence = 1.1
		}), want: "minimum_activation_confidence"},
		{name: "semantic standing memory invalid", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.SemanticAdmission.StandingMemory = 4097
		}), want: "standing_memory"},
		{name: "model descriptor drift", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Model.Descriptor.Model = "drifted"
		}), want: "artifact or descriptor drifted"},
		{name: "model authority escalation", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.Model.Descriptor.ToolAuthority = continuation.ToolAuthorityExecute
		}), want: "proposal-only"},
		{name: "silent model gains voice authority", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.SilentModel.Descriptor.SpeechAuthority = continuation.SpeechAuthorityVoice
		}), want: "silent-authoritative"},
		{name: "silent model uses another provider artifact", payload: mutateScenarioApplication(t, fixture.application, func(config *scenarioconversation.ApplicationConfig) {
			config.SilentModel.Artifact = inspect.ArtifactIdentity{
				ID: "plugin://test/scenario/other-model", Revision: "build:1",
			}
		}), want: "artifact or descriptor drifted"},
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
	fixture := newScenarioProfileFixture(t)
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

func TestScenarioConversationGraphOwnsPostCommitFifteenSecondSilenceWakeup(t *testing.T) {
	fixture := newScenarioProfileFixture(t)
	config, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	graph := preview.Plan.Graph()
	var timerFound bool
	for _, node := range graph.Nodes {
		if node.ID != "post_commit_silence" {
			continue
		}
		timerFound = node.Element.Name == "interaction.PostCommitSilence" &&
			node.ConfigSchema == "schema://openrealtime/interaction/post-commit-silence-config/v1"
	}
	if !timerFound {
		t.Fatal("scenario graph has no descriptor-locked post-commit silence timer")
	}
	for _, edge := range [][4]string{
		{"audio_commit_outcome_copy", "out", "post_commit_silence", "committed"},
		{"message_commit_outcome_copy", "out", "post_commit_silence", "committed"},
		{"trajectory_snapshot_copy", "out", "post_commit_silence", "context"},
		{"post_commit_silence", "create", "semantic_admission", "quiet"},
		{"semantic_admission", "voice_create", "voice_session_invocation", "create"},
		{"semantic_admission", "silent_create", "silent_session_invocation", "create"},
	} {
		if !scenarioGraphHasEdge(graph, edge[0], edge[1], edge[2], edge[3]) {
			t.Fatalf("scenario graph omits timer edge %s.%s -> %s.%s", edge[0], edge[1], edge[2], edge[3])
		}
	}
	if scenarioGraphHasEdge(graph,
		"message_commit_outcome_copy", "out", "semantic_admission", "committed") {
		t.Fatal("conversation.item.create bypasses explicit response.create through automatic semantic admission")
	}
	for name, endpoint := range map[string]ir.Endpoint{
		"response_create":             {Node: "semantic_admission", Port: "create"},
		"post_commit_silence_state":   {Node: "post_commit_silence", Port: "state"},
		"post_commit_silence_outcome": {Node: "post_commit_silence", Port: "outcome"},
	} {
		var found bool
		for _, boundary := range graph.Boundaries {
			if boundary.Name == name && boundary.Endpoint.Node == endpoint.Node &&
				boundary.Endpoint.Port == endpoint.Port {
				found = true
			}
		}
		if !found {
			t.Fatalf("scenario graph boundary %q does not bind %+v", name, endpoint)
		}
	}

	var values struct {
		Nodes map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(config.Artifacts.Values.Data, &values); err != nil {
		t.Fatal(err)
	}
	var timerConfig struct {
		DelayMS int `json:"delay_ms"`
	}
	if err := json.Unmarshal(values.Nodes["post_commit_silence"], &timerConfig); err != nil {
		t.Fatal(err)
	}
	if timerConfig.DelayMS != 15_000 {
		t.Fatalf("scenario post-commit silence delay = %dms, want 15000ms", timerConfig.DelayMS)
	}
	assertScenarioFactoriesUnopened(t, fixture)
}

func TestScenarioConversationGraphTerminatesSilentTextBeforeSpeech(t *testing.T) {
	fixture := newScenarioProfileFixture(t)
	config, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	graph := preview.Plan.Graph()
	for _, edge := range [][4]string{
		{"semantic_admission", "silent_committed", "silent_session_invocation", "committed"},
		{"semantic_admission", "silent_create", "silent_session_invocation", "create"},
		{"silent_session_invocation", "trigger", "silent_model", "trigger"},
		{"silent_model", "text", "silent_model_text_drop", "in"},
		{"voice_model", "text", "model_text_copy", "in"},
		{"voice_model_outcome_copy", "out", "segment", "terminal"},
	} {
		if !scenarioGraphHasEdge(graph, edge[0], edge[1], edge[2], edge[3]) {
			t.Fatalf("scenario graph omits authority edge %s.%s -> %s.%s",
				edge[0], edge[1], edge[2], edge[3])
		}
	}
	for _, forbidden := range [][4]string{
		{"silent_model", "text", "model_text_copy", "in"},
		{"silent_model", "text", "segment", "text"},
		{"silent_model", "outcome", "segment", "terminal"},
		{"message_commit_outcome_copy", "out", "semantic_admission", "committed"},
	} {
		if scenarioGraphHasEdge(graph, forbidden[0], forbidden[1], forbidden[2], forbidden[3]) {
			t.Fatalf("silent/text authority escaped through %s.%s -> %s.%s",
				forbidden[0], forbidden[1], forbidden[2], forbidden[3])
		}
	}
	for _, node := range graph.Nodes {
		if node.ID == "silent_model_text_drop" && node.Element.Name == "flow.Drop" {
			assertScenarioFactoriesUnopened(t, fixture)
			return
		}
	}
	t.Fatal("scenario graph has no descriptor-locked terminal drop for silent model text")
}

func scenarioGraphHasEdge(graph ir.Graph, fromNode, fromPort, toNode, toPort string) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func TestScenarioConversationLaunchRequiresExactSharedTrajectoryStoreSelection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*graphlaunch.Config, scenarioProfileFixture)
		want   string
	}{
		{
			name: "missing selection",
			mutate: func(config *graphlaunch.Config, _ scenarioProfileFixture) {
				config.PlanOptions.OptionalDependencies = []string{
					cognitionelements.MediaResolverService,
				}
			},
			want: stateelements.TrajectoryStoreService,
		},
		{
			name: "mismatched artifact",
			mutate: func(config *graphlaunch.Config, fixture scenarioProfileFixture) {
				drifted := fixture.registration.DependencyArtifact
				drifted.Revision = "build:trajectory-store-drift"
				for index := range config.Catalog.Assembly.Dependencies {
					if config.Catalog.Assembly.Dependencies[index].Name == stateelements.TrajectoryStoreService {
						config.Catalog.Assembly.Dependencies[index].Artifact = drifted
					}
				}
				for index := range config.Catalog.MountDependencies {
					if config.Catalog.MountDependencies[index].Name == stateelements.TrajectoryStoreService {
						config.Catalog.MountDependencies[index].Artifact = drifted
					}
				}
			},
			want: "selection drifted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScenarioProfileFixture(t)
			config, err := graphs.ScenarioConversationLaunchConfig(fixture.pluginConfig())
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&config, fixture)
			if _, err := graphlaunch.New(context.Background(), config); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario trajectory-store selection error = %v, want %q", err, test.want)
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
	architecture    projectarch.Definition
	registration    scenarioconversation.ApplicationRegistrationConfig
	gatewayArtifact inspect.ArtifactIdentity
	asrOpened       *atomic.Int32
	modelOpened     *atomic.Int32
	ttsOpened       *atomic.Int32
}

func newScenarioProfileFixture(t testing.TB) scenarioProfileFixture {
	t.Helper()
	architecture, err := projectarch.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
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
	silentModelSelection := modelSelection
	silentModelSelection.Reference = "model://test/scenario/silent/v1"
	silentModelSelection.Descriptor.SpeechAuthority = continuation.SpeechAuthoritySilent
	policySelection := scenarioconversation.ApplicationPolicySelection{
		Reference: "policy://test/scenario/v1", Artifact: artifact("policy"),
		Descriptor: testScenarioSemanticDescriptor(),
	}
	ttsSelection := scenarioconversation.ApplicationTTSSelection{
		Reference: "tts://test/scenario/v1", Artifact: artifact("tts"), Voice: "scenario-voice",
		Descriptor: v1.Descriptor{Name: "scenario-tts", Version: "1", Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
		}},
	}
	application := scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		Architecture:  architecture.Identity(),
		ASR:           asrSelection, Policy: policySelection, Model: modelSelection,
		SemanticAdmission: scenarioconversation.SemanticAdmissionSelection{
			StandingExtraction: true, VerifyVoiceActivation: true, VerifySilentAction: true,
			MinimumActivationConfidence: 0.7, StandingMemory: 17,
		},
		SilentModel: silentModelSelection, TTS: ttsSelection,
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
		Policies: []scenarioconversation.PolicyFactoryRegistration{{
			ApplicationPolicySelection: policySelection,
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				return testScenarioSemanticDecider{}, nil
			},
		}},
		Models: []scenarioconversation.ModelFactoryRegistration{{
			ApplicationModelSelection: modelSelection,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelOpened.Add(1)
				return nil, nil
			},
		}, {
			ApplicationModelSelection: silentModelSelection,
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
		application: application, architecture: architecture,
		registration: registration, gatewayArtifact: artifact("gateway"),
		asrOpened: asrOpened, modelOpened: modelOpened, ttsOpened: ttsOpened,
	}
}

func (fixture scenarioProfileFixture) pluginConfig() scenarioconversation.PluginConfig {
	return scenarioconversation.PluginConfig{
		RuntimeArtifact:    fixture.registration.RuntimeArtifact,
		DependencyArtifact: fixture.registration.DependencyArtifact,
		Architecture:       fixture.architecture,
		ASR: scenarioconversation.ASRPlugin{
			Reference: scenarioconversation.ASRReference, Artifact: fixture.application.ASR.Artifact,
			Descriptor: fixture.application.ASR.Descriptor, Factory: fixture.registration.ASR[0].Factory,
		},
		Policy: scenarioconversation.PolicyPlugin{
			Reference: scenarioconversation.PolicyReference, Artifact: fixture.application.Policy.Artifact,
			Descriptor: fixture.application.Policy.Descriptor, Factory: fixture.registration.Policies[0].Factory,
		},
		SemanticAdmission: fixture.application.SemanticAdmission,
		Model: scenarioconversation.ModelPlugin{
			Reference: scenarioconversation.ModelReference, Artifact: fixture.application.Model.Artifact,
			Descriptor: fixture.application.Model.Descriptor, Factory: fixture.registration.Models[0].Factory,
		},
		SilentModel: scenarioconversation.ModelPlugin{
			Reference: scenarioconversation.SilentModelReference, Artifact: fixture.application.SilentModel.Artifact,
			Descriptor: fixture.application.SilentModel.Descriptor, Factory: fixture.registration.Models[1].Factory,
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

type testScenarioSemanticDecider struct{}

func (testScenarioSemanticDecider) Name() string { return "test-scenario-semantic" }
func (testScenarioSemanticDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return testScenarioSemanticDescriptor()
}
func (testScenarioSemanticDecider) Decide(
	_ context.Context, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	for index, option := range decision.Options {
		if option == string(coreinteraction.ActAnswer) {
			return coreinteraction.Outcome{Index: index, Option: option}, nil
		}
	}
	return coreinteraction.Outcome{}, errors.New("answer act is unavailable")
}

func (testScenarioSemanticDecider) Generate(
	context.Context, string, string, int,
) (string, error) {
	return "none", nil
}

func testScenarioSemanticDescriptor() policyelements.SemanticDeciderDescriptor {
	return policyelements.SemanticDeciderDescriptor{
		Provider: "test", Model: "scenario-policy", Protocol: "test-enumerated",
		Revision: "1", ConfigurationDigest: "sha256:" + strings.Repeat("0", 64),
		DecisionTimeoutMS: 1000, StandingExtraction: true,
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
