package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	lspRPCParseError           = -32700
	lspRPCInvalidRequest       = -32600
	lspRPCMethodNotFound       = -32601
	lspRPCInvalidParams        = -32602
	lspRPCInternalError        = -32603
	lspRPCServerNotInitialized = -32002
	lspRPCRequestCancelled     = -32800
	lspRPCContentModified      = -32801
)

var (
	errLSPRPCInvalidRequest = errors.New("invalid JSON-RPC request")
	errLSPRPCMethodNotFound = errors.New("JSON-RPC method not found")
)

type lspRPCEnvelope struct {
	id        json.RawMessage
	hasID     bool
	method    string
	params    json.RawMessage
	hasParams bool
}

type lspRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lspRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspRPCError    `json:"error,omitempty"`
}

// HandleJSONRPC accepts one complete JSON-RPC 2.0 message and returns at most
// one complete response. It owns no framing, socket, process, workspace, or
// filesystem behavior. Request failures are encoded as JSON-RPC errors;
// notification failures have no response and are returned to the caller.
func (adapter *LSPAdapter) HandleJSONRPC(
	ctx context.Context, source []byte,
) ([]byte, error) {
	if adapter == nil {
		return nil, errors.New("LSP adapter is nil")
	}
	if ctx == nil {
		return nil, errors.New("LSP JSON-RPC context is nil")
	}
	if len(source) == 0 || len(source) > adapter.limits.MaxRequestBytes {
		return adapter.protocolErrorResponse(
			nil, lspRPCInvalidRequest, "JSON-RPC request exceeds its byte bound",
		)
	}
	envelope, err := adapter.decodeRPCEnvelope(source)
	if err != nil {
		if errors.Is(err, errLSPRPCInvalidRequest) {
			return adapter.protocolErrorResponse(nil, lspRPCInvalidRequest, "invalid JSON-RPC request")
		}
		return adapter.protocolErrorResponse(nil, lspRPCParseError, "invalid JSON-RPC payload")
	}

	result, dispatchErr := adapter.dispatchRPC(ctx, envelope)
	if !envelope.hasID {
		if dispatchErr != nil {
			return nil, fmt.Errorf("LSP notification %q: %w", envelope.method, dispatchErr)
		}
		return nil, nil
	}
	if dispatchErr != nil {
		protocol := classifyLSPRPCError(dispatchErr)
		return adapter.protocolErrorResponse(envelope.id, protocol.Code, protocol.Message)
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		return adapter.protocolErrorResponse(envelope.id, lspRPCInternalError, "encode LSP result")
	}
	response, err := adapter.encodeRPCResponse(lspRPCResponse{
		JSONRPC: "2.0", ID: envelope.id, Result: encodedResult,
	})
	if err == nil {
		return response, nil
	}
	return adapter.protocolErrorResponse(envelope.id, lspRPCInternalError, "LSP response exceeds its byte bound")
}

func (adapter *LSPAdapter) decodeRPCEnvelope(source []byte) (lspRPCEnvelope, error) {
	structuralLimit := max(1024, adapter.limits.MaxRequestBytes/2+16)
	if err := strictjson.ValidateWithLimits(source, strictjson.Limits{
		MaxInputBytes: adapter.limits.MaxRequestBytes, MaxDepth: 64,
		MaxTokens: structuralLimit, MaxObjectMembers: structuralLimit,
		MaxArrayElements: structuralLimit, MaxKeyBytes: min(adapter.limits.MaxRequestBytes, 64<<10),
		MaxTotalKeyBytes: int64(adapter.limits.MaxRequestBytes),
		MaxWorkBytes:     int64(adapter.limits.MaxRequestBytes)*8 + 4096,
	}); err != nil {
		return lspRPCEnvelope{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil || fields == nil {
		return lspRPCEnvelope{}, fmt.Errorf("%w: envelope is not an object", errLSPRPCInvalidRequest)
	}
	for field := range fields {
		switch field {
		case "jsonrpc", "id", "method", "params":
		default:
			return lspRPCEnvelope{}, fmt.Errorf("%w: envelope has unknown field %q",
				errLSPRPCInvalidRequest, field)
		}
	}
	var version string
	if raw, found := fields["jsonrpc"]; !found || json.Unmarshal(raw, &version) != nil || version != "2.0" {
		return lspRPCEnvelope{}, fmt.Errorf("%w: version is not 2.0", errLSPRPCInvalidRequest)
	}
	var method string
	if raw, found := fields["method"]; !found || json.Unmarshal(raw, &method) != nil ||
		method == "" || len(method) > adapter.limits.MaxMethodBytes || !utf8.ValidString(method) ||
		strings.ContainsAny(method, "\x00\r\n") {
		return lspRPCEnvelope{}, fmt.Errorf("%w: method is invalid", errLSPRPCInvalidRequest)
	}
	envelope := lspRPCEnvelope{method: method}
	if raw, found := fields["id"]; found {
		if err := adapter.validateRPCID(raw); err != nil {
			return lspRPCEnvelope{}, fmt.Errorf("%w: %v", errLSPRPCInvalidRequest, err)
		}
		envelope.id = append(json.RawMessage(nil), raw...)
		envelope.hasID = true
	}
	if raw, found := fields["params"]; found {
		envelope.params = append(json.RawMessage(nil), raw...)
		envelope.hasParams = true
	}
	return envelope, nil
}

func (adapter *LSPAdapter) validateRPCID(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || len(trimmed) > adapter.limits.MaxIDBytes || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("JSON-RPC id is absent, null, or oversized")
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) != nil || len(value) > adapter.limits.MaxIDBytes ||
			!utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("JSON-RPC string id is invalid")
		}
		return nil
	}
	for index, value := range trimmed {
		if value >= '0' && value <= '9' {
			continue
		}
		if index == 0 && value == '-' && len(trimmed) > 1 {
			continue
		}
		return errors.New("JSON-RPC numeric id is not an integer")
	}
	return nil
}

func (adapter *LSPAdapter) encodeRPCResponse(value lspRPCResponse) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) > adapter.limits.MaxResponseBytes {
		return nil, fmt.Errorf("LSP response has %d bytes; maximum is %d",
			len(encoded), adapter.limits.MaxResponseBytes)
	}
	return encoded, nil
}

func (adapter *LSPAdapter) protocolErrorResponse(
	id json.RawMessage, code int, message string,
) ([]byte, error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	return adapter.encodeRPCResponse(lspRPCResponse{
		JSONRPC: "2.0", ID: id, Error: &lspRPCError{Code: code, Message: message},
	})
}

func decodeLSPRPCParams(raw json.RawMessage, destination any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("LSP params must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("LSP params contain trailing data")
	}
	return nil
}

func requireNoLSPRPCParams(envelope lspRPCEnvelope) error {
	if !envelope.hasParams || bytes.Equal(bytes.TrimSpace(envelope.params), []byte("null")) {
		return nil
	}
	return errors.New("LSP method does not accept params")
}

type lspRPCTextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type lspRPCTextDocumentItem struct {
	URI        string  `json:"uri"`
	LanguageID string  `json:"languageId"`
	Version    *int    `json:"version"`
	Text       *string `json:"text"`
}

type lspRPCVersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version *int   `json:"version"`
}

type lspRPCPosition struct {
	Line      *int `json:"line"`
	Character *int `json:"character"`
}

func (value lspRPCPosition) projection() (LSPPosition, error) {
	if value.Line == nil || value.Character == nil || !validLSPVersion(*value.Line) ||
		!validLSPVersion(*value.Character) {
		return LSPPosition{}, errors.New("LSP position is absent, negative, or oversized")
	}
	return LSPPosition{Line: *value.Line, Character: *value.Character}, nil
}

type lspRPCDidOpenParams struct {
	TextDocument lspRPCTextDocumentItem `json:"textDocument"`
}

type lspRPCFullContentChange struct {
	Text *string `json:"text"`
}

type lspRPCDidChangeParams struct {
	TextDocument   lspRPCVersionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []lspRPCFullContentChange             `json:"contentChanges"`
}

type lspRPCDidCloseParams struct {
	TextDocument lspRPCTextDocumentIdentifier `json:"textDocument"`
}

type lspRPCDiagnosticParams struct {
	TextDocument       lspRPCTextDocumentIdentifier `json:"textDocument"`
	Identifier         string                       `json:"identifier,omitempty"`
	PreviousResultID   string                       `json:"previousResultId,omitempty"`
	WorkDoneToken      json.RawMessage              `json:"workDoneToken,omitempty"`
	PartialResultToken json.RawMessage              `json:"partialResultToken,omitempty"`
}

type lspRPCCompletionContext struct {
	TriggerKind      *int   `json:"triggerKind"`
	TriggerCharacter string `json:"triggerCharacter,omitempty"`
}

type lspRPCCompletionParams struct {
	TextDocument       lspRPCTextDocumentIdentifier `json:"textDocument"`
	Position           lspRPCPosition               `json:"position"`
	Context            *lspRPCCompletionContext     `json:"context,omitempty"`
	WorkDoneToken      json.RawMessage              `json:"workDoneToken,omitempty"`
	PartialResultToken json.RawMessage              `json:"partialResultToken,omitempty"`
}

type lspRPCHoverParams struct {
	TextDocument  lspRPCTextDocumentIdentifier `json:"textDocument"`
	Position      lspRPCPosition               `json:"position"`
	WorkDoneToken json.RawMessage              `json:"workDoneToken,omitempty"`
}

type lspRPCDefinitionParams struct {
	TextDocument       lspRPCTextDocumentIdentifier `json:"textDocument"`
	Position           lspRPCPosition               `json:"position"`
	WorkDoneToken      json.RawMessage              `json:"workDoneToken,omitempty"`
	PartialResultToken json.RawMessage              `json:"partialResultToken,omitempty"`
}

type lspRPCRenameParams struct {
	TextDocument  lspRPCTextDocumentIdentifier `json:"textDocument"`
	Position      lspRPCPosition               `json:"position"`
	NewName       string                       `json:"newName"`
	WorkDoneToken json.RawMessage              `json:"workDoneToken,omitempty"`
}

type lspRPCFormattingOptions struct {
	TabSize                *int  `json:"tabSize"`
	InsertSpaces           *bool `json:"insertSpaces"`
	TrimTrailingWhitespace *bool `json:"trimTrailingWhitespace,omitempty"`
	InsertFinalNewline     *bool `json:"insertFinalNewline,omitempty"`
	TrimFinalNewlines      *bool `json:"trimFinalNewlines,omitempty"`
}

type lspRPCFormattingParams struct {
	TextDocument  lspRPCTextDocumentIdentifier `json:"textDocument"`
	Options       lspRPCFormattingOptions      `json:"options"`
	WorkDoneToken json.RawMessage              `json:"workDoneToken,omitempty"`
}

type lspRPCVirtualDocumentParams struct {
	URI string `json:"uri"`
}

type lspRPCInitializeResult struct {
	Capabilities lspRPCServerCapabilities `json:"capabilities"`
	ServerInfo   lspRPCServerInfo         `json:"serverInfo"`
}

type lspRPCServerCapabilities struct {
	PositionEncoding           string                        `json:"positionEncoding"`
	TextDocumentSync           lspRPCTextDocumentSyncOptions `json:"textDocumentSync"`
	DiagnosticProvider         lspRPCDiagnosticOptions       `json:"diagnosticProvider"`
	CompletionProvider         struct{}                      `json:"completionProvider"`
	HoverProvider              bool                          `json:"hoverProvider"`
	DefinitionProvider         bool                          `json:"definitionProvider"`
	RenameProvider             bool                          `json:"renameProvider"`
	DocumentFormattingProvider bool                          `json:"documentFormattingProvider"`
}

type lspRPCTextDocumentSyncOptions struct {
	OpenClose bool `json:"openClose"`
	Change    int  `json:"change"`
}

type lspRPCDiagnosticOptions struct {
	InterFileDependencies bool `json:"interFileDependencies"`
	WorkspaceDiagnostics  bool `json:"workspaceDiagnostics"`
}

type lspRPCServerInfo struct {
	Name string `json:"name"`
}

func lspRPCInitializeCapabilities() lspRPCInitializeResult {
	return lspRPCInitializeResult{
		Capabilities: lspRPCServerCapabilities{
			PositionEncoding:   "utf-16",
			TextDocumentSync:   lspRPCTextDocumentSyncOptions{OpenClose: true, Change: 1},
			DiagnosticProvider: lspRPCDiagnosticOptions{}, CompletionProvider: struct{}{},
			HoverProvider: true, DefinitionProvider: true, RenameProvider: true,
			DocumentFormattingProvider: true,
		},
		ServerInfo: lspRPCServerInfo{Name: "OpenRealtime"},
	}
}

func (adapter *LSPAdapter) dispatchRPC(
	ctx context.Context, envelope lspRPCEnvelope,
) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch envelope.method {
	case "initialize", "shutdown", "textDocument/diagnostic", "textDocument/completion",
		"textDocument/hover", "textDocument/definition", "textDocument/rename",
		"textDocument/formatting", "openrealtime/virtualDocument":
		if !envelope.hasID {
			return nil, fmt.Errorf("%w: method %q requires an id",
				errLSPRPCInvalidRequest, envelope.method)
		}
	case "initialized", "exit", "textDocument/didOpen", "textDocument/didChange",
		"textDocument/didClose":
		if envelope.hasID {
			return nil, fmt.Errorf("%w: method %q is notification-only",
				errLSPRPCInvalidRequest, envelope.method)
		}
	}

	switch envelope.method {
	case "initialize":
		if !envelope.hasParams || !rpcJSONObject(envelope.params) {
			return nil, errors.New("initialize params must be an object")
		}
		if err := adapter.initialize(); err != nil {
			return nil, fmt.Errorf("%w: %v", errLSPRPCInvalidRequest, err)
		}
		return lspRPCInitializeCapabilities(), nil
	case "initialized":
		if envelope.hasParams {
			var params struct{}
			if err := decodeLSPRPCParams(envelope.params, &params); err != nil {
				return nil, err
			}
		}
		return nil, adapter.requireReady()
	case "shutdown":
		if err := requireNoLSPRPCParams(envelope); err != nil {
			return nil, err
		}
		return nil, adapter.shutdown()
	case "exit":
		if err := requireNoLSPRPCParams(envelope); err != nil {
			return nil, err
		}
		adapter.Dispose()
		return nil, nil
	case "textDocument/didOpen":
		return nil, adapter.rpcDidOpen(ctx, envelope.params)
	case "textDocument/didChange":
		return nil, adapter.rpcDidChange(ctx, envelope.params)
	case "textDocument/didClose":
		return nil, adapter.rpcDidClose(envelope.params)
	case "textDocument/diagnostic":
		return adapter.rpcDiagnostics(ctx, envelope.params)
	case "textDocument/completion":
		return adapter.rpcCompletions(ctx, envelope.params)
	case "textDocument/hover":
		return adapter.rpcHover(ctx, envelope.params)
	case "textDocument/definition":
		return adapter.rpcDefinitions(ctx, envelope.params)
	case "textDocument/rename":
		return adapter.rpcRename(ctx, envelope.params)
	case "textDocument/formatting":
		return adapter.rpcFormatting(ctx, envelope.params)
	case "openrealtime/virtualDocument":
		return adapter.rpcVirtualDocument(ctx, envelope.params)
	default:
		return nil, fmt.Errorf("%w: %s", errLSPRPCMethodNotFound, envelope.method)
	}
}

func rpcJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func (adapter *LSPAdapter) rpcDidOpen(ctx context.Context, raw json.RawMessage) error {
	var params lspRPCDidOpenParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return err
	}
	item := params.TextDocument
	if item.Version == nil || item.Text == nil {
		return errors.New("didOpen requires document version and text")
	}
	return adapter.openDocument(ctx, item.URI, item.LanguageID, *item.Version, *item.Text)
}

func (adapter *LSPAdapter) rpcDidChange(ctx context.Context, raw json.RawMessage) error {
	var params lspRPCDidChangeParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return err
	}
	if params.TextDocument.Version == nil || len(params.ContentChanges) != 1 ||
		params.ContentChanges[0].Text == nil {
		return errors.New("didChange requires one versioned full-text change")
	}
	return adapter.changeDocument(ctx, params.TextDocument.URI,
		*params.TextDocument.Version, *params.ContentChanges[0].Text)
}

func (adapter *LSPAdapter) rpcDidClose(raw json.RawMessage) error {
	var params lspRPCDidCloseParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return err
	}
	return adapter.closeDocument(params.TextDocument.URI)
}

func (adapter *LSPAdapter) rpcDiagnostics(
	ctx context.Context, raw json.RawMessage,
) (LSPFullDocumentDiagnosticReport, error) {
	var params lspRPCDiagnosticParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken, params.PartialResultToken); err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	if len(params.Identifier) > adapter.limits.MaxMethodBytes ||
		len(params.PreviousResultID) > adapter.limits.MaxIDBytes {
		return LSPFullDocumentDiagnosticReport{}, errors.New("diagnostic identity exceeds its bound")
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	result, err := document.Snapshot.LSPDiagnostics()
	if err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	return result, nil
}

func (adapter *LSPAdapter) rpcCompletions(
	ctx context.Context, raw json.RawMessage,
) (LSPCompletionList, error) {
	var params lspRPCCompletionParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return LSPCompletionList{}, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken, params.PartialResultToken); err != nil {
		return LSPCompletionList{}, err
	}
	if params.Context != nil {
		if params.Context.TriggerKind == nil || *params.Context.TriggerKind < 1 ||
			*params.Context.TriggerKind > 3 || len(params.Context.TriggerCharacter) > 256 ||
			strings.ContainsAny(params.Context.TriggerCharacter, "\x00\r\n") {
			return LSPCompletionList{}, errors.New("completion context is invalid")
		}
	}
	position, err := params.Position.projection()
	if err != nil {
		return LSPCompletionList{}, err
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return LSPCompletionList{}, err
	}
	result, err := document.Snapshot.LSPCompletions(position)
	if errors.Is(err, ErrNoCompletionContext) || errors.Is(err, ErrNoSymbol) {
		result, err = LSPCompletionList{Items: []LSPCompletionItem{}}, nil
	}
	if err != nil {
		return LSPCompletionList{}, err
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return LSPCompletionList{}, err
	}
	return result, nil
}

func (adapter *LSPAdapter) rpcHover(ctx context.Context, raw json.RawMessage) (any, error) {
	var params lspRPCHoverParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return nil, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken); err != nil {
		return nil, err
	}
	position, err := params.Position.projection()
	if err != nil {
		return nil, err
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	result, err := document.Snapshot.LSPConfigurationHover(position)
	if errors.Is(err, ErrNoSymbol) {
		if err := adapter.finishRPCQuery(ctx, document); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return nil, err
	}
	return result, nil
}

func (adapter *LSPAdapter) rpcDefinitions(
	ctx context.Context, raw json.RawMessage,
) ([]LSPLocationLink, error) {
	var params lspRPCDefinitionParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return nil, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken, params.PartialResultToken); err != nil {
		return nil, err
	}
	position, err := params.Position.projection()
	if err != nil {
		return nil, err
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	result, err := document.Snapshot.LSPDefinitions(position, document.identity())
	if errors.Is(err, ErrNoSymbol) || errors.Is(err, ErrDefinitionMissing) {
		result, err = []LSPLocationLink{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return nil, err
	}
	return result, nil
}

func (adapter *LSPAdapter) rpcRename(
	ctx context.Context, raw json.RawMessage,
) (LSPWorkspaceEdit, error) {
	var params lspRPCRenameParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return LSPWorkspaceEdit{}, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken); err != nil {
		return LSPWorkspaceEdit{}, err
	}
	position, err := params.Position.projection()
	if err != nil {
		return LSPWorkspaceEdit{}, err
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return LSPWorkspaceEdit{}, err
	}
	result, err := document.Snapshot.LSPRename(position, params.NewName, document.identity())
	if err != nil {
		return LSPWorkspaceEdit{}, err
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return LSPWorkspaceEdit{}, err
	}
	return result, nil
}

func (adapter *LSPAdapter) rpcFormatting(
	ctx context.Context, raw json.RawMessage,
) ([]LSPTextEdit, error) {
	var params lspRPCFormattingParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return nil, err
	}
	if err := adapter.validateRPCProgressTokens(params.WorkDoneToken); err != nil {
		return nil, err
	}
	if params.Options.TabSize == nil || params.Options.InsertSpaces == nil ||
		*params.Options.TabSize < 1 || *params.Options.TabSize > 256 {
		return nil, errors.New("formatting options are incomplete or invalid")
	}
	document, err := adapter.document(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	workspaceEdit, err := document.Snapshot.LSPFormatting(document.identity())
	if err != nil {
		return nil, err
	}
	if len(workspaceEdit.DocumentChanges) != 1 ||
		workspaceEdit.DocumentChanges[0].TextDocument != (LSPVersionedTextDocumentIdentifier{
			URI: document.URI, Version: document.Version,
		}) {
		return nil, errors.New("formatting projection changed document identity")
	}
	if err := adapter.finishRPCQuery(ctx, document); err != nil {
		return nil, err
	}
	return append([]LSPTextEdit(nil), workspaceEdit.DocumentChanges[0].Edits...), nil
}

func (adapter *LSPAdapter) rpcVirtualDocument(
	ctx context.Context, raw json.RawMessage,
) (LSPVirtualDocument, error) {
	var params lspRPCVirtualDocumentParams
	if err := decodeLSPRPCParams(raw, &params); err != nil {
		return LSPVirtualDocument{}, err
	}
	result, err := adapter.virtualDocument(params.URI)
	if err != nil {
		return LSPVirtualDocument{}, err
	}
	if err := ctx.Err(); err != nil {
		return LSPVirtualDocument{}, err
	}
	return result, nil
}

func (adapter *LSPAdapter) validateRPCProgressTokens(values ...json.RawMessage) error {
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		if err := adapter.validateRPCID(value); err != nil {
			return fmt.Errorf("invalid LSP progress token: %w", err)
		}
	}
	return nil
}

func (adapter *LSPAdapter) finishRPCQuery(
	ctx context.Context, document lspOpenDocument,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !adapter.current(document) {
		return fmt.Errorf("%w: document changed while producing the LSP result", ErrLSPVersion)
	}
	return nil
}

func classifyLSPRPCError(err error) lspRPCError {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return lspRPCError{Code: lspRPCRequestCancelled, Message: "request cancelled"}
	case errors.Is(err, ErrLSPNotInitialized), errors.Is(err, ErrLSPDisposed):
		return lspRPCError{Code: lspRPCServerNotInitialized, Message: "server not initialized"}
	case errors.Is(err, ErrLSPVersion):
		return lspRPCError{Code: lspRPCContentModified, Message: "document content changed"}
	case errors.Is(err, errLSPRPCInvalidRequest):
		return lspRPCError{Code: lspRPCInvalidRequest, Message: boundedLSPRPCErrorMessage(err)}
	case errors.Is(err, errLSPRPCMethodNotFound):
		return lspRPCError{Code: lspRPCMethodNotFound, Message: "method not found"}
	case errors.Is(err, ErrPresentationLimit):
		return lspRPCError{Code: lspRPCInternalError, Message: "LSP result exceeds its presentation bound"}
	default:
		return lspRPCError{Code: lspRPCInvalidParams, Message: boundedLSPRPCErrorMessage(err)}
	}
}

func boundedLSPRPCErrorMessage(err error) string {
	if err == nil {
		return "LSP request failed"
	}
	message := err.Error()
	if len(message) <= 1024 {
		return message
	}
	message = message[:1024]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}
