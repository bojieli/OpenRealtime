package editor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const workspaceChildSource = `graph pair {
    test.Source :: producer;
    test.Sink :: consumer;
    producer.out -> consumer.in;
    input start = producer.start;
    output done = consumer.done;
}
`

const workspaceRootSource = `import "lib/pair.ortg" as lib;

graph root {
    lib.pair :: middle;
    input start = middle.start;
    output done = middle.done;
}
`

const workspaceTwoImportRootSource = `import "lib/pair.ortg" as lib;
import "other/pair.ortg" as other;

graph root {
    lib.pair :: middle;
    input start = middle.start;
    output done = middle.done;
}
`

func TestWorkspaceIndexNavigatesImportsSubgraphsAndBoundaries(t *testing.T) {
	root := testWorkspaceDocument(t,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 7, workspaceRootSource)
	child := testWorkspaceDocument(t,
		"/workspace/lib/pair.ortg", "file:///workspace/lib/pair.ortg", 11, workspaceChildSource)
	index, err := NewWorkspaceIndex([]WorkspaceDocument{root, child}, WorkspaceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(index.Fingerprint(), "sha256:") || len(index.Documents()) != 2 {
		t.Fatalf("workspace identity = %q, documents = %+v", index.Fingerprint(), index.Documents())
	}
	reordered, err := NewWorkspaceIndex([]WorkspaceDocument{child, root}, WorkspaceLimits{})
	if err != nil || reordered.Fingerprint() != index.Fingerprint() ||
		!reflect.DeepEqual(reordered.Documents(), index.Documents()) {
		t.Fatalf("reordered workspace = %q %+v, %v",
			reordered.Fingerprint(), reordered.Documents(), err)
	}
	newVersion := child
	newVersion.Identity.Version++
	versioned, err := NewWorkspaceIndex([]WorkspaceDocument{root, newVersion}, WorkspaceLimits{})
	if err != nil || versioned.Fingerprint() == index.Fingerprint() {
		t.Fatalf("target version did not change workspace identity: %q, %v",
			versioned.Fingerprint(), err)
	}

	tests := []struct {
		name         string
		needle       string
		delta        int
		kind         WorkspaceDefinitionKind
		originText   string
		targetText   string
		targetPrefix string
	}{
		{"import-path", `"lib/pair.ortg"`, 3, WorkspaceImportDefinition, `"lib/pair.ortg"`, "pair", "graph pair"},
		{"import-alias", " as lib;", 5, WorkspaceImportDefinition, "lib", "pair", "graph pair"},
		{"subgraph", "lib.pair", 2, WorkspaceSubgraphDefinition, "lib.pair", "pair", "graph pair"},
		{"input-boundary", "middle.start", len("middle."), WorkspaceBoundaryDefinition, "start", "start", "input start"},
		{"output-boundary", "middle.done", len("middle."), WorkspaceBoundaryDefinition, "done", "done", "output done"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			position := testWorkspacePosition(t, root.Snapshot, test.needle, test.delta)
			definition, handled, err := index.Definition(root.Identity, position)
			if err != nil || !handled || definition.Kind != test.kind ||
				definition.WorkspaceDigest != index.Fingerprint() ||
				definition.TargetDocument != child.Identity ||
				lspRangeText(t, workspaceRootSource, definition.OriginSelectionRange) != test.originText ||
				lspRangeText(t, workspaceChildSource, definition.TargetSelectionRange) != test.targetText ||
				!strings.HasPrefix(lspRangeText(t, workspaceChildSource, definition.TargetRange), test.targetPrefix) {
				t.Fatalf("definition = %+v, handled=%v, error=%v", definition, handled, err)
			}
		})
	}

	nodePosition := testWorkspacePosition(t, root.Snapshot, "middle.start", 2)
	if definition, handled, err := index.Definition(root.Identity, nodePosition); err != nil || handled {
		t.Fatalf("same-document node definition = %+v, handled=%v, error=%v",
			definition, handled, err)
	}
	ordinaryPosition := testWorkspacePosition(t, child.Snapshot, "test.Source", 2)
	if definition, handled, err := index.Definition(child.Identity, ordinaryPosition); err != nil || handled {
		t.Fatalf("descriptor definition = %+v, handled=%v, error=%v", definition, handled, err)
	}

	documents := index.Documents()
	documents[0].URI = "mutated"
	if index.Documents()[0].URI == "mutated" {
		t.Fatal("workspace document identities retained caller mutation")
	}
	stale := root.Identity
	stale.Version++
	if _, _, err := index.Definition(stale,
		testWorkspacePosition(t, root.Snapshot, "lib.pair", 1)); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale workspace identity = %v", err)
	}
}

func TestWorkspaceIndexSupportsNestedAndDefaultAliasNavigation(t *testing.T) {
	leafSource := strings.Replace(workspaceChildSource, "graph pair", "graph leaf", 1)
	childSource := `import "leaf.ortg" as inner;

graph child {
    inner.leaf :: nested;
    input start = nested.start;
    output done = nested.done;
}
`
	rootSource := `import "lib/child.ortg";

graph root {
    child.child :: outer;
    input start = outer.start;
    output done = outer.done;
}
`
	root := testWorkspaceDocument(t, "/workspace/root.ortg", "file:///workspace/root.ortg", 1, rootSource)
	child := testWorkspaceDocument(t, "/workspace/lib/child.ortg", "file:///workspace/lib/child.ortg", 2, childSource)
	leaf := testWorkspaceDocument(t, "/workspace/lib/leaf.ortg", "file:///workspace/lib/leaf.ortg", 3, leafSource)
	index, err := NewWorkspaceIndex([]WorkspaceDocument{leaf, root, child}, WorkspaceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	rootDefinition, handled, err := index.Definition(root.Identity,
		testWorkspacePosition(t, root.Snapshot, "child.child", 2))
	if err != nil || !handled || rootDefinition.TargetDocument != child.Identity ||
		lspRangeText(t, childSource, rootDefinition.TargetSelectionRange) != "child" {
		t.Fatalf("root definition = %+v, handled=%v, error=%v", rootDefinition, handled, err)
	}
	childDefinition, handled, err := index.Definition(child.Identity,
		testWorkspacePosition(t, child.Snapshot, "inner.leaf", 2))
	if err != nil || !handled || childDefinition.TargetDocument != leaf.Identity ||
		lspRangeText(t, leafSource, childDefinition.TargetSelectionRange) != "leaf" {
		t.Fatalf("child definition = %+v, handled=%v, error=%v", childDefinition, handled, err)
	}
}

func TestWorkspaceIndexImportRangesUseUTF16WithoutSplittingSurrogates(t *testing.T) {
	rootSource := strings.Replace(workspaceRootSource, "lib/pair.ortg", "lib/😀.ortg", 1)
	root := testWorkspaceDocument(t,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 1, rootSource)
	child := testWorkspaceDocument(t,
		"/workspace/lib/😀.ortg", "file:///workspace/lib/%F0%9F%98%80.ortg", 1, workspaceChildSource)
	index, err := NewWorkspaceIndex([]WorkspaceDocument{root, child}, WorkspaceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	definition, handled, err := index.Definition(root.Identity, LSPPosition{Line: 0, Character: 12})
	if err != nil || !handled || definition.TargetDocument != child.Identity ||
		lspRangeText(t, rootSource, definition.OriginSelectionRange) != `"lib/😀.ortg"` {
		t.Fatalf("Unicode import definition = %+v, handled=%v, error=%v",
			definition, handled, err)
	}
	if _, _, err := index.Definition(root.Identity, LSPPosition{Line: 0, Character: 13}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("split-surrogate workspace position = %v", err)
	}
}

func TestWorkspaceIndexFailsClosedOnMissingAmbiguousAndBoundedInputs(t *testing.T) {
	root := testWorkspaceDocument(t,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 1, workspaceRootSource)
	child := testWorkspaceDocument(t,
		"/workspace/lib/pair.ortg", "file:///workspace/lib/pair.ortg", 1, workspaceChildSource)
	missing, err := NewWorkspaceIndex([]WorkspaceDocument{root}, WorkspaceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		needle string
		delta  int
	}{{`"lib/pair.ortg"`, 2}, {"lib.pair", 2}, {"middle.start", len("middle.")}} {
		_, handled, err := missing.Definition(root.Identity,
			testWorkspacePosition(t, root.Snapshot, test.needle, test.delta))
		if !handled || !errors.Is(err, ErrDefinitionMissing) {
			t.Fatalf("missing %q handled=%v error=%v", test.needle, handled, err)
		}
	}

	secondChild := testWorkspaceDocument(t,
		"/workspace/other/pair.ortg", "file:///workspace/other/pair.ortg", 1, workspaceChildSource)
	ambiguousSource := `import "lib/pair.ortg" as lib;
import "other/pair.ortg" as lib;

graph root {
    lib.pair :: middle;
    input start = middle.start;
    output done = middle.done;
}
`
	ambiguousRoot := testWorkspaceDocument(t,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 2, ambiguousSource)
	ambiguous, err := NewWorkspaceIndex(
		[]WorkspaceDocument{ambiguousRoot, child, secondChild}, WorkspaceLimits{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, handled, err := ambiguous.Definition(ambiguousRoot.Identity,
		testWorkspacePosition(t, ambiguousRoot.Snapshot, "lib.pair", 2))
	if !handled || !errors.Is(err, ErrWorkspaceAmbiguous) {
		t.Fatalf("ambiguous target handled=%v error=%v", handled, err)
	}

	for name, documentsAndLimits := range map[string]struct {
		documents []WorkspaceDocument
		limits    WorkspaceLimits
	}{
		"documents": {[]WorkspaceDocument{root, child}, WorkspaceLimits{MaxDocuments: 1}},
		"bytes": {[]WorkspaceDocument{root, child}, WorkspaceLimits{
			MaxTotalSourceBytes: len(workspaceRootSource) + len(workspaceChildSource) - 1,
		}},
		"imports": {[]WorkspaceDocument{ambiguousRoot, child, secondChild}, WorkspaceLimits{MaxImports: 1}},
		"path":    {[]WorkspaceDocument{root}, WorkspaceLimits{MaxPathBytes: 3}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWorkspaceIndex(documentsAndLimits.documents, documentsAndLimits.limits); !errors.Is(err, ErrWorkspaceLimit) && !errors.Is(err, ErrWorkspaceIdentity) {
				t.Fatalf("bound error = %v", err)
			}
		})
	}
	duplicateURI := testWorkspaceDocument(t,
		"/workspace/duplicate.ortg", child.Identity.URI, 1, workspaceChildSource)
	if _, err := NewWorkspaceIndex([]WorkspaceDocument{child, duplicateURI}, WorkspaceLimits{}); !errors.Is(err, ErrWorkspaceAmbiguous) {
		t.Fatalf("duplicate URI error = %v", err)
	}
	duplicatePath := testWorkspaceDocument(t,
		child.Identity.Path, "file:///workspace/duplicate.ortg", 1, workspaceChildSource)
	if _, err := NewWorkspaceIndex([]WorkspaceDocument{child, duplicatePath}, WorkspaceLimits{}); !errors.Is(err, ErrWorkspaceAmbiguous) {
		t.Fatalf("duplicate path error = %v", err)
	}
	if _, err := NewWorkspaceIndex(nil, WorkspaceLimits{MaxDocuments: -1}); err == nil {
		t.Fatal("negative workspace bound was accepted")
	}

	externalSource := strings.Replace(workspaceRootSource, "lib/pair.ortg", "https://example.test/pair.ortg", 1)
	external := testWorkspaceDocument(t,
		"/workspace/external.ortg", "file:///workspace/external.ortg", 1, externalSource)
	if _, err := NewWorkspaceIndex([]WorkspaceDocument{external}, WorkspaceLimits{}); !errors.Is(err, ErrWorkspaceIdentity) {
		t.Fatalf("absolute import URI error = %v", err)
	}
}

func TestWorkspaceIndexRejectsUnindexableAndUnresolvedSymbols(t *testing.T) {
	child := testWorkspaceDocument(t,
		"/workspace/lib/pair.ortg", "file:///workspace/lib/pair.ortg", 1, workspaceChildSource)
	for name, source := range map[string]string{
		"noncanonical": strings.Replace(workspaceRootSource, "lib.pair ::", "lib.pair  ::", 1),
		"recovered": `import "lib/pair.ortg" as lib;

graph root {
    lib.pair :: middle;
`,
	} {
		t.Run(name, func(t *testing.T) {
			root := testWorkspaceDocument(t,
				"/workspace/root.ortg", "file:///workspace/root.ortg", 1, source)
			index, err := NewWorkspaceIndex([]WorkspaceDocument{root, child}, WorkspaceLimits{})
			if err != nil {
				t.Fatal(err)
			}
			_, handled, err := index.Definition(root.Identity,
				testWorkspacePosition(t, root.Snapshot, "lib.pair", 2))
			if handled || !errors.Is(err, ErrSyntaxUnavailable) {
				t.Fatalf("unindexable definition handled=%v error=%v", handled, err)
			}
		})
	}

	for name, source := range map[string]string{
		"wrong-graph":     strings.Replace(workspaceRootSource, "lib.pair", "lib.wrong", 1),
		"absent-boundary": strings.Replace(workspaceRootSource, "middle.start", "middle.missing", 1),
	} {
		t.Run(name, func(t *testing.T) {
			root := testWorkspaceDocument(t,
				"/workspace/root.ortg", "file:///workspace/root.ortg", 1, source)
			index, err := NewWorkspaceIndex([]WorkspaceDocument{root, child}, WorkspaceLimits{})
			if err != nil {
				t.Fatal(err)
			}
			needle, delta := "lib.wrong", 2
			if name == "absent-boundary" {
				needle, delta = "middle.missing", len("middle.")
			}
			_, handled, err := index.Definition(root.Identity,
				testWorkspacePosition(t, root.Snapshot, needle, delta))
			if !handled || !errors.Is(err, ErrDefinitionMissing) {
				t.Fatalf("unresolved definition handled=%v error=%v", handled, err)
			}
		})
	}
}

func TestWorkspaceIndexConcurrentReadersAreDeterministic(t *testing.T) {
	root := testWorkspaceDocument(t,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 7, workspaceRootSource)
	child := testWorkspaceDocument(t,
		"/workspace/lib/pair.ortg", "file:///workspace/lib/pair.ortg", 11, workspaceChildSource)
	index, err := NewWorkspaceIndex([]WorkspaceDocument{root, child}, WorkspaceLimits{})
	if err != nil {
		t.Fatal(err)
	}
	position := testWorkspacePosition(t, root.Snapshot, "middle.start", len("middle."))
	want, handled, err := index.Definition(root.Identity, position)
	if err != nil || !handled {
		t.Fatalf("baseline = %+v, %v, %v", want, handled, err)
	}
	var wait sync.WaitGroup
	failures := make(chan error, 16)
	for reader := range 16 {
		wait.Add(1)
		go func(reader int) {
			defer wait.Done()
			for range 100 {
				got, handled, err := index.Definition(root.Identity, position)
				if err != nil || !handled || got != want {
					failures <- fmt.Errorf("reader %d got %+v, %v, %v", reader, got, handled, err)
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
}

func TestLSPAdapterProjectsDigestBoundWorkspaceDefinitions(t *testing.T) {
	adapter, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{})
	initializeTestLSPAdapter(t, adapter)
	rootURI := "file:///workspace/root.ortg"
	childURI := "file:///workspace/lib/pair.ortg"
	openTestLSPDocument(t, adapter, rootURI, 7, workspaceRootSource)
	openTestLSPDocument(t, adapter, childURI, 11, workspaceChildSource)

	tests := []struct {
		name         string
		needle       string
		delta        int
		targetURI    string
		targetText   string
		targetPrefix string
	}{
		{"import", `"lib/pair.ortg"`, 3, childURI, "pair", "graph pair"},
		{"subgraph", "lib.pair", 2, childURI, "pair", "graph pair"},
		{"boundary", "middle.start", len("middle."), childURI, "start", "input start"},
		{"same-document-node", "middle.start", 2, rootURI, "middle", "middle"},
	}
	rootDocument, err := adapter.document(rootURI)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			position := testWorkspacePosition(t, rootDocument.Snapshot, test.needle, test.delta)
			links := decodeTestLSPResult[[]LSPLocationLink](t,
				testLSPRequest(t, adapter, test.name, "textDocument/definition", map[string]any{
					"textDocument": map[string]any{"uri": rootURI},
					"position": map[string]any{
						"line": position.Line, "character": position.Character,
					},
				}))
			if len(links) != 1 || links[0].TargetURI != test.targetURI {
				t.Fatalf("links = %+v", links)
			}
			targetSource := workspaceChildSource
			if test.targetURI == rootURI {
				targetSource = workspaceRootSource
			}
			if lspRangeText(t, targetSource, links[0].TargetSelectionRange) != test.targetText ||
				!strings.HasPrefix(lspRangeText(t, targetSource, links[0].TargetRange), test.targetPrefix) {
				t.Fatalf("target link = %+v", links[0])
			}
		})
	}

	childDocument, err := adapter.document(childURI)
	if err != nil {
		t.Fatal(err)
	}
	descriptorPosition := testWorkspacePosition(t, childDocument.Snapshot, "test.Source", 2)
	descriptorLinks := decodeTestLSPResult[[]LSPLocationLink](t,
		testLSPRequest(t, adapter, 71, "textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": childURI},
			"position": map[string]any{
				"line": descriptorPosition.Line, "character": descriptorPosition.Character,
			},
		}))
	if len(descriptorLinks) != 1 ||
		!strings.HasPrefix(descriptorLinks[0].TargetURI, "openrealtime-descriptor:/test.Source@2/") {
		t.Fatalf("descriptor fallback = %+v", descriptorLinks)
	}

	testLSPNotification(t, adapter, "textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": childURI},
	})
	missingPosition := testWorkspacePosition(t, rootDocument.Snapshot, "lib.pair", 2)
	missingLinks := decodeTestLSPResult[[]LSPLocationLink](t,
		testLSPRequest(t, adapter, 72, "textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": rootURI},
			"position": map[string]any{
				"line": missingPosition.Line, "character": missingPosition.Character,
			},
		}))
	if missingLinks == nil || len(missingLinks) != 0 {
		t.Fatalf("missing imported document links = %+v", missingLinks)
	}
}

func TestLSPAdapterEnforcesAggregateWorkspaceBoundsTransactionally(t *testing.T) {
	catalog := testCatalog(t)
	adapter, err := NewLSPAdapter(catalog, LSPAdapterOptions{
		Workspace: WorkspaceLimits{
			MaxDocuments: 128, MaxTotalSourceBytes: len(workspaceRootSource) - 1,
			MaxImports: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	initializeTestLSPAdapter(t, adapter)
	assertTestLSPNotificationError(t, adapter, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": "file:///workspace/root.ortg", "languageId": LSPDocumentLanguageID,
			"version": 1, "text": workspaceRootSource,
		},
	}, ErrWorkspaceLimit)
	if adapter.DocumentCount() != 0 || adapter.workspaceBytes != 0 || adapter.workspaceImports != 0 {
		t.Fatalf("rejected open changed workspace count=%d bytes=%d imports=%d",
			adapter.DocumentCount(), adapter.workspaceBytes, adapter.workspaceImports)
	}

	bounded, err := NewLSPAdapter(catalog, LSPAdapterOptions{
		Workspace: WorkspaceLimits{
			MaxDocuments:        128,
			MaxTotalSourceBytes: len(workspaceRootSource)*2 + 64,
			MaxImports:          1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	initializeTestLSPAdapter(t, bounded)
	openTestLSPDocument(t, bounded, "file:///workspace/first.ortg", 1, workspaceRootSource)
	assertTestLSPNotificationError(t, bounded, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": "file:///workspace/second.ortg", "languageId": LSPDocumentLanguageID,
			"version": 1, "text": workspaceRootSource,
		},
	}, ErrWorkspaceLimit)
	if bounded.DocumentCount() != 1 || bounded.workspaceImports != 1 {
		t.Fatalf("rejected import open count=%d imports=%d",
			bounded.DocumentCount(), bounded.workspaceImports)
	}
	before, err := bounded.document("file:///workspace/first.ortg")
	if err != nil {
		t.Fatal(err)
	}
	assertTestLSPNotificationError(t, bounded, "textDocument/didChange", map[string]any{
		"textDocument": map[string]any{
			"uri": "file:///workspace/first.ortg", "version": 2,
		},
		"contentChanges": []any{map[string]any{"text": workspaceTwoImportRootSource}},
	}, ErrWorkspaceLimit)
	after, err := bounded.document("file:///workspace/first.ortg")
	if err != nil || after.Version != before.Version || after.Snapshot != before.Snapshot ||
		bounded.workspaceImports != 1 {
		t.Fatalf("rejected import change before=%+v after=%+v imports=%d error=%v",
			before, after, bounded.workspaceImports, err)
	}
	testLSPNotification(t, bounded, "textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": "file:///workspace/first.ortg"},
	})
	if bounded.workspaceBytes != 0 || bounded.workspaceImports != 0 {
		t.Fatalf("close retained bytes=%d imports=%d", bounded.workspaceBytes, bounded.workspaceImports)
	}
	openTestLSPDocument(t, bounded, "file:///workspace/second.ortg", 1, workspaceRootSource)
}

func TestLSPAdapterWorkspaceDefinitionsRaceWithTargetChanges(t *testing.T) {
	adapter, _ := newTestLSPAdapter(t, Limits{}, LSPAdapterLimits{})
	initializeTestLSPAdapter(t, adapter)
	rootURI := "file:///workspace/root.ortg"
	childURI := "file:///workspace/lib/pair.ortg"
	openTestLSPDocument(t, adapter, rootURI, 1, workspaceRootSource)
	openTestLSPDocument(t, adapter, childURI, 1, workspaceChildSource)
	root, err := adapter.document(rootURI)
	if err != nil {
		t.Fatal(err)
	}
	position := testWorkspacePosition(t, root.Snapshot, "middle.start", len("middle."))
	request := marshalTestLSPRequest(t, 81, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": rootURI},
		"position": map[string]any{
			"line": position.Line, "character": position.Character,
		},
	})
	const changes = 30
	changeRequests := make([][]byte, changes)
	for index := range changes {
		text := workspaceChildSource
		if index%2 == 0 {
			text = strings.ReplaceAll(workspaceChildSource, "producer", "cameraaa")
		}
		changeRequests[index] = marshalTestLSPNotification(t, "textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": childURI, "version": index + 2},
			"contentChanges": []any{map[string]any{"text": text}},
		})
	}

	failures := make(chan error, 16)
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for index, change := range changeRequests {
			response, err := adapter.HandleJSONRPC(context.Background(), change)
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
			for range 60 {
				response, err := adapter.HandleJSONRPC(context.Background(), request)
				if err != nil {
					failures <- fmt.Errorf("reader %d: %w", reader, err)
					return
				}
				decoded := decodeWorkspaceRaceResponse(response)
				if decoded != 0 && decoded != lspRPCContentModified {
					failures <- fmt.Errorf("reader %d code = %d: %s", reader, decoded, response)
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
}

func decodeWorkspaceRaceResponse(source []byte) int {
	var response lspRPCResponse
	if jsonErr := json.Unmarshal(source, &response); jsonErr != nil {
		return lspRPCInternalError
	}
	if response.Error != nil {
		return response.Error.Code
	}
	var links []LSPLocationLink
	if json.Unmarshal(response.Result, &links) != nil || len(links) != 1 {
		return lspRPCInternalError
	}
	return 0
}

func testWorkspaceDocument(
	t testing.TB, path, uri string, version int, source string,
) WorkspaceDocument {
	t.Helper()
	document, err := AnalyzeWithOptions(
		context.Background(), path, []byte(source), testCatalog(t), Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return WorkspaceDocument{
		Identity: LSPDocumentIdentity{
			Path: path, URI: uri, Version: version, SourceDigest: document.SourceDigest(),
		},
		Snapshot: document,
	}
}

func testWorkspacePosition(
	t testing.TB, document *Document, needle string, delta int,
) LSPPosition {
	t.Helper()
	offset := strings.Index(string(document.source), needle)
	if offset < 0 || delta < 0 || offset+delta > len(document.source) {
		t.Fatalf("source has no valid %q + %d", needle, delta)
	}
	span, err := sourceSpan(document.positions, offset+delta, offset+delta)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := document.lspRange(span)
	if err != nil {
		t.Fatal(err)
	}
	return projected.Start
}

func BenchmarkWorkspaceIndexNavigation(b *testing.B) {
	root := testWorkspaceDocument(b,
		"/workspace/root.ortg", "file:///workspace/root.ortg", 7, workspaceRootSource)
	child := testWorkspaceDocument(b,
		"/workspace/lib/pair.ortg", "file:///workspace/lib/pair.ortg", 11, workspaceChildSource)
	documents := []WorkspaceDocument{root, child}
	position := testWorkspacePosition(b, root.Snapshot, "middle.start", len("middle."))
	b.Run("build", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := NewWorkspaceIndex(documents, WorkspaceLimits{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	index, err := NewWorkspaceIndex(documents, WorkspaceLimits{})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("definition", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, handled, err := index.Definition(root.Identity, position); err != nil || !handled {
				b.Fatalf("handled=%v error=%v", handled, err)
			}
		}
	})
	b.Run("lsp-definition", func(b *testing.B) {
		adapter, _ := newTestLSPAdapter(b, Limits{}, LSPAdapterLimits{})
		initializeTestLSPAdapter(b, adapter)
		rootURI := "file:///workspace/root.ortg"
		childURI := "file:///workspace/lib/pair.ortg"
		openTestLSPDocument(b, adapter, rootURI, 1, workspaceRootSource)
		openTestLSPDocument(b, adapter, childURI, 1, workspaceChildSource)
		opened, err := adapter.document(rootURI)
		if err != nil {
			b.Fatal(err)
		}
		position := testWorkspacePosition(b, opened.Snapshot, "middle.start", len("middle."))
		request := marshalTestLSPRequest(b, 91, "textDocument/definition", map[string]any{
			"textDocument": map[string]any{"uri": rootURI},
			"position": map[string]any{
				"line": position.Line, "character": position.Character,
			},
		})
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			response, err := adapter.HandleJSONRPC(context.Background(), request)
			if err != nil || len(response) == 0 {
				b.Fatalf("response=%s error=%v", response, err)
			}
		}
	})
}
