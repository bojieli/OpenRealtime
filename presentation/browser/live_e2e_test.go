package browser_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	"github.com/bojieli/OpenRealtime/presentation/host"
	"github.com/coder/websocket"
)

const presentationLiveEndpointEnvironment = "OPENREALTIME_PRESENTATION_LIVE_ENDPOINT"

// TestLiveComposablePresentationClientAgainstRealModelInChromium always runs
// the exact public composition against a hermetic protocol peer. When the
// provisioned endpoint is supplied, the same test and browser driver become
// the behavioral real-model release gate. Unlike the retired monolithic
// surface, only descriptor-locked host and browser plugins are mounted; the
// upstream credential and endpoint remain inside the host relay.
func TestLiveComposablePresentationClientAgainstRealModelInChromium(t *testing.T) {
	challenge := livePresentationChallenge(t)
	endpoint := strings.TrimSpace(os.Getenv(presentationLiveEndpointEnvironment))
	live := endpoint != ""
	token := os.Getenv("OPENREALTIME_TOKEN")
	model := strings.TrimSpace(os.Getenv("OPENREALTIME_PRESENTATION_MODEL"))
	var fixtureSawChallenge atomic.Bool
	if live {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != endpoint {
			t.Fatalf("%s must be an exact credential-free ws/wss URL", presentationLiveEndpointEnvironment)
		}
	} else {
		token = "fixture-host-only-token"
		backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Bearer "+token {
				http.Error(writer, "fixture credential missing", http.StatusUnauthorized)
				return
			}
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			_ = connection.Write(request.Context(), websocket.MessageText, []byte(
				`{"type":"session.created","session":{"id":"fixture-live-presentation"}}`,
			))
			for {
				_, payload, err := connection.Read(request.Context())
				if err != nil {
					return
				}
				if bytes.Contains(payload, []byte(challenge)) {
					fixtureSawChallenge.Store(true)
				}
				var event struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(payload, &event) != nil || event.Type != "response.create" {
					continue
				}
				for _, response := range []string{
					`{"type":"response.created","response":{"id":"fixture-response","status":"in_progress"}}`,
					fmt.Sprintf(`{"type":"response.output_text.delta","response_id":"fixture-response","item_id":"fixture-output","delta":%q}`, challenge),
					`{"type":"response.done","response":{"id":"fixture-response","status":"completed","status_details":null}}`,
				} {
					if err := connection.Write(request.Context(), websocket.MessageText, []byte(response)); err != nil {
						return
					}
				}
			}
		}))
		defer backend.Close()
		endpoint = "ws" + strings.TrimPrefix(backend.URL, "http")
		model = ""
	}
	node, chromium := requireBrowser(t)

	bundle, err := presentationbrowser.MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	router := host.NewRouterFactory()
	target := host.NewEndpointDirectoryFactory()
	relay := host.NewWebSocketRelayFactory(nil)
	var credential *host.CredentialFactory
	if token == "" {
		credential = host.NewAnonymousCredentialFactory()
	} else {
		credential, err = host.NewBearerCredentialFactory(token)
		if err != nil {
			t.Fatal(err)
		}
	}
	factories := []pluginruntime.Factory{
		bundle.Shell, bundle.ManifestHost, bundle.ModuleStore, relay, target, credential, router,
	}
	ids := map[string]string{
		bundle.Shell.Descriptor().Name:        "shell",
		bundle.ManifestHost.Descriptor().Name: "manifest",
		bundle.ModuleStore.Descriptor().Name:  "modules",
		relay.Descriptor().Name:               "relay",
		target.Descriptor().Name:              "target",
		credential.Descriptor().Name:          "credential",
		router.Descriptor().Name:              "router",
	}
	plan := compileHostPlan(t, factories, ids)
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	targetValues, err := json.Marshal(host.EndpointDirectoryConfig{
		Endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: endpoint,
		}},
		Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	permissions := map[string][]plugin.Permission{"relay": {{
		Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"websocket"},
	}}}
	if token != "" {
		permissions["credential"] = []plugin.Permission{{
			Kind: "secret.read", Resource: "realtime-credential", Operations: []string{"read"},
		}}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": targetValues}, Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := host.HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	assertPresentationHostHidesUpstream(t, server.URL, endpoint, token)
	driver, err := filepath.Abs(filepath.Join("testdata", "live_composable.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, node, driver, server.URL)
	command.Env = append(os.Environ(),
		"CHROMIUM="+chromium, "CDP_PORT="+freePort(t), "LIVE_PRESENTATION_CHALLENGE="+challenge,
	)
	output, err := command.CombinedOutput()
	t.Log("\n" + string(output))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("live composable presentation timed out: %v", ctx.Err())
		}
		t.Fatalf("live composable presentation failed: %v", err)
	}
	if !live && !fixtureSawChallenge.Load() {
		t.Fatal("hermetic protocol peer did not receive the unpredictable challenge")
	}
	if live {
		fmt.Fprintln(os.Stderr, "live composable presentation client completed")
	}
}

func assertPresentationHostHidesUpstream(t *testing.T, base, endpoint, token string) {
	t.Helper()
	for _, path := range []string{"/", "/client/v1/manifest"} {
		response, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("presentation resource %s status=%d read=%v", path, response.StatusCode, readErr)
		}
		for name, secret := range map[string]string{"upstream endpoint": endpoint, "bearer token": token} {
			if secret != "" && bytes.Contains(body, []byte(secret)) {
				t.Fatalf("presentation resource %s exposed the %s", path, name)
			}
		}
	}
}

func livePresentationChallenge(t *testing.T) string {
	t.Helper()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return "OPENREALTIME-LIVE-" + hex.EncodeToString(nonce[:])
}
