package bench_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

func TestLiveInspectionClientAttestsTheNegotiatedSessionNotTheTaskLabel(t *testing.T) {
	graph, configuration, expected := attestationFixture(t)
	snapshot := liveInspectionFixture(t, graph, configuration, expected)
	access := testInspectionAccess("sess_exact")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != access.Path {
			t.Errorf("inspection path = %q, want %q", request.URL.Path, access.Path)
		}
		if request.Header.Get(management.CapabilityHeader) != access.Token {
			t.Errorf("inspection capability was not presented: %v", request.Header)
		}
		if request.Header.Get("Authorization") != "" {
			t.Errorf("deployment credential leaked into management request")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(writer).Encode(snapshot)
	}))
	t.Cleanup(server.Close)

	client := bench.LiveInspectionClient{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
	}
	resolver, err := client.Resolver(graph, configuration, expected)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := resolver(context.Background(), bench.AttestationRequest{
		// Deliberately unrelated to the server session ID: Scope labels the
		// benchmark row and must never select management authority.
		Scope: "meeting-task-17", Inspection: &access,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRequirement, err := bench.RequireGraph(graph, configuration, expected)
	if err != nil {
		t.Fatal(err)
	}
	actualRequirement, err := bench.RequireGraph(graph, configuration, actual)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualRequirement, wantRequirement) {
		t.Fatalf("live resolution changed\nwant: %+v\n got: %+v", expected, actual)
	}

	attestor := bench.GraphAttestor{
		Graph: graph, Configuration: configuration, Resolve: resolver,
	}
	evidence, err := attestor.Attest(context.Background(), bench.AttestationRequest{
		Scope: "meeting-task-17", Inspection: &access,
		Status: binding.Status{Graph: binding.ArchitectureIdentity{
			ID: graph.ID, Revision: int(graph.Revision), Fingerprint: graph.Fingerprint,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Scope != "meeting-task-17" || evidence.Graph == nil ||
		evidence.Graph.Graph.Fingerprint != graph.Fingerprint {
		t.Fatalf("remote inspection evidence = %+v", evidence)
	}
}

func TestLiveInspectionClientRefusesRedirectExpiryAndInsecureRemoteOrigin(t *testing.T) {
	access := testInspectionAccess("sess_redirect")
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", destination.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	client := bench.LiveInspectionClient{Endpoint: "ws" + strings.TrimPrefix(origin.URL, "http")}
	if _, err := client.Snapshot(context.Background(), access); err == nil ||
		!strings.Contains(err.Error(), "307") {
		t.Fatalf("inspection redirect error = %v", err)
	}
	if redirected.Load() != 0 {
		t.Fatal("inspection credentials followed a redirect")
	}

	expired := access
	expired.ExpiresAtMS = time.Now().Add(-time.Second).UnixMilli()
	if _, err := client.Snapshot(context.Background(), expired); err == nil ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired inspection access error = %v", err)
	}
	remote := bench.LiveInspectionClient{Endpoint: "ws://example.com/v1/realtime"}
	if _, err := remote.Snapshot(context.Background(), access); err == nil ||
		!strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("insecure remote inspection error = %v", err)
	}
}

func TestLiveInspectionClientRejectsNonManagementCapabilitiesBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	client := bench.LiveInspectionClient{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}
	valid := testInspectionAccess("sess_capability")
	tests := []struct {
		name  string
		token string
	}{
		{name: "obsolete inspection prefix", token: "ins_" + strings.TrimPrefix(valid.Token, "mgmt_")},
		{name: "missing prefix", token: strings.TrimPrefix(valid.Token, "mgmt_")},
		{name: "short random value", token: "mgmt_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))},
		{name: "long random value", token: "mgmt_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 33))},
		{name: "padded base64", token: valid.Token + "="},
		{name: "header injection", token: valid.Token + "\r\nX-Forged: true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			access := valid
			access.Token = test.token
			if _, err := client.Snapshot(context.Background(), access); err == nil ||
				!strings.Contains(err.Error(), "invalid capability") {
				t.Fatalf("invalid inspection capability error = %v", err)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid inspection capabilities reached the network %d times", requests.Load())
	}
}

func TestLiveInspectionClientRequiresStrictBoundedJSONAndExactNoStoreDirective(t *testing.T) {
	access := testInspectionAccess("sess_response")
	tests := []struct {
		name        string
		contentType string
		cache       string
		body        string
		want        string
	}{
		{
			name: "unknown field", contentType: "application/json", cache: "no-store",
			body: `{"unknown":true}`, want: "unknown field",
		},
		{
			name: "duplicate field", contentType: "application/json", cache: "no-store",
			body: `{"format_version":1,"format_version":1}`, want: "duplicate JSON key",
		},
		{
			name: "wrong media type", contentType: "text/plain", cache: "no-store",
			body: `{}`, want: "content type",
		},
		{
			name: "lookalike cache directive", contentType: "application/json", cache: "no-storex",
			body: `{}`, want: "not marked no-store",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				writer.Header().Set("Cache-Control", test.cache)
				_, _ = writer.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			client := bench.LiveInspectionClient{
				Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
			}
			if _, err := client.Snapshot(context.Background(), access); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("inspection response error = %v, want marker %q", err, test.want)
			}
		})
	}
}

func testInspectionAccess(sessionID string) openrealtime.InspectionAccess {
	return openrealtime.InspectionAccess{
		SessionID: sessionID,
		Path:      management.APIPrefix + "/sessions/" + sessionID + "/live",
		Token: "mgmt_" + base64.RawURLEncoding.EncodeToString(
			bytes.Repeat([]byte{0x51}, 32),
		),
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}
}
