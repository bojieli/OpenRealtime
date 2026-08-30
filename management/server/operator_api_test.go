package server

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
)

type operatorPlaneAuthorizer struct{ graph string }

func (authorizer operatorPlaneAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if request.Capability != "operator-token" {
		return management.ErrUnauthorized
	}
	allowed := request.Resource == "authoring" &&
		(request.Operation == management.AnalyzeDocument ||
			request.Operation == management.CompileDocument || request.Operation == management.RenderGraph)
	allowed = allowed || (request.Operation == management.ReadGraph && request.Resource == authorizer.graph)
	if !allowed {
		return management.ErrUnauthorized
	}
	return nil
}

type sessionPlaneAuthorizer struct{}

func (sessionPlaneAuthorizer) Authorize(
	_ context.Context, request management.AuthorizationRequest,
) error {
	if request.Capability == "session-token" && request.Operation == management.ReadSession &&
		request.Resource == "sess-test" {
		return nil
	}
	return management.ErrUnauthorized
}

func TestOperatorAPIKeepsSessionAndOperatorAuthorityPlanesSeparate(t *testing.T) {
	graph := testGraph(t)
	sessionBundle, err := NewBundle(BundleConfig{
		Authorizer: sessionPlaneAuthorizer{}, Sessions: testSessions{live: testLive(graph)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionRealm, err := sessionBundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := sessionRealm.Close(ctx); err != nil {
			t.Errorf("close session management realm: %v", err)
		}
	})
	sessionHandler, err := HTTPHandler(sessionRealm, "http")
	if err != nil {
		t.Fatal(err)
	}

	var fallbackCalls atomic.Int64
	base := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, management.APIPrefix+"/sessions/") {
			sessionHandler.ServeHTTP(writer, request)
			return
		}
		fallbackCalls.Add(1)
		writer.Header().Set("X-Base-Path", request.URL.Path)
		writer.WriteHeader(http.StatusAlreadyReported)
	})
	authoring := &testAuthoring{}
	api, err := MountOperatorAPI(context.Background(), base, OperatorAPIConfig{
		Authorizer:    operatorPlaneAuthorizer{graph: graph.Fingerprint},
		StaticCatalog: testStaticCatalog{graph: graph}, Authoring: authoring,
	})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := api.Close(ctx); err != nil {
			t.Errorf("close operator API: %v", err)
		}
	})
	handler := api.Handler()
	graphPath := management.APIPrefix + "/graphs/" + graph.Fingerprint
	sessionPath := management.APIPrefix + "/sessions/sess-test/live"
	authoringPath := management.APIPrefix + "/authoring/analyze"
	document := []byte(`{"path":"agent.ortg","source":"graph x {\n}"}`)

	if response := request(t, handler, http.MethodGet, graphPath, "session-token", nil); response.Code != http.StatusNotFound {
		t.Fatalf("session token crossed into static API: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(t, handler, http.MethodGet, graphPath, "operator-token", nil); response.Code != http.StatusOK {
		t.Fatalf("operator graph read = %d %s", response.Code, response.Body.String())
	}
	if response := request(t, handler, http.MethodPost, authoringPath, "session-token", document); response.Code != http.StatusNotFound {
		t.Fatalf("session token crossed into authoring API: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(t, handler, http.MethodPost, authoringPath, "operator-token", document); response.Code != http.StatusOK || authoring.analyzeCalls.Load() != 1 {
		t.Fatalf("operator authoring = %d calls=%d body=%s",
			response.Code, authoring.analyzeCalls.Load(), response.Body.String())
	}
	if response := request(t, handler, http.MethodGet, sessionPath, "operator-token", nil); response.Code != http.StatusNotFound {
		t.Fatalf("operator token crossed into session API: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(t, handler, http.MethodGet, sessionPath, "session-token", nil); response.Code != http.StatusOK {
		t.Fatalf("session read = %d %s", response.Code, response.Body.String())
	}

	beforeFallback := fallbackCalls.Load()
	if response := request(t, handler, http.MethodPost, graphPath, "operator-token", nil); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong operator route method = %d, want 405", response.Code)
	}
	if fallbackCalls.Load() != beforeFallback {
		t.Fatal("wrong method escaped the selected operator route family")
	}
	response := request(t, handler, http.MethodGet, "/healthz", "session-token", nil)
	if response.Code != http.StatusAlreadyReported || response.Header().Get("X-Base-Path") != "/healthz" ||
		fallbackCalls.Load() != beforeFallback+1 {
		t.Fatalf("base fallthrough = status=%d header=%q calls=%d",
			response.Code, response.Header().Get("X-Base-Path"), fallbackCalls.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := api.Close(ctx); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := api.Close(ctx); err != nil {
		t.Fatalf("second operator API close: %v", err)
	}
	if response := request(t, handler, http.MethodGet, graphPath, "operator-token", nil); response.Code != http.StatusNotFound {
		t.Fatalf("closed operator route = %d, want 404", response.Code)
	}
	if response := request(t, handler, http.MethodGet, sessionPath, "session-token", nil); response.Code != http.StatusOK {
		t.Fatalf("operator close stopped session realm: %d %s", response.Code, response.Body.String())
	}
	if response := request(t, handler, http.MethodGet, "/healthz", "", nil); response.Code != http.StatusAlreadyReported {
		t.Fatalf("operator close stopped base handler: %d", response.Code)
	}
}

func TestOperatorAPIOverlaysOnlySelectedRouteFamilies(t *testing.T) {
	base := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Fallthrough", request.URL.Path)
		writer.WriteHeader(http.StatusNoContent)
	})
	api, err := MountOperatorAPI(context.Background(), base, OperatorAPIConfig{
		Authorizer: operatorPlaneAuthorizer{}, Authoring: &testAuthoring{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := api.Close(ctx); err != nil {
			t.Errorf("close operator API: %v", err)
		}
	})
	handler := api.Handler()

	for _, path := range []string{
		management.APIPrefix + "/graphs/sha256:" + strings.Repeat("a", 64),
		management.APIPrefix + "/sessions/sess-test/live",
		management.APIPrefix + "/reconciliations",
		"/healthz",
	} {
		response := request(t, handler, http.MethodGet, path, "operator-token", nil)
		if response.Code != http.StatusNoContent || response.Header().Get("X-Fallthrough") != path {
			t.Fatalf("unselected route %s did not fall through: status=%d header=%q",
				path, response.Code, response.Header().Get("X-Fallthrough"))
		}
	}
	response := request(t, handler, http.MethodGet,
		management.APIPrefix+"/authoring/analyze", "operator-token", nil)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("X-Fallthrough") != "" {
		t.Fatalf("selected authoring family escaped overlay: status=%d header=%q",
			response.Code, response.Header().Get("X-Fallthrough"))
	}
}

func TestMountOperatorAPIRejectsMissingCompositionInputs(t *testing.T) {
	base := http.NotFoundHandler()
	authoring := &testAuthoring{}
	for name, mount := range map[string]func() (*OperatorAPI, error){
		"nil context": func() (*OperatorAPI, error) {
			return MountOperatorAPI(nil, base, OperatorAPIConfig{Authorizer: operatorPlaneAuthorizer{}, Authoring: authoring})
		},
		"nil base": func() (*OperatorAPI, error) {
			return MountOperatorAPI(context.Background(), nil,
				OperatorAPIConfig{Authorizer: operatorPlaneAuthorizer{}, Authoring: authoring})
		},
		"no service": func() (*OperatorAPI, error) {
			return MountOperatorAPI(context.Background(), base,
				OperatorAPIConfig{Authorizer: operatorPlaneAuthorizer{}})
		},
		"no authorizer": func() (*OperatorAPI, error) {
			return MountOperatorAPI(context.Background(), base, OperatorAPIConfig{Authoring: authoring})
		},
	} {
		t.Run(name, func(t *testing.T) {
			api, err := mount()
			if err == nil || api != nil {
				t.Fatalf("MountOperatorAPI() = %#v, %v; want nil API and non-nil error", api, err)
			}
		})
	}
	if err := (*OperatorAPI)(nil).Close(context.Background()); err == nil {
		t.Fatal("nil OperatorAPI.Close unexpectedly succeeded")
	}
}

var (
	_ management.Authorizer = operatorPlaneAuthorizer{}
	_ management.Authorizer = sessionPlaneAuthorizer{}
)
