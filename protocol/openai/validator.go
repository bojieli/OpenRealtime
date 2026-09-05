package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// bundle is the compiled pinned schema closure.
type bundle struct {
	schemas map[string]*jsonschema.Schema
	err     error
}

// bundleCompilations counts how many times the pinned bundle has been compiled
// in this process.
//
// It exists so a test can assert the count is one. Compiling is correct however
// often it happens, so no functional test can see the difference; the cost
// appears only as connection latency and resident memory under load, which is
// exactly where a regression would go unnoticed. Compiling the 370 KiB bundle
// into its 133 schemas costs roughly 34 ms and retains about 1.6 MiB, so a
// per-session compile is a third of a second of CPU and 1.6 GiB of duplicated
// immutable data at a thousand concurrent sessions.
var bundleCompilations atomic.Uint64

// compiledBundle compiles the pinned schema closure once for the process.
//
// Sharing it is safe because it is immutable after compilation: the compiler is
// used only here, and jsonschema.Schema.Validate reads. A validator therefore
// holds no state of its own and one per session costs nothing.
var compiledBundle = sync.OnceValue(compile)

// Validator checks complete events against the pinned OpenAI Realtime schema.
//
// The zero value is usable and every instance shares the process-wide compiled
// bundle. It remains a named type with a constructor because callers hold one
// per connection and a future per-connection option - a profile restriction, a
// strictness level - belongs on it rather than on a package-level function.
type Validator struct{}

func NewValidator() *Validator {
	return &Validator{}
}

func (validator *Validator) Validate(
	profile Profile,
	direction Direction,
	message Message,
) error {
	definition, ok := Lookup(profile, direction, message.Type())
	if !ok {
		return fmt.Errorf("event %q is not defined for %s/%s", message.Type(), profile, direction)
	}
	compiled := compiledBundle()
	if compiled.err != nil {
		return compiled.err
	}
	schema := compiled.schemas[definition.SchemaRef]
	if schema == nil {
		return fmt.Errorf("schema %s was not compiled", definition.SchemaRef)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(message.raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode event for validation: %w", err)
	}
	if err := schema.Validate(value); err != nil {
		return fmt.Errorf("%s %s event is not OpenAI-compatible: %w", direction, message.Type(), err)
	}
	return nil
}

func compile() bundle {
	bundleCompilations.Add(1)
	if len(SchemaBundle) == 0 {
		return bundle{err: errors.New("embedded OpenAI Realtime schema bundle is empty")}
	}
	compiler := jsonschema.NewCompiler()
	const resourceURL = "https://openrealtime.dev/schemas/openai-realtime-events.json"
	var document any
	decoder := json.NewDecoder(bytes.NewReader(SchemaBundle))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return bundle{err: fmt.Errorf("decode OpenAI Realtime schema bundle: %w", err)}
	}
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return bundle{err: fmt.Errorf("load OpenAI Realtime schema bundle: %w", err)}
	}
	schemas := make(map[string]*jsonschema.Schema)
	for _, definition := range generatedDefinitions {
		if _, exists := schemas[definition.SchemaRef]; exists {
			continue
		}
		schema, err := compiler.Compile(resourceURL + definition.SchemaRef)
		if err != nil {
			return bundle{err: fmt.Errorf("compile %s: %w", definition.SchemaRef, err)}
		}
		schemas[definition.SchemaRef] = schema
	}
	return bundle{schemas: schemas}
}
