package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestHTTPRouterReconcilesThroughCompleteServerClosure(t *testing.T) {
	factories := completeServerFactories(t)
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-router-v1", "build-1", "c")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	replacementFactory := serverplugin.NewHTTPRouterFactory()
	const replacementImplementation = "test/server-http-router-v2"
	replacementArtifact := serverArtifact("go://openrealtime/test/server-router-v2", "build-2", "d")
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
	value, contract, provider, revision, err := realm.Export(serverplugin.RealtimeHTTPExport)
	if err != nil || contract != serverplugin.RealtimeHTTPContract() || provider != "http-router" {
		t.Fatalf("server HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok {
		t.Fatalf("server HTTP export value = %T", value)
	}
	predecessorServer := httptest.NewServer(service.Handler())
	t.Cleanup(predecessorServer.Close)
	assertServerProviderBinding(t, predecessorServer.URL, "server-profile-test")
	predecessor := dialServerProviderSession(t, predecessorServer.URL)
	t.Cleanup(func() { _ = predecessor.CloseNow() })
	updateRealtimeSession(t, predecessor, "router-v1")

	before := realm.Live()
	receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "http-router", SetImplementation: true, Implementation: replacementImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertServerSessionClosed(t, predecessor, "server HTTP-router replacement")
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 5 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("server HTTP-router reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "http-router" ||
		transition.BeforeImplementation != factories["http-router"].Descriptor().Name ||
		transition.AfterImplementation != replacementImplementation ||
		transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != replacementArtifact {
		t.Fatalf("server HTTP-router transition = %#v", transition)
	}
	wantRetired := map[string]bool{
		"http-router": true, "gateway": true, "realtime": true,
		"observability": true, "session-api": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("server HTTP-router retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("server HTTP-router retirement omitted %v", wantRetired)
	}
	after := realm.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["http-router"].Implementation != replacementImplementation ||
		after.Entries["http-router"].Runtime != replacementArtifact ||
		after.Entries["gateway"].Runtime != originalArtifact ||
		after.Entries["realtime"].Runtime != originalArtifact ||
		after.Entries["observability"].Runtime != originalArtifact ||
		after.Entries["session-api"].Runtime != originalArtifact ||
		after.Entries["sessions"].Runtime != originalArtifact ||
		after.Entries["inspection"].Runtime != originalArtifact {
		t.Fatalf("replacement server HTTP-router live evidence = %+v", after)
	}
	afterValue, afterContract, afterProvider, afterRevision, err := realm.Export(
		serverplugin.RealtimeHTTPExport,
	)
	if err != nil || afterValue == value || afterContract != contract || afterProvider != provider ||
		afterRevision <= revision {
		t.Fatalf("server HTTP export did not advance across router replacement: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	assertHTTPStatus(t, predecessorServer.URL+"/healthz", http.StatusNotFound)
	afterService, ok := afterValue.(serverplugin.RealtimeHTTP)
	if !ok {
		t.Fatalf("replacement server HTTP export value = %T", afterValue)
	}
	replacementServer := httptest.NewServer(afterService.Handler())
	t.Cleanup(replacementServer.Close)
	assertServerProviderBinding(t, replacementServer.URL, "server-profile-test")
	assertHTTPStatus(t,
		replacementServer.URL+management.APIPrefix+"/sessions/sess_missing/live",
		http.StatusNotFound,
	)
	replacement := dialServerProviderSession(t, replacementServer.URL)
	t.Cleanup(func() { _ = replacement.CloseNow() })
	updateRealtimeSession(t, replacement, "router-v2")
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
	assertHTTPStatus(t, replacementServer.URL+"/healthz", http.StatusNotFound)
}

var _ pluginruntime.CandidatePreMounter = serverplugin.NewHTTPRouterFactory()
