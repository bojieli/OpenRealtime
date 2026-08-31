package config_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const topology = `// Deployment fixture.
graph deployable {
    test.Source :: source;
    test.Sink :: sink;

    source.out -> sink.in;
    input text_in = source.in;
    output text_out = sink.out;
}
`

const sourceSchemaID = "https://schemas.openrealtime.test/source-config/v1"

type fixture struct {
	catalog    *resolve.Catalog
	discovery  *graphconfig.StaticDiscovery
	artifacts  graphconfig.Artifacts
	options    graphconfig.Options
	identities map[string]element.Identity
}

func newFixture(t testing.TB) fixture {
	t.Helper()
	catalog := resolve.NewCatalog()
	descriptors := []element.Descriptor{sourceDescriptor(1, 4), sinkDescriptor()}
	identities := make(map[string]element.Identity, len(descriptors))
	for _, descriptor := range descriptors {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
		identity, err := descriptor.Identity()
		if err != nil {
			t.Fatal(err)
		}
		identities[descriptor.Name] = identity
	}
	discovery := graphconfig.NewStaticDiscovery()
	mustRegisterImplementation(t, discovery, graphconfig.ImplementationResolution{
		Reference: "impl/source", Contract: identities["test.Source"],
		Artifact: artifact("implementation/source", "1"), Evidence: inspect.EvidenceRegistered,
		Placements: []string{"llm-pool"}, Transports: []string{"in-process"},
		ResourceKeys: []string{"cpu"}, SecretSlots: []string{"apiKey"},
		Capabilities: []inspect.CapabilityIdentity{{
			Name: "text-generation", Contract: "Event<test.Text>",
			Provider: artifact("provider/source", "2"),
		}},
	})
	mustRegisterImplementation(t, discovery, graphconfig.ImplementationResolution{
		Reference: "impl/sink", Contract: identities["test.Sink"],
		Artifact: artifact("implementation/sink", "3"), Evidence: inspect.EvidenceRegistered,
		Transports: []string{"in-process"},
	})
	if err := discovery.RegisterDependency(graphconfig.DependencyResolution{
		Name: "service.clock", Artifact: artifact("service/clock", "4"),
		Scope: graphconfig.DependencyScopeProcess,
	}); err != nil {
		t.Fatal(err)
	}

	options := graphconfig.Options{
		Catalog: catalog, Discovery: discovery, SchemaResolver: schemaResolver{},
		SecretCatalog: &graphsecret.Document{
			APIVersion: graphsecret.APIVersion, Catalog: "deployable",
			Secrets: map[string]graphsecret.Binding{
				"secret://providers/source/api-key": {Provider: "env", Locator: "SOURCE_API_KEY"},
			},
		},
	}
	topologyArtifact := graphconfig.Artifact{Path: "agent.ortg", Data: []byte(topology)}
	updated, err := graphconfig.UpdateLock(context.Background(), topologyArtifact, graphconfig.Artifact{}, options)
	if err != nil {
		t.Fatalf("update fixture lock: %v", err)
	}
	lockPayload, err := updated.Lock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	artifacts := graphconfig.Artifacts{
		Topology: topologyArtifact,
		Values: graphconfig.Artifact{Path: "agent.values.yaml", Data: []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: deployable
nodes:
  source:
    model: fast
    budget: 32
`)},
		Lock: graphconfig.Artifact{Path: "openrealtime.lock", Data: lockPayload},
		Channels: graphconfig.Artifact{Path: "agent.channels.yaml", Data: []byte(`apiVersion: openrealtime.ai/channels/v1alpha1
graph: deployable
edges:
  source.out->sink.in:
    depth: 8
`)},
		Deployment: graphconfig.Artifact{Path: "agent.deployment.yaml", Data: []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: deployable
nodes:
  source:
    implementation: impl/source
    placement: llm-pool
    transport: in-process
    resources:
      cpu: "2"
    secrets:
      apiKey: secret://providers/source/api-key
  sink:
    implementation: impl/sink
    transport: in-process
`)},
	}
	return fixture{
		catalog: catalog, discovery: discovery, artifacts: artifacts,
		options: options, identities: identities,
	}
}

func TestCreateFreezesCompletePrelaunchPlan(t *testing.T) {
	fixture := newFixture(t)
	plan, err := graphconfig.Create(context.Background(), fixture.artifacts, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("validate plan: %v", err)
	}
	identity := plan.Identity()
	for name, digest := range map[string]string{
		"source": identity.SourceDigest, "lock": identity.LockDigest,
		"values": identity.ValuesDigest, "channels": identity.ChannelsDigest,
		"schema": identity.ValuesSchemaDigest, "deployment": identity.PublicDeploymentDigest,
		"resolution": identity.ResolutionDigest, "graph": identity.GraphFingerprint,
		"plan": identity.PlanFingerprint,
	} {
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
			t.Fatalf("%s digest = %q", name, digest)
		}
	}
	graph := plan.Graph()
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 || graph.Edges[0].Depth != 8 {
		t.Fatalf("compiled graph = %+v", graph)
	}
	for _, node := range graph.Nodes {
		if node.ConfigReference == "" || node.ConfigDigest == "" ||
			node.DeploymentReference == "" || node.DeploymentDigest == "" {
			t.Fatalf("node lacks config/deployment identity: %+v", node)
		}
	}
	resolution := plan.Resolution()
	if len(resolution.Nodes) != 2 || len(resolution.Dependencies) != 1 ||
		resolution.Dependencies[0].Name != "service.clock" ||
		resolution.Dependencies[0].Scope != graphconfig.DependencyScopeProcess {
		t.Fatalf("resolution = %+v", resolution)
	}
	if plan.DeploymentFingerprint() == "" || plan.SecretCatalogFingerprint() == "" {
		t.Fatal("private deployment/secret identity is missing")
	}

	// Every accessor must be an independent snapshot.
	graph.Nodes[0].ID = "mutated"
	values := plan.Values()
	values["source"][0] = '['
	delete(values, "sink")
	deployment := plan.Deployment()
	node := deployment["source"]
	node.Resources["cpu"] = "999"
	deployment["source"] = node
	lock := plan.Lock()
	lock.Entries[0].Reference = "mutated"
	source := plan.Source()
	source.Data[0] = 'X'
	resolution.Nodes[0].NodeID = "mutated"
	resolution.Nodes[0].Implementation.Transports[0] = "mutated"
	bundle := plan.ValuesSchema()
	bundle.Schema[0] = 'X'
	if err := plan.Validate(); err != nil {
		t.Fatalf("caller mutation changed plan: %v", err)
	}
	if plan.Graph().Nodes[0].ID == "mutated" || plan.Lock().Entries[0].Reference == "mutated" ||
		plan.Deployment()["source"].Resources["cpu"] != "2" {
		t.Fatal("plan accessors alias internal state")
	}
}

func TestCreateSelectsOnlyExplicitDeclaredOptionalDependencies(t *testing.T) {
	fixture := newFixture(t)
	optionalArtifact := artifact("service/optional", "1")
	if err := fixture.discovery.RegisterDependency(graphconfig.DependencyResolution{
		Name: "service.optional", Artifact: optionalArtifact,
		Scope: graphconfig.DependencyScopeProcess,
	}); err != nil {
		t.Fatal(err)
	}

	base := mustCreate(t, fixture.artifacts, fixture.options)
	if got := base.Resolution().Dependencies; len(got) != 1 || got[0].Name != "service.clock" {
		t.Fatalf("unselected dependency resolution = %+v", got)
	}

	selection := []string{"service.optional"}
	selectedOptions := fixture.options
	selectedOptions.OptionalDependencies = selection
	selected := mustCreate(t, fixture.artifacts, selectedOptions)
	dependencies := selected.Resolution().Dependencies
	if len(dependencies) != 2 || dependencies[0].Name != "service.clock" ||
		dependencies[1].Name != "service.optional" || dependencies[1].Artifact != optionalArtifact {
		t.Fatalf("selected dependency resolution = %+v", dependencies)
	}
	if selected.Identity().ResolutionDigest == base.Identity().ResolutionDigest ||
		selected.Identity().PlanFingerprint == base.Identity().PlanFingerprint {
		t.Fatal("optional dependency selection did not enter the immutable plan identity")
	}
	selection[0] = "mutated.after.create"
	if err := selected.Validate(); err != nil {
		t.Fatalf("caller mutation changed selected plan: %v", err)
	}

	tests := map[string]struct {
		selection []string
		want      string
	}{
		"duplicate":        {selection: []string{"service.optional", "service.optional"}, want: "repeats"},
		"already required": {selection: []string{"service.clock"}, want: "already required"},
		"undeclared":       {selection: []string{"service.other"}, want: "not declared"},
		"non-canonical":    {selection: []string{" service.optional"}, want: "not canonical"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			options := fixture.options
			options.OptionalDependencies = test.selection
			if _, err := graphconfig.Create(context.Background(), fixture.artifacts, options); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("selected dependency absent from discovery", func(t *testing.T) {
		missing := newFixture(t)
		options := missing.options
		options.OptionalDependencies = []string{"service.optional"}
		if _, err := graphconfig.Create(context.Background(), missing.artifacts, options); err == nil ||
			!strings.Contains(err.Error(), `implementation "service.optional"`) &&
				!strings.Contains(err.Error(), `dependency "service.optional"`) {
			t.Fatalf("missing optional dependency error = %v", err)
		}
	})
}

func TestPlanIdentitySeparatesSemanticAxesAndPrivateSecretRotation(t *testing.T) {
	fixture := newFixture(t)
	base := mustCreate(t, fixture.artifacts, fixture.options)

	equivalent := fixture.artifacts
	equivalent.Values = graphconfig.Artifact{Path: "values.json", Data: []byte(`{
  "nodes":{"source":{"budget":32,"model":"fast"}},
  "graph":"deployable","apiVersion":"openrealtime.ai/config/v1alpha1"
}`)}
	equivalent.Channels = graphconfig.Artifact{Path: "channels.json", Data: []byte(`{
  "edges":{"source.out->sink.in":{"depth":8}},
  "graph":"deployable","apiVersion":"openrealtime.ai/channels/v1alpha1"
}`)}
	equivalent.Deployment = graphconfig.Artifact{Path: "deployment.json", Data: []byte(`{
  "nodes":{
    "sink":{"transport":"in-process","implementation":"impl/sink"},
    "source":{"secrets":{"apiKey":"secret://providers/source/api-key"},
      "resources":{"cpu":"2"},"transport":"in-process","placement":"llm-pool","implementation":"impl/source"}
  },"graph":"deployable","apiVersion":"openrealtime.ai/deployment/v1alpha1"
}`)}
	same := mustCreate(t, equivalent, fixture.options)
	if same.Identity() != base.Identity() || same.DeploymentFingerprint() != base.DeploymentFingerprint() {
		t.Fatalf("format/map order changed identity:\nbase=%+v\nsame=%+v", base.Identity(), same.Identity())
	}

	commented := fixture.artifacts
	commented.Topology = graphconfig.Artifact{Path: "agent.ortg", Data: []byte("// Exact source revision.\n" + topology)}
	sourceChanged := mustCreate(t, commented, fixture.options)
	if sourceChanged.Identity().GraphFingerprint != base.Identity().GraphFingerprint ||
		sourceChanged.Identity().SourceDigest == base.Identity().SourceDigest ||
		sourceChanged.Identity().PlanFingerprint == base.Identity().PlanFingerprint {
		t.Fatal("source provenance and executable graph identities were conflated")
	}

	valuesChanged := fixture.artifacts
	valuesChanged.Values = graphconfig.Artifact{Path: "values.json", Data: []byte(`{
  "apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable",
  "nodes":{"source":{"model":"slow","budget":32}}
}`)}
	differentValues := mustCreate(t, valuesChanged, fixture.options)
	if differentValues.Identity().ValuesDigest == base.Identity().ValuesDigest ||
		differentValues.Identity().GraphFingerprint == base.Identity().GraphFingerprint ||
		differentValues.Identity().LockDigest != base.Identity().LockDigest {
		t.Fatal("values did not change only their expected identity axis")
	}

	channelsChanged := fixture.artifacts
	channelsChanged.Channels = graphconfig.Artifact{Path: "channels.yaml", Data: []byte(`apiVersion: openrealtime.ai/channels/v1alpha1
graph: deployable
edges:
  source.out->sink.in: {depth: 9}
`)}
	differentChannels := mustCreate(t, channelsChanged, fixture.options)
	if differentChannels.Identity().ChannelsDigest == base.Identity().ChannelsDigest ||
		differentChannels.Identity().GraphFingerprint == base.Identity().GraphFingerprint ||
		differentChannels.Graph().Edges[0].Depth != 9 {
		t.Fatal("channel overlay did not enter compiled identity")
	}

	placementChanged := fixture.artifacts
	placementChanged.Deployment.Data = []byte(strings.ReplaceAll(
		string(placementChanged.Deployment.Data), "placement: llm-pool", "placement: gpu-pool"))
	placementDiscovery := cloneDiscovery(t, fixture, "gpu-pool")
	placementOptions := fixture.options
	placementOptions.Discovery = placementDiscovery
	differentPlacement := mustCreate(t, placementChanged, placementOptions)
	if differentPlacement.Identity().PublicDeploymentDigest == base.Identity().PublicDeploymentDigest ||
		differentPlacement.Identity().GraphFingerprint == base.Identity().GraphFingerprint {
		t.Fatal("non-secret deployment change retained public identity")
	}

	rotatedArtifacts := fixture.artifacts
	rotatedArtifacts.Deployment.Data = []byte(strings.ReplaceAll(
		string(rotatedArtifacts.Deployment.Data), "secret://providers/source/api-key", "secret://providers/source/rotated"))
	rotatedOptions := fixture.options
	rotatedOptions.SecretCatalog = &graphsecret.Document{
		APIVersion: graphsecret.APIVersion, Catalog: "deployable",
		Secrets: map[string]graphsecret.Binding{
			"secret://providers/source/rotated": {Provider: "env", Locator: "ROTATED_SOURCE_KEY"},
		},
	}
	rotated := mustCreate(t, rotatedArtifacts, rotatedOptions)
	if rotated.DeploymentFingerprint() == base.DeploymentFingerprint() {
		t.Fatal("secret reference rotation retained private deployment identity")
	}
	if rotated.SecretCatalogFingerprint() == base.SecretCatalogFingerprint() {
		t.Fatal("secret catalog rotation retained private catalog identity")
	}
	if rotated.Identity() != base.Identity() {
		t.Fatal("secret reference rotation leaked into public plan identity")
	}
}

func TestTopologyFrontendsShareExecutableIdentityAndRetainSourceProvenance(t *testing.T) {
	fixture := newFixture(t)
	base := mustCreate(t, fixture.artifacts, fixture.options)
	parsed, err := syntax.Parse("agent.ortg", []byte(topology))
	if err != nil {
		t.Fatal(err)
	}
	document := manifest.FromSyntax(parsed)
	jsonSource, err := manifest.MarshalJSON(document)
	if err != nil {
		t.Fatal(err)
	}
	yamlSource, err := manifest.MarshalYAML(document)
	if err != nil {
		t.Fatal(err)
	}

	frontends := []graphconfig.Artifact{
		{Path: "agent.json", Data: jsonSource},
		{Path: "agent.yaml", Data: yamlSource},
	}
	seenSources := map[string]struct{}{base.Identity().SourceDigest: {}}
	for _, frontend := range frontends {
		artifacts := fixture.artifacts
		artifacts.Topology = frontend
		plan := mustCreate(t, artifacts, fixture.options)
		if plan.Identity().GraphFingerprint != base.Identity().GraphFingerprint ||
			plan.Identity().LockDigest != base.Identity().LockDigest ||
			plan.Identity().ValuesDigest != base.Identity().ValuesDigest ||
			plan.Identity().PublicDeploymentDigest != base.Identity().PublicDeploymentDigest {
			t.Fatalf("frontend %s changed executable identity:\nbase=%+v\nplan=%+v",
				frontend.Path, base.Identity(), plan.Identity())
		}
		if plan.Identity().SourceDigest == base.Identity().SourceDigest ||
			plan.Identity().PlanFingerprint == base.Identity().PlanFingerprint {
			t.Fatalf("frontend %s lost exact source provenance", frontend.Path)
		}
		if _, duplicate := seenSources[plan.Identity().SourceDigest]; duplicate {
			t.Fatalf("frontend %s shares an exact source identity", frontend.Path)
		}
		seenSources[plan.Identity().SourceDigest] = struct{}{}
	}
}

func TestResolutionFingerprintIsCanonicalAndRejectsDuplicates(t *testing.T) {
	fixture := newFixture(t)
	plan := mustCreate(t, fixture.artifacts, fixture.options)
	resolution := plan.Resolution()
	for left, right := 0, len(resolution.Nodes)-1; left < right; left, right = left+1, right-1 {
		resolution.Nodes[left], resolution.Nodes[right] = resolution.Nodes[right], resolution.Nodes[left]
	}
	fingerprint, err := resolution.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != plan.Identity().ResolutionDigest {
		t.Fatalf("resolution order changed identity: %s / %s", fingerprint, plan.Identity().ResolutionDigest)
	}
	scopeChanged := plan.Resolution()
	scopeChanged.Dependencies[0].Scope = graphconfig.DependencyScopeMount
	scopeFingerprint, err := scopeChanged.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if scopeFingerprint == plan.Identity().ResolutionDigest {
		t.Fatal("dependency scope did not enter the immutable resolution identity")
	}
	resolution.Nodes = append(resolution.Nodes, resolution.Nodes[0])
	if _, err := resolution.Fingerprint(); err == nil || !strings.Contains(err.Error(), "repeats node") {
		t.Fatalf("duplicate resolution error = %v", err)
	}
}

func TestDependencyDiscoveryRequiresExplicitCanonicalScope(t *testing.T) {
	catalog := graphconfig.NewStaticDiscovery()
	base := graphconfig.DependencyResolution{
		Name: "service.scoped", Artifact: artifact("service/scoped", "1"),
	}
	if err := catalog.RegisterDependency(base); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("missing scope error = %v", err)
	}
	base.Scope = graphconfig.DependencyScope("session")
	if err := catalog.RegisterDependency(base); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("invalid scope error = %v", err)
	}
	base.Scope = graphconfig.DependencyScopeMount
	if err := catalog.RegisterDependency(base); err != nil {
		t.Fatalf("explicit mount scope: %v", err)
	}
}

func TestLockedCreateAndExplicitUpdateAreDistinct(t *testing.T) {
	fixture := newFixture(t)
	old := mustCreate(t, fixture.artifacts, fixture.options)
	newDescriptor := sourceDescriptor(2, 6)
	if err := fixture.catalog.Register(newDescriptor); err != nil {
		t.Fatal(err)
	}
	// Normal creation remains pinned to revision 1 even after discovery gains a
	// newer descriptor.
	pinned := mustCreate(t, fixture.artifacts, fixture.options)
	if pinned.Graph().Nodes[1].Element.Revision != old.Graph().Nodes[1].Element.Revision {
		t.Fatal("locked creation silently updated a descriptor")
	}
	updated, err := graphconfig.UpdateLock(context.Background(), fixture.artifacts.Topology,
		fixture.artifacts.Channels, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	identity, found := updated.Lock().Lookup("test.Source")
	if !found || identity.Revision != 2 || updated.Graph().Fingerprint == old.Graph().Fingerprint {
		t.Fatalf("explicit update = %+v / %s", identity, updated.Graph().Fingerprint)
	}

	forged := fixture.artifacts
	lock := fixtureLock(t, fixture.artifacts.Lock.Data)
	lock.Entries[0].Identity.Digest = "sha256:" + strings.Repeat("0", 64)
	forged.Lock.Data = mustLock(t, lock)
	if _, err := graphconfig.Create(context.Background(), forged, fixture.options); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("forged stale lock error = %v", err)
	}

	unused := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Unused", Revision: 1,
		Ports: []element.Port{{
			Name: "in", Direction: element.Input, Cardinality: element.One,
			Type: element.Event(element.Named("test.Text")), DefaultDepth: 1,
		}},
	}
	if err := fixture.catalog.Register(unused); err != nil {
		t.Fatal(err)
	}
	unusedIdentity, _ := unused.Identity()
	extra := fixture.artifacts
	lock = fixtureLock(t, fixture.artifacts.Lock.Data)
	lock.Entries = append(lock.Entries, resolve.Entry{Reference: unused.Name, Identity: unusedIdentity})
	extra.Lock.Data = mustLock(t, lock)
	if _, err := graphconfig.Create(context.Background(), extra, fixture.options); err == nil ||
		!strings.Contains(err.Error(), "exact minimal lock") {
		t.Fatalf("extra lock entry error = %v", err)
	}
}

func TestCreateFailsBeforeDiscoveryOnInvalidTypedValues(t *testing.T) {
	fixture := newFixture(t)
	spy := &spyDiscovery{inner: fixture.discovery}
	fixture.options.Discovery = spy
	for name, values := range map[string]string{
		"invalid enum":     `{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","nodes":{"source":{"model":"unknown","budget":32}}}`,
		"missing required": `{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","nodes":{}}`,
		"unknown property": `{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","nodes":{"source":{"model":"fast","extra":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			spy.calls.Store(0)
			artifacts := fixture.artifacts
			artifacts.Values = graphconfig.Artifact{Path: "values.json", Data: []byte(values)}
			if _, err := graphconfig.Create(context.Background(), artifacts, fixture.options); err == nil ||
				!strings.Contains(err.Error(), "element-owned schemas") {
				t.Fatalf("typed values error = %v", err)
			}
			if spy.calls.Load() != 0 {
				t.Fatalf("discovery ran %d times before typed values failed", spy.calls.Load())
			}
		})
	}

	fixture.options.SchemaResolver = nil
	if _, err := graphconfig.Create(context.Background(), fixture.artifacts, fixture.options); err == nil ||
		!strings.Contains(err.Error(), "config schema contract is unresolved") {
		t.Fatalf("unresolved schema error = %v", err)
	}
}

func TestCreateRejectsDeploymentDiscoveryAndSecretGaps(t *testing.T) {
	baseFixture := newFixture(t)
	for name, mutate := range map[string]func(*fixture){
		"missing discovery": func(value *fixture) { value.options.Discovery = nil },
		"missing secrets":   func(value *fixture) { value.options.SecretCatalog = nil },
		"missing dependency": func(value *fixture) {
			catalog := graphconfig.NewStaticDiscovery()
			copyImplementations(t, value.discovery, catalog)
			value.options.Discovery = catalog
		},
		"unsupported placement": func(value *fixture) {
			value.artifacts.Deployment.Data = []byte(strings.ReplaceAll(
				string(value.artifacts.Deployment.Data), "llm-pool", "other-pool"))
		},
		"unsupported transport": func(value *fixture) {
			value.artifacts.Deployment.Data = []byte(strings.ReplaceAll(
				string(value.artifacts.Deployment.Data), "transport: in-process", "transport: remote"))
		},
		"unsupported resource": func(value *fixture) {
			value.artifacts.Deployment.Data = []byte(strings.ReplaceAll(
				string(value.artifacts.Deployment.Data), "cpu: \"2\"", "gpu: shared"))
		},
		"unsupported secret slot": func(value *fixture) {
			value.artifacts.Deployment.Data = []byte(strings.ReplaceAll(
				string(value.artifacts.Deployment.Data), "apiKey: secret://", "token: secret://"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := baseFixture
			mutate(&value)
			if _, err := graphconfig.Create(context.Background(), value.artifacts, value.options); err == nil {
				t.Fatal("invalid deployment/discovery state was accepted")
			}
		})
	}

	declared := graphconfig.NewStaticDiscovery()
	for _, implementation := range baseFixture.discovery.Snapshot().Implementations {
		implementation.Evidence = inspect.EvidenceDeclared
		if err := declared.RegisterImplementation(implementation); err != nil {
			t.Fatal(err)
		}
	}
	for _, dependency := range baseFixture.discovery.Snapshot().Dependencies {
		if err := declared.RegisterDependency(dependency); err != nil {
			t.Fatal(err)
		}
	}
	declaredOptions := baseFixture.options
	declaredOptions.Discovery = declared
	if _, err := graphconfig.Create(context.Background(), baseFixture.artifacts, declaredOptions); err == nil ||
		!strings.Contains(err.Error(), "declaration-only") {
		t.Fatalf("declaration-only implementation error = %v", err)
	}
}

func TestArtifactParsingIsStrictAndBounded(t *testing.T) {
	fixture := newFixture(t)
	for name, mutate := range map[string]func(*graphconfig.Artifacts, *graphconfig.Options){
		"duplicate JSON key": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Values = graphconfig.Artifact{Path: "values.json", Data: []byte(`{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","graph":"other","nodes":{}}`)}
		},
		"wrong channels graph": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Channels.Data = []byte(strings.ReplaceAll(string(artifacts.Channels.Data), "graph: deployable", "graph: other"))
		},
		"unknown channel edge": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Channels.Data = []byte(strings.ReplaceAll(string(artifacts.Channels.Data), "source.out->sink.in", "missing.edge"))
		},
		"zero channel depth": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Channels.Data = []byte(strings.ReplaceAll(string(artifacts.Channels.Data), "depth: 8", "depth: 0"))
		},
		"YAML lock": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Lock = graphconfig.Artifact{Path: "lock.yaml", Data: []byte("format_version: 1\nelements: []\n")}
		},
		"topology byte bound": func(_ *graphconfig.Artifacts, options *graphconfig.Options) {
			options.Limits.MaxTopologyBytes = 1
		},
		"JSON depth bound": func(artifacts *graphconfig.Artifacts, options *graphconfig.Options) {
			options.Limits.MaxJSONDepth = 2
			artifacts.Values = graphconfig.Artifact{Path: "values.json", Data: []byte(`{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","nodes":{"source":{"model":"fast"}}}`)}
		},
		"noncanonical path": func(artifacts *graphconfig.Artifacts, _ *graphconfig.Options) {
			artifacts.Values.Path = " values.yaml "
		},
	} {
		t.Run(name, func(t *testing.T) {
			artifacts, options := fixture.artifacts, fixture.options
			mutate(&artifacts, &options)
			if _, err := graphconfig.Create(context.Background(), artifacts, options); err == nil {
				t.Fatal("strict/bounded artifact violation was accepted")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := graphconfig.Create(ctx, fixture.artifacts, fixture.options); err == nil {
		t.Fatal("cancelled creation was accepted")
	}
}

func TestStaticDiscoveryIsImmutableConcurrentMetadata(t *testing.T) {
	fixture := newFixture(t)
	snapshot := fixture.discovery.Snapshot()
	if len(snapshot.Implementations) != 2 || len(snapshot.Dependencies) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	snapshot.Implementations[0].Transports[0] = "mutated"
	if fixture.discovery.Snapshot().Implementations[0].Transports[0] == "mutated" {
		t.Fatal("discovery snapshot aliases catalog state")
	}
	changed := fixture.discovery.Snapshot().Implementations[0]
	changed.Artifact.Digest = "sha256:" + strings.Repeat("f", 64)
	if err := fixture.discovery.RegisterImplementation(changed); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mutable re-registration error = %v", err)
	}

	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_ = fixture.discovery.Snapshot()
				_, _ = fixture.discovery.ResolveImplementation(context.Background(), graphconfig.ImplementationRequest{Reference: "impl/source"})
				_, _ = fixture.discovery.ResolveDependency(context.Background(), "service.clock")
			}
		}()
	}
	wait.Wait()
}

func BenchmarkCreateLockedPlan(b *testing.B) {
	fixture := newFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		plan, err := graphconfig.Create(context.Background(), fixture.artifacts, fixture.options)
		if err != nil {
			b.Fatal(err)
		}
		if plan.Identity().PlanFingerprint == "" {
			b.Fatal("missing plan identity")
		}
	}
}

func FuzzCreateValuesArtifactNeverPanics(f *testing.F) {
	fixture := newFixture(f)
	for _, seed := range [][]byte{
		fixture.artifacts.Values.Data,
		[]byte(`{"apiVersion":"openrealtime.ai/config/v1alpha1","graph":"deployable","nodes":{}}`),
		[]byte(`{"graph":"a","graph":"b"}`),
		[]byte(strings.Repeat("[", 256) + strings.Repeat("]", 256)),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		artifacts := fixture.artifacts
		artifacts.Values = graphconfig.Artifact{Path: "fuzz-values.json", Data: source}
		options := fixture.options
		options.Limits.MaxValuesBytes = 1 << 20
		_, _ = graphconfig.Create(context.Background(), artifacts, options)
	})
}

type schemaResolver struct{}

func (schemaResolver) ResolveConfigSchema(_ context.Context, reference string) (schema.ResolvedSchema, error) {
	if reference != sourceSchemaID {
		return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
	}
	return schema.ResolvedSchema{ID: sourceSchemaID, Document: json.RawMessage(`{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$id":"https://schemas.openrealtime.test/source-config/v1",
  "type":"object",
  "required":["model"],
  "properties":{
    "model":{"type":"string","enum":["fast","slow"]},
    "budget":{"type":"integer","minimum":1,"maximum":4096}
  },
  "additionalProperties":false
}`)}, nil
}

type spyDiscovery struct {
	inner graphconfig.Discovery
	calls atomic.Int64
}

func (spy *spyDiscovery) ResolveImplementation(ctx context.Context, request graphconfig.ImplementationRequest) (graphconfig.ImplementationResolution, error) {
	spy.calls.Add(1)
	return spy.inner.ResolveImplementation(ctx, request)
}

func (spy *spyDiscovery) ResolveDependency(ctx context.Context, name string) (graphconfig.DependencyResolution, error) {
	spy.calls.Add(1)
	return spy.inner.ResolveDependency(ctx, name)
}

func sourceDescriptor(revision uint64, depth int) element.Descriptor {
	value := element.Event(element.Named("test.Text"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Source", Revision: revision,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true, DefaultDepth: depth},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, Required: true, DefaultDepth: depth},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}},
		ConfigSchema: sourceSchemaID,
		Dependencies: []element.Dependency{{Name: "service.clock"}},
		Effects:      []element.Effect{{Name: "cache.registration", Reversible: true}},
	}
}

func sinkDescriptor() element.Descriptor {
	value := element.Event(element.Named("test.Text"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Sink", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 4},
		},
		Reaction:     element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}},
		Dependencies: []element.Dependency{{Name: "service.optional", Optional: true}},
	}
}

func artifact(id, digit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: id, Revision: "1", Digest: "sha256:" + strings.Repeat(digit, 64)}
}

func mustRegisterImplementation(t testing.TB, catalog *graphconfig.StaticDiscovery, resolution graphconfig.ImplementationResolution) {
	t.Helper()
	if err := catalog.RegisterImplementation(resolution); err != nil {
		t.Fatal(err)
	}
}

func mustCreate(t testing.TB, artifacts graphconfig.Artifacts, options graphconfig.Options) *graphconfig.Plan {
	t.Helper()
	plan, err := graphconfig.Create(context.Background(), artifacts, options)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func fixtureLock(t testing.TB, payload []byte) resolve.Lock {
	t.Helper()
	lock, err := resolve.ParseLock(payload)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func mustLock(t testing.TB, lock resolve.Lock) []byte {
	t.Helper()
	payload, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func copyImplementations(t testing.TB, source, target *graphconfig.StaticDiscovery) {
	t.Helper()
	for _, implementation := range source.Snapshot().Implementations {
		mustRegisterImplementation(t, target, implementation)
	}
}

func cloneDiscovery(t testing.TB, fixture fixture, placement string) *graphconfig.StaticDiscovery {
	t.Helper()
	result := graphconfig.NewStaticDiscovery()
	for _, implementation := range fixture.discovery.Snapshot().Implementations {
		if implementation.Reference == "impl/source" {
			implementation.Placements = append(implementation.Placements, placement)
		}
		mustRegisterImplementation(t, result, implementation)
	}
	for _, dependency := range fixture.discovery.Snapshot().Dependencies {
		if err := result.RegisterDependency(dependency); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
