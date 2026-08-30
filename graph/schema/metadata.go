package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// PropertyMetadata is a deterministic, renderer-neutral view of one directly
// declared root configuration property. Schema retains the exact canonical
// subschema so a future renderer need not rely on this convenience projection.
type PropertyMetadata struct {
	Name        string            `json:"name"`
	Pointer     string            `json:"pointer"`
	Required    bool              `json:"required,omitempty"`
	Types       []string          `json:"types,omitempty"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Format      string            `json:"format,omitempty"`
	Default     json.RawMessage   `json:"default,omitempty"`
	Enum        []json.RawMessage `json:"enum,omitempty"`
	Schema      json.RawMessage   `json:"schema"`
}

// PropertyReport is one immutable resolved schema snapshot. Complete is false
// when object properties are selected through composition or a root $ref; in
// that case Properties remains an honest list of only directly declared
// fields and consumers must not treat it as a complete form model.
type PropertyReport struct {
	Reference            string             `json:"reference"`
	SchemaID             string             `json:"schema_id"`
	Digest               string             `json:"digest"`
	SchemaBytes          int                `json:"schema_bytes"`
	Complete             bool               `json:"complete"`
	Properties           []PropertyMetadata `json:"properties"`
	AdditionalProperties json.RawMessage    `json:"additional_properties,omitempty"`
}

// PropertyOptions bounds one schema resolution and metadata projection.
type PropertyOptions struct {
	Resolver      Resolver
	Limits        Limits
	MaxProperties int
}

// ResolvePropertyMetadata resolves and fully validates one self-contained
// Draft 2020-12 config schema before returning any field metadata. It performs
// no fallback I/O and never follows a non-local reference.
func ResolvePropertyMetadata(
	ctx context.Context, reference string, options PropertyOptions,
) (PropertyReport, error) {
	if ctx == nil {
		return PropertyReport{}, fmt.Errorf("%w: nil context", ErrInvalidOptions)
	}
	if reference == "" || reference != strings.TrimSpace(reference) {
		return PropertyReport{}, fmt.Errorf("%w: invalid config reference", ErrInvalidOptions)
	}
	if options.Resolver == nil {
		return PropertyReport{}, fmt.Errorf("%w: %q", ErrUnknownConfigContract, reference)
	}
	limits, err := options.Limits.normalized()
	if err != nil {
		return PropertyReport{}, err
	}
	maximum := options.MaxProperties
	if maximum == 0 {
		maximum = 4096
	}
	if maximum < 1 || maximum > 65_536 {
		return PropertyReport{}, fmt.Errorf("%w: max properties must be in 1..65536", ErrInvalidOptions)
	}
	if err := checkContext(ctx, "resolve property metadata"); err != nil {
		return PropertyReport{}, err
	}
	resolved, err := options.Resolver.ResolveConfigSchema(ctx, reference)
	if err != nil {
		if errors.Is(err, ErrSchemaNotFound) {
			return PropertyReport{}, fmt.Errorf("%w: %q", ErrUnknownConfigContract, reference)
		}
		return PropertyReport{}, fmt.Errorf("resolve config schema %q: %w", reference, err)
	}
	if err := checkContext(ctx, "resolve property metadata"); err != nil {
		return PropertyReport{}, err
	}
	document := slices.Clone(resolved.Document)
	if len(document) > limits.MaxResolvedSchemaBytes {
		return PropertyReport{}, limitError("resolved schema bytes", len(document), limits.MaxResolvedSchemaBytes)
	}
	definition, err := normalizeResolvedSchema(resolved.ID, document, limits)
	if err != nil {
		return PropertyReport{}, fmt.Errorf("%w: contract %q: %w", ErrInvalidResolvedSchema, reference, err)
	}
	root := definition.document
	properties := map[string]any{}
	if value, present := root["properties"]; present {
		properties = value.(map[string]any) // normalizeResolvedSchema verified this shape.
	}
	if len(properties) > maximum {
		return PropertyReport{}, limitError("schema properties", len(properties), maximum)
	}
	required := make(map[string]struct{})
	if raw, present := root["required"]; present {
		for _, value := range raw.([]any) { // JSON Schema compilation verified string entries.
			required[value.(string)] = struct{}{}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]PropertyMetadata, 0, len(names))
	for _, name := range names {
		schemaValue := properties[name]
		encoded, err := json.Marshal(schemaValue)
		if err != nil {
			return PropertyReport{}, fmt.Errorf("encode property %q metadata: %w", name, err)
		}
		item := PropertyMetadata{
			Name: name, Pointer: "#/properties/" + escapeJSONPointer(name), Schema: encoded,
		}
		_, item.Required = required[name]
		if object, ok := schemaValue.(map[string]any); ok {
			item.Types = schemaTypes(object["type"])
			item.Title, _ = object["title"].(string)
			item.Description, _ = object["description"].(string)
			item.Format, _ = object["format"].(string)
			if value, present := object["default"]; present {
				item.Default, err = json.Marshal(value)
				if err != nil {
					return PropertyReport{}, fmt.Errorf("encode property %q default: %w", name, err)
				}
			}
			if values, ok := object["enum"].([]any); ok {
				item.Enum = make([]json.RawMessage, len(values))
				for index, value := range values {
					item.Enum[index], err = json.Marshal(value)
					if err != nil {
						return PropertyReport{}, fmt.Errorf("encode property %q enum: %w", name, err)
					}
				}
			}
		}
		items = append(items, item)
	}
	complete := true
	for _, keyword := range []string{
		"$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "if", "then", "else",
		"patternProperties", "dependentSchemas", "unevaluatedProperties",
	} {
		if _, present := root[keyword]; present {
			complete = false
		}
	}
	for name := range required {
		if _, declared := properties[name]; !declared {
			complete = false
		}
	}
	var additional json.RawMessage
	if value, present := root["additionalProperties"]; present {
		additional, err = json.Marshal(value)
		if err != nil {
			return PropertyReport{}, fmt.Errorf("encode additionalProperties metadata: %w", err)
		}
	}
	digest := sha256.Sum256(definition.canonical)
	return PropertyReport{
		Reference: reference, SchemaID: definition.id,
		Digest: "sha256:" + hex.EncodeToString(digest[:]), SchemaBytes: len(definition.canonical), Complete: complete,
		Properties: items, AdditionalProperties: additional,
	}, nil
}

func schemaTypes(value any) []string {
	var result []string
	switch typed := value.(type) {
	case string:
		result = []string{typed}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
	}
	sort.Strings(result)
	return result
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func clonePropertyReport(value PropertyReport) PropertyReport {
	value.AdditionalProperties = slices.Clone(value.AdditionalProperties)
	items := value.Properties
	value.Properties = make([]PropertyMetadata, len(items))
	for index, item := range items {
		item.Types = slices.Clone(item.Types)
		item.Default = slices.Clone(item.Default)
		item.Schema = slices.Clone(item.Schema)
		values := item.Enum
		item.Enum = make([]json.RawMessage, len(values))
		for enumIndex := range values {
			item.Enum[enumIndex] = slices.Clone(values[enumIndex])
		}
		value.Properties[index] = item
	}
	return value
}
