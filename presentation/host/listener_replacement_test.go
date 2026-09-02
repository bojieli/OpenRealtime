package host

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestLoopbackListenerReplacementIsAtomic(t *testing.T) {
	oldReservation := reserveLoopbackAddress(t)
	oldAddress := oldReservation.Addr().String()
	newReservation := reserveLoopbackAddress(t)
	newAddress := newReservation.Addr().String()
	t.Cleanup(func() {
		_ = oldReservation.Close()
		_ = newReservation.Close()
	})
	if err := oldReservation.Close(); err != nil {
		t.Fatal(err)
	}

	listenerV1 := NewLoopbackListenerFactory()
	listenerV2 := NewLoopbackListenerFactory()
	route := &testRouteFactory{}
	router := NewRouterFactory()
	plan := makeHostPlan(t, []pluginruntime.Factory{listenerV1, route, router})
	registry := pluginruntime.NewRegistry()
	originalArtifacts := map[string]inspect.ArtifactIdentity{
		"listener": hostTestArtifact("go://host-listener-v1", "build-1", "1"),
		"route":    hostTestArtifact("go://host-test-route", "build-1", "2"),
		"router":   hostTestArtifact("go://host-router", "build-1", "3"),
	}
	for _, row := range []struct {
		entry   string
		factory pluginruntime.Factory
	}{
		{"listener", listenerV1}, {"route", route}, {"router", router},
	} {
		if err := registry.RegisterArtifact(
			row.factory.Descriptor().Name, originalArtifacts[row.entry], row.factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	const candidateImplementation = "openrealtime.presentation.host.loopback-listener-v2"
	candidateArtifact := hostTestArtifact("go://host-listener-v2", "build-2", "4")
	if err := registry.RegisterArtifact(candidateImplementation, candidateArtifact, listenerV2); err != nil {
		t.Fatal(err)
	}

	oldConfig, err := json.Marshal(listenerConfig{
		Address: oldAddress, ReadHeaderTimeoutMS: 500, ShutdownTimeoutMS: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := plugin.Permission{
		Kind: listenPermissionKind, Resource: listenPermissionResource,
		Operations: []string{listenPermissionOperation},
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values:      map[string]json.RawMessage{"listener": oldConfig},
		Permissions: map[string][]plugin.Permission{"listener": {grant}},
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
	oldValue, listenerContract, listenerProvider, oldRevision, err := mounted.Export("listener")
	if err != nil || listenerContract != presentation.ListenerContract || listenerProvider != "listener" {
		t.Fatalf("host listener export = %T %+v %q %d, %v",
			oldValue, listenerContract, listenerProvider, oldRevision, err)
	}
	oldInfo, ok := oldValue.(ListenerInfo)
	if !ok || oldInfo.Address != oldAddress || oldInfo.URL != "http://"+oldAddress {
		t.Fatalf("initial listener info = %#v", oldValue)
	}
	assertStatus(t, oldInfo.URL+"/healthz", http.StatusNoContent)

	before := mounted.Live()
	if receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "listener", SetPermissions: true, Permissions: nil,
		}},
	}); err == nil || !strings.Contains(err.Error(), "loopback-listen grant") || receipt.FormatVersion != 0 {
		t.Fatalf("permissionless listener candidate receipt/error = %#v, %v", receipt, err)
	}
	afterRefusal := mounted.Live()
	if afterRefusal.Sequence != before.Sequence ||
		afterRefusal.Entries["listener"].Implementation != listenerV1.Descriptor().Name ||
		afterRefusal.Entries["listener"].Runtime != originalArtifacts["listener"] {
		t.Fatalf("refused listener candidate disturbed predecessor = %+v", afterRefusal)
	}
	afterRefusalValue, _, _, afterRefusalRevision, err := mounted.Export("listener")
	if err != nil || afterRefusalValue != oldValue || afterRefusalRevision != oldRevision {
		t.Fatalf("listener export changed after refusal: %#v/%d, %v",
			afterRefusalValue, afterRefusalRevision, err)
	}
	assertStatus(t, oldInfo.URL+"/healthz", http.StatusNoContent)

	newConfig, err := json.Marshal(listenerConfig{
		Address: newAddress, ReadHeaderTimeoutMS: 500, ShutdownTimeoutMS: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := newReservation.Close(); err != nil {
		t.Fatal(err)
	}
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: afterRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "listener", SetImplementation: true, Implementation: candidateImplementation,
			SetConfig: true, Config: newConfig,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 1 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("listener replacement receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "listener" || transition.BeforeRuntime != originalArtifacts["listener"] ||
		transition.AfterRuntime != candidateArtifact || transition.AfterImplementation != candidateImplementation ||
		transition.BeforeConfigDigest == transition.AfterConfigDigest {
		t.Fatalf("listener replacement transition = %#v", transition)
	}
	retirement := receipt.Retirements[0]
	if retirement.Entry != "listener" || retirement.RetiredScopes == 0 ||
		retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
		retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
		retirement.RemainingServices != 0 {
		t.Fatalf("listener retirement retained ownership = %#v", retirement)
	}

	after := mounted.Live()
	listenerLive := after.Entries["listener"]
	if after.Sequence != receipt.AfterSequence || listenerLive.Implementation != candidateImplementation ||
		listenerLive.Runtime != candidateArtifact || listenerLive.Workers != 2 ||
		listenerLive.Effects != 2 || len(listenerLive.Services) != 1 {
		t.Fatalf("replacement listener live evidence = %+v", after)
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err := mounted.Export("http")
	if err != nil || afterHTTPValue != httpValue || afterHTTPContract != httpContract ||
		afterHTTPProvider != httpProvider || afterHTTPRevision != httpRevision {
		t.Fatalf("stable host export changed across listener replacement: %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	newValue, newContract, newProvider, newRevision, err := mounted.Export("listener")
	if err != nil || newContract != listenerContract || newProvider != listenerProvider ||
		newRevision <= oldRevision {
		t.Fatalf("replacement listener export = %T %+v %q %d, %v",
			newValue, newContract, newProvider, newRevision, err)
	}
	newInfo, ok := newValue.(ListenerInfo)
	if !ok || newInfo.Address != newAddress || newInfo.URL != "http://"+newAddress {
		t.Fatalf("replacement listener info = %#v", newValue)
	}
	assertStatus(t, newInfo.URL+"/healthz", http.StatusNoContent)
	assertListenerClosed(t, oldInfo.URL+"/healthz")

	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
}

func reserveLoopbackAddress(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func assertListenerClosed(t *testing.T, target string) {
	t.Helper()
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatalf("retired listener still served status %d", response.StatusCode)
	}
}
