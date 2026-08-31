package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestEndpointDirectoryFactoryMountsExactExplicitProfile(t *testing.T) {
	factory := NewEndpointDirectoryFactory()
	descriptor := factory.Descriptor()
	if descriptor.Name != "openrealtime.presentation.host.endpoint-directory" ||
		descriptor.Revision != 1 || descriptor.ConfigSchema == nil ||
		*descriptor.ConfigSchema != presentation.EndpointDirectoryConfigContract ||
		!reflect.DeepEqual(descriptor.Provides, []plugin.Contract{presentation.EndpointDirectoryContract}) {
		t.Fatalf("strict endpoint directory descriptor = %#v", descriptor)
	}
	endpoints := []presentation.Endpoint{
		{Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://realtime.example.test/v1/realtime"},
		{Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: "https://media.example.test/v1/realtime/calls"},
		{Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example.test/custom/v7"},
		{Name: presentation.EndpointEffects, Protocol: presentation.ProtocolClientEffects,
			URL: "wss://effects.example.test/client/v1/effects"},
		{Name: presentation.EndpointArtifacts, Protocol: presentation.ProtocolHostArtifacts,
			URL: "https://resources.example.test/client/v1/artifacts"},
		{Name: presentation.EndpointDownloads, Protocol: presentation.ProtocolHostDownloads,
			URL: "https://downloads.example.test/client/v1/downloads"},
	}
	values, err := json.Marshal(EndpointDirectoryConfig{
		Endpoints: endpoints, Model: "pinned-model", DialTimeoutMS: 1_500, ReadLimit: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted := mountEndpointTarget(t, factory, values)
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	value, contract, provider, revision, err := mounted.Export("endpoints")
	if err != nil || contract != presentation.EndpointDirectoryContract || provider != "target" || revision == 0 {
		t.Fatalf("endpoint directory export = %#v, %#v, %q, %d, %v",
			value, contract, provider, revision, err)
	}
	target, ok := value.(EndpointTarget)
	if !ok || target.Model() != "pinned-model" || target.DialTimeout().Milliseconds() != 1_500 ||
		target.ReadLimit() != 1<<20 {
		t.Fatalf("mounted endpoint target = %#v", value)
	}
	directory := target.Directory()
	if err := directory.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range endpoints {
		got, err := target.Endpoint(endpoint.Name, endpoint.Protocol)
		if err != nil || got != endpoint {
			t.Fatalf("mounted endpoint %s = %#v, %v", endpoint.Name, got, err)
		}
	}
	directory.Endpoints[0].URL = "wss://attacker.invalid/rebound"
	valueAgain, _, _, _, err := mounted.Export("endpoints")
	if err != nil {
		t.Fatal(err)
	}
	targetAgain := valueAgain.(EndpointTarget)
	effects, err := targetAgain.Endpoint(presentation.EndpointEffects, presentation.ProtocolClientEffects)
	if err != nil || effects.URL != "wss://effects.example.test/client/v1/effects" {
		t.Fatalf("mounted directory retained caller mutation: %#v, %v", effects, err)
	}
}

func TestEndpointDirectoryRejectsShortcutAndSameOriginConfiguration(t *testing.T) {
	strict := NewEndpointDirectoryFactory()
	legacyValues := json.RawMessage(`{"websocket":"wss://server.example.test/v1/realtime"}`)
	if err := strict.ValidateConfig(legacyValues); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("strict factory legacy config error = %v", err)
	}
	for _, shortcut := range []json.RawMessage{
		json.RawMessage(`{"same_origin":true}`),
		json.RawMessage(`{"management_path":"/openrealtime/v1"}`),
		json.RawMessage(`{"effects_path":"/client/v1/effects"}`),
	} {
		if err := strict.ValidateConfig(shortcut); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("strict factory shortcut config %s error = %v", shortcut, err)
		}
	}
}

func TestManagementRelayUsesDeclaredEndpointAndNeverRealtimeOrigin(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.URL.Path
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"format_version":1}`))
	}))
	defer backend.Close()

	router := NewRouterFactory()
	target := NewEndpointDirectoryFactory()
	relay := NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{relay, target, router}
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values, err := json.Marshal(EndpointDirectoryConfig{Endpoints: []presentation.Endpoint{
		{Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "ws://127.0.0.1:1/must-not-be-used-for-management"},
		{Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: backend.URL + "/custom/management/v7"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: makeHostPlan(t, factories), Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
		Permissions: map[string][]plugin.Permission{"management": {{
			Kind: connectPermissionKind, Resource: connectPermissionResource,
			Operations: []string{managementOperation},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostServer := httptest.NewServer(handler)
	defer hostServer.Close()
	request, err := http.NewRequest(
		http.MethodGet,
		hostServer.URL+"/client/v1/management/sessions/sess-1/live",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(management.CapabilityHeader, "mgmt_exact")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(payload), `"format_version":1`) {
		t.Fatalf("explicit management relay status=%d body=%s", response.StatusCode, payload)
	}
	if got := <-seen; got != "/custom/management/v7/sessions/sess-1/live" {
		t.Fatalf("management relay destination = %q", got)
	}
}

func TestManagementRelayRefusesProfileWithoutDeclaredEndpointBeforeRoutesMount(t *testing.T) {
	router := NewRouterFactory()
	target := NewEndpointDirectoryFactory()
	relay := NewManagementRelayFactory(nil, nil)
	factories := []pluginruntime.Factory{relay, target, router}
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values, err := json.Marshal(EndpointDirectoryConfig{Endpoints: []presentation.Endpoint{{
		Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
		URL: "ws://127.0.0.1:1/v1/realtime",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: makeHostPlan(t, factories), Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
		Permissions: map[string][]plugin.Permission{"management": {{
			Kind: connectPermissionKind, Resource: connectPermissionResource,
			Operations: []string{managementOperation},
		}}},
	})
	if mounted != nil || err == nil || !strings.Contains(err.Error(), `does not declare "management.canonical"`) {
		t.Fatalf("missing explicit management endpoint mount = %#v, %v", mounted, err)
	}

	requires := relay.Descriptor().Requires
	if !reflect.DeepEqual(requires, []plugin.Requirement{
		{Contract: presentation.HTTPRoutesContract},
		{Contract: presentation.EndpointDirectoryContract},
	}) {
		t.Fatalf("management relay dependencies = %#v", requires)
	}
}

func TestRealtimeRelaysRefuseUndeclaredTransportEndpointsAtMount(t *testing.T) {
	tests := []struct {
		name       string
		relay      pluginruntime.Factory
		entry      string
		permission plugin.Permission
		missing    presentation.EndpointName
	}{
		{
			name: "websocket", relay: NewWebSocketRelayFactory(nil), entry: "websocket",
			permission: relayPermission(websocketOperation), missing: presentation.EndpointRealtimeWebSocket,
		},
		{
			name: "webrtc", relay: NewWebRTCRelayFactory(nil, nil), entry: "webrtc",
			permission: relayPermission(httpOperation), missing: presentation.EndpointRealtimeWebRTC,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := NewRouterFactory()
			target := NewEndpointDirectoryFactory()
			credential := NewAnonymousCredentialFactory()
			factories := []pluginruntime.Factory{test.relay, target, credential, router}
			registry := pluginruntime.NewRegistry()
			for _, factory := range factories {
				if err := registry.Register("", factory); err != nil {
					t.Fatal(err)
				}
			}
			values, err := json.Marshal(EndpointDirectoryConfig{Endpoints: []presentation.Endpoint{{
				Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
				URL: "https://management.example.test/openrealtime/v1",
			}}})
			if err != nil {
				t.Fatal(err)
			}
			mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
				Plan: makeHostPlan(t, factories), Registry: registry,
				Values:      map[string]json.RawMessage{"target": values},
				Permissions: map[string][]plugin.Permission{test.entry: {test.permission}},
			})
			if mounted != nil || err == nil || !strings.Contains(
				err.Error(), `does not declare "`+string(test.missing)+`"`,
			) {
				t.Fatalf("missing %s endpoint mount = %#v, %v", test.missing, mounted, err)
			}
		})
	}
}

func mountEndpointTarget(
	t *testing.T,
	factory *TargetFactory,
	values json.RawMessage,
) *pluginruntime.Mounted {
	t.Helper()
	descriptor := factory.Descriptor()
	catalog := plugin.NewCatalog()
	if _, err := catalog.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	exports := []plugin.ProfileExport{{
		Name: "endpoints", Provider: "target", Service: presentation.EndpointDirectoryContract.Name,
	}}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "host.endpoint-directory.test", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{{ID: "target", Plugin: descriptor.Name, Scope: "root"}},
		Exports: exports,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	registry := pluginruntime.NewRegistry()
	if err := registry.Register("", factory); err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mounted
}

func testEndpointDirectoryValues(
	t *testing.T, model string, endpoints ...presentation.Endpoint,
) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(EndpointDirectoryConfig{Endpoints: endpoints, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
