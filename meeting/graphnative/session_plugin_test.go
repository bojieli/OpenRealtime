package graphnative

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestSessionPluginConstructionIsResourceFreeAndDeclaresExactMountServices(t *testing.T) {
	artifacts := foregroundTestArtifacts()
	var foregroundCalls, visualCalls, backgroundCalls, ttsCalls atomic.Int32
	backgroundDescriptor := foregroundTestDescriptor()
	backgroundDescriptor.Phase = trajectory.PhaseSlow
	backgroundDescriptor.SpeechAuthority = continuation.SpeechAuthoritySilent
	foregroundCapabilities := foregroundTestCapabilities()
	foregroundCapabilities.Observers = []string{"screen", "audio", "meeting-notes"}
	config := SessionPluginConfig{
		AdapterArtifact: artifacts["adapter"],
		Foreground: ForegroundPlugin{
			Artifact: artifacts["foreground"], ProviderArtifact: artifacts["foreground-provider"],
			RuntimeArtifact:     artifacts["foreground-runtime"],
			WireAdapterArtifact: artifacts["foreground-wire"], BindingName: "meeting-fixture",
			Ownership: foregroundTestOwnership(), Capabilities: foregroundCapabilities,
			Descriptor: foregroundTestDescriptor(),
			Factory: func(context.Context, legacy.Options) (legacy.Binding, error) {
				foregroundCalls.Add(1)
				return nil, errors.New("foreground must stay lazy")
			},
		},
		Visual: VisualPlugin{
			Artifact: artifacts["visual"],
			Descriptor: perceptionelements.VisualProviderDescriptor{
				Name: "meeting-fixture-visual", Revision: "2026-08-30",
			},
			Factory: func(context.Context, legacy.Options) (perceptionelements.VisualProvider, error) {
				visualCalls.Add(1)
				return nil, errors.New("visual must stay lazy")
			},
		},
		Background: BackgroundPlugin{
			Artifact:   artifacts["background"],
			Descriptor: backgroundDescriptor,
			Factory: func(context.Context, legacy.Options) (continuation.Provider, error) {
				backgroundCalls.Add(1)
				return nil, errors.New("background must stay lazy")
			},
		},
		TTS: TTSPlugin{
			Artifact: artifacts["tts"], Descriptor: foregroundTestTTSDescriptor(),
			Factory: func(context.Context, legacy.Options) (v1.SpeechProvider, error) {
				ttsCalls.Add(1)
				return nil, errors.New("TTS must stay lazy")
			},
		},
	}
	plugin, err := NewSessionPlugin(config)
	if err != nil {
		t.Fatal(err)
	}
	if foregroundCalls.Load()+visualCalls.Load()+backgroundCalls.Load()+ttsCalls.Load() != 0 {
		t.Fatal("session plugin construction acquired a provider resource")
	}

	adapter := plugin.AdapterConfig()
	if adapter.Reference != SessionAdapterReference || adapter.Profile.Name != SessionProfileName ||
		adapter.Profile.Revision != SessionProfileRevision || adapter.Factory == nil {
		t.Fatalf("adapter contribution = %+v", adapter)
	}
	if got := adapter.Profile.AdditionalObservers; !slices.Equal(got, []string{"audio", "meeting-notes"}) {
		t.Fatalf("adapter additional observers = %v, want exact foreground selectors without built-in screen", got)
	}
	wantNames := []string{
		modelelements.DeploymentRegistryService, modelelements.PayloadCodecService,
		perceptionelements.VisualProviderRegistryService,
		cognitionelements.ProviderRegistryService, speechelements.TTSProviderRegistryService,
		stateelements.TrajectoryStoreService,
	}
	assembly := plugin.AssemblyDependencies()
	mounts := plugin.MountDependencies()
	if len(assembly) != len(wantNames) || len(mounts) != len(wantNames) {
		t.Fatalf("dependencies: assembly=%d mount=%d want=%d", len(assembly), len(mounts), len(wantNames))
	}
	for index, want := range wantNames {
		if assembly[index].Name != want || assembly[index].Scope != graphconfig.DependencyScopeMount ||
			mounts[index].Name != want || assembly[index].Artifact != mounts[index].Artifact ||
			mounts[index].Factory == nil {
			t.Fatalf("dependency %d: assembly=%+v mount=%+v", index, assembly[index], mounts[index])
		}
	}
	if foregroundCalls.Load()+visualCalls.Load()+backgroundCalls.Load()+ttsCalls.Load() != 0 {
		t.Fatal("dependency metadata inspection acquired a provider resource")
	}
	ttsIndex := slices.IndexFunc(mounts, func(candidate graphlaunch.MountDependencyPlugin) bool {
		return candidate.Name == speechelements.TTSProviderRegistryService
	})
	if ttsIndex < 0 {
		t.Fatal("session plugin omitted its TTS mount contribution")
	}
	prepared, err := mounts[ttsIndex].Factory(context.Background(), legacy.Options{
		SessionID: "meeting-tts-mount-laziness", Sink: foregroundTestClientSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].Name != speechelements.TTSProviderRegistryService ||
		prepared[0].Artifact != artifacts["tts"] {
		t.Fatalf("prepared TTS dependency = %+v", prepared)
	}
	if registry, ok := prepared[0].Service.(*speechelements.TTSProviderRegistry); !ok || registry == nil {
		t.Fatalf("prepared TTS service has type %T", prepared[0].Service)
	}
	if got := ttsCalls.Load(); got != 0 {
		t.Fatalf("TTS mount acquired provider resource %d time(s)", got)
	}
}

func TestMeetingStoreCoordinatorHandsOneExactStoreToAdapterAndCleansCanceledMount(t *testing.T) {
	coordinator := newMeetingStoreCoordinator()
	ctx, cancel := context.WithCancel(context.Background())
	store, err := coordinator.prepare(ctx, "meeting-session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.prepare(ctx, "meeting-session-1"); err == nil {
		t.Fatal("duplicate trajectory store preparation was accepted")
	}
	taken, err := coordinator.take("meeting-session-1")
	if err != nil {
		t.Fatal(err)
	}
	if taken != store {
		t.Fatal("adapter did not receive graph's exact trajectory store")
	}
	if _, err := coordinator.take("meeting-session-1"); err == nil {
		t.Fatal("trajectory store was claimed twice")
	}
	cancel()

	canceledCtx, canceled := context.WithCancel(context.Background())
	if _, err := coordinator.prepare(canceledCtx, "meeting-session-2"); err != nil {
		t.Fatal(err)
	}
	canceled()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		coordinator.mu.Lock()
		_, retained := coordinator.pending["meeting-session-2"]
		coordinator.mu.Unlock()
		if !retained {
			if _, err := coordinator.take("meeting-session-2"); err == nil {
				t.Fatal("canceled mount store remained claimable")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled mount retained an unclaimed trajectory store")
}

func TestSessionPluginClonesCallerOwnedCapabilities(t *testing.T) {
	plugin, _, _ := foregroundTestPlugin(t)
	before := plugin.AdapterConfig()
	before.Profile.AdditionalObservers[0] = "mutated"
	after := plugin.AdapterConfig()
	if slices.Contains(after.Profile.AdditionalObservers, "mutated") ||
		!slices.Equal(after.Profile.AdditionalObservers, []string{"meeting.foreground.asr"}) {
		t.Fatalf("caller mutated plugin adapter metadata: %+v", after.Profile.AdditionalObservers)
	}
}
