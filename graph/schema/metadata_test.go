package schema_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/schema"
)

type metadataResolver func(context.Context, string) (schema.ResolvedSchema, error)

func (resolver metadataResolver) ResolveConfigSchema(ctx context.Context, reference string) (schema.ResolvedSchema, error) {
	return resolver(ctx, reference)
}

const metadataSchema = `{
  "$schema":"https://json-schema.org/draft/2020-12/schema",
  "$id":"https://schemas.example.test/editor-config-v1.json",
  "type":"object",
  "required":["model"],
  "properties":{
    "temperature":{"type":"number","default":0.2,"description":"Sampling temperature"},
    "model":{"type":"string","title":"Model","enum":["small","large"]},
    "a/b~c":{"type":["null","integer"]}
  },
  "additionalProperties":false
}`

func TestResolvedPropertyMetadataIsExactDeterministicAndBounded(t *testing.T) {
	resolver := metadataResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/editor-config-v1.json",
			Document: json.RawMessage(metadataSchema),
		}, nil
	})
	first, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/editor/v1", schema.PropertyOptions{
		Resolver: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Complete || first.SchemaBytes == 0 || first.Digest == "" ||
		len(first.Properties) != 3 || first.Properties[0].Name != "a/b~c" ||
		first.Properties[0].Pointer != "#/properties/a~1b~0c" ||
		!slices.Equal(first.Properties[0].Types, []string{"integer", "null"}) ||
		first.Properties[1].Name != "model" || !first.Properties[1].Required ||
		len(first.Properties[1].Enum) != 2 || first.Properties[2].Name != "temperature" ||
		string(first.AdditionalProperties) != "false" {
		t.Fatalf("property report = %+v", first)
	}
	encoded, _ := json.Marshal(first)
	for iteration := 0; iteration < 50; iteration++ {
		next, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/editor/v1", schema.PropertyOptions{
			Resolver: resolver,
		})
		if err != nil {
			t.Fatal(err)
		}
		other, _ := json.Marshal(next)
		if string(other) != string(encoded) {
			t.Fatalf("property report changed:\n%s\n%s", encoded, other)
		}
	}
	if _, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/editor/v1", schema.PropertyOptions{
		Resolver: resolver, MaxProperties: 2,
	}); !errors.Is(err, schema.ErrLimitExceeded) {
		t.Fatalf("property bound error = %v", err)
	}
}

func TestPropertyMetadataFailsClosedForInvalidAndMarksCompositionIncomplete(t *testing.T) {
	invalid := metadataResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID: "https://schemas.example.test/invalid.json",
			Document: json.RawMessage(`{
                    "$id":"https://schemas.example.test/invalid.json",
                    "type":"object","properties":{"x":{"$ref":"https://remote.test/x"}}
                }`),
		}, nil
	})
	if _, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/invalid", schema.PropertyOptions{
		Resolver: invalid,
	}); !errors.Is(err, schema.ErrInvalidResolvedSchema) {
		t.Fatalf("external reference error = %v", err)
	}
	composed := metadataResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID: "https://schemas.example.test/composed.json",
			Document: json.RawMessage(`{
                    "$id":"https://schemas.example.test/composed.json",
                    "allOf":[{"type":"object","properties":{"hidden":{"type":"string"}}}],
                    "type":"object","properties":{"visible":{"type":"boolean"}}
                }`),
		}, nil
	})
	report, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/composed", schema.PropertyOptions{
		Resolver: composed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.Properties) != 1 || report.Properties[0].Name != "visible" {
		t.Fatalf("composed metadata invented flattened fields: %+v", report)
	}
}

func FuzzPropertyMetadataNeverAdmitsInvalidOrNondeterministicSchema(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(metadataSchema),
		[]byte(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		[]byte(`{"type":"string"}`),
		[]byte(`{"type":"object","properties":{"x":{"$ref":"https://remote.test/x"}}}`),
		{0xff, '{', '}'},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, document []byte) {
		if len(document) > 8192 {
			t.Skip()
		}
		resolver := metadataResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
			return schema.ResolvedSchema{
				ID:       "https://schemas.example.test/fuzz.json",
				Document: append(json.RawMessage(nil), document...),
			}, nil
		})
		options := schema.PropertyOptions{
			Resolver: resolver,
			Limits: schema.Limits{
				MaxResolvedSchemaBytes: 8192,
				MaxSchemaDepth:         32,
				MaxSchemaValues:        4096,
				MaxStringBytes:         4096,
			},
			MaxProperties: 256,
		}
		first, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/fuzz", options)
		if err != nil {
			return
		}
		second, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/fuzz", options)
		if err != nil {
			t.Fatalf("accepted schema was not accepted again: %v", err)
		}
		left, _ := json.Marshal(first)
		right, _ := json.Marshal(second)
		if !slices.Equal(left, right) || first.SchemaID != "https://schemas.example.test/fuzz.json" || first.Digest == "" {
			t.Fatalf("accepted metadata was nondeterministic or lost identity:\n%s\n%s", left, right)
		}
	})
}

func BenchmarkResolvePropertyMetadata(b *testing.B) {
	resolver := metadataResolver(func(context.Context, string) (schema.ResolvedSchema, error) {
		return schema.ResolvedSchema{
			ID:       "https://schemas.example.test/editor-config-v1.json",
			Document: json.RawMessage(metadataSchema),
		}, nil
	})
	b.ReportAllocs()
	for range b.N {
		report, err := schema.ResolvePropertyMetadata(context.Background(), "schema://test/editor/v1", schema.PropertyOptions{
			Resolver: resolver,
		})
		if err != nil || len(report.Properties) != 3 {
			b.Fatalf("property metadata = %+v, %v", report, err)
		}
	}
}
