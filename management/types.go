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
	ReadGraph       Operation = "graph.read"
	ReadDescriptor  Operation = "descriptor.read"
	ReadSchema      Operation = "schema.read"
	ReadSession     Operation = "session.read"
	ReadTrace       Operation = "trace.read"
	AnalyzeDocument Operation = "authoring.analyze"
	CompileDocument Operation = "authoring.compile"
	RenderGraph     Operation = "authoring.render"
	ApplyCandidate  Operation = "reconciliation.apply"
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
	Compile(context.Context, AuthoringDocument) (CompileResult, error)
	Render(context.Context, RenderRequest) (RenderResult, error)
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
