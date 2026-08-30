package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	DefaultMaxArtifactBytes      int64 = 2 << 20
	DefaultMaxArtifactTotalBytes int64 = 16 << 20
	DefaultMaxArtifacts                = 64

	maximumArtifactBytes      int64 = 8 << 20
	maximumArtifactTotalBytes int64 = 64 << 20

	storagePermissionKind   = "storage.memory"
	storagePublishOperation = "publish"
	artifactStorageResource = "presentation-artifacts"
)

var artifactStorePolicy = resourceStorePolicy{
	name: "presentation artifact store",
	defaults: ResourceStoreLimits{
		MaxEntries: DefaultMaxArtifacts, MaxItemBytes: DefaultMaxArtifactBytes,
		MaxTotalBytes: DefaultMaxArtifactTotalBytes,
	},
	maximumItemBytes: maximumArtifactBytes, maximumTotalBytes: maximumArtifactTotalBytes,
}

// ArtifactInput is model-authored markup admitted by an effect/tool provider.
// HTML is copied before Publish returns and is never exposed by store metadata.
type ArtifactInput struct {
	ID    string
	Title string
	HTML  string
}

// Artifact is the payload-free reference a client can safely receive.
type Artifact struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Path      string    `json:"path"`
	Digest    string    `json:"digest"`
	Bytes     int64     `json:"bytes"`
	Version   uint64    `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ArtifactStore is the typed host service used by an independently mounted
// effect provider. Serving and publication remain owned by this plugin scope.
type ArtifactStore interface {
	Publish(context.Context, ArtifactInput) (Artifact, error)
	Lookup(id string) (Artifact, bool)
	List() []Artifact
	Stats() ResourceStoreStats
}

type storedArtifact struct {
	metadata Artifact
	html     []byte
}

type artifactStore struct {
	mu        sync.Mutex
	limits    ResourceStoreLimits
	entries   map[string]*storedArtifact
	order     []string
	total     int64
	evictions uint64
	lifecycle resourceLifecycle
	clock     func() time.Time
}

func newArtifactStore(limits ResourceStoreLimits, clock func() time.Time) *artifactStore {
	if clock == nil {
		clock = time.Now
	}
	return &artifactStore{
		limits: limits, entries: make(map[string]*storedArtifact),
		lifecycle: newResourceLifecycle(), clock: clock,
	}
}

func (store *artifactStore) Publish(ctx context.Context, input ArtifactInput) (Artifact, error) {
	if ctx == nil {
		return Artifact{}, errors.New("publish artifact: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, fmt.Errorf("publish artifact: %w", err)
	}
	if err := validateResourceID("artifact", input.ID); err != nil {
		return Artifact{}, err
	}
	title, err := validateArtifactTitle(input.Title)
	if err != nil {
		return Artifact{}, err
	}
	if title == "" {
		title = input.ID
	}
	store.mu.Lock()
	if store.lifecycle.closed {
		store.mu.Unlock()
		return Artifact{}, ErrResourceStoreClosed
	}
	maxItemBytes := store.limits.MaxItemBytes
	store.mu.Unlock()
	if input.HTML == "" || strings.TrimSpace(input.HTML) == "" {
		return Artifact{}, errors.New("artifact HTML cannot be empty")
	}
	if !utf8.ValidString(input.HTML) {
		return Artifact{}, errors.New("artifact HTML must be valid UTF-8")
	}
	if int64(len(input.HTML)) > maxItemBytes {
		return Artifact{}, fmt.Errorf("artifact has %d bytes; max_item_bytes is %d",
			len(input.HTML), maxItemBytes)
	}
	content := []byte(input.HTML)
	adopted := false
	defer func() {
		if !adopted {
			wipe(content)
		}
	}()
	digest := contentDigest(content)

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed {
		return Artifact{}, ErrResourceStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, fmt.Errorf("publish artifact: %w", err)
	}
	existing, updating := store.entries[input.ID]
	if updating && existing.metadata.Version == math.MaxUint64 {
		return Artifact{}, errors.New("artifact version is exhausted")
	}
	if err := store.makeRoomLocked(input.ID, int64(len(content)), !updating); err != nil {
		return Artifact{}, err
	}
	version := uint64(1)
	if updating {
		version = existing.metadata.Version + 1
		store.total -= int64(len(existing.html))
		wipe(existing.html)
	} else {
		existing = &storedArtifact{}
		store.entries[input.ID] = existing
	}
	metadata := Artifact{
		ID: input.ID, Title: title, Path: "/client/v1/artifacts/" + input.ID,
		Digest: digest, Bytes: int64(len(content)), Version: version,
		UpdatedAt: store.clock().UTC(),
	}
	existing.metadata = metadata
	existing.html = content
	store.total += int64(len(content))
	store.moveToNewestLocked(input.ID)
	adopted = true
	return metadata, nil
}

func (store *artifactStore) makeRoomLocked(id string, bytes int64, adding bool) error {
	oldBytes := int64(0)
	if existing := store.entries[id]; existing != nil {
		oldBytes = int64(len(existing.html))
	}
	wantEntries := len(store.entries)
	if adding {
		wantEntries++
	}
	wantBytes := store.total - oldBytes + bytes
	for wantEntries > store.limits.MaxEntries || wantBytes > store.limits.MaxTotalBytes {
		victim := ""
		for _, candidate := range store.order {
			if candidate != id {
				victim = candidate
				break
			}
		}
		if victim == "" {
			return errors.New("artifact cannot fit within configured retention bounds")
		}
		entry := store.entries[victim]
		victimBytes := int64(len(entry.html))
		wipe(entry.html)
		delete(store.entries, victim)
		store.removeFromOrderLocked(victim)
		store.total -= victimBytes
		wantEntries--
		wantBytes -= victimBytes
		if store.evictions < math.MaxUint64 {
			store.evictions++
		}
	}
	return nil
}

func (store *artifactStore) moveToNewestLocked(id string) {
	store.removeFromOrderLocked(id)
	store.order = append(store.order, id)
}

func (store *artifactStore) removeFromOrderLocked(id string) {
	for index, candidate := range store.order {
		if candidate == id {
			copy(store.order[index:], store.order[index+1:])
			store.order = store.order[:len(store.order)-1]
			return
		}
	}
}

func (store *artifactStore) Lookup(id string) (Artifact, bool) {
	if validateResourceID("artifact", id) != nil {
		return Artifact{}, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed {
		return Artifact{}, false
	}
	entry, found := store.entries[id]
	if !found {
		return Artifact{}, false
	}
	return entry.metadata, true
}

func (store *artifactStore) List() []Artifact {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed {
		return []Artifact{}
	}
	result := make([]Artifact, 0, len(store.order))
	for _, id := range store.order {
		result = append(result, store.entries[id].metadata)
	}
	return result
}

func (store *artifactStore) Stats() ResourceStoreStats {
	store.mu.Lock()
	defer store.mu.Unlock()
	return ResourceStoreStats{
		Limits: store.limits, Entries: len(store.entries), Bytes: store.total,
		Evictions: store.evictions, Closed: store.lifecycle.closed,
	}
}

type artifactResponse struct {
	metadata Artifact
	html     []byte
}

func (store *artifactStore) beginResponse(id string) (artifactResponse, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed || !store.lifecycle.beginLocked() {
		return artifactResponse{}, false
	}
	entry, found := store.entries[id]
	if !found {
		store.lifecycle.finishLocked()
		return artifactResponse{}, false
	}
	return artifactResponse{metadata: entry.metadata, html: slices.Clone(entry.html)}, true
}

func (store *artifactStore) finishResponse(response *artifactResponse) {
	wipe(response.html)
	response.html = nil
	store.mu.Lock()
	store.lifecycle.finishLocked()
	store.mu.Unlock()
}

func (store *artifactStore) close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close artifact store: nil context")
	}
	store.mu.Lock()
	drained := store.lifecycle.closeLocked()
	for _, entry := range store.entries {
		wipe(entry.html)
	}
	store.entries = nil
	store.order = nil
	store.total = 0
	store.mu.Unlock()
	return waitForResourceDrain(ctx, drained)
}

func artifactDocument(title string, html []byte) []byte {
	trimmed := bytes.TrimSpace(html)
	if beginsHTMLDocument(trimmed) {
		return slices.Clone(html)
	}
	prefix, suffix := artifactDocumentWrapper(title)
	document := make([]byte, 0, len(prefix)+len(html)+len(suffix))
	document = append(document, prefix...)
	document = append(document, html...)
	document = append(document, suffix...)
	return document
}

func artifactDocumentWrapper(title string) (prefix, suffix string) {
	return `<!doctype html><html><head><meta charset="utf-8">` +
			`<meta name="viewport" content="width=device-width, initial-scale=1">` +
			`<title>` + escapeHTMLTitle(title) + `</title>` +
			`<style>body{margin:0;padding:1rem;font:14px/1.55 ui-sans-serif,system-ui,sans-serif;` +
			`color-scheme:light dark}</style></head><body>`,
		`</body></html>`
}

func beginsHTMLDocument(content []byte) bool {
	const doctype = "<!doctype html"
	if len(content) >= len(doctype) && bytes.EqualFold(content[:len(doctype)], []byte(doctype)) {
		return true
	}
	const root = "<html"
	if len(content) < len(root) || !bytes.EqualFold(content[:len(root)], []byte(root)) {
		return false
	}
	return len(content) == len(root) || content[len(root)] == '>' ||
		content[len(root)] == ' ' || content[len(root)] == '\t' ||
		content[len(root)] == '\r' || content[len(root)] == '\n'
}

const artifactContentSecurityPolicy = "sandbox allow-scripts; default-src 'none'; " +
	"script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; " +
	"font-src data:; connect-src 'none'; media-src 'none'; object-src 'none'; " +
	"frame-src 'none'; child-src 'none'; form-action 'none'; base-uri 'none'; " +
	"frame-ancestors 'self'"

func setArtifactSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", artifactContentSecurityPolicy)
	header.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "SAMEORIGIN")
	header.Set("Cache-Control", "no-store, max-age=0")
}

func (store *artifactStore) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	setArtifactSecurityHeaders(writer.Header())
	id := request.PathValue("id")
	if validateResourceID("artifact", id) != nil {
		http.NotFound(writer, request)
		return
	}
	response, found := store.beginResponse(id)
	if !found {
		http.NotFound(writer, request)
		return
	}
	defer store.finishResponse(&response)
	if err := validateResourceRevision(request, response.metadata.Version, response.metadata.Digest); err != nil {
		if errors.Is(err, errResourceRevisionMismatch) {
			http.Error(writer, "artifact revision no longer matches", http.StatusPreconditionFailed)
		} else {
			http.Error(writer, "invalid artifact revision", http.StatusBadRequest)
		}
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Content-Length", fmt.Sprint(len(response.html)))
	writer.Header().Set("X-OpenRealtime-Artifact-Version", fmt.Sprint(response.metadata.Version))
	writer.Header().Set("X-OpenRealtime-Artifact-Digest", response.metadata.Digest)
	writer.Header().Set("ETag", `"`+response.metadata.Digest+`"`)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(response.html)
	}
}

// ArtifactStoreFactory owns one bounded artifact store and its exact route.
type ArtifactStoreFactory struct{ descriptor plugin.Descriptor }

func NewArtifactStoreFactory() *ArtifactStoreFactory {
	schema := presentation.ArtifactStoreConfigContract
	return &ArtifactStoreFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.artifact-store", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:     []plugin.Contract{presentation.ArtifactStoreContract},
		Requires:     []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		ConfigSchema: &schema,
		Permissions: []plugin.Permission{{
			Kind: storagePermissionKind, Resource: artifactStorageResource,
			Operations: []string{storagePublishOperation},
		}},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}}
}

func (factory *ArtifactStoreFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *ArtifactStoreFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseResourceStoreConfig(raw, artifactStorePolicy)
	return err
}

func (factory *ArtifactStoreFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if !mount.Permissions.Allows(storagePermissionKind, artifactStorageResource, storagePublishOperation) {
		return errors.New("presentation artifact store lacks its deployment memory-publish grant")
	}
	limits, err := parseResourceStoreConfig(mount.Config, artifactStorePolicy)
	if err != nil {
		return err
	}
	store := newArtifactStore(limits, time.Now)
	if err := mount.Lifecycle.Defer("artifact-store", store.close); err != nil {
		return err
	}
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/artifacts/{id}", Handler: http.HandlerFunc(store.serveHTTP),
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.ArtifactStoreContract, ArtifactStore(store))
}

func lookupArtifactStore(services pluginruntime.Services) (ArtifactStore, error) {
	value, contract, _, _, found := services.Lookup(presentation.ArtifactStoreContract.Name)
	if !found || contract != presentation.ArtifactStoreContract {
		return nil, errors.New("presentation artifact store is unavailable")
	}
	store, ok := value.(ArtifactStore)
	if !ok || store == nil {
		return nil, errors.New("presentation artifact store has the wrong Go type")
	}
	return store, nil
}
