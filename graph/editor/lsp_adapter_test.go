package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const testLSPDocumentURI = "file:///workspace/agent.ortg"

func TestLSPAdapterFullTextLifecycleAndProtocolProjections(t *testing.T) {
	adapter, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{})
	initializeTestLSPAdapter(t, adapter)
	if adapter.DocumentCount() != 0 {
		t.Fatalf("documents before didOpen = %d", adapter.DocumentCount())
	}

	openTestLSPDocument(t, adapter, testLSPDocumentURI, 7, validSource)
	if adapter.DocumentCount() != 1 {
		t.Fatalf("documents after didOpen = %d", adapter.DocumentCount())
	}

	diagnostics := decodeTestLSPResult[LSPFullDocumentDiagnosticReport](t,
		testLSPRequest(t, adapter, "diagnostics", "textDocument/diagnostic", map[string]any{
			"textDocument": map[string]any{"uri": testLSPDocumentURI},
		}))
	if diagnostics.Kind != "full" || diagnostics.Items == nil || len(diagnostics.Items) != 0 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}

	completionRequest := marshalTestLSPRequest(t, 12, "textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI},
		"position":     map[string]any{"line": 1, "character": 10},
		"context":      map[string]any{"triggerKind": 1},
	})
	firstCompletionResponse, err := adapter.HandleJSONRPC(context.Background(), completionRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondCompletionResponse, err := adapter.HandleJSONRPC(context.Background(), completionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstCompletionResponse, secondCompletionResponse) {
		t.Fatalf("completion response is nondeterministic:\n%s\n%s",
			firstCompletionResponse, secondCompletionResponse)
	}
	completion := decodeTestLSPResult[LSPCompletionList](t,
		decodeTestLSPResponse(t, firstCompletionResponse))
	if completion.IsIncomplete || len(completion.Items) != 2 ||
		completion.Items[0].Label != "test.Sink" || completion.Items[1].Label != "test.Source" {
		t.Fatalf("completion = %+v", completion)
	}

	hover := decodeTestLSPResult[LSPHover](t,
		testLSPRequest(t, adapter, 13, "textDocument/hover", testLSPPositionParams(
			testLSPDocumentURI, 1, 5,
		)))
	if hover.Contents.Kind != "plaintext" ||
		!strings.Contains(hover.Contents.Value, `property[0].default: null`) ||
		!strings.Contains(hover.Contents.Value, `schema status: "resolved"`) {
		t.Fatalf("hover = %+v", hover)
	}
	noHover := testLSPRequest(t, adapter, 131, "textDocument/hover", testLSPPositionParams(
		testLSPDocumentURI, 0, 0,
	))
	if noHover.Error != nil || !bytes.Equal(bytes.TrimSpace(noHover.Result), []byte("null")) {
		t.Fatalf("no-symbol hover = %+v", noHover)
	}

	definitions := decodeTestLSPResult[[]LSPLocationLink](t,
		testLSPRequest(t, adapter, 14, "textDocument/definition", testLSPPositionParams(
			testLSPDocumentURI, 3, 14,
		)))
	if len(definitions) != 1 ||
		!strings.HasPrefix(definitions[0].TargetURI, "openrealtime-descriptor:/test.Source@2/sha256:") {
		t.Fatalf("definitions = %+v", definitions)
	}
	virtual := decodeTestLSPResult[LSPVirtualDocument](t,
		testLSPRequest(t, adapter, 15, "openrealtime/virtualDocument", map[string]any{
			"uri": definitions[0].TargetURI,
		}))
	if virtual.URI != definitions[0].TargetURI || virtual.LanguageID != "plaintext" ||
		lspRangeText(t, virtual.Text, definitions[0].TargetSelectionRange) != "out" ||
		!strings.Contains(virtual.Text, "canonical metadata JSON:") {
		t.Fatalf("virtual document = %+v", virtual)
	}

	rename := decodeTestLSPResult[LSPWorkspaceEdit](t,
		testLSPRequest(t, adapter, 16, "textDocument/rename", map[string]any{
			"textDocument": map[string]any{"uri": testLSPDocumentURI},
			"position":     map[string]any{"line": 3, "character": 5},
			"newName":      "camera",
		}))
	if len(rename.DocumentChanges) != 1 ||
		rename.DocumentChanges[0].TextDocument.Version != 7 ||
		len(rename.DocumentChanges[0].Edits) != 3 {
		t.Fatalf("rename = %+v", rename)
	}

	unformatted := strings.Replace(validSource, "test.Source ::", "test.Source  ::", 1)
	testLSPNotification(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": testLSPDocumentURI, "version": 8},
		"contentChanges": []any{map[string]any{"text": unformatted}},
	})
	formatting := decodeTestLSPResult[[]LSPTextEdit](t,
		testLSPRequest(t, adapter, 17, "textDocument/formatting", map[string]any{
			"textDocument": map[string]any{"uri": testLSPDocumentURI},
			"options":      map[string]any{"tabSize": 4, "insertSpaces": true},
		}))
	if len(formatting) != 1 ||
		string(applyLSPTextEdits(t, []byte(unformatted), formatting)) != validSource {
		t.Fatalf("formatting = %+v", formatting)
	}

	testLSPNotification(t, adapter, "textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI},
	})
	if adapter.DocumentCount() != 0 {
		t.Fatalf("documents after didClose = %d", adapter.DocumentCount())
	}
	shutdown := testLSPRequest(t, adapter, 18, "shutdown", nil)
	if !bytes.Equal(bytes.TrimSpace(shutdown.Result), []byte("null")) || adapter.DocumentCount() != 0 {
		t.Fatalf("shutdown = %s, documents = %d", shutdown.Result, adapter.DocumentCount())
	}
	testLSPNotification(t, adapter, "exit", nil)
	disposed := testLSPRequest(t, adapter, 19, "textDocument/diagnostic", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI},
	})
	if disposed.Error == nil || disposed.Error.Code != lspRPCServerNotInitialized {
		t.Fatalf("disposed query = %+v", disposed)
	}
}

func TestLSPAdapterRejectsMalformedAndInvalidJSONRPC(t *testing.T) {
	adapter, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{})
	cases := []struct {
		name string
		raw  string
		code int
	}{
		{"malformed", `{`, lspRPCParseError},
		{"duplicate-key", `{"jsonrpc":"2.0","jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, lspRPCParseError},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}]`, lspRPCInvalidRequest},
		{"unknown-envelope-field", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{},"extra":true}`, lspRPCInvalidRequest},
		{"wrong-version", `{"jsonrpc":"1.0","id":1,"method":"initialize","params":{}}`, lspRPCInvalidRequest},
		{"missing-method", `{"jsonrpc":"2.0","id":1,"params":{}}`, lspRPCInvalidRequest},
		{"null-id", `{"jsonrpc":"2.0","id":null,"method":"initialize","params":{}}`, lspRPCInvalidRequest},
		{"fractional-id", `{"jsonrpc":"2.0","id":1.5,"method":"initialize","params":{}}`, lspRPCInvalidRequest},
		{"exponent-id", `{"jsonrpc":"2.0","id":1e2,"method":"initialize","params":{}}`, lspRPCInvalidRequest},
		{"boolean-id", `{"jsonrpc":"2.0","id":true,"method":"initialize","params":{}}`, lspRPCInvalidRequest},
		{"control-method", `{"jsonrpc":"2.0","id":1,"method":"bad\nmethod","params":{}}`, lspRPCInvalidRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response, err := adapter.HandleJSONRPC(context.Background(), []byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			decoded := decodeTestLSPResponse(t, response)
			if decoded.Error == nil || decoded.Error.Code != test.code ||
				!bytes.Equal(bytes.TrimSpace(decoded.ID), []byte("null")) {
				t.Fatalf("response = %s", response)
			}
		})
	}

	beforeInitialize := testLSPRequest(t, adapter, 21, "textDocument/diagnostic", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI},
	})
	if beforeInitialize.Error == nil || beforeInitialize.Error.Code != lspRPCServerNotInitialized {
		t.Fatalf("pre-initialize request = %+v", beforeInitialize)
	}
	unknown := testLSPRequest(t, adapter, "opaque-id", "unknown/method", map[string]any{})
	if unknown.Error == nil || unknown.Error.Code != lspRPCMethodNotFound ||
		string(unknown.ID) != `"opaque-id"` {
		t.Fatalf("unknown method = %+v", unknown)
	}

	response, err := adapter.HandleJSONRPC(context.Background(), marshalTestLSPNotification(
		t, "textDocument/completion", testLSPPositionParams(testLSPDocumentURI, 0, 0),
	))
	if response != nil || !errors.Is(err, errLSPRPCInvalidRequest) {
		t.Fatalf("request-shaped notification response=%s error=%v", response, err)
	}
	response, err = adapter.HandleJSONRPC(context.Background(), marshalTestLSPNotification(
		t, "unknown/notification", map[string]any{},
	))
	if response != nil || !errors.Is(err, errLSPRPCMethodNotFound) {
		t.Fatalf("unknown notification response=%s error=%v", response, err)
	}

	initializeTestLSPAdapter(t, adapter)
	repeated := testLSPRequest(t, adapter, 22, "initialize", map[string]any{})
	if repeated.Error == nil || repeated.Error.Code != lspRPCInvalidRequest {
		t.Fatalf("repeated initialize = %+v", repeated)
	}
	response, err = adapter.HandleJSONRPC(context.Background(), marshalTestLSPNotification(
		t, "initialized", map[string]any{"unknown": true},
	))
	if response != nil || err == nil {
		t.Fatalf("invalid initialized response=%s error=%v", response, err)
	}

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	response, err = adapter.HandleJSONRPC(cancelledContext, marshalTestLSPRequest(
		t, 23, "textDocument/diagnostic", map[string]any{
			"textDocument": map[string]any{"uri": testLSPDocumentURI},
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	cancelled := decodeTestLSPResponse(t, response)
	if cancelled.Error == nil || cancelled.Error.Code != lspRPCRequestCancelled {
		t.Fatalf("cancelled request = %+v", cancelled)
	}
}

func TestLSPAdapterBoundsRequestsIDsAndConstruction(t *testing.T) {
	if _, err := NewLSPAdapter(nil, LSPAdapterOptions{}); err == nil {
		t.Fatal("nil catalog was accepted")
	}
	if _, _, err := func() (*LSPAdapter, *resolve.Catalog, error) {
		catalog := testCatalog(t)
		adapter, adapterErr := NewLSPAdapter(catalog, LSPAdapterOptions{
			Limits: LSPAdapterLimits{MaxResponseBytes: 100},
		})
		return adapter, catalog, adapterErr
	}(); err == nil {
		t.Fatal("response bound too small for protocol errors was accepted")
	}

	adapter, _ := newTestLSPAdapter(t, Limits{MaxSourceBytes: 64}, LSPAdapterLimits{
		MaxRequestBytes: 1088, MaxIDBytes: 8,
	})
	oversized := bytes.Repeat([]byte{' '}, adapter.limits.MaxRequestBytes+1)
	response, err := adapter.HandleJSONRPC(context.Background(), oversized)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeTestLSPResponse(t, response)
	if decoded.Error == nil || decoded.Error.Code != lspRPCInvalidRequest {
		t.Fatalf("oversized request = %+v", decoded)
	}
	response, err = adapter.HandleJSONRPC(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":"123456789","method":"initialize","params":{}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	decoded = decodeTestLSPResponse(t, response)
	if decoded.Error == nil || decoded.Error.Code != lspRPCInvalidRequest {
		t.Fatalf("oversized id = %+v", decoded)
	}

	boundedResponse, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{
		MaxResponseBytes: 1025, MaxIDBytes: 1,
	})
	initialized := testLSPRequest(t, boundedResponse, 1, "initialize", map[string]any{})
	if initialized.Error != nil {
		t.Fatalf("bounded initialize = %+v", initialized)
	}
	testLSPNotification(t, boundedResponse, "initialized", map[string]any{})
	openTestLSPDocument(t, boundedResponse, testLSPDocumentURI, 1, validSource)
	boundedCompletion := testLSPRequest(t, boundedResponse, 2, "textDocument/completion",
		testLSPPositionParams(testLSPDocumentURI, 1, 10))
	if boundedCompletion.Error == nil || boundedCompletion.Error.Code != lspRPCInternalError {
		t.Fatalf("oversized completion response = %+v", boundedCompletion)
	}
}

func TestLSPAdapterSynchronizationIsTransactionalAndCatalogFrozen(t *testing.T) {
	maximumSource := len(validSource) + 16
	adapter, sourceCatalog := newTestLSPAdapter(t,
		Limits{MaxSourceBytes: maximumSource},
		LSPAdapterLimits{MaxRequestBytes: maximumSource + 1024, MaxDocuments: 1},
	)
	if err := sourceCatalog.Register(auxiliaryDescriptor()); err != nil {
		t.Fatal(err)
	}
	initializeTestLSPAdapter(t, adapter)
	openTestLSPDocument(t, adapter, testLSPDocumentURI, 1, validSource)
	original, err := adapter.document(testLSPDocumentURI)
	if err != nil {
		t.Fatal(err)
	}

	completion := decodeTestLSPResult[LSPCompletionList](t,
		testLSPRequest(t, adapter, 31, "textDocument/completion", testLSPPositionParams(
			testLSPDocumentURI, 1, 10,
		)))
	if len(completion.Items) != 2 {
		t.Fatalf("post-construction catalog registration reached adapter: %+v", completion)
	}

	assertTestLSPNotificationError(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": testLSPDocumentURI, "version": 1},
		"contentChanges": []any{map[string]any{"text": validSource}},
	}, ErrLSPVersion)
	assertTestLSPNotificationError(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI, "version": 2},
		"contentChanges": []any{map[string]any{
			"range": map[string]any{
				"start": map[string]any{"line": 0, "character": 0},
				"end":   map[string]any{"line": 0, "character": 0},
			},
			"text": "x",
		}},
	}, nil)
	assertTestLSPNotificationError(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI, "version": 2},
		"contentChanges": []any{
			map[string]any{"text": validSource}, map[string]any{"text": validSource},
		},
	}, nil)
	assertTestLSPNotificationError(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": testLSPDocumentURI, "version": 2},
		"contentChanges": []any{map[string]any{"text": strings.Repeat("x", maximumSource+1)}},
	}, nil)

	retained, err := adapter.document(testLSPDocumentURI)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Version != original.Version || retained.Snapshot != original.Snapshot ||
		retained.Snapshot.SourceDigest() != original.Snapshot.SourceDigest() {
		t.Fatalf("failed change mutated document: before=%+v after=%+v", original, retained)
	}

	unformatted := strings.Replace(validSource, "test.Source ::", "test.Source  ::", 1)
	testLSPNotification(t, adapter, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": testLSPDocumentURI, "version": 2},
		"contentChanges": []any{map[string]any{"text": unformatted}},
	})
	changed, err := adapter.document(testLSPDocumentURI)
	if err != nil || changed.Version != 2 || changed.Snapshot == original.Snapshot ||
		changed.Snapshot.SourceDigest() == original.Snapshot.SourceDigest() {
		t.Fatalf("successful change = %+v, %v", changed, err)
	}
	if err := adapter.finishRPCQuery(context.Background(), original); !errors.Is(err, ErrLSPVersion) {
		t.Fatalf("stale query lease = %v", err)
	}

	assertTestLSPNotificationError(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": "file:///workspace/second.ortg", "languageId": LSPDocumentLanguageID,
			"version": 1, "text": validSource,
		},
	}, nil)
	assertTestLSPNotificationError(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": testLSPDocumentURI, "languageId": LSPDocumentLanguageID,
			"version": 3, "text": validSource,
		},
	}, ErrLSPDocumentOpen)
	assertTestLSPNotificationError(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": "relative.ortg", "languageId": LSPDocumentLanguageID,
			"version": 1, "text": validSource,
		},
	}, ErrInvalidPosition)
	assertTestLSPNotificationError(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": "file:///workspace/third.ortg", "languageId": "plaintext",
			"version": 1, "text": validSource,
		},
	}, nil)
	if adapter.DocumentCount() != 1 {
		t.Fatalf("failed opens changed document count = %d", adapter.DocumentCount())
	}

	adapter.Dispose()
	if adapter.DocumentCount() != 0 {
		t.Fatalf("dispose retained %d documents", adapter.DocumentCount())
	}
	if _, err := adapter.document(testLSPDocumentURI); !errors.Is(err, ErrLSPDisposed) {
		t.Fatalf("disposed document lookup = %v", err)
	}
}

func assertTestLSPNotificationError(
	t testing.TB, adapter *LSPAdapter, method string, params any, target error,
) {
	t.Helper()
	response, err := adapter.HandleJSONRPC(
		context.Background(), marshalTestLSPNotification(t, method, params),
	)
	if response != nil || err == nil || (target != nil && !errors.Is(err, target)) {
		t.Fatalf("notification %q response=%s error=%v, want target %v",
			method, response, err, target)
	}
}

func TestLSPAdapterConcurrentReadersObserveOneVersionOrContentModified(t *testing.T) {
	adapter, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{})
	initializeTestLSPAdapter(t, adapter)
	openTestLSPDocument(t, adapter, testLSPDocumentURI, 1, validSource)

	diagnosticRequest := marshalTestLSPRequest(t, 41, "textDocument/diagnostic", map[string]any{
		"textDocument": map[string]any{"uri": testLSPDocumentURI},
	})
	completionRequest := marshalTestLSPRequest(t, 42, "textDocument/completion",
		testLSPPositionParams(testLSPDocumentURI, 1, 10))
	const changes = 40
	changeRequests := make([][]byte, changes)
	for index := range changes {
		text := validSource
		if index%2 == 0 {
			text = strings.ReplaceAll(validSource, "producer", "cameraaa")
		}
		changeRequests[index] = marshalTestLSPNotification(t, "textDocument/didChange", map[string]any{
			"textDocument": map[string]any{
				"uri": testLSPDocumentURI, "version": index + 2,
			},
			"contentChanges": []any{map[string]any{"text": text}},
		})
	}

	failures := make(chan error, 32)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for index, request := range changeRequests {
			response, err := adapter.HandleJSONRPC(context.Background(), request)
			if err != nil || response != nil {
				failures <- fmt.Errorf("change %d response=%s error=%v", index, response, err)
				return
			}
		}
	}()
	for reader := range 8 {
		wait.Add(1)
		go func(reader int) {
			defer wait.Done()
			for iteration := range 80 {
				request := diagnosticRequest
				if (reader+iteration)%2 != 0 {
					request = completionRequest
				}
				response, err := adapter.HandleJSONRPC(context.Background(), request)
				if err != nil {
					failures <- err
					return
				}
				if strictjson.Validate(response) != nil {
					failures <- fmt.Errorf("reader %d received invalid JSON: %s", reader, response)
					return
				}
				var decoded lspRPCResponse
				if json.Unmarshal(response, &decoded) != nil {
					failures <- fmt.Errorf("reader %d could not decode response", reader)
					return
				}
				if decoded.Error != nil && decoded.Error.Code != lspRPCContentModified {
					failures <- fmt.Errorf("reader %d response error = %+v", reader, decoded.Error)
					return
				}
			}
		}(reader)
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	latest, err := adapter.document(testLSPDocumentURI)
	if err != nil || latest.Version != changes+1 {
		t.Fatalf("latest document = %+v, %v", latest, err)
	}
}

func BenchmarkLSPAdapterJSONRPC(b *testing.B) {
	adapter, _ := newTestLSPAdapter(b, Limits{}, LSPAdapterLimits{})
	initializeTestLSPAdapter(b, adapter)
	openTestLSPDocument(b, adapter, testLSPDocumentURI, 1, validSource)
	requests := map[string][]byte{
		"diagnostics": marshalTestLSPRequest(b, 51, "textDocument/diagnostic", map[string]any{
			"textDocument": map[string]any{"uri": testLSPDocumentURI},
		}),
		"completion": marshalTestLSPRequest(b, 52, "textDocument/completion",
			testLSPPositionParams(testLSPDocumentURI, 1, 10)),
	}
	for name, request := range requests {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				response, err := adapter.HandleJSONRPC(context.Background(), request)
				if err != nil || len(response) == 0 {
					b.Fatalf("response=%s error=%v", response, err)
				}
			}
		})
	}
}

func newTestLSPAdapter(
	t testing.TB, editorLimits Limits, adapterLimits LSPAdapterLimits,
) (*LSPAdapter, *resolve.Catalog) {
	t.Helper()
	catalog := testCatalog(t)
	resolver := editorSchemaResolver(func(_ context.Context, reference string) (schema.ResolvedSchema, error) {
		if reference != "schema://test/source-config/v1" {
			return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
		}
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/source-config-v1.json",
			Document: json.RawMessage(lspMetadataSchema),
		}, nil
	})
	adapter, err := NewLSPAdapter(catalog, LSPAdapterOptions{
		Editor: Options{Limits: editorLimits, SchemaResolver: resolver},
		Limits: adapterLimits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter, catalog
}

func initializeTestLSPAdapter(t testing.TB, adapter *LSPAdapter) {
	t.Helper()
	response := testLSPRequest(t, adapter, "initialize", "initialize", map[string]any{
		"processId": nil,
		"rootUri":   "file:///ignored-workspace",
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
		},
		"workspaceFolders": []any{map[string]any{
			"uri": "file:///ignored-workspace", "name": "ignored",
		}},
	})
	result := decodeTestLSPResult[lspRPCInitializeResult](t, response)
	capabilities := result.Capabilities
	if capabilities.PositionEncoding != "utf-16" ||
		!capabilities.TextDocumentSync.OpenClose || capabilities.TextDocumentSync.Change != 1 ||
		capabilities.DiagnosticProvider.InterFileDependencies ||
		capabilities.DiagnosticProvider.WorkspaceDiagnostics ||
		!capabilities.HoverProvider || !capabilities.DefinitionProvider ||
		!capabilities.RenameProvider || !capabilities.DocumentFormattingProvider ||
		result.ServerInfo.Name != "OpenRealtime" {
		t.Fatalf("initialize result = %+v", result)
	}
	testLSPNotification(t, adapter, "initialized", map[string]any{})
}

func openTestLSPDocument(
	t testing.TB, adapter *LSPAdapter, uri string, version int, text string,
) {
	t.Helper()
	testLSPNotification(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": LSPDocumentLanguageID,
			"version": version, "text": text,
		},
	})
}

func testLSPPositionParams(uri string, line, character int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}
}

func marshalTestLSPRequest(
	t testing.TB, id any, method string, params any,
) []byte {
	t.Helper()
	fields := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		fields["params"] = params
	} else {
		fields["params"] = nil
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func marshalTestLSPNotification(t testing.TB, method string, params any) []byte {
	t.Helper()
	fields := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		fields["params"] = params
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func testLSPRequest(
	t testing.TB, adapter *LSPAdapter, id any, method string, params any,
) lspRPCResponse {
	t.Helper()
	response, err := adapter.HandleJSONRPC(
		context.Background(), marshalTestLSPRequest(t, id, method, params),
	)
	if err != nil {
		t.Fatal(err)
	}
	return decodeTestLSPResponse(t, response)
}

func testLSPNotification(
	t testing.TB, adapter *LSPAdapter, method string, params any,
) {
	t.Helper()
	response, err := adapter.HandleJSONRPC(
		context.Background(), marshalTestLSPNotification(t, method, params),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response != nil {
		t.Fatalf("notification %q returned %s", method, response)
	}
}

func decodeTestLSPResponse(t testing.TB, source []byte) lspRPCResponse {
	t.Helper()
	if len(source) == 0 {
		t.Fatal("LSP request returned no response")
	}
	if err := strictjson.Validate(source); err != nil {
		t.Fatalf("invalid LSP response %q: %v", source, err)
	}
	var response lspRPCResponse
	if err := json.Unmarshal(source, &response); err != nil {
		t.Fatal(err)
	}
	if response.JSONRPC != "2.0" || len(response.ID) == 0 ||
		(response.Error == nil) == (len(response.Result) == 0) {
		t.Fatalf("invalid LSP response envelope: %s", source)
	}
	return response
}

func decodeTestLSPResult[T any](t testing.TB, response lspRPCResponse) T {
	t.Helper()
	var result T
	if response.Error != nil {
		t.Fatalf("unexpected LSP error: %+v", response.Error)
	}
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
