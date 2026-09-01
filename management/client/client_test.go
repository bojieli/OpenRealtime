package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/management"
	managementclient "github.com/bojieli/OpenRealtime/management/client"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/plugin"
)

const clientSource = `graph managed-client {
    test.ClientSource :: source;
    test.ClientSink :: sink;
    source.out -> sink.in;
}
`

type recordedSession struct {
	graph ir.Graph
	live  inspect.Live
	trace inspect.LiveTrace
}

func (session *recordedSession) Graph() ir.Graph    { return session.graph }
func (session *recordedSession) Live() inspect.Live { return session.live.Clone() }
func (session *recordedSession) RecordedTrace() (inspect.LiveTrace, error) {
	return session.trace.Clone(), nil
}

type reconciler struct{}

type clientSchemaResolver func(context.Context, string) (schema.ResolvedSchema, error)

func (resolver clientSchemaResolver) ResolveConfigSchema(ctx context.Context, reference string) (schema.ResolvedSchema, error) {
	return resolver(ctx, reference)
}

func (reconciler) Apply(
	_ context.Context, request management.ReconciliationRequest,
) (management.ReconciliationReceipt, error) {
	return management.ReconciliationReceipt{
		FormatVersion: 1, SessionID: request.SessionID,
		PreviousFingerprint: request.ExpectedFingerprint, CandidateFingerprint: request.Candidate.Fingerprint,
		State: "applied", SafePointSequence: 9,
	}, nil
}

func TestClientExercisesTheCompleteMountedManagementAPI(t *testing.T) {
	elements, graph, lock := compileFixture(t)
	plugins := plugin.NewCatalog()
	pluginIdentity, err := plugins.Register(plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "test.management.ClientPlugin", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{{
			Name: "test.management.client_service", Revision: 1,
			Digest: "sha256:" + strings.Repeat("f", 64),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	values, err := schema.Generate(context.Background(), graph, elements, schema.Options{})
	if err != nil {
		t.Fatal(err)
	}
	static, err := management.NewCatalog(elements, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if err := static.RegisterGraph(graph, values); err != nil {
		t.Fatal(err)
	}
	authoring, err := management.NewAuthoringEngine(management.AuthoringOptions{
		Catalog: elements,
		SchemaResolver: clientSchemaResolver(func(_ context.Context, reference string) (schema.ResolvedSchema, error) {
			if reference != "schema://test/client-source/v1" {
				return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
			}
			return schema.ResolvedSchema{
				ID: "https://schemas.example.test/client-source-v1.json",
				Document: json.RawMessage(`{
                    "$id":"https://schemas.example.test/client-source-v1.json",
                    "type":"object","required":["model"],
                    "properties":{"model":{"type":"string"}},"additionalProperties":false
                }`),
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	publicationRoot := t.TempDir()
	publicationIdentity := "sha256:" + strings.Repeat("c", 64)
	publication, err := management.NewRootedSourcePublisher(management.RootedSourcePublisherOptions{
		Root: publicationRoot, RootIdentity: publicationIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publication.Close(); err != nil {
			t.Errorf("close client source publisher: %v", err)
		}
	})

	live, trace := liveFixture(t, graph)
	sessions := management.NewSessionRegistry()
	disposeSession, err := sessions.Register("sess-client", &recordedSession{graph: graph, live: live, trace: trace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(disposeSession)

	authority := management.NewCapabilityRegistry()
	operations := []management.Operation{
		management.ReadGraph, management.ReadDescriptor, management.ReadSchema,
		management.ReadSession, management.ReadTrace, management.AnalyzeDocument,
		management.CompileDocument, management.RenderGraph, management.CreateSource,
		management.UpdateSource, management.ApplyCandidate,
	}
	grants := make([]management.Grant, len(operations))
	for index, operation := range operations {
		grants[index] = management.Grant{Operation: operation, Resource: "*"}
	}
	access, err := authority.Issue(time.Minute, grants)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := managementserver.NewBundle(managementserver.BundleConfig{
		Authorizer: authority, StaticCatalog: static, Sessions: sessions,
		Authoring: authoring, SourcePublication: publication, Reconciliation: reconciler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := mounted.Close(ctx); err != nil {
			t.Errorf("close management profile: %v", err)
		}
	})
	handler, err := managementserver.HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	t.Cleanup(host.Close)
	remote, err := managementclient.New(managementclient.Config{
		BaseURL: host.URL, Capabilities: managementclient.StaticCapability(access.Token),
	})
	if err != nil {
		t.Fatal(err)
	}

	gotGraph, err := remote.Graph(context.Background(), graph.Fingerprint)
	if err != nil || gotGraph.Fingerprint != graph.Fingerprint {
		t.Fatalf("graph = %s, %v", gotGraph.Fingerprint, err)
	}
	elementIdentity := graph.Nodes[0].Element
	gotElement, err := remote.ElementDescriptor(context.Background(), elementIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if identity, identityErr := gotElement.Identity(); identityErr != nil || identity != elementIdentity {
		t.Fatalf("element descriptor identity = %+v, %v", identity, identityErr)
	}
	gotPlugin, err := remote.PluginDescriptor(context.Background(), pluginIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if identity, identityErr := gotPlugin.Identity(); identityErr != nil || identity != pluginIdentity {
		t.Fatalf("plugin descriptor identity = %+v, %v", identity, identityErr)
	}
	gotSchema, err := remote.ValuesSchema(context.Background(), graph.Fingerprint)
	if err != nil || gotSchema.Digest != values.Digest {
		t.Fatalf("values schema = %s, %v", gotSchema.Digest, err)
	}

	snapshot, err := remote.Snapshot(context.Background(), "sess-client")
	if err != nil || snapshot.Fingerprint != graph.Fingerprint {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	if snapshot.Nodes["source"].LastOutcome != "" {
		t.Fatal("headless client exposed a payload-derived live field")
	}
	model, err := remote.Model(context.Background(), "sess-client")
	if err != nil || model.Fingerprint != snapshot.Fingerprint || model.GraphID != snapshot.GraphID ||
		len(model.Nodes) != len(snapshot.Nodes) {
		t.Fatalf("session model = %+v, %v", model, err)
	}
	page, err := remote.Deltas(context.Background(), "sess-client", 0, 8)
	if err != nil || page.Baseline == nil || page.Next != live.Sequence {
		t.Fatalf("delta page = %+v, %v", page, err)
	}
	recording, err := remote.Trace(context.Background(), "sess-client")
	if err != nil || recording.Fingerprint != trace.Fingerprint {
		t.Fatalf("trace = %s, %v", recording.Fingerprint, err)
	}

	document := management.AuthoringDocument{Path: "agent.ortg", Source: clientSource, Lock: &lock}
	analysis, err := remote.Analyze(context.Background(), document)
	if err != nil || !analysis.Parsed || !analysis.Canonical || analysis.Formatting == nil {
		t.Fatalf("analysis = %+v, %v", analysis, err)
	}
	var resolvedProperties int
	for _, metadata := range analysis.Catalog.Elements {
		if metadata.Identity.Name == "test.ClientSource" && metadata.Config.SchemaStatus == "resolved" {
			resolvedProperties = len(metadata.Config.Properties)
		}
	}
	if resolvedProperties != 1 {
		t.Fatalf("resolved property metadata did not survive the API: %+v", analysis.Catalog)
	}
	partial := management.AuthoringDocument{
		Path: "partial.ortg",
		Source: `graph managed-client {
    test.ClientSource :: source;
    source.
`,
	}
	recovery, err := remote.Analyze(context.Background(), partial)
	if err != nil || recovery.Parsed || !recovery.Recovered || recovery.Formatting != nil ||
		resolvedConfigProperties(recovery, "test.ClientSource") != 1 {
		t.Fatalf("remote recovery analysis = %+v, %v", recovery, err)
	}
	if compiledRecovery, err := remote.Compile(context.Background(), partial); !errors.Is(err, management.ErrInvalid) ||
		compiledRecovery.Graph.Fingerprint != "" {
		t.Fatalf("remote recovery crossed compile boundary: %+v, %v", compiledRecovery, err)
	}
	compiled, err := remote.Compile(context.Background(), document)
	if err != nil || compiled.Graph.Fingerprint != graph.Fingerprint || !compiled.Lock.Equal(lock) {
		t.Fatalf("compile = %+v, %v", compiled, err)
	}
	rendered, err := remote.Render(context.Background(), management.RenderRequest{
		Graph: graph, Format: management.RenderMermaid,
	})
	if err != nil || rendered.Fingerprint != graph.Fingerprint || !strings.Contains(rendered.Text, graph.Fingerprint) {
		t.Fatalf("render = %+v, %v", rendered, err)
	}
	createSource := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  publicationIdentity,
		Mode:          management.SourceCreate,
		Path:          "client.ortg",
		Source:        "graph client_write {\n}\n",
	}
	createdSource, err := remote.Publish(context.Background(), createSource)
	if err != nil || management.ValidateSourceWriteReceipt(createSource, createdSource) != nil {
		t.Fatalf("source create receipt = %+v, %v", createdSource, err)
	}
	updateSource := createSource
	updateSource.Mode = management.SourceUpdate
	updateSource.Source = "graph client_update {\n}\n"
	updateSource.ExpectedSourceDigest = createdSource.SourceDigest
	updatedSource, err := remote.Publish(context.Background(), updateSource)
	if err != nil || management.ValidateSourceWriteReceipt(updateSource, updatedSource) != nil {
		t.Fatalf("source update receipt = %+v, %v", updatedSource, err)
	}
	published, err := os.ReadFile(filepath.Join(publicationRoot, createSource.Path))
	if err != nil || string(published) != updateSource.Source {
		t.Fatalf("published source = %q, %v", published, err)
	}
	receipt, err := remote.Apply(context.Background(), management.ReconciliationRequest{
		SessionID: "sess-client", ExpectedFingerprint: graph.Fingerprint, Candidate: graph,
		ValuesFingerprint:     "sha256:" + strings.Repeat("a", 64),
		DeploymentFingerprint: "sha256:" + strings.Repeat("b", 64),
	})
	if err != nil || receipt.SafePointSequence != 9 {
		t.Fatalf("reconciliation receipt = %+v, %v", receipt, err)
	}
}

func TestClientRefusesRedirectsAndNonStrictResponses(t *testing.T) {
	var reached atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Add(1)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	remote, err := managementclient.New(managementclient.Config{
		BaseURL: redirect.URL, Capabilities: managementclient.StaticCapability("narrow-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "sha256:" + strings.Repeat("1", 64)
	if _, err := remote.Graph(context.Background(), fingerprint); !errors.Is(err, management.ErrUnavailable) {
		t.Fatalf("redirect error = %v", err)
	}
	if reached.Load() != 0 {
		t.Fatal("management client followed a redirect and risked forwarding its capability")
	}

	duplicate := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"format_version":1,"format_version":1}`))
	}))
	t.Cleanup(duplicate.Close)
	remote, err = managementclient.New(managementclient.Config{
		BaseURL: duplicate.URL, Capabilities: managementclient.StaticCapability("narrow-secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Graph(context.Background(), fingerprint); !errors.Is(err, management.ErrConflict) {
		t.Fatalf("duplicate-key response error = %v", err)
	}
}

func TestClientConfigurationFailsClosed(t *testing.T) {
	var nilSource managementclient.CapabilityFunc
	for _, config := range []managementclient.Config{
		{BaseURL: "http://example.com", Capabilities: managementclient.StaticCapability("token")},
		{BaseURL: "https://user@example.com", Capabilities: managementclient.StaticCapability("token")},
		{BaseURL: "https://example.com/prefix", Capabilities: managementclient.StaticCapability("token")},
		{BaseURL: "https://example.com", Capabilities: nilSource},
	} {
		if _, err := managementclient.New(config); !errors.Is(err, management.ErrInvalid) {
			t.Fatalf("configuration %+v error = %v", config, err)
		}
	}
}

func compileFixture(t *testing.T) (*resolve.Catalog, ir.Graph, resolve.Lock) {
	t.Helper()
	catalog := resolve.NewCatalog()
	value := element.Event(element.Named("test.ClientValue"))
	for _, descriptor := range []element.Descriptor{
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.ClientSource", Revision: 1,
			ConfigSchema: "schema://test/client-source/v1",
			Ports:        []element.Port{{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One}},
		},
		{
			FormatVersion: element.DescriptorFormatVersion, Name: "test.ClientSink", Revision: 1,
			Ports: []element.Port{{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true}},
		},
	} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	file, err := syntax.Parse("agent.ortg", []byte(clientSource))
	if err != nil {
		t.Fatal(err)
	}
	result, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: catalog, Lock: resolve.NewLock(), ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog, result.Graph, result.Lock
}

func resolvedConfigProperties(result management.AnalysisResult, name string) int {
	for _, metadata := range result.Catalog.Elements {
		if metadata.Identity.Name == name && metadata.Config.SchemaStatus == "resolved" {
			return len(metadata.Config.Properties)
		}
	}
	return 0
}

func liveFixture(t *testing.T, graph ir.Graph) (inspect.Live, inspect.LiveTrace) {
	t.Helper()
	configuration := inspect.ArtifactIdentity{
		ID: "values://managed-client", Revision: "1", Digest: "sha256:" + strings.Repeat("c", 64),
	}
	nodes := make(map[string]inspect.NodeLive, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = inspect.NodeLive{
			State: "mounted", LastOutcome: "private model text",
			Resolution: &inspect.NodeResolution{
				Element: node.Element, Implementation: "test.client.runtime",
				Runtime: inspect.ArtifactIdentity{
					ID: "runtime://" + node.ID, Revision: "1", Digest: "sha256:" + strings.Repeat("d", 64),
				},
				RuntimeEvidence: inspect.EvidenceRegistered, CapabilitiesEvidence: inspect.EvidenceLive,
			},
		}
	}
	edges := make(map[string]inspect.EdgeLive, len(graph.Edges))
	for _, edge := range graph.Edges {
		edges[edge.ID] = inspect.EdgeLive{}
	}
	live := inspect.Live{
		FormatVersion: inspect.LiveFormatVersion, GraphID: graph.ID, GraphRevision: graph.Revision,
		Fingerprint: graph.Fingerprint, Configuration: &configuration, Sequence: 1,
		ObservedAt: time.Now().UTC(), State: "mounted", Nodes: nodes, Edges: edges,
		Flows: map[string]inspect.FlowLive{},
	}
	snapshot, err := inspect.TraceSnapshotFromLive(graph, configuration, live, 1)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := inspect.FreezeLiveTrace(inspect.LiveTrace{
		FormatVersion: inspect.LiveTraceFormatVersion,
		Graph: inspect.GraphReference{
			FormatVersion: graph.FormatVersion, ID: graph.ID,
			Revision: graph.Revision, Fingerprint: graph.Fingerprint,
		},
		Configuration: configuration, Limits: inspect.DefaultTraceLimits(),
		Snapshots: []inspect.TraceSnapshot{snapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	return live, trace
}

var _ management.Reconciliation = reconciler{}
