package management

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const maxManagedSourceBytes = 1 << 20

type AuthoringOptions struct {
	Catalog        *resolve.Catalog
	Loader         graphcompiler.SourceLoader
	EditorLimits   editor.Limits
	SchemaResolver schema.Resolver
	SchemaLimits   schema.Limits
}

// AuthoringEngine is a pure bounded implementation of the management
// authoring service. It reads no files unless the deployment explicitly
// supplies a constrained SourceLoader and it never writes source.
type AuthoringEngine struct {
	catalog        *resolve.Catalog
	loader         graphcompiler.SourceLoader
	limits         editor.Limits
	schemaResolver schema.Resolver
	schemaLimits   schema.Limits
}

func NewAuthoringEngine(options AuthoringOptions) (*AuthoringEngine, error) {
	if options.Catalog == nil {
		return nil, fmt.Errorf("create management authoring engine: %w: nil catalog", ErrInvalid)
	}
	if options.EditorLimits == (editor.Limits{}) {
		options.EditorLimits = editor.DefaultLimits()
	}
	return &AuthoringEngine{
		catalog: options.Catalog, loader: options.Loader, limits: options.EditorLimits,
		schemaResolver: options.SchemaResolver, schemaLimits: options.SchemaLimits,
	}, nil
}

func (engine *AuthoringEngine) Analyze(
	ctx context.Context, input AuthoringDocument,
) (AnalysisResult, error) {
	if err := checkAuthoringContext(ctx); err != nil {
		return AnalysisResult{}, err
	}
	if err := validateDocument(input, true); err != nil {
		return AnalysisResult{}, err
	}
	document, err := editor.AnalyzeWithOptions(ctx, input.Path, []byte(input.Source), engine.catalog, editor.Options{
		Limits: engine.limits, SchemaResolver: engine.schemaResolver, SchemaLimits: engine.schemaLimits,
	})
	if err != nil {
		return AnalysisResult{}, fmt.Errorf("%w: analyze topology: %v", ErrInvalid, err)
	}
	result := AnalysisResult{
		SourceDigest: document.SourceDigest(), Parsed: document.Parsed(), Canonical: document.Canonical(),
		Recovered: document.Recovered(), Diagnostics: document.Diagnostics(), Catalog: document.CatalogMetadata(),
	}
	if document.Parsed() {
		formatting, err := document.FormatEdits()
		if err != nil {
			return AnalysisResult{}, fmt.Errorf("%w: format topology: %v", ErrConflict, err)
		}
		result.Formatting = &formatting
	}
	return result, nil
}

func (engine *AuthoringEngine) Rename(
	ctx context.Context, input RenameDocumentRequest,
) (RenameDocumentResult, error) {
	if err := checkAuthoringContext(ctx); err != nil {
		return RenameDocumentResult{}, err
	}
	if err := ValidateRenameDocumentRequest(input); err != nil {
		return RenameDocumentResult{}, err
	}
	document, err := editor.AnalyzeWithOptions(
		ctx, input.Document.Path, []byte(input.Document.Source), engine.catalog, editor.Options{
			Limits: engine.limits, SchemaResolver: engine.schemaResolver, SchemaLimits: engine.schemaLimits,
		},
	)
	if err != nil {
		return RenameDocumentResult{}, fmt.Errorf("%w: analyze topology for rename: %v", ErrInvalid, err)
	}
	edits, err := document.RenameNodeID(input.Node, input.NewName)
	if err != nil {
		return RenameDocumentResult{}, fmt.Errorf("%w: rename topology node: %v", ErrInvalid, err)
	}
	result := RenameDocumentResult{Node: input.Node, NewName: input.NewName, Edits: edits}
	if err := ValidateRenameDocumentResult(input, result); err != nil {
		return RenameDocumentResult{}, fmt.Errorf("authoring rename produced invalid edits: %w", err)
	}
	return result, nil
}

func (engine *AuthoringEngine) RemoveEdge(
	ctx context.Context, input RemoveDocumentEdgeRequest,
) (RemoveDocumentEdgeResult, error) {
	if err := checkAuthoringContext(ctx); err != nil {
		return RemoveDocumentEdgeResult{}, err
	}
	if err := ValidateRemoveDocumentEdgeRequest(input); err != nil {
		return RemoveDocumentEdgeResult{}, err
	}
	document, err := editor.AnalyzeWithOptions(
		ctx, input.Document.Path, []byte(input.Document.Source), engine.catalog, editor.Options{
			Limits: engine.limits, SchemaResolver: engine.schemaResolver, SchemaLimits: engine.schemaLimits,
		},
	)
	if err != nil {
		return RemoveDocumentEdgeResult{}, fmt.Errorf("%w: analyze topology for edge removal: %v", ErrInvalid, err)
	}
	edits, err := document.RemoveEdgeID(input.Edge)
	if err != nil {
		return RemoveDocumentEdgeResult{}, fmt.Errorf("%w: remove topology edge: %v", ErrInvalid, err)
	}
	result := RemoveDocumentEdgeResult{Edge: input.Edge, Edits: edits}
	if err := ValidateRemoveDocumentEdgeResult(input, result); err != nil {
		return RemoveDocumentEdgeResult{}, fmt.Errorf("authoring edge removal produced invalid edits: %w", err)
	}
	return result, nil
}

func (engine *AuthoringEngine) Compile(
	ctx context.Context, input AuthoringDocument,
) (CompileResult, error) {
	if err := checkAuthoringContext(ctx); err != nil {
		return CompileResult{}, err
	}
	if err := validateDocument(input, false); err != nil {
		return CompileResult{}, err
	}
	file, err := parseAuthoringDocument(input.Path, []byte(input.Source))
	if err != nil {
		return CompileResult{}, fmt.Errorf("%w: parse topology: %v", ErrInvalid, err)
	}
	lock := resolve.NewLock()
	mode := resolve.Update
	if input.Lock != nil {
		canonical, err := input.Lock.Canonical()
		if err != nil {
			return CompileResult{}, fmt.Errorf("%w: resolution lock: %v", ErrInvalid, err)
		}
		lock, mode = canonical, resolve.Locked
	}
	depth := make(map[string]int, len(input.ChannelDepth))
	for key, value := range input.ChannelDepth {
		depth[key] = value
	}
	revision := input.Revision
	if revision == 0 {
		revision = 1
	}
	result, err := graphcompiler.Compile(file, graphcompiler.Options{
		Catalog: engine.catalog, Lock: lock, ResolutionMode: mode,
		Revision: revision, ChannelDepth: depth, Loader: engine.loader,
	})
	if err != nil {
		return CompileResult{}, fmt.Errorf("%w: compile topology: %v", ErrInvalid, err)
	}
	return CompileResult{Graph: result.Graph, Lock: result.Lock}, nil
}

func (engine *AuthoringEngine) Render(
	ctx context.Context, input RenderRequest,
) (RenderResult, error) {
	if err := checkAuthoringContext(ctx); err != nil {
		return RenderResult{}, err
	}
	if err := input.Graph.Validate(); err != nil {
		return RenderResult{}, fmt.Errorf("%w: graph IR: %v", ErrInvalid, err)
	}
	result := RenderResult{Fingerprint: input.Graph.Fingerprint, Format: input.Format}
	switch input.Format {
	case RenderModel:
		model, err := inspect.Build(input.Graph)
		if err != nil {
			return RenderResult{}, fmt.Errorf("%w: build graph view: %v", ErrInvalid, err)
		}
		result.Model = &model
	case RenderMermaid:
		value, err := inspect.Mermaid(input.Graph)
		if err != nil {
			return RenderResult{}, fmt.Errorf("%w: render Mermaid: %v", ErrInvalid, err)
		}
		result.Text = value
	case RenderDOT:
		value, err := inspect.DOT(input.Graph)
		if err != nil {
			return RenderResult{}, fmt.Errorf("%w: render DOT: %v", ErrInvalid, err)
		}
		result.Text = value
	default:
		return RenderResult{}, fmt.Errorf("%w: unsupported render format %q", ErrInvalid, input.Format)
	}
	return result, nil
}

func validateDocument(input AuthoringDocument, ortgOnly bool) error {
	if input.Path == "" || input.Path != strings.TrimSpace(input.Path) || len(input.Path) > 4096 ||
		strings.ContainsAny(input.Path, "\x00\r\n") {
		return fmt.Errorf("%w: authoring path", ErrInvalid)
	}
	if len(input.Source) == 0 || len(input.Source) > maxManagedSourceBytes {
		return fmt.Errorf("%w: authoring source must be 1..%d bytes", ErrInvalid, maxManagedSourceBytes)
	}
	extension := strings.ToLower(filepath.Ext(input.Path))
	if ortgOnly && extension != ".ortg" {
		return fmt.Errorf("%w: language-service analysis requires .ortg", ErrInvalid)
	}
	if extension != ".ortg" && extension != ".yaml" && extension != ".yml" && extension != ".json" {
		return fmt.Errorf("%w: unsupported topology extension", ErrInvalid)
	}
	for edge, depth := range input.ChannelDepth {
		if edge == "" || edge != strings.TrimSpace(edge) || depth <= 0 || depth > 1<<20 {
			return fmt.Errorf("%w: invalid channel-depth override", ErrInvalid)
		}
	}
	return nil
}

func parseAuthoringDocument(path string, source []byte) (syntax.File, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ortg":
		return syntax.Parse(path, slices.Clone(source))
	case ".yaml", ".yml":
		return manifest.ParseYAML(path, slices.Clone(source))
	case ".json":
		return manifest.ParseJSON(path, slices.Clone(source))
	default:
		return syntax.File{}, fmt.Errorf("unsupported topology extension")
	}
}

func checkAuthoringContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: request canceled: %v", ErrUnavailable, err)
	}
	return nil
}
