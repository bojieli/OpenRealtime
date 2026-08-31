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

const (
	presentationLiveEndpointEnvironment = "OPENREALTIME_PRESENTATION_LIVE_ENDPOINT"
	presentationLiveRequiredEnvironment = "OPENREALTIME_PRESENTATION_LIVE_REQUIRED"
)

// TestLiveComposablePresentationClientAgainstRealModelInChromium always runs
// the exact public composition against a hermetic protocol peer. When the
// provisioned endpoint is supplied, the same test and browser driver become
// the behavioral real-model release gate. Unlike the retired monolithic
// surface, only descriptor-locked host and browser plugins are mounted; the
// upstream credential and endpoint remain inside the host relay.
func TestLiveComposablePresentationClientAgainstRealModelInChromium(t *testing.T) {
	challenge := livePresentationChallenge(t)
	endpoint, err := livePresentationEndpointFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	live := endpoint != ""
	token := os.Getenv("OPENREALTIME_TOKEN")
	model := strings.TrimSpace(os.Getenv("OPENREALTIME_PRESENTATION_MODEL"))
	var fixtureSawToolOutput atomic.Bool
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
			responseIndex := 0
			for {
				_, payload, err := connection.Read(request.Context())
				if err != nil {
					return
				}
				var event struct {
					Type    string          `json:"type"`
					Session json.RawMessage `json:"session"`
				}
				if json.Unmarshal(payload, &event) != nil {
					continue
				}
				if event.Type == "session.update" {
					response := fmt.Sprintf(`{"type":"session.updated","session":%s}`, event.Session)
					if err := connection.Write(request.Context(), websocket.MessageText, []byte(response)); err != nil {
						return
					}
					continue
				}
				if event.Type == "conversation.item.create" && bytes.Contains(payload, []byte(challenge)) {
					fixtureSawToolOutput.Store(true)
				}
				if event.Type != "response.create" {
					continue
				}
				responseIndex++
				responses := []string{
					fmt.Sprintf(`{"type":"response.created","response":{"id":"fixture-response-%d","status":"in_progress"}}`, responseIndex),
				}
				if responseIndex == 1 {
					responses = append(responses,
						`{"type":"response.function_call_arguments.done","response_id":"fixture-response-1","item_id":"fixture-tool-item","call_id":"fixture-challenge-call","name":"read_release_challenge","arguments":"{}"}`,
					)
				} else {
					responses = append(responses, fmt.Sprintf(
						`{"type":"response.output_text.delta","response_id":"fixture-response-2","item_id":"fixture-output","delta":%q}`,
						challenge,
					))
				}
				responses = append(responses, fmt.Sprintf(
					`{"type":"response.done","response":{"id":"fixture-response-%d","status":"completed","status_details":null}}`,
					responseIndex,
				))
				for _, response := range responses {
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

	bundle, err := presentationbrowser.ComposeTextBundle(
		"openrealtime.browser.live-tool-challenge", []presentationbrowser.ClientModule{
			liveChallengeClientModule(challenge),
		},
	)
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
	if !live && !fixtureSawToolOutput.Load() {
		t.Fatal("hermetic protocol peer did not receive the unpredictable tool output")
	}
	if live {
		fmt.Fprintln(os.Stderr, "live composable presentation client completed")
	}
}

func TestLivePresentationGateRequiresProvisionedEndpointWhenMarkedRequired(t *testing.T) {
	t.Setenv(presentationLiveEndpointEnvironment, "")
	t.Setenv(presentationLiveRequiredEnvironment, "1")
	if _, err := livePresentationEndpointFromEnvironment(); err == nil ||
		!strings.Contains(err.Error(), presentationLiveEndpointEnvironment) {
		t.Fatalf("missing required live endpoint error = %v", err)
	}

	t.Setenv(presentationLiveRequiredEnvironment, "yes")
	if _, err := livePresentationEndpointFromEnvironment(); err == nil ||
		!strings.Contains(err.Error(), "must be empty or 1") {
		t.Fatalf("invalid live-required sentinel error = %v", err)
	}
}

func livePresentationEndpointFromEnvironment() (string, error) {
	endpoint := strings.TrimSpace(os.Getenv(presentationLiveEndpointEnvironment))
	required := strings.TrimSpace(os.Getenv(presentationLiveRequiredEnvironment))
	if required != "" && required != "1" {
		return "", fmt.Errorf("%s must be empty or 1", presentationLiveRequiredEnvironment)
	}
	if required == "1" && endpoint == "" {
		return "", fmt.Errorf("%s requires %s", presentationLiveRequiredEnvironment,
			presentationLiveEndpointEnvironment)
	}
	return endpoint, nil
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

func liveChallengeClientModule(challenge string) presentationbrowser.ClientModule {
	source := fmt.Sprintf(`const challenge = %q;
export default {
  name: "openrealtime.presentation.client.release-challenge",
  revision: 1,
  async mount(context) {
    const state = context.services.get("presentation.client.session_state");
    const events = context.services.get("presentation.client.protocol_events");
    const configuration = context.services.get("presentation.client.session_configuration");
    if (!state || !events || !configuration) throw new Error("release challenge dependencies are unavailable");
    context.root.dataset.challengeToolCalls = "0";
    context.root.dataset.challengeToolNegotiated = "no";
    const remove = configuration.contribute({tools: [{
      type: "function",
      name: "read_release_challenge",
      description: "Read the opaque release-check value. Use this tool when asked for that value; it has no arguments.",
      parameters: {type: "object", properties: {}, required: [], additionalProperties: false},
    }]});
    let pending = "";
    let calls = 0;
    const unsubscribe = events.subscribe((event) => {
      if (event?.type === "session.updated") {
        const tools = event.session?.tools;
        context.root.dataset.challengeToolNegotiated = Array.isArray(tools) &&
          tools.some((tool) => tool?.name === "read_release_challenge") ? "yes" : "no";
        return;
      }
      if (event?.type === "response.function_call_arguments.done" &&
          event.name === "read_release_challenge") {
        if (pending) throw new Error("release challenge tool was called concurrently");
        pending = event.call_id;
        calls++;
        context.root.dataset.challengeToolCalls = String(calls);
        return;
      }
      if (event?.type === "response.done" && pending) {
        const callID = pending;
        pending = "";
        state.toolResult(callID, "done", challenge, "");
      }
    });
    context.lifecycle.defer("release-challenge-events", unsubscribe);
    context.lifecycle.defer("release-challenge-configuration", remove);
  },
};
`, challenge)
	return presentationbrowser.ClientModule{
		Entry: "release-challenge", Entrypoint: "release-challenge.js",
		PluginName: "openrealtime.presentation.client.release-challenge", Source: []byte(source),
		Requires: []plugin.Requirement{
			{Contract: presentation.ClientStateContract},
			{Contract: presentation.ClientProtocolEventsContract},
			{Contract: presentation.ClientSessionConfigurationContract},
		},
	}
}
