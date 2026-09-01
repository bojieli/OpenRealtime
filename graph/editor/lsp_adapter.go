package editor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
)

var (
	ErrLSPDisposed        = errors.New("LSP adapter is disposed")
	ErrLSPNotInitialized  = errors.New("LSP adapter is not initialized")
	ErrLSPDocumentOpen    = errors.New("LSP document is already open")
	ErrLSPDocumentMissing = errors.New("LSP document is not open")
	ErrLSPVersion         = errors.New("LSP document version is stale")
)

const LSPDocumentLanguageID = "openrealtime-ortg"

// LSPAdapterLimits independently bounds protocol work around the editor's own
// source/catalog/result limits. Zero fields select defaults.
type LSPAdapterLimits struct {
	MaxRequestBytes  int `json:"max_request_bytes"`
	MaxResponseBytes int `json:"max_response_bytes"`
	MaxDocuments     int `json:"max_documents"`
	MaxURIBytes      int `json:"max_uri_bytes"`
	MaxMethodBytes   int `json:"max_method_bytes"`
	MaxIDBytes       int `json:"max_id_bytes"`
}

// LSPAdapterOptions configures an in-memory, transport-neutral adapter. Editor
// options may resolve schemas but expose no file, network, process, or runtime
// mounting operation through this API.
type LSPAdapterOptions struct {
	Editor    Options          `json:"editor"`
	Limits    LSPAdapterLimits `json:"limits"`
	Workspace WorkspaceLimits  `json:"workspace"`
}

type lspAdapterPhase uint8

const (
	lspAdapterCreated lspAdapterPhase = iota + 1
	lspAdapterReady
	lspAdapterShutdown
	lspAdapterDisposed
)

type lspOpenDocument struct {
	URI      string
	Version  int
	Snapshot *Document
}

// LSPAdapter owns only immutable in-memory Document snapshots. HandleJSONRPC
// is transport-neutral: a caller supplies one complete JSON-RPC message and
// decides how bytes are framed or carried.
type LSPAdapter struct {
	mu               sync.RWMutex
	catalog          *resolve.Catalog
	editorOptions    Options
	limits           LSPAdapterLimits
	workspace        WorkspaceLimits
	phase            lspAdapterPhase
	documents        map[string]lspOpenDocument
	workspaceBytes   int
	workspaceImports int
}

func NewLSPAdapter(catalog *resolve.Catalog, options LSPAdapterOptions) (*LSPAdapter, error) {
	if catalog == nil {
		return nil, errors.New("LSP adapter requires a descriptor catalog")
	}
	editorLimits, err := normalizeLimits(options.Editor.Limits)
	if err != nil {
		return nil, err
	}
	options.Editor.Limits = editorLimits
	limits, err := normalizeLSPAdapterLimits(options.Limits)
	if err != nil {
		return nil, err
	}
	if limits.MaxRequestBytes < editorLimits.MaxSourceBytes+1024 {
		return nil, fmt.Errorf(
			"LSP request bound %d cannot carry the configured %d-byte source bound",
			limits.MaxRequestBytes, editorLimits.MaxSourceBytes,
		)
	}
	if limits.MaxResponseBytes < limits.MaxIDBytes+1024 {
		return nil, fmt.Errorf(
			"LSP response bound %d cannot carry the configured %d-byte id bound",
			limits.MaxResponseBytes, limits.MaxIDBytes,
		)
	}
	workspaceConfiguration := options.Workspace
	if workspaceConfiguration.MaxDocuments == 0 {
		workspaceConfiguration.MaxDocuments = limits.MaxDocuments
	}
	if workspaceConfiguration.MaxTotalSourceBytes == 0 {
		maximum := int64(limits.MaxDocuments) * int64(editorLimits.MaxSourceBytes)
		if maximum < 128<<20 {
			maximum = 128 << 20
		}
		if maximum > 1<<30 {
			maximum = 1 << 30
		}
		workspaceConfiguration.MaxTotalSourceBytes = int(maximum)
	}
	if workspaceConfiguration.MaxImports == 0 {
		maximum := int64(limits.MaxDocuments) * 64
		if maximum < 8192 {
			maximum = 8192
		}
		if maximum > 1<<20 {
			maximum = 1 << 20
		}
		workspaceConfiguration.MaxImports = int(maximum)
	}
	workspace, err := normalizeWorkspaceLimits(workspaceConfiguration)
	if err != nil {
		return nil, err
	}
	if workspace.MaxDocuments < limits.MaxDocuments {
		return nil, fmt.Errorf(
			"editor workspace document bound %d is smaller than the LSP adapter bound %d",
			workspace.MaxDocuments, limits.MaxDocuments,
		)
	}
	if workspace.MaxPathBytes < editorLimits.MaxPathBytes {
		return nil, fmt.Errorf(
			"editor workspace path bound %d is smaller than the editor path bound %d",
			workspace.MaxPathBytes, editorLimits.MaxPathBytes,
		)
	}
	frozen, err := freezeLSPCatalog(catalog, editorLimits)
	if err != nil {
		return nil, fmt.Errorf("freeze LSP descriptor catalog: %w", err)
	}
	return &LSPAdapter{
		catalog: frozen, editorOptions: options.Editor, limits: limits, workspace: workspace,
		phase: lspAdapterCreated, documents: make(map[string]lspOpenDocument),
	}, nil
}

func normalizeLSPAdapterLimits(value LSPAdapterLimits) (LSPAdapterLimits, error) {
	defaults := LSPAdapterLimits{
		MaxRequestBytes: 8 << 20, MaxResponseBytes: 65 << 20,
		MaxDocuments: 128, MaxURIBytes: 64 << 10,
		MaxMethodBytes: 256, MaxIDBytes: 256,
	}
	fields := []struct {
		name string
		set  *int
		def  int
		min  int
		hard int
	}{
		{"max_request_bytes", &value.MaxRequestBytes, defaults.MaxRequestBytes, 1024, 128 << 20},
		{"max_response_bytes", &value.MaxResponseBytes, defaults.MaxResponseBytes, 1024, 128 << 20},
		{"max_documents", &value.MaxDocuments, defaults.MaxDocuments, 1, 4096},
		{"max_uri_bytes", &value.MaxURIBytes, defaults.MaxURIBytes, 3, 64 << 10},
		{"max_method_bytes", &value.MaxMethodBytes, defaults.MaxMethodBytes,
			len("openrealtime/virtualDocument"), 4096},
		{"max_id_bytes", &value.MaxIDBytes, defaults.MaxIDBytes, 1, 4096},
	}
	for _, field := range fields {
		if *field.set == 0 {
			*field.set = field.def
		}
		if *field.set < field.min || *field.set > field.hard {
			return LSPAdapterLimits{}, fmt.Errorf(
				"LSP adapter %s must be between %d and %d", field.name, field.min, field.hard,
			)
		}
	}
	return value, nil
}

func freezeLSPCatalog(source *resolve.Catalog, limits Limits) (*resolve.Catalog, error) {
	var previous []catalogSnapshot
	for attempt := 0; attempt < 4; attempt++ {
		current, err := captureCatalog(source, limits)
		if err != nil {
			return nil, err
		}
		if previous != nil && equalCatalogSnapshots(previous, current) {
			result := resolve.NewCatalog()
			for _, captured := range current {
				if err := result.Register(captured.descriptor); err != nil {
					return nil, err
				}
			}
			return result, nil
		}
		previous = current
	}
	return nil, errors.New("descriptor catalog changed continuously while the LSP adapter froze it")
}

func (adapter *LSPAdapter) Dispose() {
	if adapter == nil {
		return
	}
	adapter.mu.Lock()
	adapter.phase = lspAdapterDisposed
	clear(adapter.documents)
	adapter.workspaceBytes = 0
	adapter.workspaceImports = 0
	adapter.mu.Unlock()
}

func (adapter *LSPAdapter) DocumentCount() int {
	if adapter == nil {
		return 0
	}
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	return len(adapter.documents)
}

func (adapter *LSPAdapter) initialize() error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.phase == lspAdapterDisposed {
		return ErrLSPDisposed
	}
	if adapter.phase != lspAdapterCreated {
		return errors.New("LSP adapter was already initialized")
	}
	adapter.phase = lspAdapterReady
	return nil
}

func (adapter *LSPAdapter) shutdown() error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.phase == lspAdapterDisposed {
		return ErrLSPDisposed
	}
	if adapter.phase != lspAdapterReady {
		return ErrLSPNotInitialized
	}
	adapter.phase = lspAdapterShutdown
	clear(adapter.documents)
	adapter.workspaceBytes = 0
	adapter.workspaceImports = 0
	return nil
}

func (adapter *LSPAdapter) requireReady() error {
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	return adapter.requireReadyLocked()
}

func (adapter *LSPAdapter) requireReadyLocked() error {
	switch adapter.phase {
	case lspAdapterReady:
		return nil
	case lspAdapterDisposed:
		return ErrLSPDisposed
	default:
		return ErrLSPNotInitialized
	}
}

func (adapter *LSPAdapter) analyze(
	ctx context.Context, uri string, text string,
) (*Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(text) {
		return nil, errors.New("LSP document text is not UTF-8")
	}
	return AnalyzeWithOptions(
		ctx, uri, []byte(text), adapter.catalog, adapter.editorOptions,
	)
}

func (adapter *LSPAdapter) openDocument(
	ctx context.Context, uri string, languageID string, version int, text string,
) error {
	if err := adapter.validateURI(uri); err != nil {
		return err
	}
	if languageID != LSPDocumentLanguageID {
		return fmt.Errorf("LSP document language %q is unsupported", languageID)
	}
	if !validLSPVersion(version) {
		return fmt.Errorf("%w: invalid initial version %d", ErrLSPVersion, version)
	}
	if len(text) > adapter.editorOptions.Limits.MaxSourceBytes {
		return fmt.Errorf("LSP document exceeds the source byte bound")
	}
	if err := adapter.requireReady(); err != nil {
		return err
	}
	snapshot, err := adapter.analyze(ctx, uri, text)
	if err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := adapter.requireReadyLocked(); err != nil {
		return err
	}
	if _, exists := adapter.documents[uri]; exists {
		return ErrLSPDocumentOpen
	}
	if len(adapter.documents) >= adapter.limits.MaxDocuments {
		return fmt.Errorf("LSP adapter document limit %d reached", adapter.limits.MaxDocuments)
	}
	imports := len(snapshot.file.Imports)
	if len(snapshot.source) > adapter.workspace.MaxTotalSourceBytes-adapter.workspaceBytes {
		return fmt.Errorf("%w: open documents exceed %d source bytes",
			ErrWorkspaceLimit, adapter.workspace.MaxTotalSourceBytes)
	}
	if imports > adapter.workspace.MaxImports-adapter.workspaceImports {
		return fmt.Errorf("%w: open documents exceed %d imports",
			ErrWorkspaceLimit, adapter.workspace.MaxImports)
	}
	adapter.documents[uri] = lspOpenDocument{URI: uri, Version: version, Snapshot: snapshot}
	adapter.workspaceBytes += len(snapshot.source)
	adapter.workspaceImports += imports
	return nil
}

func (adapter *LSPAdapter) changeDocument(
	ctx context.Context, uri string, version int, text string,
) error {
	if err := adapter.validateURI(uri); err != nil {
		return err
	}
	if !validLSPVersion(version) {
		return fmt.Errorf("%w: invalid changed version %d", ErrLSPVersion, version)
	}
	if len(text) > adapter.editorOptions.Limits.MaxSourceBytes {
		return fmt.Errorf("LSP document exceeds the source byte bound")
	}
	current, err := adapter.document(uri)
	if err != nil {
		return err
	}
	if version <= current.Version {
		return fmt.Errorf("%w: changed version %d is not newer than %d",
			ErrLSPVersion, version, current.Version)
	}
	snapshot, err := adapter.analyze(ctx, uri, text)
	if err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := adapter.requireReadyLocked(); err != nil {
		return err
	}
	latest, found := adapter.documents[uri]
	if !found {
		return ErrLSPDocumentMissing
	}
	if latest.Version != current.Version || latest.Snapshot != current.Snapshot {
		return fmt.Errorf("%w: document advanced during analysis", ErrLSPVersion)
	}
	prospectiveBytes := adapter.workspaceBytes - len(latest.Snapshot.source) + len(snapshot.source)
	prospectiveImports := adapter.workspaceImports - len(latest.Snapshot.file.Imports) + len(snapshot.file.Imports)
	if prospectiveBytes < 0 || prospectiveBytes > adapter.workspace.MaxTotalSourceBytes {
		return fmt.Errorf("%w: changed documents exceed %d source bytes",
			ErrWorkspaceLimit, adapter.workspace.MaxTotalSourceBytes)
	}
	if prospectiveImports < 0 || prospectiveImports > adapter.workspace.MaxImports {
		return fmt.Errorf("%w: changed documents exceed %d imports",
			ErrWorkspaceLimit, adapter.workspace.MaxImports)
	}
	adapter.documents[uri] = lspOpenDocument{URI: uri, Version: version, Snapshot: snapshot}
	adapter.workspaceBytes = prospectiveBytes
	adapter.workspaceImports = prospectiveImports
	return nil
}

func (adapter *LSPAdapter) closeDocument(uri string) error {
	if err := adapter.validateURI(uri); err != nil {
		return err
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := adapter.requireReadyLocked(); err != nil {
		return err
	}
	document, found := adapter.documents[uri]
	if !found {
		return ErrLSPDocumentMissing
	}
	sourceBytes, imports := len(document.Snapshot.source), len(document.Snapshot.file.Imports)
	if sourceBytes > adapter.workspaceBytes || imports > adapter.workspaceImports {
		return errors.New("LSP adapter workspace accounting underflow")
	}
	delete(adapter.documents, uri)
	adapter.workspaceBytes -= sourceBytes
	adapter.workspaceImports -= imports
	return nil
}

func (adapter *LSPAdapter) document(uri string) (lspOpenDocument, error) {
	if err := adapter.validateURI(uri); err != nil {
		return lspOpenDocument{}, err
	}
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	if err := adapter.requireReadyLocked(); err != nil {
		return lspOpenDocument{}, err
	}
	document, found := adapter.documents[uri]
	if !found {
		return lspOpenDocument{}, ErrLSPDocumentMissing
	}
	return document, nil
}

func (adapter *LSPAdapter) current(document lspOpenDocument) bool {
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	latest, found := adapter.documents[document.URI]
	return adapter.phase == lspAdapterReady && found &&
		latest.Version == document.Version && latest.Snapshot == document.Snapshot
}

func (adapter *LSPAdapter) workspaceIndex() (
	*WorkspaceIndex, []lspOpenDocument, error,
) {
	adapter.mu.RLock()
	if err := adapter.requireReadyLocked(); err != nil {
		adapter.mu.RUnlock()
		return nil, nil, err
	}
	keys := make([]string, 0, len(adapter.documents))
	for uri := range adapter.documents {
		keys = append(keys, uri)
	}
	sort.Strings(keys)
	leases := make([]lspOpenDocument, len(keys))
	documents := make([]WorkspaceDocument, len(keys))
	for position, uri := range keys {
		lease := adapter.documents[uri]
		leases[position] = lease
		documents[position] = WorkspaceDocument{
			Identity: lease.identity(), Snapshot: lease.Snapshot,
		}
	}
	adapter.mu.RUnlock()
	index, err := NewWorkspaceIndex(documents, adapter.workspace)
	if err != nil {
		return nil, leases, err
	}
	return index, leases, nil
}

func (adapter *LSPAdapter) workspaceCurrent(documents []lspOpenDocument) bool {
	adapter.mu.RLock()
	defer adapter.mu.RUnlock()
	if adapter.phase != lspAdapterReady || len(adapter.documents) != len(documents) {
		return false
	}
	for _, document := range documents {
		latest, found := adapter.documents[document.URI]
		if !found || latest.Version != document.Version || latest.Snapshot != document.Snapshot {
			return false
		}
	}
	return true
}

func (document lspOpenDocument) identity() LSPDocumentIdentity {
	return LSPDocumentIdentity{
		Path: document.Snapshot.Path(), URI: document.URI, Version: document.Version,
		SourceDigest: document.Snapshot.SourceDigest(),
	}
}

func (adapter *LSPAdapter) virtualDocument(uri string) (LSPVirtualDocument, error) {
	if len(uri) > adapter.limits.MaxURIBytes || !utf8.ValidString(uri) {
		return LSPVirtualDocument{}, ErrDefinitionMissing
	}
	elementName, err := descriptorNameFromVirtualURI(uri)
	if err != nil {
		return LSPVirtualDocument{}, err
	}
	adapter.mu.RLock()
	if err := adapter.requireReadyLocked(); err != nil {
		adapter.mu.RUnlock()
		return LSPVirtualDocument{}, err
	}
	keys := make([]string, 0, len(adapter.documents))
	for key := range adapter.documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	documents := make([]lspOpenDocument, len(keys))
	for index, key := range keys {
		documents[index] = adapter.documents[key]
	}
	adapter.mu.RUnlock()
	stale := false
	var result *LSPVirtualDocument
	for _, document := range documents {
		virtual, projectionErr := document.Snapshot.LSPVirtualDescriptorDocument(elementName)
		if projectionErr != nil || virtual.URI != uri {
			continue
		}
		if !adapter.current(document) {
			stale = true
			continue
		}
		if result != nil && *result != virtual {
			return LSPVirtualDocument{}, fmt.Errorf(
				"%w: open snapshots disagree on virtual descriptor content", ErrDefinitionMissing,
			)
		}
		copy := virtual
		result = &copy
	}
	if stale {
		return LSPVirtualDocument{}, fmt.Errorf(
			"%w: document changed while producing the virtual document", ErrLSPVersion,
		)
	}
	if result != nil {
		return *result, nil
	}
	return LSPVirtualDocument{}, ErrDefinitionMissing
}

func descriptorNameFromVirtualURI(value string) (string, error) {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "openrealtime-descriptor" || parsed.Host != "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" ||
		parsed.Opaque != "" || parsed.String() != value {
		return "", ErrDefinitionMissing
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ErrDefinitionMissing
	}
	separator := strings.LastIndexByte(parts[0], '@')
	if separator < 1 || separator == len(parts[0])-1 {
		return "", ErrDefinitionMissing
	}
	name := parts[0][:separator]
	revision, revisionErr := strconv.ParseUint(parts[0][separator+1:], 10, 64)
	if revisionErr != nil || element.ValidateIdentity(element.Identity{
		Name: name, Revision: revision, Digest: parts[1],
	}) != nil {
		return "", ErrDefinitionMissing
	}
	return name, nil
}

func (adapter *LSPAdapter) validateURI(value string) error {
	if len(value) > adapter.limits.MaxURIBytes {
		return fmt.Errorf("%w: LSP document URI exceeds its bound", ErrInvalidPosition)
	}
	return validateLSPDocumentURI(value)
}

func validLSPVersion(value int) bool { return value >= 0 && int64(value) <= int64(1<<31-1) }
