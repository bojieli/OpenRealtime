// Package agenttool provides an extensible, authority-aware tool registry for
// canonical-trajectory continuations.
package agenttool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Handler executes one validated tool call and returns a JSON-marshalable
// result. The complete call, including its stable ID, is supplied so handlers
// can propagate an idempotency key to external systems. Handlers must honor
// context cancellation where their dependencies support it.
type Handler func(context.Context, trajectory.ToolCall) (any, error)

// Authorization decides whether a call may execute. It is evaluated before a
// handler and is independent of the model phase that proposed the call.
type Authorization interface {
	Authorize(context.Context, Definition, trajectory.ToolCall) error
}

// AuthorizationFunc adapts a function to Authorization.
type AuthorizationFunc func(context.Context, Definition, trajectory.ToolCall) error

func (function AuthorizationFunc) Authorize(ctx context.Context, definition Definition, call trajectory.ToolCall) error {
	return function(ctx, definition, call)
}

// Definition combines the model-visible schema with execution policy.
type Definition struct {
	Tool                 continuation.ToolDefinition `json:"tool"`
	Capability           continuation.Capability     `json:"capability"`
	ReadOnly             bool                        `json:"read_only"`
	ConfirmationRequired bool                        `json:"confirmation_required"`
}

type entry struct {
	definition Definition
	handler    Handler
	schema     *jsonschema.Schema
}

type callState struct {
	name      string
	arguments string
	done      chan struct{}
	result    trajectory.ToolResult
}

// Registry stores immutable definitions and provides idempotent execution by
// tool call ID.
type Registry struct {
	mu sync.Mutex

	authorization Authorization
	entries       map[string]entry
	order         []string
	calls         map[string]*callState
}

// NewRegistry creates an empty registry. A nil authorization permits only
// read-only tools that do not require confirmation.
func NewRegistry(authorization Authorization) *Registry {
	return &Registry{
		authorization: authorization,
		entries:       make(map[string]entry), calls: make(map[string]*callState),
	}
}

// Register adds one definition. Registration is rejected after a duplicate
// name; existing definitions are never silently replaced.
func (registry *Registry) Register(definition Definition, handler Handler) error {
	if handler == nil {
		return errors.New("tool handler is required")
	}
	if err := validateDefinition(definition); err != nil {
		return err
	}
	schema, err := compileSchema(definition.Tool)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	name := definition.Tool.Name
	if _, exists := registry.entries[name]; exists {
		return fmt.Errorf("tool %q is already registered", name)
	}
	registry.entries[name] = entry{definition: cloneDefinition(definition), handler: handler, schema: schema}
	registry.order = append(registry.order, name)
	return nil
}

// Capabilities returns a stable registration-order snapshot for both fast and
// slow model phases.
func (registry *Registry) Capabilities() []continuation.Capability {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	result := make([]continuation.Capability, 0, len(registry.order))
	for _, name := range registry.order {
		result = append(result, registry.entries[name].definition.Capability)
	}
	return result
}

// Tools returns executable schemas for the authorized continuation phase.
func (registry *Registry) Tools() []continuation.ToolDefinition {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	result := make([]continuation.ToolDefinition, 0, len(registry.order))
	for _, name := range registry.order {
		tool := registry.entries[name].definition.Tool
		tool.Parameters = slices.Clone(tool.Parameters)
		result = append(result, tool)
	}
	return result
}

// Execute runs a call at most once within this registry, including when
// identical calls arrive concurrently. Repeating a call ID returns the retained
// result. Reusing an ID with different input is rejected. Call IDs should also
// be propagated by handlers to external systems that support idempotency.
func (registry *Registry) Execute(ctx context.Context, call trajectory.ToolCall) trajectory.ToolResult {
	if err := validateCall(call); err != nil {
		return failedResult(call, err.Error())
	}

	registry.mu.Lock()
	registered, exists := registry.entries[call.Name]
	if !exists {
		registry.mu.Unlock()
		return failedResult(call, "tool is not registered")
	}
	if prior, exists := registry.calls[call.CallID]; exists {
		registry.mu.Unlock()
		if prior.name != call.Name || prior.arguments != string(call.Arguments) {
			return failedResult(call, "tool call ID was reused with different input")
		}
		select {
		case <-prior.done:
			return cloneResult(prior.result)
		case <-ctx.Done():
			return failedResult(call, "wait for existing tool call: "+ctx.Err().Error())
		}
	}
	state := &callState{
		name: call.Name, arguments: string(call.Arguments), done: make(chan struct{}),
	}
	registry.calls[call.CallID] = state
	registry.mu.Unlock()

	var result trajectory.ToolResult
	if err := validateArguments(registered.schema, call.Arguments); err != nil {
		result = failedResult(call, "invalid tool arguments: "+err.Error())
	} else if registry.authorization == nil {
		if !registered.definition.ReadOnly || registered.definition.ConfirmationRequired {
			result = failedResult(call, "tool execution requires explicit authorization")
		}
	} else if err := registry.authorization.Authorize(ctx, registered.definition, call); err != nil {
		result = failedResult(call, "authorization denied: "+err.Error())
	}

	if result.Error == "" {
		value, err := invokeHandler(ctx, registered.handler, cloneCall(call))
		result = trajectory.ToolResult{CallID: call.CallID, Name: call.Name}
		if err != nil {
			result.Error = err.Error()
		} else {
			encoded, encodeErr := json.Marshal(value)
			if encodeErr != nil {
				result.Error = "encode tool result: " + encodeErr.Error()
			} else {
				result.Output = encoded
			}
		}
	}

	registry.mu.Lock()
	state.result = cloneResult(result)
	close(state.done)
	registry.mu.Unlock()
	return cloneResult(result)
}

func compileSchema(tool continuation.ToolDefinition) (*jsonschema.Schema, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(tool.Parameters))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode tool %q parameter schema: %w", tool.Name, err)
	}
	compiler := jsonschema.NewCompiler()
	resourceURL := "https://openrealtime.dev/tool-schema/" + url.PathEscape(tool.Name) + ".json"
	if err := compiler.AddResource(resourceURL, document); err != nil {
		return nil, fmt.Errorf("load tool %q parameter schema: %w", tool.Name, err)
	}
	schema, err := compiler.Compile(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("compile tool %q parameter schema: %w", tool.Name, err)
	}
	return schema, nil
}

func validateCall(call trajectory.ToolCall) error {
	if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool arguments must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &object); err != nil || object == nil {
		return errors.New("tool arguments must be one JSON object")
	}
	return nil
}

func validateArguments(schema *jsonschema.Schema, arguments json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return schema.Validate(value)
}

func invokeHandler(ctx context.Context, handler Handler, call trajectory.ToolCall) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool handler panicked: %v", recovered)
		}
	}()
	return handler(ctx, call)
}

func failedResult(call trajectory.ToolCall, message string) trajectory.ToolResult {
	return trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Error: message}
}

func cloneCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func validateDefinition(definition Definition) error {
	tool := definition.Tool
	capability := definition.Capability
	if strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Description) == "" {
		return errors.New("tool name and description are required")
	}
	if len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) {
		return errors.New("tool parameters must be valid JSON")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema == nil {
		return errors.New("tool parameters must be one JSON object")
	}
	if capability.Name != tool.Name || strings.TrimSpace(capability.Description) == "" || !capability.Available {
		return errors.New("available capability must match the tool name and include a description")
	}
	if capability.ConfirmationRequired != definition.ConfirmationRequired {
		return errors.New("capability and definition confirmation policy differ")
	}
	return nil
}

func cloneDefinition(definition Definition) Definition {
	definition.Tool.Parameters = slices.Clone(definition.Tool.Parameters)
	return definition
}

func cloneResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = slices.Clone(result.Output)
	return result
}
