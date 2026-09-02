package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestRealtimeAndObservabilityRoutesReconcileWithoutDroppingSession(t *testing.T) {
	factories := completeServerFactories(t)
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-routes-v1", "build-1", "a")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	const realtimeImplementation = "test/server-realtime-route-v2"
	const observabilityImplementation = "test/server-observability-route-v2"
	realtimeArtifact := serverArtifact("go://openrealtime/test/realtime-route-v2", "build-2", "b")
	observabilityArtifact := serverArtifact("go://openrealtime/test/observability-route-v2", "build-2", "c")
	if err := registry.RegisterArtifact(
		realtimeImplementation, realtimeArtifact, serverplugin.NewRealtimeRouteFactory(),
	); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterArtifact(
		observabilityImplementation, observabilityArtifact, serverplugin.NewObservabilityRouteFactory(),
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
	value, contract, provider, revision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil || contract != serverplugin.RealtimeHTTPContract() || provider != "http-router" {
		t.Fatalf("server HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok {
		t.Fatalf("server HTTP export value = %T", value)
	}
	httpServer := httptest.NewServer(service.Handler())
	defer httpServer.Close()
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	assertHTTPStatus(t, httpServer.URL+"/metrics", http.StatusOK)

	dialContext, cancelDial := context.WithTimeout(context.Background(), 2*time.Second)
	connection, _, err := websocket.Dial(
		dialContext,
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime?model=route-reconcile",
		nil,
	)
	cancelDial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.CloseNow() })
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	_, payload, err := connection.Read(readContext)
	cancelRead()
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	if err := json.Unmarshal(payload, &created); err != nil || created["type"] != "session.created" {
		t.Fatalf("initial realtime event = %s, %v", payload, err)
	}
	updateRealtimeSession(t, connection, "before_reconcile")

	before := realm.Live()
	receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{
			{Entry: "realtime", SetImplementation: true, Implementation: realtimeImplementation},
			{Entry: "observability", SetImplementation: true, Implementation: observabilityImplementation},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 2 ||
		len(receipt.Retirements) != 2 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("server route reconciliation receipt = %#v", receipt)
	}
	transitions := make(map[string]pluginruntime.EntryTransition, len(receipt.Transitions))
	for _, transition := range receipt.Transitions {
		transitions[transition.Entry] = transition
	}
	if transition := transitions["realtime"]; transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != realtimeArtifact ||
		transition.AfterImplementation != realtimeImplementation {
		t.Fatalf("realtime route transition = %#v", transition)
	}
	if transition := transitions["observability"]; transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != observabilityArtifact ||
		transition.AfterImplementation != observabilityImplementation {
		t.Fatalf("observability route transition = %#v", transition)
	}
	retired := make(map[string]bool, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retired[retirement.Entry] = true
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("server route retirement retained ownership = %#v", retirement)
		}
	}
	if !retired["realtime"] || !retired["observability"] {
		t.Fatalf("server route retirements = %#v", receipt.Retirements)
	}

	after := realm.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["realtime"].Implementation != realtimeImplementation ||
		after.Entries["realtime"].Runtime != realtimeArtifact ||
		after.Entries["observability"].Implementation != observabilityImplementation ||
		after.Entries["observability"].Runtime != observabilityArtifact ||
		after.Entries["gateway"].Runtime != originalArtifact ||
		after.Entries["http-router"].Runtime != originalArtifact {
		t.Fatalf("reconciled server route live evidence = %+v", after)
	}
	afterValue, afterContract, afterProvider, afterRevision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable server HTTP export changed across route reconciliation: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	assertHTTPStatus(t, httpServer.URL+"/metrics", http.StatusOK)
	updateRealtimeSession(t, connection, "after_reconcile")

	if err := connection.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	closeContext, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	if err := realm.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	closed := realm.Live()
	if closed.State != "closed" || closed.Fingerprint != plan.Fingerprint ||
		closed.Exports[serverplugin.RealtimeHTTPExport].Available {
		t.Fatalf("closed server realm evidence = %+v", closed)
	}
	for id, entry := range closed.Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed server entry %s retained ownership = %+v", id, entry)
		}
	}
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusNotFound)
}

func updateRealtimeSession(t *testing.T, connection *websocket.Conn, eventID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]any{
		"type": "session.update", "event_id": eventID,
		"session": map[string]any{"type": "realtime"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write realtime session update: %v", err)
	}
	for {
		_, response, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read realtime session update: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(response, &event); err != nil {
			t.Fatalf("decode realtime session update event: %v", err)
		}
		if event["type"] == "error" {
			t.Fatalf("realtime session update failed: %s", response)
		}
		if event["type"] == "session.updated" {
			return
		}
	}
}

var _ pluginruntime.Factory = serverplugin.NewRealtimeRouteFactory()
var _ pluginruntime.CandidatePreMounter = serverplugin.NewRealtimeRouteFactory()
var _ pluginruntime.Factory = serverplugin.NewObservabilityRouteFactory()
var _ pluginruntime.CandidatePreMounter = serverplugin.NewObservabilityRouteFactory()
