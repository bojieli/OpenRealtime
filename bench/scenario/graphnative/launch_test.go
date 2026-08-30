package graphnative_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	graphnative "github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/gateway"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	scenarioAdapterReference = "go://openrealtime/test/scenario-adapter/v1"
	scenarioAdapterName      = "openrealtime.test.scenario-adapter"
	scenarioElementReference = "go://openrealtime/test/scenario-boundary/v1"
)

type scenarioLaunchCounters struct {
	binders, unusedBinders, adapterFactories, elementMounts atomic.Int64
}

type scenarioLaunchFixture struct {
	config   graphnative.Config
	counters *scenarioLaunchCounters
}

func TestNewPreparesExactGenericGraphProviderWithoutAcquiringSessionResources(t *testing.T) {
	fixture := newScenarioLaunchFixture(t)
	result, err := graphnative.New(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Contract.Validate(); err != nil {
		t.Fatalf("scenario contract: %v", err)
	}
	if err := result.Plan.Validate(); err != nil {
		t.Fatalf("scenario launch plan: %v", err)
	}
	if result.Binding == nil || result.Binding.Graph().Fingerprint != result.Plan.Graph().Fingerprint {
		t.Fatalf("scenario graph provider = %v plan = %s", result.Binding, result.Plan.Graph().Fingerprint)
	}
	var provider serverplugin.SessionProvider = result.Binding
	if provider.Name() != scenarioAdapterName {
		t.Fatalf("scenario session provider name = %q", provider.Name())
	}
	if fixture.counters.binders.Load() != 1 || fixture.counters.unusedBinders.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 0 ||
		fixture.counters.elementMounts.Load() != 0 {
		t.Fatalf("resource-free scenario launch counters = %+v", scenarioCounterSnapshot(fixture.counters))
	}

	// New and GuardAdapter own their catalog and contract containers. Caller
	// mutations after preparation must not redirect the provider.
	fixture.config.Cases = []string{"an ordinary question"}
	fixture.config.Launch.Catalog.Adapters[0].Bind = nil
	if result.Contract.Cases[0].Name != "count-as-they-go" || result.Binding == nil {
		t.Fatal("prepared scenario provider aliases caller configuration")
	}
}

func TestGuardLaunchComposesDirectlyThroughGenericServerGraphBundle(t *testing.T) {
	fixture := newScenarioLaunchFixture(t)
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := graphnative.GuardLaunch(contract, fixture.config.Launch)
	if err != nil {
		t.Fatal(err)
	}
	composition, err := serverplugin.NewGraphBundle(context.Background(), serverplugin.GraphBundleConfig{
		Graph: guarded,
		Server: serverplugin.BundleConfig{
			ProfileName: "openrealtime.server.scenario-contract-test", ProfileRevision: 1,
			Gateway:          gateway.Config{Model: "scenario-contract-test", ValidateWire: true},
			ProviderArtifact: scenarioArtifact("server/scenario-provider", "1"),
			GatewayArtifact:  scenarioArtifact("server/scenario-gateway", "1"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition.GraphPlan == nil || composition.ServerBundle == nil ||
		composition.GraphPlan.Identity().PlanFingerprint == "" ||
		composition.ServerBundle.Plan.Fingerprint == "" {
		t.Fatalf("scenario graph/server composition omitted identities: %+v", composition)
	}
	if fixture.counters.binders.Load() != 1 || fixture.counters.unusedBinders.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 0 || fixture.counters.elementMounts.Load() != 0 {
		t.Fatalf("scenario graph/server composition acquired resources: %+v",
			scenarioCounterSnapshot(fixture.counters))
	}

	// Installing a guard owns only the returned adapter catalog slice.
	guarded.Catalog.Adapters[0].Bind = nil
	if fixture.config.Launch.Catalog.Adapters[0].Bind == nil {
		t.Fatal("GuardLaunch mutated the caller's adapter catalog")
	}
}

func TestNewFailsClosedOnMissingScenarioOperationsCapabilitiesAndGraphIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*graphbinding.SessionAdapterProfile)
		want   string
	}{
		{
			name: "still image message operation",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				profile.Boundaries = removeOperation(profile.Boundaries, graphbinding.AdapterInputText)
			},
			want: "gateway.input.text",
		},
		{
			name: "tool result operation",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				profile.Boundaries = removeOperation(profile.Boundaries, graphbinding.AdapterInputToolResult)
			},
			want: "gateway.input.tool_result",
		},
		{
			name: "concurrent IO capability",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				profile.Capabilities.Stack.ConcurrentIO = false
			},
			want: "capability.concurrent_io",
		},
		{
			name: "still image capability",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				profile.Capabilities.Stack.VisualInput = false
			},
			want: "capability.visual_input",
		},
		{
			name: "selected graph identity",
			mutate: func(profile *graphbinding.SessionAdapterProfile) {
				profile.GraphFingerprint = "sha256:" + strings.Repeat("f", 64)
			},
			want: "mounted graph is",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScenarioLaunchFixture(t)
			fixture.config.Launch.Catalog.Adapters[0] = mutateAdapterProfile(
				fixture.config.Launch.Catalog.Adapters[0], test.mutate,
			)
			_, err := graphnative.New(context.Background(), fixture.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario launch error = %v, want %q", err, test.want)
			}
			if fixture.counters.binders.Load() != 1 || fixture.counters.unusedBinders.Load() != 0 ||
				fixture.counters.adapterFactories.Load() != 0 ||
				fixture.counters.elementMounts.Load() != 0 {
				t.Fatalf("rejected scenario launch acquired resources: %+v",
					scenarioCounterSnapshot(fixture.counters))
			}
		})
	}
}

func TestOrdinaryDiagnosticDoesNotRequireUnselectedToolOrImageExtensions(t *testing.T) {
	fixture := newScenarioLaunchFixture(t)
	fixture.config.Cases = []string{"an ordinary question"}
	fixture.config.Launch.Catalog.Adapters[0] = mutateAdapterProfile(
		fixture.config.Launch.Catalog.Adapters[0],
		func(profile *graphbinding.SessionAdapterProfile) {
			for _, operation := range append(slices.Clone(toolOperationsForTest()), graphbinding.AdapterInputText) {
				profile.Boundaries = removeOperation(profile.Boundaries, operation)
			}
			profile.Capabilities.Stack.VisualInput = false
			profile.Capabilities.Stack.TextInjection = false
		},
	)
	result, err := graphnative.New(context.Background(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Contract.Cases) != 1 || result.Contract.Cases[0].Name != "an ordinary question" {
		t.Fatalf("ordinary diagnostic contract = %+v", result.Contract.Cases)
	}
	if fixture.counters.unusedBinders.Load() != 0 || fixture.counters.adapterFactories.Load() != 0 ||
		fixture.counters.elementMounts.Load() != 0 {
		t.Fatalf("ordinary preparation acquired resources: %+v", scenarioCounterSnapshot(fixture.counters))
	}
}

func TestNewRejectsMissingAmbiguousAndDriftedSelectedAdapterBeforeBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*graphnative.Config)
		want   string
	}{
		{
			name: "missing",
			mutate: func(config *graphnative.Config) {
				config.Launch.Catalog.Adapters = nil
			},
			want: "is absent from the plugin catalog",
		},
		{
			name: "ambiguous",
			mutate: func(config *graphnative.Config) {
				config.Launch.Catalog.Adapters = append(
					config.Launch.Catalog.Adapters, config.Launch.Catalog.Adapters[0],
				)
			},
			want: "adapter catalog is ambiguous",
		},
		{
			name: "runtime artifact drift",
			mutate: func(config *graphnative.Config) {
				config.Launch.Adapter.RuntimeArtifact = scenarioArtifact("adapter/scenario-drift", "1")
			},
			want: "runtime artifact drifted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScenarioLaunchFixture(t)
			test.mutate(&fixture.config)
			_, err := graphnative.New(context.Background(), fixture.config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario adapter catalog error = %v, want %q", err, test.want)
			}
			if fixture.counters.binders.Load() != 0 || fixture.counters.unusedBinders.Load() != 0 ||
				fixture.counters.adapterFactories.Load() != 0 ||
				fixture.counters.elementMounts.Load() != 0 {
				t.Fatalf("adapter catalog refusal acquired resources: %+v",
					scenarioCounterSnapshot(fixture.counters))
			}
		})
	}
}

func TestNewHonorsCanceledLaunchContextBeforeAdapterBinding(t *testing.T) {
	fixture := newScenarioLaunchFixture(t)
	want := errors.New("scenario launch canceled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(want)
	_, err := graphnative.New(ctx, fixture.config)
	if !errors.Is(err, want) {
		t.Fatalf("canceled scenario launch error = %v, want %v", err, want)
	}
	if fixture.counters.binders.Load() != 0 || fixture.counters.unusedBinders.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 0 || fixture.counters.elementMounts.Load() != 0 {
		t.Fatalf("canceled scenario launch acquired resources: %+v",
			scenarioCounterSnapshot(fixture.counters))
	}
}

func TestGuardAdapterRejectsMalformedContractsPluginsAndNilPlans(t *testing.T) {
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	fixture := newScenarioLaunchFixture(t)
	plugin := fixture.config.Launch.Catalog.Adapters[0]

	drifted := contract.Clone()
	drifted.Fingerprint = "sha256:" + strings.Repeat("0", 64)
	if _, err := graphnative.GuardAdapter(drifted, plugin); err == nil {
		t.Fatal("GuardAdapter accepted a stale scenario contract fingerprint")
	}
	withoutBinder := plugin
	withoutBinder.Bind = nil
	if _, err := graphnative.GuardAdapter(contract, withoutBinder); err == nil ||
		!strings.Contains(err.Error(), "requires a binder") {
		t.Fatalf("nil scenario adapter binder error = %v", err)
	}
	mutableReference := plugin
	mutableReference.Reference = "go://openrealtime/test/scenario-adapter/latest"
	if _, err := graphnative.GuardAdapter(contract, mutableReference); err == nil ||
		!strings.Contains(err.Error(), "canonical reference") {
		t.Fatalf("mutable scenario adapter reference error = %v", err)
	}

	guarded, err := graphnative.GuardAdapter(contract, plugin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Bind(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "nil plan") {
		t.Fatalf("nil scenario adapter plan error = %v", err)
	}
	if fixture.counters.binders.Load() != 0 {
		t.Fatalf("nil-plan guard called the underlying binder %d time(s)", fixture.counters.binders.Load())
	}
}

func TestNewPropagatesSelectedBinderFailureWithoutAcquiringResources(t *testing.T) {
	fixture := newScenarioLaunchFixture(t)
	want := errors.New("selected scenario binder failed")
	fixture.config.Launch.Catalog.Adapters[0].Bind = func(
		context.Context,
		*graphconfig.Plan,
	) (graphlaunch.BoundAdapter, error) {
		fixture.counters.binders.Add(1)
		return graphlaunch.BoundAdapter{}, want
	}
	_, err := graphnative.New(context.Background(), fixture.config)
	if !errors.Is(err, want) {
		t.Fatalf("scenario binder error = %v, want %v", err, want)
	}
	if fixture.counters.binders.Load() != 1 || fixture.counters.unusedBinders.Load() != 0 ||
		fixture.counters.adapterFactories.Load() != 0 || fixture.counters.elementMounts.Load() != 0 {
		t.Fatalf("failed scenario binder acquired resources: %+v",
			scenarioCounterSnapshot(fixture.counters))
	}
}

func newScenarioLaunchFixture(t testing.TB) scenarioLaunchFixture {
	t.Helper()
	contract, err := graphnative.BuildContract()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, topology, boundaryByOperation := scenarioBoundaryDescriptor(contract.Operations)
	descriptors := resolve.NewCatalog()
	if err := descriptors.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	counters := &scenarioLaunchCounters{}
	elementArtifact := scenarioArtifact("runtime/scenario-boundary", "1")
	assembly := graphassembly.Catalog{Implementations: []graphruntime.FactoryRegistration{{
		Profile: graphruntime.FactoryProfile{
			Reference:  scenarioElementReference,
			Artifact:   elementArtifact,
			Transports: []string{"in-process"},
		},
		Factory: scenarioBoundaryFactory{descriptor: descriptor, mounts: &counters.elementMounts},
	}}}
	discovery, err := assembly.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	topologyArtifact := graphconfig.Artifact{
		Path: "scenario.ortg", Encoding: graphconfig.ORTG, Data: []byte(topology),
	}
	lockResult, err := graphconfig.UpdateLock(
		context.Background(), topologyArtifact, graphconfig.Artifact{}, graphconfig.Options{
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
	adapterArtifact := scenarioArtifact("adapter/scenario", "1")
	plugin := graphlaunch.AdapterPlugin{
		Reference: scenarioAdapterReference,
		Artifact:  adapterArtifact,
		Bind: func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			counters.binders.Add(1)
			profile, err := scenarioAdapterProfile(plan.Graph(), boundaryByOperation)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{
				Profile: profile,
				Registration: graphbinding.AdapterRegistration{
					Reference: scenarioAdapterReference,
					Artifact:  adapterArtifact,
					Factory: func(
						context.Context, *graphruntime.Mounted, legacy.Options,
						graphbinding.SessionAdapterProfile,
					) (graphbinding.SessionAdapter, error) {
						counters.adapterFactories.Add(1)
						return &scenarioTestAdapter{}, nil
					},
				},
			}, nil
		},
	}
	unusedPlugin := graphlaunch.AdapterPlugin{
		Reference: "go://openrealtime/test/unused-scenario-adapter/v1",
		Artifact:  scenarioArtifact("adapter/unused-scenario", "1"),
		Bind: func(context.Context, *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			counters.unusedBinders.Add(1)
			return graphlaunch.BoundAdapter{}, errors.New("unused scenario adapter binder was called")
		},
	}
	return scenarioLaunchFixture{
		counters: counters,
		config: graphnative.Config{Launch: graphlaunch.Config{
			Artifacts: graphconfig.Artifacts{
				Topology: topologyArtifact,
				Values: graphconfig.Artifact{
					Path: "scenario.values.yaml", Encoding: graphconfig.YAML,
					Data: []byte("apiVersion: openrealtime.ai/config/v1alpha1\ngraph: scenario_session\nnodes: {}\n"),
				},
				Lock: graphconfig.Artifact{
					Path: "openrealtime.lock", Encoding: graphconfig.JSON, Data: lock,
				},
				Deployment: graphconfig.Artifact{
					Path: "scenario.deployment.yaml", Encoding: graphconfig.YAML,
					Data: []byte("apiVersion: openrealtime.ai/deployment/v1alpha1\ngraph: scenario_session\nnodes:\n  boundary:\n    implementation: " + scenarioElementReference + "\n    transport: in-process\n"),
				},
			},
			PlanOptions: graphconfig.Options{Catalog: descriptors, Loader: graphcompiler.FileLoader{}},
			Catalog: graphlaunch.Catalog{
				Assembly: assembly, Adapters: []graphlaunch.AdapterPlugin{plugin, unusedPlugin},
			},
			Adapter:         plugin.Selection(scenarioAdapterName, 1),
			ShutdownTimeout: time.Second,
		}},
	}
}

func scenarioBoundaryDescriptor(
	operations []graphbinding.AdapterOperation,
) (element.Descriptor, string, map[graphbinding.AdapterOperation]string) {
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.ScenarioBoundary",
		Revision:      1,
	}
	boundaries := make(map[graphbinding.AdapterOperation]string, len(operations))
	var topology strings.Builder
	topology.WriteString("graph scenario_session {\n    test.ScenarioBoundary :: boundary;\n")
	for index, operation := range operations {
		port := fmt.Sprintf("port_%02d", index)
		boundary := fmt.Sprintf("operation_%02d", index)
		direction := element.Output
		declaration := "output"
		if strings.HasPrefix(string(operation), "gateway.input.") {
			direction = element.Input
			declaration = "input"
		}
		typeName := element.Event(element.Named(fmt.Sprintf("test.ScenarioPayload%02d", index)))
		descriptor.Ports = append(descriptor.Ports, element.Port{
			Name: port, Direction: direction, Type: typeName,
			Cardinality: element.One, Required: true, DefaultDepth: 4,
		})
		if direction == element.Input {
			descriptor.Reaction.Triggers = append(descriptor.Reaction.Triggers, port)
		} else {
			descriptor.Reaction.Outcomes = append(descriptor.Reaction.Outcomes, port)
		}
		fmt.Fprintf(&topology, "    %s %s = boundary.%s;\n", declaration, boundary, port)
		boundaries[operation] = boundary
	}
	descriptor.Reaction.MaxConcurrency = 1
	topology.WriteString("}\n")
	return descriptor, topology.String(), boundaries
}

func scenarioAdapterProfile(
	graph ir.Graph,
	boundaryByOperation map[graphbinding.AdapterOperation]string,
) (graphbinding.SessionAdapterProfile, error) {
	boundaries := make(map[string]ir.Boundary, len(graph.Boundaries))
	for _, boundary := range graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	operations := make([]graphbinding.AdapterOperation, 0, len(boundaryByOperation))
	for operation := range boundaryByOperation {
		operations = append(operations, operation)
	}
	slices.Sort(operations)
	mappings := make([]graphbinding.AdapterBoundary, 0, len(operations))
	for _, operation := range operations {
		boundary := boundaries[boundaryByOperation[operation]]
		mappings = append(mappings, graphbinding.AdapterBoundary{
			Operation: operation, Boundary: boundary.Name,
			Direction: boundary.Direction, Type: boundary.Type,
		})
	}
	return graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion:    graphbinding.SessionAdapterProfileFormatVersion,
		Name:             scenarioAdapterName,
		Revision:         1,
		GraphFingerprint: graph.Fingerprint,
		Ownership: legacy.Ownership{
			Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
			SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
			Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
		},
		Capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{
			AudioInput: true, AudioOutput: true, VisualInput: true,
			Transcription: true, TurnGeneration: true, ConcurrentIO: true, TextInjection: true,
		}},
		Boundaries: mappings,
	})
}

func mutateAdapterProfile(
	plugin graphlaunch.AdapterPlugin,
	mutate func(*graphbinding.SessionAdapterProfile),
) graphlaunch.AdapterPlugin {
	result := plugin
	original := plugin.Bind
	result.Bind = func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
		bound, err := original(ctx, plan)
		if err != nil {
			return graphlaunch.BoundAdapter{}, err
		}
		mutate(&bound.Profile)
		bound.Profile, err = graphbinding.FreezeSessionAdapterProfile(bound.Profile)
		return bound, err
	}
	return result
}

func removeOperation(
	boundaries []graphbinding.AdapterBoundary,
	operation graphbinding.AdapterOperation,
) []graphbinding.AdapterBoundary {
	result := make([]graphbinding.AdapterBoundary, 0, len(boundaries))
	for _, boundary := range boundaries {
		if boundary.Operation != operation {
			result = append(result, boundary)
		}
	}
	return result
}

func toolOperationsForTest() []graphbinding.AdapterOperation {
	return []graphbinding.AdapterOperation{
		graphbinding.AdapterInputToolResult,
		graphbinding.AdapterInputCreateResponse,
		graphbinding.AdapterOutputToolCalls,
	}
}

type scenarioBoundaryFactory struct {
	descriptor element.Descriptor
	mounts     *atomic.Int64
}

func (factory scenarioBoundaryFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory scenarioBoundaryFactory) Mount(
	context.Context,
	element.MountContext,
) (element.Runnable, error) {
	factory.mounts.Add(1)
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}), nil
}

type scenarioTestAdapter struct{}

func (*scenarioTestAdapter) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
func (*scenarioTestAdapter) Update(context.Context, legacy.Settings) error { return nil }
func (*scenarioTestAdapter) Audio(context.Context, perception.Frame) error { return nil }
func (*scenarioTestAdapter) Video(context.Context, perception.Frame) error {
	return legacy.ErrUnsupported
}
func (*scenarioTestAdapter) Text(context.Context, legacy.TextInput) error { return nil }
func (*scenarioTestAdapter) ToolResult(context.Context, trajectory.ToolResult) error {
	return nil
}
func (*scenarioTestAdapter) CommitAudio(context.Context) error    { return legacy.ErrUnsupported }
func (*scenarioTestAdapter) CreateResponse(context.Context) error { return nil }
func (*scenarioTestAdapter) Cancel(context.Context, string) error { return nil }
func (*scenarioTestAdapter) Truncate(context.Context, legacy.Truncation) error {
	return legacy.ErrUnsupported
}
func (*scenarioTestAdapter) Trajectory() trajectory.Snapshot    { return trajectory.Snapshot{} }
func (*scenarioTestAdapter) Status() legacy.Status              { return legacy.Status{} }
func (*scenarioTestAdapter) Close(context.Context, error) error { return nil }

func scenarioArtifact(id, revision string) inspect.ArtifactIdentity {
	digest := sha256.Sum256([]byte(id + "\x00" + revision))
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func scenarioCounterSnapshot(counters *scenarioLaunchCounters) map[string]int64 {
	return map[string]int64{
		"binders":           counters.binders.Load(),
		"unused_binders":    counters.unusedBinders.Load(),
		"adapter_factories": counters.adapterFactories.Load(),
		"element_mounts":    counters.elementMounts.Load(),
	}
}

var (
	_ element.Factory             = scenarioBoundaryFactory{}
	_ graphbinding.SessionAdapter = (*scenarioTestAdapter)(nil)
)
