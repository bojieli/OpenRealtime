package launch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	launchService       = "service.launch-session"
	unusedLaunchService = "service.unused-launch-session"
	launchAdapterRef    = "go://openrealtime/test/launch-adapter/v1"
	launchAdapterName   = "openrealtime.test.launch-adapter"
)

type launchCounters struct {
	binders, unusedBinders             atomic.Int64
	dependencyFactories, unusedDeps    atomic.Int64
	elementMounts, unusedElementMounts atomic.Int64
	adapterFactories                   atomic.Int64
	acquired, closed                   atomic.Int64
}

type launchFixture struct {
	config   graphlaunch.Config
	counters *launchCounters
	artifact inspect.ArtifactIdentity
}

func TestNewBuildsExactNativeSessionProviderWithoutAcquiringResources(t *testing.T) {
	fixture := newLaunchFixture(t)
	result, err := graphlaunch.New(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Plan.Validate(); err != nil {
		t.Fatalf("launcher plan: %v", err)
	}
	if result.Evidence.Graph != result.Plan.Identity().GraphID ||
		len(result.Evidence.Profiles) != 1 ||
		result.Evidence.Profiles[0].PlanFingerprint != result.Plan.Identity().PlanFingerprint {
		t.Fatalf("bound launch evidence = %+v", result.Evidence)
	}
	var provider serverplugin.SessionProvider = result.Binding
	if provider.Name() != launchAdapterName ||
		result.Binding.Graph().Fingerprint != result.Plan.Graph().Fingerprint {
		t.Fatalf("native provider = %q graph %+v", provider.Name(), result.Binding.Graph())
	}
	assertNoLaunchAcquisition(t, fixture.counters)
	if fixture.counters.binders.Load() != 1 || fixture.counters.unusedBinders.Load() != 0 {
		t.Fatalf("adapter binder calls selected=%d unused=%d",
			fixture.counters.binders.Load(), fixture.counters.unusedBinders.Load())
	}

	// Completed launch owns its catalog containers. These mutations must not
	// redirect the selected adapter, implementation, or dependency factory.
	fixture.config.Catalog.Assembly.Implementations[0].Profile.Transports[0] = "redirected"
	fixture.config.Catalog.Adapters[0].Bind = nil
	fixture.config.Catalog.MountDependencies[0].Factory = nil
	fixture.config.Artifacts.Topology.Data[0] = 'X'
	fixture.config.Evidence.Profiles[0].Name = "redirected"
	if result.Evidence.Profiles[0].Name != "launch-warm" {
		t.Fatalf("launch result aliases caller evidence: %+v", result.Evidence)
	}

	runtime, err := provider.Start(context.Background(), legacy.Options{
		Sink: &launchSink{}, SessionID: "launch-normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.counters.dependencyFactories.Load() != 1 ||
		fixture.counters.unusedDeps.Load() != 0 ||
		fixture.counters.elementMounts.Load() != 1 ||
		fixture.counters.unusedElementMounts.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 1 ||
		fixture.counters.acquired.Load() != 1 {
		t.Fatalf("selected acquisition counters = %+v", counterSnapshot(fixture.counters))
	}
	native, ok := runtime.(*graphbinding.NativeRuntime)
	if !ok {
		t.Fatalf("runtime type = %T", runtime)
	}
	live := native.Live()
	if live.Adapter == nil || live.Adapter.Implementation != launchAdapterRef ||
		live.Adapter.Runtime != fixture.artifact ||
		live.Adapter.RuntimeEvidence != inspect.EvidenceRegistered ||
		live.Adapter.ProfileFingerprint == "" ||
		live.Adapter.ProfileFingerprint != runtime.Status().Profile {
		t.Fatalf("adapter evidence = %+v status = %+v", live.Adapter, runtime.Status())
	}
	if err := runtime.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if fixture.counters.closed.Load() != 1 {
		t.Fatalf("dependency-backed resource closes = %d, want 1", fixture.counters.closed.Load())
	}
}

func TestConcurrentSessionsGetIndependentDependencyLifecycles(t *testing.T) {
	fixture := newLaunchFixture(t)
	result, err := graphlaunch.New(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	const sessions = 12
	var wait sync.WaitGroup
	errorsFound := make(chan error, sessions)
	for index := 0; index < sessions; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			live, startErr := result.Binding.Start(context.Background(), legacy.Options{
				Sink: &launchSink{}, SessionID: fmt.Sprintf("launch-race-%d", index),
			})
			if startErr != nil {
				errorsFound <- startErr
				return
			}
			if closeErr := live.Close(context.Background(), nil); closeErr != nil {
				errorsFound <- closeErr
			}
		}(index)
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if fixture.counters.dependencyFactories.Load() != sessions ||
		fixture.counters.elementMounts.Load() != sessions ||
		fixture.counters.adapterFactories.Load() != sessions ||
		fixture.counters.acquired.Load() != sessions ||
		fixture.counters.closed.Load() != sessions ||
		fixture.counters.unusedDeps.Load() != 0 ||
		fixture.counters.unusedElementMounts.Load() != 0 {
		t.Fatalf("concurrent lifecycle counters = %+v", counterSnapshot(fixture.counters))
	}
}

func TestNewFailsClosedOnAdapterAndDependencyCatalogDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*graphlaunch.Config)
		want   string
	}{
		{
			name:   "missing evidence manifest",
			mutate: func(config *graphlaunch.Config) { config.Evidence = graphevidence.Document{} },
			want:   "evidence apiVersion",
		},
		{
			name: "evidence graph drift",
			mutate: func(config *graphlaunch.Config) {
				config.Evidence.Graph = "wrong_launch_session"
			},
			want: "manifest targets graph",
		},
		{
			name: "evidence plan drift",
			mutate: func(config *graphlaunch.Config) {
				config.Evidence.Profiles[0].PlanFingerprint = launchArtifact("plan/drift", "1").Digest
			},
			want: "plan fingerprint",
		},
		{
			name: "evidence node drift",
			mutate: func(config *graphlaunch.Config) {
				config.Evidence.Profiles[0].NodeID = "missing"
			},
			want: "is absent from the plan",
		},
		{
			name: "evidence element drift",
			mutate: func(config *graphlaunch.Config) {
				config.Evidence.Profiles[0].Element.Digest = launchArtifact("element/drift", "1").Digest
			},
			want: "element identity drifted",
		},
		{
			name: "evidence implementation drift",
			mutate: func(config *graphlaunch.Config) {
				config.Evidence.Profiles[0].Implementation = launchArtifact("runtime/drift", "1")
			},
			want: "implementation identity drifted",
		},
		{
			name:   "missing selected adapter",
			mutate: func(config *graphlaunch.Config) { config.Catalog.Adapters = nil },
			want:   "missing explicitly selected reference",
		},
		{
			name: "duplicate adapter",
			mutate: func(config *graphlaunch.Config) {
				config.Catalog.Adapters = append(config.Catalog.Adapters, config.Catalog.Adapters[0])
			},
			want: "adapter catalog is ambiguous",
		},
		{
			name: "adapter runtime drift",
			mutate: func(config *graphlaunch.Config) {
				config.Adapter.RuntimeArtifact = launchArtifact("adapter/drift", "1")
			},
			want: "runtime artifact drifted",
		},
		{
			name: "bound registration drift",
			mutate: func(config *graphlaunch.Config) {
				original := config.Catalog.Adapters[0].Bind
				config.Catalog.Adapters[0].Bind = func(
					ctx context.Context, plan *graphconfig.Plan,
				) (graphlaunch.BoundAdapter, error) {
					bound, err := original(ctx, plan)
					bound.Registration.Reference += "/drift"
					return bound, err
				}
			},
			want: "binder registration drifted",
		},
		{
			name: "profile identity drift",
			mutate: func(config *graphlaunch.Config) {
				config.Adapter.ProfileName = "openrealtime.test.wrong-adapter"
			},
			want: "profile identity drifted",
		},
		{
			name:   "missing selected mount dependency",
			mutate: func(config *graphlaunch.Config) { config.Catalog.MountDependencies = nil },
			want:   "missing selected contribution",
		},
		{
			name: "duplicate mount dependency",
			mutate: func(config *graphlaunch.Config) {
				config.Catalog.MountDependencies = append(
					config.Catalog.MountDependencies, config.Catalog.MountDependencies[0],
				)
			},
			want: "mount dependency catalog is ambiguous",
		},
		{
			name: "mount artifact drift",
			mutate: func(config *graphlaunch.Config) {
				config.Catalog.MountDependencies[0].Artifact = launchArtifact("service/drift", "1")
			},
			want: "artifact drifted from assembly metadata",
		},
		{
			name: "missing implementation",
			mutate: func(config *graphlaunch.Config) {
				config.Catalog.Assembly.Implementations = nil
			},
			want: "implementation \"impl/launch\" is absent from discovery",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLaunchFixture(t)
			test.mutate(&fixture.config)
			_, err := graphlaunch.New(context.Background(), fixture.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
			assertNoLaunchAcquisition(t, fixture.counters)
			if strings.Contains(test.name, "evidence") && fixture.counters.binders.Load() != 0 {
				t.Fatalf("rejected evidence reached adapter binder: %+v", counterSnapshot(fixture.counters))
			}
		})
	}
}

func TestMountContributionFailsBeforeElementOrAdapterAcquisition(t *testing.T) {
	fixture := newLaunchFixture(t)
	fixture.config.Catalog.MountDependencies[0].Factory = func(
		context.Context, legacy.Options,
	) ([]graphruntime.PreparedMountDependency, error) {
		fixture.counters.dependencyFactories.Add(1)
		return []graphruntime.PreparedMountDependency{
			{Name: launchService, Artifact: fixture.config.Catalog.MountDependencies[0].Artifact, Service: &launchServiceValue{counters: fixture.counters}},
			{Name: "service.excess", Artifact: launchArtifact("service/excess", "1"), Service: &struct{}{}},
		}, nil
	}
	result, err := graphlaunch.New(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = result.Binding.Start(context.Background(), legacy.Options{Sink: &launchSink{}})
	if err == nil || !strings.Contains(err.Error(), "contributed 2 entries; want exactly one") {
		t.Fatalf("Start() dependency contribution error = %v", err)
	}
	if fixture.counters.elementMounts.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 0 || fixture.counters.acquired.Load() != 0 {
		t.Fatalf("rejected mount contribution acquired resources: %+v", counterSnapshot(fixture.counters))
	}
}

func TestNewRejectsAlternatePlanCatalogAuthority(t *testing.T) {
	fixture := newLaunchFixture(t)
	discovery, err := fixture.config.Catalog.Assembly.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	fixture.config.PlanOptions.Discovery = discovery
	_, err = graphlaunch.New(context.Background(), fixture.config)
	if err == nil || !strings.Contains(err.Error(), "discovery is derived") {
		t.Fatalf("alternate discovery error = %v", err)
	}
	assertNoLaunchAcquisition(t, fixture.counters)
}

func newLaunchFixture(t testing.TB) launchFixture {
	t.Helper()
	counters := &launchCounters{}
	valueType := element.Event(element.Named("test.LaunchValue"))
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Launch", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: valueType, Cardinality: element.One, Required: true, DefaultDepth: 2},
		},
		Reaction: element.Reaction{
			Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1,
		},
		Dependencies: []element.Dependency{
			{Name: launchService}, {Name: unusedLaunchService, Optional: true},
		},
	}
	descriptors := resolve.NewCatalog()
	if err := descriptors.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	implementationArtifact := launchArtifact("runtime/launch", "1")
	unusedImplementationArtifact := launchArtifact("runtime/unused-launch", "1")
	dependencyArtifact := launchArtifact("service/launch", "1")
	unusedDependencyArtifact := launchArtifact("service/unused-launch", "1")
	assemblyCatalog := graphassembly.Catalog{
		Implementations: []graphruntime.FactoryRegistration{
			{
				Profile: graphruntime.FactoryProfile{
					Reference: "impl/launch", Artifact: implementationArtifact,
					Transports: []string{"in-process"},
				},
				Factory: launchElementFactory{descriptor: descriptor, counters: counters},
			},
			{
				Profile: graphruntime.FactoryProfile{
					Reference: "impl/unused-launch", Artifact: unusedImplementationArtifact,
					Transports: []string{"in-process"},
				},
				Factory: launchElementFactory{descriptor: descriptor, counters: counters, unused: true},
			},
		},
		Dependencies: []graphassembly.Dependency{
			{Name: launchService, Artifact: dependencyArtifact, Scope: graphconfig.DependencyScopeMount},
			{Name: unusedLaunchService, Artifact: unusedDependencyArtifact, Scope: graphconfig.DependencyScopeMount},
		},
	}
	discovery, err := assemblyCatalog.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	topology := graphconfig.Artifact{
		Path: "agent.ortg", Encoding: graphconfig.ORTG,
		Data: []byte(`graph launch_session {
    test.Launch :: worker;
    input input = worker.in;
    output output = worker.out;
}
`),
	}
	lockResult, err := graphconfig.UpdateLock(
		context.Background(), topology, graphconfig.Artifact{}, graphconfig.Options{
			Catalog: descriptors, Discovery: discovery, Loader: graphcompiler.FileLoader{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockResult.Lock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	adapterArtifact := launchArtifact("adapter/launch", "1")
	selectedAdapter := graphlaunch.AdapterPlugin{
		Reference: launchAdapterRef, Artifact: adapterArtifact,
		Bind: func(
			ctx context.Context, plan *graphconfig.Plan,
		) (graphlaunch.BoundAdapter, error) {
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			counters.binders.Add(1)
			profile, err := graphbinding.FreezeSessionAdapterProfile(
				graphbinding.SessionAdapterProfile{
					FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
					Name:          launchAdapterName, Revision: 1,
					GraphFingerprint: plan.Graph().Fingerprint,
					Ownership:        launchOwnership(),
				},
			)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{
				Profile: profile,
				Registration: graphbinding.AdapterRegistration{
					Reference: launchAdapterRef, Artifact: adapterArtifact,
					Factory: func(
						context.Context, *graphruntime.Mounted, legacy.Options,
						graphbinding.SessionAdapterProfile,
					) (graphbinding.SessionAdapter, error) {
						counters.adapterFactories.Add(1)
						return &launchAdapter{}, nil
					},
				},
			}, nil
		},
	}
	unusedAdapter := graphlaunch.AdapterPlugin{
		Reference: "go://openrealtime/test/unused-launch-adapter/v1",
		Artifact:  launchArtifact("adapter/unused-launch", "1"),
		Bind: func(context.Context, *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			counters.unusedBinders.Add(1)
			return graphlaunch.BoundAdapter{}, errors.New("unused adapter binder was called")
		},
	}
	selectedDependency := graphlaunch.MountDependencyPlugin{
		Name: launchService, Artifact: dependencyArtifact,
		Factory: func(
			ctx context.Context, _ legacy.Options,
		) ([]graphruntime.PreparedMountDependency, error) {
			if err := context.Cause(ctx); err != nil {
				return nil, err
			}
			counters.dependencyFactories.Add(1)
			return []graphruntime.PreparedMountDependency{{
				Name: launchService, Artifact: dependencyArtifact,
				Service: &launchServiceValue{counters: counters},
			}}, nil
		},
	}
	unusedDependency := graphlaunch.MountDependencyPlugin{
		Name: unusedLaunchService, Artifact: unusedDependencyArtifact,
		Factory: func(
			context.Context, legacy.Options,
		) ([]graphruntime.PreparedMountDependency, error) {
			counters.unusedDeps.Add(1)
			return nil, errors.New("unused dependency factory was called")
		},
	}
	fixture := launchFixture{
		artifact: adapterArtifact, counters: counters,
		config: graphlaunch.Config{
			Artifacts: graphconfig.Artifacts{
				Topology: topology,
				Values: graphconfig.Artifact{
					Path: "agent.values.yaml", Encoding: graphconfig.YAML,
					Data: []byte("apiVersion: openrealtime.ai/config/v1alpha1\ngraph: launch_session\nnodes: {}\n"),
				},
				Lock: graphconfig.Artifact{
					Path: "openrealtime.lock", Encoding: graphconfig.JSON, Data: lock,
				},
				Deployment: graphconfig.Artifact{
					Path: "agent.deployment.yaml", Encoding: graphconfig.YAML,
					Data: []byte("apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: launch_session\nnodes:\n  worker:\n    implementation: impl/launch\n    transport: in-process\n"),
				},
			},
			PlanOptions: graphconfig.Options{
				Catalog: descriptors, Loader: graphcompiler.FileLoader{},
			},
			Catalog: graphlaunch.Catalog{
				Assembly: assemblyCatalog,
				Adapters: []graphlaunch.AdapterPlugin{selectedAdapter, unusedAdapter},
				MountDependencies: []graphlaunch.MountDependencyPlugin{
					selectedDependency, unusedDependency,
				},
			},
			Adapter:         selectedAdapter.Selection(launchAdapterName, 1),
			ShutdownTimeout: time.Second,
		},
	}
	options := fixture.config.PlanOptions
	options.Discovery = discovery
	plan, err := graphconfig.Create(context.Background(), fixture.config.Artifacts, options)
	if err != nil {
		t.Fatal(err)
	}
	graph := plan.Graph()
	resolution := plan.Resolution()
	if len(graph.Nodes) != 1 || len(resolution.Nodes) != 1 {
		t.Fatalf("launch evidence fixture nodes = %d resolutions = %d", len(graph.Nodes), len(resolution.Nodes))
	}
	fixture.config.Evidence = graphevidence.Document{
		APIVersion: graphevidence.APIVersion, Graph: graph.ID,
		Profiles: []graphevidence.Profile{{
			Name: "launch-warm", Artifact: launchArtifact("evidence/launch-warm", "1"),
			PlanFingerprint: plan.Identity().PlanFingerprint,
			NodeID:          graph.Nodes[0].ID,
			Element:         graph.Nodes[0].Element,
			Implementation:  resolution.Nodes[0].Implementation.Artifact,
			Hardware:        launchArtifact("hardware/launch", "1"),
			Load:            launchArtifact("load/launch", "1"),
		}},
	}
	return fixture
}

type launchServiceValue struct{ counters *launchCounters }

type launchElementFactory struct {
	descriptor element.Descriptor
	counters   *launchCounters
	unused     bool
}

func (factory launchElementFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory launchElementFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if factory.unused {
		factory.counters.unusedElementMounts.Add(1)
		return nil, errors.New("unused implementation mounted")
	}
	factory.counters.elementMounts.Add(1)
	service, _, found := mount.Services.Lookup(launchService)
	value, correct := service.(*launchServiceValue)
	if !found || !correct || value.counters != factory.counters {
		return nil, errors.New("selected mount dependency service drifted")
	}
	value.counters.acquired.Add(1)
	if err := mount.Lifecycle.Defer("close-launch-service", func(context.Context) error {
		value.counters.closed.Add(1)
		return nil
	}); err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

type launchAdapter struct{}

func (*launchAdapter) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (*launchAdapter) Update(context.Context, legacy.Settings) error { return nil }
func (*launchAdapter) Audio(context.Context, perception.Frame) error { return legacy.ErrUnsupported }
func (*launchAdapter) Video(context.Context, perception.Frame) error { return legacy.ErrUnsupported }
func (*launchAdapter) Text(context.Context, legacy.TextInput) error  { return legacy.ErrUnsupported }
func (*launchAdapter) ToolResult(context.Context, trajectory.ToolResult) error {
	return legacy.ErrUnsupported
}
func (*launchAdapter) CommitAudio(context.Context) error    { return legacy.ErrUnsupported }
func (*launchAdapter) CreateResponse(context.Context) error { return legacy.ErrUnsupported }
func (*launchAdapter) Cancel(context.Context, string) error { return nil }
func (*launchAdapter) Truncate(context.Context, legacy.Truncation) error {
	return legacy.ErrUnsupported
}
func (*launchAdapter) Trajectory() trajectory.Snapshot    { return trajectory.Snapshot{} }
func (*launchAdapter) Status() legacy.Status              { return legacy.Status{} }
func (*launchAdapter) Close(context.Context, error) error { return nil }

type launchSink struct{}

func (*launchSink) TurnBegin(context.Context) error                                   { return nil }
func (*launchSink) TurnEnd(context.Context, legacy.TurnOutcome) error                 { return nil }
func (*launchSink) Activity(context.Context, legacy.ActivityEvent) error              { return nil }
func (*launchSink) Transcript(context.Context, legacy.TranscriptEvent) error          { return nil }
func (*launchSink) Observation(context.Context, perception.Observation) error         { return nil }
func (*launchSink) SpeechBegin(context.Context, action.Utterance) error               { return nil }
func (*launchSink) SpeechText(context.Context, action.Utterance, string) error        { return nil }
func (*launchSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error { return nil }
func (*launchSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error { return nil }
func (*launchSink) ToolCalls(context.Context, legacy.ToolCallEvent) error             { return nil }
func (*launchSink) Failed(context.Context, legacy.ErrorEvent)                         {}

func launchOwnership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
}

func launchArtifact(id, revision string) inspect.ArtifactIdentity {
	digest := sha256.Sum256([]byte(id + "\x00" + revision))
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func assertNoLaunchAcquisition(t testing.TB, counters *launchCounters) {
	t.Helper()
	if counters.dependencyFactories.Load() != 0 || counters.unusedDeps.Load() != 0 ||
		counters.elementMounts.Load() != 0 || counters.unusedElementMounts.Load() != 0 ||
		counters.adapterFactories.Load() != 0 || counters.acquired.Load() != 0 ||
		counters.closed.Load() != 0 {
		t.Fatalf("resource-free launch acquired resources: %+v", counterSnapshot(counters))
	}
}

func counterSnapshot(counters *launchCounters) map[string]int64 {
	return map[string]int64{
		"binders": counters.binders.Load(), "unused_binders": counters.unusedBinders.Load(),
		"dependency_factories": counters.dependencyFactories.Load(), "unused_dependencies": counters.unusedDeps.Load(),
		"element_mounts": counters.elementMounts.Load(), "unused_element_mounts": counters.unusedElementMounts.Load(),
		"adapter_factories": counters.adapterFactories.Load(), "acquired": counters.acquired.Load(),
		"closed": counters.closed.Load(),
	}
}
