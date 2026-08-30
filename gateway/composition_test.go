package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/management"
)

func TestGatewayCompositionRequiresAnExactInspectionPlaneAndCanonicalHandler(t *testing.T) {
	bind := compositionBinding(t)
	plane, err := gateway.NewSessionInspectionPlane(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		config gateway.Config
		want   string
	}{
		{
			name:   "plane without handler",
			config: gateway.Config{Binding: bind, SessionInspection: plane},
			want:   "must be supplied together",
		},
		{
			name:   "handler without plane",
			config: gateway.Config{Binding: bind, ManagementHandler: http.NotFoundHandler()},
			want:   "must be supplied together",
		},
		{
			name: "external TTL shadow",
			config: gateway.Config{
				Binding: bind, SessionInspection: plane,
				ManagementHandler: http.NotFoundHandler(), InspectionTokenTTL: time.Minute,
			},
			want: "TTL belongs",
		},
		{
			name: "zero plane",
			config: gateway.Config{
				Binding: bind, SessionInspection: &gateway.SessionInspectionPlane{},
				ManagementHandler: http.NotFoundHandler(),
			},
			want: "plane is incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := gateway.New(test.config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("gateway composition error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSessionInspectionPlaneRejectsInvalidCapabilityLifetime(t *testing.T) {
	for _, ttl := range []time.Duration{-time.Nanosecond, management.MaximumCapabilityTTL + time.Nanosecond} {
		if _, err := gateway.NewSessionInspectionPlane(ttl); err == nil ||
			!strings.Contains(err.Error(), "inspection token TTL") {
			t.Fatalf("inspection plane TTL %s error = %v", ttl, err)
		}
	}
}

func TestGatewayCompatibilityHandlerDelegatesInjectedManagementWithoutChangingAuth(t *testing.T) {
	plane, err := gateway.NewSessionInspectionPlane(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []managementCall
	canonical := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls = append(calls, managementCall{
			path: request.URL.Path, capability: request.Header.Get(management.CapabilityHeader),
		})
		mu.Unlock()
		writer.WriteHeader(299)
	})
	server, err := gateway.New(gateway.Config{
		Binding: compositionBinding(t), Token: "deployment-secret",
		SessionInspection: plane, ManagementHandler: canonical,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close composed gateway: %v", err)
		}
	})
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	canonicalRequest, err := http.NewRequest(
		http.MethodGet, httpServer.URL+management.APIPrefix+"/sessions/sess_exact/live", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRequest.Header.Set(management.CapabilityHeader, "canonical-token")
	canonicalResponse, err := http.DefaultClient.Do(canonicalRequest)
	if err != nil {
		t.Fatal(err)
	}
	canonicalResponse.Body.Close()
	if canonicalResponse.StatusCode != 299 {
		t.Fatalf("canonical injected management status = %d", canonicalResponse.StatusCode)
	}

	legacyPath := "/v1/realtime/sessions/sess_exact/live"
	unauthorized, err := http.Get(httpServer.URL + legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("legacy alias without deployment bearer = %d", unauthorized.StatusCode)
	}

	legacyRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+legacyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest.Header.Set("Authorization", "Bearer deployment-secret")
	legacyRequest.Header.Set(gateway.InspectionTokenHeader, "legacy-token")
	legacyResponse, err := http.DefaultClient.Do(legacyRequest)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse.Body.Close()
	if legacyResponse.StatusCode != 299 {
		t.Fatalf("authorized legacy alias status = %d", legacyResponse.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0] != (managementCall{
		path: management.APIPrefix + "/sessions/sess_exact/live", capability: "canonical-token",
	}) || calls[1] != (managementCall{
		path: management.APIPrefix + "/sessions/sess_exact/live", capability: "legacy-token",
	}) {
		t.Fatalf("injected canonical management calls = %+v", calls)
	}
}

type managementCall struct {
	path       string
	capability string
}

func compositionBinding(t *testing.T) *cascade.Binding {
	t.Helper()
	bind, err := cascade.New(cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) { return staticASR{text: "hi"}, nil },
		Fast:       fast(), Slow: slow(), Speech: toneSpeech{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bind
}
