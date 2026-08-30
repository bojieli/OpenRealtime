package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	presentationhost "github.com/bojieli/OpenRealtime/presentation/host"
)

func TestCompilePresentationHostPinsEveryPlugin(t *testing.T) {
	bundle, err := presentationbrowser.MinimalBundle()
	if err != nil {
		t.Fatal(err)
	}
	instances := []namedPresentationFactory{
		{id: "shell", factory: bundle.Shell},
		{id: "manifest", factory: bundle.ManifestHost},
		{id: "modules", factory: bundle.ModuleStore},
		{id: "relay", factory: presentationhost.NewWebSocketRelayFactory(nil)},
		{id: "target", factory: presentationhost.NewEndpointDirectoryFactory()},
		{id: "credential", factory: presentationhost.NewAnonymousCredentialFactory()},
		{id: "listener", factory: presentationhost.NewLoopbackListenerFactory()},
		{id: "router", factory: presentationhost.NewRouterFactory()},
	}
	plan, registry, err := compilePresentationHost("openrealtime.host.test", instances)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if registry == nil || len(plan.Entries) != len(instances) || len(plan.Exports) != 2 {
		t.Fatalf("compiled presentation host = %#v, registry=%#v", plan, registry)
	}
	alternate, _, err := compilePresentationHost("openrealtime.host.other", instances)
	if err != nil {
		t.Fatal(err)
	}
	if alternate.ProfileFingerprint == plan.ProfileFingerprint || alternate.Fingerprint == plan.Fingerprint {
		t.Fatal("presentation host profile name did not bind the profile and plan identities")
	}
	if _, _, err := compilePresentationHost("openrealtime.host.test", append(instances,
		namedPresentationFactory{id: "router", factory: presentationhost.NewRouterFactory()})); err == nil || !strings.Contains(err.Error(), "repeats instance") {
		t.Fatalf("duplicate instance error = %v", err)
	}
	if _, _, err := compilePresentationHost("", instances); err == nil ||
		!strings.Contains(err.Error(), "profile name is required") {
		t.Fatalf("missing profile name error = %v", err)
	}
}

func TestPresentationEndpointConfigDeclaresOnlySelectedExactEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		websocket  string
		webrtc     string
		management string
		want       map[presentation.EndpointName]string
	}{
		{
			name: "minimal websocket", profile: "browser-minimal",
			websocket: "wss://realtime.example.test/v1/realtime?model=exact",
			want: map[presentation.EndpointName]string{
				presentation.EndpointRealtimeWebSocket: "wss://realtime.example.test/v1/realtime?model=exact",
			},
		},
		{
			name: "developer distinct management origin", profile: "browser-developer",
			websocket:  "wss://realtime.example.test/v1/realtime",
			management: "https://management.example.test/custom/openrealtime/v7",
			want: map[presentation.EndpointName]string{
				presentation.EndpointRealtimeWebSocket: "wss://realtime.example.test/v1/realtime",
				presentation.EndpointManagement:        "https://management.example.test/custom/openrealtime/v7",
			},
		},
		{
			name: "developer WebRTC without inferred websocket", profile: "browser-developer-webrtc",
			websocket:  "wss://must-not-be-declared.example.test/v1/realtime",
			webrtc:     "https://media.example.test/v1/realtime/calls",
			management: "https://management.example.test/openrealtime/v1",
			want: map[presentation.EndpointName]string{
				presentation.EndpointRealtimeWebRTC: "https://media.example.test/v1/realtime/calls",
				presentation.EndpointManagement:     "https://management.example.test/openrealtime/v1",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := presentationEndpointConfig(
				test.profile, test.websocket, test.webrtc, test.management, "model-v1",
			)
			if err != nil {
				t.Fatal(err)
			}
			if config.Model != "model-v1" || len(config.Endpoints) != len(test.want) {
				t.Fatalf("endpoint config = %#v, want %#v", config, test.want)
			}
			for _, endpoint := range config.Endpoints {
				if test.want[endpoint.Name] != endpoint.URL {
					t.Errorf("unexpected endpoint %#v", endpoint)
				}
			}
			directory, err := presentation.FreezeEndpointDirectory(config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			if err := directory.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPresentRejectsUnsafeListenerBeforeStarting(t *testing.T) {
	var output bytes.Buffer
	err := runPresent([]string{
		"-listen", "0.0.0.0:0", "-endpoint", "ws://127.0.0.1:1/v1/realtime", "-token-env", "",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("unsafe listener error = %v", err)
	}
}

func TestPresentWebRTCProfileRequiresExplicitAdapterEndpoint(t *testing.T) {
	var output bytes.Buffer
	err := runPresent([]string{
		"-client-profile", "browser-developer-webrtc",
		"-endpoint", "ws://127.0.0.1:1/v1/realtime", "-token-env", "",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "requires -webrtc-endpoint") {
		t.Fatalf("missing WebRTC endpoint error = %v", err)
	}
}

func TestPresentDeveloperProfilesRequireExplicitManagementEndpoint(t *testing.T) {
	for _, profile := range []string{"browser-developer", "browser-developer-webrtc"} {
		t.Run(profile, func(t *testing.T) {
			arguments := []string{
				"-client-profile", profile,
				"-endpoint", "ws://127.0.0.1:1/v1/realtime", "-token-env", "",
			}
			if profile == "browser-developer-webrtc" {
				arguments = append(arguments,
					"-webrtc-endpoint", "http://127.0.0.1:1/v1/realtime/calls",
				)
			}
			var output bytes.Buffer
			err := runPresent(arguments, &output)
			if err == nil || !strings.Contains(err.Error(), "requires -management-endpoint") {
				t.Fatalf("missing management endpoint error = %v", err)
			}
		})
	}
}

func TestPresentationEndpointConfigRejectsCredentialBearingURL(t *testing.T) {
	_, err := presentationEndpointConfig(
		"browser-minimal", "wss://token:secret@realtime.example.test/v1/realtime", "", "", "",
	)
	if err == nil || !strings.Contains(err.Error(), "credential-free") {
		t.Fatalf("credential-bearing endpoint error = %v", err)
	}
}

func TestPresentDeveloperProfilesSelectObserverOnlyCompositions(t *testing.T) {
	tests := []struct {
		name      string
		profile   string
		webrtc    string
		wantMedia bool
	}{
		{name: "websocket", profile: "browser-developer"},
		{name: "webrtc", profile: "browser-developer-webrtc", webrtc: "https://adapter.invalid/v1/realtime/calls", wantMedia: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle, err := selectPresentationBundle(test.profile, test.webrtc)
			if err != nil {
				t.Fatal(err)
			}
			entries := make(map[string]bool, len(bundle.Plan.Entries))
			for _, planned := range bundle.Plan.Entries {
				entries[planned.Entry.ID] = true
			}
			for _, required := range []string{
				"management-operator", "management-transport", "management-authoring",
				"management-operator-view", "authoring-editor-view",
				"authoring-configuration-view", "authoring-canvas-view",
			} {
				if !entries[required] {
					t.Errorf("observer profile omitted %q", required)
				}
			}
			for _, forbidden := range []string{
				"effects", "artifact-references", "confirmation-view", "artifact-view",
			} {
				if entries[forbidden] {
					t.Errorf("normal present profile selected implicit effect entry %q", forbidden)
				}
			}
			for _, endpoint := range bundle.Manifest.Endpoints {
				if endpoint.Name == "effects.local" {
					t.Errorf("normal present profile advertised implicit effects endpoint %#v", endpoint)
				}
			}
			if got := entries["media"]; got != test.wantMedia {
				t.Errorf("media entry present = %t, want %t", got, test.wantMedia)
			}
		})
	}
}
