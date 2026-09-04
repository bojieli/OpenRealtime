package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/gateway"
	realtimecubinding "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/policymodel"
)

func TestFreezeProductionRealtimeCUProfilePublishesExactInspectionCompanions(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "realtime-cu-campaign")
	options := defaultRealtimeCUProfileOptions()
	options.out = filepath.Join(directory, "realtime-cu.profile.yaml")
	options.graphOut = filepath.Join(directory, "realtime-cu.graph.json")
	options.valuesOut = filepath.Join(directory, "realtime-cu.values.json")
	options.resolutionOut = filepath.Join(directory, "realtime-cu.resolution.json")
	options.executionOut = filepath.Join(directory, "realtime-cu.execution.json")
	options.operatorCapabilityEnv = "OPENREALTIME_REALTIME_CU_OPERATOR_CAPABILITY"
	configureRealtimeCUProfileTestDeployments(&options)
	t.Setenv(options.tokenEnv, "realtime-cu-profile-test-token")
	if err := validateRealtimeCUProfileOptions(options); err != nil {
		t.Fatal(err)
	}
	executable := inspect.ArtifactIdentity{
		ID:     "go://test/openrealtime/realtime-cu-profile-host/v1",
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	frozen, err := freezeProductionRealtimeCUProfile(context.Background(), options, executable)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Profile.Application.Selection.Reference != realtimecubinding.ApplicationReference ||
		frozen.Profile.Adapter.Reference != realtimecubinding.AdapterReference ||
		frozen.Profile.Plan != frozen.Plan.Identity() ||
		frozen.Execution.Kind != bench.ExecutionGraphNative ||
		frozen.Profile.Server.InspectionTokenTTLMS < uint64((5*time.Minute)/time.Millisecond) ||
		frozen.Profile.Server.TokenEnvironment != options.tokenEnv ||
		frozen.Profile.Server.OperatorCapabilityEnvironment != options.operatorCapabilityEnv {
		t.Fatalf("frozen Realtime-CU profile = %+v execution=%+v",
			frozen.Profile, frozen.Execution)
	}
	if frozen.Resolution.Deployment == nil ||
		frozen.Resolution.Deployment.PrivateDeploymentFingerprint == "" {
		t.Fatalf("frozen Realtime-CU resolution lacks exact deployment evidence: %+v",
			frozen.Resolution.Deployment)
	}
	activationResolution := realtimeCUResolutionElement(t, frozen.Resolution, "activation")
	if activationResolution.Runtime.ID !=
		"go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/activation/v15" ||
		activationResolution.Runtime.Revision != "implementation:15" {
		t.Fatalf("frozen activation runtime = %+v", activationResolution.Runtime)
	}
	coordinatorResolution := realtimeCUResolutionElement(t, frozen.Resolution, "cancellation_coordinator")
	if coordinatorResolution.Runtime.ID !=
		"go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/session-cancellation-coordinator/v4" ||
		coordinatorResolution.Runtime.Revision != "implementation:4" {
		t.Fatalf("frozen cancellation coordinator runtime = %+v", coordinatorResolution.Runtime)
	}
	settlementResolution := realtimeCUResolutionElement(t, frozen.Resolution, "settlement")
	if settlementResolution.Runtime.ID !=
		"builtin://openrealtime/elements/policy.IntentSettlement" ||
		settlementResolution.Runtime.Revision != "implementation:2" {
		t.Fatalf("frozen settlement runtime = %+v", settlementResolution.Runtime)
	}
	modelResolution := realtimeCUResolutionElement(t, frozen.Resolution, "model")
	if len(modelResolution.Capabilities) == 0 {
		t.Fatalf("frozen model resolution omitted live capabilities: %+v", modelResolution)
	}
	application, err := realtimecubinding.DecodeApplicationConfig(
		frozen.Profile.Application.Configuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if application.Target.Width != 1280 || application.Target.Height != 577 ||
		application.Target.Name != "benchmark-browser" ||
		application.Model.Artifact != options.deployments.Model {
		t.Fatalf("frozen Realtime-CU browser target = %+v", application.Target)
	}
	var output bytes.Buffer
	if err := writeFrozenRealtimeCUProfile(&output, options, frozen); err != nil {
		t.Fatal(err)
	}
	profilePayload, err := os.ReadFile(options.out)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := launchprofile.ParseYAML(options.out, profilePayload)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Fingerprint != frozen.Profile.Fingerprint {
		t.Fatal("retained launch profile changed identity")
	}
	graphPayload, err := os.ReadFile(options.graphOut)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Parse(graphPayload)
	if err != nil {
		t.Fatal(err)
	}
	valuesPayload, err := os.ReadFile(options.valuesOut)
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseJSON(options.valuesOut, valuesPayload)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(graph, values)
	if err != nil {
		t.Fatal(err)
	}
	requirement, err := bench.ReadExecutionRequirement(options.executionOut)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := bench.ReadExpectedResolution(options.resolutionOut)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := bench.RequireGraph(graph, requirement.Graph.Configuration, resolution)
	if err != nil {
		t.Fatal(err)
	}
	requirementPayload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	reconstructedPayload, err := bench.MarshalExecutionRequirement(reconstructed)
	if err != nil {
		t.Fatal(err)
	}
	if requirement.Graph.Graph.Fingerprint != bound.Graph.Fingerprint ||
		!bytes.Equal(reconstructedPayload, requirementPayload) {
		t.Fatal("profile companions did not reconstruct one reviewed execution requirement")
	}
	if err := writeFrozenRealtimeCUProfile(&output, options, frozen); err != nil {
		t.Fatalf("exact crash-recovery adoption failed: %v", err)
	}
	if err := os.WriteFile(options.valuesOut, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFrozenRealtimeCUProfile(&output, options, frozen); err == nil {
		t.Fatal("Realtime-CU profile publication adopted a drifted campaign")
	}
}

func realtimeCUResolutionElement(
	t *testing.T, resolution bench.LiveResolution, node string,
) bench.ElementResolution {
	t.Helper()
	for _, element := range resolution.Elements {
		if element.Node == node {
			return element
		}
	}
	t.Fatalf("Realtime-CU resolution omitted node %q", node)
	return bench.ElementResolution{}
}

func TestProbeRealtimeCUExpectedResolutionClosesEveryFailurePath(t *testing.T) {
	binding, plan := realtimeCUProfileProbeFixture(t)
	closeFailure := errors.New("injected profile probe close failure")
	tests := []struct {
		name              string
		withoutInspection bool
		forceNotReady     bool
		closeErr          error
		context           func() (context.Context, context.CancelFunc)
		want              error
	}{
		{
			name: "runtime without inspection", withoutInspection: true,
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
		{
			name: "readiness deadline", forceNotReady: true,
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
		{
			name: "terminal close failure", closeErr: closeFailure,
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 5*time.Second)
			},
			want: closeFailure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := &realtimeCUProfileProbeBindingFixture{
				SessionBinding: binding, withoutInspection: test.withoutInspection,
				forceNotReady: test.forceNotReady, closeErr: test.closeErr,
				started: make(chan realtimeCUProfileProbeClosedRuntime, 1),
			}
			ctx, cancel := test.context()
			defer cancel()
			resolution, err := probeRealtimeCUExpectedResolution(ctx, fixture, plan)
			if err == nil {
				t.Fatal("failed profile probe returned no error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("profile probe error = %v, want %v", err, test.want)
			}
			if len(resolution.Elements) != 0 || resolution.Deployment != nil {
				t.Fatalf("failed profile probe returned a usable resolution: %+v", resolution)
			}
			runtime := <-fixture.started
			if !runtime.probeClosed() {
				t.Fatal("failed profile probe did not close its started runtime")
			}
		})
	}
}

func realtimeCUProfileProbeFixture(t *testing.T) (gateway.SessionBinding, *graphconfig.Plan) {
	t.Helper()
	deployments := realtimeCUProfileTestDeployments()
	verifier := &fixtureRealtimeCUDeploymentVerifier{identity: deployments}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	options := defaultRealtimeCUProfileOptions()
	options.deployments = deployments
	options.deploymentVerifier = verifier
	t.Setenv(options.tokenEnv, "realtime-cu-profile-probe-test-token")
	frozen, err := freezeProductionRealtimeCUProfile(
		context.Background(), options, artifacts.Gateway,
	)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newServeRealtimeCURegistration(
		context.Background(), artifacts.Gateway, deployments, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	config, err := registration.Application.Factory(
		context.Background(), frozen.Profile.Application.Configuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := graphlaunch.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return prepared.Binding, prepared.Plan
}

type realtimeCUProfileProbeClosedRuntime interface {
	legacy.Runtime
	probeClosed() bool
}

type realtimeCUProfileProbeBindingFixture struct {
	gateway.SessionBinding
	withoutInspection bool
	forceNotReady     bool
	closeErr          error
	started           chan realtimeCUProfileProbeClosedRuntime
}

func (fixture *realtimeCUProfileProbeBindingFixture) Start(
	_ context.Context, options legacy.Options,
) (legacy.Runtime, error) {
	runtime, err := fixture.SessionBinding.Start(context.Background(), options)
	if err != nil {
		return nil, err
	}
	if fixture.withoutInspection {
		tracked := &realtimeCUProfileProbeUninspectedRuntime{Runtime: runtime}
		fixture.started <- tracked
		return tracked, nil
	}
	inspected, ok := runtime.(realtimeCUProfileProbeRuntime)
	if !ok {
		_ = runtime.Close(context.Background(), errors.New("test fixture lacks inspection"))
		return nil, errors.New("test fixture runtime lacks inspection")
	}
	tracked := &realtimeCUProfileProbeInspectedRuntime{
		Runtime: runtime, inspected: inspected,
		forceNotReady: fixture.forceNotReady, closeErr: fixture.closeErr,
	}
	fixture.started <- tracked
	return tracked, nil
}

type realtimeCUProfileProbeUninspectedRuntime struct {
	legacy.Runtime
	closed atomic.Bool
}

func (runtime *realtimeCUProfileProbeUninspectedRuntime) Close(
	ctx context.Context, cause error,
) error {
	runtime.closed.Store(true)
	return runtime.Runtime.Close(ctx, cause)
}

func (runtime *realtimeCUProfileProbeUninspectedRuntime) probeClosed() bool {
	return runtime.closed.Load()
}

type realtimeCUProfileProbeInspectedRuntime struct {
	legacy.Runtime
	inspected     realtimeCUProfileProbeRuntime
	forceNotReady bool
	closeErr      error
	closed        atomic.Bool
}

func (runtime *realtimeCUProfileProbeInspectedRuntime) Live() inspect.Live {
	if runtime.forceNotReady {
		return inspect.Live{}
	}
	return runtime.inspected.Live()
}

func (runtime *realtimeCUProfileProbeInspectedRuntime) Done() <-chan struct{} {
	return runtime.inspected.Done()
}

func (runtime *realtimeCUProfileProbeInspectedRuntime) Close(
	ctx context.Context, cause error,
) error {
	runtime.closed.Store(true)
	return errors.Join(runtime.Runtime.Close(ctx, cause), runtime.closeErr)
}

func (runtime *realtimeCUProfileProbeInspectedRuntime) probeClosed() bool {
	return runtime.closed.Load()
}

func TestValidateRealtimeCUProfileOptionsRequiresDistinctCompleteOutputs(t *testing.T) {
	options := defaultRealtimeCUProfileOptions()
	options.out = "/tmp/realtime-cu-campaign/profile"
	options.graphOut = "/tmp/realtime-cu-campaign/graph"
	options.valuesOut = "/tmp/realtime-cu-campaign/values"
	options.resolutionOut = "/tmp/realtime-cu-campaign/resolution"
	options.executionOut = options.graphOut
	configureRealtimeCUProfileTestDeployments(&options)
	if err := validateRealtimeCUProfileOptions(options); err == nil {
		t.Fatal("profile options accepted duplicate output paths")
	}
}

func TestValidateRealtimeCUProfileOptionsRequiresGatewayAuthentication(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "campaign")
	options := defaultRealtimeCUProfileOptions()
	options.out = filepath.Join(directory, "profile")
	options.graphOut = filepath.Join(directory, "graph")
	options.valuesOut = filepath.Join(directory, "values")
	options.resolutionOut = filepath.Join(directory, "resolution")
	options.executionOut = filepath.Join(directory, "execution")
	configureRealtimeCUProfileTestDeployments(&options)
	options.tokenEnv = ""
	if err := validateRealtimeCUProfileOptions(options); err == nil ||
		!strings.Contains(err.Error(), "invalid identity") {
		t.Fatalf("unauthenticated Realtime-CU profile error = %v", err)
	}
}

func TestRealtimeCUProfileCampaignFailureIsInvisibleAndRetryable(t *testing.T) {
	for failAfter := 1; failAfter <= 5; failAfter++ {
		t.Run(fmt.Sprint(failAfter), func(t *testing.T) {
			campaign := filepath.Join(t.TempDir(), "campaign")
			options := realtimeCUProfileOptions{out: filepath.Join(campaign, "profile")}
			artifacts := []struct {
				label   string
				path    string
				payload []byte
			}{
				{"one", filepath.Join(campaign, "one"), []byte("one")},
				{"two", filepath.Join(campaign, "two"), []byte("two")},
				{"three", filepath.Join(campaign, "three"), []byte("three")},
				{"four", filepath.Join(campaign, "four"), []byte("four")},
				{"profile", filepath.Join(campaign, "profile"), []byte("profile")},
			}
			injected := errors.New("injected publication failure")
			err := publishRealtimeCUProfileCampaign(options, artifacts, func(completed int) error {
				if completed == failAfter {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatalf("publication error = %v", err)
			}
			if _, err := os.Lstat(campaign); !os.IsNotExist(err) {
				t.Fatalf("partial campaign became visible: %v", err)
			}
			if err := publishRealtimeCUProfileCampaign(options, artifacts, nil); err != nil {
				t.Fatalf("retry publication: %v", err)
			}
		})
	}
}

func TestRealtimeCUProfileConcurrentPublicationNeverReplacesCampaign(t *testing.T) {
	parent := t.TempDir()
	campaign := filepath.Join(parent, "realtime-cu-profile")
	options := realtimeCUProfileOptions{out: filepath.Join(campaign, "profile")}
	artifacts := []struct {
		label   string
		path    string
		payload []byte
	}{
		{label: "one", path: filepath.Join(campaign, "one"), payload: []byte("one")},
		{label: "two", path: filepath.Join(campaign, "two"), payload: []byte("two")},
	}
	const publishers = 8
	started := make(chan struct{})
	results := make(chan error, publishers)
	var ready sync.WaitGroup
	ready.Add(publishers)
	for index := 0; index < publishers; index++ {
		go func() {
			ready.Done()
			<-started
			results <- publishRealtimeCUProfileCampaign(options, artifacts, nil)
		}()
	}
	ready.Wait()
	close(started)
	succeeded := 0
	for index := 0; index < publishers; index++ {
		if err := <-results; err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatal("no concurrent Realtime-CU profile publisher succeeded")
	}
	if err := publishRealtimeCUProfileCampaign(options, artifacts, nil); err != nil {
		t.Fatalf("reopen concurrently published Realtime-CU profile: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(campaign) {
		t.Fatalf("concurrent Realtime-CU publication debris = %+v", entries)
	}
}

func TestProductionProfileHostResolvesFrozenRealtimeCUApplicationWithoutResources(t *testing.T) {
	deployments := realtimeCUProfileTestDeployments()
	verifier := &fixtureRealtimeCUDeploymentVerifier{identity: deployments}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	providers, err := newServeScenarioProviders(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	host, err := newServeProfileHost(artifacts, providers)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newServeRealtimeCURegistration(
		context.Background(), artifacts.Gateway, deployments, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	host.RealtimeCU = &registration
	host.Applications, err = launchprofile.NewRegistry([]launchprofile.Registration{
		host.Delegate, host.ScenarioSuite, registration.Application,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := defaultRealtimeCUProfileOptions()
	options.deployments = deployments
	options.deploymentVerifier = verifier
	t.Setenv(options.tokenEnv, "realtime-cu-host-test-token")
	frozen, err := freezeProductionRealtimeCUProfile(context.Background(), options, artifacts.Gateway)
	if err != nil {
		t.Fatal(err)
	}
	composition, err := newProfiledServeComposition(context.Background(), frozen.Profile, host, nil)
	if err != nil {
		t.Fatal(err)
	}
	if composition.Graph == nil || composition.Graph.GraphPlan == nil ||
		composition.Graph.GraphPlan.Identity() != frozen.Profile.Plan ||
		host.RealtimeCU == nil || host.RealtimeCU.Application.Reference != realtimecubinding.ApplicationReference {
		t.Fatal("production profile host did not resolve the exact frozen Realtime-CU application")
	}
}

func TestServeRealtimeCURegistrationBindsLazyFreshSettlementPolicyAndSessionMedia(t *testing.T) {
	deployments := realtimeCUProfileTestDeployments()
	verifier := &fixtureRealtimeCUDeploymentVerifier{identity: deployments}
	executable := inspect.ArtifactIdentity{
		ID:     "go://test/openrealtime/realtime-cu-settlement-host/v1",
		Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
	secret := "local-model-secret-must-never-enter-the-profile"
	t.Setenv(realtimeCULocalModelKeyEnvironment, secret)
	selected, err := newServeRealtimeCURegistration(
		context.Background(), executable, deployments, verifier,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.combinedChecks.Load() != 1 || verifier.modelChecks.Load() != 0 ||
		verifier.observerChecks.Load() != 0 {
		t.Fatalf("registration verification combined=%d model=%d observer=%d",
			verifier.combinedChecks.Load(), verifier.modelChecks.Load(), verifier.observerChecks.Load())
	}
	if selected.Policy.Reference != realtimeCULocalPolicyReference ||
		selected.Policy.Artifact.ID != "profile://openrealtime/realtime-cu/local-settlement-policy" ||
		selected.Policy.Artifact == deployments.Model || !selected.Policy.Descriptor.Vision ||
		selected.Policy.Descriptor.ConfigurationDigest == "" {
		t.Fatalf("selected settlement policy = %+v", selected.Policy)
	}
	if bytes.Contains(selected.Policy.Configuration, []byte(secret)) ||
		bytes.Contains(selected.Policy.Configuration, []byte(`"api_key"`)) {
		t.Fatalf("settlement policy configuration contains credential material: %s",
			selected.Policy.Configuration)
	}
	policyConfig, described, err := decodeServePolicyConfiguration(
		realtimeCULocalModelProvider, selected.Policy.Configuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if policyConfig.Model != realtimeCULocalModelName ||
		policyConfig.BaseURL != realtimeCULocalModelURL || policyConfig.RequestTimeoutMS != 2_000 ||
		policyConfig.Vision == nil || !*policyConfig.Vision ||
		policyConfig.GuidedChoice == nil || !*policyConfig.GuidedChoice ||
		policyConfig.TokenEnvironment != "" || described != selected.Policy.Descriptor {
		t.Fatalf("serialized settlement configuration=%+v descriptor=%+v", policyConfig, described)
	}

	applicationPayload, err := json.Marshal(realtimecubinding.ApplicationConfig{
		FormatVersion: realtimecubinding.ApplicationFormatVersion,
		Model:         selected.Model, SettlementPolicy: selected.Policy,
		Observer: selected.Observer,
		Target: computeruse.Target{
			Name: "settlement-test-browser", Sources: []string{realtimecubinding.SourceScreen},
			Width: 1280, Height: 577,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	launchConfig, err := selected.Application.Factory(context.Background(), applicationPayload)
	if err != nil {
		t.Fatal(err)
	}
	if verifier.modelChecks.Load() != 0 {
		t.Fatalf("application resolution opened or readied settlement policy %d times",
			verifier.modelChecks.Load())
	}

	policyDependency := realtimeCUProfileMountDependency(
		t, launchConfig, policyelements.SemanticDeciderRegistryService,
	)
	if policyDependency.Artifact != selected.Policy.Artifact {
		t.Fatalf("semantic registry artifact=%+v, want %+v",
			policyDependency.Artifact, selected.Policy.Artifact)
	}
	mediaDependency := realtimeCUProfileMountDependency(
		t, launchConfig, cognitionelements.MediaResolverService,
	)
	sessionContext, cancelSession := context.WithCancel(context.Background())
	options := legacy.Options{SessionID: "realtime-cu-local-settlement-test"}
	policyServices, err := policyDependency.Factory(sessionContext, options)
	if err != nil {
		cancelSession()
		t.Fatal(err)
	}
	mediaServices, err := mediaDependency.Factory(sessionContext, options)
	if err != nil {
		cancelSession()
		t.Fatal(err)
	}
	if len(policyServices) != 1 || len(mediaServices) != 1 {
		cancelSession()
		t.Fatalf("mount contributions policy=%d media=%d", len(policyServices), len(mediaServices))
	}
	registry, ok := policyServices[0].Service.(*policyelements.SemanticDeciderRegistry)
	if !ok || registry == nil {
		cancelSession()
		t.Fatalf("semantic registry service has type %T", policyServices[0].Service)
	}
	resolver, ok := mediaServices[0].Service.(continuation.MediaResolver)
	if !ok || resolver == nil {
		cancelSession()
		t.Fatalf("session media resolver has type %T", mediaServices[0].Service)
	}
	registered, err := registry.Describe(realtimecubinding.SettlementPolicyReference)
	if err != nil {
		cancelSession()
		t.Fatal(err)
	}
	if registered != selected.Policy.Descriptor || verifier.modelChecks.Load() != 0 {
		cancelSession()
		t.Fatalf("lazy registry descriptor=%+v model checks=%d",
			registered, verifier.modelChecks.Load())
	}
	firstValue, firstDescriptor, err := registry.Open(realtimecubinding.SettlementPolicyReference)
	if err != nil {
		cancelSession()
		t.Fatal(err)
	}
	secondValue, secondDescriptor, err := registry.Open(realtimecubinding.SettlementPolicyReference)
	if err != nil {
		cancelSession()
		t.Fatal(err)
	}
	first, firstOK := firstValue.(*serveSemanticDecider)
	second, secondOK := secondValue.(*serveSemanticDecider)
	if !firstOK || !secondOK || first == second || first.Client == second.Client ||
		firstDescriptor != selected.Policy.Descriptor || secondDescriptor != selected.Policy.Descriptor ||
		verifier.modelChecks.Load() != 2 {
		cancelSession()
		t.Fatalf("fresh settlement clients first=%T/%p second=%T/%p checks=%d",
			firstValue, first, secondValue, second, verifier.modelChecks.Load())
	}
	if err := first.Close(); err != nil {
		cancelSession()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		cancelSession()
		t.Fatalf("idempotent first settlement client close: %v", err)
	}
	if _, err := first.Decide(context.Background(), coreinteraction.Decision{
		Prompt: "closed client must refuse", Options: []string{"continue", "succeeded"},
	}); !errors.Is(err, policymodel.ErrClientClosed) {
		cancelSession()
		t.Fatalf("first settlement client after close error = %v", err)
	}
	if err := second.Close(); err != nil {
		cancelSession()
		t.Fatal(err)
	}
	if _, err := second.Generate(context.Background(), "closed client must refuse", "", 8); !errors.Is(
		err, policymodel.ErrClientClosed,
	) {
		cancelSession()
		t.Fatalf("second settlement client after close error = %v", err)
	}

	drift := errors.New("settlement deployment drifted")
	verifier.err = drift
	if _, _, err := registry.Open(realtimecubinding.SettlementPolicyReference); !errors.Is(err, drift) {
		cancelSession()
		t.Fatalf("drifted settlement open error = %v", err)
	}
	if verifier.modelChecks.Load() != 3 {
		cancelSession()
		t.Fatalf("drifted settlement open model checks = %d, want 3", verifier.modelChecks.Load())
	}
	cancelSession()
	if _, err := resolver("unavailable-after-session"); !errors.Is(err, context.Canceled) {
		t.Fatalf("session resolver after cancellation error = %v", err)
	}
	checksBefore := verifier.modelChecks.Load()
	if _, _, err := registry.Open(realtimecubinding.SettlementPolicyReference); !errors.Is(err, context.Canceled) {
		t.Fatalf("semantic open after session cancellation error = %v", err)
	}
	if verifier.modelChecks.Load() != checksBefore {
		t.Fatalf("canceled session crossed readiness boundary: before=%d after=%d",
			checksBefore, verifier.modelChecks.Load())
	}
}

func TestServeRealtimeCUSettlementPolicyIdentityExcludesCredentialBytes(t *testing.T) {
	deployments := realtimeCUProfileTestDeployments()
	executable := inspect.ArtifactIdentity{
		ID:     "go://test/openrealtime/realtime-cu-settlement-identity/v1",
		Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	freeze := func(secret string) serveRealtimeCURegistration {
		t.Helper()
		t.Setenv(realtimeCULocalModelKeyEnvironment, secret)
		selected, err := newServeRealtimeCURegistration(
			context.Background(), executable, deployments,
			&fixtureRealtimeCUDeploymentVerifier{identity: deployments},
		)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(selected.Policy.Configuration, []byte(secret)) ||
			strings.Contains(selected.Policy.Artifact.Digest, secret) ||
			strings.Contains(selected.Policy.Descriptor.ConfigurationDigest, secret) {
			t.Fatal("settlement policy identity exposed credential bytes")
		}
		return selected
	}
	first := freeze("first-local-settlement-secret")
	second := freeze("second-local-settlement-secret")
	if first.Policy.Reference != second.Policy.Reference ||
		first.Policy.Artifact != second.Policy.Artifact ||
		first.Policy.Descriptor != second.Policy.Descriptor ||
		!bytes.Equal(first.Policy.Configuration, second.Policy.Configuration) {
		t.Fatalf("credential rotation changed non-secret policy identity:\nfirst=%+v\nsecond=%+v",
			first.Policy, second.Policy)
	}
	policyConfig, _, err := decodeServePolicyConfiguration(
		realtimeCULocalModelProvider, first.Policy.Configuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	policyConfig.RequestTimeoutMS++
	changedConfiguration, err := json.Marshal(policyConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, changedDescriptor, err := decodeServePolicyConfiguration(
		realtimeCULocalModelProvider, changedConfiguration,
	)
	if err != nil {
		t.Fatal(err)
	}
	changedArtifact, err := realtimeCULocalPolicyArtifact(
		executable, deployments.Model, changedConfiguration, changedDescriptor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changedDescriptor == first.Policy.Descriptor || changedArtifact == first.Policy.Artifact {
		t.Fatalf("behavioral policy change did not change descriptor and artifact:\noriginal=%+v/%+v\nchanged=%+v/%+v",
			first.Policy.Descriptor, first.Policy.Artifact, changedDescriptor, changedArtifact)
	}
	changedSelection := first.Policy
	changedSelection.Configuration = changedConfiguration
	changedSelection.Descriptor = changedDescriptor
	payload, err := json.Marshal(realtimecubinding.ApplicationConfig{
		FormatVersion: realtimecubinding.ApplicationFormatVersion,
		Model:         first.Model, SettlementPolicy: changedSelection,
		Observer: first.Observer,
		Target: computeruse.Target{
			Name: "settlement-artifact-test-browser", Sources: []string{realtimecubinding.SourceScreen},
			Width: 1280, Height: 577,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Application.Factory(context.Background(), payload); err == nil ||
		!strings.Contains(err.Error(), "differs from its artifact-bound selection") {
		t.Fatalf("config changed under frozen settlement artifact error = %v", err)
	}
}

func realtimeCUProfileMountDependency(
	t *testing.T, config graphlaunch.Config, name string,
) graphlaunch.MountDependencyPlugin {
	t.Helper()
	for _, dependency := range config.Catalog.MountDependencies {
		if dependency.Name == name {
			return dependency
		}
	}
	t.Fatalf("Realtime-CU mount dependency %q is missing", name)
	return graphlaunch.MountDependencyPlugin{}
}

func realtimeCUProfileTestDeployments() realtimeCUDeploymentIdentities {
	return realtimeCUDeploymentIdentities{
		Model: inspect.ArtifactIdentity{
			ID: "hf://Qwen/Qwen3-VL-30B-A3B-Instruct-FP8", Revision: "fixture-model-revision",
			Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		ASR: inspect.ArtifactIdentity{
			ID: "hf://mobiuslabsgmbh/faster-whisper-large-v3-turbo", Revision: "fixture-asr-revision",
			Digest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
		Vision: inspect.ArtifactIdentity{
			ID: "hf://Qwen/Qwen3-VL-30B-A3B-Instruct-FP8", Revision: "fixture-model-revision",
			Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
}

type fixtureRealtimeCUDeploymentVerifier struct {
	identity       realtimeCUDeploymentIdentities
	err            error
	combinedChecks atomic.Int32
	modelChecks    atomic.Int32
	observerChecks atomic.Int32
}

func (verifier *fixtureRealtimeCUDeploymentVerifier) Resolve(
	ctx context.Context,
) (realtimeCUDeploymentIdentities, error) {
	if ctx == nil {
		return realtimeCUDeploymentIdentities{}, errors.New("nil deployment resolution context")
	}
	if verifier.err != nil {
		return realtimeCUDeploymentIdentities{}, verifier.err
	}
	return verifier.identity, nil
}

func (verifier *fixtureRealtimeCUDeploymentVerifier) Verify(
	ctx context.Context, expected realtimeCUDeploymentIdentities,
) error {
	verifier.combinedChecks.Add(1)
	if ctx == nil {
		return errors.New("nil deployment verification context")
	}
	if verifier.err != nil {
		return verifier.err
	}
	if expected != verifier.identity {
		return errors.New("fixture deployment identity changed")
	}
	return nil
}

func (verifier *fixtureRealtimeCUDeploymentVerifier) VerifyModel(
	ctx context.Context, expected inspect.ArtifactIdentity,
) error {
	verifier.modelChecks.Add(1)
	if ctx == nil {
		return errors.New("nil model deployment verification context")
	}
	if verifier.err != nil {
		return verifier.err
	}
	if expected != verifier.identity.Model {
		return errors.New("fixture model deployment identity changed")
	}
	return nil
}

func (verifier *fixtureRealtimeCUDeploymentVerifier) VerifyObserver(
	ctx context.Context, expectedASR, expectedVision inspect.ArtifactIdentity,
) error {
	verifier.observerChecks.Add(1)
	if ctx == nil {
		return errors.New("nil observer deployment verification context")
	}
	if verifier.err != nil {
		return verifier.err
	}
	if expectedASR != verifier.identity.ASR || expectedVision != verifier.identity.Vision {
		return errors.New("fixture observer deployment identity changed")
	}
	return nil
}

func TestRealtimeCUDeploymentReadinessScopesOneBackendPerFactoryBoundary(t *testing.T) {
	deployments := realtimeCUProfileTestDeployments()
	verifier := &fixtureRealtimeCUDeploymentVerifier{identity: deployments}
	if err := verifyRealtimeCUModelDeployment(context.Background(), verifier, deployments); err != nil {
		t.Fatal(err)
	}
	if err := verifyRealtimeCUObserverDeployment(context.Background(), verifier, deployments); err != nil {
		t.Fatal(err)
	}
	if verifier.combinedChecks.Load() != 0 || verifier.modelChecks.Load() != 1 ||
		verifier.observerChecks.Load() != 1 {
		t.Fatalf("deployment readiness calls combined=%d model=%d observer=%d",
			verifier.combinedChecks.Load(), verifier.modelChecks.Load(), verifier.observerChecks.Load())
	}
}

func configureRealtimeCUProfileTestDeployments(options *realtimeCUProfileOptions) {
	options.deployments = realtimeCUProfileTestDeployments()
	options.deploymentVerifier = &fixtureRealtimeCUDeploymentVerifier{identity: options.deployments}
}
