package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Validator struct {
	once     sync.Once
	schemas  map[string]*jsonschema.Schema
	buildErr error
}

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
	validator.once.Do(validator.compile)
	if validator.buildErr != nil {
		return validator.buildErr
	}
	schema := validator.schemas[definition.SchemaRef]
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

func (validator *Validator) compile() {
	if len(SchemaBundle) == 0 {
		validator.buildErr = errors.New("embedded OpenAI Realtime schema bundle is empty")
		return
	}
	compiler := jsonschema.NewCompiler()
	const resourceURL = "https://openrealtime.dev/schemas/openai-realtime-events.json"
	var document any
	decoder := json.NewDecoder(bytes.NewReader(SchemaBundle))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		validator.buildErr = fmt.Errorf("decode OpenAI Realtime schema bundle: %w", err)
		return
	}
	if err := compiler.AddResource(resourceURL, document); err != nil {
		validator.buildErr = fmt.Errorf("load OpenAI Realtime schema bundle: %w", err)
		return
	}
	validator.schemas = make(map[string]*jsonschema.Schema)
	for _, definition := range generatedDefinitions {
		if _, exists := validator.schemas[definition.SchemaRef]; exists {
			continue
		}
		schema, err := compiler.Compile(resourceURL + definition.SchemaRef)
		if err != nil {
			validator.buildErr = fmt.Errorf("compile %s: %w", definition.SchemaRef, err)
			return
		}
		validator.schemas[definition.SchemaRef] = schema
	}
}
