package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
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

const downloadStoreStateFormatVersion = 1

type downloadStoreState struct {
	FormatVersion uint64                    `json:"format_version"`
	Evictions     uint64                    `json:"evictions"`
	Entries       []downloadStoreStateEntry `json:"entries"`
}

type downloadStoreStateEntry struct {
	ID        string `json:"id"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Content   []byte `json:"content"`
	Version   uint64 `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

type downloadStoreStateWire struct {
	FormatVersion *uint64                        `json:"format_version"`
	Evictions     *uint64                        `json:"evictions"`
	Entries       *[]downloadStoreStateEntryWire `json:"entries"`
}

type downloadStoreStateEntryWire struct {
	ID        *string `json:"id"`
	Filename  *string `json:"filename"`
	MediaType *string `json:"media_type"`
	Content   *string `json:"content"`
	Version   *uint64 `json:"version"`
	UpdatedAt *string `json:"updated_at"`
}

func decodeDownloadStoreState(
	raw json.RawMessage, limits ResourceStoreLimits,
) (downloadStoreState, error) {
	if len(raw) == 0 || len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return downloadStoreState{}, fmt.Errorf(
			"download store state must be 1-%d bytes",
			pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	if err := strictjson.ValidateWithLimits(raw, strictjson.Limits{
		MaxInputBytes: pluginruntime.MaximumStateSnapshotBytes,
		MaxDepth:      4, MaxTokens: 16 + 9*limits.MaxEntries,
		MaxObjectMembers: 6, MaxArrayElements: limits.MaxEntries,
		MaxKeyBytes: 32, MaxTotalKeyBytes: int64(64 + 80*limits.MaxEntries),
		MaxWorkBytes: 4 * pluginruntime.MaximumStateSnapshotBytes,
	}); err != nil {
		return downloadStoreState{}, fmt.Errorf("download store state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire downloadStoreStateWire
	if err := decoder.Decode(&wire); err != nil {
		return downloadStoreState{}, fmt.Errorf("download store state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return downloadStoreState{}, errors.New("download store state has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return downloadStoreState{}, fmt.Errorf("download store state trailing data: %w", err)
	}
	if wire.FormatVersion == nil || wire.Evictions == nil || wire.Entries == nil ||
		*wire.FormatVersion != downloadStoreStateFormatVersion {
		return downloadStoreState{}, errors.New("download store state has missing or unsupported fields")
	}
	if len(*wire.Entries) > limits.MaxEntries {
		return downloadStoreState{}, errors.New("download store state exceeds max_entries")
	}
	state := downloadStoreState{
		FormatVersion: downloadStoreStateFormatVersion,
		Evictions:     *wire.Evictions,
		Entries:       make([]downloadStoreStateEntry, 0, len(*wire.Entries)),
	}
	keepContent := false
	defer func() {
		if !keepContent {
			wipeDownloadStoreState(&state)
		}
	}()
	seen := make(map[string]struct{}, len(*wire.Entries))
	var total int64
	for index, entry := range *wire.Entries {
		if entry.ID == nil || entry.Filename == nil || entry.MediaType == nil ||
			entry.Content == nil || entry.Version == nil || entry.UpdatedAt == nil {
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has missing fields", index)
		}
		if err := validateResourceID("download", *entry.ID); err != nil {
			return downloadStoreState{}, fmt.Errorf("download store state entry %d: %w", index, err)
		}
		if _, duplicate := seen[*entry.ID]; duplicate {
			return downloadStoreState{}, fmt.Errorf("download store state repeats id %q", *entry.ID)
		}
		seen[*entry.ID] = struct{}{}
		if err := validateDownloadFilename(*entry.Filename); err != nil {
			return downloadStoreState{}, fmt.Errorf("download store state entry %d: %w", index, err)
		}
		mediaType, err := canonicalMediaType(*entry.MediaType)
		if err != nil || mediaType != *entry.MediaType {
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has an invalid media type", index)
		}
		content, err := base64.StdEncoding.Strict().DecodeString(*entry.Content)
		if err != nil || base64.StdEncoding.EncodeToString(content) != *entry.Content {
			wipe(content)
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has invalid base64 content", index)
		}
		if len(content) == 0 || int64(len(content)) > limits.MaxItemBytes {
			wipe(content)
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has invalid content", index)
		}
		if int64(len(content)) > limits.MaxTotalBytes-total {
			wipe(content)
			return downloadStoreState{}, errors.New("download store state exceeds max_total_bytes")
		}
		total += int64(len(content))
		if *entry.Version == 0 {
			wipe(content)
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has an invalid version", index)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, *entry.UpdatedAt)
		if err != nil || updatedAt.IsZero() || *entry.UpdatedAt != updatedAt.UTC().Format(time.RFC3339Nano) {
			wipe(content)
			return downloadStoreState{}, fmt.Errorf("download store state entry %d has an invalid timestamp", index)
		}
		state.Entries = append(state.Entries, downloadStoreStateEntry{
			ID: *entry.ID, Filename: *entry.Filename, MediaType: mediaType,
			Content: content, Version: *entry.Version, UpdatedAt: *entry.UpdatedAt,
		})
	}
	keepContent = true
	return state, nil
}

func wipeDownloadStoreState(state *downloadStoreState) {
	if state == nil {
		return
	}
	for index := range state.Entries {
		wipe(state.Entries[index].Content)
		state.Entries[index].Content = nil
	}
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

func (store *downloadStore) snapshot(ctx context.Context) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("snapshot download store: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot download store: %w", err)
	}
	store.mu.Lock()
	encodedContentBytes := int64(4) * ((store.total + 2) / 3)
	if store.lifecycle.closed {
		store.mu.Unlock()
		return nil, ErrResourceStoreClosed
	}
	if encodedContentBytes >= int64(pluginruntime.MaximumStateSnapshotBytes) {
		entries, total := len(store.entries), store.total
		store.mu.Unlock()
		return nil, fmt.Errorf(
			"download store state with %d entries and %d content bytes exceeds the %d-byte migration envelope",
			entries, total, pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	state := downloadStoreState{
		FormatVersion: downloadStoreStateFormatVersion,
		Evictions:     store.evictions,
		Entries:       make([]downloadStoreStateEntry, 0, len(store.order)),
	}
	for _, id := range store.order {
		entry := store.entries[id]
		state.Entries = append(state.Entries, downloadStoreStateEntry{
			ID: entry.metadata.ID, Filename: entry.metadata.Filename,
			MediaType: entry.metadata.MediaType, Content: slices.Clone(entry.content),
			Version:   entry.metadata.Version,
			UpdatedAt: entry.metadata.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	store.mu.Unlock()
	defer wipeDownloadStoreState(&state)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot download store: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("snapshot download store: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"download store state is %d bytes; migration limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	return raw, nil
}

func (store *downloadStore) restore(state downloadStoreState) error {
	entries := make(map[string]*storedDownload, len(state.Entries))
	order := make([]string, 0, len(state.Entries))
	var total int64
	for _, row := range state.Entries {
		updatedAt, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		if err != nil {
			for _, entry := range entries {
				wipe(entry.content)
			}
			return fmt.Errorf("restore download store timestamp: %w", err)
		}
		entries[row.ID] = &storedDownload{
			metadata: Download{
				ID: row.ID, Filename: row.Filename, MediaType: row.MediaType,
				Path:   "/client/v1/downloads/" + row.ID,
				Digest: contentDigest(row.Content), Bytes: int64(len(row.Content)),
				Version: row.Version, UpdatedAt: updatedAt,
			},
			content: row.Content,
		}
		order = append(order, row.ID)
		total += int64(len(row.Content))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed || len(store.entries) != 0 || len(store.order) != 0 || store.total != 0 {
		for _, entry := range entries {
			wipe(entry.content)
		}
		return errors.New("download store cannot restore into a nonempty or closed instance")
	}
	store.entries = entries
	store.order = order
	store.total = total
	store.evictions = state.Evictions
	return nil
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
	stateSchema := presentation.DownloadStoreStateContract
	return &DownloadStoreFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.download-store", Revision: 2,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:     []plugin.Contract{presentation.DownloadStoreContract},
		Requires:     []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		ConfigSchema: &schema,
		StateSchema:  &stateSchema,
		Permissions: []plugin.Permission{{
			Kind: storagePermissionKind, Resource: downloadStorageResource,
			Operations: []string{storagePublishOperation},
		}},
		Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true, DisposeTimeoutMS: 5_000},
	}}
}

func (factory *DownloadStoreFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *DownloadStoreFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseResourceStoreConfig(raw, downloadStorePolicy)
	return err
}

func (factory *DownloadStoreFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	limits, err := parseResourceStoreConfig(mount.Config, downloadStorePolicy)
	if err != nil {
		return err
	}
	return (downloadStoreCandidate{entryID: mount.EntryID, limits: limits}).Activate(ctx, mount)
}

func (factory *DownloadStoreFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if !candidate.Permissions.Allows(
		storagePermissionKind, downloadStorageResource, storagePublishOperation,
	) {
		return nil, errors.New("presentation download store lacks its deployment memory-publish grant")
	}
	if _, err := lookupRoutes(candidate.Services); err != nil {
		return nil, err
	}
	limits, err := parseResourceStoreConfig(candidate.Config, downloadStorePolicy)
	if err != nil {
		return nil, err
	}
	return downloadStoreCandidate{entryID: candidate.EntryID, limits: limits}, nil
}

type downloadStoreCandidate struct {
	entryID string
	limits  ResourceStoreLimits
}

func (candidate downloadStoreCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if mount.EntryID != candidate.entryID {
		return errors.New("download store candidate entry changed before activation")
	}
	if !mount.Permissions.Allows(storagePermissionKind, downloadStorageResource, storagePublishOperation) {
		return errors.New("presentation download store lacks its deployment memory-publish grant")
	}
	limits, err := parseResourceStoreConfig(mount.Config, downloadStorePolicy)
	if err != nil {
		return err
	}
	if limits != candidate.limits {
		return errors.New("download store candidate limits changed before activation")
	}
	if mount.State == nil {
		return errors.New("presentation download store state lifecycle is unavailable")
	}
	restored, available, err := mount.State.Restored()
	if err != nil {
		return err
	}
	store := newDownloadStore(limits, time.Now)
	if err := mount.Lifecycle.Defer("download-store", store.close); err != nil {
		return err
	}
	if available {
		state, err := decodeDownloadStoreState(restored, limits)
		if err != nil {
			return err
		}
		if err := store.restore(state); err != nil {
			wipeDownloadStoreState(&state)
			return err
		}
	}
	if err := mount.State.Snapshot(store.snapshot); err != nil {
		return err
	}
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/downloads/{id}", Handler: http.HandlerFunc(store.serveHTTP),
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.DownloadStoreContract, DownloadStore(store))
}

func (candidate downloadStoreCandidate) MigrateState(
	_ context.Context, migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	if migration.EntryID != candidate.entryID ||
		migration.Schema != presentation.DownloadStoreStateContract ||
		migration.SourceImplementation == "" ||
		migration.SourceImplementation != strings.TrimSpace(migration.SourceImplementation) {
		return nil, errors.New("download store state migration identity is invalid")
	}
	state, err := decodeDownloadStoreState(migration.Snapshot, candidate.limits)
	if err != nil {
		return nil, err
	}
	defer wipeDownloadStoreState(&state)
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("migrate download store state: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"migrated download store state is %d bytes; limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	return raw, nil
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

var _ pluginruntime.Factory = (*DownloadStoreFactory)(nil)
var _ pluginruntime.ConfigValidator = (*DownloadStoreFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*DownloadStoreFactory)(nil)
var _ pluginruntime.CandidateStateMigrator = downloadStoreCandidate{}
