package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	standardelements "github.com/bojieli/OpenRealtime/elements"
	graphschema "github.com/bojieli/OpenRealtime/graph/schema"
)

// SchemaResolver composes the Meeting Assistant plugin schemas over another
// exact resolver. It never performs filesystem or network fallback.
type SchemaResolver struct {
	base      graphschema.Resolver
	documents map[string]json.RawMessage
}

func NewSchemaResolver(base graphschema.Resolver) (*SchemaResolver, error) {
	documents := make(map[string]json.RawMessage, 2)
	for reference, document := range map[string]any{
		ScreenForkConfigSchema: map[string]any{
			"$schema": graphschema.Draft202012, "$id": ScreenForkConfigSchema,
			"type": "object", "additionalProperties": false,
			"required": []string{"source", "kind"},
			"properties": map[string]any{
				"source":          map[string]any{"type": "string", "minLength": 1, "maxLength": 1024},
				"kind":            map[string]any{"enum": []string{"screen", "camera", "video"}},
				"max_frame_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": maximumMaxFrameBytes},
			},
		},
		BackgroundConfigSchema: map[string]any{
			"$schema": graphschema.Draft202012, "$id": BackgroundConfigSchema,
			"type": "object", "additionalProperties": false,
			"required": []string{"role", "instruction"},
			"properties": map[string]any{
				"role":              map[string]any{"const": "background"},
				"instruction":       map[string]any{"type": "string", "minLength": 1, "maxLength": maximumInstructionBytes},
				"max_output_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 1_000_000},
				"max_runs":          map[string]any{"type": "integer", "minimum": 1, "maximum": maximumMaxBackgroundRuns},
				"max_text_bytes":    map[string]any{"type": "integer", "minimum": 1, "maximum": maximumBackgroundBytes},
				"terminal_memory":   map[string]any{"type": "integer", "minimum": 1, "maximum": maximumTerminalMemory},
			},
		},
	} {
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		documents[reference] = encoded
	}
	return &SchemaResolver{base: base, documents: documents}, nil
}

func DefaultSchemaResolver() (*SchemaResolver, error) {
	base, err := standardelements.StandardConfigSchemaCatalog()
	if err != nil {
		return nil, err
	}
	return NewSchemaResolver(base)
}

func (resolver *SchemaResolver) ResolveConfigSchema(
	ctx context.Context, reference string,
) (graphschema.ResolvedSchema, error) {
	if ctx == nil {
		return graphschema.ResolvedSchema{}, errors.New("resolve meeting config schema: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return graphschema.ResolvedSchema{}, err
	}
	if resolver == nil {
		return graphschema.ResolvedSchema{}, graphschema.ErrSchemaNotFound
	}
	if document, found := resolver.documents[reference]; found {
		return graphschema.ResolvedSchema{ID: reference, Document: slices.Clone(document)}, nil
	}
	if resolver.base == nil {
		return graphschema.ResolvedSchema{}, graphschema.ErrSchemaNotFound
	}
	return resolver.base.ResolveConfigSchema(ctx, reference)
}
