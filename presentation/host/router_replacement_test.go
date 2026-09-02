package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestRouterReplacementRollsBackAndRemountsListenerClosure(t *testing.T) {
	reservation := reserveLoopbackAddress(t)
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}

	routerV1 := NewRouterFactory()
	routerV2 := NewRouterFactory()
	route := &testRouteFactory{}
	listener := NewLoopbackListenerFactory()
	factories := []pluginruntime.Factory{listener, route, routerV1}
	plan := makeHostPlan(t, factories)
	registry := pluginruntime.NewRegistry()
	originalArtifacts := map[string]inspect.ArtifactIdentity{
		"listener":   hostTestArtifact("go://host-listener", "build-1", "1"),
		"test_route": hostTestArtifact("go://host-test-route", "build-1", "2"),
		"router":     hostTestArtifact("go://host-router-v1", "build-1", "3"),
	}
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"listener", listener}, {"test_route", route}, {"router", routerV1},
	} {
		if err := registry.RegisterArtifact(
			row.factory.Descriptor().Name, originalArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	const failingImplementation = "openrealtime.presentation.host.router-failing"
	failing := &failingHostRouterFactory{
		delegate: NewRouterFactory(), failure: errors.New("fixture router activation failed"),
	}
	if err := registry.RegisterArtifact(
		failingImplementation, hostTestArtifact("go://host-router-failing", "build-2", "4"), failing,
	); err != nil {
		t.Fatal(err)
	}
	const candidateImplementation = "openrealtime.presentation.host.router-v2"
	candidateArtifact := hostTestArtifact("go://host-router-v2", "build-2", "5")
	if err := registry.RegisterArtifact(candidateImplementation, candidateArtifact, routerV2); err != nil {
		t.Fatal(err)
	}

	listenerValues, err := json.Marshal(listenerConfig{
		Address: address, ReadHeaderTimeoutMS: 500, ShutdownTimeoutMS: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"listener": listenerValues},
		Permissions: map[string][]plugin.Permission{"listener": {{
			Kind: listenPermissionKind, Resource: listenPermissionResource,
			Operations: []string{listenPermissionOperation},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	oldHTTPValue, httpContract, httpProvider, oldHTTPRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v",
			oldHTTPValue, httpContract, httpProvider, oldHTTPRevision, err)
	}
	oldHandler, ok := oldHTTPValue.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", oldHTTPValue)
	}
	oldListenerValue, listenerContract, listenerProvider, oldListenerRevision, err := mounted.Export("listener")
	if err != nil || listenerContract != presentation.ListenerContract || listenerProvider != "listener" {
		t.Fatalf("host listener export = %T %+v %q %d, %v",
			oldListenerValue, listenerContract, listenerProvider, oldListenerRevision, err)
	}
	oldInfo, ok := oldListenerValue.(ListenerInfo)
	if !ok || oldInfo.Address != address {
		t.Fatalf("initial listener info = %#v", oldListenerValue)
	}
	assertStatus(t, oldInfo.URL+"/healthz", http.StatusNoContent)

	before := mounted.Live()
	failedReceipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "router", SetImplementation: true, Implementation: failingImplementation,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), failing.failure.Error()) || failedReceipt.FormatVersion != 0 {
		t.Fatalf("failing router candidate receipt/error = %#v, %v", failedReceipt, err)
	}
	afterRollback := mounted.Live()
	if afterRollback.Sequence <= before.Sequence ||
		afterRollback.Entries["router"].Implementation != routerV1.Descriptor().Name ||
		afterRollback.Entries["router"].Runtime != originalArtifacts["router"] ||
		afterRollback.Entries["listener"].Runtime != originalArtifacts["listener"] ||
		afterRollback.Entries["test_route"].Runtime != originalArtifacts["test_route"] {
		t.Fatalf("router rollback did not restore the predecessor closure = %+v", afterRollback)
	}
	rollbackHTTPValue, rollbackHTTPContract, rollbackHTTPProvider, rollbackHTTPRevision, err := mounted.Export("http")
	if err != nil || rollbackHTTPContract != httpContract || rollbackHTTPProvider != httpProvider ||
		rollbackHTTPRevision <= oldHTTPRevision || rollbackHTTPValue == oldHTTPValue {
		t.Fatalf("rollback HTTP export = %T/%+v/%s/%d, %v",
			rollbackHTTPValue, rollbackHTTPContract, rollbackHTTPProvider, rollbackHTTPRevision, err)
	}
	rollbackHandler := rollbackHTTPValue.(http.Handler)
	rollbackListenerValue, _, _, rollbackListenerRevision, err := mounted.Export("listener")
	rollbackInfo, rollbackInfoOK := rollbackListenerValue.(ListenerInfo)
	if err != nil || !rollbackInfoOK || rollbackInfo.URL != oldInfo.URL ||
		rollbackListenerRevision <= oldListenerRevision {
		t.Fatalf("rollback listener export = %#v/%d, %v",
			rollbackListenerValue, rollbackListenerRevision, err)
	}
	assertHandlerStatus(t, oldHandler, "/healthz", http.StatusNotFound)
	assertStatus(t, rollbackInfo.URL+"/healthz", http.StatusNoContent)

	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: afterRollback.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "router", SetImplementation: true, Implementation: candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != afterRollback.Sequence ||
		receipt.AfterSequence <= afterRollback.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 3 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("router replacement receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "router" || transition.BeforeRuntime != originalArtifacts["router"] ||
		transition.AfterRuntime != candidateArtifact || transition.AfterImplementation != candidateImplementation {
		t.Fatalf("router replacement transition = %#v", transition)
	}
	retired := make(map[string]bool, len(receipt.Retirements))
	for _, retirement := range receipt.Retirements {
		retired[retirement.Entry] = true
		if retirement.RetiredScopes == 0 || retirement.ClosedScopes != retirement.RetiredScopes ||
			retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
			retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
			t.Fatalf("router closure retirement retained ownership = %#v", retirement)
		}
	}
	for _, entry := range []string{"router", "test_route", "listener"} {
		if !retired[entry] {
			t.Fatalf("router replacement did not retire %s: %#v", entry, receipt.Retirements)
		}
	}

	after := mounted.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["router"].Implementation != candidateImplementation ||
		after.Entries["router"].Runtime != candidateArtifact ||
		after.Entries["listener"].Runtime != originalArtifacts["listener"] ||
		after.Entries["test_route"].Runtime != originalArtifacts["test_route"] ||
		after.Entries["listener"].Workers != 2 {
		t.Fatalf("replacement router live evidence = %+v", after)
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPContract != httpContract || afterHTTPProvider != httpProvider ||
		afterHTTPRevision <= rollbackHTTPRevision || afterHTTPValue == rollbackHTTPValue {
		t.Fatalf("replacement HTTP export = %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterListenerValue, _, _, afterListenerRevision, err := mounted.Export("listener")
	afterInfo, afterInfoOK := afterListenerValue.(ListenerInfo)
	if err != nil || !afterInfoOK || afterInfo.URL != oldInfo.URL ||
		afterListenerRevision <= rollbackListenerRevision {
		t.Fatalf("replacement listener export = %#v/%d, %v",
			afterListenerValue, afterListenerRevision, err)
	}
	assertHandlerStatus(t, rollbackHandler, "/healthz", http.StatusNotFound)
	assertStatus(t, afterInfo.URL+"/healthz", http.StatusNoContent)

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHandlerStatus(t, afterHTTPValue.(http.Handler), "/healthz", http.StatusNotFound)
	assertListenerClosed(t, afterInfo.URL+"/healthz")
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
}

type failingHostRouterFactory struct {
	delegate *RouterFactory
	failure  error
}

func (factory *failingHostRouterFactory) Descriptor() plugin.Descriptor {
	return factory.delegate.Descriptor()
}

func (factory *failingHostRouterFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return factory.delegate.Mount(ctx, mount)
}

func (factory *failingHostRouterFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return failingHostRouterCandidate{failure: factory.failure}, nil
}

type failingHostRouterCandidate struct{ failure error }

func (candidate failingHostRouterCandidate) Activate(
	context.Context, pluginruntime.MountContext,
) error {
	return candidate.failure
}

func assertHandlerStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("handler GET %s status = %d, want %d", path, response.Code, want)
	}
}
