package catalog_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const catalogTopology = `graph text_agent {
	catalog.Pass :: pass;
	input text = pass.in;
	output reply = pass.out;
}
`

func TestEntryDerivesExactAudioFreeContractFromPlan(t *testing.T) {
	plan := catalogPlan(t, 1)
	tags := []string{"text", "realtime"}
	profiles := []inspect.ArtifactIdentity{catalogArtifact("profile/text-latency", "a")}
	entry, err := graphcatalog.NewEntry(plan, graphcatalog.Metadata{
		Stage: graphcatalog.Candidate, Summary: "Text-only graph for catalog discovery.",
		Change: "Initial immutable graph-native revision.", Tags: tags, Profiles: profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	tags[0] = "mutated"
	profiles[0].ID = "mutated"
	if entry.GraphID != "text_agent" || entry.Revision != 1 ||
		!strings.HasPrefix(entry.Fingerprint, "sha256:") || entry.Plan != plan.Identity() {
		t.Fatalf("entry identity = %+v", entry)
	}
	if err := entry.Graph.Validate(); err != nil {
		t.Fatalf("entry Graph IR: %v", err)
	}
	if len(entry.Boundaries) != 2 || entry.Boundaries[0].Type != "Event<test.Text>" ||
		entry.Boundaries[1].Type != "Event<test.Text>" {
		t.Fatalf("boundaries = %+v", entry.Boundaries)
	}
	for _, boundary := range entry.Boundaries {
		if strings.Contains(strings.ToLower(boundary.Type), "audio") {
			t.Fatalf("audio-free contract acquired audio: %+v", boundary)
		}
	}
	if len(entry.Nodes) != 1 || entry.Nodes[0].Implementation.Reference != "impl/pass" ||
		entry.Nodes[0].StateTransfer == nil || !entry.Nodes[0].StateTransfer.Snapshot ||
		!entry.Nodes[0].StateTransfer.Restore || !entry.Nodes[0].StateTransfer.Quiesce ||
		len(entry.Dependencies) != 1 || entry.Dependencies[0].Artifact == nil ||
		entry.Dependencies[0].Scope != graphconfig.DependencyScopeProcess ||
		len(entry.Effects) != 1 || entry.Effects[0].Name != "local.cache" {
		t.Fatalf("derived metadata = %+v / %+v / %+v", entry.Nodes, entry.Dependencies, entry.Effects)
	}
	if entry.Tags[0] == "mutated" || entry.Profiles[0].ID == "mutated" {
		t.Fatal("entry aliased metadata input")
	}
	entry.Nodes[0].StateTransfer.Restore = false
	if !entry.Graph.Nodes[0].StateTransfer.Restore {
		t.Fatal("catalog node metadata aliases Graph IR state-transfer capabilities")
	}
}

func TestEntryPreservesDependencyScopeInIdentityAndRoundTrip(t *testing.T) {
	process, err := graphcatalog.NewEntry(
		catalogPlanWithScope(t, 1, graphconfig.DependencyScopeProcess),
		graphcatalog.Metadata{
			Stage: graphcatalog.Candidate, Summary: "Process dependency catalog entry.",
			Change: "Record exact dependency scope.",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	mount, err := graphcatalog.NewEntry(
		catalogPlanWithScope(t, 1, graphconfig.DependencyScopeMount),
		graphcatalog.Metadata{
			Stage: graphcatalog.Candidate, Summary: "Mount dependency catalog entry.",
			Change: "Record exact dependency scope.",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mount.Dependencies) != 1 ||
		mount.Dependencies[0].Scope != graphconfig.DependencyScopeMount {
		t.Fatalf("mount dependency metadata = %+v", mount.Dependencies)
	}
	if mount.Plan.ResolutionDigest == process.Plan.ResolutionDigest ||
		mount.Fingerprint == process.Fingerprint {
		t.Fatal("dependency scope did not enter plan and catalog identities")
	}
	document, err := graphcatalog.Freeze([]graphcatalog.Entry{mount})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := document.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := graphcatalog.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Entries[0].Dependencies[0].Scope; got != graphconfig.DependencyScopeMount {
		t.Fatalf("round-trip dependency scope = %q", got)
	}
}

func TestEntryAcceptsEffectFreeProductionPlanAndRoundTrips(t *testing.T) {
	entry, err := graphcatalog.NewEntry(
		catalogPlanWithOptions(t, 1, graphconfig.DependencyScopeProcess, false),
		graphcatalog.Metadata{
			Stage: graphcatalog.Candidate, Summary: "Effect-free catalog entry.",
			Change: "Exercise an exact plan with no declared effects.",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.Effects) != 0 {
		t.Fatalf("effect-free entry effects = %+v", entry.Effects)
	}
	document, err := graphcatalog.Freeze([]graphcatalog.Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := document.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graphcatalog.Parse(payload); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogIsDeterministicStrictAndSecretRedacted(t *testing.T) {
	first := catalogEntry(t, 1, graphcatalog.Candidate, []string{"text", "realtime"})
	second := catalogEntry(t, 2, graphcatalog.Stable, []string{"text", "production"})
	left, err := graphcatalog.Freeze([]graphcatalog.Entry{second, first})
	if err != nil {
		t.Fatal(err)
	}
	right, err := graphcatalog.Freeze([]graphcatalog.Entry{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if left.Fingerprint != right.Fingerprint {
		t.Fatalf("catalog order changed identity: %s / %s", left.Fingerprint, right.Fingerprint)
	}
	payload, err := left.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := graphcatalog.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	secondPayload, err := parsed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(secondPayload) {
		t.Fatalf("catalog round trip changed bytes:\n%s\n%s", payload, secondPayload)
	}
	privateFingerprint := catalogPlan(t, 1).DeploymentFingerprint()
	privateSecretFingerprint := catalogPlan(t, 1).SecretCatalogFingerprint()
	for _, forbidden := range []string{
		"secret://catalog/token", "CATALOG_TOKEN", privateFingerprint, privateSecretFingerprint,
	} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("public catalog leaked %q:\n%s", forbidden, payload)
		}
	}
}

func TestCatalogDiscoveryUsesExactGraphContractsAndReturnsClones(t *testing.T) {
	first := catalogEntry(t, 1, graphcatalog.Candidate, []string{"text", "realtime"})
	second := catalogEntry(t, 2, graphcatalog.Stable, []string{"text", "production"})
	document, err := graphcatalog.Freeze([]graphcatalog.Entry{second, first})
	if err != nil {
		t.Fatal(err)
	}
	results, err := document.Discover(graphcatalog.Query{
		Stages: []graphcatalog.Stage{graphcatalog.Candidate}, Tags: []string{"realtime"},
		InputTypes: []string{"Event<test.Text>"}, OutputTypes: []string{"Event<test.Text>"},
		Effects: []string{"local.cache"}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Revision != 1 {
		t.Fatalf("discovery results = %+v", results)
	}
	results[0].Tags[0] = "mutated"
	results[0].Graph.Nodes[0].ID = "mutated"
	results[0].Nodes[0].Implementation.SecretSlots[0] = "mutated"
	again, err := document.Discover(graphcatalog.Query{Stages: []graphcatalog.Stage{graphcatalog.Candidate}})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Tags[0] == "mutated" || again[0].Graph.Nodes[0].ID == "mutated" ||
		again[0].Nodes[0].Implementation.SecretSlots[0] == "mutated" {
		t.Fatal("discovery result aliases catalog state")
	}
	if _, err := document.Lookup(first.GraphID, first.Revision, first.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Lookup(first.GraphID, first.Revision, "sha256:"+strings.Repeat("0", 64)); err == nil {
		t.Fatal("lookup accepted a mutable or forged graph revision selector")
	}
	if _, err := document.Discover(graphcatalog.Query{Limit: 1001}); err == nil {
		t.Fatal("unbounded discovery query was accepted")
	}
}

func TestCatalogRejectsMetadataDriftTamperingAndAmbiguousJSON(t *testing.T) {
	entry := catalogEntry(t, 1, graphcatalog.Candidate, []string{"text"})
	for name, mutate := range map[string]func(*graphcatalog.Entry){
		"boundary drift": func(value *graphcatalog.Entry) { value.Boundaries[0].Type = "Event<test.Audio>" },
		"node drift":     func(value *graphcatalog.Entry) { value.Nodes[0].Implementation.Reference = "impl/other" },
		"artifact drift": func(value *graphcatalog.Entry) {
			value.Nodes[0].Implementation.Artifact.Digest = "sha256:" + strings.Repeat("f", 64)
		},
		"dependency artifact drift": func(value *graphcatalog.Entry) {
			artifact := *value.Dependencies[0].Artifact
			artifact.Digest = "sha256:" + strings.Repeat("f", 64)
			value.Dependencies[0].Artifact = &artifact
		},
		"dependency scope drift": func(value *graphcatalog.Entry) {
			value.Dependencies[0].Scope = graphconfig.DependencyScopeMount
		},
		"effect drift":  func(value *graphcatalog.Entry) { value.Effects = nil },
		"channel drift": func(value *graphcatalog.Entry) { value.Channels.Count++ },
		"lineage drift": func(value *graphcatalog.Entry) { value.Lineage = []string{"forged"} },
		"invalid tag":   func(value *graphcatalog.Entry) { value.Tags = []string{"Not Canonical"} },
		"invalid stage": func(value *graphcatalog.Entry) { value.Stage = "preview" },
		"invalid profile": func(value *graphcatalog.Entry) {
			value.Profiles = []inspect.ArtifactIdentity{{ID: "profile/latest", Revision: "latest"}}
		},
		"duplicate profile": func(value *graphcatalog.Entry) {
			value.Profiles = append(value.Profiles, value.Profiles[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := entry
			changed.Boundaries = append([]graphcatalog.Boundary(nil), entry.Boundaries...)
			changed.Nodes = append([]graphcatalog.Node(nil), entry.Nodes...)
			changed.Dependencies = append([]graphcatalog.Dependency(nil), entry.Dependencies...)
			changed.Effects = append([]element.Effect(nil), entry.Effects...)
			changed.Tags = append([]string(nil), entry.Tags...)
			changed.Profiles = append([]inspect.ArtifactIdentity(nil), entry.Profiles...)
			mutate(&changed)
			if _, err := graphcatalog.Freeze([]graphcatalog.Entry{changed}); err == nil {
				t.Fatal("drifted catalog metadata was accepted")
			}
		})
	}
	if _, err := graphcatalog.Freeze([]graphcatalog.Entry{entry, entry}); err == nil ||
		!strings.Contains(err.Error(), "repeats immutable graph revision") {
		t.Fatalf("duplicate revision error = %v", err)
	}

	document, err := graphcatalog.Freeze([]graphcatalog.Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := document.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(payload), entry.Summary, "Edited in place.", 1)
	if _, err := graphcatalog.Parse([]byte(tampered)); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered catalog error = %v", err)
	}
	duplicate := `{"format_version":1,"format_version":1,"fingerprint":"sha256:` + strings.Repeat("0", 64) + `","entries":[]}`
	if _, err := graphcatalog.Parse([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate JSON error = %v", err)
	}
	unknown := strings.Replace(string(payload), `"entries":`, `"unknown":true,"entries":`, 1)
	if _, err := graphcatalog.Parse([]byte(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown JSON field error = %v", err)
	}
	deep := strings.Repeat("[", 129) + strings.Repeat("]", 129)
	if _, err := graphcatalog.Parse([]byte(deep)); err == nil || !strings.Contains(err.Error(), "nesting depth") {
		t.Fatalf("deep JSON error = %v", err)
	}
}

func BenchmarkCatalogFreeze(b *testing.B) {
	entries := make([]graphcatalog.Entry, 16)
	for index := range entries {
		entries[index] = catalogEntry(b, uint64(index+1), graphcatalog.Candidate, []string{"text", "benchmark"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		document, err := graphcatalog.Freeze(entries)
		if err != nil {
			b.Fatal(err)
		}
		if document.Fingerprint == "" {
			b.Fatal("missing fingerprint")
		}
	}
}

func BenchmarkCatalogDiscover(b *testing.B) {
	entries := make([]graphcatalog.Entry, 64)
	for index := range entries {
		entries[index] = catalogEntry(b, uint64(index+1), graphcatalog.Candidate, []string{"text", "benchmark"})
	}
	document, err := graphcatalog.Freeze(entries)
	if err != nil {
		b.Fatal(err)
	}
	query := graphcatalog.Query{
		Tags: []string{"benchmark"}, InputTypes: []string{"Event<test.Text>"}, Limit: 64,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		results, discoverErr := document.Discover(query)
		if discoverErr != nil || len(results) != 64 {
			b.Fatalf("discover = %d, %v", len(results), discoverErr)
		}
	}
}

func FuzzCatalogParseNeverPanics(f *testing.F) {
	document, err := graphcatalog.Freeze([]graphcatalog.Entry{
		catalogEntry(f, 1, graphcatalog.Candidate, []string{"text"}),
	})
	if err != nil {
		f.Fatal(err)
	}
	payload, err := document.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{
		payload,
		[]byte(`{"format_version":1,"format_version":1}`),
		[]byte(strings.Repeat("[", 256) + strings.Repeat("]", 256)),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		_, _ = graphcatalog.Parse(source)
	})
}

func catalogEntry(t testing.TB, revision uint64, stage graphcatalog.Stage, tags []string) graphcatalog.Entry {
	t.Helper()
	entry, err := graphcatalog.NewEntry(catalogPlan(t, revision), graphcatalog.Metadata{
		Stage: stage, Summary: "A graph-native text agent.",
		Change: "Immutable catalog revision.", Tags: tags,
		Profiles: []inspect.ArtifactIdentity{catalogArtifact("profile/text", "a")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func catalogPlan(t testing.TB, revision uint64) *graphconfig.Plan {
	return catalogPlanWithScope(t, revision, graphconfig.DependencyScopeProcess)
}

func catalogPlanWithScope(
	t testing.TB,
	revision uint64,
	dependencyScope graphconfig.DependencyScope,
) *graphconfig.Plan {
	return catalogPlanWithOptions(t, revision, dependencyScope, true)
}

func catalogPlanWithOptions(
	t testing.TB,
	revision uint64,
	dependencyScope graphconfig.DependencyScope,
	withEffects bool,
) *graphconfig.Plan {
	t.Helper()
	descriptor := catalogDescriptor()
	if !withEffects {
		descriptor.Effects = nil
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	descriptors := resolve.NewCatalog()
	if err := descriptors.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	discovery := graphconfig.NewStaticDiscovery()
	if err := discovery.RegisterImplementation(graphconfig.ImplementationResolution{
		Reference: "impl/pass", Contract: identity,
		Artifact: catalogArtifact("implementation/pass", "b"), Evidence: inspect.EvidenceRegistered,
		SecretSlots: []string{"token"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := discovery.RegisterDependency(graphconfig.DependencyResolution{
		Name: "service.clock", Artifact: catalogArtifact("service/clock", "c"),
		Scope: dependencyScope,
	}); err != nil {
		t.Fatal(err)
	}
	options := graphconfig.Options{
		Catalog: descriptors, Discovery: discovery, Revision: revision,
		SecretCatalog: &graphsecret.Document{
			APIVersion: graphsecret.APIVersion, Catalog: "text-agent",
			Secrets: map[string]graphsecret.Binding{
				"secret://catalog/token": {Provider: "env", Locator: "CATALOG_TOKEN"},
			},
		},
	}
	topologyArtifact := graphconfig.Artifact{Path: "agent.ortg", Data: []byte(catalogTopology)}
	updated, err := graphconfig.UpdateLock(context.Background(), topologyArtifact, graphconfig.Artifact{}, options)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := updated.Lock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := graphconfig.Create(context.Background(), graphconfig.Artifacts{
		Topology: topologyArtifact,
		Values: graphconfig.Artifact{Path: "values.yaml", Data: []byte(`apiVersion: openrealtime.ai/config/v1alpha1
graph: text_agent
nodes: {}
`)},
		Lock: graphconfig.Artifact{Path: "openrealtime.lock", Data: lock},
		Deployment: graphconfig.Artifact{Path: "deployment.yaml", Data: []byte(`apiVersion: openrealtime.ai/deployment/v1alpha1
graph: text_agent
nodes:
  pass:
    implementation: impl/pass
    secrets:
      token: secret://catalog/token
`)},
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func catalogDescriptor() element.Descriptor {
	value := element.Event(element.Named("test.Text"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "catalog.Pass", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 4},
		},
		Reaction:    element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}},
		StateSchema: "schema://test/catalog-state/v1",
		StateTransfer: &element.StateTransferCapabilities{
			Snapshot: true, Restore: true, Quiesce: true,
		},
		Dependencies: []element.Dependency{{Name: "service.clock"}},
		Effects:      []element.Effect{{Name: "local.cache", Reversible: true}},
	}
}

func catalogArtifact(id, digit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: id, Revision: "1", Digest: "sha256:" + strings.Repeat(digit, 64)}
}
