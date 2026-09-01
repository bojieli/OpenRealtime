package editor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const validSource = `graph demo {
    test.Source :: producer;
    test.Sink :: consumer;
    producer.out -> consumer.in;
    input start = producer.start;
    output done = consumer.done;
}
`

func TestLanguageServiceCompletionHoverDefinitionAndMetadata(t *testing.T) {
	document := analyzeValid(t, Limits{})
	if report := document.Diagnostics(); report.Total != 0 || report.Incomplete {
		t.Fatalf("valid diagnostics = %+v", report)
	}
	if !document.Parsed() || !document.Canonical() || document.Path() != "agent.ortg" ||
		!strings.HasPrefix(document.SourceDigest(), "sha256:") {
		t.Fatalf("document identity parsed=%v canonical=%v path=%q digest=%q",
			document.Parsed(), document.Canonical(), document.Path(), document.SourceDigest())
	}

	elementCursor := cursorAt(t, document, validSource, "test.Source", 0)
	elements, err := document.CompleteElements(elementCursor)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(elements); !slices.Equal(labels, []string{"test.Sink", "test.Source"}) {
		t.Fatalf("element completions = %v", labels)
	}
	if got := sourceText(document.Source(), elements.Items[0].Replacement); got != "test.Source" {
		t.Fatalf("element replacement = %q", got)
	}

	nodeCursor := cursorAt(t, document, validSource, "producer.out ->", 0)
	nodes, err := document.CompleteNodes(nodeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(nodes); !slices.Equal(labels, []string{"producer"}) {
		t.Fatalf("node completions = %v", labels)
	}

	portCursor := cursorAt(t, document, validSource, "producer.out ->", len("producer."))
	ports, err := document.CompletePorts(portCursor)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(ports); !slices.Equal(labels, []string{"out", "telemetry"}) {
		t.Fatalf("port completions = %v", labels)
	}
	for _, completion := range ports.Items {
		if completion.Kind != CompletionPort || completion.Node != "producer" ||
			completion.Element != "test.Source" || completion.Type == "" {
			t.Fatalf("port completion = %+v", completion)
		}
	}

	hover, err := document.Hover(portCursor)
	if err != nil {
		t.Fatal(err)
	}
	if hover.Kind != SymbolPortReference || hover.Node != "producer" || hover.Port == nil ||
		hover.Port.Name != "out" || hover.Port.Direction != element.Output ||
		hover.Port.Type.String() != "Event<test.Value>" || hover.Descriptor == nil ||
		hover.Descriptor.Identity.Revision != 2 || hover.Config == nil ||
		hover.Config.ValuesPath != "nodes.producer" ||
		hover.Config.Artifact != valuesArtifact || !hover.Config.Resolved || hover.Config.InlineTopologyValues ||
		hover.Config.SchemaReference != "schema://test/source-config/v1" {
		t.Fatalf("port hover = %+v", hover)
	}

	nodeDefinitions, err := document.Definitions(nodeCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeDefinitions.Items) != 1 || nodeDefinitions.Items[0].Span == nil ||
		sourceText(document.Source(), *nodeDefinitions.Items[0].Span) != "producer" ||
		nodeDefinitions.Items[0].URI != "agent.ortg" {
		t.Fatalf("node definitions = %+v", nodeDefinitions)
	}
	portDefinitions, err := document.Definitions(portCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(portDefinitions.Items) != 1 ||
		!strings.Contains(portDefinitions.Items[0].URI, "test.Source@2/") ||
		!strings.HasSuffix(portDefinitions.Items[0].URI, "#ports/out") {
		t.Fatalf("port definitions = %+v", portDefinitions)
	}
	elementDefinitions, err := document.Definitions(elementCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(elementDefinitions.Items) != 1 || elementDefinitions.Items[0].Kind != SymbolElement ||
		!strings.Contains(elementDefinitions.Items[0].URI, "test.Source@2/") {
		t.Fatalf("element definitions = %+v", elementDefinitions)
	}

	metadata := document.CatalogMetadata()
	if metadata.Total != 2 || metadata.Incomplete ||
		!slices.Equal(metadataNames(metadata), []string{"test.Sink", "test.Source"}) {
		t.Fatalf("catalog metadata = %+v", metadata)
	}
	sourceMetadata := metadata.Elements[1]
	if sourceMetadata.TopologyDeclaration != "test.Source :: <node>;" ||
		sourceMetadata.Config.Artifact != valuesArtifact ||
		!sourceMetadata.Config.Resolved || sourceMetadata.Config.InlineTopologyValues || sourceMetadata.Config.EmptyObjectOnly ||
		sourceMetadata.Config.SchemaReference != "schema://test/source-config/v1" ||
		len(sourceMetadata.Dependencies) != 1 || len(sourceMetadata.Effects) != 1 {
		t.Fatalf("source metadata = %+v", sourceMetadata)
	}
	if role := findMetadataPort(t, sourceMetadata, "start").ReactionRole; role != "trigger" {
		t.Fatalf("start reaction role = %q", role)
	}

	// Every result is recursively independent of the document snapshot.
	metadata.Elements[1].Ports[0].Type.Arguments[0].Name = "mutated.Type"
	metadata.Elements[1].Reaction.Triggers[0] = "mutated"
	hover.Descriptor.Ports[0].Type.Arguments[0].Name = "mutated.Hover"
	again := document.CatalogMetadata().Elements[1]
	if findMetadataPort(t, again, "out").Type.String() != "Event<test.Value>" ||
		again.Reaction.Triggers[0] != "start" {
		t.Fatal("metadata result retained caller aliases")
	}
	againHover, err := document.Hover(portCursor)
	if err != nil || againHover.Port.Type.String() != "Event<test.Value>" {
		t.Fatalf("hover result retained caller aliases: %+v, %v", againHover, err)
	}
}

func TestRenameIsGraphAwareAtomicAndStaleSafe(t *testing.T) {
	document := analyzeValid(t, Limits{})
	cursor := cursorAt(t, document, validSource, "producer.out ->", 2)
	edits, err := document.RenameNode(cursor, "camera")
	if err != nil {
		t.Fatal(err)
	}
	if edits.SourceDigest != document.SourceDigest() || len(edits.Edits) != 3 {
		t.Fatalf("rename edits = %+v", edits)
	}
	byID, err := document.RenameNodeID("producer", "camera")
	if err != nil || !reflect.DeepEqual(byID, edits) {
		t.Fatalf("visual node-ID rename = %+v, %v; want %+v", byID, err, edits)
	}
	for index, edit := range edits.Edits {
		if edit.OldText != "producer" || edit.NewText != "camera" ||
			(index > 0 && edits.Edits[index-1].Span.Start.Offset >= edit.Span.Start.Offset) {
			t.Fatalf("rename edit %d = %+v", index, edit)
		}
	}
	renamed, err := ApplyEdits(document.Source(), edits)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(validSource, "producer", "camera")
	if string(renamed) != want {
		t.Fatalf("renamed source:\n%s\nwant:\n%s", renamed, want)
	}
	updated, err := Analyze("agent.ortg", renamed, testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if report := updated.Diagnostics(); report.Total != 0 {
		t.Fatalf("renamed diagnostics = %+v", report)
	}

	if _, err := document.RenameNode(cursor, "consumer"); !errors.Is(err, ErrRenameCollision) {
		t.Fatalf("rename collision = %v", err)
	}
	if _, err := document.RenameNode(cursor, "bad.name"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("invalid rename = %v", err)
	}
	if _, err := document.RenameNodeID("missing", "camera"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("missing node-ID rename = %v", err)
	}
	portCursor := cursorAt(t, document, validSource, "producer.out ->", len("producer.")+1)
	if _, err := document.RenameNode(portCursor, "renamed"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("port rename = %v", err)
	}
	if _, err := ApplyEdits(append(document.Source(), ' '), edits); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale apply = %v", err)
	}

	// Mutating a returned edit set cannot alter later rename results.
	edits.Edits[0].NewText = "corrupt"
	again, err := document.RenameNode(cursor, "display")
	if err != nil || again.Edits[0].NewText != "display" {
		t.Fatalf("rename retained caller aliases: %+v, %v", again, err)
	}
	noOp, err := document.RenameNode(cursor, "producer")
	if err != nil || len(noOp.Edits) != 0 || noOp.SourceDigest != document.SourceDigest() {
		t.Fatalf("no-op rename = %+v, %v", noOp, err)
	}
}

func TestRemoveEdgeIDIsCanonicalExactAndStaleSafe(t *testing.T) {
	source := `graph demo {
    test.Source :: producer;
    test.Sink :: consumer;
    // removable café edge
    edge optional = producer.out -> consumer.in;
    input start = producer.start;
    output done = consumer.done;
}
`
	document, err := Analyze("agent.ortg", []byte(source), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	edits, err := document.RemoveEdgeID("optional")
	if err != nil || edits.Path != "agent.ortg" || edits.SourceDigest != document.SourceDigest() ||
		len(edits.Edits) != 1 {
		t.Fatalf("edge-removal edits = %+v, %v", edits, err)
	}
	removed, err := ApplyEdits(document.Source(), edits)
	want := `graph demo {
    test.Source :: producer;
    test.Sink :: consumer;
    input start = producer.start;
    output done = consumer.done;
}
`
	if err != nil || string(removed) != want {
		t.Fatalf("edge-removed source:\n%s\nwant:\n%s\nerror: %v", removed, want, err)
	}
	parsed, err := syntax.Parse("agent.ortg", removed)
	if err != nil || syntax.Format(parsed) != string(removed) {
		t.Fatalf("edge-removed source is not canonical: %v", err)
	}
	if _, err := ApplyEdits(append(document.Source(), ' '), edits); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale edge-removal apply = %v", err)
	}
	if _, err := document.RemoveEdgeID("missing"); !errors.Is(err, ErrInvalidEdgeMutation) {
		t.Fatalf("missing edge removal = %v", err)
	}

	unnamed, err := Analyze("unnamed.ortg", []byte(validSource), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	unnamedEdits, err := unnamed.RemoveEdgeID("producer.out->consumer.in")
	if err != nil {
		t.Fatalf("unnamed edge removal = %v", err)
	}
	unnamedResult, err := ApplyEdits(unnamed.Source(), unnamedEdits)
	if err != nil || strings.Contains(string(unnamedResult), "producer.out -> consumer.in") {
		t.Fatalf("unnamed edge remained after removal: %s, %v", unnamedResult, err)
	}
	lossySource := strings.Replace(validSource, "producer.out -> consumer.in", "producer.out => consumer.in", 1)
	lossy, err := Analyze("lossy.ortg", []byte(lossySource), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	lossyEdits, err := lossy.RemoveEdgeID("producer.out->consumer.in")
	if err != nil {
		t.Fatalf("lossy endpoint-derived edge removal = %v", err)
	}
	lossyResult, err := ApplyEdits(lossy.Source(), lossyEdits)
	if err != nil || strings.Contains(string(lossyResult), "producer.out => consumer.in") {
		t.Fatalf("lossy edge remained after removal: %s, %v", lossyResult, err)
	}

	duplicateSource := strings.Replace(source,
		"    input start", "    edge optional = producer.out -> consumer.in;\n    input start", 1)
	duplicate, err := Analyze("duplicate.ortg", []byte(duplicateSource), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duplicate.RemoveEdgeID("optional"); !errors.Is(err, ErrInvalidEdgeMutation) {
		t.Fatalf("ambiguous edge removal = %v", err)
	}
	noncanonical, err := Analyze("noncanonical.ortg",
		[]byte(strings.Replace(source, "edge optional =", "edge  optional =", 1)), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noncanonical.RemoveEdgeID("optional"); !errors.Is(err, ErrSyntaxUnavailable) {
		t.Fatalf("noncanonical edge removal = %v", err)
	}
}

func TestDiagnosticsPositionsCanonicalityAndUnknownContracts(t *testing.T) {
	catalog := testCatalog(t)
	broken := []byte(`graph broken {
    test.Source :: producer;
    producer.missing -> ghost.in;
    input start = producer.start;
}
`)
	first, err := Analyze("broken.ortg", broken, catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	report := first.Diagnostics()
	if report.Total == 0 || report.Items[0].Path != "broken.ortg" {
		t.Fatalf("broken diagnostics = %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 20; iteration++ {
		next, err := Analyze("broken.ortg", broken, catalog, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		other, _ := json.Marshal(next.Diagnostics())
		if !bytes.Equal(encoded, other) {
			t.Fatalf("diagnostics changed:\n%s\n%s", encoded, other)
		}
	}
	missingPort := cursorAt(t, first, string(broken), "producer.missing", len("producer."))
	ports, err := first.CompletePorts(missingPort)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(ports); !slices.Equal(labels, []string{"out", "telemetry"}) {
		t.Fatalf("unknown port completion invented a contract: %v", labels)
	}
	if _, err := first.Definitions(missingPort); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("unknown port definition = %v", err)
	}

	invalid, err := Analyze("invalid.ortg", []byte("graph bad {"), catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Parsed() || invalid.Canonical() || invalid.Diagnostics().Items[0].Code != "E_SYNTAX" {
		t.Fatalf("invalid source snapshot = parsed %v canonical %v diagnostics %+v",
			invalid.Parsed(), invalid.Canonical(), invalid.Diagnostics())
	}
	invalidCursor, err := invalid.Cursor(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Hover(invalidCursor); !errors.Is(err, ErrSyntaxUnavailable) {
		t.Fatalf("invalid hover = %v", err)
	}

	nonCanonical := []byte(strings.Replace(validSource, "test.Source ::", "test.Source  ::", 1))
	unformatted, err := Analyze("unformatted.ortg", nonCanonical, catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !unformatted.Parsed() || unformatted.Canonical() ||
		!hasDiagnostic(unformatted.Diagnostics(), "E_NON_CANONICAL_SOURCE") {
		t.Fatalf("non-canonical snapshot = %+v", unformatted.Diagnostics())
	}
	unformattedCursor, _ := unformatted.Cursor(strings.Index(string(nonCanonical), "test.Source"))
	if _, err := unformatted.Complete(unformattedCursor); !errors.Is(err, ErrSyntaxUnavailable) {
		t.Fatalf("non-canonical completion = %v", err)
	}
}

func TestUnknownAndAmbiguousNodesNeverProduceInventedContractsOrPartialRenames(t *testing.T) {
	catalog := testCatalog(t)
	unknownSource := `graph unknown {
    missing.Element :: mystery;
    input value = mystery.fake;
}
`
	unknown, err := Analyze("unknown.ortg", []byte(unknownSource), catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	portCursor := cursorAt(t, unknown, unknownSource, "mystery.fake", len("mystery."))
	ports, err := unknown.CompletePorts(portCursor)
	if err != nil {
		t.Fatal(err)
	}
	if ports.Total != 0 || len(ports.Items) != 0 {
		t.Fatalf("unknown element port completions = %+v", ports)
	}
	hover, err := unknown.Hover(portCursor)
	if err != nil {
		t.Fatal(err)
	}
	if hover.Descriptor != nil || hover.Port != nil || hover.Config == nil || hover.Config.Resolved ||
		hover.Config.EmptyObjectOnly || hover.Config.ValuesPath != "nodes.mystery" {
		t.Fatalf("unknown element hover invented a contract: %+v", hover)
	}
	if _, err := unknown.Definitions(portCursor); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("unknown element definition = %v", err)
	}

	duplicateSource := `graph duplicate {
    test.Source :: repeated;
    test.Sink :: repeated;
    input start = repeated.start;
    output done = repeated.done;
}
`
	duplicate, err := Analyze("duplicate.ortg", []byte(duplicateSource), catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDiagnostic(duplicate.Diagnostics(), "E_DUPLICATE_NODE") {
		t.Fatalf("duplicate diagnostics = %+v", duplicate.Diagnostics())
	}
	declaration := cursorAt(t, duplicate, duplicateSource, "repeated;", 1)
	if _, err := duplicate.RenameNode(declaration, "unique"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("ambiguous rename = %v", err)
	}
	reference := cursorAt(t, duplicate, duplicateSource, "repeated.start", 1)
	if _, err := duplicate.Definitions(reference); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("ambiguous definition = %v", err)
	}
	portReference := cursorAt(t, duplicate, duplicateSource, "repeated.start", len("repeated."))
	hover, err = duplicate.Hover(portReference)
	if err != nil {
		t.Fatal(err)
	}
	if hover.ElementReference != "" || hover.Descriptor != nil || hover.Port != nil ||
		hover.Config == nil || hover.Config.Resolved || hover.Config.EmptyObjectOnly {
		t.Fatalf("ambiguous hover invented a contract: %+v", hover)
	}
}

func TestApplyEditsRejectsForgedOverlappingAndStaleRanges(t *testing.T) {
	document := analyzeValid(t, Limits{})
	cursor := cursorAt(t, document, validSource, "producer.out ->", 1)
	edits, err := document.RenameNode(cursor, "camera")
	if err != nil {
		t.Fatal(err)
	}

	overlap := edits
	overlap.Edits = slices.Clone(edits.Edits)
	overlap.Edits[1].Span = overlap.Edits[0].Span
	if _, err := ApplyEdits(document.Source(), overlap); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("overlapping edits = %v", err)
	}
	forged := edits
	forged.Edits = slices.Clone(edits.Edits)
	forged.Edits[0].Span.Start.Column++
	if _, err := ApplyEdits(document.Source(), forged); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("forged edit position = %v", err)
	}
	wrongText := edits
	wrongText.Edits = slices.Clone(edits.Edits)
	wrongText.Edits[0].OldText = "not-producer"
	if _, err := ApplyEdits(document.Source(), wrongText); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale edit text = %v", err)
	}
}

func TestPositionsAndLimitsAreStrictAndResultsAreBounded(t *testing.T) {
	document := analyzeValid(t, Limits{MaxResultItems: 1, MaxRenameEdits: 2})
	metadata := document.CatalogMetadata()
	if metadata.Total != 2 || len(metadata.Elements) != 1 || !metadata.Incomplete {
		t.Fatalf("bounded metadata = %+v", metadata)
	}
	elementCursor := cursorAt(t, document, validSource, "test.Source", 0)
	completions, err := document.CompleteElements(elementCursor)
	if err != nil {
		t.Fatal(err)
	}
	if completions.Total != 2 || len(completions.Items) != 1 || !completions.Incomplete {
		t.Fatalf("bounded completions = %+v", completions)
	}
	renameCursor := cursorAt(t, document, validSource, "producer.out ->", 1)
	if _, err := document.RenameNode(renameCursor, "camera"); !errors.Is(err, ErrEditLimit) {
		t.Fatalf("bounded rename = %v", err)
	}

	stale := elementCursor
	stale.SourceDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := document.Hover(stale); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale cursor = %v", err)
	}
	invalid := elementCursor
	invalid.Position.Line++
	if err := document.ValidateCursor(invalid); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("inconsistent cursor = %v", err)
	}
	if _, err := document.Cursor(len(validSource) + 1); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("out-of-range cursor = %v", err)
	}
	commented := strings.Replace(validSource, "graph demo", "// α\ngraph demo", 1)
	unicodeDocument, err := Analyze("unicode.ortg", []byte(commented), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	alpha := strings.Index(commented, "α")
	if _, err := unicodeDocument.Cursor(alpha + 1); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("split UTF-8 cursor = %v", err)
	}
	commentCursor, _ := unicodeDocument.Cursor(alpha)
	if _, err := unicodeDocument.Hover(commentCursor); !errors.Is(err, ErrNoSymbol) {
		t.Fatalf("comment hover = %v", err)
	}

	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxSourceBytes: 1}); err == nil {
		t.Fatal("source byte bound was not enforced")
	}
	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxCatalogElements: 1}); err == nil {
		t.Fatal("catalog element bound was not enforced")
	}
	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxCatalogPorts: 1}); err == nil {
		t.Fatal("catalog port bound was not enforced")
	}
	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxCatalogBytes: 1}); err == nil {
		t.Fatal("catalog byte bound was not enforced")
	}
	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxGraphStatements: 1}); err == nil {
		t.Fatal("graph statement bound was not enforced")
	}
	if _, err := Analyze("bad\npath", []byte(validSource), testCatalog(t), Limits{}); err == nil {
		t.Fatal("invalid path was not rejected")
	}
	if _, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), Limits{MaxResultItems: -1}); err == nil {
		t.Fatal("negative result bound was not rejected")
	}
	identifierDocument := analyzeValid(t, Limits{MaxIdentifierBytes: 3})
	identifierCursor := cursorAt(t, identifierDocument, validSource, "producer.out ->", 1)
	if _, err := identifierDocument.RenameNode(identifierCursor, "long"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("identifier byte bound = %v", err)
	}
}

func TestSnapshotOwnsInputsAndSupportsConcurrentReaders(t *testing.T) {
	source := []byte(validSource)
	catalog := testCatalog(t)
	document, err := Analyze("agent.ortg", source, catalog, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	source[0] = 'X'
	if string(document.Source()) != validSource {
		t.Fatal("document retained caller source")
	}
	if err := catalog.Register(auxiliaryDescriptor()); err != nil {
		t.Fatal(err)
	}
	if document.CatalogMetadata().Total != 2 {
		t.Fatal("document observed a post-analysis catalog registration")
	}

	portCursor := cursorAt(t, document, validSource, "producer.out ->", len("producer."))
	nodeCursor := cursorAt(t, document, validSource, "producer.out ->", 1)
	var wait sync.WaitGroup
	failures := make(chan error, 64)
	for worker := 0; worker < 64; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 50; iteration++ {
				metadata := document.CatalogMetadata()
				metadata.Elements[0].Ports[0].Type.Arguments[0].Name = "caller.mutation"
				hover, hoverErr := document.Hover(portCursor)
				ports, completionErr := document.CompletePorts(portCursor)
				definitions, definitionErr := document.Definitions(nodeCursor)
				edits, renameErr := document.RenameNode(nodeCursor, "camera")
				if hoverErr != nil || completionErr != nil || definitionErr != nil || renameErr != nil ||
					hover.Port == nil || len(ports.Items) != 2 || len(definitions.Items) != 1 || len(edits.Edits) != 3 {
					failures <- fmt.Errorf("query failure hover=%v ports=%v definitions=%v rename=%v", hoverErr, completionErr, definitionErr, renameErr)
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if findMetadataPort(t, document.CatalogMetadata().Elements[0], "done").Type.String() != "Event<test.Done>" {
		t.Fatal("concurrent result mutation reached document state")
	}
}

func analyzeValid(t *testing.T, limits Limits) *Document {
	t.Helper()
	document, err := Analyze("agent.ortg", []byte(validSource), testCatalog(t), limits)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func testCatalog(t testing.TB) *resolve.Catalog {
	t.Helper()
	catalog := resolve.NewCatalog()
	for _, descriptor := range []element.Descriptor{sourceDescriptor(1), sourceDescriptor(2), sinkDescriptor()} {
		if err := catalog.Register(descriptor); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func sourceDescriptor(revision uint64) element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Source", Revision: revision,
		Ports: []element.Port{
			{Name: "start", Direction: element.Input, Type: element.Trigger(element.Named("test.Start")), Cardinality: element.One, Required: true, DefaultDepth: 2},
			{Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")), Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "telemetry", Direction: element.Output, Type: element.Event(element.Named("test.Telemetry")), Cardinality: element.One, Required: false, LossAllowed: true, DefaultDepth: 1},
		},
		Reaction:    element.Reaction{Triggers: []string{"start"}, Outcomes: []string{"out", "telemetry"}, MaxConcurrency: 1},
		StateSchema: "schema://test/source-state/v1", ConfigSchema: "schema://test/source-config/v1",
		Dependencies: []element.Dependency{{Name: "test.clock"}},
		Effects:      []element.Effect{{Name: "test.subscription", Reversible: true}},
	}
}

func sinkDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Sink", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Value")), Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "spare", Direction: element.Input, Type: element.Event(element.Named("test.Value")), Cardinality: element.One, Required: false, DefaultDepth: 4},
			{Name: "done", Direction: element.Output, Type: element.Event(element.Named("test.Done")), Cardinality: element.One, Required: true, DefaultDepth: 4},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"done"}, MaxConcurrency: 1},
	}
}

func auxiliaryDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Auxiliary", Revision: 1,
		Ports: []element.Port{{Name: "in", Direction: element.Input, Type: element.Event(element.Named("test.Value")), Cardinality: element.One}},
	}
}

func cursorAt(t *testing.T, document *Document, source, needle string, delta int) Cursor {
	t.Helper()
	offset := strings.Index(source, needle)
	if offset < 0 {
		t.Fatalf("source has no %q", needle)
	}
	cursor, err := document.Cursor(offset + delta)
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func completionLabels(list CompletionList) []string {
	result := make([]string, len(list.Items))
	for index := range list.Items {
		result[index] = list.Items[index].Label
	}
	return result
}

func metadataNames(report MetadataReport) []string {
	result := make([]string, len(report.Elements))
	for index := range report.Elements {
		result[index] = report.Elements[index].Identity.Name
	}
	return result
}

func findMetadataPort(t *testing.T, metadata ElementMetadata, name string) PortMetadata {
	t.Helper()
	for _, port := range metadata.Ports {
		if port.Name == name {
			return port
		}
	}
	t.Fatalf("metadata %s has no port %s", metadata.Identity.Name, name)
	return PortMetadata{}
}

func sourceText(source []byte, span syntax.Span) string {
	return string(source[span.Start.Offset:span.End.Offset])
}

func hasDiagnostic(report DiagnosticReport, code string) bool {
	return slices.ContainsFunc(report.Items, func(value Diagnostic) bool { return value.Code == code })
}

func TestPublicResultsAreDeterministicValues(t *testing.T) {
	document := analyzeValid(t, Limits{})
	left, _ := json.Marshal(document.CatalogMetadata())
	right, _ := json.Marshal(document.CatalogMetadata())
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("metadata encoding changed:\n%s\n%s", left, right)
	}
}

func TestPartialAuthoringCompletionAPIsNeverInventDescriptorsOrPorts(t *testing.T) {
	document := analyzeValid(t, Limits{})
	insert, err := document.Cursor(strings.Index(validSource, "    test.Source"))
	if err != nil {
		t.Fatal(err)
	}
	elements, err := document.ElementCompletions(insert, "test.S")
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(elements); !slices.Equal(labels, []string{"test.Sink", "test.Source"}) {
		t.Fatalf("partial element completions = %v", labels)
	}
	for _, item := range elements.Items {
		if item.Replacement.Start != insert.Position || item.Replacement.End != insert.Position {
			t.Fatalf("partial element replacement = %+v", item.Replacement)
		}
	}
	nodes, err := document.NodeCompletions(insert, "pro", "out", element.Output)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(nodes); !slices.Equal(labels, []string{"producer"}) {
		t.Fatalf("partial node completions = %v", labels)
	}
	if missing, err := document.NodeCompletions(insert, "", "invented", element.Output); err != nil || missing.Total != 0 {
		t.Fatalf("invented node port completions = %+v, %v", missing, err)
	}
	ports, err := document.PortCompletions(insert, "consumer", "", element.Input)
	if err != nil {
		t.Fatal(err)
	}
	if labels := completionLabels(ports); !slices.Equal(labels, []string{"in", "spare"}) {
		t.Fatalf("partial port completions = %v", labels)
	}
	if missing, err := document.PortCompletions(insert, "unknown", "", element.Input); err != nil || missing.Total != 0 {
		t.Fatalf("unknown node port completions = %+v, %v", missing, err)
	}
	if _, err := document.PortCompletions(insert, "consumer", "", element.Direction("sideways")); err == nil {
		t.Fatal("invalid completion direction was accepted")
	}
}

type editorSchemaResolver func(context.Context, string) (schema.ResolvedSchema, error)

func (resolver editorSchemaResolver) ResolveConfigSchema(ctx context.Context, reference string) (schema.ResolvedSchema, error) {
	return resolver(ctx, reference)
}

func TestRecoverySnapshotSupportsExplicitPartialCompletionsButNotFormatting(t *testing.T) {
	source := `graph partial {
    test.Source :: producer;
    producer.
`
	document, err := Analyze("partial.ortg", []byte(source), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if document.Parsed() || !document.Recovered() || document.Canonical() ||
		!hasDiagnostic(document.Diagnostics(), "E_SYNTAX") {
		t.Fatalf("recovery snapshot parsed=%v recovered=%v diagnostics=%+v",
			document.Parsed(), document.Recovered(), document.Diagnostics())
	}
	cursor, err := document.Cursor(len(source))
	if err != nil {
		t.Fatal(err)
	}
	ports, err := document.PortCompletions(cursor, "producer", "o", element.Output)
	if err != nil || !slices.Equal(completionLabels(ports), []string{"out"}) {
		t.Fatalf("recovered port completions = %+v, %v", ports, err)
	}
	elements, err := document.ElementCompletions(cursor, "test.S")
	if err != nil || !slices.Equal(completionLabels(elements), []string{"test.Sink", "test.Source"}) {
		t.Fatalf("recovered element completions = %+v, %v", elements, err)
	}
	if _, err := document.FormatEdits(); !errors.Is(err, ErrFormattingUnavailable) {
		t.Fatalf("recovery formatter error = %v", err)
	}
}

func TestFormatEditsAreCanonicalAtomicAndStaleSafe(t *testing.T) {
	source := []byte(strings.Replace(validSource, "test.Source ::", "test.Source  ::", 1))
	document, err := Analyze("format.ortg", source, testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	edits, err := document.FormatEdits()
	if err != nil {
		t.Fatal(err)
	}
	if len(edits.Edits) != 1 || edits.SourceDigest != document.SourceDigest() ||
		edits.Edits[0].OldText != string(source) || edits.Edits[0].NewText != validSource {
		t.Fatalf("format edit set = %+v", edits)
	}
	formatted, err := ApplyEdits(source, edits)
	if err != nil || string(formatted) != validSource {
		t.Fatalf("formatted source = %q, %v", formatted, err)
	}
	if _, err := ApplyEdits(append(slices.Clone(source), ' '), edits); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale format edits = %v", err)
	}
	canonical := analyzeValid(t, Limits{})
	noOp, err := canonical.FormatEdits()
	if err != nil || len(noOp.Edits) != 0 || noOp.SourceDigest != canonical.SourceDigest() {
		t.Fatalf("canonical format edits = %+v, %v", noOp, err)
	}
}

func TestResolvedValuesPropertiesAreImmutableAndInvalidSchemasAreDiagnostic(t *testing.T) {
	resolver := editorSchemaResolver(func(_ context.Context, reference string) (schema.ResolvedSchema, error) {
		if reference != "schema://test/source-config/v1" {
			return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
		}
		return schema.ResolvedSchema{
			ID: "https://schemas.example.test/source-config-v1.json",
			Document: json.RawMessage(`{
                "$id":"https://schemas.example.test/source-config-v1.json",
                "type":"object","required":["model"],
                "properties":{"temperature":{"type":"number","default":0.2},"model":{"type":"string"}},
                "additionalProperties":false
            }`),
		}, nil
	})
	document, err := AnalyzeWithOptions(context.Background(), "agent.ortg", []byte(validSource), testCatalog(t), Options{
		SchemaResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := document.CatalogMetadata().Elements[1]
	if metadata.Config.SchemaStatus != ConfigSchemaResolved || !metadata.Config.PropertiesComplete ||
		len(metadata.Config.Properties) != 2 || metadata.Config.Properties[0].Name != "model" ||
		!metadata.Config.Properties[0].Required || metadata.Config.SchemaDigest == "" {
		t.Fatalf("resolved config metadata = %+v", metadata.Config)
	}
	metadata.Config.Properties[0].Schema[0] = 'X'
	again := document.CatalogMetadata().Elements[1].Config
	if again.Properties[0].Name != "model" || again.Properties[0].Schema[0] == 'X' {
		t.Fatal("config property metadata retained caller aliases")
	}

	invalid := editorSchemaResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/invalid.json",
			Document: json.RawMessage(`{"$id":"https://schemas.example.test/invalid.json","type":"object","properties":{"x":{"$ref":"https://remote.test/x"}}}`),
		}, nil
	})
	broken, err := AnalyzeWithOptions(context.Background(), "agent.ortg", []byte(validSource), testCatalog(t), Options{
		SchemaResolver: invalid,
	})
	if err != nil {
		t.Fatal(err)
	}
	brokenMetadata := broken.CatalogMetadata().Elements[1].Config
	if brokenMetadata.SchemaStatus != ConfigSchemaInvalid || len(brokenMetadata.Properties) != 0 ||
		!hasDiagnostic(broken.Diagnostics(), "E_VALUES_SCHEMA_INVALID") {
		t.Fatalf("invalid schema metadata = %+v diagnostics=%+v", brokenMetadata, broken.Diagnostics())
	}
}

func BenchmarkAnalyzeRecoveryWithResolvedSchema(b *testing.B) {
	catalog := testCatalog(b)
	resolver := editorSchemaResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/source-config-v1.json",
			Document: json.RawMessage(`{"$id":"https://schemas.example.test/source-config-v1.json","type":"object","properties":{"model":{"type":"string"}},"additionalProperties":false}`),
		}, nil
	})
	source := []byte("graph partial {\n    test.Source :: producer;\n    producer.\n")
	b.ReportAllocs()
	for range b.N {
		document, err := AnalyzeWithOptions(context.Background(), "partial.ortg", source, catalog, Options{
			SchemaResolver: resolver,
		})
		if err != nil || !document.Recovered() || document.CatalogMetadata().Elements[1].Config.SchemaStatus != ConfigSchemaResolved {
			b.Fatalf("analysis = %+v, %v", document, err)
		}
	}
}
