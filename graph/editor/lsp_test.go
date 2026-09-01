package editor

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/schema"
)

const lspMetadataSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$id":"https://schemas.example.test/source-config-v1.json",
  "type":"object",
  "required":["model"],
  "properties":{
    "model":{
      "type":["null","string"],
      "title":"<b>Model</b>",
      "description":"first line\nsecond line",
      "format":"model-name",
      "default":null,
      "enum":[null,"large"]
    },
    "temperature":{"type":"number","default":0.2}
  },
  "additionalProperties":false
}`

func TestLSPConfigurationHoverRendersEveryResolvedFieldAsPlaintext(t *testing.T) {
	document := lspMetadataDocument(t, validSource)
	result, err := document.LSPConfigurationHover(LSPPosition{Line: 1, Character: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.Contents.Kind != "plaintext" ||
		result.Range != (LSPRange{
			Start: LSPPosition{Line: 1, Character: 4},
			End:   LSPPosition{Line: 1, Character: 15},
		}) {
		t.Fatalf("LSP hover identity = %+v", result)
	}
	digest := document.CatalogMetadata().Elements[1].Config.SchemaDigest
	want := strings.Join([]string{
		`configuration contract: "producer"`,
		`element: "test.Source"`,
		`values path: "nodes.producer"`,
		`artifact: "openrealtime.ai/config/v1alpha1"`,
		`descriptor resolved: true`,
		`inline topology values: false`,
		`empty object only: false`,
		`schema status: "resolved"`,
		`schema reference: "schema://test/source-config/v1"`,
		`schema identity: "https://schemas.example.test/source-config-v1.json"`,
		`schema digest: "` + digest + `"`,
		`properties complete: true`,
		`additional properties: false`,
		`property count: 2`,
		`property[0].name: "model"`,
		`property[0].pointer: "#/properties/model"`,
		`property[0].required: true`,
		`property[0].types: ["null","string"]`,
		`property[0].title: "<b>Model</b>"`,
		`property[0].description: "first line\nsecond line"`,
		`property[0].format: "model-name"`,
		`property[0].default: null`,
		`property[0].enum: [null,"large"]`,
		`property[0].schema: {"default":null,"description":"first line\nsecond line","enum":[null,"large"],"format":"model-name","title":"\u003cb\u003eModel\u003c/b\u003e","type":["null","string"]}`,
		`property[1].name: "temperature"`,
		`property[1].pointer: "#/properties/temperature"`,
		`property[1].required: false`,
		`property[1].types: ["number"]`,
		`property[1].title: absent`,
		`property[1].description: absent`,
		`property[1].format: absent`,
		`property[1].default: 0.2`,
		`property[1].enum: absent`,
		`property[1].schema: {"default":0.2,"type":"number"}`,
	}, "\n")
	if result.Contents.Value != want {
		t.Fatalf("LSP hover:\n%s\nwant:\n%s", result.Contents.Value, want)
	}
	if strings.Contains(result.Contents.Value, "first line\nsecond line\nproperty") {
		t.Fatal("schema description injected a presentation line")
	}
	cursor, err := document.Cursor(strings.Index(validSource, "test.Source") + 1)
	if err != nil {
		t.Fatal(err)
	}
	rawHover, err := document.Hover(cursor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renderLSPConfigurationBounded(rawHover, len(result.Contents.Value)-1); !errors.Is(err, ErrPresentationLimit) {
		t.Fatalf("LSP presentation bound error = %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"kind":"plaintext"`) ||
		!strings.Contains(string(encoded), `"line":1,"character":4`) {
		t.Fatalf("LSP wire projection = %s, %v", encoded, err)
	}

	again, err := document.LSPConfigurationHover(LSPPosition{Line: 1, Character: 5})
	if err != nil || again != result {
		t.Fatalf("LSP projection changed = %+v, %v", again, err)
	}
}

func TestLSPConfigurationHoverPreservesUnresolvedAndEmptyContracts(t *testing.T) {
	document := analyzeValid(t, Limits{})
	unresolved, err := document.LSPConfigurationHover(LSPPosition{Line: 1, Character: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`schema status: "unresolved"`, `schema identity: absent`,
		`properties complete: false`, `property count: 0`,
	} {
		if !strings.Contains(unresolved.Contents.Value, line) {
			t.Fatalf("unresolved LSP hover omitted %q:\n%s", line, unresolved.Contents.Value)
		}
	}
	empty, err := document.LSPConfigurationHover(LSPPosition{Line: 2, Character: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`schema status: "empty-object-only"`, `schema reference: absent`,
		`empty object only: true`, `properties complete: true`,
	} {
		if !strings.Contains(empty.Contents.Value, line) {
			t.Fatalf("empty LSP hover omitted %q:\n%s", line, empty.Contents.Value)
		}
	}
}

func TestLSPPositionsUseUTF16AndMalformedMetadataFailsClosed(t *testing.T) {
	commented := strings.Replace(validSource, "graph demo", "// 😀\ngraph demo", 1)
	document := lspMetadataDocument(t, commented)
	if _, err := document.LSPConfigurationHover(LSPPosition{Line: 0, Character: 4}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("split surrogate error = %v", err)
	}
	if _, err := document.LSPConfigurationHover(LSPPosition{Line: 0, Character: 5}); !errors.Is(err, ErrNoSymbol) {
		t.Fatalf("valid UTF-16 comment position = %v", err)
	}
	if _, err := document.LSPConfigurationHover(LSPPosition{Line: -1}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("negative LSP position = %v", err)
	}
	if _, err := document.LSPConfigurationHover(LSPPosition{Line: 99}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("out-of-range LSP position = %v", err)
	}

	hover := Hover{Node: "n", Config: &NodeConfigContract{ConfigContract: ConfigContract{
		Artifact: "openrealtime.ai/config/v1alpha1", Resolved: true,
		SchemaStatus: ConfigSchemaResolved, Properties: []ValuesPropertyMetadata{{
			Name: "bad", Pointer: "#/properties/bad", Schema: json.RawMessage(`{"type":`),
		}},
	}}}
	if _, err := renderLSPConfiguration(hover); err == nil {
		t.Fatal("malformed retained schema metadata reached the LSP projection")
	}
}

func TestLSPDiagnosticsUseExactUTF16RangesAndFailClosedOnPartialEvidence(t *testing.T) {
	source := []byte("// 😀\nnode\n")
	positions := newSourcePositions(source)
	emoji, err := sourceSpan(positions, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	wordStart := strings.Index(string(source), "node")
	word, err := sourceSpan(positions, wordStart, wordStart+len("node"))
	if err != nil {
		t.Fatal(err)
	}
	document := &Document{
		path: "fixture.ortg", source: source, digest: sourceDigest(source),
		limits: DefaultLimits(), positions: positions,
		diagnostics: []Diagnostic{
			{
				Code: "E_EMOJI", Severity: SeverityError, Path: "fixture.ortg",
				Span: emoji, Message: "emoji-shaped fixture", Notes: []string{"first note"},
			},
			{
				Code: "W_NODE", Severity: SeverityWarning, Path: "fixture.ortg",
				Span: word, Message: "node-shaped fixture",
			},
		},
	}
	report, err := document.LSPDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	if report.Kind != "full" || len(report.Items) != 2 ||
		report.Items[0].Range != (LSPRange{
			Start: LSPPosition{Line: 0, Character: 3},
			End:   LSPPosition{Line: 0, Character: 5},
		}) || report.Items[1].Range != (LSPRange{
		Start: LSPPosition{Line: 1, Character: 0},
		End:   LSPPosition{Line: 1, Character: 4},
	}) || report.Items[0].Severity != LSPDiagnosticError ||
		report.Items[1].Severity != LSPDiagnosticWarning ||
		report.Items[0].Source != "openrealtime" ||
		report.Items[0].Data.SourceDigest != document.SourceDigest() ||
		!reflect.DeepEqual(report.Items[0].Data.Notes, []string{"first note"}) {
		t.Fatalf("LSP diagnostic projection = %+v", report)
	}
	report.Items[0].Data.Notes[0] = "mutated"
	again, err := document.LSPDiagnostics()
	if err != nil || again.Items[0].Data.Notes[0] != "first note" {
		t.Fatalf("LSP diagnostics retained caller aliases: %+v, %v", again, err)
	}
	encoded, err := json.Marshal(again)
	if err != nil || !strings.Contains(string(encoded), `"kind":"full"`) ||
		!strings.Contains(string(encoded), `"severity":1`) ||
		!strings.Contains(string(encoded), `"source_digest":"sha256:`) {
		t.Fatalf("LSP diagnostic wire projection = %s, %v", encoded, err)
	}

	truncated := *document
	truncated.limits.MaxResultItems = 1
	if _, err := truncated.LSPDiagnostics(); !errors.Is(err, ErrPresentationLimit) {
		t.Fatalf("partial diagnostics were labeled full: %v", err)
	}
	unknownSeverity := *document
	unknownSeverity.diagnostics = []Diagnostic{document.diagnostics[0]}
	unknownSeverity.diagnostics[0].Severity = Severity("information")
	if _, err := unknownSeverity.LSPDiagnostics(); !errors.Is(err, ErrPresentationLimit) {
		t.Fatalf("unknown LSP diagnostic severity = %v", err)
	}
	malformedText := *document
	malformedText.diagnostics = []Diagnostic{document.diagnostics[0]}
	malformedText.diagnostics[0].Message = string([]byte{0xff})
	if _, err := malformedText.LSPDiagnostics(); !errors.Is(err, ErrPresentationLimit) {
		t.Fatalf("malformed UTF-8 LSP diagnostic = %v", err)
	}
	stale := *document
	stale.diagnostics = []Diagnostic{document.diagnostics[0]}
	stale.diagnostics[0].Span.Start.Line++
	if _, err := stale.LSPDiagnostics(); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("stale LSP diagnostic span = %v", err)
	}

	invalid, err := Analyze("invalid.ortg", []byte("graph bad {"), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	invalidReport, err := invalid.LSPDiagnostics()
	if err != nil || len(invalidReport.Items) == 0 || invalidReport.Items[0].Code != "E_SYNTAX" {
		t.Fatalf("recovered LSP diagnostics = %+v, %v", invalidReport, err)
	}
	unknownSource := strings.Replace(validSource, "graph demo", "// 😀\ngraph demo", 1)
	unknownSource = strings.Replace(unknownSource, "test.Source", "missing.Source", 1)
	unknown, err := Analyze("unknown.ortg", []byte(unknownSource), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	unknownReport, err := unknown.LSPDiagnostics()
	if err != nil || len(unknownReport.Items) == 0 || unknownReport.Items[0].Range.Start.Line < 1 {
		t.Fatalf("compiler-backed UTF-16 LSP diagnostics = %+v, %v", unknownReport, err)
	}
}

func TestLSPCompletionsAreCanonicalBoundedAndSourceDigestBound(t *testing.T) {
	document := analyzeValid(t, Limits{})
	result, err := document.LSPCompletions(LSPPosition{Line: 1, Character: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsIncomplete || len(result.Items) != 2 ||
		result.Items[0].Label != "test.Sink" || result.Items[1].Label != "test.Source" {
		t.Fatalf("LSP completion list = %+v", result)
	}
	wantRange := LSPRange{
		Start: LSPPosition{Line: 1, Character: 4},
		End:   LSPPosition{Line: 1, Character: 15},
	}
	for _, item := range result.Items {
		if item.Kind != LSPCompletionClass || item.SortText != item.Label ||
			item.FilterText != item.Label || item.InsertTextFormat != 1 ||
			item.TextEdit.Range != wantRange || item.TextEdit.NewText != item.Label ||
			item.Data.SourceDigest != document.SourceDigest() ||
			item.Data.Kind != CompletionElement || item.Data.Element != item.Label ||
			item.Data.ElementRevision == 0 || !strings.HasPrefix(item.Data.ElementDigest, "sha256:") {
			t.Fatalf("LSP completion item = %+v", item)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || !strings.Contains(string(encoded), `"isIncomplete":false`) ||
		!strings.Contains(string(encoded), `"newText":"test.Sink"`) ||
		!strings.Contains(string(encoded), `"insertTextFormat":1`) {
		t.Fatalf("LSP completion wire projection = %s, %v", encoded, err)
	}
	again, err := document.LSPCompletions(LSPPosition{Line: 1, Character: 10})
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("LSP completions changed = %+v, %v", again, err)
	}
	nodes, err := document.LSPCompletions(LSPPosition{Line: 3, Character: 7})
	if err != nil || len(nodes.Items) != 1 || nodes.Items[0].Label != "producer" ||
		nodes.Items[0].Kind != LSPCompletionVariable || nodes.Items[0].Data.Node != "producer" ||
		nodes.Items[0].Data.Port != "out" || nodes.Items[0].Data.Type == "" {
		t.Fatalf("LSP node completion = %+v, %v", nodes, err)
	}
	ports, err := document.LSPCompletions(LSPPosition{Line: 3, Character: 14})
	if err != nil || len(ports.Items) != 1 || ports.Items[0].Label != "out" ||
		ports.Items[0].Kind != LSPCompletionField || ports.Items[0].Data.Node != "producer" ||
		ports.Items[0].Data.Port != "out" || ports.Items[0].Data.Type == "" {
		t.Fatalf("LSP port completion = %+v, %v", ports, err)
	}

	bounded := analyzeValid(t, Limits{MaxResultItems: 1})
	partial, err := bounded.LSPCompletions(LSPPosition{Line: 1, Character: 10})
	if err != nil || !partial.IsIncomplete || len(partial.Items) != 1 ||
		partial.Items[0].Label != "test.Sink" {
		t.Fatalf("bounded LSP completion list = %+v, %v", partial, err)
	}
	commented := strings.Replace(validSource, "graph demo", "// 😀\ngraph demo", 1)
	unicodeDocument := lspMetadataDocument(t, commented)
	if _, err := unicodeDocument.LSPCompletions(LSPPosition{Line: 0, Character: 4}); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("split-surrogate LSP completion = %v", err)
	}
	if _, err := unicodeDocument.LSPCompletions(LSPPosition{Line: 0, Character: 5}); !errors.Is(err, ErrNoCompletionContext) {
		t.Fatalf("comment LSP completion context = %v", err)
	}
}

func TestLSPDefinitionsUseExactSourceAndVirtualDescriptorRanges(t *testing.T) {
	document := lspMetadataDocument(t, validSource)
	identity := lspDocumentIdentity(document, 7)

	nodeLinks, err := document.LSPDefinitions(LSPPosition{Line: 3, Character: 5}, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeLinks) != 1 || nodeLinks[0].TargetURI != identity.URI ||
		nodeLinks[0].OriginSelectionRange != (LSPRange{
			Start: LSPPosition{Line: 3, Character: 4},
			End:   LSPPosition{Line: 3, Character: 12},
		}) || nodeLinks[0].TargetSelectionRange != (LSPRange{
		Start: LSPPosition{Line: 1, Character: 19},
		End:   LSPPosition{Line: 1, Character: 27},
	}) || nodeLinks[0].TargetRange != nodeLinks[0].TargetSelectionRange {
		t.Fatalf("LSP source definition = %+v", nodeLinks)
	}

	elementLinks, err := document.LSPDefinitions(LSPPosition{Line: 1, Character: 5}, identity)
	if err != nil {
		t.Fatal(err)
	}
	virtual, err := document.LSPVirtualDescriptorDocument("test.Source")
	if err != nil {
		t.Fatal(err)
	}
	parsedVirtualURI, parseErr := url.ParseRequestURI(virtual.URI)
	if len(elementLinks) != 1 || elementLinks[0].TargetURI != virtual.URI ||
		parseErr != nil || parsedVirtualURI.Scheme != "openrealtime-descriptor" ||
		parsedVirtualURI.User != nil || parsedVirtualURI.Fragment != "" ||
		virtual.LanguageID != "plaintext" ||
		lspRangeText(t, virtual.Text, elementLinks[0].TargetSelectionRange) != "test.Source" ||
		lspRangeText(t, virtual.Text, elementLinks[0].TargetRange) != virtual.Text ||
		strings.Contains(virtual.Text, "<b>Model</b>") ||
		!strings.Contains(virtual.Text, `\u003cb\u003eModel\u003c/b\u003e`) {
		t.Fatalf("LSP virtual element definition = link %+v document %+v", elementLinks, virtual)
	}
	portLinks, err := document.LSPDefinitions(LSPPosition{Line: 3, Character: 14}, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(portLinks) != 1 || portLinks[0].TargetURI != virtual.URI ||
		lspRangeText(t, virtual.Text, portLinks[0].TargetSelectionRange) != "out" ||
		portLinks[0].OriginSelectionRange != (LSPRange{
			Start: LSPPosition{Line: 3, Character: 13},
			End:   LSPPosition{Line: 3, Character: 16},
		}) {
		t.Fatalf("LSP virtual port definition = %+v", portLinks)
	}
	again, err := document.LSPVirtualDescriptorDocument("test.Source")
	if err != nil || again != virtual {
		t.Fatalf("LSP virtual descriptor changed = %+v, %v", again, err)
	}
	if _, err := document.LSPVirtualDescriptorDocument("missing.Element"); !errors.Is(err, ErrDefinitionMissing) {
		t.Fatalf("missing LSP virtual descriptor = %v", err)
	}

	stale := identity
	stale.SourceDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := document.LSPDefinitions(LSPPosition{Line: 1, Character: 5}, stale); !errors.Is(err, ErrStalePosition) {
		t.Fatalf("stale LSP definition identity = %v", err)
	}
	for _, invalidURI := range []string{
		"agent.ortg", "file://user:secret@localhost/workspace/agent.ortg",
		"file:///workspace/agent.ortg#fragment", " file:///workspace/agent.ortg",
	} {
		invalid := identity
		invalid.URI = invalidURI
		if _, err := document.LSPDefinitions(LSPPosition{Line: 1, Character: 5}, invalid); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("invalid LSP definition URI %q = %v", invalidURI, err)
		}
	}
}

func TestLSPRenameAndFormattingAreExactVersionedWorkspaceEdits(t *testing.T) {
	document := analyzeValid(t, Limits{})
	identity := lspDocumentIdentity(document, 11)
	rename, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "camera", identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(rename.DocumentChanges) != 1 ||
		rename.DocumentChanges[0].TextDocument != (LSPVersionedTextDocumentIdentifier{
			URI: identity.URI, Version: 11,
		}) || len(rename.DocumentChanges[0].Edits) != 3 {
		t.Fatalf("LSP rename workspace edit = %+v", rename)
	}
	renamed := applyLSPTextEdits(t, []byte(validSource), rename.DocumentChanges[0].Edits)
	wantRenamed := strings.ReplaceAll(validSource, "producer", "camera")
	if string(renamed) != wantRenamed {
		t.Fatalf("LSP renamed source:\n%s\nwant:\n%s", renamed, wantRenamed)
	}
	encoded, err := json.Marshal(rename)
	if err != nil || !strings.Contains(string(encoded), `"documentChanges"`) ||
		!strings.Contains(string(encoded), `"version":11`) ||
		!strings.Contains(string(encoded), `"newText":"camera"`) ||
		strings.Contains(string(encoded), "old_text") || strings.Contains(string(encoded), "source_digest") {
		t.Fatalf("LSP workspace-edit wire projection = %s, %v", encoded, err)
	}
	rename.DocumentChanges[0].Edits[0].NewText = "mutated"
	again, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "camera", identity)
	if err != nil || again.DocumentChanges[0].Edits[0].NewText != "camera" {
		t.Fatalf("LSP rename retained caller aliases = %+v, %v", again, err)
	}
	noOp, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "producer", identity)
	if err != nil || len(noOp.DocumentChanges) != 1 || noOp.DocumentChanges[0].Edits == nil ||
		len(noOp.DocumentChanges[0].Edits) != 0 {
		t.Fatalf("no-op LSP rename = %+v, %v", noOp, err)
	}

	for name, mutation := range map[string]func(*LSPDocumentIdentity){
		"path":    func(value *LSPDocumentIdentity) { value.Path = "other.ortg" },
		"digest":  func(value *LSPDocumentIdentity) { value.SourceDigest = "sha256:" + strings.Repeat("0", 64) },
		"version": func(value *LSPDocumentIdentity) { value.Version = -1 },
		"uri":     func(value *LSPDocumentIdentity) { value.URI = "relative.ortg" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := identity
			mutation(&invalid)
			_, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "camera", invalid)
			if name == "path" || name == "digest" {
				if !errors.Is(err, ErrStalePosition) {
					t.Fatalf("LSP identity error = %v", err)
				}
			} else if !errors.Is(err, ErrInvalidPosition) {
				t.Fatalf("LSP identity error = %v", err)
			}
		})
	}

	unformattedSource := strings.Replace(validSource, "test.Source ::", "test.Source  ::", 1)
	unformatted, err := Analyze("agent.ortg", []byte(unformattedSource), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	formatIdentity := lspDocumentIdentity(unformatted, 12)
	format, err := unformatted.LSPFormatting(formatIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if len(format.DocumentChanges) != 1 || len(format.DocumentChanges[0].Edits) != 1 ||
		format.DocumentChanges[0].TextDocument.Version != 12 ||
		string(applyLSPTextEdits(
			t, []byte(unformattedSource), format.DocumentChanges[0].Edits,
		)) != validSource {
		t.Fatalf("LSP formatting workspace edit = %+v", format)
	}
	recovered, err := Analyze("agent.ortg", []byte("graph partial {"), testCatalog(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.LSPFormatting(lspDocumentIdentity(recovered, 13)); !errors.Is(err, ErrFormattingUnavailable) {
		t.Fatalf("recovered LSP formatting = %v", err)
	}

	raw, err := document.RenameNode(cursorAt(t, document, validSource, "producer.out", 1), "camera")
	if err != nil {
		t.Fatal(err)
	}
	raw.Edits[0].OldText = "stale"
	if _, err := document.lspWorkspaceEdit(raw, identity); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("stale raw workspace edit = %v", err)
	}
	raw, err = document.RenameNode(cursorAt(t, document, validSource, "producer.out", 1), "camera")
	if err != nil {
		t.Fatal(err)
	}
	raw.Edits[1].Span = raw.Edits[0].Span
	if _, err := document.lspWorkspaceEdit(raw, identity); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("overlapping raw workspace edit = %v", err)
	}
	raw, err = document.RenameNode(cursorAt(t, document, validSource, "producer.out", 1), "camera")
	if err != nil {
		t.Fatal(err)
	}
	raw.Edits[0].NewText = string([]byte{0xff})
	if _, err := document.lspWorkspaceEdit(raw, identity); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("malformed UTF-8 raw workspace edit = %v", err)
	}
}

func TestLSPWireProjectionsAreDeterministicForConcurrentReaders(t *testing.T) {
	document := lspMetadataDocument(t, validSource)
	wantDiagnostics, err := document.LSPDiagnostics()
	if err != nil {
		t.Fatal(err)
	}
	wantCompletions, err := document.LSPCompletions(LSPPosition{Line: 1, Character: 10})
	if err != nil {
		t.Fatal(err)
	}
	identity := lspDocumentIdentity(document, 19)
	wantDefinitions, err := document.LSPDefinitions(LSPPosition{Line: 3, Character: 14}, identity)
	if err != nil {
		t.Fatal(err)
	}
	wantRename, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "camera", identity)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	failures := make(chan string, 16)
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 25 {
				diagnostics, diagnosticErr := document.LSPDiagnostics()
				completions, completionErr := document.LSPCompletions(
					LSPPosition{Line: 1, Character: 10},
				)
				definitions, definitionErr := document.LSPDefinitions(
					LSPPosition{Line: 3, Character: 14}, identity,
				)
				rename, renameErr := document.LSPRename(
					LSPPosition{Line: 3, Character: 5}, "camera", identity,
				)
				if diagnosticErr != nil || completionErr != nil || definitionErr != nil || renameErr != nil ||
					!reflect.DeepEqual(diagnostics, wantDiagnostics) ||
					!reflect.DeepEqual(completions, wantCompletions) ||
					!reflect.DeepEqual(definitions, wantDefinitions) ||
					!reflect.DeepEqual(rename, wantRename) {
					select {
					case failures <- "concurrent LSP projection changed":
					default:
					}
					return
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

func BenchmarkLSPConfigurationHoverResolvedMetadata(b *testing.B) {
	document := lspMetadataDocument(b, validSource)
	position := LSPPosition{Line: 1, Character: 5}
	b.ReportAllocs()
	for range b.N {
		if _, err := document.LSPConfigurationHover(position); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPCompletionProjection(b *testing.B) {
	document := lspMetadataDocument(b, validSource)
	position := LSPPosition{Line: 1, Character: 10}
	b.ReportAllocs()
	for range b.N {
		if _, err := document.LSPCompletions(position); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPDefinitionAndRenameProjection(b *testing.B) {
	document := lspMetadataDocument(b, validSource)
	identity := lspDocumentIdentity(document, 1)
	b.Run("virtual-port-definition", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := document.LSPDefinitions(LSPPosition{Line: 3, Character: 14}, identity); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("versioned-rename", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := document.LSPRename(LSPPosition{Line: 3, Character: 5}, "camera", identity); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func lspDocumentIdentity(document *Document, version int) LSPDocumentIdentity {
	return LSPDocumentIdentity{
		Path: document.Path(), URI: "file:///workspace/agent.ortg",
		Version: version, SourceDigest: document.SourceDigest(),
	}
}

func lspRangeText(t testing.TB, text string, value LSPRange) string {
	t.Helper()
	source := []byte(text)
	document := &Document{source: source, positions: newSourcePositions(source)}
	start, err := document.offsetAtLSPPosition(value.Start)
	if err != nil {
		t.Fatal(err)
	}
	end, err := document.offsetAtLSPPosition(value.End)
	if err != nil {
		t.Fatal(err)
	}
	if end < start {
		t.Fatalf("LSP range is reversed: %+v", value)
	}
	return string(source[start:end])
}

func applyLSPTextEdits(t testing.TB, source []byte, edits []LSPTextEdit) []byte {
	t.Helper()
	document := &Document{source: source, positions: newSourcePositions(source)}
	result := append([]byte(nil), source...)
	for index := len(edits) - 1; index >= 0; index-- {
		start, err := document.offsetAtLSPPosition(edits[index].Range.Start)
		if err != nil {
			t.Fatal(err)
		}
		end, err := document.offsetAtLSPPosition(edits[index].Range.End)
		if err != nil {
			t.Fatal(err)
		}
		if end < start {
			t.Fatalf("LSP edit %d is reversed", index)
		}
		next := make([]byte, 0, len(result)-(end-start)+len(edits[index].NewText))
		next = append(next, result[:start]...)
		next = append(next, edits[index].NewText...)
		next = append(next, result[end:]...)
		result = next
	}
	return result
}

func lspMetadataDocument(t testing.TB, source string) *Document {
	t.Helper()
	resolver := editorSchemaResolver(func(_ context.Context, reference string) (schema.ResolvedSchema, error) {
		if reference != "schema://test/source-config/v1" {
			return schema.ResolvedSchema{}, schema.ErrSchemaNotFound
		}
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/source-config-v1.json",
			Document: json.RawMessage(lspMetadataSchema),
		}, nil
	})
	document, err := AnalyzeWithOptions(context.Background(), "agent.ortg", []byte(source), testCatalog(t), Options{
		SchemaResolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}
