package elements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	graphschema "github.com/bojieli/OpenRealtime/graph/schema"
)

// ConfigSchemaCatalog is the immutable standard-library values contract.
// It contains configuration structure only: provider deployments may add
// factories and services, but cannot reinterpret the built-in element
// configuration accepted by a frozen descriptor.
type ConfigSchemaCatalog struct {
	documents  map[string]json.RawMessage
	references []string
}

var standardConfigSchemas struct {
	once    sync.Once
	catalog *ConfigSchemaCatalog
	err     error
}

// StandardConfigSchemaCatalog constructs the complete, self-contained Draft
// 2020-12 schema catalog for standard element descriptors. JSON Schema checks
// every representable shape and bound during config.Plan creation; the exact
// Factory.ConfigValidator is deliberately run again by runtime.PreparePlan for
// semantic constraints such as one configured byte limit bounding another.
func StandardConfigSchemaCatalog() (*ConfigSchemaCatalog, error) {
	standardConfigSchemas.once.Do(func() {
		sources := standardConfigSchemaDocuments()
		documents := make(map[string]json.RawMessage, len(sources))
		references := make([]string, 0, len(sources))
		for reference, document := range sources {
			encoded, err := json.Marshal(document)
			if err != nil {
				standardConfigSchemas.err = fmt.Errorf(
					"encode standard config schema %s: %w", reference, err,
				)
				return
			}
			documents[reference] = encoded
			references = append(references, reference)
		}
		sort.Strings(references)
		standardConfigSchemas.catalog = &ConfigSchemaCatalog{
			documents: documents, references: references,
		}
	})
	return standardConfigSchemas.catalog, standardConfigSchemas.err
}

// ResolveConfigSchema implements graph/schema.Resolver without filesystem or
// network fallback. Unknown plugin contracts remain unknown and therefore
// fail config.Plan's RequireResolved deployment gate.
func (catalog *ConfigSchemaCatalog) ResolveConfigSchema(
	ctx context.Context,
	reference string,
) (graphschema.ResolvedSchema, error) {
	if ctx == nil {
		return graphschema.ResolvedSchema{}, errors.New("resolve standard config schema: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return graphschema.ResolvedSchema{}, err
	}
	if catalog == nil {
		return graphschema.ResolvedSchema{}, graphschema.ErrSchemaNotFound
	}
	document, found := catalog.documents[reference]
	if !found {
		return graphschema.ResolvedSchema{}, graphschema.ErrSchemaNotFound
	}
	return graphschema.ResolvedSchema{ID: reference, Document: slices.Clone(document)}, nil
}

// References returns the exact deterministic config-schema inventory.
func (catalog *ConfigSchemaCatalog) References() []string {
	if catalog == nil {
		return nil
	}
	return slices.Clone(catalog.references)
}

type schemaObject = map[string]any

func standardConfigSchemaDocuments() map[string]schemaObject {
	const (
		mib                    = 1 << 20
		maximumConfigBytes     = 1 << 30
		maximumBoundBytes      = 64 << 20
		maximumElementJSON     = 512 << 10
		maximumElementBinary   = 16 << 20
		maximumIdentifierBytes = 1024
	)

	identifier := func(maximum int) schemaObject {
		return stringSchema(1, maximum)
	}
	optionalIdentifier := func(maximum int) schemaObject {
		return stringSchema(0, maximum)
	}
	boundedState := integerSchema(0, 4096)
	largeBoundedState := integerSchema(1, 1_000_000)

	capabilityRequirement := objectSchema(schemaObject{
		"name":     identifier(maximumIdentifierBytes),
		"contract": optionalIdentifier(maximumIdentifierBytes),
	}, "name")
	mediaFormat := objectSchema(schemaObject{
		"kind":                   enumSchema("audio", "video"),
		"encoding":               identifier(128),
		"sample_format":          optionalIdentifier(128),
		"sample_rate_hz":         integerSchema(0, 768_000),
		"channels":               integerSchema(0, 64),
		"max_frame_duration_ms":  integerSchema(0, 10_000),
		"max_width":              integerSchema(0, 32_768),
		"max_height":             integerSchema(0, 32_768),
		"max_frame_rate_millihz": integerSchema(0, 1_000_000),
	}, "kind", "encoding")
	mediaFormat["allOf"] = []any{
		conditional(
			propertyEquals("kind", "audio"),
			schemaObject{
				"required": []string{"sample_format", "sample_rate_hz", "channels", "max_frame_duration_ms"},
				"properties": schemaObject{
					"sample_format":          identifier(128),
					"sample_rate_hz":         integerSchema(1, 768_000),
					"channels":               integerSchema(1, 64),
					"max_frame_duration_ms":  integerSchema(1, 10_000),
					"max_width":              integerSchema(0, 0),
					"max_height":             integerSchema(0, 0),
					"max_frame_rate_millihz": integerSchema(0, 0),
				},
			},
			schemaObject{
				"required": []string{"max_width", "max_height", "max_frame_rate_millihz"},
				"properties": schemaObject{
					"sample_format":          schemaObject{"type": "string", "maxLength": 0},
					"sample_rate_hz":         integerSchema(0, 0),
					"channels":               integerSchema(0, 0),
					"max_frame_duration_ms":  integerSchema(0, 0),
					"max_width":              integerSchema(1, 32_768),
					"max_height":             integerSchema(1, 32_768),
					"max_frame_rate_millihz": integerSchema(1, 1_000_000),
				},
			},
		),
	}
	wireFormat := objectSchema(schemaObject{
		"payload_mode":     enumSchema("json", "binary", "json_binary"),
		"max_json_bytes":   integerSchema(0, maximumElementJSON),
		"max_binary_bytes": integerSchema(0, maximumElementBinary),
		"media":            mediaFormat,
	}, "payload_mode")
	wireFormat["allOf"] = []any{
		conditional(propertyEquals("payload_mode", "json"), schemaObject{
			"required": []string{"max_json_bytes"},
			"properties": schemaObject{
				"max_json_bytes": integerSchema(1, maximumElementJSON), "max_binary_bytes": integerSchema(0, 0),
			},
		}, schemaObject{}),
		conditional(propertyEquals("payload_mode", "binary"), schemaObject{
			"required": []string{"max_binary_bytes"},
			"properties": schemaObject{
				"max_json_bytes": integerSchema(0, 0), "max_binary_bytes": integerSchema(1, maximumElementBinary),
			},
		}, schemaObject{}),
		conditional(propertyEquals("payload_mode", "json_binary"), schemaObject{
			"required": []string{"max_json_bytes", "max_binary_bytes"},
			"properties": schemaObject{
				"max_json_bytes":   integerSchema(1, maximumElementJSON),
				"max_binary_bytes": integerSchema(1, maximumElementBinary),
			},
		}, schemaObject{}),
		conditional(schemaObject{"required": []string{"media"}}, schemaObject{
			"properties": schemaObject{"payload_mode": enumSchema("binary", "json_binary")},
		}, schemaObject{}),
	}

	policyCapability := objectSchema(schemaObject{
		"name":                  identifier(256),
		"description":           schemaObject{"type": "string"},
		"available":             schemaObject{"type": "boolean"},
		"execution_phase":       schemaObject{"type": "string"},
		"confirmation_required": schemaObject{"type": "boolean"},
	}, "name")
	policyTool := objectSchema(schemaObject{
		"name":        identifier(256),
		"description": schemaObject{"type": "string"},
		"parameters":  schemaObject{},
		"background":  schemaObject{"type": "boolean"},
	}, "name", "parameters")
	invocation := objectSchema(schemaObject{
		"instruction":       stringSchema(1, mib),
		"source_revision":   integerSchema(0, 0),
		"capabilities":      arraySchema(policyCapability, 0, 4096),
		"tools":             arraySchema(policyTool, 0, 4096),
		"max_output_tokens": integerSchema(0, 1_000_000),
		"effort": schemaObject{
			"type": "string", "pattern": `^(minimal|low|medium|high|\+?[0-9]+)$`,
		},
	}, "instruction")

	documents := map[string]schemaObject{
		"schema://openrealtime/acoustic/admission-config/v1": standardObject(
			"schema://openrealtime/acoustic/admission-config/v1",
			schemaObject{
				"threshold":           numberSchema(0, 1),
				"prefix_padding_ms":   integerSchema(0, 10_000),
				"silence_duration_ms": integerSchema(1, 60_000),
				"speech_duration_ms":  integerSchema(0, 10_000),
				"source":              optionalIdentifier(256),
				"cancellation_memory": boundedState,
				"terminal_memory":     boundedState,
				"max_frame_bytes":     zeroOrIntegerRange(2, 32<<20),
				"max_sample_rate_hz":  integerSchema(0, 768_000),
			},
		),
		"schema://openrealtime/acoustic/endpoint-policy-config/v1": standardObject(
			"schema://openrealtime/acoustic/endpoint-policy-config/v1",
			schemaObject{
				"mode":                 enumSchema("automatic", "manual", "external"),
				"candidate_timeout_ms": integerSchema(0, 600_000),
				"fallback":             enumSchema("close", "reopen"),
				"cancellation_memory":  boundedState,
				"terminal_memory":      boundedState,
			},
		),
		"schema://openrealtime/authority/proposal-admission-config/v1": standardObject(
			"schema://openrealtime/authority/proposal-admission-config/v1",
			schemaObject{"max_pending": boundedState, "allow_system": schemaObject{"type": "boolean"}},
		),
		"schema://openrealtime/authority/provenance-join-config/v1": standardObject(
			"schema://openrealtime/authority/provenance-join-config/v1",
			schemaObject{"max_pending": boundedState, "terminal_memory": boundedState},
		),
		"schema://openrealtime/action/tool-lookup-config/v1": standardObject(
			"schema://openrealtime/action/tool-lookup-config/v1",
			schemaObject{"registry": identifier(0)}, "registry",
		),
		"schema://openrealtime/authority/confirmation-config/v1": standardObject(
			"schema://openrealtime/authority/confirmation-config/v1",
			schemaObject{"provider": identifier(0), "max_pending": boundedState}, "provider",
		),
		"schema://openrealtime/authority/target-fence-config/v1": standardObject(
			"schema://openrealtime/authority/target-fence-config/v1",
			schemaObject{"target": identifier(0)}, "target",
		),
		"schema://openrealtime/action/ledger-commit-config/v1": standardObject(
			"schema://openrealtime/action/ledger-commit-config/v1",
			schemaObject{"ledger": identifier(0), "max_pending": boundedState}, "ledger",
		),
		"schema://openrealtime/action/authorized-call-commit-config/v1": standardObject(
			"schema://openrealtime/action/authorized-call-commit-config/v1",
			schemaObject{"max_pending": boundedState, "terminal_memory": boundedState},
		),
		"schema://openrealtime/action/tool-result-commit-config/v1": standardObject(
			"schema://openrealtime/action/tool-result-commit-config/v1",
			schemaObject{"max_pending": boundedState, "terminal_memory": boundedState},
		),
		"schema://openrealtime/action/dispatch-config/v1": standardObject(
			"schema://openrealtime/action/dispatch-config/v1",
			schemaObject{
				"registry": identifier(0), "ledger": identifier(0),
				"max_pending": boundedState, "max_completed": boundedState,
			}, "registry", "ledger",
		),
		"schema://openrealtime/cognition/text-model-config/v1": standardObject(
			"schema://openrealtime/cognition/text-model-config/v1",
			schemaObject{
				"provider": identifier(0), "retain_reasoning": schemaObject{"type": "boolean"},
				"max_output_bytes":   zeroOrIntegerRange(1, 64<<20),
				"max_events":         zeroOrIntegerRange(1, 1_000_000),
				"max_tool_proposals": zeroOrIntegerRange(1, 4096),
			}, "provider",
		),
		"schema://openrealtime/ingress/user-content-config/v1": standardObject(
			"schema://openrealtime/ingress/user-content-config/v1",
			schemaObject{
				"observer": identifier(256), "text_source": identifier(256),
				"attachment_source": identifier(256), "retention_scope": identifier(256),
				"max_text_bytes":     integerSchema(1, 64<<20),
				"max_metadata_bytes": integerSchema(1, mib),
				"max_input_bytes":    integerSchema(1, maximumConfigBytes),
				"max_pending":        largeBoundedState, "max_pending_bytes": integerSchema(1, maximumConfigBytes),
				"max_streams": largeBoundedState, "terminal_memory": largeBoundedState,
			},
		),
		"schema://openrealtime/interaction/segment-prepared-text-config/v1": standardObject(
			"schema://openrealtime/interaction/segment-prepared-text-config/v1",
			schemaObject{
				"minimum_runes":     integerSchema(1, 4096),
				"max_segment_bytes": integerSchema(1, maximumBoundBytes),
				"max_run_bytes":     integerSchema(1, maximumBoundBytes),
				"max_segments":      integerSchema(1, 4096), "terminal_memory": integerSchema(1, 4096),
			},
		),
		"schema://openrealtime/interaction/speech-arbiter-config/v1": standardObject(
			"schema://openrealtime/interaction/speech-arbiter-config/v1",
			schemaObject{
				"max_pending_runs":    integerSchema(1, 4096),
				"max_buffered_bytes":  integerSchema(1, maximumBoundBytes),
				"max_buffered_deltas": integerSchema(1, 1_000_000),
				"max_delta_bytes":     integerSchema(1, maximumBoundBytes),
			},
		),
		"schema://openrealtime/interaction/model-result-commit-config/v1": standardObject(
			"schema://openrealtime/interaction/model-result-commit-config/v1",
			schemaObject{"max_pending": integerSchema(1, 4096)},
		),
		"schema://openrealtime/interaction/post-commit-silence-config/v1": standardObject(
			"schema://openrealtime/interaction/post-commit-silence-config/v1",
			schemaObject{"delay_ms": integerSchema(1, 600_000)}, "delay_ms",
		),
		"schema://openrealtime/media/attachment-resolver-config/v1": standardObject(
			"schema://openrealtime/media/attachment-resolver-config/v1",
			schemaObject{
				"max_pending": largeBoundedState, "max_bytes": integerSchema(1, maximumConfigBytes),
				"max_metadata_bytes": integerSchema(1, mib),
				"allowed_mime_types": arraySchema(stringSchema(1, 256), 0, 256),
				"terminal_memory":    largeBoundedState,
			},
		),
		"schema://openrealtime/media/retained-media-config/v1": standardObject(
			"schema://openrealtime/media/retained-media-config/v1",
			schemaObject{
				"scope": identifier(256), "max_items": largeBoundedState,
				"max_bytes":          integerSchema(1, maximumConfigBytes),
				"max_item_bytes":     integerSchema(1, maximumConfigBytes),
				"max_metadata_bytes": integerSchema(1, mib),
				"max_active_leases":  largeBoundedState,
				"window_ms":          integerSchema(0, 31_536_000_000),
				"terminal_memory":    largeBoundedState, "cancel_memory": largeBoundedState,
			},
		),
		"schema://openrealtime/model/external-config/v1": standardObject(
			"schema://openrealtime/model/external-config/v1",
			schemaObject{
				"deployment":            identifier(1024),
				"settings":              schemaObject{"type": "object"},
				"required_capabilities": arraySchema(capabilityRequirement, 0, 256),
				"port_formats": schemaObject{
					"type": "object", "maxProperties": 256,
					"propertyNames":        stringSchema(1, 1024),
					"additionalProperties": arraySchema(wireFormat, 1, 16),
				},
			}, "deployment",
		),
		"schema://openrealtime/perception/asr-config/v1": standardObject(
			"schema://openrealtime/perception/asr-config/v1",
			schemaObject{
				"provider": identifier(0), "name": schemaObject{"type": "string"},
				"source": schemaObject{"type": "string"},
			}, "provider",
		),
		"schema://openrealtime/perception/visual-observer-config/v1": standardObject(
			"schema://openrealtime/perception/visual-observer-config/v1",
			schemaObject{
				"provider": identifier(0), "source": identifier(0), "name": schemaObject{"type": "string"},
				"change_threshold": numberSchema(0, 1), "attach_keyframes": schemaObject{"type": "boolean"},
				"max_frames_per_batch": integerSchema(1, 256),
				"max_frame_bytes":      integerSchema(1, maximumBoundBytes),
			}, "provider", "source",
		),
		"schema://openrealtime/policy/generate-on-observation-config/v1": standardObject(
			"schema://openrealtime/policy/generate-on-observation-config/v1",
			schemaObject{
				"role": identifier(256), "invocation": invocation,
				"max_pending": largeBoundedState, "terminal_memory": largeBoundedState,
				"cancel_memory": largeBoundedState,
			}, "role", "invocation",
		),
		"schema://openrealtime/policy/session-invocation-config/v1": standardObject(
			"schema://openrealtime/policy/session-invocation-config/v1",
			schemaObject{
				"role": identifier(256), "terminal_memory": largeBoundedState,
				"cancel_memory": largeBoundedState,
			}, "role",
		),
		"schema://openrealtime/policy/semantic-admission-config/v1": standardObject(
			"schema://openrealtime/policy/semantic-admission-config/v1",
			schemaObject{
				"decider": identifier(1024), "recent_lines": integerSchema(1, 4096),
				"max_pending": largeBoundedState, "terminal_memory": largeBoundedState,
				"cancel_memory": largeBoundedState,
			}, "decider",
		),
		"schema://openrealtime/speech/tts-config/v1": standardObject(
			"schema://openrealtime/speech/tts-config/v1",
			schemaObject{
				"provider": identifier(0), "max_text_bytes": zeroOrIntegerRange(1, maximumBoundBytes),
				"max_chunk_bytes": zeroOrIntegerRange(2, maximumBoundBytes),
				"max_audio_bytes": zeroOrIntegerRange(2, maximumBoundBytes),
				"cancel_memory":   zeroOrIntegerRange(1, 4096),
			}, "provider",
		),
		"schema://openrealtime/speech/playback-config/v1": standardObject(
			"schema://openrealtime/speech/playback-config/v1",
			schemaObject{
				"sink": identifier(0), "frame_duration_ms": zeroOrIntegerRange(1, 1000),
				"max_chunk_bytes": zeroOrIntegerRange(2, maximumBoundBytes),
				"cancel_memory":   zeroOrIntegerRange(1, 4096),
			}, "sink",
		),
		"schema://openrealtime/trajectory/observation-commit-config/v1": standardObject(
			"schema://openrealtime/trajectory/observation-commit-config/v1",
			schemaObject{"revision_namespace": identifier(0)},
		),
		"schema://openrealtime/trajectory/store-config/v1": standardObject(
			"schema://openrealtime/trajectory/store-config/v1", schemaObject{},
		),
		"schema://openrealtime/video/adaptive-observation-config/v1": standardObject(
			"schema://openrealtime/video/adaptive-observation-config/v1",
			schemaObject{
				"source": identifier(0), "mode": enumSchema("fixed", "adaptive", "manual"),
				"fixed_interval_ms":  integerSchema(1, 86_400_000),
				"min_interval_ms":    integerSchema(1, 86_400_000),
				"max_interval_ms":    integerSchema(1, 86_400_000),
				"change_threshold":   numberSchema(0, 1),
				"max_frame_bytes":    integerSchema(1, maximumBoundBytes),
				"max_change_samples": integerSchema(16, 4096),
				"max_metadata_bytes": integerSchema(256, mib),
				"terminal_memory":    largeBoundedState,
			}, "source",
		),
	}

	endpoint := documents["schema://openrealtime/acoustic/endpoint-policy-config/v1"]
	endpoint["allOf"] = []any{conditional(
		propertyEquals("mode", "external"),
		schemaObject{
			"required":   []string{"candidate_timeout_ms"},
			"properties": schemaObject{"candidate_timeout_ms": integerSchema(1, 600_000)},
		},
		schemaObject{"properties": schemaObject{"candidate_timeout_ms": integerSchema(0, 0)}},
	)}
	return documents
}

func standardObject(id string, properties schemaObject, required ...string) schemaObject {
	result := objectSchema(properties, required...)
	result["$schema"] = graphschema.Draft202012
	result["$id"] = id
	return result
}

func objectSchema(properties schemaObject, required ...string) schemaObject {
	result := schemaObject{
		"type": "object", "properties": properties, "additionalProperties": false,
	}
	if len(required) != 0 {
		result["required"] = slices.Clone(required)
	}
	return result
}

func stringSchema(minimum, maximum int) schemaObject {
	result := schemaObject{"type": "string"}
	if minimum > 0 {
		result["minLength"] = minimum
	}
	if maximum > 0 {
		result["maxLength"] = maximum
	}
	return result
}

func integerSchema(minimum, maximum int64) schemaObject {
	return schemaObject{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func numberSchema(minimum, maximum float64) schemaObject {
	return schemaObject{"type": "number", "minimum": minimum, "maximum": maximum}
}

func zeroOrIntegerRange(minimum, maximum int64) schemaObject {
	return schemaObject{"anyOf": []any{
		schemaObject{"type": "integer", "const": 0},
		integerSchema(minimum, maximum),
	}}
}

func enumSchema(values ...string) schemaObject {
	return schemaObject{"type": "string", "enum": slices.Clone(values)}
}

func arraySchema(items schemaObject, minimum, maximum int) schemaObject {
	return schemaObject{
		"type": "array", "items": items, "minItems": minimum, "maxItems": maximum,
	}
}

func propertyEquals(name string, value any) schemaObject {
	return schemaObject{
		"required": []string{name}, "properties": schemaObject{name: schemaObject{"const": value}},
	}
}

func conditional(ifSchema, thenSchema, elseSchema schemaObject) schemaObject {
	return schemaObject{"if": ifSchema, "then": thenSchema, "else": elseSchema}
}
