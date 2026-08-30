package realtimecu

import (
	"context"
	"strings"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestPluginValidationRejectsAuthorityAndTargetWideningWithoutOpeningFactories(t *testing.T) {
	base := validTestPluginConfig()
	var modelOpened, observerOpened bool
	base.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelOpened = true
		return nil, nil
	}
	base.Observer.Factory = func(context.Context, legacy.Options) (Observer, error) {
		observerOpened = true
		return nil, nil
	}
	if _, err := NewPlugin(base); err != nil {
		t.Fatal(err)
	}
	if modelOpened || observerOpened {
		t.Fatal("plugin validation acquired a provider resource")
	}

	cases := []struct {
		name   string
		mutate func(*PluginConfig)
		want   string
	}{
		{name: "executable model", want: "proposal-only", mutate: func(config *PluginConfig) {
			config.Model.Descriptor.ToolAuthority = continuation.ToolAuthorityExecute
		}},
		{name: "voiced model", want: "silent", mutate: func(config *PluginConfig) {
			config.Model.Descriptor.SpeechAuthority = continuation.SpeechAuthorityVoice
		}},
		{name: "missing camera", want: "camera", mutate: func(config *PluginConfig) {
			config.Observer.Sources = []string{SourceMicrophone, SourceScreen}
		}},
		{name: "duplicate observer source", want: "camera", mutate: func(config *PluginConfig) {
			config.Observer.Sources = []string{SourceCamera, SourceMicrophone, SourceScreen, SourceScreen}
		}},
		{name: "noncanonical observer source", want: "canonical", mutate: func(config *PluginConfig) {
			config.Observer.Sources = []string{SourceCamera, " microphone", SourceScreen}
		}},
		{name: "camera action target", want: "only screen", mutate: func(config *PluginConfig) {
			config.Target.Sources = []string{SourceScreen, SourceCamera}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			config.Observer.Sources = append([]string(nil), base.Observer.Sources...)
			config.Target.Sources = append([]string(nil), base.Target.Sources...)
			testCase.mutate(&config)
			if _, err := NewPlugin(config); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("NewPlugin error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestPluginValidationRequiresExactlyOneObserverFactoryMode(t *testing.T) {
	base := validTestPluginConfig()
	for _, testCase := range []struct {
		name   string
		mutate func(*PluginConfig)
	}{
		{name: "no factory", mutate: func(config *PluginConfig) {
			config.Observer.Factory = nil
		}},
		{name: "both factories", mutate: func(config *PluginConfig) {
			config.Observer.ResourceFactory = func(
				context.Context, legacy.Options, ObserverResources,
			) (Observer, error) {
				return nil, nil
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := base
			testCase.mutate(&config)
			if _, err := NewPlugin(config); err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("NewPlugin error = %v, want exactly-one factory failure", err)
			}
		})
	}

	resourceAware := base
	resourceAware.Observer.Factory = nil
	resourceAware.Observer.ResourceFactory = func(
		context.Context, legacy.Options, ObserverResources,
	) (Observer, error) {
		return nil, nil
	}
	if _, err := NewPlugin(resourceAware); err != nil {
		t.Fatalf("resource-aware observer plugin failed validation: %v", err)
	}
}

func validTestPluginConfig() PluginConfig {
	return PluginConfig{
		RuntimeArtifact: testArtifact("runtime", "a"),
		Model: ModelPlugin{
			Reference: "go://test/realtime-cu/model/v1",
			Artifact:  testArtifact("model", "b"),
			Descriptor: continuation.Descriptor{
				Provider: "test", Model: "realtime-cu", Phase: trajectory.PhaseFast,
				Effort: continuation.EffortMinimal, Streaming: true,
				ToolAuthority:   continuation.ToolAuthorityPropose,
				SpeechAuthority: continuation.SpeechAuthoritySilent,
			},
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) { return nil, nil },
		},
		Observer: ObserverPlugin{
			Reference: "go://test/realtime-cu/observer/v1", Name: "test-observer",
			Artifact: testArtifact("observer", "c"),
			Sources:  []string{SourceCamera, SourceMicrophone, SourceScreen},
			Factory:  func(context.Context, legacy.Options) (Observer, error) { return nil, nil },
		},
		Target: testTarget(),
	}
}

func testArtifact(name, digit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: "artifact://test/realtime-cu/" + name, Revision: "v1",
		Digest: "sha256:" + strings.Repeat(digit, 64),
	}
}
