package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
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

func TestDownloadStoreReplacementMigratesRetainedContentAtSafePoint(t *testing.T) {
	host := mountDownloadReplacementHost(t, json.RawMessage(
		`{"max_entries":2,"max_item_bytes":4096,"max_total_bytes":8192}`,
	))
	server := httptest.NewServer(host.handler)
	t.Cleanup(server.Close)

	const (
		retainedID       = "retained_download_3R"
		retainedFilename = "private migration 3R.bin"
	)
	retainedContent := append([]byte{0x00, 0x01, 0xfe, 0xff},
		[]byte(" private binary migration payload 3R ")...)
	if _, err := host.store.Publish(context.Background(), DownloadInput{
		ID: retainedID, Filename: "initial.bin", Content: []byte("initial"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.store.Publish(context.Background(), DownloadInput{
		ID: "evicted_download_3R", Filename: "evicted.txt", MediaType: "text/plain",
		Content: []byte("evicted"),
	}); err != nil {
		t.Fatal(err)
	}
	retained, err := host.store.Publish(context.Background(), DownloadInput{
		ID: retainedID, Filename: retainedFilename, MediaType: "application/octet-stream",
		Content: retainedContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	newestContent := []byte("newest download 3R")
	newest, err := host.store.Publish(context.Background(), DownloadInput{
		ID: "newest_download_3R", Filename: "newest.txt", MediaType: "text/plain; charset=utf-8",
		Content: newestContent,
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
	assertDownloadResource(t, server.URL, retained, retainedContent)
	assertDownloadResource(t, server.URL, newest, newestContent)

	beforeRefusal := host.mounted.Live()
	refused, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        beforeRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "downloads", SetPermissions: true, Permissions: []plugin.Permission{},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "memory-publish grant") {
		t.Fatalf("permissionless download candidate error = %v", err)
	}
	if !reflect.DeepEqual(refused, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("permissionless download candidate returned receipt %#v", refused)
	}
	afterRefusal := host.mounted.Live()
	if afterRefusal.Sequence != beforeRefusal.Sequence ||
		!reflect.DeepEqual(afterRefusal.Entries["downloads"], beforeRefusal.Entries["downloads"]) ||
		host.store.Stats().Closed {
		t.Fatalf("permission refusal crossed safe point: before=%+v after=%+v stats=%#v",
			beforeRefusal, afterRefusal, host.store.Stats())
	}
	assertDownloadResource(t, server.URL, retained, retainedContent)

	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "downloads", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats := host.store.Stats(); !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("retired download store retained content: %#v", stats)
	}

	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err :=
		host.mounted.Export("http")
	if err != nil || afterHTTPValue != host.handler ||
		afterHTTPContract != presentation.HTTPHandlerContract || afterHTTPProvider != "router" ||
		afterHTTPRevision != host.httpRevision {
		t.Fatalf("stable HTTP export after download replacement = %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterStoreValue, afterStoreContract, afterStoreProvider, afterStoreRevision, err :=
		host.mounted.Export("downloads")
	if err != nil || afterStoreContract != presentation.DownloadStoreContract ||
		afterStoreProvider != "downloads" || afterStoreRevision <= host.storeRevision {
		t.Fatalf("replacement download export = %T/%+v/%s/%d, %v",
			afterStoreValue, afterStoreContract, afterStoreProvider, afterStoreRevision, err)
	}
	replacement, ok := afterStoreValue.(DownloadStore)
	if !ok || replacement == nil || replacement == host.store {
		t.Fatalf("replacement download service = %T, predecessor=%T", afterStoreValue, host.store)
	}
	if got := replacement.Stats(); !reflect.DeepEqual(got, wantStats) {
		t.Fatalf("replacement download stats = %#v, want %#v", got, wantStats)
	}
	if got := replacement.List(); !reflect.DeepEqual(got, wantList) {
		t.Fatalf("replacement download order/metadata = %#v, want %#v", got, wantList)
	}
	assertDownloadResource(t, server.URL, retained, retainedContent)
	assertDownloadResource(t, server.URL, newest, newestContent)

	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != host.plan.Fingerprint ||
		receipt.BeforeSequence != before.Sequence || receipt.AfterSequence != host.mounted.Live().Sequence ||
		len(receipt.Transitions) != 1 || len(receipt.Retirements) != 1 ||
		len(receipt.StateTransfers) != 1 {
		t.Fatalf("download replacement receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "downloads" ||
		transition.BeforeImplementation != host.originalImplementation ||
		transition.AfterImplementation != host.candidateImplementation ||
		transition.BeforeRuntime != host.originalArtifact ||
		transition.AfterRuntime != host.candidateArtifact {
		t.Fatalf("download replacement transition = %#v", transition)
	}
	retirement := receipt.Retirements[0]
	if retirement.Entry != "downloads" || retirement.RetiredScopes == 0 ||
		retirement.ClosedScopes != retirement.RetiredScopes ||
		retirement.RemainingWorkers != 0 || retirement.RemainingEffects != 0 ||
		retirement.RemainingChildScopes != 0 || retirement.RemainingServices != 0 {
		t.Fatalf("download replacement retirement = %#v", retirement)
	}
	transfer := receipt.StateTransfers[0]
	if transfer.Entry != "downloads" || transfer.Schema != presentation.DownloadStoreStateContract ||
		transfer.BeforeStateDigest == "" || transfer.BeforeStateDigest != transfer.AfterStateDigest ||
		transfer.MigratorImplementation != host.candidateImplementation {
		t.Fatalf("download replacement state transfer = %#v", transfer)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		retainedID, retainedFilename, base64.StdEncoding.EncodeToString(retainedContent),
		"evicted_download_3R",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("download replacement receipt exposed %q: %s", secret, encoded)
		}
	}

	revised, err := replacement.Publish(context.Background(), DownloadInput{
		ID: retainedID, Filename: "post-migration.txt", MediaType: "text/plain",
		Content: []byte("post migration"),
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
		t.Fatalf("replacement download store retained final content: %#v", stats)
	}
}

func TestDownloadStoreReplacementRefusesOversizedSnapshotBeforeTeardown(t *testing.T) {
	host := mountDownloadReplacementHost(t, json.RawMessage(
		`{"max_entries":2,"max_item_bytes":2097152,"max_total_bytes":4194304}`,
	))
	server := httptest.NewServer(host.handler)
	t.Cleanup(server.Close)

	contentBytes := 3 * pluginruntime.MaximumStateSnapshotBytes / 4
	content := bytes.Repeat([]byte{0xa5}, contentBytes)
	download, err := host.store.Publish(context.Background(), DownloadInput{
		ID: "oversized_state_5S", Filename: "oversized.bin", Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "downloads", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot plugin state") ||
		!strings.Contains(err.Error(), "migration envelope") {
		t.Fatalf("oversized download snapshot error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("oversized download snapshot returned receipt %#v", receipt)
	}
	after := host.mounted.Live()
	if after.Sequence != before.Sequence ||
		!reflect.DeepEqual(after.Entries["downloads"], before.Entries["downloads"]) {
		t.Fatalf("oversized snapshot changed live identity: before=%+v after=%+v", before, after)
	}
	if stats := host.store.Stats(); stats.Closed || stats.Entries != 1 ||
		stats.Bytes != int64(len(content)) {
		t.Fatalf("oversized snapshot disturbed predecessor store: %#v", stats)
	}
	if admitted, err := host.store.Publish(context.Background(), DownloadInput{
		ID: "after_refusal_5S", Filename: "after.txt", MediaType: "text/plain",
		Content: []byte("still writable"),
	}); err != nil || admitted.Version != 1 {
		t.Fatalf("download mutation did not resume after snapshot refusal = %#v, %v", admitted, err)
	}
	value, contract, provider, revision, err := host.mounted.Export("downloads")
	if err != nil || value != host.store || contract != presentation.DownloadStoreContract ||
		provider != "downloads" || revision != host.storeRevision {
		t.Fatalf("download export after oversized refusal = %T/%+v/%s/%d, %v",
			value, contract, provider, revision, err)
	}
	response := getResource(t, &http.Client{}, server.URL+download.Path)
	if response.StatusCode != http.StatusOK || len(response.body) != len(content) ||
		contentDigest(response.body) != download.Digest {
		t.Fatalf("download after oversized refusal: status=%d bytes=%d digest=%s",
			response.StatusCode, len(response.body), contentDigest(response.body))
	}
}

func TestDownloadStoreStateMigrationRejectsMalformedOrIncompatibleState(t *testing.T) {
	limits := ResourceStoreLimits{MaxEntries: 2, MaxItemBytes: 64, MaxTotalBytes: 96}
	candidate := downloadStoreCandidate{entryID: "downloads", limits: limits}
	valid := pluginruntime.StateMigration{
		EntryID: "downloads", Schema: presentation.DownloadStoreStateContract,
		SourceImplementation: "openrealtime.presentation.host.download-store",
		Snapshot: json.RawMessage(
			`{"entries":[{"content":"AAEC/w==","filename":"safe.bin","id":"file","media_type":"application/octet-stream","updated_at":"2026-09-02T00:00:00Z","version":1}],"evictions":0,"format_version":1}`,
		),
	}
	migrated, err := candidate.MigrateState(context.Background(), valid)
	if err != nil {
		t.Fatalf("valid download migration: %v", err)
	}
	state, err := decodeDownloadStoreState(migrated, limits)
	if err != nil || len(state.Entries) != 1 || state.Entries[0].ID != "file" ||
		!bytes.Equal(state.Entries[0].Content, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("valid migrated download state = %#v, %v", state, err)
	}
	wipeDownloadStoreState(&state)

	tests := []struct {
		name    string
		mutate  func(*pluginruntime.StateMigration)
		wantErr string
	}{
		{
			name: "wrong entry", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) { migration.EntryID = "artifacts" },
		},
		{
			name: "wrong schema", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Schema = presentation.ArtifactStoreContract
			},
		},
		{
			name: "duplicate field", wantErr: "duplicate",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"evictions":1,"entries":[]}`,
				)
			},
		},
		{
			name: "invalid base64", wantErr: "invalid base64",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"file","filename":"safe.bin","media_type":"application/octet-stream","content":"***","version":1,"updated_at":"2026-09-02T00:00:00Z"}]}`,
				)
			},
		},
		{
			name: "unsafe filename", wantErr: "filename",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"file","filename":"../secret","media_type":"application/octet-stream","content":"eA==","version":1,"updated_at":"2026-09-02T00:00:00Z"}]}`,
				)
			},
		},
		{
			name: "noncanonical media type", wantErr: "invalid media type",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"file","filename":"safe.txt","media_type":"TEXT/PLAIN","content":"eA==","version":1,"updated_at":"2026-09-02T00:00:00Z"}]}`,
				)
			},
		},
		{
			name: "noncanonical timestamp", wantErr: "invalid timestamp",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = json.RawMessage(
					`{"format_version":1,"evictions":0,"entries":[{"id":"file","filename":"safe.bin","media_type":"application/octet-stream","content":"eA==","version":1,"updated_at":"2026-09-02T01:00:00+01:00"}]}`,
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
				t.Fatalf("malformed download migration = %s, %v; want %q",
					migrated, err, test.wantErr)
			}
		})
	}
}

type downloadReplacementHost struct {
	mounted                 *pluginruntime.Mounted
	plan                    plugin.Plan
	handler                 http.Handler
	store                   DownloadStore
	httpRevision            uint64
	storeRevision           uint64
	originalImplementation  string
	candidateImplementation string
	originalArtifact        inspect.ArtifactIdentity
	candidateArtifact       inspect.ArtifactIdentity
}

func mountDownloadReplacementHost(
	t *testing.T, values json.RawMessage,
) downloadReplacementHost {
	t.Helper()
	router := NewRouterFactory()
	original := NewDownloadStoreFactory()
	candidate := NewDownloadStoreFactory()
	catalog := plugin.NewCatalog()
	for _, factory := range []pluginruntime.Factory{router, original} {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.download-replacement.test",
		Revision:      1,
		Realm:         plugin.PresentationHostRealm,
		Scopes:        []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{
			{ID: "router", Plugin: router.Descriptor().Name, Scope: "root"},
			{ID: "downloads", Plugin: original.Descriptor().Name, Scope: "root"},
		},
		Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "downloads", Provider: "downloads", Service: presentation.DownloadStoreContract.Name},
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
	const candidateImplementation = "openrealtime.presentation.host.download-store-v2"
	originalArtifact := hostTestArtifact("go://host-download-store-v1", "build-1", "a")
	candidateArtifact := hostTestArtifact("go://host-download-store-v2", "build-2", "b")
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}{
		{router.Descriptor().Name, hostTestArtifact("go://host-router", "build-1", "9"), router},
		{originalImplementation, originalArtifact, original},
		{candidateImplementation, candidateArtifact, candidate},
	} {
		if err := registry.RegisterArtifact(row.implementation, row.artifact, row.factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"downloads": values},
		Permissions: map[string][]plugin.Permission{
			"downloads": original.Descriptor().Permissions,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close download replacement host: %v", err)
		}
	})
	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("download replacement HTTP export = %T/%+v/%s/%d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("download replacement HTTP value = %T", httpValue)
	}
	storeValue, storeContract, storeProvider, storeRevision, err := mounted.Export("downloads")
	if err != nil || storeContract != presentation.DownloadStoreContract || storeProvider != "downloads" {
		t.Fatalf("download replacement store export = %T/%+v/%s/%d, %v",
			storeValue, storeContract, storeProvider, storeRevision, err)
	}
	store, ok := storeValue.(DownloadStore)
	if !ok {
		t.Fatalf("download replacement store value = %T", storeValue)
	}
	return downloadReplacementHost{
		mounted: mounted, plan: plan, handler: handler, store: store,
		httpRevision: httpRevision, storeRevision: storeRevision,
		originalImplementation: originalImplementation, candidateImplementation: candidateImplementation,
		originalArtifact: originalArtifact, candidateArtifact: candidateArtifact,
	}
}

func assertDownloadResource(t *testing.T, serverURL string, download Download, wantContent []byte) {
	t.Helper()
	target := fmt.Sprintf("%s%s?version=%d&digest=%s",
		serverURL, download.Path, download.Version, url.QueryEscape(download.Digest))
	response := getResource(t, &http.Client{}, target)
	disposition, parameters, dispositionErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if response.StatusCode != http.StatusOK || !bytes.Equal(response.body, wantContent) ||
		response.Header.Get("Content-Type") != download.MediaType ||
		response.Header.Get("X-OpenRealtime-Download-Version") != fmt.Sprint(download.Version) ||
		response.Header.Get("X-OpenRealtime-Download-Digest") != download.Digest ||
		dispositionErr != nil || disposition != "attachment" || parameters["filename"] != download.Filename {
		t.Fatalf("download resource %s: status=%d headers=%v bytes=%x disposition=%q/%#v/%v",
			download.ID, response.StatusCode, response.Header, response.body,
			disposition, parameters, dispositionErr)
	}
	assertDownloadPolicy(t, response.Header)
}
