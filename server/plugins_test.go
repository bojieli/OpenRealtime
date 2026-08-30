package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

func TestCompiledServerProfileExportsTheSameRealtimeAPIToAnyClient(t *testing.T) {
	factories := completeServerFactories(t)
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	providerArtifact := serverArtifact("go://openrealtime/test/session-provider", "build-1", "a")
	gatewayArtifact := serverArtifact("go://openrealtime/test/realtime-gateway", "build-1", "b")
	for id, factory := range factories {
		artifact := gatewayArtifact
		if id == "sessions" {
			artifact = providerArtifact
		}
		if err := registry.RegisterArtifact(factory.Descriptor().Name, artifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	realm, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close server realm: %v", err)
		}
	})

	value, contract, provider, revision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil {
		t.Fatal(err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok || contract != serverplugin.RealtimeHTTPContract() || provider != "http-router" || revision == 0 {
		t.Fatalf("server export = %T %+v %q revision=%d", value, contract, provider, revision)
	}
	httpServer := httptest.NewServer(service.Handler())
	t.Cleanup(httpServer.Close)

	response, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || health["binding"] != "server-profile-test" {
		t.Fatalf("profile-exported health = status %d payload %+v", response.StatusCode, health)
	}
	metrics, err := http.Get(httpServer.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK || metrics.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("profile-exported metrics = status %d headers %v", metrics.StatusCode, metrics.Header)
	}
	canonical, err := http.Get(httpServer.URL + management.APIPrefix + "/sessions/sess_missing/live")
	if err != nil {
		t.Fatal(err)
	}
	defer canonical.Body.Close()
	if canonical.StatusCode != http.StatusNotFound ||
		canonical.Header.Get("Cache-Control") != "no-store" ||
		canonical.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("canonical management route = status %d headers %v", canonical.StatusCode, canonical.Header)
	}

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/realtime?model=profile-test"
	connection, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "done")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatal(err)
	}
	if created["type"] != "session.created" {
		t.Fatalf("first realtime event = %+v", created)
	}

	live := realm.Live()
	if live.Fingerprint != plan.Fingerprint || live.Realm != plugin.ServerRealm ||
		!live.Exports[serverplugin.RealtimeHTTPExport].Available ||
		live.Entries["sessions"].Runtime != providerArtifact ||
		live.Entries["gateway"].Runtime != gatewayArtifact ||
		live.Entries["http-router"].Runtime != gatewayArtifact ||
		live.Entries["inspection"].Runtime != gatewayArtifact ||
		live.Entries["observability"].Runtime != gatewayArtifact ||
		live.Entries["session-api"].Runtime != gatewayArtifact ||
		len(live.Entries) != len(factories) {
		t.Fatalf("server realm live evidence = %+v", live)
	}
}

func TestServerProfileCannotCompileAListenerWithoutSessionProvider(t *testing.T) {
	factories := completeServerFactories(t)
	delete(factories, "sessions")
	serverPlan(t, factories, false)
}

func completeServerFactories(t *testing.T) map[string]pluginruntime.Factory {
	t.Helper()
	provider, err := serverplugin.NewSessionProviderFactory(serverTestProvider{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := serverplugin.NewSessionInspectionPlaneFactory(0)
	if err != nil {
		t.Fatal(err)
	}
	gatewayFactory, err := serverplugin.NewGatewayFactory(serverplugin.GatewayFactoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]pluginruntime.Factory{
		"http-router":       serverplugin.NewHTTPRouterFactory(),
		"sessions":          provider,
		"inspection":        inspection,
		"gateway":           gatewayFactory,
		"realtime":          serverplugin.NewRealtimeRouteFactory(),
		"observability":     serverplugin.NewObservabilityRouteFactory(),
		"session-api":       managementserver.NewSessionAPIFactory(),
		"inspection-compat": serverplugin.NewInspectionCompatibilityRouteFactory(),
	}
}

func serverPlan(t *testing.T, factories map[string]pluginruntime.Factory, wantSuccess bool) plugin.Plan {
	t.Helper()
	catalog := plugin.NewCatalog()
	order := []string{
		"http-router", "sessions", "inspection", "gateway", "realtime",
		"observability", "session-api", "inspection-compat", "conflict",
	}
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	for _, id := range order {
		factory, present := factories[id]
		if !present {
			continue
		}
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: id, Plugin: factory.Descriptor().Name, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "openrealtime.server.test-profile", Revision: 1,
		Realm: plugin.ServerRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries,
		Exports: []plugin.ProfileExport{{
			Name: serverplugin.RealtimeHTTPExport, Provider: "http-router",
			Service: serverplugin.RealtimeHTTPContract().Name,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if wantSuccess {
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	if err == nil || !strings.Contains(err.Error(), "requires unavailable service") {
		t.Fatalf("gateway-only server profile error = %v", err)
	}
	return plugin.Plan{}
}

func serverArtifact(id, revision, hexadecimal string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + strings.Repeat(hexadecimal, 64),
	}
}

type serverTestProvider struct{}

func (serverTestProvider) Name() string { return "server-profile-test" }
func (serverTestProvider) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
}
func (serverTestProvider) Capabilities() binding.Capabilities {
	return binding.Capabilities{FastSlow: true}
}
func (serverTestProvider) Start(
	_ context.Context, options binding.Options,
) (binding.Runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("test session requires sink")
	}
	return serverTestRuntime{}, nil
}

type serverTestRuntime struct{}

func (serverTestRuntime) Update(context.Context, binding.Settings) error { return nil }
func (serverTestRuntime) Audio(context.Context, perception.Frame) error  { return nil }
func (serverTestRuntime) Video(context.Context, perception.Frame) error {
	return binding.ErrUnsupported
}
func (serverTestRuntime) Text(context.Context, binding.TextInput) error           { return nil }
func (serverTestRuntime) ToolResult(context.Context, trajectory.ToolResult) error { return nil }
func (serverTestRuntime) CommitAudio(context.Context) error                       { return nil }
func (serverTestRuntime) CreateResponse(context.Context) error                    { return nil }
func (serverTestRuntime) Cancel(context.Context, string) error                    { return nil }
func (serverTestRuntime) Truncate(context.Context, binding.Truncation) error      { return nil }
func (serverTestRuntime) Trajectory() trajectory.Snapshot                         { return trajectory.Snapshot{} }
func (serverTestRuntime) Status() binding.Status                                  { return binding.Status{} }
func (serverTestRuntime) Close(context.Context, error) error                      { return nil }
