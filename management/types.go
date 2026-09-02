package management

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/plugin"
)

var (
	ErrNotFound     = errors.New("management resource not found")
	ErrUnauthorized = errors.New("management capability not authorized")
	ErrInvalid      = errors.New("invalid management request")
	ErrConflict     = errors.New("management resource conflict")
	ErrUnavailable  = errors.New("management service unavailable")
)

// Operation is the smallest management authority unit. Read capabilities and
// operator mutation capabilities are intentionally disjoint.
type Operation string

const (
	ReadGraph          Operation = "graph.read"
	ReadDescriptor     Operation = "descriptor.read"
	ReadSchema         Operation = "schema.read"
	ReadSession        Operation = "session.read"
	ReadTrace          Operation = "trace.read"
	AnalyzeDocument    Operation = "authoring.analyze"
	RenameDocument     Operation = "authoring.rename"
	RemoveDocumentEdge Operation = "authoring.edge.remove"
	CreateDocumentEdge Operation = "authoring.edge.create"
	CompileDocument    Operation = "authoring.compile"
	RenderGraph        Operation = "authoring.render"
	ReadSource         Operation = "authoring.source.read"
	CreateSource       Operation = "authoring.source.create"
	UpdateSource       Operation = "authoring.source.update"
	ApplyCandidate     Operation = "reconciliation.apply"
)

// AuthorizationRequest contains no request payload. Resource is a canonical
// immutable identity or session ID, never a human prompt or model output.
type AuthorizationRequest struct {
	Capability string
	Operation  Operation
	Resource   string
}

type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

// StaticCatalog provides immutable management resources. Implementations must
// return independent copies because handlers encode concurrently.
type StaticCatalog interface {
	Graph(context.Context, string) (ir.Graph, error)
	ElementDescriptor(context.Context, element.Identity) (element.Descriptor, error)
	PluginDescriptor(context.Context, plugin.Identity) (plugin.Descriptor, error)
	ValuesSchema(context.Context, string) (schema.Bundle, error)
}

// DeltaPage is a bounded resumable recording page. Baseline, Events, and Next
// share the trace recorder's sequence domain; the independently sampled Live
// endpoint keeps its own mount sequence. A baseline is included for a new or
// compacted cursor so replay never has to invent missing state.
type DeltaPage struct {
	FormatVersion uint64                 `json:"format_version"`
	SessionID     string                 `json:"session_id"`
	Graph         inspect.GraphReference `json:"graph"`
	After         uint64                 `json:"after"`
	Next          uint64                 `json:"next"`
	Compacted     bool                   `json:"compacted,omitempty"`
	Dropped       uint64                 `json:"dropped,omitempty"`
	Baseline      *inspect.TraceSnapshot `json:"baseline,omitempty"`
	Events        []inspect.TraceEvent   `json:"events"`
}

// SessionInspection is the payload-free live observability boundary. Limit is
// an explicit caller hint and implementations still enforce their own ceiling.
type SessionInspection interface {
	Snapshot(context.Context, string) (inspect.Live, error)
	Model(context.Context, string) (inspect.Model, error)
	Deltas(context.Context, string, uint64, uint32) (DeltaPage, error)
	Trace(context.Context, string) (inspect.LiveTrace, error)
}

// AuthoringDocument is in-memory source. Management services never infer a
// filesystem authority from Path and never write it directly.
type AuthoringDocument struct {
	Path         string         `json:"path"`
	Source       string         `json:"source"`
	Lock         *resolve.Lock  `json:"lock,omitempty"`
	ChannelDepth map[string]int `json:"channel_depth,omitempty"`
	Revision     uint64         `json:"revision,omitempty"`
}

type AnalysisResult struct {
	SourceDigest string                  `json:"source_digest"`
	Parsed       bool                    `json:"parsed"`
	Recovered    bool                    `json:"recovered"`
	Canonical    bool                    `json:"canonical"`
	Diagnostics  editor.DiagnosticReport `json:"diagnostics"`
	Catalog      editor.MetadataReport   `json:"catalog"`
	Formatting   *editor.EditSet         `json:"formatting,omitempty"`
}

type CompileResult struct {
	Graph ir.Graph     `json:"graph"`
	Lock  resolve.Lock `json:"lock"`
}

// RenameDocumentRequest identifies one node in an exact canonical .ortg or
// normalized YAML/JSON document. Node is the currently compiled identifier;
// NewName is its desired replacement. The operation returns edits only and
// performs no I/O.
type RenameDocumentRequest struct {
	Document AuthoringDocument `json:"document"`
	Node     string            `json:"node"`
	NewName  string            `json:"new_name"`
}

type RenameDocumentResult struct {
	Node    string         `json:"node"`
	NewName string         `json:"new_name"`
	Edits   editor.EditSet `json:"edits"`
}

// RemoveDocumentEdgeRequest identifies one edge in an exact canonical .ortg or
// normalized YAML/JSON document. Edge is the same stable identity emitted into
// compiled Graph IR. The operation returns edits only and performs no I/O.
type RemoveDocumentEdgeRequest struct {
	Document AuthoringDocument `json:"document"`
	Edge     string            `json:"edge"`
}

type RemoveDocumentEdgeResult struct {
	Edge  string         `json:"edge"`
	Edits editor.EditSet `json:"edits"`
}

type AuthoringEdgeEndpoint struct {
	Node string `json:"node"`
	Port string `json:"port"`
}

// CreateDocumentEdgeRequest appends one named edge to exact canonical .ortg or
// normalized YAML/JSON source currently compiled under ExpectedFingerprint.
// The engine recompiles the candidate at the next revision before returning
// its edit set.
type CreateDocumentEdgeRequest struct {
	Document            AuthoringDocument     `json:"document"`
	ExpectedFingerprint string                `json:"expected_fingerprint"`
	Edge                string                `json:"edge"`
	From                AuthoringEdgeEndpoint `json:"from"`
	To                  AuthoringEdgeEndpoint `json:"to"`
	Delivery            string                `json:"delivery"`
}

type CreateDocumentEdgeResult struct {
	Edge                 string         `json:"edge"`
	PreviousFingerprint  string         `json:"previous_fingerprint"`
	CandidateFingerprint string         `json:"candidate_fingerprint"`
	Edits                editor.EditSet `json:"edits"`
}

type RenderFormat string

const (
	RenderModel   RenderFormat = "model"
	RenderMermaid RenderFormat = "mermaid"
	RenderDOT     RenderFormat = "dot"
)

type RenderRequest struct {
	Graph  ir.Graph     `json:"graph"`
	Format RenderFormat `json:"format"`
}

type RenderResult struct {
	Fingerprint string         `json:"fingerprint"`
	Format      RenderFormat   `json:"format"`
	Model       *inspect.Model `json:"model,omitempty"`
	Text        string         `json:"text,omitempty"`
}

type Authoring interface {
	Analyze(context.Context, AuthoringDocument) (AnalysisResult, error)
	Rename(context.Context, RenameDocumentRequest) (RenameDocumentResult, error)
	RemoveEdge(context.Context, RemoveDocumentEdgeRequest) (RemoveDocumentEdgeResult, error)
	CreateEdge(context.Context, CreateDocumentEdgeRequest) (CreateDocumentEdgeResult, error)
	Compile(context.Context, AuthoringDocument) (CompileResult, error)
	Render(context.Context, RenderRequest) (RenderResult, error)
}

// SourceReadFormatVersion identifies the rooted read request/result schema.
const SourceReadFormatVersion uint64 = 1

// SourceReadRequest names one canonical source artifact beneath an explicitly
// configured root. The relative path never grants filesystem authority.
type SourceReadRequest struct {
	FormatVersion uint64 `json:"format_version"`
	RootIdentity  string `json:"root_identity"`
	Path          string `json:"path"`
}

// SourceReadResult returns the exact bounded UTF-8 source plus deterministic
// evidence binding it to the requested root and path. ResultDigest covers the
// metadata and SourceDigest; SourceDigest independently covers Source.
type SourceReadResult struct {
	FormatVersion uint64 `json:"format_version"`
	RootIdentity  string `json:"root_identity"`
	Path          string `json:"path"`
	Source        string `json:"source"`
	SourceDigest  string `json:"source_digest"`
	SourceBytes   uint64 `json:"source_bytes"`
	ResultDigest  string `json:"result_digest"`
}

// SourceReading is the only management contract that can acquire authoring
// source bytes from a configured filesystem root.
type SourceReading interface {
	Read(context.Context, SourceReadRequest) (SourceReadResult, error)
}

// SourceWriteMode separates create-only publication from stale-digest-bound
// replacement so capabilities can authorize the two effects independently.
type SourceWriteMode string

const (
	SourceCreate SourceWriteMode = "create"
	SourceUpdate SourceWriteMode = "update"
)

// SourceWriteFormatVersion identifies the request and receipt wire schema.
const SourceWriteFormatVersion uint64 = 1

// SourceWriteRequest carries an in-memory authoring document to one explicitly
// configured rooted publisher. Path is a canonical slash-separated path
// relative to that root; it never grants filesystem authority by itself.
type SourceWriteRequest struct {
	FormatVersion        uint64          `json:"format_version"`
	RootIdentity         string          `json:"root_identity"`
	Mode                 SourceWriteMode `json:"mode"`
	Path                 string          `json:"path"`
	Source               string          `json:"source"`
	ExpectedSourceDigest string          `json:"expected_source_digest,omitempty"`
}

// SourceWriteReceipt is payload-free, deterministic evidence of the exact
// publication. ReceiptDigest binds every preceding field, including whether a
// superseded staging name could not be removed after an atomic update.
type SourceWriteReceipt struct {
	FormatVersion        uint64          `json:"format_version"`
	RootIdentity         string          `json:"root_identity"`
	Mode                 SourceWriteMode `json:"mode"`
	Path                 string          `json:"path"`
	PreviousSourceDigest string          `json:"previous_source_digest,omitempty"`
	SourceDigest         string          `json:"source_digest"`
	SourceBytes          uint64          `json:"source_bytes"`
	CleanupPending       bool            `json:"cleanup_pending,omitempty"`
	ReceiptDigest        string          `json:"receipt_digest"`
}

// SourcePublication is the only management contract that can mutate authoring
// files. Pure analysis and LSP services deliberately do not implement it.
type SourcePublication interface {
	Publish(context.Context, SourceWriteRequest) (SourceWriteReceipt, error)
}

// ReconciliationRequest names the exact mounted predecessor and the complete
// immutable candidate. Values and deployment remain separately identified;
// credentials and secret locators are never carried by this public schema.
type ReconciliationRequest struct {
	SessionID             string   `json:"session_id"`
	ExpectedFingerprint   string   `json:"expected_fingerprint"`
	Candidate             ir.Graph `json:"candidate"`
	ValuesFingerprint     string   `json:"values_fingerprint"`
	DeploymentFingerprint string   `json:"deployment_fingerprint"`
	StateMigration        string   `json:"state_migration,omitempty"`
}

type ReconciliationReceipt struct {
	FormatVersion        uint64 `json:"format_version"`
	SessionID            string `json:"session_id"`
	PreviousFingerprint  string `json:"previous_fingerprint"`
	CandidateFingerprint string `json:"candidate_fingerprint"`
	State                string `json:"state"`
	SafePointSequence    uint64 `json:"safe_point_sequence,omitempty"`
	RollbackFingerprint  string `json:"rollback_fingerprint,omitempty"`
}

type Reconciliation interface {
	Apply(context.Context, ReconciliationRequest) (ReconciliationReceipt, error)
}
