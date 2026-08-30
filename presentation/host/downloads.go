package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	DefaultMaxDownloadBytes      int64 = 8 << 20
	DefaultMaxDownloadTotalBytes int64 = 32 << 20
	DefaultMaxDownloads                = 64

	maximumDownloadBytes      int64 = 32 << 20
	maximumDownloadTotalBytes int64 = 128 << 20

	downloadStorageResource = "presentation-downloads"
)

var downloadStorePolicy = resourceStorePolicy{
	name: "presentation download store",
	defaults: ResourceStoreLimits{
		MaxEntries: DefaultMaxDownloads, MaxItemBytes: DefaultMaxDownloadBytes,
		MaxTotalBytes: DefaultMaxDownloadTotalBytes,
	},
	maximumItemBytes: maximumDownloadBytes, maximumTotalBytes: maximumDownloadTotalBytes,
}

// DownloadInput is one generated file admitted by an effect/tool provider.
// Content is cloned before Publish returns; paths are deliberately absent.
type DownloadInput struct {
	ID        string
	Filename  string
	MediaType string
	Content   []byte
}

// Download is the payload-free reference safe to expose to a client.
type Download struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MediaType string    `json:"media_type"`
	Path      string    `json:"path"`
	Digest    string    `json:"digest"`
	Bytes     int64     `json:"bytes"`
	Version   uint64    `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DownloadStore is a bounded memory-backed publication service. It cannot
// read a filesystem path and its route always forces attachment disposition.
type DownloadStore interface {
	Publish(context.Context, DownloadInput) (Download, error)
	Lookup(id string) (Download, bool)
	List() []Download
	Stats() ResourceStoreStats
}

type storedDownload struct {
	metadata Download
	content  []byte
}

type downloadStore struct {
	mu        sync.Mutex
	limits    ResourceStoreLimits
	entries   map[string]*storedDownload
	order     []string
	total     int64
	evictions uint64
	lifecycle resourceLifecycle
	clock     func() time.Time
}

func newDownloadStore(limits ResourceStoreLimits, clock func() time.Time) *downloadStore {
	if clock == nil {
		clock = time.Now
	}
	return &downloadStore{
		limits: limits, entries: make(map[string]*storedDownload),
		lifecycle: newResourceLifecycle(), clock: clock,
	}
}

func (store *downloadStore) Publish(ctx context.Context, input DownloadInput) (Download, error) {
	if ctx == nil {
		return Download{}, errors.New("publish download: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Download{}, fmt.Errorf("publish download: %w", err)
	}
	if err := validateResourceID("download", input.ID); err != nil {
		return Download{}, err
	}
	if err := validateDownloadFilename(input.Filename); err != nil {
		return Download{}, err
	}
	mediaType, err := canonicalMediaType(input.MediaType)
	if err != nil {
		return Download{}, err
	}
	store.mu.Lock()
	if store.lifecycle.closed {
		store.mu.Unlock()
		return Download{}, ErrResourceStoreClosed
	}
	maxItemBytes := store.limits.MaxItemBytes
	store.mu.Unlock()
	if len(input.Content) == 0 {
		return Download{}, errors.New("download content cannot be empty")
	}
	if int64(len(input.Content)) > maxItemBytes {
		return Download{}, fmt.Errorf("download has %d bytes; max_item_bytes is %d",
			len(input.Content), maxItemBytes)
	}
	content := slices.Clone(input.Content)
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
		return Download{}, ErrResourceStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return Download{}, fmt.Errorf("publish download: %w", err)
	}
	existing, updating := store.entries[input.ID]
	if updating && existing.metadata.Version == math.MaxUint64 {
		return Download{}, errors.New("download version is exhausted")
	}
	if err := store.makeRoomLocked(input.ID, int64(len(content)), !updating); err != nil {
		return Download{}, err
	}
	version := uint64(1)
	if updating {
		version = existing.metadata.Version + 1
		store.total -= int64(len(existing.content))
		wipe(existing.content)
	} else {
		existing = &storedDownload{}
		store.entries[input.ID] = existing
	}
	metadata := Download{
		ID: input.ID, Filename: input.Filename, MediaType: mediaType,
		Path: "/client/v1/downloads/" + input.ID, Digest: digest,
		Bytes: int64(len(content)), Version: version, UpdatedAt: store.clock().UTC(),
	}
	existing.metadata = metadata
	existing.content = content
	store.total += int64(len(content))
	store.moveToNewestLocked(input.ID)
	adopted = true
	return metadata, nil
}

func (store *downloadStore) makeRoomLocked(id string, bytes int64, adding bool) error {
	oldBytes := int64(0)
	if existing := store.entries[id]; existing != nil {
		oldBytes = int64(len(existing.content))
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
			return errors.New("download cannot fit within configured retention bounds")
		}
		entry := store.entries[victim]
		victimBytes := int64(len(entry.content))
		wipe(entry.content)
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

func (store *downloadStore) moveToNewestLocked(id string) {
	store.removeFromOrderLocked(id)
	store.order = append(store.order, id)
}

func (store *downloadStore) removeFromOrderLocked(id string) {
	for index, candidate := range store.order {
		if candidate == id {
			copy(store.order[index:], store.order[index+1:])
			store.order = store.order[:len(store.order)-1]
			return
		}
	}
}

func (store *downloadStore) Lookup(id string) (Download, bool) {
	if validateResourceID("download", id) != nil {
		return Download{}, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed {
		return Download{}, false
	}
	entry, found := store.entries[id]
	if !found {
		return Download{}, false
	}
	return entry.metadata, true
}

func (store *downloadStore) List() []Download {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed {
		return []Download{}
	}
	result := make([]Download, 0, len(store.order))
	for _, id := range store.order {
		result = append(result, store.entries[id].metadata)
	}
	return result
}

func (store *downloadStore) Stats() ResourceStoreStats {
	store.mu.Lock()
	defer store.mu.Unlock()
	return ResourceStoreStats{
		Limits: store.limits, Entries: len(store.entries), Bytes: store.total,
		Evictions: store.evictions, Closed: store.lifecycle.closed,
	}
}

type downloadResponse struct {
	metadata Download
	content  []byte
}

func (store *downloadStore) beginResponse(id string) (downloadResponse, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed || !store.lifecycle.beginLocked() {
		return downloadResponse{}, false
	}
	entry, found := store.entries[id]
	if !found {
		store.lifecycle.finishLocked()
		return downloadResponse{}, false
	}
	return downloadResponse{metadata: entry.metadata, content: slices.Clone(entry.content)}, true
}

func (store *downloadStore) finishResponse(response *downloadResponse) {
	wipe(response.content)
	response.content = nil
	store.mu.Lock()
	store.lifecycle.finishLocked()
	store.mu.Unlock()
}

func (store *downloadStore) close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close download store: nil context")
	}
	store.mu.Lock()
	drained := store.lifecycle.closeLocked()
	for _, entry := range store.entries {
		wipe(entry.content)
	}
	store.entries = nil
	store.order = nil
	store.total = 0
	store.mu.Unlock()
	return waitForResourceDrain(ctx, drained)
}

const downloadContentSecurityPolicy = "sandbox; default-src 'none'; object-src 'none'; " +
	"frame-src 'none'; child-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

func setDownloadSecurityHeaders(header http.Header) {
	header.Set("Content-Security-Policy", downloadContentSecurityPolicy)
	header.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Download-Options", "noopen")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cache-Control", "no-store, max-age=0")
}

func (store *downloadStore) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	setDownloadSecurityHeaders(writer.Header())
	id := request.PathValue("id")
	if validateResourceID("download", id) != nil {
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
			http.Error(writer, "download revision no longer matches", http.StatusPreconditionFailed)
		} else {
			http.Error(writer, "invalid download revision", http.StatusBadRequest)
		}
		return
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{
		"filename": response.metadata.Filename,
	})
	writer.Header().Set("Content-Type", response.metadata.MediaType)
	writer.Header().Set("Content-Disposition", disposition)
	writer.Header().Set("Content-Length", fmt.Sprint(len(response.content)))
	writer.Header().Set("X-OpenRealtime-Download-Version", fmt.Sprint(response.metadata.Version))
	writer.Header().Set("X-OpenRealtime-Download-Digest", response.metadata.Digest)
	writer.Header().Set("ETag", `"`+response.metadata.Digest+`"`)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(response.content)
	}
}

// DownloadStoreFactory owns one bounded generated-file store and route.
type DownloadStoreFactory struct{ descriptor plugin.Descriptor }

func NewDownloadStoreFactory() *DownloadStoreFactory {
	schema := presentation.DownloadStoreConfigContract
	return &DownloadStoreFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.download-store", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:     []plugin.Contract{presentation.DownloadStoreContract},
		Requires:     []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		ConfigSchema: &schema,
		Permissions: []plugin.Permission{{
			Kind: storagePermissionKind, Resource: downloadStorageResource,
			Operations: []string{storagePublishOperation},
		}},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}}
}

func (factory *DownloadStoreFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *DownloadStoreFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseResourceStoreConfig(raw, downloadStorePolicy)
	return err
}

func (factory *DownloadStoreFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if !mount.Permissions.Allows(storagePermissionKind, downloadStorageResource, storagePublishOperation) {
		return errors.New("presentation download store lacks its deployment memory-publish grant")
	}
	limits, err := parseResourceStoreConfig(mount.Config, downloadStorePolicy)
	if err != nil {
		return err
	}
	store := newDownloadStore(limits, time.Now)
	if err := mount.Lifecycle.Defer("download-store", store.close); err != nil {
		return err
	}
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/downloads/{id}", Handler: http.HandlerFunc(store.serveHTTP),
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.DownloadStoreContract, DownloadStore(store))
}

func lookupDownloadStore(services pluginruntime.Services) (DownloadStore, error) {
	value, contract, _, _, found := services.Lookup(presentation.DownloadStoreContract.Name)
	if !found || contract != presentation.DownloadStoreContract {
		return nil, errors.New("presentation download store is unavailable")
	}
	store, ok := value.(DownloadStore)
	if !ok || store == nil {
		return nil, errors.New("presentation download store has the wrong Go type")
	}
	return store, nil
}
