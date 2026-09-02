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

	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestSessionProviderReconcilesThroughLiveServerClosure(t *testing.T) {
	providerV1 := &namedServerProvider{name: "server-provider-v1"}
	providerV2 := &namedServerProvider{name: "server-provider-v2"}
	providerV1Factory, err := serverplugin.NewSessionProviderFactory(providerV1)
	if err != nil {
		t.Fatal(err)
	}
	providerV2Factory, err := serverplugin.NewSessionProviderFactory(providerV2)
	if err != nil {
		t.Fatal(err)
	}
	factories := completeServerFactories(t)
	factories["sessions"] = providerV1Factory
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-provider-v1", "build-1", "1")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	const candidateImplementation = "test/server-session-provider-v2"
	candidateArtifact := serverArtifact("go://openrealtime/test/server-provider-v2", "build-2", "2")
	if err := registry.RegisterArtifact(
		candidateImplementation, candidateArtifact, providerV2Factory,
	); err != nil {
		t.Fatal(err)
	}
	driftedProvider := &namedServerProvider{name: "server-provider-drift-candidate"}
	driftedFactory, err := serverplugin.NewSessionProviderFactory(driftedProvider)
	if err != nil {
		t.Fatal(err)
	}
	const driftedImplementation = "test/server-session-provider-drifted"
	if err := registry.RegisterArtifact(
		driftedImplementation,
		serverArtifact("go://openrealtime/test/server-provider-drifted", "build-2", "3"),
		driftedFactory,
	); err != nil {
		t.Fatal(err)
	}
	driftedProvider.name = " server-provider-drift-candidate "

	realm, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realm.Close(context.Background()) })
	value, contract, provider, revision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil || contract != serverplugin.RealtimeHTTPContract() || provider != "http-router" {
		t.Fatalf("server HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok {
		t.Fatalf("server HTTP export value = %T", value)
	}
	httpServer := httptest.NewServer(service.Handler())
	t.Cleanup(httpServer.Close)
	assertServerProviderBinding(t, httpServer.URL, providerV1.name)
	predecessor := dialServerProviderSession(t, httpServer.URL)
	t.Cleanup(func() { _ = predecessor.CloseNow() })
	updateRealtimeSession(t, predecessor, "provider-v1-before-refusal")

	before := realm.Live()
	if receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "sessions", SetImplementation: true, Implementation: driftedImplementation,
		}},
	}); err == nil || !strings.Contains(err.Error(), "invalid name") || receipt.FormatVersion != 0 {
		t.Fatalf("drifted provider candidate receipt/error = %#v, %v", receipt, err)
	}
	afterRefusal := realm.Live()
	if afterRefusal.Sequence != before.Sequence ||
		afterRefusal.Entries["sessions"].Implementation != providerV1Factory.Descriptor().Name ||
		afterRefusal.Entries["sessions"].Runtime != originalArtifact {
		t.Fatalf("drifted provider candidate disturbed predecessor = %+v", afterRefusal)
	}
	assertServerProviderBinding(t, httpServer.URL, providerV1.name)
	updateRealtimeSession(t, predecessor, "provider-v1-after-refusal")

	receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        afterRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "sessions", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertServerSessionClosed(t, predecessor, "session-provider replacement")
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 4 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("session-provider reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "sessions" ||
		transition.BeforeImplementation != providerV1Factory.Descriptor().Name ||
		transition.AfterImplementation != candidateImplementation ||
		transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != candidateArtifact {
		t.Fatalf("session-provider transition = %#v", transition)
	}
	wantRetired := map[string]bool{
		"sessions": true, "gateway": true, "realtime": true, "observability": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("session-provider retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("session-provider retirement omitted %v", wantRetired)
	}
	after := realm.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["sessions"].Implementation != candidateImplementation ||
		after.Entries["sessions"].Runtime != candidateArtifact ||
		after.Entries["gateway"].Runtime != originalArtifact ||
		after.Entries["realtime"].Runtime != originalArtifact ||
		after.Entries["observability"].Runtime != originalArtifact ||
		after.Entries["http-router"].Runtime != originalArtifact ||
		after.Entries["inspection"].Runtime != originalArtifact ||
		after.Entries["session-api"].Runtime != originalArtifact {
		t.Fatalf("replacement session-provider live evidence = %+v", after)
	}
	afterValue, afterContract, afterProvider, afterRevision, err := realm.Export(
		serverplugin.RealtimeHTTPExport,
	)
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable server export changed across provider replacement: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	assertServerProviderBinding(t, httpServer.URL, providerV2.name)
	replacement := dialServerProviderSession(t, httpServer.URL)
	t.Cleanup(func() { _ = replacement.CloseNow() })
	updateRealtimeSession(t, replacement, "provider-v2")
	if err := replacement.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}

	if err := realm.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := realm.Live()
	for id, entry := range closed.Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed server entry %s retained ownership = %+v", id, entry)
		}
	}
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusNotFound)
}

type namedServerProvider struct {
	serverTestProvider
	name string
}

func (provider *namedServerProvider) Name() string { return provider.name }

func dialServerProviderSession(t *testing.T, endpoint string) *websocket.Conn {
	t.Helper()
	dialContext, cancelDial := context.WithTimeout(context.Background(), 2*time.Second)
	connection, _, err := websocket.Dial(
		dialContext,
		"ws"+strings.TrimPrefix(endpoint, "http")+"/v1/realtime?model=provider-reconcile",
		nil,
	)
	cancelDial()
	if err != nil {
		t.Fatal(err)
	}
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	_, payload, err := connection.Read(readContext)
	cancelRead()
	if err != nil {
		_ = connection.CloseNow()
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil || event["type"] != "session.created" {
		_ = connection.CloseNow()
		t.Fatalf("initial realtime event = %s, %v", payload, err)
	}
	return connection
}

func assertServerSessionClosed(t *testing.T, connection *websocket.Conn, operation string) {
	t.Helper()
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelRead()
	for {
		if _, _, err := connection.Read(readContext); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s left the predecessor connection active", operation)
			}
			return
		}
	}
}

func assertServerProviderBinding(t *testing.T, endpoint, want string) {
	t.Helper()
	response, err := http.Get(endpoint + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || health["binding"] != want {
		t.Fatalf("server provider health = status %d payload %+v, want binding %q",
			response.StatusCode, health, want)
	}
}

var _ serverplugin.SessionProvider = (*namedServerProvider)(nil)
