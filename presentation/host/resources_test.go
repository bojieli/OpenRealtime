package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestResourceStoreDescriptorsAreExactHostPlugins(t *testing.T) {
	tests := []struct {
		name       string
		factory    pluginruntime.Factory
		service    plugin.Contract
		config     plugin.Contract
		state      *plugin.Contract
		resource   string
		pluginName string
	}{
		{
			name: "artifacts", factory: NewArtifactStoreFactory(),
			service: presentation.ArtifactStoreContract, config: presentation.ArtifactStoreConfigContract,
			state:    &presentation.ArtifactStoreStateContract,
			resource: artifactStorageResource, pluginName: "openrealtime.presentation.host.artifact-store",
		},
		{
			name: "downloads", factory: NewDownloadStoreFactory(),
			service: presentation.DownloadStoreContract, config: presentation.DownloadStoreConfigContract,
			resource: downloadStorageResource, pluginName: "openrealtime.presentation.host.download-store",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor := test.factory.Descriptor()
			if err := descriptor.Validate(); err != nil {
				t.Fatal(err)
			}
			if descriptor.Name != test.pluginName || descriptor.Realm != plugin.PresentationHostRealm ||
				!slices.Equal(descriptor.Platforms, []string{"go"}) {
				t.Fatalf("descriptor identity/placement = %#v", descriptor)
			}
			if !slices.Equal(descriptor.Provides, []plugin.Contract{test.service}) ||
				!slices.Equal(descriptor.Requires, []plugin.Requirement{{
					Contract: presentation.HTTPRoutesContract,
				}}) || descriptor.ConfigSchema == nil || *descriptor.ConfigSchema != test.config {
				t.Fatalf("descriptor contracts = %#v", descriptor)
			}
			if (descriptor.StateSchema == nil) != (test.state == nil) ||
				(test.state != nil && *descriptor.StateSchema != *test.state) {
				t.Fatalf("descriptor state schema = %#v, want %#v", descriptor.StateSchema, test.state)
			}
			wantPermission := []plugin.Permission{{
				Kind: storagePermissionKind, Resource: test.resource,
				Operations: []string{storagePublishOperation},
			}}
			if !slices.EqualFunc(descriptor.Permissions, wantPermission, func(left, right plugin.Permission) bool {
				return left.Kind == right.Kind && left.Resource == right.Resource &&
					left.Authority == right.Authority && slices.Equal(left.Operations, right.Operations)
			}) {
				t.Fatalf("descriptor permission ceiling = %#v, want %#v", descriptor.Permissions, wantPermission)
			}
			wantStateLifecycle := test.state != nil
			if descriptor.Lifecycle != (plugin.Lifecycle{
				DisposeTimeoutMS: 5_000,
				Snapshot:         wantStateLifecycle,
				Restore:          wantStateLifecycle,
			}) {
				t.Fatalf("resource plugin lifecycle = %#v", descriptor.Lifecycle)
			}
			identity, err := descriptor.Identity()
			if err != nil || identity.Name != descriptor.Name || identity.Digest == "" {
				t.Fatalf("descriptor identity = %#v, %v", identity, err)
			}
		})
	}
}

func TestResourceStoreConfigurationIsStrictAndHardBounded(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		policy  resourceStorePolicy
		want    ResourceStoreLimits
		wantErr string
	}{
		{name: "artifact defaults", raw: `{}`, policy: artifactStorePolicy, want: artifactStorePolicy.defaults},
		{name: "download defaults", raw: `{}`, policy: downloadStorePolicy, want: downloadStorePolicy.defaults},
		{
			name: "explicit", raw: `{"max_entries":2,"max_item_bytes":7,"max_total_bytes":11}`,
			policy: artifactStorePolicy,
			want:   ResourceStoreLimits{MaxEntries: 2, MaxItemBytes: 7, MaxTotalBytes: 11},
		},
		{name: "duplicate", raw: `{"max_entries":2,"max_entries":3}`, policy: artifactStorePolicy, wantErr: "duplicate"},
		{name: "unknown", raw: `{"unbounded":true}`, policy: artifactStorePolicy, wantErr: "unknown field"},
		{name: "not object", raw: `[]`, policy: artifactStorePolicy, wantErr: "cannot unmarshal"},
		{name: "negative entries", raw: `{"max_entries":-1}`, policy: artifactStorePolicy, wantErr: "max_entries"},
		{name: "too many entries", raw: `{"max_entries":257}`, policy: artifactStorePolicy, wantErr: "max_entries"},
		{
			name:   "artifact item hard ceiling",
			raw:    fmt.Sprintf(`{"max_item_bytes":%d}`, maximumArtifactBytes+1),
			policy: artifactStorePolicy, wantErr: "max_item_bytes",
		},
		{
			name:   "download item hard ceiling",
			raw:    fmt.Sprintf(`{"max_item_bytes":%d}`, maximumDownloadBytes+1),
			policy: downloadStorePolicy, wantErr: "max_item_bytes",
		},
		{
			name: "total below item", raw: `{"max_item_bytes":8,"max_total_bytes":7}`,
			policy: artifactStorePolicy, wantErr: "max_total_bytes",
		},
		{
			name:   "artifact total hard ceiling",
			raw:    fmt.Sprintf(`{"max_total_bytes":%d}`, maximumArtifactTotalBytes+1),
			policy: artifactStorePolicy, wantErr: "max_total_bytes",
		},
		{name: "trailing", raw: `{} {}`, policy: artifactStorePolicy, wantErr: "trailing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseResourceStoreConfig([]byte(test.raw), test.policy)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parse error = %v, want substring %q", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("limits = %#v, %v; want %#v", got, err, test.want)
			}
		})
	}
}

func TestArtifactStoreRevisesAndEvictsWithinBothBounds(t *testing.T) {
	instant := time.Date(2026, 8, 29, 12, 0, 0, 0, time.FixedZone("fixture", 3600))
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 2, MaxItemBytes: 10, MaxTotalBytes: 10,
	}, func() time.Time { return instant })
	ctx := context.Background()
	first, err := store.Publish(ctx, ArtifactInput{ID: "a", HTML: "12345"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Title != "a" || first.Version != 1 || first.Bytes != 5 ||
		first.UpdatedAt.Location() != time.UTC || first.Path != "/client/v1/artifacts/a" ||
		!strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("first artifact = %#v", first)
	}
	if _, err := store.Publish(ctx, ArtifactInput{ID: "b", Title: "Second", HTML: "67890"}); err != nil {
		t.Fatal(err)
	}
	revised, err := store.Publish(ctx, ArtifactInput{ID: "a", Title: "First", HTML: "xx"})
	if err != nil || revised.Version != 2 {
		t.Fatalf("revised artifact = %#v, %v", revised, err)
	}
	if _, err := store.Publish(ctx, ArtifactInput{ID: "c", HTML: "1234567"}); err != nil {
		t.Fatal(err)
	}
	listed := store.List()
	if len(listed) != 2 || listed[0].ID != "a" || listed[1].ID != "c" {
		t.Fatalf("retained artifacts = %#v", listed)
	}
	if _, found := store.Lookup("b"); found {
		t.Fatal("oldest artifact survived total-byte eviction")
	}
	stats := store.Stats()
	if stats.Entries != 2 || stats.Bytes != 9 || stats.Evictions != 1 {
		t.Fatalf("artifact stats = %#v", stats)
	}
	before := stats
	if _, err := store.Publish(ctx, ArtifactInput{ID: "bad/id", HTML: "x"}); err == nil {
		t.Fatal("invalid artifact id was accepted")
	}
	if _, err := store.Publish(ctx, ArtifactInput{ID: "large", HTML: "12345678901"}); err == nil {
		t.Fatal("oversized artifact was accepted")
	}
	if got := store.Stats(); got != before {
		t.Fatalf("refused artifact mutated store: before=%#v after=%#v", before, got)
	}
}

func TestArtifactInputAndDocumentHandlingAreAdversariallySafe(t *testing.T) {
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: 1024, MaxTotalBytes: 4096,
	}, nil)
	tests := []ArtifactInput{
		{ID: "../escape", HTML: "x"},
		{ID: "space id", HTML: "x"},
		{ID: "ok", Title: " padded", HTML: "x"},
		{ID: "ok", Title: "line\nbreak", HTML: "x"},
		{ID: "ok", HTML: " \n\t"},
		{ID: "ok", HTML: string([]byte{0xff})},
	}
	for _, input := range tests {
		if _, err := store.Publish(context.Background(), input); err == nil {
			t.Fatalf("unsafe artifact input was accepted: %#v", input)
		}
	}
	title := `safe</title><script>alert(1)</script>&`
	artifact, err := store.Publish(context.Background(), ArtifactInput{
		ID: "card", Title: title, HTML: `<button>choose</button>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	response, found := store.beginResponse(artifact.ID)
	if !found {
		t.Fatal("published artifact missing")
	}
	document := string(artifactDocument(response.metadata.Title, response.html))
	store.finishResponse(&response)
	if !strings.Contains(document, `safe&lt;/title&gt;&lt;script&gt;alert(1)&lt;/script&gt;&amp;`) ||
		strings.Contains(document, `<title>safe</title><script>`) {
		t.Fatalf("artifact wrapper did not escape title: %s", document)
	}
	complete := `<!DOCTYPE html><html><body>complete</body></html>`
	if got := string(artifactDocument("ignored", []byte(complete))); got != complete {
		t.Fatalf("complete artifact was rewritten: %q", got)
	}
}

func TestDownloadStoreCopiesInputRevisesAndEvictsWithinBothBounds(t *testing.T) {
	store := newDownloadStore(ResourceStoreLimits{
		MaxEntries: 2, MaxItemBytes: 10, MaxTotalBytes: 10,
	}, nil)
	content := []byte("12345")
	first, err := store.Publish(context.Background(), DownloadInput{
		ID: "a", Filename: "a.csv", MediaType: "text/csv; charset=utf-8", Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	content[0] = 'X'
	if first.Version != 1 || first.Bytes != 5 || first.Path != "/client/v1/downloads/a" ||
		first.MediaType != "text/csv; charset=utf-8" {
		t.Fatalf("first download = %#v", first)
	}
	if _, err := store.Publish(context.Background(), DownloadInput{
		ID: "b", Filename: "b.bin", Content: []byte("67890"),
	}); err != nil {
		t.Fatal(err)
	}
	revised, err := store.Publish(context.Background(), DownloadInput{
		ID: "a", Filename: "a.txt", MediaType: "text/plain", Content: []byte("xx"),
	})
	if err != nil || revised.Version != 2 {
		t.Fatalf("revised download = %#v, %v", revised, err)
	}
	if _, err := store.Publish(context.Background(), DownloadInput{
		ID: "c", Filename: "c.bin", Content: []byte("1234567"),
	}); err != nil {
		t.Fatal(err)
	}
	listed := store.List()
	if len(listed) != 2 || listed[0].ID != "a" || listed[1].ID != "c" {
		t.Fatalf("retained downloads = %#v", listed)
	}
	if _, found := store.Lookup("b"); found {
		t.Fatal("oldest download survived total-byte eviction")
	}
	stats := store.Stats()
	if stats.Entries != 2 || stats.Bytes != 9 || stats.Evictions != 1 {
		t.Fatalf("download stats = %#v", stats)
	}
	response, found := store.beginResponse("a")
	if !found || string(response.content) != "xx" {
		t.Fatalf("stored content aliases caller or wrong revision: %q", response.content)
	}
	store.finishResponse(&response)
}

func TestDownloadInputRejectsPathsHeadersAndAmbiguousTypes(t *testing.T) {
	store := newDownloadStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: 16, MaxTotalBytes: 32,
	}, nil)
	tests := []DownloadInput{
		{ID: "../x", Filename: "x.txt", Content: []byte("x")},
		{ID: "x", Filename: "../secret", Content: []byte("x")},
		{ID: "x", Filename: `dir\\secret`, Content: []byte("x")},
		{ID: "x", Filename: "header\r\nInjected: yes", Content: []byte("x")},
		{ID: "x", Filename: "spoof\u202eexe.txt", Content: []byte("x")},
		{ID: "x", Filename: "x.txt", MediaType: "text/*", Content: []byte("x")},
		{ID: "x", Filename: "x.txt", MediaType: "text/plain\r\nX: y", Content: []byte("x")},
		{ID: "x", Filename: "x.txt"},
		{ID: "x", Filename: "x.txt", Content: []byte("12345678901234567")},
	}
	for _, input := range tests {
		if _, err := store.Publish(context.Background(), input); err == nil {
			t.Fatalf("unsafe download input was accepted: %#v", input)
		}
	}
	if stats := store.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("refused downloads mutated store: %#v", stats)
	}
}

func TestMountedResourceRoutesEnforceSandboxAndAttachmentPolicies(t *testing.T) {
	host := mountResourceHost(t, resourceHostConfig{artifacts: true, downloads: true, grant: true})
	artifact, err := host.artifacts.Publish(context.Background(), ArtifactInput{
		ID: "card", Title: "Card", HTML: `<script>document.body.dataset.ready='yes'</script><p>ready</p>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	download, err := host.downloads.Publish(context.Background(), DownloadInput{
		ID: "report", Filename: `résumé "final".html`, MediaType: "text/html",
		Content: []byte(`<script>top.location='https://attacker.invalid'</script>`),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host.handler)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: time.Second}

	artifactResponse := getResource(t, client, server.URL+artifact.Path)
	if artifactResponse.StatusCode != http.StatusOK ||
		artifactResponse.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
		artifactResponse.Header.Get("X-OpenRealtime-Artifact-Version") != "1" ||
		!strings.Contains(string(artifactResponse.body), "dataset.ready") {
		t.Fatalf("artifact response status=%d headers=%v body=%q",
			artifactResponse.StatusCode, artifactResponse.Header, artifactResponse.body)
	}
	assertArtifactPolicy(t, artifactResponse.Header)
	pinnedArtifact := getResource(t, client, server.URL+artifact.Path+"?version=1&digest="+
		url.QueryEscape(artifact.Digest))
	if pinnedArtifact.StatusCode != http.StatusOK || contentDigest(pinnedArtifact.body) != artifact.Digest ||
		pinnedArtifact.Header.Get("X-OpenRealtime-Artifact-Digest") != artifact.Digest ||
		pinnedArtifact.Header.Get("ETag") != `"`+artifact.Digest+`"` {
		t.Fatalf("pinned artifact response status=%d headers=%v digest=%s",
			pinnedArtifact.StatusCode, pinnedArtifact.Header, contentDigest(pinnedArtifact.body))
	}

	downloadResponse := getResource(t, client, server.URL+download.Path)
	if downloadResponse.StatusCode != http.StatusOK || string(downloadResponse.body) != string([]byte(`<script>top.location='https://attacker.invalid'</script>`)) ||
		downloadResponse.Header.Get("Content-Type") != "text/html" ||
		downloadResponse.Header.Get("X-OpenRealtime-Download-Version") != "1" {
		t.Fatalf("download response status=%d headers=%v body=%q",
			downloadResponse.StatusCode, downloadResponse.Header, downloadResponse.body)
	}
	disposition, parameters, err := mime.ParseMediaType(downloadResponse.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || parameters["filename"] != download.Filename {
		t.Fatalf("download disposition = %q %#v, %v", disposition, parameters, err)
	}
	assertDownloadPolicy(t, downloadResponse.Header)
	pinnedDownload := getResource(t, client, server.URL+download.Path+"?version=1&digest="+
		url.QueryEscape(download.Digest))
	if pinnedDownload.StatusCode != http.StatusOK || contentDigest(pinnedDownload.body) != download.Digest ||
		pinnedDownload.Header.Get("X-OpenRealtime-Download-Digest") != download.Digest ||
		pinnedDownload.Header.Get("ETag") != `"`+download.Digest+`"` {
		t.Fatalf("pinned download response status=%d headers=%v digest=%s",
			pinnedDownload.StatusCode, pinnedDownload.Header, contentDigest(pinnedDownload.body))
	}

	for _, path := range []string{"/client/v1/artifacts/missing", "/client/v1/artifacts/%2e%2e", "/client/v1/downloads/missing"} {
		response := getResource(t, client, server.URL+path)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s status = %d", path, response.StatusCode)
		}
		if strings.Contains(path, "artifacts") {
			assertArtifactPolicy(t, response.Header)
		} else {
			assertDownloadPolicy(t, response.Header)
		}
	}

	for _, path := range []string{artifact.Path, download.Path} {
		request, _ := http.NewRequest(http.MethodHead, server.URL+path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || len(body) != 0 || response.ContentLength <= 0 {
			t.Fatalf("HEAD %s status=%d length=%d body=%q", path, response.StatusCode, response.ContentLength, body)
		}
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+artifact.Path, strings.NewReader("ignored"))
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST artifact status = %d", response.StatusCode)
	}

	revisedArtifact, err := host.artifacts.Publish(context.Background(), ArtifactInput{
		ID: artifact.ID, Title: "Card revised", HTML: "<p>new revision</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		artifact.Path + "?version=1&digest=" + url.QueryEscape(artifact.Digest),
		download.Path + "?version=1&digest=" + url.QueryEscape("sha256:"+strings.Repeat("0", 64)),
	} {
		stale := getResource(t, client, server.URL+target)
		if stale.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("stale resource %s status = %d", target, stale.StatusCode)
		}
	}
	malformed := getResource(t, client, server.URL+artifact.Path+"?version=2&digest="+
		url.QueryEscape(revisedArtifact.Digest)+"&extra=1")
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("extended artifact revision query status = %d", malformed.StatusCode)
	}
	fresh := getResource(t, client, server.URL+revisedArtifact.Path+"?version=2&digest="+
		url.QueryEscape(revisedArtifact.Digest))
	if fresh.StatusCode != http.StatusOK || contentDigest(fresh.body) != revisedArtifact.Digest {
		t.Fatalf("fresh artifact response status=%d digest=%s", fresh.StatusCode, contentDigest(fresh.body))
	}
}

func TestResourceStoresFailClosedWithoutPublishGrants(t *testing.T) {
	for _, test := range []struct {
		name      string
		artifacts bool
		downloads bool
		want      string
	}{
		{name: "artifact", artifacts: true, want: "memory-publish grant"},
		{name: "download", downloads: true, want: "memory-publish grant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildResourceHost(resourceHostConfig{
				artifacts: test.artifacts, downloads: test.downloads, grant: false,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("mount error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestUnmountWithdrawsRouteClosesAndWipesStoreThenReactivationStartsEmpty(t *testing.T) {
	host := mountResourceHost(t, resourceHostConfig{artifacts: true, downloads: true, grant: true})
	if _, err := host.artifacts.Publish(context.Background(), ArtifactInput{
		ID: "secret", HTML: "transient secret",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.downloads.Publish(context.Background(), DownloadInput{
		ID: "keep", Filename: "keep.txt", Content: []byte("download remains"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.mounted.Unmount(context.Background(), "artifacts"); err != nil {
		t.Fatal(err)
	}
	if got := host.artifacts.Stats(); !got.Closed || got.Entries != 0 || got.Bytes != 0 {
		t.Fatalf("unmounted artifact store retained data: %#v", got)
	}
	if _, err := host.artifacts.Publish(context.Background(), ArtifactInput{
		ID: "late", HTML: "not admitted",
	}); !errors.Is(err, ErrResourceStoreClosed) {
		t.Fatalf("publish through withdrawn service = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/client/v1/artifacts/secret", nil)
	response := httptest.NewRecorder()
	host.handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("withdrawn artifact route status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/client/v1/downloads/keep", nil)
	response = httptest.NewRecorder()
	host.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "download remains" {
		t.Fatalf("unrelated download route status=%d body=%q", response.Code, response.Body.String())
	}

	if err := host.mounted.Activate(context.Background(), "artifacts"); err != nil {
		t.Fatal(err)
	}
	value, contract, _, _, err := host.mounted.Export("artifacts")
	if err != nil || contract != presentation.ArtifactStoreContract {
		t.Fatalf("reactivated export = %#v, %#v, %v", value, contract, err)
	}
	reactivated := value.(ArtifactStore)
	if reactivated == host.artifacts || reactivated.Stats().Entries != 0 || reactivated.Stats().Closed {
		t.Fatalf("reactivated store reused withdrawn state: old=%p new=%p stats=%#v",
			host.artifacts, reactivated, reactivated.Stats())
	}
	request = httptest.NewRequest(http.MethodGet, "/client/v1/artifacts/secret", nil)
	response = httptest.NewRecorder()
	host.handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("reactivated route leaked old artifact, status=%d", response.Code)
	}
}

func TestUnmountDrainsAnAdmittedResourceHandlerBeforeClosing(t *testing.T) {
	host := mountResourceHost(t, resourceHostConfig{artifacts: true, grant: true})
	if _, err := host.artifacts.Publish(context.Background(), ArtifactInput{
		ID: "card", HTML: "blocked response",
	}); err != nil {
		t.Fatal(err)
	}
	writer := &blockingResourceWriter{
		header: make(http.Header), started: make(chan struct{}), release: make(chan struct{}),
	}
	served := make(chan struct{})
	go func() {
		host.handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/client/v1/artifacts/card", nil))
		close(served)
	}()
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("artifact handler did not reach response write")
	}
	unmounted := make(chan error, 1)
	go func() { unmounted <- host.mounted.Unmount(context.Background(), "artifacts") }()
	select {
	case err := <-unmounted:
		t.Fatalf("unmount returned before admitted handler drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(writer.release)
	select {
	case err := <-unmounted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unmount did not finish after resource handler drained")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("resource handler did not return")
	}
	if stats := host.artifacts.Stats(); !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("drained store retained content: %#v", stats)
	}
}

func TestResourceStoreDisposalHonorsContextAndCanFinishAfterAStalledHandler(t *testing.T) {
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 1, MaxItemBytes: 64, MaxTotalBytes: 64,
	}, nil)
	if _, err := store.Publish(context.Background(), ArtifactInput{ID: "card", HTML: "secret"}); err != nil {
		t.Fatal(err)
	}
	response, found := store.beginResponse("card")
	if !found {
		t.Fatal("artifact response was not admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded close error = %v, want context cancellation", err)
	}
	if stats := store.Stats(); !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("timed-out close retained store content: %#v", stats)
	}
	store.finishResponse(&response)
	if err := store.close(context.Background()); err != nil {
		t.Fatalf("finish close after handler drain: %v", err)
	}
}

func TestResourceStoresAreRaceSafeDuringPublicationReadsAndClose(t *testing.T) {
	artifacts := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 16, MaxItemBytes: 128, MaxTotalBytes: 1024,
	}, nil)
	downloads := newDownloadStore(ResourceStoreLimits{
		MaxEntries: 16, MaxItemBytes: 128, MaxTotalBytes: 1024,
	}, nil)
	start := make(chan struct{})
	var ready sync.WaitGroup
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		ready.Add(1)
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			ready.Done()
			<-start
			for iteration := 0; iteration < 100; iteration++ {
				id := fmt.Sprintf("item_%d", (worker+iteration)%24)
				if worker%2 == 0 {
					_, err := artifacts.Publish(context.Background(), ArtifactInput{
						ID: id, HTML: strings.Repeat("x", 1+iteration%64),
					})
					if err != nil && !errors.Is(err, ErrResourceStoreClosed) {
						t.Errorf("publish artifact: %v", err)
						return
					}
					_, _ = artifacts.Lookup(id)
				} else {
					_, err := downloads.Publish(context.Background(), DownloadInput{
						ID: id, Filename: id + ".bin", Content: []byte(strings.Repeat("y", 1+iteration%64)),
					})
					if err != nil && !errors.Is(err, ErrResourceStoreClosed) {
						t.Errorf("publish download: %v", err)
						return
					}
					_, _ = downloads.Lookup(id)
				}
			}
		}(worker)
	}
	ready.Wait()
	close(start)
	if err := artifacts.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := downloads.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if err := artifacts.close(context.Background()); err != nil {
		t.Fatalf("idempotent artifact close: %v", err)
	}
	if err := downloads.close(context.Background()); err != nil {
		t.Fatalf("idempotent download close: %v", err)
	}
	for name, stats := range map[string]ResourceStoreStats{
		"artifacts": artifacts.Stats(), "downloads": downloads.Stats(),
	} {
		if !stats.Closed || stats.Entries != 0 || stats.Bytes != 0 {
			t.Errorf("%s final stats = %#v", name, stats)
		}
	}
}

type resourceHostConfig struct {
	artifacts bool
	downloads bool
	grant     bool
}

type resourceHost struct {
	mounted   *pluginruntime.Mounted
	handler   http.Handler
	artifacts ArtifactStore
	downloads DownloadStore
}

func mountResourceHost(t *testing.T, config resourceHostConfig) *resourceHost {
	t.Helper()
	host, err := buildResourceHost(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.mounted.Close(context.Background()); err != nil {
			t.Errorf("close resource host: %v", err)
		}
	})
	return host
}

func buildResourceHost(config resourceHostConfig) (*resourceHost, error) {
	router := NewRouterFactory()
	factories := []pluginruntime.Factory{router}
	entries := []plugin.ProfileEntry{{
		ID: "router", Plugin: router.Descriptor().Name, Scope: "root",
	}}
	exports := []plugin.ProfileExport{{
		Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name,
	}}
	values := make(map[string]json.RawMessage)
	permissions := make(map[string][]plugin.Permission)
	if config.artifacts {
		factory := NewArtifactStoreFactory()
		factories = append(factories, factory)
		entries = append(entries, plugin.ProfileEntry{ID: "artifacts", Plugin: factory.Descriptor().Name, Scope: "root"})
		exports = append(exports, plugin.ProfileExport{
			Name: "artifacts", Provider: "artifacts", Service: presentation.ArtifactStoreContract.Name,
		})
		values["artifacts"] = json.RawMessage(`{}`)
		if config.grant {
			permissions["artifacts"] = []plugin.Permission{{
				Kind: storagePermissionKind, Resource: artifactStorageResource,
				Operations: []string{storagePublishOperation},
			}}
		}
	}
	if config.downloads {
		factory := NewDownloadStoreFactory()
		factories = append(factories, factory)
		entries = append(entries, plugin.ProfileEntry{ID: "downloads", Plugin: factory.Descriptor().Name, Scope: "root"})
		exports = append(exports, plugin.ProfileExport{
			Name: "downloads", Provider: "downloads", Service: presentation.DownloadStoreContract.Name,
		})
		values["downloads"] = json.RawMessage(`{}`)
		if config.grant {
			permissions["downloads"] = []plugin.Permission{{
				Kind: storagePermissionKind, Resource: downloadStorageResource,
				Operations: []string{storagePublishOperation},
			}}
		}
	}
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	for _, factory := range factories {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			return nil, err
		}
		if err := registry.Register("", factory); err != nil {
			return nil, err
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.resource-host.test", Revision: 1, Realm: plugin.PresentationHostRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries, Exports: exports,
	})
	if err != nil {
		return nil, err
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return nil, err
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		return nil, err
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, Values: values, Permissions: permissions,
	})
	if err != nil {
		return nil, err
	}
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		_ = mounted.Close(context.Background())
		return nil, err
	}
	host := &resourceHost{mounted: mounted, handler: handler}
	if config.artifacts {
		value, contract, _, _, exportErr := mounted.Export("artifacts")
		if exportErr != nil || contract != presentation.ArtifactStoreContract {
			_ = mounted.Close(context.Background())
			return nil, errors.Join(exportErr, errors.New("artifact store export has the wrong contract"))
		}
		host.artifacts = value.(ArtifactStore)
	}
	if config.downloads {
		value, contract, _, _, exportErr := mounted.Export("downloads")
		if exportErr != nil || contract != presentation.DownloadStoreContract {
			_ = mounted.Close(context.Background())
			return nil, errors.Join(exportErr, errors.New("download store export has the wrong contract"))
		}
		host.downloads = value.(DownloadStore)
	}
	return host, nil
}

type resourceResponse struct {
	StatusCode int
	Header     http.Header
	body       []byte
}

func getResource(t *testing.T, client *http.Client, target string) resourceResponse {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resourceResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), body: body}
}

func assertArtifactPolicy(t *testing.T, header http.Header) {
	t.Helper()
	policy := header.Get("Content-Security-Policy")
	for _, required := range []string{
		"sandbox allow-scripts", "default-src 'none'", "connect-src 'none'",
		"form-action 'none'", "base-uri 'none'", "frame-ancestors 'self'",
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("artifact CSP %q lacks %q", policy, required)
		}
	}
	if strings.Contains(policy, "allow-same-origin") {
		t.Errorf("artifact CSP grants same-origin: %q", policy)
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store, max-age=0", "Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"X-Frame-Options": "SAMEORIGIN",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("artifact header %s = %q, want %q", name, got, want)
		}
	}
}

func assertDownloadPolicy(t *testing.T, header http.Header) {
	t.Helper()
	policy := header.Get("Content-Security-Policy")
	for _, required := range []string{
		"sandbox", "default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'",
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("download CSP %q lacks %q", policy, required)
		}
	}
	for name, want := range map[string]string{
		"Cache-Control": "no-store, max-age=0", "Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"X-Download-Options": "noopen", "X-Frame-Options": "DENY",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("download header %s = %q, want %q", name, got, want)
		}
	}
}

type blockingResourceWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (writer *blockingResourceWriter) Header() http.Header { return writer.header }

func (writer *blockingResourceWriter) WriteHeader(int) {}

func (writer *blockingResourceWriter) Write(payload []byte) (int, error) {
	writer.once.Do(func() { close(writer.started) })
	<-writer.release
	return len(payload), nil
}
