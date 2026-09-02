package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestGatewayReconcilesThroughLiveServerRouteClosure(t *testing.T) {
	provider := &namedServerProvider{name: "gateway-provider-v1"}
	providerFactory, err := serverplugin.NewSessionProviderFactory(provider)
	if err != nil {
		t.Fatal(err)
	}
	factories := completeServerFactories(t)
	factories["sessions"] = providerFactory
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-gateway-v1", "build-1", "a")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	replacementFactory, err := serverplugin.NewGatewayFactory(serverplugin.GatewayFactoryConfig{
		Gateway: gateway.Config{Model: "gateway-v2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	const replacementImplementation = "test/server-gateway-v2"
	replacementArtifact := serverArtifact("go://openrealtime/test/server-gateway-v2", "build-2", "b")
	if err := registry.RegisterArtifact(
		replacementImplementation, replacementArtifact, replacementFactory,
	); err != nil {
		t.Fatal(err)
	}

	realm, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = realm.Close(context.Background()) })
	value, contract, exportProvider, revision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil || contract != serverplugin.RealtimeHTTPContract() || exportProvider != "http-router" {
		t.Fatalf("server HTTP export = %T %+v %q %d, %v",
			value, contract, exportProvider, revision, err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok {
		t.Fatalf("server HTTP export value = %T", value)
	}
	httpServer := httptest.NewServer(service.Handler())
	t.Cleanup(httpServer.Close)
	assertServerProviderBinding(t, httpServer.URL, provider.name)
	predecessor, predecessorModel := dialGatewayModelSession(t, httpServer.URL)
	t.Cleanup(func() { _ = predecessor.CloseNow() })
	if predecessorModel != "openrealtime" {
		t.Fatalf("predecessor gateway model = %q", predecessorModel)
	}
	updateRealtimeSession(t, predecessor, "gateway-v1-before-refusal")

	before := realm.Live()
	provider.name = " gateway-provider-v1 "
	refused, refusalErr := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "gateway", SetImplementation: true, Implementation: replacementImplementation,
		}},
	})
	provider.name = "gateway-provider-v1"
	if refusalErr == nil || !strings.Contains(refusalErr.Error(), "invalid name") ||
		refused.FormatVersion != 0 {
		t.Fatalf("drifted gateway dependency receipt/error = %#v, %v", refused, refusalErr)
	}
	afterRefusal := realm.Live()
	if afterRefusal.Sequence != before.Sequence ||
		afterRefusal.Entries["gateway"].Implementation != factories["gateway"].Descriptor().Name ||
		afterRefusal.Entries["gateway"].Runtime != originalArtifact {
		t.Fatalf("gateway candidate refusal disturbed predecessor = %+v", afterRefusal)
	}
	assertServerProviderBinding(t, httpServer.URL, provider.name)
	updateRealtimeSession(t, predecessor, "gateway-v1-after-refusal")

	receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        afterRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "gateway", SetImplementation: true, Implementation: replacementImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertServerSessionClosed(t, predecessor, "gateway replacement")
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 3 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("gateway reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "gateway" ||
		transition.BeforeImplementation != factories["gateway"].Descriptor().Name ||
		transition.AfterImplementation != replacementImplementation ||
		transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != replacementArtifact {
		t.Fatalf("gateway transition = %#v", transition)
	}
	wantRetired := map[string]bool{
		"gateway": true, "realtime": true, "observability": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("gateway retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("gateway retirement omitted %v", wantRetired)
	}
	after := realm.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["gateway"].Implementation != replacementImplementation ||
		after.Entries["gateway"].Runtime != replacementArtifact ||
		after.Entries["realtime"].Runtime != originalArtifact ||
		after.Entries["observability"].Runtime != originalArtifact ||
		after.Entries["http-router"].Runtime != originalArtifact ||
		after.Entries["sessions"].Runtime != originalArtifact ||
		after.Entries["inspection"].Runtime != originalArtifact ||
		after.Entries["session-api"].Runtime != originalArtifact {
		t.Fatalf("replacement gateway live evidence = %+v", after)
	}
	afterValue, afterContract, afterProvider, afterRevision, err := realm.Export(
		serverplugin.RealtimeHTTPExport,
	)
	if err != nil || afterValue != value || afterContract != contract ||
		afterProvider != exportProvider || afterRevision != revision {
		t.Fatalf("stable server export changed across gateway replacement: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	assertServerProviderBinding(t, httpServer.URL, provider.name)
	assertHTTPStatus(t,
		httpServer.URL+management.APIPrefix+"/sessions/sess_missing/live",
		http.StatusNotFound,
	)
	replacement, replacementModel := dialGatewayModelSession(t, httpServer.URL)
	t.Cleanup(func() { _ = replacement.CloseNow() })
	if replacementModel != "gateway-v2" {
		t.Fatalf("replacement gateway model = %q", replacementModel)
	}
	updateRealtimeSession(t, replacement, "gateway-v2")
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

func dialGatewayModelSession(t *testing.T, endpoint string) (*websocket.Conn, string) {
	t.Helper()
	dialContext, cancelDial := context.WithTimeout(context.Background(), 2*time.Second)
	connection, _, err := websocket.Dial(
		dialContext,
		"ws"+strings.TrimPrefix(endpoint, "http")+"/v1/realtime",
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
		t.Fatalf("initial gateway event = %s, %v", payload, err)
	}
	session, ok := event["session"].(map[string]any)
	model, modelOK := session["model"].(string)
	if !ok || !modelOK || model == "" {
		_ = connection.CloseNow()
		t.Fatalf("initial gateway session = %s", payload)
	}
	return connection, model
}
