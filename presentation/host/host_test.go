package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/coder/websocket"
)

func TestComposedHostServesExactManifestAndModulesAndUnmountsCleanly(t *testing.T) {
	module, err := NewModuleStoreFactory(1, []ModuleSource{{
		Name: "shell.js", MediaType: "text/javascript", Content: []byte("export const ready = true;\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	asset := module.Descriptor().Assets[0]
	clientPlan := makeClientPlan(t, asset)
	manifest, err := presentation.FreezeManifest(presentation.ClientManifest{
		FormatVersion: presentation.ManifestFormatVersion, Platform: "browser", Plan: clientPlan,
		Implementations: []presentation.ManifestImplementation{{
			Entry: "shell", Implementation: "browser-shell",
			Artifact:   inspect.ArtifactIdentity{ID: "module://browser-shell", Digest: asset.Digest},
			Entrypoint: "shell.js",
		}},
		Assets: []presentation.ManifestAsset{{
			Entry: "shell", Name: asset.Name, MediaType: asset.MediaType, Digest: asset.Digest,
			Path: "/client/v1/modules/" + strings.TrimPrefix(asset.Digest, "sha256:"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestFactory, err := NewManifestFactory(manifest)
	if err != nil {
		t.Fatal(err)
	}
	routerFactory := NewRouterFactory()
	hostPlan := makeHostPlan(t, []pluginruntime.Factory{manifestFactory, module, routerFactory})
	registry := pluginruntime.NewRegistry()
	for _, factory := range []pluginruntime.Factory{manifestFactory, module, routerFactory} {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: hostPlan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/client/v1/manifest")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("ETag") != `"`+manifest.Fingerprint+`"` {
		t.Fatalf("manifest response status=%d headers=%v body=%s", response.StatusCode, response.Header, payload)
	}
	gotManifest, err := presentation.ParseManifest(payload)
	if err != nil || gotManifest.Fingerprint != manifest.Fingerprint {
		t.Fatalf("served manifest = %#v, %v", gotManifest, err)
	}
	moduleResponse, err := http.Get(server.URL + manifest.Assets[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	modulePayload, _ := io.ReadAll(moduleResponse.Body)
	moduleResponse.Body.Close()
	if moduleResponse.StatusCode != http.StatusOK || string(modulePayload) != "export const ready = true;\n" ||
		moduleResponse.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("module response status=%d headers=%v body=%q",
			moduleResponse.StatusCode, moduleResponse.Header, modulePayload)
	}

	if err := mounted.Unmount(context.Background(), "modules"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, server.URL+"/client/v1/manifest", http.StatusNotFound)
	assertStatus(t, server.URL+manifest.Assets[0].Path, http.StatusNotFound)
	if live := mounted.Live(); live.Entries["router"].State != "active" ||
		live.Entries["modules"].State != "inactive" || live.Entries["manifest"].State != "inactive" {
		t.Fatalf("host state after module loss = %#v", live.Entries)
	}
	if err := mounted.Activate(context.Background(), "modules"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, server.URL+"/client/v1/manifest", http.StatusOK)
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, server.URL+"/client/v1/manifest", http.StatusNotFound)
}

func TestStableHostRouterSurvivesRouteReplacementRollbackAndRemoval(t *testing.T) {
	routerFactory := NewRouterFactory()
	stableFactory := &stableTestRouteFactory{}
	originalFactory := &testRouteFactory{}
	descriptor := originalFactory.Descriptor()
	validReplacement := &replacementTestRouteFactory{
		descriptor: descriptor,
		routes: []Route{
			{Pattern: "GET /healthz", Handler: statusHandler(218)},
			{Pattern: "GET /readyz", Handler: statusHandler(219)},
		},
	}
	conflictingReplacement := &replacementTestRouteFactory{
		descriptor: descriptor,
		routes:     []Route{{Pattern: "GET /stable", Handler: statusHandler(http.StatusTeapot)}},
	}
	plan := makeHostPlan(t, []pluginruntime.Factory{
		routerFactory, stableFactory, originalFactory,
	})
	registry := pluginruntime.NewRegistry()
	originalArtifact := hostTestArtifact("go://openrealtime/presentation/host-routes", "build-1", "a")
	for _, factory := range []pluginruntime.Factory{
		routerFactory, stableFactory, originalFactory,
	} {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	validArtifact := hostTestArtifact(
		"go://openrealtime/presentation/host-routes-v2", "build-2", "b",
	)
	if err := registry.RegisterArtifact(
		"test/host-route-v2", validArtifact, validReplacement,
	); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterArtifact(
		"test/host-route-conflict",
		hostTestArtifact("go://openrealtime/presentation/host-routes-conflict", "build-2", "c"),
		conflictingReplacement,
	); err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	value, contract, provider, revision, err := mounted.Export("http")
	if err != nil || contract != presentation.HTTPHandlerContract || provider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	handler, ok := value.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", value)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	assertStatus(t, server.URL+"/healthz", http.StatusNoContent)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)

	if err := mounted.Replace(
		context.Background(), "test_route", "test/host-route-conflict",
	); err == nil || !strings.Contains(err.Error(), "previous implementation was restored") ||
		!strings.Contains(err.Error(), `HTTP route "GET /stable" is already owned by stable_route`) {
		t.Fatalf("conflicting host-route replacement error = %v", err)
	}
	assertStatus(t, server.URL+"/healthz", http.StatusNoContent)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)
	if live := mounted.Live(); live.Entries["test_route"].Implementation != descriptor.Name ||
		live.Entries["test_route"].Runtime != originalArtifact {
		t.Fatalf("failed host replacement was not rolled back: %+v", live.Entries["test_route"])
	}

	if err := mounted.Replace(
		context.Background(), "test_route", "test/host-route-v2",
	); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, server.URL+"/healthz", 218)
	assertStatus(t, server.URL+"/readyz", 219)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)
	afterValue, afterContract, afterProvider, afterRevision, err := mounted.Export("http")
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable host export changed across route replacement: before=%T/%+v/%s/%d after=%T/%+v/%s/%d error=%v",
			value, contract, provider, revision,
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	if live := mounted.Live(); live.Entries["test_route"].Implementation != "test/host-route-v2" ||
		live.Entries["test_route"].Runtime != validArtifact ||
		live.Entries["router"].Runtime != originalArtifact {
		t.Fatalf("host replacement live evidence = %+v", live)
	}

	if err := mounted.Unmount(context.Background(), "test_route"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, server.URL+"/healthz", http.StatusNotFound)
	assertStatus(t, server.URL+"/readyz", http.StatusNotFound)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
	assertStatus(t, server.URL+"/stable", http.StatusNotFound)
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatalf("idempotent host close: %v", err)
	}
}

func TestHostReconcilesMultipleRouteCapabilitiesAtomically(t *testing.T) {
	routerFactory := NewRouterFactory()
	stableFactory := &stableTestRouteFactory{}
	routeFactory := &testRouteFactory{}
	stableDescriptor := stableFactory.Descriptor()
	routeDescriptor := routeFactory.Descriptor()
	validStable := &replacementTestRouteFactory{
		descriptor: stableDescriptor,
		routes:     []Route{{Pattern: "GET /stable", Handler: statusHandler(220)}},
	}
	validRoute := &replacementTestRouteFactory{
		descriptor: routeDescriptor,
		routes: []Route{
			{Pattern: "GET /healthz", Handler: statusHandler(218)},
			{Pattern: "GET /readyz", Handler: statusHandler(219)},
		},
	}
	conflictingStable := &replacementTestRouteFactory{
		descriptor: stableDescriptor,
		routes:     []Route{{Pattern: "GET /candidate-shared", Handler: statusHandler(221)}},
	}
	conflictingRoute := &replacementTestRouteFactory{
		descriptor: routeDescriptor,
		routes:     []Route{{Pattern: "GET /candidate-shared", Handler: statusHandler(222)}},
	}
	plan := makeHostPlan(t, []pluginruntime.Factory{routerFactory, stableFactory, routeFactory})
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}{
		{routerFactory.Descriptor().Name, hostTestArtifact("go://host-router", "build-1", "1"), routerFactory},
		{stableDescriptor.Name, hostTestArtifact("go://host-stable-v1", "build-1", "2"), stableFactory},
		{routeDescriptor.Name, hostTestArtifact("go://host-route-v1", "build-1", "3"), routeFactory},
		{"test/host-stable-v2", hostTestArtifact("go://host-stable-v2", "build-2", "4"), validStable},
		{"test/host-route-v2-multi", hostTestArtifact("go://host-route-v2-multi", "build-2", "5"), validRoute},
		{"test/host-stable-conflict", hostTestArtifact("go://host-stable-conflict", "build-2", "6"), conflictingStable},
		{"test/host-route-conflict-multi", hostTestArtifact("go://host-route-conflict-multi", "build-2", "7"), conflictingRoute},
	} {
		if err := registry.RegisterArtifact(row.implementation, row.artifact, row.factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	value, contract, provider, revision, err := mounted.Export("http")
	if err != nil || contract != presentation.HTTPHandlerContract || provider != "router" {
		t.Fatalf("host HTTP export = %T %+v %q %d, %v", value, contract, provider, revision, err)
	}
	handler, ok := value.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", value)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	assertStatus(t, server.URL+"/healthz", http.StatusNoContent)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)

	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{
			{Entry: "test_route", SetImplementation: true, Implementation: "test/host-route-conflict-multi"},
			{Entry: "stable_route", SetImplementation: true, Implementation: "test/host-stable-conflict"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "previous composition was restored") ||
		!strings.Contains(err.Error(), "candidate-shared") {
		t.Fatalf("multi-route conflict error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("failed multi-route reconciliation returned receipt %#v", receipt)
	}
	assertStatus(t, server.URL+"/healthz", http.StatusNoContent)
	assertStatus(t, server.URL+"/stable", http.StatusNoContent)
	assertStatus(t, server.URL+"/candidate-shared", http.StatusNotFound)
	afterFailure := mounted.Live()
	if afterFailure.Entries["stable_route"].Implementation != stableDescriptor.Name ||
		afterFailure.Entries["test_route"].Implementation != routeDescriptor.Name {
		t.Fatalf("multi-route rollback live evidence = %+v", afterFailure.Entries)
	}

	receipt, err = mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: afterFailure.Sequence,
		Updates: []pluginruntime.EntryUpdate{
			{Entry: "test_route", SetImplementation: true, Implementation: "test/host-route-v2-multi"},
			{Entry: "stable_route", SetImplementation: true, Implementation: "test/host-stable-v2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != afterFailure.Sequence ||
		receipt.AfterSequence <= afterFailure.Sequence ||
		receipt.AfterSequence != mounted.Live().Sequence || len(receipt.Transitions) != 2 ||
		receipt.Transitions[0].Entry != "stable_route" ||
		receipt.Transitions[1].Entry != "test_route" || len(receipt.Retirements) != 2 {
		t.Fatalf("multi-route reconciliation receipt = %#v", receipt)
	}
	for _, retirement := range receipt.Retirements {
		if retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("multi-route retirement retained ownership: %#v", retirement)
		}
	}
	assertStatus(t, server.URL+"/healthz", 218)
	assertStatus(t, server.URL+"/readyz", 219)
	assertStatus(t, server.URL+"/stable", 220)
	afterValue, afterContract, afterProvider, afterRevision, err := mounted.Export("http")
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable host export changed across multi-route reconciliation")
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
	assertStatus(t, server.URL+"/healthz", http.StatusNotFound)
	assertStatus(t, server.URL+"/stable", http.StatusNotFound)
}

func TestHostReconcilesStatefulRouteWithMigrationAndRollback(t *testing.T) {
	routerFactory := NewRouterFactory()
	descriptor := statefulTestRouteDescriptor()
	v1State := newStatefulTestRouteState(7)
	missingState := newStatefulTestRouteState(0)
	failingState := newStatefulTestRouteState(0)
	v2State := newStatefulTestRouteState(0)
	v1 := &statefulTestRouteFactory{descriptor: descriptor, label: "v1", state: v1State}
	missingMigrator := &statefulTestRouteFactory{
		descriptor: descriptor, label: "missing", state: missingState,
	}
	failing := &statefulTestRouteFactory{
		descriptor: descriptor, label: "failure", state: failingState,
		supportsMigration: true, migrationDelta: 100, fail: errors.New("intentional stateful route failure"),
	}
	v2 := &statefulTestRouteFactory{
		descriptor: descriptor, label: "v2", state: v2State,
		supportsMigration: true, migrationDelta: 1,
	}
	plan := makeHostPlan(t, []pluginruntime.Factory{routerFactory, v1})
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}{
		{routerFactory.Descriptor().Name, hostTestArtifact("go://host-router", "build-1", "1"), routerFactory},
		{descriptor.Name, hostTestArtifact("go://host-stateful-v1", "build-1", "2"), v1},
		{"test/host-stateful-no-migrator", hostTestArtifact("go://host-stateful-no-migrator", "build-2", "3"), missingMigrator},
		{"test/host-stateful-failure", hostTestArtifact("go://host-stateful-failure", "build-2", "4"), failing},
		{"test/host-stateful-v2", hostTestArtifact("go://host-stateful-v2", "build-2", "5"), v2},
	} {
		if err := registry.RegisterArtifact(row.implementation, row.artifact, row.factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	value, _, _, _, err := mounted.Export("http")
	if err != nil {
		t.Fatal(err)
	}
	handler, ok := value.(http.Handler)
	if !ok {
		t.Fatalf("host HTTP export value = %T", value)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	assertBody(t, server.URL+"/stateful", "v1:7")

	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "stateful_route", SetImplementation: true,
			Implementation: "test/host-stateful-no-migrator",
		}},
	})
	if !errors.Is(err, pluginruntime.ErrStateMigrationNeeded) ||
		!strings.Contains(err.Error(), "has no migrator") {
		t.Fatalf("missing host state migrator error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) ||
		mounted.Live().Sequence != before.Sequence || v1State.snapshotCalls() != 0 ||
		v1State.disposeCalls() != 0 || missingState.preMountDisposeCalls() != 1 {
		t.Fatalf("missing migrator crossed safe point: receipt=%#v live=%+v v1=%+v missing=%+v",
			receipt, mounted.Live(), v1State.counts(), missingState.counts())
	}
	assertBody(t, server.URL+"/stateful", "v1:7")

	receipt, err = mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: mounted.Live().Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "stateful_route", SetImplementation: true,
			Implementation: "test/host-stateful-failure",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "intentional stateful route failure") ||
		!strings.Contains(err.Error(), "previous composition was restored") {
		t.Fatalf("failed stateful host activation error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("failed stateful host activation returned receipt %#v", receipt)
	}
	assertBody(t, server.URL+"/stateful", "v1:7")
	if live := mounted.Live(); live.Entries["stateful_route"].Implementation != descriptor.Name ||
		v1State.snapshotCalls() != 1 || v1State.disposeCalls() != 1 ||
		v1State.restoreCalls() != 1 || failingState.migrationCalls() != 1 ||
		failingState.mountCalls() != 1 || failingState.disposeCalls() != 1 ||
		failingState.preMountDisposeCalls() != 1 {
		t.Fatalf("stateful host rollback evidence live=%+v v1=%+v candidate=%+v",
			live, v1State.counts(), failingState.counts())
	}

	beforeSuccess := mounted.Live()
	receipt, err = mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: beforeSuccess.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "stateful_route", SetImplementation: true, Implementation: "test/host-stateful-v2",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBody(t, server.URL+"/stateful", "v2:8")
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.BeforeSequence != beforeSuccess.Sequence || receipt.AfterSequence != mounted.Live().Sequence ||
		len(receipt.Transitions) != 1 || receipt.Transitions[0].Entry != "stateful_route" ||
		len(receipt.StateTransfers) != 1 || receipt.StateTransfers[0].Entry != "stateful_route" ||
		receipt.StateTransfers[0].Schema != *descriptor.StateSchema ||
		receipt.StateTransfers[0].BeforeStateDigest == receipt.StateTransfers[0].AfterStateDigest ||
		receipt.StateTransfers[0].MigratorImplementation != "test/host-stateful-v2" {
		t.Fatalf("stateful host receipt = %#v", receipt)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || strings.Contains(string(encoded), "counter") || strings.Contains(string(encoded), "v1:7") {
		t.Fatalf("stateful host receipt exposed payload: %s, %v", encoded, err)
	}
	if v1State.snapshotCalls() != 2 || v1State.disposeCalls() != 2 ||
		v2State.migrationCalls() != 1 || v2State.mountCalls() != 1 || v2State.restoreCalls() != 1 {
		t.Fatalf("stateful host success counts v1=%+v v2=%+v", v1State.counts(), v2State.counts())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, mounted.Live(), plan.Fingerprint)
	assertStatus(t, server.URL+"/stateful", http.StatusNotFound)
	if v2State.disposeCalls() != 1 || v2State.preMountDisposeCalls() != 1 {
		t.Fatalf("stateful host final disposal counts = %+v", v2State.counts())
	}
}

func TestRouterRegistrationIsAtomicAndConcurrentRequestsSeeCompleteMuxes(t *testing.T) {
	router := newRouter()
	dispose, err := router.Register("first", []Route{{
		Pattern: "GET /one", Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.Register("duplicate", []Route{{
		Pattern: "GET /one", Handler: http.NotFoundHandler(),
	}}); err == nil || !strings.Contains(err.Error(), "already owned") {
		t.Fatalf("duplicate registration error = %v", err)
	}

	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				request := httptest.NewRequest(http.MethodGet, "/one", nil)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if status := response.Code; status != http.StatusNoContent && status != http.StatusNotFound {
					t.Errorf("partial mux status = %d", status)
					return
				}
			}
		}()
	}
	dispose()
	dispose()
	wait.Wait()
}

func TestCredentialHoldingRelaysUseOnlyPublicEndpoints(t *testing.T) {
	const token = "relay-secret"
	websocketSeen := make(chan string, 1)
	sdpSeen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/realtime":
			websocketSeen <- request.Header.Get("Authorization") + "\x00" + request.URL.Query().Get("model")
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			kind, payload, err := connection.Read(request.Context())
			if err == nil {
				_ = connection.Write(request.Context(), kind, payload)
			}
		case "/v1/realtime/calls":
			body, _ := io.ReadAll(request.Body)
			sdpSeen <- request.Header.Get("Authorization") + "\x00" +
				request.URL.Query().Get("model") + "\x00" + string(body)
			writer.Header().Set("Content-Type", "application/sdp")
			_, _ = writer.Write([]byte("answer-sdp"))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()
	websocketURL := "ws" + strings.TrimPrefix(backend.URL, "http") + "/v1/realtime"

	routerFactory := NewRouterFactory()
	targetFactory := NewEndpointDirectoryFactory()
	credentialFactory, err := NewBearerCredentialFactory(token)
	if err != nil {
		t.Fatal(err)
	}
	websocketRelay := NewWebSocketRelayFactory(nil)
	webrtcRelay := NewWebRTCRelayFactory(nil, nil)
	plan := makeHostPlan(t, []pluginruntime.Factory{
		websocketRelay, webrtcRelay, targetFactory, credentialFactory, routerFactory,
	})
	registry := pluginruntime.NewRegistry()
	for _, factory := range []pluginruntime.Factory{
		websocketRelay, webrtcRelay, targetFactory, credentialFactory, routerFactory,
	} {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values := testEndpointDirectoryValues(t, "fixture-model",
		presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: websocketURL,
		},
		presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: backend.URL + "/v1/realtime/calls",
		},
	)
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"target": values},
		Permissions: map[string][]plugin.Permission{
			"credential": {{
				Kind: credentialPermissionKind, Resource: credentialPermissionResource,
				Operations: []string{credentialPermissionOperation},
			}},
			"websocket": {relayPermission(websocketOperation)},
			"webrtc":    {relayPermission(httpOperation)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	hostServer := httptest.NewServer(handler)
	defer hostServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(hostServer.URL, "http")+"/client/v1/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"fixture"}`)); err != nil {
		t.Fatal(err)
	}
	_, echoed, err := client.Read(ctx)
	if err != nil || string(echoed) != `{"type":"fixture"}` {
		t.Fatalf("relay echo = %q, %v", echoed, err)
	}
	_ = client.Close(websocket.StatusNormalClosure, "done")
	select {
	case seen := <-websocketSeen:
		if seen != "Bearer "+token+"\x00fixture-model" {
			t.Fatalf("WebSocket backend observed %q", seen)
		}
	case <-ctx.Done():
		t.Fatal("WebSocket backend did not observe relay")
	}

	response, err := http.Post(hostServer.URL+"/client/v1/realtime/calls",
		"application/sdp", strings.NewReader("offer-sdp"))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(answer) != "answer-sdp" {
		t.Fatalf("WebRTC relay status=%d body=%q", response.StatusCode, answer)
	}
	select {
	case seen := <-sdpSeen:
		if seen != "Bearer "+token+"\x00fixture-model\x00offer-sdp" {
			t.Fatalf("WebRTC backend observed %q", seen)
		}
	case <-ctx.Done():
		t.Fatal("WebRTC backend did not observe relay")
	}

	if err := mounted.Unmount(context.Background(), "credential"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, hostServer.URL+"/client/v1/realtime", http.StatusNotFound)
	assertStatus(t, hostServer.URL+"/client/v1/realtime/calls", http.StatusNotFound)
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWebRTCRelayRefusesCredentialBearingRedirect(t *testing.T) {
	var captured atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/captured" {
			captured.Store(true)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(writer, request, "/captured", http.StatusTemporaryRedirect)
	}))
	defer backend.Close()

	factory := NewWebRTCRelayFactory(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/client/v1/realtime/calls", strings.NewReader("offer-sdp"))
	response := httptest.NewRecorder()
	factory.relayWebRTC(relayTarget{
		WebRTC: backend.URL + "/offer", DialTimeout: time.Second,
	}, staticCredential("Bearer must-not-cross-redirect"), response, request)
	if response.Code != http.StatusTemporaryRedirect || captured.Load() {
		t.Fatalf("WebRTC redirect response=%d followed=%t", response.Code, captured.Load())
	}
}

func TestRelaysFailClosedWithoutDeploymentPermission(t *testing.T) {
	routerFactory := NewRouterFactory()
	targetFactory := NewEndpointDirectoryFactory()
	credentialFactory := NewAnonymousCredentialFactory()
	relayFactory := NewWebSocketRelayFactory(nil)
	plan := makeHostPlan(t, []pluginruntime.Factory{
		routerFactory, targetFactory, credentialFactory, relayFactory,
	})
	registry := pluginruntime.NewRegistry()
	for _, factory := range []pluginruntime.Factory{
		routerFactory, targetFactory, credentialFactory, relayFactory,
	} {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values := testEndpointDirectoryValues(t, "", presentation.Endpoint{
		Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
		URL: "ws://127.0.0.1:1/v1/realtime",
	})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, Values: map[string]json.RawMessage{"target": values},
	}); err == nil || !strings.Contains(err.Error(), "network-connect grant") {
		t.Fatalf("missing relay permission error = %v", err)
	}
}

func TestLoopbackListenerIsAPluginAndStopsWithItsScope(t *testing.T) {
	routerFactory := NewRouterFactory()
	routeFactory := &testRouteFactory{}
	listenerFactory := NewLoopbackListenerFactory()
	plan := makeHostPlan(t, []pluginruntime.Factory{listenerFactory, routeFactory, routerFactory})
	registry := pluginruntime.NewRegistry()
	for _, factory := range []pluginruntime.Factory{listenerFactory, routeFactory, routerFactory} {
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
	}
	values, _ := json.Marshal(listenerConfig{Address: "127.0.0.1:0"})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"listener": values},
		Permissions: map[string][]plugin.Permission{"listener": {{
			Kind: listenPermissionKind, Resource: listenPermissionResource,
			Operations: []string{listenPermissionOperation},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, contract, _, _, err := mounted.Export("listener")
	if err != nil || contract != presentation.ListenerContract {
		t.Fatalf("listener export = %#v, %#v, %v", value, contract, err)
	}
	info, ok := value.(ListenerInfo)
	if !ok || info.URL == "" {
		t.Fatalf("listener info = %#v", value)
	}
	response, err := http.Get(info.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("listener health status = %d", response.StatusCode)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(requestContext, http.MethodGet, info.URL+"/healthz", nil)
	if _, err := http.DefaultClient.Do(request); err == nil {
		t.Fatal("closed listener still accepted a request")
	}

	badValues, _ := json.Marshal(listenerConfig{Address: "0.0.0.0:0"})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"listener": badValues},
		Permissions: map[string][]plugin.Permission{"listener": {{
			Kind: listenPermissionKind, Resource: listenPermissionResource,
			Operations: []string{listenPermissionOperation},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "not loopback") {
		t.Fatalf("non-loopback listener error = %v", err)
	}
}

type testRouteFactory struct{}

func (*testRouteFactory) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.test-route", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
	}
}

func (*testRouteFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	return registerRoutes(mount, []Route{{
		Pattern: "GET /healthz", Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
	}})
}

type stableTestRouteFactory struct{}

func (*stableTestRouteFactory) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.test-stable-route", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
	}
}

func (*stableTestRouteFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	return registerRoutes(mount, []Route{{
		Pattern: "GET /stable", Handler: statusHandler(http.StatusNoContent),
	}})
}

type replacementTestRouteFactory struct {
	descriptor plugin.Descriptor
	routes     []Route
}

func (factory *replacementTestRouteFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *replacementTestRouteFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return registerRoutes(mount, factory.routes)
}

func (factory *replacementTestRouteFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return replacementTestRouteCandidate{factory: factory}, nil
}

type replacementTestRouteCandidate struct{ factory *replacementTestRouteFactory }

func (candidate replacementTestRouteCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

type statefulTestRouteFactory struct {
	descriptor        plugin.Descriptor
	label             string
	state             *statefulTestRouteState
	supportsMigration bool
	migrationDelta    int
	fail              error
}

func statefulTestRouteDescriptor() plugin.Descriptor {
	schema := plugin.Contract{
		Name: "presentation.host.test_stateful_route.state", Revision: 1,
		Digest: "sha256:" + strings.Repeat("8", 64),
	}
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.test-stateful-route", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Requires:    []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		StateSchema: &schema, Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true},
	}
}

func (factory *statefulTestRouteFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *statefulTestRouteFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	restored, available, err := mount.State.Restored()
	if err != nil {
		return err
	}
	if available {
		if err := factory.state.restore(restored); err != nil {
			return err
		}
	}
	if err := mount.State.Snapshot(func(context.Context) (json.RawMessage, error) {
		return factory.state.snapshot()
	}); err != nil {
		return err
	}
	factory.state.noteMount()
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /stateful",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, factory.label+":"+factory.state.counterString())
		}),
	}}); err != nil {
		return err
	}
	if err := mount.Lifecycle.Defer("stateful-test-route", func(context.Context) error {
		factory.state.noteDispose()
		return nil
	}); err != nil {
		return err
	}
	return factory.fail
}

func (factory *statefulTestRouteFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if err := candidate.Lifecycle.Defer("stateful-test-route-candidate", func(context.Context) error {
		factory.state.notePreMountDispose()
		return nil
	}); err != nil {
		return nil, err
	}
	if factory.supportsMigration {
		return statefulTestRouteMigratingCandidate{factory: factory}, nil
	}
	return statefulTestRouteCandidate{factory: factory}, nil
}

type statefulTestRouteCandidate struct{ factory *statefulTestRouteFactory }

func (candidate statefulTestRouteCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

type statefulTestRouteMigratingCandidate struct{ factory *statefulTestRouteFactory }

func (candidate statefulTestRouteMigratingCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

func (candidate statefulTestRouteMigratingCandidate) MigrateState(
	_ context.Context, migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	return candidate.factory.state.migrate(migration, candidate.factory.migrationDelta)
}

type statefulTestRouteState struct {
	mu sync.Mutex

	counter          int
	mounts           int
	snapshots        int
	restores         int
	migrations       int
	disposals        int
	preMountDisposes int
}

func newStatefulTestRouteState(counter int) *statefulTestRouteState {
	return &statefulTestRouteState{counter: counter}
}

func (state *statefulTestRouteState) snapshot() (json.RawMessage, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.snapshots++
	return json.Marshal(struct {
		Counter int `json:"counter"`
	}{Counter: state.counter})
}

func (state *statefulTestRouteState) restore(raw json.RawMessage) error {
	var snapshot struct {
		Counter int `json:"counter"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}
	state.mu.Lock()
	state.counter = snapshot.Counter
	state.restores++
	state.mu.Unlock()
	return nil
}

func (state *statefulTestRouteState) migrate(
	migration pluginruntime.StateMigration, delta int,
) (json.RawMessage, error) {
	state.mu.Lock()
	state.migrations++
	state.mu.Unlock()
	var snapshot struct {
		Counter int `json:"counter"`
	}
	if err := json.Unmarshal(migration.Snapshot, &snapshot); err != nil {
		return nil, err
	}
	snapshot.Counter += delta
	return json.Marshal(snapshot)
}

func (state *statefulTestRouteState) noteMount() {
	state.mu.Lock()
	state.mounts++
	state.mu.Unlock()
}

func (state *statefulTestRouteState) noteDispose() {
	state.mu.Lock()
	state.disposals++
	state.mu.Unlock()
}

func (state *statefulTestRouteState) notePreMountDispose() {
	state.mu.Lock()
	state.preMountDisposes++
	state.mu.Unlock()
}

func (state *statefulTestRouteState) counterString() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return strconv.Itoa(state.counter)
}

func (state *statefulTestRouteState) counts() statefulTestRouteCounts {
	state.mu.Lock()
	defer state.mu.Unlock()
	return statefulTestRouteCounts{
		mounts: state.mounts, snapshots: state.snapshots, restores: state.restores,
		migrations: state.migrations, disposals: state.disposals,
		preMountDisposes: state.preMountDisposes,
	}
}

func (state *statefulTestRouteState) mountCalls() int     { return state.counts().mounts }
func (state *statefulTestRouteState) snapshotCalls() int  { return state.counts().snapshots }
func (state *statefulTestRouteState) restoreCalls() int   { return state.counts().restores }
func (state *statefulTestRouteState) migrationCalls() int { return state.counts().migrations }
func (state *statefulTestRouteState) disposeCalls() int   { return state.counts().disposals }
func (state *statefulTestRouteState) preMountDisposeCalls() int {
	return state.counts().preMountDisposes
}

type statefulTestRouteCounts struct {
	mounts           int
	snapshots        int
	restores         int
	migrations       int
	disposals        int
	preMountDisposes int
}

func statusHandler(status int) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(status)
	})
}

func hostTestArtifact(id, revision, digestDigit string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{
		ID: id, Revision: revision, Digest: "sha256:" + strings.Repeat(digestDigit, 64),
	}
}

func assertHostRealmClosed(t *testing.T, live pluginruntime.Live, fingerprint string) {
	t.Helper()
	if live.State != "closed" || live.Realm != plugin.PresentationHostRealm ||
		live.Fingerprint != fingerprint || len(live.Entries) == 0 {
		t.Fatalf("closed host realm evidence = %+v", live)
	}
	for id, entry := range live.Entries {
		if entry.State != "closed" || entry.Workers != 0 || entry.Effects != 0 ||
			len(entry.Services) != 0 || entry.Error != "" {
			t.Fatalf("closed host entry %s retained ownership: %+v", id, entry)
		}
	}
	for name, exported := range live.Exports {
		if exported.Available {
			t.Fatalf("closed host export %s remained available: %+v", name, exported)
		}
	}
}

func makeClientPlan(t *testing.T, asset plugin.Asset) plugin.Plan {
	t.Helper()
	descriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.client.shell", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"browser"}, Assets: []plugin.Asset{asset},
	}
	catalog := plugin.NewCatalog()
	if _, err := catalog.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.test", Revision: 1, Realm: plugin.ClientRealm,
		Scopes:  []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{{ID: "shell", Plugin: descriptor.Name, Scope: "root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func makeHostPlan(t *testing.T, factories []pluginruntime.Factory) plugin.Plan {
	t.Helper()
	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	for _, factory := range factories {
		descriptor := factory.Descriptor()
		if _, err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
		id := ""
		switch descriptor.Name {
		case "openrealtime.presentation.host.router":
			id = "router"
		case "openrealtime.presentation.host.module-store":
			id = "modules"
		case "openrealtime.presentation.host.client-manifest":
			id = "manifest"
		case "openrealtime.presentation.host.endpoint-directory":
			id = "target"
		case "openrealtime.presentation.host.secret-credential",
			"openrealtime.presentation.host.anonymous-credential":
			id = "credential"
		case "openrealtime.presentation.host.websocket-relay":
			id = "websocket"
		case "openrealtime.presentation.host.webrtc-relay":
			id = "webrtc"
		case "openrealtime.presentation.host.management-relay":
			id = "management"
		case "openrealtime.presentation.host.loopback-listener":
			id = "listener"
		case "openrealtime.presentation.host.test-route":
			id = "test_route"
		case "openrealtime.presentation.host.test-stable-route":
			id = "stable_route"
		case "openrealtime.presentation.host.test-stateful-route":
			id = "stateful_route"
		default:
			t.Fatalf("unknown test factory %s", descriptor.Name)
		}
		entries = append(entries, plugin.ProfileEntry{ID: id, Plugin: descriptor.Name, Scope: "root"})
	}
	exports := []plugin.ProfileExport{{
		Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name,
	}}
	for _, entry := range entries {
		if entry.ID == "listener" {
			exports = append(exports, plugin.ProfileExport{
				Name: "listener", Provider: "listener", Service: presentation.ListenerContract.Name,
			})
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "host.test", Revision: 1, Realm: plugin.PresentationHostRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
		Exports: exports,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func assertStatus(t *testing.T, target string, want int) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", target, response.StatusCode, want)
	}
}

func assertBody(t *testing.T, target, want string) {
	t.Helper()
	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(payload) != want {
		t.Fatalf("GET %s status=%d body=%q, want status=200 body=%q, error=%v",
			target, response.StatusCode, payload, want, readErr)
	}
}
