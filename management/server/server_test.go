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
	"github.com/bojieli/OpenRealtime/graph/resolve"
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
			request.Operation == management.RenameDocument || request.Operation == management.RemoveDocumentEdge ||
			request.Operation == management.CreateDocumentEdge ||
			request.Operation == management.CompileDocument ||
			request.Operation == management.RenderGraph)
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
	live  inspect.Live
	model inspect.Model
}

func (sessions testSessions) Snapshot(_ context.Context, session string) (inspect.Live, error) {
	if session != "sess-test" {
		return inspect.Live{}, management.ErrNotFound
	}
	return sessions.live.Clone(), nil
}

func (sessions testSessions) Model(_ context.Context, session string) (inspect.Model, error) {
	if session != "sess-test" {
		return inspect.Model{}, management.ErrNotFound
	}
	if sessions.model.Fingerprint == "" {
		return inspect.Model{}, management.ErrUnavailable
	}
	return sessions.model, nil
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

type revocableRenameAuthorizer struct{ allowed atomic.Bool }

func (authorizer *revocableRenameAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if authorizer.allowed.Load() && request.Capability == "scoped-token" &&
		request.Operation == management.RenameDocument && request.Resource == "authoring" {
		return nil
	}
	return management.ErrUnauthorized
}

type revocableEdgeAuthorizer struct{ allowed atomic.Bool }

func (authorizer *revocableEdgeAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if authorizer.allowed.Load() && request.Capability == "scoped-token" &&
		request.Operation == management.RemoveDocumentEdge && request.Resource == "authoring" {
		return nil
	}
	return management.ErrUnauthorized
}

type revocableCreateEdgeAuthorizer struct{ allowed atomic.Bool }

func (authorizer *revocableCreateEdgeAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if authorizer.allowed.Load() && request.Capability == "scoped-token" &&
		request.Operation == management.CreateDocumentEdge && request.Resource == "authoring" {
		return nil
	}
	return management.ErrUnauthorized
}

type blockingSessions struct {
	testSessions
	entered chan struct{}
	release chan struct{}
}

type blockingModelSessions struct {
	testSessions
	entered chan struct{}
	release chan struct{}
}

func (sessions *blockingModelSessions) Model(ctx context.Context, session string) (inspect.Model, error) {
	select {
	case sessions.entered <- struct{}{}:
	default:
	}
	select {
	case <-sessions.release:
		return sessions.testSessions.Model(ctx, session)
	case <-ctx.Done():
		return inspect.Model{}, ctx.Err()
	}
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
	analyzeCalls      atomic.Int64
	renameCalls       atomic.Int64
	removeEdgeCalls   atomic.Int64
	createEdgeCalls   atomic.Int64
	forgeResult       bool
	forgeRenameResult bool
	forgeRemoveResult bool
	forgeCreateResult bool
	renameEntered     chan struct{}
	renameRelease     chan struct{}
	removeEdgeEntered chan struct{}
	removeEdgeRelease chan struct{}
	createEdgeEntered chan struct{}
	createEdgeRelease chan struct{}
}

func (authoring *testAuthoring) Rename(
	ctx context.Context, input management.RenameDocumentRequest,
) (management.RenameDocumentResult, error) {
	authoring.renameCalls.Add(1)
	if authoring.renameEntered != nil {
		select {
		case authoring.renameEntered <- struct{}{}:
		default:
		}
		select {
		case <-authoring.renameRelease:
		case <-ctx.Done():
			return management.RenameDocumentResult{}, ctx.Err()
		}
	}
	document, err := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
		resolve.NewCatalog(), editor.DefaultLimits())
	if err != nil {
		return management.RenameDocumentResult{}, err
	}
	edits, err := document.RenameNodeID(input.Node, input.NewName)
	if err != nil {
		return management.RenameDocumentResult{}, err
	}
	if authoring.forgeRenameResult && len(edits.Edits) > 0 {
		edits.Edits = edits.Edits[:len(edits.Edits)-1]
	}
	return management.RenameDocumentResult{
		Node: input.Node, NewName: input.NewName, Edits: edits,
	}, nil
}

func (authoring *testAuthoring) RemoveEdge(
	ctx context.Context, input management.RemoveDocumentEdgeRequest,
) (management.RemoveDocumentEdgeResult, error) {
	authoring.removeEdgeCalls.Add(1)
	if authoring.removeEdgeEntered != nil {
		select {
		case authoring.removeEdgeEntered <- struct{}{}:
		default:
		}
		select {
		case <-authoring.removeEdgeRelease:
		case <-ctx.Done():
			return management.RemoveDocumentEdgeResult{}, ctx.Err()
		}
	}
	document, err := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
		resolve.NewCatalog(), editor.DefaultLimits())
	if err != nil {
		return management.RemoveDocumentEdgeResult{}, err
	}
	edits, err := document.RemoveEdgeID(input.Edge)
	if err != nil {
		return management.RemoveDocumentEdgeResult{}, err
	}
	if authoring.forgeRemoveResult && len(edits.Edits) == 1 {
		edits.Edits[0].NewText += "\n"
	}
	return management.RemoveDocumentEdgeResult{Edge: input.Edge, Edits: edits}, nil
}

func (authoring *testAuthoring) CreateEdge(
	ctx context.Context, input management.CreateDocumentEdgeRequest,
) (management.CreateDocumentEdgeResult, error) {
	authoring.createEdgeCalls.Add(1)
	if authoring.createEdgeEntered != nil {
		select {
		case authoring.createEdgeEntered <- struct{}{}:
		default:
		}
		select {
		case <-authoring.createEdgeRelease:
		case <-ctx.Done():
			return management.CreateDocumentEdgeResult{}, ctx.Err()
		}
	}
	document, err := editor.Analyze(input.Document.Path, []byte(input.Document.Source),
		resolve.NewCatalog(), editor.DefaultLimits())
	if err != nil {
		return management.CreateDocumentEdgeResult{}, err
	}
	edits, err := document.CreateEdgeID(input.Edge,
		syntax.Endpoint{Node: input.From.Node, Port: input.From.Port},
		syntax.Endpoint{Node: input.To.Node, Port: input.To.Port}, syntax.Delivery(input.Delivery))
	if err != nil {
		return management.CreateDocumentEdgeResult{}, err
	}
	if authoring.forgeCreateResult && len(edits.Edits) == 1 {
		edits.Edits[0].NewText += "\n"
	}
	digest := sha256.Sum256([]byte(input.ExpectedFingerprint + "\x00" + input.Edge))
	return management.CreateDocumentEdgeResult{
		Edge: input.Edge, PreviousFingerprint: input.ExpectedFingerprint,
		CandidateFingerprint: "sha256:" + hex.EncodeToString(digest[:]), Edits: edits,
	}, nil
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
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	authoring := &testAuthoring{}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{graph: graph.Fingerprint}, StaticCatalog: testStaticCatalog{graph: graph},
		Sessions: testSessions{live: testLive(graph), model: model}, Authoring: authoring,
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
	response = request(t, handler, http.MethodGet,
		management.APIPrefix+"/sessions/sess-test/model", "session-token", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"reaction"`) ||
		!strings.Contains(response.Body.String(), graph.Fingerprint) {
		t.Fatalf("session model response = %d %s", response.Code, response.Body.String())
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

	renameInput := management.RenameDocumentRequest{
		Document: management.AuthoringDocument{
			Path: "agent.ortg", Source: "graph x {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Node: "source", NewName: "camera",
	}
	renameBody, err := json.Marshal(renameInput)
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/rename", "author-token", renameBody)
	if response.Code != http.StatusOK || authoring.renameCalls.Load() != 1 ||
		!strings.Contains(response.Body.String(), `"new_name":"camera"`) ||
		strings.Count(response.Body.String(), `"old_text":"source"`) != 3 {
		t.Fatalf("valid graph-wide rename = %d calls=%d body=%s",
			response.Code, authoring.renameCalls.Load(), response.Body.String())
	}
	duplicateRename := []byte(`{"document":{"path":"agent.ortg","source":"graph x {\n}\n"},` +
		`"node":"source","node":"other","new_name":"camera"}`)
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/rename", "author-token", duplicateRename)
	if response.Code != http.StatusBadRequest || authoring.renameCalls.Load() != 1 {
		t.Fatalf("duplicate-key rename = %d calls=%d body=%s",
			response.Code, authoring.renameCalls.Load(), response.Body.String())
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/rename", "session-token", renameBody)
	if response.Code != http.StatusNotFound || authoring.renameCalls.Load() != 1 {
		t.Fatalf("wrong-scope rename = %d calls=%d body=%s",
			response.Code, authoring.renameCalls.Load(), response.Body.String())
	}
	removeInput := management.RemoveDocumentEdgeRequest{
		Document: renameInput.Document, Edge: "source.out->source.in",
	}
	removeBody, err := json.Marshal(removeInput)
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/remove-edge", "author-token", removeBody)
	if response.Code != http.StatusOK || authoring.removeEdgeCalls.Load() != 1 ||
		!strings.Contains(response.Body.String(), `"edge":"source.out->source.in"`) ||
		!strings.Contains(response.Body.String(), `"edits"`) {
		t.Fatalf("valid edge removal = %d calls=%d body=%s",
			response.Code, authoring.removeEdgeCalls.Load(), response.Body.String())
	}
	duplicateRemove := []byte(`{"document":{"path":"agent.ortg","source":"graph x {\n}\n"},` +
		`"edge":"first","edge":"second"}`)
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/remove-edge", "author-token", duplicateRemove)
	if response.Code != http.StatusBadRequest || authoring.removeEdgeCalls.Load() != 1 {
		t.Fatalf("duplicate-key edge removal = %d calls=%d body=%s",
			response.Code, authoring.removeEdgeCalls.Load(), response.Body.String())
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/remove-edge", "session-token", removeBody)
	if response.Code != http.StatusNotFound || authoring.removeEdgeCalls.Load() != 1 {
		t.Fatalf("wrong-scope edge removal = %d calls=%d body=%s",
			response.Code, authoring.removeEdgeCalls.Load(), response.Body.String())
	}
	createInput := management.CreateDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path:     "agent.ortg",
			Source:   "graph x {\n    test.Managed :: source;\n    test.Managed :: sink;\n}\n",
			Revision: 4,
		},
		ExpectedFingerprint: "sha256:" + strings.Repeat("a", 64), Edge: "restored",
		From: management.AuthoringEdgeEndpoint{Node: "source", Port: "out"},
		To:   management.AuthoringEdgeEndpoint{Node: "sink", Port: "in"}, Delivery: string(syntax.Lossless),
	}
	createBody, err := json.Marshal(createInput)
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/create-edge", "author-token", createBody)
	var createResult management.CreateDocumentEdgeResult
	decodeCreateErr := json.Unmarshal(response.Body.Bytes(), &createResult)
	if response.Code != http.StatusOK || authoring.createEdgeCalls.Load() != 1 || decodeCreateErr != nil ||
		management.ValidateCreateDocumentEdgeResult(createInput, createResult) != nil {
		t.Fatalf("valid edge creation = %d calls=%d result=%+v decode=%v body=%s",
			response.Code, authoring.createEdgeCalls.Load(), createResult, decodeCreateErr, response.Body.String())
	}
	duplicateCreate := []byte(`{"document":{"path":"agent.ortg","source":"graph x {\n}\n"},` +
		`"expected_fingerprint":"sha256:` + strings.Repeat("a", 64) + `",` +
		`"edge":"first","edge":"second","from":{"node":"source","port":"out"},` +
		`"to":{"node":"sink","port":"in"},"delivery":"lossless"}`)
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/create-edge", "author-token", duplicateCreate)
	if response.Code != http.StatusBadRequest || authoring.createEdgeCalls.Load() != 1 {
		t.Fatalf("duplicate-key edge creation = %d calls=%d body=%s",
			response.Code, authoring.createEdgeCalls.Load(), response.Body.String())
	}
	response = request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/create-edge", "session-token", createBody)
	if response.Code != http.StatusNotFound || authoring.createEdgeCalls.Load() != 1 {
		t.Fatalf("wrong-scope edge creation = %d calls=%d body=%s",
			response.Code, authoring.createEdgeCalls.Load(), response.Body.String())
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

func TestSessionModelRechecksCapabilityAfterSlowStaticProjection(t *testing.T) {
	graph := testGraph(t)
	model, err := inspect.Build(graph)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := &revocableAuthorizer{}
	authorizer.allowed.Store(true)
	sessions := &blockingModelSessions{
		testSessions: testSessions{live: testLive(graph), model: model},
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
		http.MethodGet, management.APIPrefix+"/sessions/sess-test/model", nil,
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
		t.Fatal("static model projection did not begin after initial authorization")
	}
	authorizer.allowed.Store(false)
	close(sessions.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked static model request did not finish")
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("revoked capability disclosed a completed static model: %d %s",
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

func TestAuthoringRenameRouteRejectsProviderEditsNotBoundToEveryReference(t *testing.T) {
	authoring := &testAuthoring{forgeRenameResult: true}
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
	body, err := json.Marshal(management.RenameDocumentRequest{
		Document: management.AuthoringDocument{
			Path: "agent.ortg", Source: "graph x {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Node: "source", NewName: "camera",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/rename", "author-token", body)
	if response.Code != http.StatusConflict || authoring.renameCalls.Load() != 1 {
		t.Fatalf("forged graph-wide rename = %d calls=%d body=%s",
			response.Code, authoring.renameCalls.Load(), response.Body.String())
	}
}

func TestAuthoringEdgeRemovalRouteRejectsProviderMutationBeyondSelectedEdge(t *testing.T) {
	authoring := &testAuthoring{forgeRemoveResult: true}
	bundle, err := NewBundle(BundleConfig{Authorizer: testAuthorizer{}, Authoring: authoring})
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
	body, err := json.Marshal(management.RemoveDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path: "agent.ortg", Source: "graph x {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Edge: "source.out->source.in",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/remove-edge", "author-token", body)
	if response.Code != http.StatusConflict || authoring.removeEdgeCalls.Load() != 1 {
		t.Fatalf("forged edge removal = %d calls=%d body=%s",
			response.Code, authoring.removeEdgeCalls.Load(), response.Body.String())
	}
}

func TestAuthoringEdgeCreationRouteRejectsProviderMutationBeyondRequestedEdge(t *testing.T) {
	authoring := &testAuthoring{forgeCreateResult: true}
	bundle, err := NewBundle(BundleConfig{Authorizer: testAuthorizer{}, Authoring: authoring})
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
	body, err := json.Marshal(management.CreateDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path:   "agent.ortg",
			Source: "graph x {\n    test.Managed :: source;\n    test.Managed :: sink;\n}\n",
		},
		ExpectedFingerprint: "sha256:" + strings.Repeat("a", 64), Edge: "restored",
		From: management.AuthoringEdgeEndpoint{Node: "source", Port: "out"},
		To:   management.AuthoringEdgeEndpoint{Node: "sink", Port: "in"}, Delivery: string(syntax.Lossless),
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/create-edge", "author-token", body)
	if response.Code != http.StatusConflict || authoring.createEdgeCalls.Load() != 1 {
		t.Fatalf("forged edge creation = %d calls=%d body=%s",
			response.Code, authoring.createEdgeCalls.Load(), response.Body.String())
	}
}

func TestAuthoringRenameResponseRechecksCapabilityAfterProviderCompletes(t *testing.T) {
	authorizer := &revocableRenameAuthorizer{}
	authorizer.allowed.Store(true)
	authoring := &testAuthoring{
		renameEntered: make(chan struct{}, 1), renameRelease: make(chan struct{}),
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authorizer, Authoring: authoring})
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
	body, err := json.Marshal(management.RenameDocumentRequest{
		Document: management.AuthoringDocument{
			Path: "agent.ortg", Source: "graph x {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Node: "source", NewName: "camera",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost,
		management.APIPrefix+"/authoring/rename", bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(management.CapabilityHeader, "scoped-token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httpRequest)
		close(done)
	}()
	select {
	case <-authoring.renameEntered:
	case <-time.After(time.Second):
		t.Fatal("rename provider did not begin after initial authorization")
	}
	authorizer.allowed.Store(false)
	close(authoring.renameRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked rename request did not finish")
	}
	if response.Code != http.StatusNotFound || authoring.renameCalls.Load() != 1 {
		t.Fatalf("revoked rename disclosed a completed edit set: %d calls=%d body=%s",
			response.Code, authoring.renameCalls.Load(), response.Body.String())
	}
}

func TestAuthoringEdgeRemovalResponseRechecksCapabilityAfterProviderCompletes(t *testing.T) {
	authorizer := &revocableEdgeAuthorizer{}
	authorizer.allowed.Store(true)
	authoring := &testAuthoring{
		removeEdgeEntered: make(chan struct{}, 1), removeEdgeRelease: make(chan struct{}),
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authorizer, Authoring: authoring})
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
	body, err := json.Marshal(management.RemoveDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path: "agent.ortg", Source: "graph x {\n    test.Managed :: source;\n    source.out -> source.in;\n}\n",
		},
		Edge: "source.out->source.in",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost,
		management.APIPrefix+"/authoring/remove-edge", bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(management.CapabilityHeader, "scoped-token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httpRequest)
		close(done)
	}()
	select {
	case <-authoring.removeEdgeEntered:
	case <-time.After(time.Second):
		t.Fatal("edge-removal provider did not begin after initial authorization")
	}
	authorizer.allowed.Store(false)
	close(authoring.removeEdgeRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked edge-removal request did not finish")
	}
	if response.Code != http.StatusNotFound || authoring.removeEdgeCalls.Load() != 1 {
		t.Fatalf("revoked edge removal disclosed a completed edit set: %d calls=%d body=%s",
			response.Code, authoring.removeEdgeCalls.Load(), response.Body.String())
	}
}

func TestAuthoringEdgeCreationResponseRechecksCapabilityAfterProviderCompletes(t *testing.T) {
	authorizer := &revocableCreateEdgeAuthorizer{}
	authorizer.allowed.Store(true)
	authoring := &testAuthoring{
		createEdgeEntered: make(chan struct{}, 1), createEdgeRelease: make(chan struct{}),
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authorizer, Authoring: authoring})
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
	body, err := json.Marshal(management.CreateDocumentEdgeRequest{
		Document: management.AuthoringDocument{
			Path:   "agent.ortg",
			Source: "graph x {\n    test.Managed :: source;\n    test.Managed :: sink;\n}\n",
		},
		ExpectedFingerprint: "sha256:" + strings.Repeat("a", 64), Edge: "restored",
		From: management.AuthoringEdgeEndpoint{Node: "source", Port: "out"},
		To:   management.AuthoringEdgeEndpoint{Node: "sink", Port: "in"}, Delivery: string(syntax.Lossless),
	})
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(http.MethodPost,
		management.APIPrefix+"/authoring/create-edge", bytes.NewReader(body))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(management.CapabilityHeader, "scoped-token")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, httpRequest)
		close(done)
	}()
	select {
	case <-authoring.createEdgeEntered:
	case <-time.After(time.Second):
		t.Fatal("edge-creation provider did not begin after initial authorization")
	}
	authorizer.allowed.Store(false)
	close(authoring.createEdgeRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("revoked edge-creation request did not finish")
	}
	if response.Code != http.StatusNotFound || authoring.createEdgeCalls.Load() != 1 {
		t.Fatalf("revoked edge creation disclosed a completed edit set: %d calls=%d body=%s",
			response.Code, authoring.createEdgeCalls.Load(), response.Body.String())
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
	benchmarkCanonicalSessionResource(b, "live")
}

func BenchmarkCanonicalSessionModel(b *testing.B) {
	benchmarkCanonicalSessionResource(b, "model")
}

func benchmarkCanonicalSessionResource(b *testing.B, resource string) {
	b.Helper()
	graph := testGraph(b)
	model, err := inspect.Build(graph)
	if err != nil {
		b.Fatal(err)
	}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: testAuthorizer{graph: graph.Fingerprint},
		Sessions:   testSessions{live: testLive(graph), model: model},
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
	path := management.APIPrefix + "/sessions/sess-test/" + resource
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(management.CapabilityHeader, "session-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("canonical %s status = %d", resource, response.Code)
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
