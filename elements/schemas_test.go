package elements_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	graphschema "github.com/bojieli/OpenRealtime/graph/schema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestStandardConfigSchemaCatalogCoversEveryFactoryContract(t *testing.T) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	descriptors, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	wanted := make(map[string]struct{})
	for _, name := range descriptors.Names() {
		descriptor, found := descriptors.Latest(name)
		if !found {
			t.Fatalf("descriptor catalog lost %s", name)
		}
		if descriptor.ConfigSchema != "" {
			wanted[descriptor.ConfigSchema] = struct{}{}
		}
	}
	references := make([]string, 0, len(wanted))
	for reference := range wanted {
		references = append(references, reference)
	}
	sort.Strings(references)
	if !reflect.DeepEqual(catalog.References(), references) {
		t.Fatalf("schema references = %v, want %v", catalog.References(), references)
	}
	if len(references) != 30 {
		t.Fatalf("standard config schema count = %d, want 30", len(references))
	}

	registrations, err := elements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, registration := range registrations {
		descriptor := registration.Factory.Descriptor()
		if descriptor.ConfigSchema == "" {
			continue
		}
		if _, ok := registration.Factory.(element.ConfigValidator); !ok {
			t.Errorf("factory %s has config schema %s but no ConfigValidator",
				registration.Profile.Reference, descriptor.ConfigSchema)
		}
	}
}

func TestStandardConfigSchemasCompileStrictlyAndAcceptFactoryValidSamples(t *testing.T) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	validators := standardValidatorsBySchema(t)
	for reference, source := range standardValidConfigSamples() {
		t.Run(reference, func(t *testing.T) {
			resolved, err := catalog.ResolveConfigSchema(context.Background(), reference)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.ID != reference {
				t.Fatalf("resolved schema ID = %q", resolved.ID)
			}
			compiled := compileStandardSchema(t, resolved)
			value := decodeSchemaValue(t, source)
			if err := compiled.Validate(value); err != nil {
				t.Fatalf("schema rejected factory-valid sample %s: %v", source, err)
			}
			validator, found := validators[reference]
			if !found {
				t.Fatalf("no exact factory validator for %s", reference)
			}
			if err := validator.ValidateConfig(json.RawMessage(source)); err != nil {
				t.Fatalf("factory rejected declared valid sample %s: %v", source, err)
			}

			object := value.(map[string]any)
			object["unknown_standard_field"] = true
			if err := compiled.Validate(object); err == nil {
				t.Fatal("standard config schema accepted an unknown top-level field")
			}
		})
	}
	if len(standardValidConfigSamples()) != len(catalog.References()) {
		t.Fatalf("valid sample count = %d, schema count = %d",
			len(standardValidConfigSamples()), len(catalog.References()))
	}
	for reference, source := range standardStructurallyInvalidConfigSamples() {
		t.Run(reference+"/structural_refusal", func(t *testing.T) {
			resolved, err := catalog.ResolveConfigSchema(context.Background(), reference)
			if err != nil {
				t.Fatal(err)
			}
			if err := compileStandardSchema(t, resolved).Validate(decodeSchemaValue(t, source)); err == nil {
				t.Fatalf("schema accepted structurally invalid sample %s", source)
			}
			if err := validators[reference].ValidateConfig(json.RawMessage(source)); err == nil {
				t.Fatalf("factory accepted structurally invalid sample %s", source)
			}
		})
	}
	if len(standardStructurallyInvalidConfigSamples()) != len(catalog.References()) {
		t.Fatalf("invalid sample count = %d, schema count = %d",
			len(standardStructurallyInvalidConfigSamples()), len(catalog.References()))
	}
}

func TestStandardSchemasDelegateNonRepresentableSemanticsToExactFactoryValidator(t *testing.T) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	validators := standardValidatorsBySchema(t)
	tests := map[string]string{
		"schema://openrealtime/ingress/user-content-config/v1":              `{"max_input_bytes":20,"max_pending_bytes":10}`,
		"schema://openrealtime/interaction/segment-prepared-text-config/v1": `{"max_segment_bytes":20,"max_run_bytes":10}`,
		"schema://openrealtime/interaction/speech-arbiter-config/v1":        `{"max_buffered_bytes":10,"max_delta_bytes":20}`,
		"schema://openrealtime/media/retained-media-config/v1":              `{"max_bytes":10,"max_item_bytes":20}`,
		"schema://openrealtime/speech/tts-config/v1":                        `{"provider":"tts","max_chunk_bytes":20,"max_audio_bytes":10}`,
		"schema://openrealtime/video/adaptive-observation-config/v1":        `{"source":"screen","min_interval_ms":20,"max_interval_ms":10}`,
	}
	for reference, source := range tests {
		t.Run(reference, func(t *testing.T) {
			resolved, err := catalog.ResolveConfigSchema(context.Background(), reference)
			if err != nil {
				t.Fatal(err)
			}
			if err := compileStandardSchema(t, resolved).Validate(decodeSchemaValue(t, source)); err != nil {
				t.Fatalf("structural schema unexpectedly modeled cross-field semantics: %v", err)
			}
			if err := validators[reference].ValidateConfig(json.RawMessage(source)); err == nil {
				t.Fatal("exact factory validator accepted invalid cross-field configuration")
			}
		})
	}
}

func TestStandardConfigSchemaResolverIsFailClosedCanceledAndSnapshotIsolated(t *testing.T) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ResolveConfigSchema(context.Background(), "schema://plugin/unknown/v1"); !errors.Is(err, graphschema.ErrSchemaNotFound) {
		t.Fatalf("unknown schema error = %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	want := errors.New("stop schema resolution")
	cancel(want)
	if _, err := catalog.ResolveConfigSchema(ctx, catalog.References()[0]); !errors.Is(err, want) {
		t.Fatalf("canceled resolution error = %v", err)
	}

	reference := catalog.References()[0]
	first, err := catalog.ResolveConfigSchema(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), first.Document...)
	first.Document[0] = '!'
	second, err := catalog.ResolveConfigSchema(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Document, original) {
		t.Fatal("resolver retained a caller mutation")
	}
	references := catalog.References()
	references[0] = "mutated"
	if catalog.References()[0] == "mutated" {
		t.Fatal("schema inventory retained a caller mutation")
	}
}

func FuzzStandardConfigSchemaResolverIsExactAndSnapshotIsolated(f *testing.F) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		f.Fatal(err)
	}
	known := make(map[string]struct{}, len(catalog.References()))
	for _, reference := range catalog.References() {
		known[reference] = struct{}{}
		f.Add(reference)
	}
	f.Add("schema://plugin/unknown/v1")
	f.Add("")
	f.Fuzz(func(t *testing.T, reference string) {
		resolved, err := catalog.ResolveConfigSchema(context.Background(), reference)
		_, exists := known[reference]
		if !exists {
			if !errors.Is(err, graphschema.ErrSchemaNotFound) {
				t.Fatalf("unknown reference %q error = %v", reference, err)
			}
			return
		}
		if err != nil || resolved.ID != reference || len(resolved.Document) == 0 {
			t.Fatalf("known reference %q resolution = %+v, %v", reference, resolved, err)
		}
		original := append([]byte(nil), resolved.Document...)
		resolved.Document[0] ^= 0xff
		again, err := catalog.ResolveConfigSchema(context.Background(), reference)
		if err != nil || !bytes.Equal(again.Document, original) {
			t.Fatalf("resolution for %q retained caller mutation", reference)
		}
	})
}

func BenchmarkStandardConfigSchemaCatalog(b *testing.B) {
	for iteration := 0; iteration < b.N; iteration++ {
		catalog, err := elements.StandardConfigSchemaCatalog()
		if err != nil {
			b.Fatal(err)
		}
		if len(catalog.References()) != 30 {
			b.Fatal("incomplete standard config schema catalog")
		}
	}
}

func BenchmarkStandardConfigSchemaResolve(b *testing.B) {
	catalog, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		b.Fatal(err)
	}
	references := catalog.References()
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		resolved, err := catalog.ResolveConfigSchema(
			context.Background(), references[iteration%len(references)],
		)
		if err != nil || len(resolved.Document) == 0 {
			b.Fatal(err)
		}
	}
}

func standardValidatorsBySchema(t *testing.T) map[string]element.ConfigValidator {
	t.Helper()
	registrations, err := elements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]element.ConfigValidator)
	for _, registration := range registrations {
		descriptor := registration.Factory.Descriptor()
		if descriptor.ConfigSchema == "" {
			continue
		}
		validator, ok := registration.Factory.(element.ConfigValidator)
		if !ok {
			t.Fatalf("factory %s has no ConfigValidator", registration.Profile.Reference)
		}
		if existing, duplicate := result[descriptor.ConfigSchema]; duplicate {
			// Two trajectory commit elements intentionally share one exact config
			// shape; either validator must accept/reject the same sample corpus.
			_ = existing
			continue
		}
		result[descriptor.ConfigSchema] = validator
	}
	return result
}

func standardValidConfigSamples() map[string]string {
	return map[string]string{
		"schema://openrealtime/acoustic/admission-config/v1":                `{}`,
		"schema://openrealtime/acoustic/endpoint-policy-config/v1":          `{}`,
		"schema://openrealtime/action/authorized-call-commit-config/v1":     `{}`,
		"schema://openrealtime/action/dispatch-config/v1":                   `{"registry":"tools","ledger":"actions"}`,
		"schema://openrealtime/action/ledger-commit-config/v1":              `{"ledger":"actions"}`,
		"schema://openrealtime/action/tool-lookup-config/v1":                `{"registry":"tools"}`,
		"schema://openrealtime/action/tool-result-commit-config/v1":         `{}`,
		"schema://openrealtime/authority/confirmation-config/v1":            `{"provider":"confirm"}`,
		"schema://openrealtime/authority/proposal-admission-config/v1":      `{}`,
		"schema://openrealtime/authority/provenance-join-config/v1":         `{}`,
		"schema://openrealtime/authority/target-fence-config/v1":            `{"target":"desktop"}`,
		"schema://openrealtime/cognition/text-model-config/v1":              `{"provider":"fast"}`,
		"schema://openrealtime/ingress/user-content-config/v1":              `{}`,
		"schema://openrealtime/interaction/model-result-commit-config/v1":   `{}`,
		"schema://openrealtime/interaction/post-commit-silence-config/v1":   `{"delay_ms":15000}`,
		"schema://openrealtime/interaction/segment-prepared-text-config/v1": `{}`,
		"schema://openrealtime/interaction/speech-arbiter-config/v1":        `{}`,
		"schema://openrealtime/media/attachment-resolver-config/v1":         `{}`,
		"schema://openrealtime/media/retained-media-config/v1":              `{}`,
		"schema://openrealtime/model/external-config/v1":                    `{"deployment":"omni"}`,
		"schema://openrealtime/perception/asr-config/v1":                    `{"provider":"asr"}`,
		"schema://openrealtime/perception/visual-observer-config/v1":        `{"provider":"vision","source":"screen"}`,
		"schema://openrealtime/policy/generate-on-observation-config/v1":    `{"role":"fast","invocation":{"instruction":"Answer briefly."}}`,
		"schema://openrealtime/policy/semantic-admission-config/v3":         `{"decider":"semantic-primary","direct_visual_input":true,"standing_extraction":true,"verify_voice_activation":true,"minimum_activation_confidence":0.75}`,
		"schema://openrealtime/policy/session-invocation-config/v1":         `{"role":"fast"}`,
		"schema://openrealtime/speech/playback-config/v1":                   `{"sink":"speaker"}`,
		"schema://openrealtime/speech/tts-config/v1":                        `{"provider":"tts"}`,
		"schema://openrealtime/trajectory/observation-commit-config/v1":     `{}`,
		"schema://openrealtime/trajectory/store-config/v1":                  `{}`,
		"schema://openrealtime/video/adaptive-observation-config/v1":        `{"source":"screen"}`,
	}
}

func standardStructurallyInvalidConfigSamples() map[string]string {
	return map[string]string{
		"schema://openrealtime/acoustic/admission-config/v1":                `{"threshold":2}`,
		"schema://openrealtime/acoustic/endpoint-policy-config/v1":          `{"mode":"external"}`,
		"schema://openrealtime/action/authorized-call-commit-config/v1":     `{"max_pending":5000}`,
		"schema://openrealtime/action/dispatch-config/v1":                   `{}`,
		"schema://openrealtime/action/ledger-commit-config/v1":              `{}`,
		"schema://openrealtime/action/tool-lookup-config/v1":                `{}`,
		"schema://openrealtime/action/tool-result-commit-config/v1":         `{"terminal_memory":5000}`,
		"schema://openrealtime/authority/confirmation-config/v1":            `{}`,
		"schema://openrealtime/authority/proposal-admission-config/v1":      `{"max_pending":5000}`,
		"schema://openrealtime/authority/provenance-join-config/v1":         `{"terminal_memory":5000}`,
		"schema://openrealtime/authority/target-fence-config/v1":            `{}`,
		"schema://openrealtime/cognition/text-model-config/v1":              `{}`,
		"schema://openrealtime/ingress/user-content-config/v1":              `{"max_pending":0}`,
		"schema://openrealtime/interaction/model-result-commit-config/v1":   `{"max_pending":0}`,
		"schema://openrealtime/interaction/post-commit-silence-config/v1":   `{}`,
		"schema://openrealtime/interaction/segment-prepared-text-config/v1": `{"minimum_runes":0}`,
		"schema://openrealtime/interaction/speech-arbiter-config/v1":        `{"max_pending_runs":0}`,
		"schema://openrealtime/media/attachment-resolver-config/v1":         `{"max_pending":0}`,
		"schema://openrealtime/media/retained-media-config/v1":              `{"max_items":0}`,
		"schema://openrealtime/model/external-config/v1":                    `{}`,
		"schema://openrealtime/perception/asr-config/v1":                    `{}`,
		"schema://openrealtime/perception/visual-observer-config/v1":        `{}`,
		"schema://openrealtime/policy/generate-on-observation-config/v1":    `{}`,
		"schema://openrealtime/policy/semantic-admission-config/v3":         `{}`,
		"schema://openrealtime/policy/session-invocation-config/v1":         `{}`,
		"schema://openrealtime/speech/playback-config/v1":                   `{}`,
		"schema://openrealtime/speech/tts-config/v1":                        `{}`,
		"schema://openrealtime/trajectory/observation-commit-config/v1":     `{"revision_namespace":""}`,
		"schema://openrealtime/trajectory/store-config/v1":                  `{"unexpected":true}`,
		"schema://openrealtime/video/adaptive-observation-config/v1":        `{}`,
	}
}

func compileStandardSchema(t *testing.T, resolved graphschema.ResolvedSchema) *jsonschema.Schema {
	t.Helper()
	value := decodeSchemaValue(t, string(resolved.Document))
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource(resolved.ID, value); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	compiled, err := compiler.Compile(resolved.ID)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return compiled
}

func decodeSchemaValue(t *testing.T, source string) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewBufferString(source))
	decoder.UseNumber()
	var result any
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode JSON %s: %v", source, err)
	}
	return result
}
