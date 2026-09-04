package realtimecu

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type realtimeCUTestSemanticDecider struct {
	descriptor policyelements.SemanticDeciderDescriptor
	closed     atomic.Int32
}

func (*realtimeCUTestSemanticDecider) Name() string { return "realtime-cu-test-settlement" }

func (decider *realtimeCUTestSemanticDecider) Descriptor() policyelements.SemanticDeciderDescriptor {
	return decider.descriptor
}

func (*realtimeCUTestSemanticDecider) Decide(
	context.Context, coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	return coreinteraction.Outcome{}, errors.New("test decider was not scripted")
}

func (decider *realtimeCUTestSemanticDecider) Close() error {
	decider.closed.Add(1)
	return nil
}

func TestSessionBundleSettlementPolicyIsLazyFreshAndUsesExactLifecycleMedia(t *testing.T) {
	config := validTestPluginConfig()
	var modelOpens, policyOpens atomic.Int32
	config.Model.Factory = func(context.Context, legacy.Options) (continuation.Provider, error) {
		modelOpens.Add(1)
		return nil, nil
	}
	var mu sync.Mutex
	var opened []*realtimeCUTestSemanticDecider
	config.SettlementPolicy.Factory = func(
		ctx context.Context, options legacy.Options,
	) (policyelements.SemanticDecider, error) {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		if options.SessionID != "settlement-policy-session" {
			return nil, errors.New("settlement policy received a different session")
		}
		policyOpens.Add(1)
		decider := &realtimeCUTestSemanticDecider{descriptor: config.SettlementPolicy.Descriptor}
		mu.Lock()
		opened = append(opened, decider)
		mu.Unlock()
		return decider, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	bundle, err := newSessionBundle(ctx, legacy.Options{SessionID: "settlement-policy-session"}, config)
	if err != nil {
		t.Fatal(err)
	}
	if modelOpens.Load() != 0 || policyOpens.Load() != 0 {
		t.Fatalf("bundle discovery opened model=%d policy=%d", modelOpens.Load(), policyOpens.Load())
	}

	service := bundle.services[policyelements.SemanticDeciderRegistryService]
	registry, ok := service.(*policyelements.SemanticDeciderRegistry)
	if !ok || registry == nil {
		t.Fatalf("semantic registry service has type %T", service)
	}
	descriptor, err := registry.Describe(SettlementPolicyReference)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor != config.SettlementPolicy.Descriptor || policyOpens.Load() != 0 {
		t.Fatalf("Describe descriptor=%+v opens=%d", descriptor, policyOpens.Load())
	}

	retained, err := bundle.media.Retain(trajectory.MediaRef{
		Handle: "result-linked-frame", MIMEType: "image/png", CapturedNS: 42,
	}, []byte("exact retained frame"))
	if err != nil {
		t.Fatal(err)
	}
	mediaService := bundle.services[cognitionelements.MediaResolverService]
	resolver, ok := mediaService.(continuation.MediaResolver)
	if !ok || resolver == nil || bundle.mediaResolver == nil {
		t.Fatalf("media resolver service does not expose the bundle resolver: %T", mediaService)
	}
	resolved, err := resolver(retained.Handle)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MIMEType != retained.MIMEType || !bytes.Equal(resolved.Bytes, []byte("exact retained frame")) {
		t.Fatalf("resolved media = %+v", resolved)
	}
	direct, err := bundle.mediaResolver(retained.Handle)
	if err != nil || direct.MIMEType != resolved.MIMEType || !bytes.Equal(direct.Bytes, resolved.Bytes) {
		t.Fatalf("bundle media resolver differs from service: media=%+v error=%v", direct, err)
	}
	resolved.Bytes[0] = 'X'
	again, err := resolver(retained.Handle)
	if err != nil || !bytes.Equal(again.Bytes, []byte("exact retained frame")) {
		t.Fatalf("resolver leaked mutable retained bytes: media=%+v error=%v", again, err)
	}

	firstValue, _, err := registry.Open(SettlementPolicyReference)
	if err != nil {
		t.Fatal(err)
	}
	secondValue, _, err := registry.Open(SettlementPolicyReference)
	if err != nil {
		t.Fatal(err)
	}
	first := firstValue.(*realtimeCUTestSemanticDecider)
	second := secondValue.(*realtimeCUTestSemanticDecider)
	if first == second || policyOpens.Load() != 2 || modelOpens.Load() != 0 {
		t.Fatalf("fresh policy ownership first=%p second=%p policy=%d model=%d",
			first, second, policyOpens.Load(), modelOpens.Load())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if first.closed.Load() != 1 || second.closed.Load() != 1 {
		t.Fatalf("caller-owned closes first=%d second=%d", first.closed.Load(), second.closed.Load())
	}

	cancel()
	if _, err := resolver(retained.Handle); !errors.Is(err, context.Canceled) {
		t.Fatalf("media resolver after lifecycle cancellation error = %v", err)
	}
	if _, _, err := registry.Open(SettlementPolicyReference); !errors.Is(err, context.Canceled) {
		t.Fatalf("semantic open after lifecycle cancellation error = %v", err)
	}
	if policyOpens.Load() != 2 {
		t.Fatalf("canceled lifecycle crossed policy factory boundary: %d", policyOpens.Load())
	}
}

func TestPluginSettlementDependencyUsesPolicyArtifactAndFingerprintsDescriptor(t *testing.T) {
	config := validTestPluginConfig()
	plugin, err := NewPlugin(config)
	if err != nil {
		t.Fatal(err)
	}
	policyDependency := findRealtimeCUTestDependency(
		t, plugin.AssemblyDependencies(), policyelements.SemanticDeciderRegistryService,
	)
	if policyDependency.Artifact != config.SettlementPolicy.Artifact {
		t.Fatalf("semantic dependency artifact = %+v, want %+v",
			policyDependency.Artifact, config.SettlementPolicy.Artifact)
	}
	foundMountPolicy := false
	for _, dependency := range plugin.MountDependencies() {
		if dependency.Name == policyelements.SemanticDeciderRegistryService {
			foundMountPolicy = true
			if dependency.Artifact != config.SettlementPolicy.Artifact {
				t.Fatalf("semantic mount dependency artifact = %+v, want %+v",
					dependency.Artifact, config.SettlementPolicy.Artifact)
			}
		}
	}
	if !foundMountPolicy {
		t.Fatal("semantic mount dependency is missing")
	}
	mediaDependency := findRealtimeCUTestDependency(
		t, plugin.AssemblyDependencies(), cognitionelements.MediaResolverService,
	)
	if mediaDependency.Artifact == config.SettlementPolicy.Artifact {
		t.Fatal("cross-service media dependency was mislabeled as the policy artifact")
	}

	changed := config
	changed.SettlementPolicy.Descriptor.ConfigurationDigest = "sha256:" + strings.Repeat("7", 64)
	changedPlugin, err := NewPlugin(changed)
	if err != nil {
		t.Fatal(err)
	}
	changedMedia := findRealtimeCUTestDependency(
		t, changedPlugin.AssemblyDependencies(), cognitionelements.MediaResolverService,
	)
	if changedMedia.Artifact == mediaDependency.Artifact {
		t.Fatal("composite dependency identity ignored settlement descriptor drift")
	}
	changedPolicy := findRealtimeCUTestDependency(
		t, changedPlugin.AssemblyDependencies(), policyelements.SemanticDeciderRegistryService,
	)
	if changedPolicy.Artifact != config.SettlementPolicy.Artifact {
		t.Fatalf("semantic dependency stopped exposing the selected policy artifact: %+v", changedPolicy.Artifact)
	}
}

func TestSessionBundleClosesDriftedSettlementClientAtRegistryBoundary(t *testing.T) {
	config := validTestPluginConfig()
	client := &realtimeCUTestSemanticDecider{descriptor: config.SettlementPolicy.Descriptor}
	client.descriptor.Model = "drifted-live-model"
	config.SettlementPolicy.Factory = func(
		context.Context, legacy.Options,
	) (policyelements.SemanticDecider, error) {
		return client, nil
	}
	bundle, err := newSessionBundle(
		context.Background(), legacy.Options{SessionID: "drifted-policy-session"}, config,
	)
	if err != nil {
		t.Fatal(err)
	}
	registry := bundle.services[policyelements.SemanticDeciderRegistryService].(*policyelements.SemanticDeciderRegistry)
	if _, _, err := registry.Open(SettlementPolicyReference); err == nil || !strings.Contains(err.Error(), "descriptor drifted") {
		t.Fatalf("drifted settlement client error = %v", err)
	}
	if client.closed.Load() != 1 {
		t.Fatalf("drifted settlement client closes = %d, want 1", client.closed.Load())
	}
}

func findRealtimeCUTestDependency(
	t *testing.T, dependencies []graphassembly.Dependency, name string,
) graphassembly.Dependency {
	t.Helper()
	for _, dependency := range dependencies {
		if dependency.Name == name {
			return dependency
		}
	}
	t.Fatalf("dependency %q is missing", name)
	return graphassembly.Dependency{}
}
