package presentation_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/presentation"
)

func TestEndpointDirectoryIsExactCanonicalAndSnapshotIsolated(t *testing.T) {
	source := []presentation.Endpoint{
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example.test/custom/v7",
		},
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: "wss://realtime.example.test/v1/realtime?deployment=pinned",
		},
		{
			Name: presentation.EndpointEffects, Protocol: presentation.ProtocolClientEffects,
			URL: "wss://effects.example.test/client/v1/effects",
		},
		{
			Name: presentation.EndpointArtifacts, Protocol: presentation.ProtocolHostArtifacts,
			URL: "https://resources.example.test/client/v1/artifacts",
		},
		{
			Name: presentation.EndpointDownloads, Protocol: presentation.ProtocolHostDownloads,
			URL: "https://downloads.example.test/client/v1/downloads",
		},
		{
			Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: "https://media.example.test/v1/realtime/calls",
		},
	}
	directory, err := presentation.FreezeEndpointDirectory(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(directory.Fingerprint, "sha256:") ||
		len(directory.Fingerprint) != len("sha256:")+64 {
		t.Fatalf("endpoint directory fingerprint = %q", directory.Fingerprint)
	}
	reversed := append([]presentation.Endpoint(nil), source...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	reordered, err := presentation.FreezeEndpointDirectory(reversed)
	if err != nil || reordered.Fingerprint != directory.Fingerprint {
		t.Fatalf("reordered endpoint directory identity = %q, %v", reordered.Fingerprint, err)
	}
	names := make([]presentation.EndpointName, len(directory.Endpoints))
	for index, endpoint := range directory.Endpoints {
		names[index] = endpoint.Name
	}
	wantNames := []presentation.EndpointName{
		presentation.EndpointEffects,
		presentation.EndpointManagement,
		presentation.EndpointRealtimeWebRTC,
		presentation.EndpointRealtimeWebSocket,
		presentation.EndpointArtifacts,
		presentation.EndpointDownloads,
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("canonical endpoint names = %v, want %v", names, wantNames)
	}

	source[0].URL = "https://attacker.invalid/rebound"
	clone := directory.Clone()
	clone.Endpoints[0].URL = "wss://attacker.invalid/rebound"
	management, err := directory.Require(
		presentation.EndpointManagement, presentation.ProtocolManagement,
	)
	if err != nil || management.URL != "https://management.example.test/custom/v7" {
		t.Fatalf("snapshot-isolated management endpoint = %#v, %v", management, err)
	}
	if _, err := directory.Require(
		presentation.EndpointManagement, presentation.ProtocolClientEffects,
	); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("wrong-protocol lookup error = %v", err)
	}
	withoutEffects, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{{
		Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
		URL: "wss://realtime.example.test/v1/realtime",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutEffects.Require(
		presentation.EndpointEffects, presentation.ProtocolClientEffects,
	); err == nil || !strings.Contains(err.Error(), "does not declare") {
		t.Fatalf("absent effects endpoint error = %v", err)
	}
}

func TestEndpointDirectoryRejectsUnknownDuplicateOrWidenableEntries(t *testing.T) {
	valid := presentation.Endpoint{
		Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
		URL: "https://management.example.test/openrealtime/v1",
	}
	tests := []struct {
		name      string
		endpoints []presentation.Endpoint
		want      string
	}{
		{name: "unknown name", endpoints: []presentation.Endpoint{{
			Name: "management.latest", Protocol: presentation.ProtocolManagement,
			URL: "https://management.example.test/openrealtime/v1",
		}}, want: "unknown endpoint name"},
		{name: "wrong protocol", endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolClientEffects,
			URL: "https://management.example.test/openrealtime/v1",
		}}, want: "protocol"},
		{name: "duplicate", endpoints: []presentation.Endpoint{valid, valid}, want: "repeats"},
		{name: "credential", endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://operator:secret@management.example.test/openrealtime/v1",
		}}, want: "credential-free"},
		{name: "wrong scheme", endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointEffects, Protocol: presentation.ProtocolClientEffects,
			URL: "https://effects.example.test/client/v1/effects",
		}}, want: "scheme"},
		{name: "base query", endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: "https://management.example.test/openrealtime/v1?token=forbidden",
		}}, want: "query-free"},
		{name: "base trailing slash", endpoints: []presentation.Endpoint{{
			Name: presentation.EndpointArtifacts, Protocol: presentation.ProtocolHostArtifacts,
			URL: "https://resources.example.test/client/v1/artifacts/",
		}}, want: "canonical base"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := presentation.FreezeEndpointDirectory(test.endpoints); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("FreezeEndpointDirectory error = %v, want %q", err, test.want)
			}
		})
	}

	directory, err := presentation.FreezeEndpointDirectory([]presentation.Endpoint{valid})
	if err != nil {
		t.Fatal(err)
	}
	tampered := directory.Clone()
	tampered.Endpoints[0].URL = "https://other.example.test/openrealtime/v1"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("tampered directory validation = %v", err)
	}
}
