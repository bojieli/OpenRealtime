package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
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

const artifactStoreStateFormatVersion = 1

type artifactStoreState struct {
	FormatVersion uint64                    `json:"format_version"`
	Evictions     uint64                    `json:"evictions"`
	Entries       []artifactStoreStateEntry `json:"entries"`
}

type artifactStoreStateEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	HTML      string `json:"html"`
	Version   uint64 `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

type artifactStoreStateWire struct {
	FormatVersion *uint64                        `json:"format_version"`
	Evictions     *uint64                        `json:"evictions"`
	Entries       *[]artifactStoreStateEntryWire `json:"entries"`
}

type artifactStoreStateEntryWire struct {
	ID        *string `json:"id"`
	Title     *string `json:"title"`
	HTML      *string `json:"html"`
	Version   *uint64 `json:"version"`
	UpdatedAt *string `json:"updated_at"`
}

func decodeArtifactStoreState(
	raw json.RawMessage, limits ResourceStoreLimits,
) (artifactStoreState, error) {
	if len(raw) == 0 || len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return artifactStoreState{}, fmt.Errorf(
			"artifact store state must be 1-%d bytes",
			pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	if err := strictjson.ValidateWithLimits(raw, strictjson.Limits{
		MaxInputBytes: pluginruntime.MaximumStateSnapshotBytes,
		MaxDepth:      4, MaxTokens: 16 + 8*limits.MaxEntries,
		MaxObjectMembers: 5, MaxArrayElements: limits.MaxEntries,
		MaxKeyBytes: 32, MaxTotalKeyBytes: int64(64 + 64*limits.MaxEntries),
		MaxWorkBytes: 4 * pluginruntime.MaximumStateSnapshotBytes,
	}); err != nil {
		return artifactStoreState{}, fmt.Errorf("artifact store state: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire artifactStoreStateWire
	if err := decoder.Decode(&wire); err != nil {
		return artifactStoreState{}, fmt.Errorf("artifact store state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return artifactStoreState{}, errors.New("artifact store state has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return artifactStoreState{}, fmt.Errorf("artifact store state trailing data: %w", err)
	}
	if wire.FormatVersion == nil || wire.Evictions == nil || wire.Entries == nil ||
		*wire.FormatVersion != artifactStoreStateFormatVersion {
		return artifactStoreState{}, errors.New("artifact store state has missing or unsupported fields")
	}
	if len(*wire.Entries) > limits.MaxEntries {
		return artifactStoreState{}, errors.New("artifact store state exceeds max_entries")
	}
	state := artifactStoreState{
		FormatVersion: artifactStoreStateFormatVersion,
		Evictions:     *wire.Evictions,
		Entries:       make([]artifactStoreStateEntry, 0, len(*wire.Entries)),
	}
	seen := make(map[string]struct{}, len(*wire.Entries))
	var total int64
	for index, entry := range *wire.Entries {
		if entry.ID == nil || entry.Title == nil || entry.HTML == nil ||
			entry.Version == nil || entry.UpdatedAt == nil {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d has missing fields", index)
		}
		if err := validateResourceID("artifact", *entry.ID); err != nil {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d: %w", index, err)
		}
		if _, duplicate := seen[*entry.ID]; duplicate {
			return artifactStoreState{}, fmt.Errorf("artifact store state repeats id %q", *entry.ID)
		}
		seen[*entry.ID] = struct{}{}
		title, err := validateArtifactTitle(*entry.Title)
		if err != nil || title == "" || title != *entry.Title {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d has an invalid title", index)
		}
		if !utf8.ValidString(*entry.HTML) || strings.TrimSpace(*entry.HTML) == "" ||
			int64(len(*entry.HTML)) > limits.MaxItemBytes {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d has invalid HTML", index)
		}
		if int64(len(*entry.HTML)) > limits.MaxTotalBytes-total {
			return artifactStoreState{}, errors.New("artifact store state exceeds max_total_bytes")
		}
		total += int64(len(*entry.HTML))
		if *entry.Version == 0 {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d has an invalid version", index)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, *entry.UpdatedAt)
		if err != nil || updatedAt.IsZero() || *entry.UpdatedAt != updatedAt.UTC().Format(time.RFC3339Nano) {
			return artifactStoreState{}, fmt.Errorf("artifact store state entry %d has an invalid timestamp", index)
		}
		state.Entries = append(state.Entries, artifactStoreStateEntry{
			ID: *entry.ID, Title: title, HTML: *entry.HTML,
			Version: *entry.Version, UpdatedAt: *entry.UpdatedAt,
		})
	}
	return state, nil
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

func (store *artifactStore) snapshot(ctx context.Context) (json.RawMessage, error) {
	if ctx == nil {
		return nil, errors.New("snapshot artifact store: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot artifact store: %w", err)
	}
	store.mu.Lock()
	if store.lifecycle.closed {
		store.mu.Unlock()
		return nil, ErrResourceStoreClosed
	}
	if store.total >= int64(pluginruntime.MaximumStateSnapshotBytes) {
		entries, total := len(store.entries), store.total
		store.mu.Unlock()
		return nil, fmt.Errorf(
			"artifact store state with %d entries and %d content bytes exceeds the %d-byte migration envelope",
			entries, total, pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	state := artifactStoreState{
		FormatVersion: artifactStoreStateFormatVersion,
		Evictions:     store.evictions,
		Entries:       make([]artifactStoreStateEntry, 0, len(store.order)),
	}
	for _, id := range store.order {
		entry := store.entries[id]
		state.Entries = append(state.Entries, artifactStoreStateEntry{
			ID: entry.metadata.ID, Title: entry.metadata.Title, HTML: string(entry.html),
			Version:   entry.metadata.Version,
			UpdatedAt: entry.metadata.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("snapshot artifact store: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("snapshot artifact store: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"artifact store state is %d bytes; migration limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	return raw, nil
}

func (store *artifactStore) restore(state artifactStoreState) error {
	entries := make(map[string]*storedArtifact, len(state.Entries))
	order := make([]string, 0, len(state.Entries))
	var total int64
	for _, row := range state.Entries {
		content := []byte(row.HTML)
		updatedAt, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		if err != nil {
			for _, entry := range entries {
				wipe(entry.html)
			}
			return fmt.Errorf("restore artifact store timestamp: %w", err)
		}
		entries[row.ID] = &storedArtifact{
			metadata: Artifact{
				ID: row.ID, Title: row.Title, Path: "/client/v1/artifacts/" + row.ID,
				Digest: contentDigest(content), Bytes: int64(len(content)),
				Version: row.Version, UpdatedAt: updatedAt,
			},
			html: content,
		}
		order = append(order, row.ID)
		total += int64(len(content))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lifecycle.closed || len(store.entries) != 0 || len(store.order) != 0 || store.total != 0 {
		for _, entry := range entries {
			wipe(entry.html)
		}
		return errors.New("artifact store cannot restore into a nonempty or closed instance")
	}
	store.entries = entries
	store.order = order
	store.total = total
	store.evictions = state.Evictions
	return nil
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
	stateSchema := presentation.ArtifactStoreStateContract
	return &ArtifactStoreFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.artifact-store", Revision: 2,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:     []plugin.Contract{presentation.ArtifactStoreContract},
		Requires:     []plugin.Requirement{{Contract: presentation.HTTPRoutesContract}},
		ConfigSchema: &schema,
		StateSchema:  &stateSchema,
		Permissions: []plugin.Permission{{
			Kind: storagePermissionKind, Resource: artifactStorageResource,
			Operations: []string{storagePublishOperation},
		}},
		Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true, DisposeTimeoutMS: 5_000},
	}}
}

func (factory *ArtifactStoreFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *ArtifactStoreFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseResourceStoreConfig(raw, artifactStorePolicy)
	return err
}

func (factory *ArtifactStoreFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	limits, err := parseResourceStoreConfig(mount.Config, artifactStorePolicy)
	if err != nil {
		return err
	}
	return (artifactStoreCandidate{entryID: mount.EntryID, limits: limits}).Activate(ctx, mount)
}

func (factory *ArtifactStoreFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if !candidate.Permissions.Allows(
		storagePermissionKind, artifactStorageResource, storagePublishOperation,
	) {
		return nil, errors.New("presentation artifact store lacks its deployment memory-publish grant")
	}
	if _, err := lookupRoutes(candidate.Services); err != nil {
		return nil, err
	}
	limits, err := parseResourceStoreConfig(candidate.Config, artifactStorePolicy)
	if err != nil {
		return nil, err
	}
	return artifactStoreCandidate{entryID: candidate.EntryID, limits: limits}, nil
}

type artifactStoreCandidate struct {
	entryID string
	limits  ResourceStoreLimits
}

func (candidate artifactStoreCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if mount.EntryID != candidate.entryID {
		return errors.New("artifact store candidate entry changed before activation")
	}
	if !mount.Permissions.Allows(storagePermissionKind, artifactStorageResource, storagePublishOperation) {
		return errors.New("presentation artifact store lacks its deployment memory-publish grant")
	}
	limits, err := parseResourceStoreConfig(mount.Config, artifactStorePolicy)
	if err != nil {
		return err
	}
	if limits != candidate.limits {
		return errors.New("artifact store candidate limits changed before activation")
	}
	if mount.State == nil {
		return errors.New("presentation artifact store state lifecycle is unavailable")
	}
	restored, available, err := mount.State.Restored()
	if err != nil {
		return err
	}
	store := newArtifactStore(limits, time.Now)
	if err := mount.Lifecycle.Defer("artifact-store", store.close); err != nil {
		return err
	}
	if available {
		state, err := decodeArtifactStoreState(restored, limits)
		if err != nil {
			return err
		}
		if err := store.restore(state); err != nil {
			return err
		}
	}
	if err := mount.State.Snapshot(store.snapshot); err != nil {
		return err
	}
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/artifacts/{id}", Handler: http.HandlerFunc(store.serveHTTP),
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.ArtifactStoreContract, ArtifactStore(store))
}

func (candidate artifactStoreCandidate) MigrateState(
	_ context.Context, migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	if migration.EntryID != candidate.entryID ||
		migration.Schema != presentation.ArtifactStoreStateContract ||
		migration.SourceImplementation == "" ||
		migration.SourceImplementation != strings.TrimSpace(migration.SourceImplementation) {
		return nil, errors.New("artifact store state migration identity is invalid")
	}
	state, err := decodeArtifactStoreState(migration.Snapshot, candidate.limits)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("migrate artifact store state: %w", err)
	}
	if len(raw) > pluginruntime.MaximumStateSnapshotBytes {
		return nil, fmt.Errorf(
			"migrated artifact store state is %d bytes; limit is %d",
			len(raw), pluginruntime.MaximumStateSnapshotBytes,
		)
	}
	return raw, nil
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

var _ pluginruntime.Factory = (*ArtifactStoreFactory)(nil)
var _ pluginruntime.ConfigValidator = (*ArtifactStoreFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*ArtifactStoreFactory)(nil)
var _ pluginruntime.CandidateStateMigrator = artifactStoreCandidate{}
