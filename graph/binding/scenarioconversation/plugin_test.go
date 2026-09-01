package scenarioconversation

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestPluginInventoryIsResourceFreeAndPinsProviderDependencies(t *testing.T) {
	architecture, err := projectarch.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	var asrOpened, policyOpened, modelOpened, ttsOpened atomic.Int32
	artifact := func(name string) inspect.ArtifactIdentity {
		return inspect.ArtifactIdentity{ID: "plugin://test/" + name, Revision: "build:1"}
	}
	config := PluginConfig{
		RuntimeArtifact: artifact("adapter"), DependencyArtifact: artifact("session-services"),
		Architecture: architecture,
		ASR: ASRPlugin{
			Reference: ASRReference, Artifact: artifact("asr"),
			Descriptor: v1.Descriptor{Name: "test-asr", Version: "1", Capabilities: v1.Capabilities{}},
			Factory: func(context.Context, legacy.Options) (v1.PerceptionProvider, error) {
				asrOpened.Add(1)
				return nil, nil
			},
		},
		Policy: PolicyPlugin{
			Reference: PolicyReference, Artifact: artifact("policy"),
			Descriptor: policyelements.SemanticDeciderDescriptor{
				Provider: "test", Model: "test-policy", Protocol: "test-enumerated", Revision: "1",
				ConfigurationDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				DecisionTimeoutMS:   1000,
			},
			Factory: func(context.Context, legacy.Options) (policyelements.SemanticDecider, error) {
				policyOpened.Add(1)
				return nil, nil
			},
		},
		Model: ModelPlugin{
			Reference: ModelReference, Artifact: artifact("model"),
			Descriptor: continuation.Descriptor{
				Provider: "test", Model: "test-model", Phase: trajectory.PhaseFast,
				Effort: continuation.EffortLow, Streaming: true,
				ToolAuthority:   continuation.ToolAuthorityPropose,
				SpeechAuthority: continuation.SpeechAuthorityVoice,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelOpened.Add(1)
				return nil, nil
			},
		},
		SilentModel: ModelPlugin{
			Reference: SilentModelReference, Artifact: artifact("model"),
			Descriptor: continuation.Descriptor{
				Provider: "test", Model: "test-model", Phase: trajectory.PhaseFast,
				Effort: continuation.EffortLow, Streaming: true,
				ToolAuthority:   continuation.ToolAuthorityPropose,
				SpeechAuthority: continuation.SpeechAuthoritySilent,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				modelOpened.Add(1)
				return nil, nil
			},
		},
		TTS: TTSPlugin{
			Reference: TTSReference, Artifact: artifact("tts"), Voice: "test-voice",
			Descriptor: v1.Descriptor{Name: "test-tts", Version: "1", Capabilities: v1.Capabilities{v1.CapabilityPCM16Output: true}},
			Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				ttsOpened.Add(1)
				return nil, nil
			},
		},
		Target: computeruse.Target{Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 720},
		Gate:   perception.DefaultGateConfig(), MaxOutputTokens: 1024,
	}
	plugin, err := NewPlugin(config)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.AdapterPlugin().Reference != AdapterReference || plugin.Selection().ProfileName != ProfileName {
		t.Fatalf("adapter inventory = %+v / %+v", plugin.AdapterPlugin(), plugin.Selection())
	}

	assembly := plugin.AssemblyDependencies()
	mount := plugin.MountDependencies()
	if len(assembly) != len(dependencyNames) || len(mount) != len(dependencyNames) {
		t.Fatalf("dependency inventory = %d/%d, want %d", len(assembly), len(mount), len(dependencyNames))
	}
	wantProvider := map[string]inspect.ArtifactIdentity{
		cognitionelements.ProviderRegistryService:     config.Model.Artifact,
		perceptionelements.ASRProviderRegistryService: config.ASR.Artifact,
		policyelements.SemanticDeciderRegistryService: config.Policy.Artifact,
		speechelements.TTSProviderRegistryService:     config.TTS.Artifact,
	}
	for index, dependency := range assembly {
		want := config.DependencyArtifact
		if provider, found := wantProvider[dependency.Name]; found {
			want = provider
		}
		if dependency.Artifact != want || mount[index].Name != dependency.Name || mount[index].Artifact != want {
			t.Fatalf("dependency %q artifact = %+v / %+v, want %+v",
				dependency.Name, dependency.Artifact, mount[index].Artifact, want)
		}
	}
	if asrOpened.Load() != 0 || policyOpened.Load() != 0 || modelOpened.Load() != 0 || ttsOpened.Load() != 0 {
		t.Fatalf("resource-free inventory opened providers: asr=%d policy=%d model=%d tts=%d",
			asrOpened.Load(), policyOpened.Load(), modelOpened.Load(), ttsOpened.Load())
	}

	if asrOpened.Load() != 0 || policyOpened.Load() != 0 || modelOpened.Load() != 0 || ttsOpened.Load() != 0 {
		t.Fatal("reading resource-free graph inventory opened a provider")
	}
}
