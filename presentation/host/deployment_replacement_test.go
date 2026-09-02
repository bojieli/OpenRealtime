package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/coder/websocket"
)

func TestRelayDeploymentProvidersReplaceAtomically(t *testing.T) {
	backendV1 := newDeploymentRelayBackend(t, "backend-v1")
	defer backendV1.server.Close()
	backendV2 := newDeploymentRelayBackend(t, "backend-v2")
	defer backendV2.server.Close()

	router := NewRouterFactory()
	targetV1 := NewEndpointDirectoryFactory()
	targetV2 := NewEndpointDirectoryFactory()
	credentialV1, err := NewBearerCredentialFactory("fixture-token-v1")
	if err != nil {
		t.Fatal(err)
	}
	credentialV2, err := NewBearerCredentialFactory("fixture-token-v2")
	if err != nil {
		t.Fatal(err)
	}
	relay := NewWebSocketRelayFactory(nil)
	factories := []pluginruntime.Factory{relay, targetV1, credentialV1, router}
	plan := makeHostPlan(t, factories)
	registry := pluginruntime.NewRegistry()
	originalArtifacts := map[string]inspect.ArtifactIdentity{
		"websocket":  hostTestArtifact("go://host-websocket-relay", "build-1", "1"),
		"target":     hostTestArtifact("go://host-endpoint-directory-v1", "build-1", "2"),
		"credential": hostTestArtifact("go://host-secret-credential-v1", "build-1", "3"),
		"router":     hostTestArtifact("go://host-router", "build-1", "4"),
	}
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"websocket", relay}, {"target", targetV1},
		{"credential", credentialV1}, {"router", router},
	} {
		if err := registry.RegisterArtifact(
			row.factory.Descriptor().Name, originalArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	const targetImplementation = "openrealtime.presentation.host.endpoint-directory-v2"
	const credentialImplementation = "openrealtime.presentation.host.secret-credential-v2"
	candidateArtifacts := map[string]inspect.ArtifactIdentity{
		"target":     hostTestArtifact("go://host-endpoint-directory-v2", "build-2", "5"),
		"credential": hostTestArtifact("go://host-secret-credential-v2", "build-2", "6"),
	}
	if err := registry.RegisterArtifact(targetImplementation, candidateArtifacts["target"], targetV2); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterArtifact(
		credentialImplementation, candidateArtifacts["credential"], credentialV2,
	); err != nil {
		t.Fatal(err)
	}

	valuesV1 := deploymentRelayTargetValues(t, backendV1.server.URL, "fixture-model-v1")
	credentialGrant := plugin.Permission{
		Kind: credentialPermissionKind, Resource: credentialPermissionResource,
		Operations: []string{credentialPermissionOperation},
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": valuesV1},
		Permissions: map[string][]plugin.Permission{
			"credential": {credentialGrant},
			"websocket":  {relayPermission(websocketOperation)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", httpValue)
	}
	hostServer := httptest.NewServer(handler)
	defer hostServer.Close()
	relayURL := "ws" + strings.TrimPrefix(hostServer.URL, "http") + "/client/v1/realtime"

	predecessor := dialDeploymentRelay(t, relayURL, "backend-v1")
	if got := backendV1.awaitRequest(t); got != "Bearer fixture-token-v1\x00fixture-model-v1" {
		predecessor.CloseNow()
		t.Fatalf("predecessor backend authorization/model = %q", got)
	}
	backendV1.awaitActive(t, 1)
	echoDeploymentRelay(t, predecessor, "still-live")
	before := mounted.Live()
	if before.Entries["websocket"].Workers != 1 {
		t.Fatalf("predecessor relay ownership = %+v", before.Entries["websocket"])
	}

	if receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "credential", SetImplementation: true, Implementation: credentialImplementation,
			SetPermissions: true, Permissions: nil,
		}},
	}); err == nil || !strings.Contains(err.Error(), "secret-read grant") || receipt.FormatVersion != 0 {
		t.Fatalf("permissionless credential candidate receipt/error = %#v, %v", receipt, err)
	}
	afterRefusal := mounted.Live()
	if afterRefusal.Sequence != before.Sequence ||
		afterRefusal.Entries["credential"].Implementation != credentialV1.Descriptor().Name ||
		afterRefusal.Entries["credential"].Runtime != originalArtifacts["credential"] ||
		afterRefusal.Entries["websocket"].Workers != 1 {
		t.Fatalf("refused credential candidate disturbed predecessor = %+v", afterRefusal)
	}
	echoDeploymentRelay(t, predecessor, "still-v1")

	valuesV2 := deploymentRelayTargetValues(t, backendV2.server.URL, "fixture-model-v2")
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: afterRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{
			{Entry: "target", SetImplementation: true, Implementation: targetImplementation,
				SetConfig: true, Config: valuesV2},
			{Entry: "credential", SetImplementation: true, Implementation: credentialImplementation},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	backendV1.awaitActive(t, 0)
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRead()
	if _, _, err := predecessor.Read(readContext); err == nil {
		predecessor.CloseNow()
		t.Fatal("deployment replacement left the predecessor relay connected")
	}
	predecessor.CloseNow()
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 2 ||
		len(receipt.Retirements) != 3 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("deployment provider replacement receipt = %#v", receipt)
	}
	transitions := make(map[string]pluginruntime.EntryTransition, len(receipt.Transitions))
	for _, transition := range receipt.Transitions {
		transitions[transition.Entry] = transition
	}
	for _, entry := range []string{"target", "credential"} {
		transition, found := transitions[entry]
		if !found || transition.BeforeRuntime != originalArtifacts[entry] ||
			transition.AfterRuntime != candidateArtifacts[entry] {
			t.Fatalf("deployment provider transition %s = %#v", entry, transition)
		}
	}
	if transitions["target"].AfterImplementation != targetImplementation ||
		transitions["target"].BeforeConfigDigest == transitions["target"].AfterConfigDigest ||
		transitions["credential"].AfterImplementation != credentialImplementation {
		t.Fatalf("deployment provider transitions = %#v", transitions)
	}
	retired := make(map[string]bool, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retired[retirement.Entry] = true
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("deployment provider retirement retained ownership = %#v", retirement)
		}
	}
	for _, entry := range []string{"target", "credential", "websocket"} {
		if !retired[entry] {
			t.Fatalf("deployment replacement did not retire %s: %#v", entry, receipt.Retirements)
		}
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["target"].Implementation != targetImplementation ||
		after.Entries["target"].Runtime != candidateArtifacts["target"] ||
		after.Entries["credential"].Implementation != credentialImplementation ||
		after.Entries["credential"].Runtime != candidateArtifacts["credential"] ||
		after.Entries["websocket"].Runtime != originalArtifacts["websocket"] ||
		after.Entries["websocket"].Workers != 0 ||
		after.Entries["router"].Runtime != originalArtifacts["router"] {
		t.Fatalf("replacement deployment provider live evidence = %+v", after)
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPValue != httpValue || afterHTTPContract != httpContract ||
		afterHTTPProvider != httpProvider || afterHTTPRevision != httpRevision {
		t.Fatalf("stable host export changed across deployment replacement: %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}

	replacement := dialDeploymentRelay(t, relayURL, "backend-v2")
	if got := backendV2.awaitRequest(t); got != "Bearer fixture-token-v2\x00fixture-model-v2" {
		replacement.CloseNow()
		t.Fatalf("replacement backend authorization/model = %q", got)
	}
	backendV2.awaitActive(t, 1)
	echoDeploymentRelay(t, replacement, "running-v2")
	if err := replacement.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	backendV2.awaitActive(t, 0)
	deadline := time.Now().Add(2 * time.Second)
	for mounted.Live().Entries["websocket"].Workers != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if live := mounted.Live(); live.Entries["websocket"].Workers != 0 {
		t.Fatalf("replacement relay retained a worker = %+v", live.Entries["websocket"])
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
}

type deploymentRelayBackend struct {
	server   *httptest.Server
	requests chan string
	active   atomic.Int32
}

func newDeploymentRelayBackend(t *testing.T, greeting string) *deploymentRelayBackend {
	t.Helper()
	backend := &deploymentRelayBackend{requests: make(chan string, 4)}
	backend.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		backend.requests <- request.Header.Get("Authorization") + "\x00" + request.URL.Query().Get("model")
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		backend.active.Add(1)
		defer backend.active.Add(-1)
		defer connection.CloseNow()
		if err := connection.Write(request.Context(), websocket.MessageText, []byte(greeting)); err != nil {
			return
		}
		for {
			kind, payload, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			if err := connection.Write(request.Context(), kind, payload); err != nil {
				return
			}
		}
	}))
	return backend
}

func (backend *deploymentRelayBackend) awaitRequest(t *testing.T) string {
	t.Helper()
	select {
	case request := <-backend.requests:
		return request
	case <-time.After(2 * time.Second):
		t.Fatal("deployment relay backend did not receive a request")
		return ""
	}
}

func (backend *deploymentRelayBackend) awaitActive(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for backend.active.Load() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := backend.active.Load(); got != want {
		t.Fatalf("active deployment relay backend sessions = %d, want %d", got, want)
	}
}

func deploymentRelayTargetValues(t *testing.T, serverURL, model string) json.RawMessage {
	t.Helper()
	return testEndpointDirectoryValues(t, model, presentation.Endpoint{
		Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
		URL: "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/realtime",
	})
}

func dialDeploymentRelay(t *testing.T, target, greeting string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.Read(ctx)
	if err != nil || string(payload) != greeting {
		connection.CloseNow()
		t.Fatalf("deployment relay greeting = %q, %v; want %q", payload, err, greeting)
	}
	return connection
}

func echoDeploymentRelay(t *testing.T, connection *websocket.Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := connection.Read(ctx)
	if err != nil || string(echoed) != payload {
		t.Fatalf("deployment relay echo = %q, %v; want %q", echoed, err, payload)
	}
}
