package editor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

type schemaMetadataOutcome struct {
	report schema.PropertyReport
	status ConfigSchemaStatus
	code   string
	level  Severity
}

func resolveSchemaMetadata(
	ctx context.Context,
	path string,
	entries map[string]catalogEntry,
	metadata []ElementMetadata,
	resolver schema.Resolver,
	schemaLimits schema.Limits,
	limits Limits,
) ([]Diagnostic, error) {
	if schemaLimits.MaxResolvedSchemaBytes == 0 ||
		schemaLimits.MaxResolvedSchemaBytes > limits.MaxResolvedSchemaBytes {
		schemaLimits.MaxResolvedSchemaBytes = limits.MaxResolvedSchemaBytes
	}
	byReference := make(map[string]schemaMetadataOutcome)
	totalBytes := 0
	for _, name := range sortedEntryNames(entries) {
		entry := entries[name]
		reference := entry.descriptor.ConfigSchema
		if reference == "" {
			continue
		}
		if _, found := byReference[reference]; found {
			continue
		}
		report, err := schema.ResolvePropertyMetadata(ctx, reference, schema.PropertyOptions{
			Resolver: resolver, Limits: schemaLimits, MaxProperties: limits.MaxSchemaProperties,
		})
		if err == nil {
			if totalBytes > limits.MaxTotalResolvedSchemaBytes-report.SchemaBytes {
				return nil, fmt.Errorf("editor resolved schemas exceed %d bytes", limits.MaxTotalResolvedSchemaBytes)
			}
			totalBytes += report.SchemaBytes
			outcome := schemaMetadataOutcome{report: report, status: ConfigSchemaResolved}
			if !report.Complete {
				outcome.code = "W_VALUES_METADATA_INCOMPLETE"
				outcome.level = SeverityWarning
			}
			byReference[reference] = outcome
			continue
		}
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("editor resolve schema metadata: %w", err)
		case errors.Is(err, schema.ErrLimitExceeded), errors.Is(err, schema.ErrInvalidOptions):
			return nil, fmt.Errorf("editor resolve schema metadata: %w", err)
		case errors.Is(err, schema.ErrInvalidResolvedSchema):
			byReference[reference] = schemaMetadataOutcome{
				status: ConfigSchemaInvalid, code: "E_VALUES_SCHEMA_INVALID", level: SeverityError,
			}
		default:
			byReference[reference] = schemaMetadataOutcome{
				status: ConfigSchemaUnresolved, code: "W_VALUES_SCHEMA_UNRESOLVED", level: SeverityWarning,
			}
		}
	}

	diagnostics := make([]Diagnostic, 0)
	anchor := syntax.Span{
		Start: syntax.Position{Line: 1, Column: 1},
		End:   syntax.Position{Line: 1, Column: 1},
	}
	for index := range metadata {
		contract := metadata[index].Config
		if contract.SchemaReference == "" {
			continue
		}
		outcome := byReference[contract.SchemaReference]
		contract.SchemaStatus = outcome.status
		if outcome.status == ConfigSchemaResolved {
			contract.SchemaID = outcome.report.SchemaID
			contract.SchemaDigest = outcome.report.Digest
			contract.PropertiesComplete = outcome.report.Complete
			contract.AdditionalProperties = slices.Clone(outcome.report.AdditionalProperties)
			contract.Properties = make([]ValuesPropertyMetadata, len(outcome.report.Properties))
			for propertyIndex, property := range outcome.report.Properties {
				contract.Properties[propertyIndex] = ValuesPropertyMetadata{
					Name: property.Name, Pointer: property.Pointer, Required: property.Required,
					Types: slices.Clone(property.Types), Title: property.Title,
					Description: property.Description, Format: property.Format,
					Default: slices.Clone(property.Default), Schema: slices.Clone(property.Schema),
				}
				values := property.Enum
				contract.Properties[propertyIndex].Enum = make([]json.RawMessage, len(values))
				for enumIndex := range values {
					contract.Properties[propertyIndex].Enum[enumIndex] = slices.Clone(values[enumIndex])
				}
			}
		}
		metadata[index].Config = contract
		entry := entries[metadata[index].Identity.Name]
		entry.metadata.Config = cloneConfigContract(contract)
		entries[metadata[index].Identity.Name] = entry
		if outcome.code == "" {
			continue
		}
		message := "values schema metadata for " + metadata[index].Identity.Name +
			" (" + contract.SchemaReference + ") "
		switch outcome.code {
		case "W_VALUES_METADATA_INCOMPLETE":
			message += "uses composition; direct properties are an incomplete projection"
		case "E_VALUES_SCHEMA_INVALID":
			message += "is invalid and no property metadata was admitted"
		default:
			message += "could not be resolved; no property metadata was invented"
		}
		diagnostics = append(diagnostics, Diagnostic{
			Code: outcome.code, Severity: outcome.level, Path: path, Span: anchor, Message: message,
		})
	}
	return diagnostics, nil
}
