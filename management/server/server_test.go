package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
)

type testAuthorizer struct{ graph string }

func (authorizer testAuthorizer) Authorize(_ context.Context, request management.AuthorizationRequest) error {
	allowed := false
	switch request.Capability {
	case "graph-token":
		allowed = request.Operation == management.ReadGraph && request.Resource == authorizer.graph
	case "session-token":
		allowed = (request.Operation == management.ReadSession || request.Operation == management.ReadTrace) &&
			request.Resource == "sess-test"
	case "author-token":
		allowed = request.Resource == "authoring" && (request.Operation == management.AnalyzeDocument ||
			request.Operation == management.CompileDocument || request.Operation == management.RenderGraph)
	case "operator-token":
		allowed = request.Operation == management.ApplyCandidate && request.Resource == "sess-test"
	}
	if !allowed {
		return management.ErrUnauthorized
	}
	return nil
}

type testStaticCatalog struct{ graph ir.Graph }

func (catalog testStaticCatalog) Graph(_ context.Context, fingerprint string) (ir.Graph, error) {
	if fingerprint != catalog.graph.Fingerprint {
		return ir.Graph{}, management.ErrNotFound
	}
	return catalog.graph, nil
}

func (testStaticCatalog) ElementDescriptor(context.Context, element.Identity) (element.Descriptor, error) {
	return element.Descriptor{}, management.ErrNotFound
}

func (testStaticCatalog) PluginDescriptor(context.Context, plugin.Identity) (plugin.Descriptor, error) {
	return plugin.Descriptor{}, management.ErrNotFound
}

func (testStaticCatalog) ValuesSchema(context.Context, string) (schema.Bundle, error) {
	return schema.Bundle{}, management.ErrNotFound
}

type testSessions struct {
	live inspect.Live
}

func (sessions testSessions) Snapshot(_ context.Context, session string) (inspect.Live, error) {
	if session != "sess-test" {
		return inspect.Live{}, management.ErrNotFound
	}
	return sessions.live.Clone(), nil
}

func (testSessions) Deltas(_ context.Context, session string, after uint64, _ uint32) (management.DeltaPage, error) {
	if session != "sess-test" {
		return management.DeltaPage{}, management.ErrNotFound
	}
	return management.DeltaPage{
		FormatVersion: 1, SessionID: session, After: after, Next: after, Events: []inspect.TraceEvent{},
	}, nil
}

func (testSessions) Trace(context.Context, string) (inspect.LiveTrace, error) {
	return inspect.LiveTrace{}, management.ErrUnavailable
}

type revocableAuthorizer struct{ allowed atomic.Bool }

func (authorizer *revocableAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if authorizer.allowed.Load() && request.Capability == "scoped-token" &&
		request.Operation == management.ReadSession && request.Resource == "sess-test" {
		return nil
	}
	return management.ErrUnauthorized
}

type blockingSessions struct {
	testSessions
	entered chan struct{}
	release chan struct{}
}

func (sessions *blockingSessions) Snapshot(ctx context.Context, session string) (inspect.Live, error) {
	select {
	case sessions.entered <- struct{}{}:
	default:
	}
	select {
	case <-sessions.release:
		return sessions.testSessions.Snapshot(ctx, session)
	case <-ctx.Done():
		return inspect.Live{}, ctx.Err()
	}
}

type testAuthoring struct {
	analyzeCalls atomic.Int64
	forgeResult  bool
}

func (authoring *testAuthoring) Analyze(_ context.Context, document management.AuthoringDocument) (management.AnalysisResult, error) {
	authoring.analyzeCalls.Add(1)
	digest := sha256.Sum256([]byte(document.Source))
	identity := "sha256:" + hex.EncodeToString(digest[:])
	file, _ := syntax.Parse(document.Path, []byte(document.Source))
	formatted := syntax.Format(file)
	canonical := formatted == document.Source
	edits := []editor.TextEdit{}
	if !canonical {
		end := syntax.Position{Offset: len(document.Source), Line: 1, Column: 1}
		for _, character := range document.Source {
			if character == '\n' {
				end.Line++
				end.Column = 1
			} else {
				end.Column++
			}
		}
		edits = []editor.TextEdit{{
			Span: syntax.Span{
				Start: syntax.Position{Line: 1, Column: 1}, End: end,
			},
			OldText: document.Source, NewText: formatted,
		}}
	}
	result := management.AnalysisResult{
		SourceDigest: identity, Parsed: true, Canonical: canonical,
		Diagnostics: editor.DiagnosticReport{Items: []editor.Diagnostic{}},
		Catalog:     editor.MetadataReport{Elements: []editor.ElementMetadata{}},
		Formatting: &editor.EditSet{
			Path: document.Path, SourceDigest: identity, Edits: edits,
		},
	}
	if authoring.forgeResult {
		result.Formatting = nil
	}
	return result, nil
}

func (*testAuthoring) Compile(context.Context, management.AuthoringDocument) (management.CompileResult, error) {
	return management.CompileResult{}, management.ErrInvalid
}

func (*testAuthoring) Render(_ context.Context, request management.RenderRequest) (management.RenderResult, error) {
	return management.RenderResult{Fingerprint: request.Graph.Fingerprint, Format: request.Format, Text: "ok"}, nil
}

type testReconciler struct{}

func (testReconciler) Apply(_ context.Context, request management.ReconciliationRequest) (management.ReconciliationReceipt, error) {
	return management.ReconciliationReceipt{
		FormatVersion: 1, SessionID: request.SessionID,
		PreviousFingerprint:  request.ExpectedFingerprint,
		CandidateFingerprint: request.Candidate.Fingerprint,
		State:                "applied", SafePointSequence: 17,
	}, nil
}

func TestManagementBundleRoutesAreScopedAuthorizedAndStrict(t *testing.T) {
	graph := testGraph(t)
	authoring := &testAuthoring{}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{graph: graph.Fingerprint}, StaticCatalog: testStaticCatalog{graph: graph},
		Sessions: testSessions{live: testLive(graph)}, Authoring: authoring,
		Reconciliation: testReconciler{},
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
			t.Errorf("close management bundle: %v", err)
		}
	})
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}

	graphPath := management.APIPrefix + "/graphs/" + graph.Fingerprint
	response := request(t, handler, http.MethodGet, graphPath, "", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unauthorized graph status = %d, want 404", response.Code)
	}
	response = request(t, handler, http.MethodGet, graphPath, "session-token", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("wrong-scope capability status = %d, want 404", response.Code)
	}
	repeatedRequest := httptest.NewRequest(http.MethodGet, graphPath, nil)
	repeatedRequest.Header.Add(management.CapabilityHeader, "graph-token")
	repeatedRequest.Header.Add(management.CapabilityHeader, "graph-token")
	repeatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(repeatedResponse, repeatedRequest)
	if repeatedResponse.Code != http.StatusNotFound {
		t.Fatalf("repeated capability status = %d, want 404", repeatedResponse.Code)
	}
	response = request(t, handler, http.MethodGet, graphPath+"?extra=1", "graph-token", nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown query status = %d, want 400", response.Code)
	}
	response = request(t, handler, http.MethodGet, graphPath, "graph-token", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), graph.Fingerprint) {
		t.Fatalf("graph response = %d %s", response.Code, response.Body.String())
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store", "Referrer-Policy": "no-referrer",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	response = request(t, handler, http.MethodGet,
		management.APIPrefix+"/sessions/sess-test/live", "session-token", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), graph.Fingerprint) {
		t.Fatalf("live response = %d %s", response.Code, response.Body.String())
	}

	duplicate := []byte(`{"path":"agent.ortg","path":"forged.ortg","source":"graph x {}"}`)
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/analyze", "author-token", duplicate)
	if response.Code != http.StatusBadRequest || authoring.analyzeCalls.Load() != 0 {
		t.Fatalf("duplicate-key authoring request = %d calls=%d body=%s",
			response.Code, authoring.analyzeCalls.Load(), response.Body.String())
	}
	valid := []byte(`{"path":"agent.ortg","source":"graph x {\n}"}`)
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/analyze", "author-token", valid)
	if response.Code != http.StatusOK || authoring.analyzeCalls.Load() != 1 {
		t.Fatalf("valid authoring request = %d calls=%d body=%s",
			response.Code, authoring.analyzeCalls.Load(), response.Body.String())
	}

	reconcileBody, err := json.Marshal(management.ReconciliationRequest{
		SessionID: "sess-test", ExpectedFingerprint: graph.Fingerprint, Candidate: graph,
		ValuesFingerprint:     "sha256:" + strings.Repeat("b", 64),
		DeploymentFingerprint: "sha256:" + strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/reconciliations", "operator-token", reconcileBody)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"safe_point_sequence":17`) {
		t.Fatalf("reconciliation response = %d %s", response.Code, response.Body.String())
	}

	if err := mounted.Unmount(context.Background(), "static-api"); err != nil {
		t.Fatal(err)
	}
	if response := request(t, handler, http.MethodGet, graphPath, "graph-token", nil); response.Code != http.StatusNotFound {
		t.Fatalf("unmounted static route status = %d", response.Code)
	}
	if response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/analyze", "author-token", valid); response.Code != http.StatusOK {
		t.Fatalf("unrelated authoring route disappeared: %d", response.Code)
	}
	if err := mounted.Activate(context.Background(), "static-api"); err != nil {
		t.Fatal(err)
	}
	if response := request(t, handler, http.MethodGet, graphPath, "graph-token", nil); response.Code != http.StatusOK {
		t.Fatalf("reactivated static route status = %d", response.Code)
	}
}

func TestSessionResponseRechecksCapabilityAfterSlowSnapshot(t *testing.T) {
	graph := testGraph(t)
	authorizer := &revocableAuthorizer{}
	authorizer.allowed.Store(true)
	sessions := &blockingSessions{
		testSessions: testSessions{live: testLive(graph)},
		entered:      make(chan struct{}, 1),
		release:      make(chan struct{}),
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authorizer, Sessions: sessions})
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
			t.Errorf("close management bundle: %v", err)
		}
	})
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(
		http.MethodGet, management.APIPrefix+"/sessions/sess-test/live", nil,
	)
	request.Header.Set(management.CapabilityHeader, "scoped-token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-sessions.entered:
	case <-time.After(time.Second):
		t.Fatal("snapshot did not begin after initial authorization")
	}
	authorizer.allowed.Store(false)
	close(sessions.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked inspection request did not finish")
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked capability disclosed a completed snapshot: %d %s",
			response.Code, response.Body.String())
	}
}

func TestAuthoringRouteRejectsProviderSnapshotNotBoundToSource(t *testing.T) {
	authoring := &testAuthoring{forgeResult: true}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{}, Authoring: authoring,
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
			t.Errorf("close management bundle: %v", err)
		}
	})
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"path":"agent.ortg","source":"graph x {\n}\n"}`)
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/analyze", "author-token", body)
	if response.Code != http.StatusConflict || authoring.analyzeCalls.Load() != 1 {
		t.Fatalf("forged authoring snapshot = %d calls=%d body=%s",
			response.Code, authoring.analyzeCalls.Load(), response.Body.String())
	}
}

func request(
	t *testing.T, handler http.Handler, method, path, token string, body []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		request.Header.Set(management.CapabilityHeader, token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func testGraph(t testing.TB) ir.Graph {
	t.Helper()
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.Managed", Revision: 1,
		Ports: []element.Port{{
			Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")),
			Cardinality: element.One,
		}},
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "managed-test", Revision: 1,
		Nodes: []ir.Node{{
			ID: "node", Element: identity, Ports: []ir.Port{{
				Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")),
				Cardinality: element.One, DefaultDepth: 1,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func BenchmarkCanonicalSessionLive(b *testing.B) {
	graph := testGraph(b)
	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{graph: graph.Fingerprint},
		Sessions:   testSessions{live: testLive(graph)},
	})
	if err != nil {
		b.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := mounted.Close(ctx); err != nil {
			b.Errorf("close management bundle: %v", err)
		}
	})
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		b.Fatal(err)
	}
	path := management.APIPrefix + "/sessions/sess-test/live"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(management.CapabilityHeader, "session-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("canonical live status = %d", response.Code)
		}
	}
}

func testLive(graph ir.Graph) inspect.Live {
	configuration := inspect.ArtifactIdentity{
		ID: "values://managed-test", Revision: "1", Digest: "sha256:" + strings.Repeat("d", 64),
	}
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion, GraphID: graph.ID, GraphRevision: graph.Revision,
		Fingerprint: graph.Fingerprint, Configuration: &configuration, Sequence: 1,
		ObservedAt: time.Now().UTC(), State: "mounted",
		Nodes: map[string]inspect.NodeLive{"node": {
			State: "mounted", Resolution: &inspect.NodeResolution{
				Element: graph.Nodes[0].Element, Implementation: "test.managed",
				Runtime: inspect.ArtifactIdentity{
					ID: "runtime://test", Revision: "1", Digest: "sha256:" + strings.Repeat("e", 64),
				}, RuntimeEvidence: inspect.EvidenceRegistered,
			},
		}},
		Edges: map[string]inspect.EdgeLive{}, Flows: map[string]inspect.FlowLive{},
	}
}

var (
	_ management.Authorizer        = testAuthorizer{}
	_ management.StaticCatalog     = testStaticCatalog{}
	_ management.SessionInspection = testSessions{}
	_ management.Authoring         = (*testAuthoring)(nil)
	_ management.Reconciliation    = testReconciler{}
)
