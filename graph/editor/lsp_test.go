package editor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
