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

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
	"github.com/coder/websocket"
)

func TestSessionInspectionPlaneReconcilesThroughLiveServerClosure(t *testing.T) {
	factories := completeServerFactories(t)
	inspectionProvider, err := serverplugin.NewSessionProviderFactory(inspectionServerProvider{})
	if err != nil {
		t.Fatal(err)
	}
	factories["sessions"] = inspectionProvider
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-inspection-v1", "build-1", "4")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(
			factory.Descriptor().Name, originalArtifact, factory,
		); err != nil {
			t.Fatal(err)
		}
	}
	replacementFactory, err := serverplugin.NewSessionInspectionPlaneFactory(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	const replacementImplementation = "test/server-session-inspection-v2"
	replacementArtifact := serverArtifact("go://openrealtime/test/server-inspection-v2", "build-2", "5")
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
	httpServer := httptest.NewServer(service.Handler())
	t.Cleanup(httpServer.Close)
	assertServerProviderBinding(t, httpServer.URL, "server-profile-test")
	predecessor := dialServerProviderSession(t, httpServer.URL)
	t.Cleanup(func() { _ = predecessor.CloseNow() })
	predecessorAccess := negotiateServerInspection(t, predecessor, "inspection-v1")
	if status := serverInspectionStatus(t, httpServer.URL, predecessorAccess); status != http.StatusOK {
		t.Fatalf("predecessor inspection status = %d", status)
	}

	before := realm.Live()
	receipt, err := realm.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "inspection", SetImplementation: true, Implementation: replacementImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	readContext, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
	for {
		if _, _, err := predecessor.Read(readContext); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				cancelRead()
				t.Fatal("session-inspection replacement left the predecessor connection active")
			}
			break
		}
	}
	cancelRead()
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence <= before.Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 5 || len(receipt.StateTransfers) != 0 {
		t.Fatalf("session-inspection reconciliation receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "inspection" ||
		transition.BeforeImplementation != factories["inspection"].Descriptor().Name ||
		transition.AfterImplementation != replacementImplementation ||
		transition.BeforeRuntime != originalArtifact || transition.AfterRuntime != replacementArtifact {
		t.Fatalf("session-inspection transition = %#v", transition)
	}
	wantRetired := map[string]bool{
		"inspection": true, "gateway": true, "realtime": true,
		"observability": true, "session-api": true,
	}
	for _, retirement := range receipt.Retirements {
		if !wantRetired[retirement.Entry] || retirement.RetiredScopes == 0 ||
			retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
			retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
			retirement.RemainingServices != 0 {
			t.Fatalf("session-inspection retirement = %#v", retirement)
		}
		delete(wantRetired, retirement.Entry)
	}
	if len(wantRetired) != 0 {
		t.Fatalf("session-inspection retirement omitted %v", wantRetired)
	}
	after := realm.Live()
	if after.Sequence != receipt.AfterSequence ||
		after.Entries["inspection"].Implementation != replacementImplementation ||
		after.Entries["inspection"].Runtime != replacementArtifact ||
		after.Entries["gateway"].Runtime != originalArtifact ||
		after.Entries["realtime"].Runtime != originalArtifact ||
		after.Entries["observability"].Runtime != originalArtifact ||
		after.Entries["session-api"].Runtime != originalArtifact ||
		after.Entries["http-router"].Runtime != originalArtifact ||
		after.Entries["sessions"].Runtime != originalArtifact {
		t.Fatalf("replacement session-inspection live evidence = %+v", after)
	}
	afterValue, afterContract, afterProvider, afterRevision, err := realm.Export(
		serverplugin.RealtimeHTTPExport,
	)
	if err != nil || afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable server export changed across inspection replacement: %T/%+v/%s/%d, %v",
			afterValue, afterContract, afterProvider, afterRevision, err)
	}
	assertServerProviderBinding(t, httpServer.URL, "server-profile-test")
	if status := serverInspectionStatus(t, httpServer.URL, predecessorAccess); status != http.StatusNotFound {
		t.Fatalf("retired inspection capability status = %d", status)
	}
	assertHTTPStatus(t,
		httpServer.URL+management.APIPrefix+"/sessions/sess_missing/live",
		http.StatusNotFound,
	)
	replacement := dialServerProviderSession(t, httpServer.URL)
	t.Cleanup(func() { _ = replacement.CloseNow() })
	replacementAccess := negotiateServerInspection(t, replacement, "inspection-v2")
	if replacementAccess.Token == predecessorAccess.Token {
		t.Fatal("replacement inspection plane reused predecessor authority")
	}
	if status := serverInspectionStatus(t, httpServer.URL, replacementAccess); status != http.StatusOK {
		t.Fatalf("replacement inspection status = %d", status)
	}
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

type inspectionServerProvider struct{ serverTestProvider }

func (inspectionServerProvider) Start(
	ctx context.Context, options binding.Options,
) (binding.Runtime, error) {
	runtime, err := (serverTestProvider{}).Start(ctx, options)
	if err != nil {
		return nil, err
	}
	return inspectionServerRuntime{Runtime: runtime}, nil
}

type inspectionServerRuntime struct{ binding.Runtime }

func (inspectionServerRuntime) Live() inspect.Live {
	return inspect.Live{
		FormatVersion: inspect.LiveFormatVersion,
		GraphID:       "server-inspection-test",
		GraphRevision: 1,
		Fingerprint:   "sha256:" + strings.Repeat("6", 64),
		Configuration: &inspect.ArtifactIdentity{
			ID:       "values://openrealtime/server-inspection-test",
			Revision: "1",
			Digest:   "sha256:" + strings.Repeat("7", 64),
		},
		Sequence: 1,
		State:    "running",
		Nodes: map[string]inspect.NodeLive{
			"runtime": {
				State: "mounted",
				Resolution: &inspect.NodeResolution{
					Element: element.Identity{
						Name:     "test.ServerInspection",
						Revision: 1,
						Digest:   "sha256:" + strings.Repeat("8", 64),
					},
					Implementation: "test/server-inspection-runtime",
					Runtime: inspect.ArtifactIdentity{
						ID:       "go://openrealtime/test/server-inspection-runtime",
						Revision: "1",
						Digest:   "sha256:" + strings.Repeat("9", 64),
					},
					RuntimeEvidence: inspect.EvidenceRegistered,
				},
			},
		},
		Edges: map[string]inspect.EdgeLive{},
		Flows: map[string]inspect.FlowLive{},
	}
}

func negotiateServerInspection(
	t *testing.T, connection *websocket.Conn, eventID string,
) openrealtime.InspectionAccess {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload, err := json.Marshal(map[string]any{
		"type": "session.update", "event_id": eventID,
		"session": map[string]any{
			"type": "realtime",
			"openrealtime": map[string]any{
				"version": openrealtime.Version,
				"debug": map[string]any{
					"enabled": true, "categories": []string{"session"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write inspection negotiation: %v", err)
	}
	for {
		_, response, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read inspection negotiation: %v", err)
		}
		var event map[string]any
		if err := json.Unmarshal(response, &event); err != nil {
			t.Fatalf("decode inspection negotiation: %v", err)
		}
		if event["type"] == "error" {
			t.Fatalf("inspection negotiation failed: %s", response)
		}
		if event["type"] != "session.updated" {
			continue
		}
		session, ok := event["session"].(map[string]any)
		if !ok {
			t.Fatalf("inspection negotiation omitted session: %s", response)
		}
		extension, ok := session["openrealtime"].(map[string]any)
		if !ok {
			t.Fatalf("inspection negotiation omitted extension: %s", response)
		}
		debug, ok := extension["debug"].(map[string]any)
		if !ok || debug["inspection"] == nil {
			t.Fatalf("inspection negotiation omitted authority: %s", response)
		}
		encoded, err := json.Marshal(debug["inspection"])
		if err != nil {
			t.Fatal(err)
		}
		var access openrealtime.InspectionAccess
		if err := json.Unmarshal(encoded, &access); err != nil {
			t.Fatal(err)
		}
		if access.SessionID == "" || access.Token == "" || access.ExpiresAtMS <= time.Now().UnixMilli() ||
			access.Path != management.APIPrefix+"/sessions/"+access.SessionID+"/live" {
			t.Fatalf("inspection negotiation returned incomplete authority: %+v", access)
		}
		return access
	}
}

func serverInspectionStatus(
	t *testing.T, endpoint string, access openrealtime.InspectionAccess,
) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+access.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(management.CapabilityHeader, access.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

var _ binding.Binding = inspectionServerProvider{}
var _ interface{ Live() inspect.Live } = inspectionServerRuntime{}
