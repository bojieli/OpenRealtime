package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestArtifactStoreReplacementMigratesRetainedContentAtSafePoint(t *testing.T) {
	host := mountArtifactReplacementHost(t, json.RawMessage(
		`{"max_entries":2,"max_item_bytes":4096,"max_total_bytes":8192}`,
	))
	server := httptest.NewServer(host.handler)
	t.Cleanup(server.Close)

	const (
		retainedID    = "retained_card_9Q"
		retainedTitle = "Private migration title 9Q"
		retainedHTML  = "<main>private migration payload 9Q</main>"
	)
	if _, err := host.store.Publish(context.Background(), ArtifactInput{
		ID: retainedID, Title: "Initial title", HTML: "<p>initial</p>",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.store.Publish(context.Background(), ArtifactInput{
		ID: "evicted_card_9Q", Title: "Evicted title", HTML: "<p>evicted</p>",
	}); err != nil {
		t.Fatal(err)
	}
	retained, err := host.store.Publish(context.Background(), ArtifactInput{
		ID: retainedID, Title: retainedTitle, HTML: retainedHTML,
	})
	if err != nil {
		t.Fatal(err)
	}
	newest, err := host.store.Publish(context.Background(), ArtifactInput{
		ID: "newest_card_9Q", Title: "Newest title", HTML: "<p>newest</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retained.Version != 2 || newest.Version != 1 {
		t.Fatalf("predecessor versions: retained=%d newest=%d", retained.Version, newest.Version)
	}
	wantList := host.store.List()
	wantStats := host.store.Stats()
	if len(wantList) != 2 || wantList[0].ID != retainedID || wantList[1].ID != newest.ID ||
		wantStats.Evictions != 1 {
		t.Fatalf("predecessor retention state: list=%#v stats=%#v", wantList, wantStats)
	}
	assertArtifactResource(t, server.URL, retained, retainedHTML)
	assertArtifactResource(t, server.URL, newest, "<p>newest</p>")

	beforeRefusal := host.mounted.Live()
	refused, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        beforeRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "artifacts", SetPermissions: true, Permissions: []plugin.Permission{},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "memory-publish grant") {
		t.Fatalf("permissionless artifact candidate error = %v", err)
	}
	if !reflect.DeepEqual(refused, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("permissionless artifact candidate returned receipt %#v", refused)
	}
	afterRefusal := host.mounted.Live()
	if afterRefusal.Sequence != beforeRefusal.Sequence ||
		!reflect.DeepEqual(afterRefusal.Entries["artifacts"], beforeRefusal.Entries["artifacts"]) ||
		host.store.Stats().Closed {
		t.Fatalf("permission refusal crossed safe point: before=%+v after=%+v stats=%#v",
			beforeRefusal, afterRefusal, host.store.Stats())
	}
	assertArtifactResource(t, server.URL, retained, retainedHTML)

	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "artifacts", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats := host.store.Stats(); !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("retired artifact store retained content: %#v", stats)
	}

	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err :=
		host.mounted.Export("http")
	if err != nil || afterHTTPValue != host.handler ||
		afterHTTPContract != presentation.HTTPHandlerContract || afterHTTPProvider != "router" ||
		afterHTTPRevision != host.httpRevision {
		t.Fatalf("stable HTTP export after artifact replacement = %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterStoreValue, afterStoreContract, afterStoreProvider, afterStoreRevision, err :=
		host.mounted.Export("artifacts")
	if err != nil || afterStoreContract != presentation.ArtifactStoreContract ||
		afterStoreProvider != "artifacts" || afterStoreRevision <= host.storeRevision {
		t.Fatalf("replacement artifact export = %T/%+v/%s/%d, %v",
			afterStoreValue, afterStoreContract, afterStoreProvider, afterStoreRevision, err)
	}
	replacement, ok := afterStoreValue.(ArtifactStore)
	if !ok || replacement == nil || replacement == host.store {
		t.Fatalf("replacement artifact service = %T (%p), predecessor=%p",
			afterStoreValue, afterStoreValue, host.store)
	}
	if got := replacement.Stats(); !reflect.DeepEqual(got, wantStats) {
		t.Fatalf("replacement artifact stats = %#v, want %#v", got, wantStats)
	}
	if got := replacement.List(); !reflect.DeepEqual(got, wantList) {
		t.Fatalf("replacement artifact order/metadata = %#v, want %#v", got, wantList)
	}
	assertArtifactResource(t, server.URL, retained, retainedHTML)
	assertArtifactResource(t, server.URL, newest, "<p>newest</p>")

	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != host.plan.Fingerprint ||
		receipt.BeforeSequence != before.Sequence || receipt.AfterSequence != host.mounted.Live().Sequence ||
		len(receipt.Transitions) != 1 || len(receipt.Retirements) != 1 ||
		len(receipt.StateTransfers) != 1 {
		t.Fatalf("artifact replacement receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "artifacts" ||
		transition.BeforeImplementation != host.originalImplementation ||
		transition.AfterImplementation != host.candidateImplementation ||
		transition.BeforeRuntime != host.originalArtifact ||
		transition.AfterRuntime != host.candidateArtifact {
		t.Fatalf("artifact replacement transition = %#v", transition)
	}
	retirement := receipt.Retirements[0]
	if retirement.Entry != "artifacts" || retirement.RetiredScopes == 0 ||
		retirement.ClosedScopes != retirement.RetiredScopes ||
		retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
		retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
		t.Fatalf("artifact replacement retirement = %#v", retirement)
	}
	transfer := receipt.StateTransfers[0]
	if transfer.Entry != "artifacts" || transfer.Schema != presentation.ArtifactStoreStateContract ||
		transfer.BeforeStateDigest == "" || transfer.BeforeStateDigest != transfer.AfterStateDigest ||
		transfer.MigratorImplementation != host.candidateImplementation {
		t.Fatalf("artifact replacement state transfer = %#v", transfer)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{retainedID, retainedTitle, retainedHTML, "evicted_card_9Q"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("artifact replacement receipt exposed %q: %s", secret, encoded)
		}
	}

	revised, err := replacement.Publish(context.Background(), ArtifactInput{
		ID: retainedID, Title: "Post-migration title", HTML: "<p>post migration</p>",
	})
	if err != nil || revised.Version != retained.Version+1 {
		t.Fatalf("post-migration revision = %#v, %v", revised, err)
	}
	if got := replacement.List(); len(got) != 2 || got[0].ID != newest.ID || got[1].ID != retainedID {
		t.Fatalf("post-migration retention order = %#v", got)
	}

	if err := host.mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, host.mounted.Live(), host.plan.Fingerprint)
	assertStatus(t, server.URL+retained.Path, http.StatusNotFound)
	if stats := replacement.Stats(); !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("replacement artifact store retained final content: %#v", stats)
	}
}

func TestArtifactStoreReplacementRefusesOversizedSnapshotBeforeTeardown(t *testing.T) {
	host := mountArtifactReplacementHost(t, json.RawMessage(
		`{"max_entries":2,"max_item_bytes":2097152,"max_total_bytes":4194304}`,
	))
	server := httptest.NewServer(host.handler)
	t.Cleanup(server.Close)

	content := strings.Repeat("s", pluginruntime.MaximumStateSnapshotBytes)
	artifact, err := host.store.Publish(context.Background(), ArtifactInput{
		ID: "oversized_state_7Z", Title: "Oversized migration state", HTML: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "artifacts", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot plugin state") ||
		!strings.Contains(err.Error(), "migration envelope") {
		t.Fatalf("oversized artifact snapshot error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("oversized artifact snapshot returned receipt %#v", receipt)
	}
	after := host.mounted.Live()
	if after.Sequence != before.Sequence ||
		!reflect.DeepEqual(after.Entries["artifacts"], before.Entries["artifacts"]) {
		t.Fatalf("oversized snapshot changed live identity: before=%+v after=%+v", before, after)
	}
	if stats := host.store.Stats(); stats.Closed || stats.Entries != 1 ||
		stats.Bytes != int64(len(content)) {
		t.Fatalf("oversized snapshot disturbed predecessor store: %#v", stats)
	}
	value, contract, provider, revision, err := host.mounted.Export("artifacts")
	if err != nil || value != host.store || contract != presentation.ArtifactStoreContract ||
		provider != "artifacts" || revision != host.storeRevision {
		t.Fatalf("artifact export after oversized refusal = %T/%+v/%s/%d, %v",
			value, contract, provider, revision, err)
	}
	response := getResource(t, &http.Client{}, server.URL+artifact.Path)
	if response.StatusCode != http.StatusOK || len(response.body) != len(content) ||
		contentDigest(response.body) != artifact.Digest {
		t.Fatalf("artifact after oversized refusal: status=%d bytes=%d digest=%s",
			response.StatusCode, len(response.body), contentDigest(response.body))
	}
}

func TestArtifactStoreStateMigrationRejectsMalformedOrIncompatibleState(t *testing.T) {
	limits := ResourceStoreLimits{MaxEntries: 2, MaxItemBytes: 64, MaxTotalBytes: 96}
	candidate := artifactStoreCandidate{entryID: "artifacts", limits: limits}
	valid := pluginruntime.StateMigration{
		EntryID: "artifacts", Schema: presentation.ArtifactStoreStateContract,
		SourceImplementation: "openrealtime.presentation.host.artifact-store",
		Snapshot: json.RawMessage(
			`{"entries":[{"html":"<p>safe</p>","id":"card","title":"Card","updated_at":"2026-09-02T00:00:00Z","version":1}],"evictions":0,"format_version":1}`,
		),
	}
	if migrated, err := candidate.MigrateState(context.Background(), valid); err != nil {
		t.Fatalf("valid artifact migration: %v", err)
	} else if state, decodeErr := decodeArtifactStoreState(migrated, limits); decodeErr != nil ||
		len(state.Entries) != 1 || state.Entries[0].ID != "card" {
		t.Fatalf("valid migrated artifact state = %#v, %v", state, decodeErr)
	}

	tests := []struct {
		name    string
		mutate  func(*pluginruntime.StateMigration)
		wantErr string
	}{
		{
			name: "wrong entry", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) { migration.EntryID = "downloads" },
		},
		{
			name: "wrong schema", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Schema = presentation.DownloadStoreContract
			},
		},
		{
			name: "duplicate field", wantErr: "duplicate",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"format_version":1,"evictions":0,"entries":[]}`,
				)
			},
		},
		{
			name: "unknown field", wantErr: "unknown field",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[],"payload":"secret"}`,
				)
			},
		},
		{
			name: "missing entry field", wantErr: "missing fields",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"card","title":"Card","html":"<p>x</p>","version":1}]}`,
				)
			},
		},
		{
			name: "duplicate id", wantErr: "repeats id",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"card","title":"Card","html":"<p>a</p>","version":1,"updated_at":"2026-09-02T00:00:00Z"},{"id":"card","title":"Card","html":"<p>b</p>","version":2,"updated_at":"2026-09-02T00:00:01Z"}]}`,
				)
			},
		},
		{
			name: "noncanonical timestamp", wantErr: "invalid timestamp",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"card","title":"Card","html":"<p>x</p>","version":1,"updated_at":"2026-09-02T01:00:00+01:00"}]}`,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			migration := valid
			test.mutate(&migration)
			migrated, err := candidate.MigrateState(context.Background(), migration)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || migrated != nil {
				t.Fatalf("malformed artifact migration = %s, %v; want %q",
					migrated, err, test.wantErr)
			}
		})
	}
}

type artifactReplacementHost struct {
	mounted                 *pluginruntime.Mounted
	plan                    plugin.Plan
	handler                 http.Handler
	store                   ArtifactStore
	httpRevision            uint64
	storeRevision           uint64
	originalImplementation  string
	candidateImplementation string
	originalArtifact        inspect.ArtifactIdentity
	candidateArtifact       inspect.ArtifactIdentity
}

func mountArtifactReplacementHost(
	t *testing.T, values json.RawMessage,
) artifactReplacementHost {
	t.Helper()
	router := NewRouterFactory()
	original := NewArtifactStoreFactory()
	candidate := NewArtifactStoreFactory()
	catalog := plugin.NewCatalog()
	for _, factory := range []pluginruntime.Factory{router, original} {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.artifact-replacement.test",
		Revision:      1,
		Realm:         plugin.PresentationHostRealm,
		Scopes:        []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{
			{ID: "router", Plugin: router.Descriptor().Name, Scope: "root"},
			{ID: "artifacts", Plugin: original.Descriptor().Name, Scope: "root"},
		},
		Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "artifacts", Provider: "artifacts", Service: presentation.ArtifactStoreContract.Name},
		},
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

	originalImplementation := original.Descriptor().Name
	const candidateImplementation = "openrealtime.presentation.host.artifact-store-v2"
	originalArtifact := hostTestArtifact("go://host-artifact-store-v1", "build-1", "7")
	candidateArtifact := hostTestArtifact("go://host-artifact-store-v2", "build-2", "8")
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}{
		{router.Descriptor().Name, hostTestArtifact("go://host-router", "build-1", "6"), router},
		{originalImplementation, originalArtifact, original},
		{candidateImplementation, candidateArtifact, candidate},
	} {
		if err := registry.RegisterArtifact(row.implementation, row.artifact, row.factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"artifacts": values},
		Permissions: map[string][]plugin.Permission{
			"artifacts": original.Descriptor().Permissions,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close artifact replacement host: %v", err)
		}
	})
	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("artifact replacement HTTP export = %T/%+v/%s/%d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("artifact replacement HTTP value = %T", httpValue)
	}
	storeValue, storeContract, storeProvider, storeRevision, err := mounted.Export("artifacts")
	if err != nil || storeContract != presentation.ArtifactStoreContract || storeProvider != "artifacts" {
		t.Fatalf("artifact replacement store export = %T/%+v/%s/%d, %v",
			storeValue, storeContract, storeProvider, storeRevision, err)
	}
	store, ok := storeValue.(ArtifactStore)
	if !ok {
		t.Fatalf("artifact replacement store value = %T", storeValue)
	}
	return artifactReplacementHost{
		mounted: mounted, plan: plan, handler: handler, store: store,
		httpRevision: httpRevision, storeRevision: storeRevision,
		originalImplementation: originalImplementation, candidateImplementation: candidateImplementation,
		originalArtifact: originalArtifact, candidateArtifact: candidateArtifact,
	}
}

func assertArtifactResource(t *testing.T, serverURL string, artifact Artifact, wantHTML string) {
	t.Helper()
	target := fmt.Sprintf("%s%s?version=%d&digest=%s",
		serverURL, artifact.Path, artifact.Version, url.QueryEscape(artifact.Digest))
	response := getResource(t, &http.Client{}, target)
	if response.StatusCode != http.StatusOK || string(response.body) != wantHTML ||
		response.Header.Get("X-OpenRealtime-Artifact-Version") != fmt.Sprint(artifact.Version) ||
		response.Header.Get("X-OpenRealtime-Artifact-Digest") != artifact.Digest {
		t.Fatalf("artifact resource %s: status=%d headers=%v body=%q",
			artifact.ID, response.StatusCode, response.Header, response.body)
	}
	assertArtifactPolicy(t, response.Header)
}
