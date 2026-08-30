package httpservice

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouterRegistrationIsAtomicConflictClosedAndScoped(t *testing.T) {
	router := NewRouter()
	dispose, err := router.Register("operations", []Route{
		{Pattern: "GET /healthz", Handler: statusHandler(http.StatusOK, "healthy")},
		{Pattern: "GET /metrics", Handler: statusHandler(http.StatusOK, "metrics")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := router.Register("conflict", []Route{
		{Pattern: "GET /new", Handler: statusHandler(http.StatusCreated, "new")},
		{Pattern: "GET /healthz", Handler: statusHandler(http.StatusCreated, "shadow")},
	}); err == nil || !strings.Contains(err.Error(), "already owned by operations") {
		t.Fatalf("conflicting route registration error = %v", err)
	}
	assertRouteStatus(t, router, "/healthz", http.StatusOK)
	assertRouteStatus(t, router, "/new", http.StatusNotFound)

	dispose()
	dispose()
	assertRouteStatus(t, router, "/healthz", http.StatusNotFound)
	assertRouteStatus(t, router, "/metrics", http.StatusNotFound)
}

func TestRouterRejectsInvalidAndTypedNilHandlersWithoutPublishing(t *testing.T) {
	tests := []struct {
		name  string
		owner string
		route Route
	}{
		{name: "owner", owner: " bad", route: Route{Pattern: "GET /x", Handler: http.NotFoundHandler()}},
		{name: "pattern", owner: "valid", route: Route{Pattern: " GET /x", Handler: http.NotFoundHandler()}},
		{name: "nil", owner: "valid", route: Route{Pattern: "GET /x"}},
		{name: "typed nil", owner: "valid", route: Route{Pattern: "GET /x", Handler: http.HandlerFunc(nil)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := NewRouter()
			if _, err := router.Register(test.owner, []Route{test.route}); err == nil {
				t.Fatal("invalid route registration succeeded")
			}
			assertRouteStatus(t, router, "/x", http.StatusNotFound)
		})
	}
}

func TestRouterRejectsOverlappingMuxPatternsWithoutPublishing(t *testing.T) {
	router := NewRouter()
	if _, err := router.Register("first", []Route{{
		Pattern: "GET /{left}/b", Handler: statusHandler(http.StatusOK, "first"),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Register("overlap", []Route{{
		Pattern: "GET /a/{right}", Handler: statusHandler(http.StatusCreated, "overlap"),
	}}); err == nil || !strings.Contains(err.Error(), "conflicting HTTP mux patterns") {
		t.Fatalf("overlapping route registration error = %v", err)
	}
	assertRouteStatus(t, router, "/a/b", http.StatusOK)
}

func statusHandler(status int, body string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	})
}

func assertRouteStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s status = %d, want %d", path, response.Code, want)
	}
}
