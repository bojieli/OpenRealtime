package main

import (
	"bytes"
	"context"
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
	"github.com/bojieli/OpenRealtime/gateway"
	realtimecubinding "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
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
		"go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/activation/v10" ||
		activationResolution.Runtime.Revision != "implementation:10" {
		t.Fatalf("frozen activation runtime = %+v", activationResolution.Runtime)
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
