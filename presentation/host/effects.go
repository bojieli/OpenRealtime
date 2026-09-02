package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/coder/websocket"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	effectPermissionKind = "effect.local"
	effectOperation      = "execute"

	artifactEffectResource = "presentation-artifact"
	downloadEffectResource = "presentation-download"

	defaultMaxEffectSessions        = 64
	defaultMaxEffectMessageBytes    = 12 << 20
	defaultMaxEffectResultBytes     = 64 << 10
	defaultMaxEffectInFlight        = 16
	defaultMaxEffectCalls           = 1024
	defaultMaxEffectAuditRecords    = 2048
	defaultEffectConfirmationMS     = 30_000
	defaultEffectExecutionMS        = 30_000
	maximumEffectSessions           = 1024
	minimumEffectMessageBytes       = 64 << 10
	maximumEffectMessageBytes       = 48 << 20
	maximumEffectResultBytes        = 1 << 20
	maximumEffectInFlight           = 256
	maximumEffectCalls              = 16_384
	maximumEffectAuditRecords       = 16_384
	maximumEffectTerminalBytes      = 128 << 20
	minimumEffectTimeoutMS          = 100
	maximumEffectTimeoutMS          = 300_000
	maximumEffectTools              = 128
	maximumEffectNameBytes          = 128
	maximumEffectDescriptionBytes   = 4096
	maximumEffectParametersBytes    = 64 << 10
	maximumEffectTargetBytes        = 256
	maximumEffectAuthorityBytes     = 8192
	maximumEffectProtocolErrorBytes = 4096
)

// EffectChannel is presentation metadata selected by the enforcing provider.
// A client may render channels differently, but it cannot select one for a
// call or infer one from a tool name.
type EffectChannel string

const (
	EffectChannelTool     EffectChannel = "tool"
	EffectChannelComputer EffectChannel = "computer"
	EffectChannelArtifact EffectChannel = "artifact"
	EffectChannelDownload EffectChannel = "download"
)

// EffectDeclaration is the immutable, payload-free definition advertised by
// the host. Confirm is enforced here; SessionConfirm is always "never"
// because the local host, rather than the remote model service, owns the
// person's confirmation exchange.
type EffectDeclaration struct {
	Name           string               `json:"name"`
	Description    string               `json:"description"`
	Parameters     json.RawMessage      `json:"parameters"`
	Confirm        legacyaction.Confirm `json:"host_confirmation"`
	SessionConfirm legacyaction.Confirm `json:"session_confirmation"`
	Target         string               `json:"target,omitempty"`
	Mutating       bool                 `json:"mutating"`
	Channel        EffectChannel        `json:"channel"`
	Digest         string               `json:"digest"`
}

func (declaration EffectDeclaration) clone() EffectDeclaration {
	declaration.Parameters = slices.Clone(declaration.Parameters)
	return declaration
}

// EffectCall is the canonical call handed to an executor or target fence.
// Arguments have already passed strict JSON and the declaration's JSON
// Schema. AuthorityEvidence never crosses this boundary.
type EffectCall struct {
	ScopeID           string
	SessionID         string
	CallID            string
	Name              string
	Arguments         json.RawMessage
	ArgumentsDigest   string
	DeclarationDigest string
	Target            string
}

// EffectResult is a bounded result. Artifact and download references contain
// metadata only; their model-authored bytes stay in the independently scoped
// stores and routes.
type EffectResult struct {
	Output   string
	Artifact *Artifact
	Download *Download
}

// EffectExecutor performs one admitted effect. Implementations must honor the
// supplied context and be idempotent by CallID as a second line of defence;
// the host also serialises and caches one result for each admitted call ID.
type EffectExecutor interface {
	Execute(context.Context, EffectCall) (EffectResult, error)
}

type EffectExecutorFunc func(context.Context, EffectCall) (EffectResult, error)

func (function EffectExecutorFunc) Execute(ctx context.Context, call EffectCall) (EffectResult, error) {
	return function(ctx, call)
}

// EffectTargetRequest carries the provider-owned target and validated call.
// No target field exists in the wire call outside the declared arguments.
type EffectTargetRequest struct {
	Declaration EffectDeclaration
	Call        EffectCall
}

// EffectTargetFence validates that arguments remain inside the declaration's
// fixed target immediately before the call enters idempotent dispatch.
type EffectTargetFence interface {
	Admit(context.Context, EffectTargetRequest) error
}

type EffectTargetFenceFunc func(context.Context, EffectTargetRequest) error

func (function EffectTargetFenceFunc) Admit(ctx context.Context, request EffectTargetRequest) error {
	return function(ctx, request)
}

// ExactArgumentTargetFence creates the common fixed-target fence. The named
// JSON argument must be a string exactly equal to the immutable declaration
// target. It is deliberately strict rather than accepting aliases.
func ExactArgumentTargetFence(field string) (EffectTargetFence, error) {
	if !validEffectName(field) {
		return nil, fmt.Errorf("effect target field %q is not canonical", field)
	}
	return EffectTargetFenceFunc(func(_ context.Context, request EffectTargetRequest) error {
		var arguments map[string]json.RawMessage
		if err := json.Unmarshal(request.Call.Arguments, &arguments); err != nil {
			return errors.New("target arguments are not an object")
		}
		encoded, found := arguments[field]
		if !found {
			return fmt.Errorf("target argument %q is required", field)
		}
		var target string
		if err := json.Unmarshal(encoded, &target); err != nil || target != request.Declaration.Target {
			return fmt.Errorf("target argument %q is outside the declared target", field)
		}
		return nil
	}), nil
}

// EffectTool binds one immutable declaration to an implementation and an
// exact permission resource. PermissionOperation defaults to "execute".
// A non-empty target requires an explicit fence.
type EffectTool struct {
	Declaration         EffectDeclaration
	Executor            EffectExecutor
	TargetFence         EffectTargetFence
	PermissionResource  string
	PermissionOperation string
}

// EffectAuthorityRequest is passed only to the deployment-supplied verifier.
// Evidence is opaque and bounded; it is never echoed, logged, retained in an
// audit record, or handed to the executor.
type EffectAuthorityRequest struct {
	ScopeID           string
	SessionID         string
	CallID            string
	Name              string
	ArgumentsDigest   string
	DeclarationDigest string
	Target            string
	Evidence          string
}

// EffectAuthorityDecision is the verifier's exact receipt. Every field is
// compared with the host-owned admission. A broad boolean is intentionally
// insufficient: an approval for one declaration or target cannot bless
// another call.
type EffectAuthorityDecision struct {
	SessionID         string
	CallID            string
	Name              string
	ArgumentsDigest   string
	DeclarationDigest string
	Target            string
}

// EffectAuthority validates a server/deployment authority receipt. Effects
// cannot mount without an exact provider service; the shipped deny provider
// is the fail-closed choice, including for non-mutating tools.
type EffectAuthority interface {
	Authorize(context.Context, EffectAuthorityRequest) (EffectAuthorityDecision, error)
}

type EffectAuthorityFunc func(context.Context, EffectAuthorityRequest) (EffectAuthorityDecision, error)

func (function EffectAuthorityFunc) Authorize(
	ctx context.Context, request EffectAuthorityRequest,
) (EffectAuthorityDecision, error) {
	return function(ctx, request)
}

type denyEffectAuthority struct{}

func (denyEffectAuthority) Authorize(
	context.Context, EffectAuthorityRequest,
) (EffectAuthorityDecision, error) {
	return EffectAuthorityDecision{}, errors.New("no effect authority verifier is configured")
}

// DenyEffectAuthorityFactory is the independently composable fail-closed
// authority provider. Development and production profiles may replace it with
// a verifier for server-minted receipts without replacing the effects route,
// declarations, stores, or client adapter.
type DenyEffectAuthorityFactory struct{ descriptor plugin.Descriptor }

func NewDenyEffectAuthorityFactory() *DenyEffectAuthorityFactory {
	descriptor, err := (plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.deny-effect-authority", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:  []plugin.Contract{presentation.EffectAuthorityContract},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}).Canonical()
	if err != nil {
		panic(err)
	}
	return &DenyEffectAuthorityFactory{descriptor: descriptor}
}

func (factory *DenyEffectAuthorityFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *DenyEffectAuthorityFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return (denyEffectAuthorityCandidate{}).Activate(ctx, mount)
}

func (factory *DenyEffectAuthorityFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return denyEffectAuthorityCandidate{}, nil
}

type denyEffectAuthorityCandidate struct{}

func (denyEffectAuthorityCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return mount.Publisher.Provide(presentation.EffectAuthorityContract, EffectAuthority(denyEffectAuthority{}))
}

func lookupEffectAuthority(services pluginruntime.Services) (EffectAuthority, error) {
	value, contract, _, _, found := services.Lookup(presentation.EffectAuthorityContract.Name)
	if !found || contract != presentation.EffectAuthorityContract {
		return nil, errors.New("presentation effect authority is unavailable")
	}
	authority, ok := value.(EffectAuthority)
	if !ok || authority == nil {
		return nil, errors.New("presentation effect authority has the wrong Go type")
	}
	return authority, nil
}

// EffectPolicy may admit a declaration whose confirmation mode is "policy".
// Nil is deliberately equivalent to no admission, so the client is asked.
type EffectPolicy func(EffectTargetRequest) bool

// EffectsOptions are trusted implementation bindings, not client- or
// profile-supplied JSON values.
type EffectsOptions struct {
	Policy EffectPolicy
	Tools  []EffectTool
}

// EffectAuditRecord is a payload-free terminal record. It names the exact
// declaration and target but retains neither arguments, result content,
// authority evidence, nor confirmation nonce.
type EffectAuditRecord struct {
	ScopeID           string               `json:"scope_id"`
	SessionID         string               `json:"session_id,omitempty"`
	CallID            string               `json:"call_id,omitempty"`
	Name              string               `json:"name,omitempty"`
	DeclarationDigest string               `json:"declaration_digest,omitempty"`
	Target            string               `json:"target,omitempty"`
	Confirmation      legacyaction.Confirm `json:"confirmation,omitempty"`
	Authorized        bool                 `json:"authorized"`
	Confirmed         bool                 `json:"confirmed"`
	Crossed           bool                 `json:"crossed"`
	Executed          bool                 `json:"executed"`
	Replayed          bool                 `json:"replayed"`
	ErrorCode         string               `json:"error_code,omitempty"`
	StartedAt         time.Time            `json:"started_at"`
	FinishedAt        time.Time            `json:"finished_at"`
}

// EffectsStats is bounded operational evidence for the mounted provider.
type EffectsStats struct {
	ActiveSessions uint64 `json:"active_sessions"`
	InFlight       uint64 `json:"in_flight"`
	Calls          uint64 `json:"calls"`
	Results        uint64 `json:"results"`
	Authorized     uint64 `json:"authorized"`
	Executed       uint64 `json:"executed"`
	Replayed       uint64 `json:"replayed"`
	Refused        uint64 `json:"refused"`
	Closed         bool   `json:"closed"`
}

// Effects is the read-only host service. Execution remains reachable only
// through the scoped route so another plugin cannot bypass admission by
// obtaining the service interface.
type Effects interface {
	Declarations() []EffectDeclaration
	Stats() EffectsStats
	Audit() []EffectAuditRecord
}

type effectConfig struct {
	MaxSessions     int    `json:"max_sessions,omitempty"`
	MaxMessageBytes int64  `json:"max_message_bytes,omitempty"`
	MaxResultBytes  int64  `json:"max_result_bytes,omitempty"`
	MaxInFlight     int    `json:"max_in_flight,omitempty"`
	MaxCalls        int    `json:"max_calls,omitempty"`
	MaxAuditRecords int    `json:"max_audit_records,omitempty"`
	ConfirmationMS  uint64 `json:"confirmation_timeout_ms,omitempty"`
	ExecutionMS     uint64 `json:"execution_timeout_ms,omitempty"`
}

type effectLimits struct {
	MaxSessions     int
	MaxMessageBytes int64
	MaxResultBytes  int64
	MaxInFlight     int
	MaxCalls        int
	MaxAuditRecords int
	Confirmation    time.Duration
	Execution       time.Duration
}

func parseEffectConfig(raw []byte) (effectLimits, error) {
	if err := strictjson.Validate(raw); err != nil {
		return effectLimits{}, fmt.Errorf("presentation effects config: %w", err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return effectLimits{}, errors.New("presentation effects config must be one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config effectConfig
	if err := decoder.Decode(&config); err != nil {
		return effectLimits{}, fmt.Errorf("presentation effects config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return effectLimits{}, errors.New("presentation effects config has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return effectLimits{}, fmt.Errorf("presentation effects config trailing data: %w", err)
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultMaxEffectSessions
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = defaultMaxEffectMessageBytes
	}
	if config.MaxResultBytes == 0 {
		config.MaxResultBytes = defaultMaxEffectResultBytes
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = defaultMaxEffectInFlight
	}
	if config.MaxCalls == 0 {
		config.MaxCalls = defaultMaxEffectCalls
	}
	if config.MaxAuditRecords == 0 {
		config.MaxAuditRecords = defaultMaxEffectAuditRecords
	}
	if config.ConfirmationMS == 0 {
		config.ConfirmationMS = defaultEffectConfirmationMS
	}
	if config.ExecutionMS == 0 {
		config.ExecutionMS = defaultEffectExecutionMS
	}
	if config.MaxSessions < 1 || config.MaxSessions > maximumEffectSessions {
		return effectLimits{}, fmt.Errorf("max_sessions must be in [1,%d]", maximumEffectSessions)
	}
	if config.MaxMessageBytes < minimumEffectMessageBytes || config.MaxMessageBytes > maximumEffectMessageBytes {
		return effectLimits{}, fmt.Errorf("max_message_bytes must be in [%d,%d]",
			minimumEffectMessageBytes, maximumEffectMessageBytes)
	}
	if config.MaxResultBytes < 1 || config.MaxResultBytes > maximumEffectResultBytes ||
		config.MaxResultBytes >= config.MaxMessageBytes {
		return effectLimits{}, errors.New("max_result_bytes must be positive, below max_message_bytes, and within its hard ceiling")
	}
	if config.MaxInFlight < 1 || config.MaxInFlight > maximumEffectInFlight {
		return effectLimits{}, fmt.Errorf("max_in_flight must be in [1,%d]", maximumEffectInFlight)
	}
	if config.MaxCalls < config.MaxInFlight || config.MaxCalls > maximumEffectCalls {
		return effectLimits{}, fmt.Errorf("max_calls must be in [max_in_flight,%d]", maximumEffectCalls)
	}
	if int64(config.MaxCalls) > maximumEffectTerminalBytes/config.MaxResultBytes {
		return effectLimits{}, fmt.Errorf("max_calls times max_result_bytes must not exceed %d",
			maximumEffectTerminalBytes)
	}
	if config.MaxAuditRecords < 1 || config.MaxAuditRecords > maximumEffectAuditRecords {
		return effectLimits{}, fmt.Errorf("max_audit_records must be in [1,%d]", maximumEffectAuditRecords)
	}
	for name, value := range map[string]uint64{
		"confirmation_timeout_ms": config.ConfirmationMS,
		"execution_timeout_ms":    config.ExecutionMS,
	} {
		if value < minimumEffectTimeoutMS || value > maximumEffectTimeoutMS {
			return effectLimits{}, fmt.Errorf("%s must be in [%d,%d]", name,
				minimumEffectTimeoutMS, maximumEffectTimeoutMS)
		}
	}
	return effectLimits{
		MaxSessions: config.MaxSessions, MaxMessageBytes: config.MaxMessageBytes,
		MaxResultBytes: config.MaxResultBytes, MaxInFlight: config.MaxInFlight,
		MaxCalls: config.MaxCalls, MaxAuditRecords: config.MaxAuditRecords,
		Confirmation: time.Duration(config.ConfirmationMS) * time.Millisecond,
		Execution:    time.Duration(config.ExecutionMS) * time.Millisecond,
	}, nil
}

type effectSchemaLoader struct{}

func (effectSchemaLoader) Load(resource string) (any, error) {
	return nil, fmt.Errorf("external effect schema resolution is disabled for %s", resource)
}

type compiledEffectTool struct {
	declaration EffectDeclaration
	schema      *jsonschema.Schema
	executor    EffectExecutor
	fence       EffectTargetFence
	resource    string
	operation   string
	builtin     string
}

func compileEffectTool(tool EffectTool) (compiledEffectTool, error) {
	declaration := tool.Declaration.clone()
	if !validEffectName(declaration.Name) {
		return compiledEffectTool{}, fmt.Errorf("effect tool name %q is not canonical", declaration.Name)
	}
	if declaration.Description == "" || declaration.Description != strings.TrimSpace(declaration.Description) ||
		!utf8.ValidString(declaration.Description) || len(declaration.Description) > maximumEffectDescriptionBytes {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s has an invalid bounded description", declaration.Name)
	}
	confirm, err := legacyaction.ParseConfirm(string(declaration.Confirm))
	if err != nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s: %w", declaration.Name, err)
	}
	declaration.Confirm = confirm
	if declaration.SessionConfirm != "" && declaration.SessionConfirm != legacyaction.ConfirmNever {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s cannot delegate host confirmation to the remote session", declaration.Name)
	}
	declaration.SessionConfirm = legacyaction.ConfirmNever
	switch declaration.Channel {
	case EffectChannelTool, EffectChannelComputer, EffectChannelArtifact, EffectChannelDownload:
	default:
		return compiledEffectTool{}, fmt.Errorf("effect tool %s has invalid channel %q", declaration.Name, declaration.Channel)
	}
	if err := validateEffectTarget(declaration.Target); err != nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s: %w", declaration.Name, err)
	}
	if declaration.Target != "" && tool.TargetFence == nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s declares target %q without a target fence",
			declaration.Name, declaration.Target)
	}
	if declaration.Target == "" && tool.TargetFence != nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s supplies a fence without a declared target", declaration.Name)
	}
	if tool.Executor == nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s has no executor", declaration.Name)
	}
	canonical, schema, err := compileEffectSchema(declaration.Name, declaration.Parameters)
	if err != nil {
		return compiledEffectTool{}, err
	}
	declaration.Parameters = canonical
	resource := tool.PermissionResource
	if resource == "" {
		resource = "tool/" + declaration.Name
	}
	operation := tool.PermissionOperation
	if operation == "" {
		operation = effectOperation
	}
	declaration.Digest, err = effectDeclarationDigest(declaration, resource, operation)
	if err != nil {
		return compiledEffectTool{}, err
	}
	if resource != strings.TrimSpace(resource) || resource == "" || len(resource) > 1024 {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s has invalid permission resource", declaration.Name)
	}
	permissionProbe := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion, Name: "openrealtime.presentation.host.permission-probe",
		Revision: 1, Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Permissions: []plugin.Permission{{Kind: effectPermissionKind, Resource: resource, Operations: []string{operation}}},
	}
	if err := permissionProbe.Validate(); err != nil {
		return compiledEffectTool{}, fmt.Errorf("effect tool %s permission: %w", declaration.Name, err)
	}
	return compiledEffectTool{
		declaration: declaration, schema: schema, executor: tool.Executor, fence: tool.TargetFence,
		resource: resource, operation: operation,
	}, nil
}

func compileEffectSchema(name string, source []byte) (json.RawMessage, *jsonschema.Schema, error) {
	if len(source) == 0 || len(source) > maximumEffectParametersBytes {
		return nil, nil, fmt.Errorf("effect tool %s parameters must be 1-%d bytes",
			name, maximumEffectParametersBytes)
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, nil, fmt.Errorf("effect tool %s parameters: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, fmt.Errorf("effect tool %s parameters: %w", name, err)
	}
	root, ok := document.(map[string]any)
	if !ok || root["type"] != "object" || root["additionalProperties"] != false {
		return nil, nil, fmt.Errorf("effect tool %s parameters must be a closed object schema", name)
	}
	if containsSchemaReference(document) {
		return nil, nil, fmt.Errorf("effect tool %s parameters cannot reference an external or hidden schema", name)
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("effect tool %s parameters: %w", name, err)
	}
	id := "urn:openrealtime:presentation:effect:" + name
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(effectSchemaLoader{})
	if err := compiler.AddResource(id, document); err != nil {
		return nil, nil, fmt.Errorf("effect tool %s parameters: %w", name, err)
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		return nil, nil, fmt.Errorf("effect tool %s parameters: %w", name, err)
	}
	return json.RawMessage(canonical), compiled, nil
}

func containsSchemaReference(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" || key == "$dynamicRef" || containsSchemaReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsSchemaReference(child) {
				return true
			}
		}
	}
	return false
}

func effectDeclarationDigest(
	declaration EffectDeclaration, resource, operation string,
) (string, error) {
	declaration.Digest = ""
	payload, err := json.Marshal(struct {
		Declaration EffectDeclaration `json:"declaration"`
		Resource    string            `json:"permission_resource"`
		Operation   string            `json:"permission_operation"`
	}{declaration, resource, operation})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validEffectName(value string) bool {
	if value == "" || len(value) > maximumEffectNameBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(index > 0 && character >= '0' && character <= '9') ||
			(index > 0 && (character == '_' || character == '-' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func validateEffectTarget(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) || len(value) > maximumEffectTargetBytes || !utf8.ValidString(value) {
		return errors.New("effect target is not canonical or exceeds its bound")
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return errors.New("effect target contains control or whitespace characters")
		}
	}
	return nil
}

func canonicalEffectArguments(schema *jsonschema.Schema, source []byte) (json.RawMessage, string, error) {
	canonical, digest, err := effectauthority.CanonicalEffectArguments(source)
	if err != nil {
		return nil, "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	if err := schema.Validate(value); err != nil {
		return nil, "", err
	}
	return canonical, digest, nil
}

var cachedBuiltinEffectTools = sync.OnceValues(compileBuiltinEffectTools)

func builtinEffectTools() ([]compiledEffectTool, error) {
	tools, err := cachedBuiltinEffectTools()
	if err != nil {
		return nil, err
	}
	result := make([]compiledEffectTool, len(tools))
	copy(result, tools)
	for index := range result {
		result[index].declaration = result[index].declaration.clone()
	}
	return result, nil
}

func compileBuiltinEffectTools() ([]compiledEffectTool, error) {
	artifactSchema := fmt.Sprintf(`{
		"type":"object",
		"properties":{
			"artifact_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},
			"title":{"type":"string","maxLength":%d},
			"html":{"type":"string","minLength":1,"maxLength":%d}
		},
		"required":["artifact_id","title","html"],"additionalProperties":false
	}`, maxArtifactTitleBytes, maximumArtifactBytes)
	downloadBase64Length := ((maximumDownloadBytes + 2) / 3) * 4
	downloadSchema := fmt.Sprintf(`{
		"type":"object",
		"properties":{
			"artifact_id":{"type":"string","pattern":"^[A-Za-z0-9_-]{1,64}$"},
			"filename":{"type":"string","minLength":1,"maxLength":%d},
			"media_type":{"type":"string","minLength":1,"maxLength":%d},
			"text":{"type":"string","minLength":1,"maxLength":%d},
			"base64":{"type":"string","minLength":1,"maxLength":%d}
		},
		"required":["artifact_id","filename","media_type"],
		"oneOf":[{"required":["text"]},{"required":["base64"]}],
		"additionalProperties":false
	}`, maxDownloadFilenameBytes, maxMediaTypeBytes, maximumDownloadBytes, downloadBase64Length)
	templates := []struct {
		tool    EffectTool
		builtin string
	}{
		{tool: EffectTool{
			Declaration: EffectDeclaration{
				Name: "display_artifact", Description: "Publish sandboxed HTML for a person to view; reusing the stable artifact_id revises it.",
				Parameters: json.RawMessage(artifactSchema), Confirm: legacyaction.ConfirmNever,
				Channel: EffectChannelArtifact,
			},
			Executor: EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
				return EffectResult{}, errors.New("artifact executor was not mounted")
			}),
			PermissionResource: artifactEffectResource,
		}, builtin: "artifact"},
		{tool: EffectTool{
			Declaration: EffectDeclaration{
				Name: "publish_download", Description: "Publish a generated text or base64 file as an attachment; reusing artifact_id revises it.",
				Parameters: json.RawMessage(downloadSchema), Confirm: legacyaction.ConfirmNever,
				Channel: EffectChannelDownload,
			},
			Executor: EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
				return EffectResult{}, errors.New("download executor was not mounted")
			}),
			PermissionResource: downloadEffectResource,
		}, builtin: "download"},
	}
	result := make([]compiledEffectTool, 0, len(templates))
	for _, template := range templates {
		compiled, err := compileEffectTool(template.tool)
		if err != nil {
			return nil, err
		}
		compiled.builtin = template.builtin
		result = append(result, compiled)
	}
	return result, nil
}

// EffectsFactory mounts the exact effect catalog, bounded socket, and
// read-only discovery service. Its descriptor contains a digest of the
// declaration catalog and one permission ceiling for every effect resource.
type EffectsFactory struct {
	descriptor    plugin.Descriptor
	tools         []compiledEffectTool
	policy        EffectPolicy
	catalog       []byte
	catalogDigest string
}

var defaultEffectsCatalogDigest = sync.OnceValues(func() (string, error) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		return "", err
	}
	return factory.CatalogDigest(), nil
})

// DefaultEffectsCatalogDigest returns the immutable catalog identity used by
// the shipped effect profile. Client profiles pin this value; profiles with
// custom tools instead use the digest from their exact EffectsFactory.
func DefaultEffectsCatalogDigest() (string, error) { return defaultEffectsCatalogDigest() }

func NewEffectsFactory(options EffectsOptions) (*EffectsFactory, error) {
	builtins, err := builtinEffectTools()
	if err != nil {
		return nil, err
	}
	if len(options.Tools)+len(builtins) > maximumEffectTools {
		return nil, fmt.Errorf("presentation effects declare more than %d tools", maximumEffectTools)
	}
	tools := append([]compiledEffectTool(nil), builtins...)
	seen := make(map[string]struct{}, len(tools)+len(options.Tools))
	for _, tool := range tools {
		seen[tool.declaration.Name] = struct{}{}
	}
	for _, source := range options.Tools {
		compiled, compileErr := compileEffectTool(source)
		if compileErr != nil {
			return nil, compileErr
		}
		if _, duplicate := seen[compiled.declaration.Name]; duplicate {
			return nil, fmt.Errorf("presentation effects repeat tool %q", compiled.declaration.Name)
		}
		seen[compiled.declaration.Name] = struct{}{}
		tools = append(tools, compiled)
	}
	declarations := make([]EffectDeclaration, len(tools))
	for index := range tools {
		declarations[index] = tools[index].declaration.clone()
	}
	catalog, err := json.Marshal(struct {
		FormatVersion uint64              `json:"format_version"`
		Tools         []EffectDeclaration `json:"tools"`
	}{1, declarations})
	if err != nil {
		return nil, err
	}
	permissionsByResource := make(map[string][]string)
	for _, tool := range tools {
		if !slices.Contains(permissionsByResource[tool.resource], tool.operation) {
			permissionsByResource[tool.resource] = append(permissionsByResource[tool.resource], tool.operation)
		}
	}
	permissions := make([]plugin.Permission, 0, len(permissionsByResource))
	for resource, operations := range permissionsByResource {
		permissions = append(permissions, plugin.Permission{
			Kind: effectPermissionKind, Resource: resource, Operations: operations,
		})
	}
	schema := presentation.EffectsConfigContract
	stateSchema := presentation.EffectsStateContract
	descriptor, err := (plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.effects", Revision: 2,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.EffectsContract},
		Requires: []plugin.Requirement{
			{Contract: presentation.HTTPRoutesContract},
			{Contract: presentation.ArtifactStoreContract},
			{Contract: presentation.DownloadStoreContract},
			{Contract: presentation.EffectAuthorityContract},
		},
		ConfigSchema: &schema, StateSchema: &stateSchema, Permissions: permissions,
		Assets: []plugin.Asset{{
			Name: "effects/declarations.v1.json", MediaType: "application/json", Digest: contentDigest(catalog),
		}},
		Lifecycle: plugin.Lifecycle{Snapshot: true, Restore: true, DisposeTimeoutMS: 5_000},
	}).Canonical()
	if err != nil {
		return nil, fmt.Errorf("presentation effects descriptor: %w", err)
	}
	return &EffectsFactory{
		descriptor: descriptor, tools: tools, policy: options.Policy, catalog: slices.Clone(catalog),
		catalogDigest: contentDigest(catalog),
	}, nil
}

func (factory *EffectsFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *EffectsFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseEffectConfig(raw)
	return err
}

func (factory *EffectsFactory) DeclarationsDocument() []byte { return slices.Clone(factory.catalog) }

func (factory *EffectsFactory) CatalogDigest() string { return factory.catalogDigest }

func (factory *EffectsFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	candidate, err := factory.prepareCandidate(
		mount.EntryID, mount.Config, mount.Services, mount.Permissions,
	)
	if err != nil {
		return err
	}
	return candidate.Activate(ctx, mount)
}

func (factory *EffectsFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return factory.prepareCandidate(
		candidate.EntryID, candidate.Config, candidate.Services, candidate.Permissions,
	)
}

func (factory *EffectsFactory) prepareCandidate(
	entryID string,
	config json.RawMessage,
	services pluginruntime.Services,
	permissions pluginruntime.Permissions,
) (effectsCandidate, error) {
	limits, err := parseEffectConfig(config)
	if err != nil {
		return effectsCandidate{}, err
	}
	if _, err := lookupRoutes(services); err != nil {
		return effectsCandidate{}, err
	}
	if _, err := lookupArtifactStore(services); err != nil {
		return effectsCandidate{}, err
	}
	if _, err := lookupDownloadStore(services); err != nil {
		return effectsCandidate{}, err
	}
	if _, err := lookupEffectAuthority(services); err != nil {
		return effectsCandidate{}, err
	}
	tools := cloneCompiledEffectTools(factory.tools)
	for index := range tools {
		if !permissions.Allows(effectPermissionKind, tools[index].resource, tools[index].operation) {
			return effectsCandidate{}, fmt.Errorf("presentation effects lack deployment grant %s/%s/%s",
				effectPermissionKind, tools[index].resource, tools[index].operation)
		}
	}
	return effectsCandidate{
		entryID: entryID, limits: limits, tools: tools, policy: factory.policy,
		catalogDigest: factory.catalogDigest,
	}, nil
}

func cloneCompiledEffectTools(source []compiledEffectTool) []compiledEffectTool {
	result := make([]compiledEffectTool, len(source))
	copy(result, source)
	for index := range result {
		result[index].declaration = result[index].declaration.clone()
	}
	return result
}

type effectsCandidate struct {
	entryID       string
	limits        effectLimits
	tools         []compiledEffectTool
	policy        EffectPolicy
	catalogDigest string
}

func (candidate effectsCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	if mount.EntryID != candidate.entryID {
		return errors.New("effects candidate entry changed before activation")
	}
	limits, err := parseEffectConfig(mount.Config)
	if err != nil {
		return err
	}
	if limits != candidate.limits {
		return errors.New("effects candidate limits changed before activation")
	}
	if _, err := lookupRoutes(mount.Services); err != nil {
		return err
	}
	artifacts, err := lookupArtifactStore(mount.Services)
	if err != nil {
		return err
	}
	downloads, err := lookupDownloadStore(mount.Services)
	if err != nil {
		return err
	}
	authority, err := lookupEffectAuthority(mount.Services)
	if err != nil {
		return err
	}
	tools := cloneCompiledEffectTools(candidate.tools)
	for index := range tools {
		if !mount.Permissions.Allows(effectPermissionKind, tools[index].resource, tools[index].operation) {
			return fmt.Errorf("presentation effects lack deployment grant %s/%s/%s",
				effectPermissionKind, tools[index].resource, tools[index].operation)
		}
		switch tools[index].builtin {
		case "artifact":
			tools[index].executor = artifactEffectExecutor{store: artifacts}
		case "download":
			tools[index].executor = downloadEffectExecutor{store: downloads}
		}
	}
	if mount.State == nil {
		return errors.New("presentation effects state lifecycle is unavailable")
	}
	restored, available, err := mount.State.Restored()
	if err != nil {
		return err
	}
	hub, err := newEffectsHub(
		ctx, limits, tools, authority, candidate.policy, candidate.catalogDigest,
	)
	if err != nil {
		return err
	}
	hub.setPermissions(mount.Permissions)
	hub.artifacts = artifacts
	hub.downloads = downloads
	if err := mount.Lifecycle.Defer("effect-sessions", hub.close); err != nil {
		return err
	}
	if available {
		state, err := decodeEffectsHubState(restored, limits, tools, candidate.catalogDigest)
		if err != nil {
			return err
		}
		if err := hub.restoreState(state); err != nil {
			return err
		}
	}
	if err := mount.State.Quiesce(hub.quiesceState); err != nil {
		return err
	}
	if err := mount.State.Snapshot(hub.snapshotState); err != nil {
		return err
	}
	if err := registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/effects", Handler: http.HandlerFunc(hub.serveHTTP),
	}}); err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.EffectsContract, Effects(hub))
}

func lookupEffects(services pluginruntime.Services) (Effects, error) {
	value, contract, _, _, found := services.Lookup(presentation.EffectsContract.Name)
	if !found || contract != presentation.EffectsContract {
		return nil, errors.New("presentation effects are unavailable")
	}
	effects, ok := value.(Effects)
	if !ok || effects == nil {
		return nil, errors.New("presentation effects have the wrong Go type")
	}
	return effects, nil
}

type artifactEffectExecutor struct{ store ArtifactStore }

func (executor artifactEffectExecutor) Execute(ctx context.Context, call EffectCall) (EffectResult, error) {
	var arguments struct {
		ArtifactID string `json:"artifact_id"`
		Title      string `json:"title"`
		HTML       string `json:"html"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return EffectResult{}, fmt.Errorf("decode display_artifact arguments: %w", err)
	}
	artifact, err := executor.store.Publish(ctx, ArtifactInput{
		ID: arguments.ArtifactID, Title: arguments.Title, HTML: arguments.HTML,
	})
	if err != nil {
		return EffectResult{}, err
	}
	output, err := json.Marshal(struct {
		ArtifactID string `json:"artifact_id"`
		Version    uint64 `json:"version"`
		Status     string `json:"status"`
	}{artifact.ID, artifact.Version, "displayed"})
	if err != nil {
		return EffectResult{}, err
	}
	return EffectResult{Output: string(output), Artifact: &artifact}, nil
}

type downloadEffectExecutor struct{ store DownloadStore }

func (executor downloadEffectExecutor) Execute(ctx context.Context, call EffectCall) (EffectResult, error) {
	var arguments struct {
		ArtifactID string  `json:"artifact_id"`
		Filename   string  `json:"filename"`
		MediaType  string  `json:"media_type"`
		Text       *string `json:"text"`
		Base64     *string `json:"base64"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return EffectResult{}, fmt.Errorf("decode publish_download arguments: %w", err)
	}
	var content []byte
	if arguments.Text != nil {
		content = []byte(*arguments.Text)
	} else if arguments.Base64 != nil {
		var err error
		content, err = base64.StdEncoding.Strict().DecodeString(*arguments.Base64)
		if err != nil {
			return EffectResult{}, errors.New("download base64 is invalid")
		}
	} else {
		return EffectResult{}, errors.New("download requires exactly one content encoding")
	}
	defer wipe(content)
	download, err := executor.store.Publish(ctx, DownloadInput{
		ID: arguments.ArtifactID, Filename: arguments.Filename,
		MediaType: arguments.MediaType, Content: content,
	})
	if err != nil {
		return EffectResult{}, err
	}
	output, err := json.Marshal(struct {
		ArtifactID string `json:"artifact_id"`
		Filename   string `json:"filename"`
		Bytes      int64  `json:"bytes"`
		Version    uint64 `json:"version"`
		Status     string `json:"status"`
	}{download.ID, download.Filename, download.Bytes, download.Version, "available"})
	if err != nil {
		return EffectResult{}, err
	}
	return EffectResult{Output: string(output), Download: &download}, nil
}

type effectWireLimits struct {
	MaxMessageBytes int64  `json:"max_message_bytes"`
	MaxResultBytes  int64  `json:"max_result_bytes"`
	MaxInFlight     int    `json:"max_in_flight"`
	MaxCalls        int    `json:"max_calls"`
	ConfirmationMS  uint64 `json:"confirmation_timeout_ms"`
	ExecutionMS     uint64 `json:"execution_timeout_ms"`
}

type effectWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type effectServerMessage struct {
	Type          string               `json:"type"`
	Version       uint64               `json:"version,omitempty"`
	ScopeID       string               `json:"scope_id,omitempty"`
	CatalogDigest string               `json:"catalog_digest,omitempty"`
	ID            string               `json:"id,omitempty"`
	Name          string               `json:"name,omitempty"`
	Arguments     json.RawMessage      `json:"arguments,omitempty"`
	Nonce         string               `json:"nonce,omitempty"`
	Confirm       legacyaction.Confirm `json:"confirm,omitempty"`
	Target        string               `json:"target,omitempty"`
	Channel       EffectChannel        `json:"channel,omitempty"`
	Output        string               `json:"output,omitempty"`
	Error         *effectWireError     `json:"error,omitempty"`
	Artifact      *Artifact            `json:"artifact,omitempty"`
	Download      *Download            `json:"download,omitempty"`
	Tools         []EffectDeclaration  `json:"tools,omitempty"`
	Limits        *effectWireLimits    `json:"limits,omitempty"`
}

type effectClientEnvelope struct {
	Type string `json:"type"`
}

type effectClientCall struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Authority string          `json:"authority"`
}

type effectClientDecision struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Nonce    string `json:"nonce"`
	Approved *bool  `json:"approved"`
}

type effectsHub struct {
	parent        context.Context
	limits        effectLimits
	tools         map[string]*compiledEffectTool
	declarations  []EffectDeclaration
	authority     EffectAuthority
	policy        EffectPolicy
	permissions   pluginruntime.Permissions
	artifacts     ArtifactStore
	downloads     DownloadStore
	catalogDigest string
	readyBytes    int

	mu               sync.Mutex
	closed           bool
	quiescing        bool
	sessions         map[string]*effectSession
	calls            map[string]*effectCallState
	drained          chan struct{}
	mutationsDrained chan struct{}
	stats            EffectsStats
	audit            []EffectAuditRecord
}

func newEffectsHub(
	parent context.Context,
	limits effectLimits,
	tools []compiledEffectTool,
	authority EffectAuthority,
	policy EffectPolicy,
	catalogDigest string,
) (*effectsHub, error) {
	if parent == nil {
		return nil, errors.New("presentation effects require a lifecycle context")
	}
	hub := &effectsHub{
		parent: parent, limits: limits, tools: make(map[string]*compiledEffectTool, len(tools)),
		authority: authority, policy: policy, sessions: make(map[string]*effectSession),
		calls: make(map[string]*effectCallState), drained: make(chan struct{}),
		catalogDigest: catalogDigest,
	}
	for index := range tools {
		tool := tools[index]
		hub.tools[tool.declaration.Name] = &tool
		hub.declarations = append(hub.declarations, tool.declaration.clone())
	}
	probe := effectServerMessage{
		Type: "ready", Version: 1, ScopeID: strings.Repeat("0", 32),
		CatalogDigest: hub.catalogDigest, Tools: hub.Declarations(),
		Limits: &effectWireLimits{
			MaxMessageBytes: limits.MaxMessageBytes, MaxResultBytes: limits.MaxResultBytes,
			MaxInFlight: limits.MaxInFlight, MaxCalls: limits.MaxCalls,
			ConfirmationMS: uint64(limits.Confirmation / time.Millisecond),
			ExecutionMS:    uint64(limits.Execution / time.Millisecond),
		},
	}
	encoded, err := json.Marshal(probe)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > limits.MaxMessageBytes {
		return nil, fmt.Errorf("effect declaration catalog has %d wire bytes; max_message_bytes is %d",
			len(encoded), limits.MaxMessageBytes)
	}
	hub.readyBytes = len(encoded)
	return hub, nil
}

func (hub *effectsHub) Declarations() []EffectDeclaration {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	result := make([]EffectDeclaration, len(hub.declarations))
	for index := range hub.declarations {
		result[index] = hub.declarations[index].clone()
	}
	return result
}

func (hub *effectsHub) Stats() EffectsStats {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	result := hub.stats
	result.Closed = hub.closed
	return result
}

func (hub *effectsHub) Audit() []EffectAuditRecord {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return slices.Clone(hub.audit)
}

func (hub *effectsHub) setPermissions(permissions pluginruntime.Permissions) {
	hub.mu.Lock()
	hub.permissions = permissions
	hub.mu.Unlock()
}

func (hub *effectsHub) addSession(session *effectSession) bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed || hub.quiescing || len(hub.sessions) >= hub.limits.MaxSessions {
		return false
	}
	hub.sessions[session.scopeID] = session
	hub.stats.ActiveSessions++
	return true
}

func (hub *effectsHub) removeSession(scopeID string) {
	hub.mu.Lock()
	if _, found := hub.sessions[scopeID]; found {
		delete(hub.sessions, scopeID)
		if hub.stats.ActiveSessions > 0 {
			hub.stats.ActiveSessions--
		}
	}
	if hub.closed && len(hub.sessions) == 0 {
		select {
		case <-hub.drained:
		default:
			close(hub.drained)
		}
	}
	hub.mu.Unlock()
}

func (hub *effectsHub) callStarted() bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed || hub.quiescing {
		return false
	}
	incrementEffectCounter(&hub.stats.Calls)
	hub.stats.InFlight++
	return true
}

func (hub *effectsHub) callReleased() {
	hub.mu.Lock()
	if hub.stats.InFlight > 0 {
		hub.stats.InFlight--
	}
	if hub.quiescing && hub.stats.InFlight == 0 && hub.mutationsDrained != nil {
		close(hub.mutationsDrained)
		hub.mutationsDrained = nil
	}
	hub.mu.Unlock()
}

func (hub *effectsHub) open() bool {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return !hub.closed
}

func (hub *effectsHub) record(record EffectAuditRecord) {
	hub.mu.Lock()
	incrementEffectCounter(&hub.stats.Results)
	if record.Authorized {
		incrementEffectCounter(&hub.stats.Authorized)
	}
	if record.Executed {
		incrementEffectCounter(&hub.stats.Executed)
	}
	if record.Replayed {
		incrementEffectCounter(&hub.stats.Replayed)
	}
	if record.ErrorCode != "" {
		incrementEffectCounter(&hub.stats.Refused)
	}
	if len(hub.audit) == hub.limits.MaxAuditRecords {
		copy(hub.audit, hub.audit[1:])
		hub.audit[len(hub.audit)-1] = record
	} else {
		hub.audit = append(hub.audit, record)
	}
	hub.mu.Unlock()
}

func (hub *effectsHub) close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close presentation effects: nil context")
	}
	hub.mu.Lock()
	if !hub.closed {
		hub.closed = true
		hub.quiescing = false
		hub.mutationsDrained = nil
		hub.stats.Closed = true
		if len(hub.sessions) == 0 {
			close(hub.drained)
		}
	}
	sessions := make([]*effectSession, 0, len(hub.sessions))
	for _, session := range hub.sessions {
		sessions = append(sessions, session)
	}
	drained := hub.drained
	hub.mu.Unlock()
	for _, session := range sessions {
		session.stop(errors.New("effect provider unmounted"))
	}
	select {
	case <-drained:
		hub.mu.Lock()
		hub.calls = nil
		hub.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("close presentation effects: %w", ctx.Err())
	}
}

func (hub *effectsHub) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if !loopbackRemote(request.RemoteAddr) {
		http.Error(writer, "the local effect channel is loopback-only", http.StatusForbidden)
		return
	}
	scopeID, err := randomEffectToken()
	if err != nil {
		http.Error(writer, "the effect channel could not create a session", http.StatusInternalServerError)
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(hub.limits.MaxMessageBytes)
	session := newEffectSession(hub, connection, scopeID, request.Context())
	if !hub.addSession(session) {
		_ = connection.Close(websocket.StatusTryAgainLater, "effect session limit reached")
		return
	}
	defer hub.removeSession(scopeID)
	session.serve()
}

func loopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

type effectConfirmationAnswer struct {
	approved bool
	err      error
}

type pendingEffectConfirmation struct {
	nonce  string
	answer chan effectConfirmationAnswer
	once   sync.Once
}

func (pending *pendingEffectConfirmation) resolve(answer effectConfirmationAnswer) bool {
	delivered := false
	pending.once.Do(func() {
		pending.answer <- answer
		delivered = true
	})
	return delivered
}

type effectCallState struct {
	mu         sync.Mutex
	identity   string
	commitment string
	done       chan struct{}
	result     effectServerMessage
	record     EffectAuditRecord
	completed  bool
}

type effectSession struct {
	hub        *effectsHub
	connection *websocket.Conn
	scopeID    string
	ctx        context.Context
	cancel     context.CancelCauseFunc
	stopOnce   sync.Once

	writeMu sync.Mutex
	jobs    sync.WaitGroup

	mu             sync.Mutex
	inFlight       int
	boundSessionID string
	decisions      map[string]*pendingEffectConfirmation
	ledger         *legacyaction.Ledger
}

func newEffectSession(
	hub *effectsHub, connection *websocket.Conn, scopeID string, requestContext context.Context,
) *effectSession {
	if requestContext == nil {
		requestContext = hub.parent
	}
	ctx, cancel := context.WithCancelCause(requestContext)
	return &effectSession{
		hub: hub, connection: connection, scopeID: scopeID, ctx: ctx, cancel: cancel,
		decisions: make(map[string]*pendingEffectConfirmation), ledger: legacyaction.NewLedger(),
	}
}

func (session *effectSession) stop(cause error) {
	session.stopOnce.Do(func() {
		session.cancel(cause)
		_ = session.connection.CloseNow()
		session.mu.Lock()
		pending := make([]*pendingEffectConfirmation, 0, len(session.decisions))
		for _, decision := range session.decisions {
			pending = append(pending, decision)
		}
		session.mu.Unlock()
		for _, decision := range pending {
			decision.resolve(effectConfirmationAnswer{err: cause})
		}
	})
}

func (session *effectSession) serve() {
	defer func() {
		session.stop(errors.New("effect session closed"))
		session.jobs.Wait()
	}()
	if err := session.write(effectServerMessage{
		Type: "ready", Version: 1, ScopeID: session.scopeID,
		CatalogDigest: session.hub.catalogDigest, Tools: session.hub.Declarations(),
		Limits: &effectWireLimits{
			MaxMessageBytes: session.hub.limits.MaxMessageBytes,
			MaxResultBytes:  session.hub.limits.MaxResultBytes,
			MaxInFlight:     session.hub.limits.MaxInFlight, MaxCalls: session.hub.limits.MaxCalls,
			ConfirmationMS: uint64(session.hub.limits.Confirmation / time.Millisecond),
			ExecutionMS:    uint64(session.hub.limits.Execution / time.Millisecond),
		},
	}); err != nil {
		return
	}
	for {
		kind, payload, err := session.connection.Read(session.ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			_ = session.writeProtocolError("binary_message", "the effect channel accepts JSON text messages only", "")
			return
		}
		envelope, err := decodeEffectEnvelope(payload)
		if err != nil {
			_ = session.writeProtocolError("invalid_message", err.Error(), "")
			continue
		}
		switch envelope.Type {
		case "call":
			if !session.hub.callStarted() {
				_ = session.writeProtocolError("provider_quiescing",
					"the effect provider is not accepting new calls", "")
				continue
			}
			call, decodeErr := decodeEffectCall(payload)
			if decodeErr != nil {
				if validEffectID(call.ID) {
					session.respondDirectFailure(call, nil, "invalid_call", decodeErr.Error(), time.Now().UTC())
				} else {
					_ = session.writeProtocolError("invalid_call", decodeErr.Error(), "")
				}
				session.hub.callReleased()
				continue
			}
			if !session.startJob() {
				session.respondDirectFailure(call, nil, "too_many_in_flight",
					"the effect session reached its declared in-flight limit", time.Now().UTC())
				session.hub.callReleased()
				continue
			}
			session.jobs.Add(1)
			go func() {
				defer session.jobs.Done()
				defer session.finishJob()
				defer session.hub.callReleased()
				session.processCall(call)
			}()
		case "decide":
			decision, decodeErr := decodeEffectDecision(payload)
			if decodeErr != nil {
				_ = session.writeProtocolError("invalid_decision", decodeErr.Error(), decision.ID)
				continue
			}
			session.decide(decision)
		default:
			_ = session.writeProtocolError("unknown_message", "unknown effect message type", "")
		}
	}
}

func (session *effectSession) startJob() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.inFlight >= session.hub.limits.MaxInFlight || session.ctx.Err() != nil {
		return false
	}
	session.inFlight++
	return true
}

func (session *effectSession) finishJob() {
	session.mu.Lock()
	if session.inFlight > 0 {
		session.inFlight--
	}
	session.mu.Unlock()
}

func decodeEffectEnvelope(payload []byte) (effectClientEnvelope, error) {
	if err := strictjson.Validate(payload); err != nil {
		return effectClientEnvelope{}, err
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return effectClientEnvelope{}, errors.New("effect message must be one JSON object")
	}
	var envelope effectClientEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return effectClientEnvelope{}, err
	}
	if envelope.Type == "" || len(envelope.Type) > 32 {
		return effectClientEnvelope{}, errors.New("effect message requires a bounded type")
	}
	return envelope, nil
}

func decodeEffectCall(payload []byte) (effectClientCall, error) {
	var call effectClientCall
	if err := decodeStrictEffectMessage(payload, &call); err != nil {
		return call, err
	}
	if call.Type != "call" {
		return call, errors.New("effect call has the wrong message type")
	}
	if !validEffectID(call.SessionID) {
		return call, errors.New("effect call requires a canonical session_id")
	}
	if !validEffectID(call.ID) {
		return call, errors.New("effect call requires a canonical id")
	}
	if !validEffectName(call.Name) {
		return call, errors.New("effect call requires a canonical tool name")
	}
	if len(call.Arguments) == 0 {
		return call, errors.New("effect call requires arguments")
	}
	if call.Authority == "" || call.Authority != strings.TrimSpace(call.Authority) ||
		len(call.Authority) > maximumEffectAuthorityBytes || !utf8.ValidString(call.Authority) {
		return call, errors.New("effect call requires one canonical bounded authority receipt")
	}
	for _, character := range call.Authority {
		if character < 0x21 || character == 0x7f {
			return call, errors.New("effect authority receipt contains whitespace or control characters")
		}
	}
	return call, nil
}

func decodeEffectDecision(payload []byte) (effectClientDecision, error) {
	var decision effectClientDecision
	if err := decodeStrictEffectMessage(payload, &decision); err != nil {
		return decision, err
	}
	if decision.Type != "decide" || !validEffectID(decision.ID) {
		return decision, errors.New("effect decision requires type decide and a canonical id")
	}
	if len(decision.Nonce) != 32 {
		return decision, errors.New("effect decision requires the exact confirmation nonce")
	}
	if _, err := hex.DecodeString(decision.Nonce); err != nil || decision.Nonce != strings.ToLower(decision.Nonce) {
		return decision, errors.New("effect decision nonce is not canonical")
	}
	if decision.Approved == nil {
		return decision, errors.New("effect decision requires approved")
	}
	return decision, nil
}

func decodeStrictEffectMessage(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("effect message has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func validEffectID(value string) bool {
	if value == "" || len(value) > maximumEffectNameBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' ||
			character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func randomEffectToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (session *effectSession) write(message effectServerMessage) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > session.hub.limits.MaxMessageBytes {
		return fmt.Errorf("effect response has %d bytes; maximum is %d", len(encoded), session.hub.limits.MaxMessageBytes)
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	return session.connection.Write(session.ctx, websocket.MessageText, encoded)
}

func (session *effectSession) writeProtocolError(code, message, id string) error {
	return session.write(effectServerMessage{
		Type: "error", ID: id, Error: &effectWireError{Code: code, Message: boundedEffectMessage(message)},
	})
}

func boundedEffectMessage(message string) string {
	if !utf8.ValidString(message) {
		return "effect operation returned an invalid diagnostic"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "effect operation failed"
	}
	if len(message) <= maximumEffectProtocolErrorBytes {
		return message
	}
	message = message[:maximumEffectProtocolErrorBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}

func (session *effectSession) decide(decision effectClientDecision) {
	session.mu.Lock()
	pending := session.decisions[decision.ID]
	session.mu.Unlock()
	if pending == nil {
		_ = session.writeProtocolError("unknown_decision", "no confirmation is pending for this call", decision.ID)
		return
	}
	if pending.nonce != decision.Nonce {
		_ = session.writeProtocolError("confirmation_mismatch", "the confirmation nonce does not match this call", decision.ID)
		return
	}
	if !pending.resolve(effectConfirmationAnswer{approved: *decision.Approved}) {
		_ = session.writeProtocolError("duplicate_decision", "this confirmation was already answered", decision.ID)
	}
}

func (session *effectSession) processCall(wire effectClientCall) {
	started := time.Now().UTC()
	tool := session.hub.tools[wire.Name]
	if tool == nil {
		session.respondDirectFailure(wire, nil, "unknown_tool", "the requested tool is not declared by this effect provider", started)
		return
	}
	arguments, argumentsDigest, err := canonicalEffectArguments(tool.schema, wire.Arguments)
	if err != nil {
		session.respondDirectFailure(wire, tool, "invalid_arguments", err.Error(), started)
		return
	}
	call := EffectCall{
		ScopeID: session.scopeID, SessionID: wire.SessionID, CallID: wire.ID,
		Name: wire.Name, Arguments: arguments, ArgumentsDigest: argumentsDigest,
		DeclarationDigest: tool.declaration.Digest, Target: tool.declaration.Target,
	}
	authorityContext, cancelAuthority := context.WithTimeout(session.ctx, session.hub.limits.Execution)
	decision, authorityErr := authorizeEffect(authorityContext, session.hub.authority, EffectAuthorityRequest{
		ScopeID: session.scopeID, SessionID: call.SessionID, CallID: call.CallID,
		Name: call.Name, ArgumentsDigest: call.ArgumentsDigest,
		DeclarationDigest: call.DeclarationDigest, Target: call.Target, Evidence: wire.Authority,
	})
	cancelAuthority()
	if authorityErr != nil {
		code := "authority_denied"
		if errors.Is(authorityErr, context.DeadlineExceeded) {
			code = "authority_timeout"
		}
		session.respondDirectFailure(wire, tool, code, "effect authority did not admit this exact call", started)
		return
	}
	wantDecision := EffectAuthorityDecision{
		SessionID: call.SessionID, CallID: call.CallID, Name: call.Name,
		ArgumentsDigest: call.ArgumentsDigest, DeclarationDigest: call.DeclarationDigest,
		Target: call.Target,
	}
	if decision != wantDecision {
		session.respondDirectFailure(wire, tool, "authority_mismatch",
			"effect authority receipt does not bind the exact declaration, arguments, and target", started)
		return
	}
	if tool.fence != nil {
		fenceContext, cancelFence := context.WithTimeout(session.ctx, session.hub.limits.Execution)
		fenceErr := fenceEffect(fenceContext, tool.fence, EffectTargetRequest{
			Declaration: tool.declaration.clone(), Call: cloneEffectCall(call),
		})
		cancelFence()
		if fenceErr != nil {
			code := "target_rejected"
			if errors.Is(fenceErr, context.DeadlineExceeded) {
				code = "target_timeout"
			}
			session.respondDirectFailure(wire, tool, code, "effect target fence refused this call", started)
			return
		}
	}
	state, owner, reserveErr := session.reserveCall(call, tool)
	if reserveErr != nil {
		session.respondDirectFailure(wire, tool, "idempotency_refused", reserveErr.Error(), started)
		return
	}
	if !owner {
		session.replayCall(state, call, tool, started)
		return
	}
	record := EffectAuditRecord{
		ScopeID: session.scopeID, SessionID: call.SessionID, CallID: call.CallID,
		Name: call.Name, DeclarationDigest: call.DeclarationDigest, Target: call.Target,
		Confirmation: tool.declaration.Confirm, Authorized: true, StartedAt: started,
	}
	if err := session.ledger.Prepare(legacyaction.Commitment{
		ID: state.commitment, Kind: effectActionKind(call.Name), CallID: call.CallID,
		Confirm: tool.declaration.Confirm,
	}); err != nil {
		session.completeFailure(state, record, "ledger_refused", "the effect commitment could not be prepared")
		return
	}
	confirmed, confirmationCode, confirmationErr := session.confirm(call, tool)
	if confirmationErr != nil || !confirmed {
		_, _ = session.ledger.Cancel(state.commitment, confirmationCode)
		message := "the effect confirmation was declined"
		if confirmationErr != nil {
			message = confirmationErr.Error()
		}
		session.completeFailure(state, record, confirmationCode, message)
		return
	}
	record.Confirmed = true
	if !session.hub.open() || session.ctx.Err() != nil {
		_, _ = session.ledger.Cancel(state.commitment, "effect provider scope closed")
		session.completeFailure(state, record, "effect_canceled", "the effect provider scope closed before execution")
		return
	}
	if !session.hub.permissions.Allows(effectPermissionKind, tool.resource, tool.operation) {
		_, _ = session.ledger.Cancel(state.commitment, "permission withdrawn")
		session.completeFailure(state, record, "permission_denied", "the deployment permission does not admit this effect")
		return
	}
	if err := session.ledger.Queue(state.commitment); err != nil {
		session.completeFailure(state, record, "ledger_refused", "the effect commitment could not be queued")
		return
	}
	if err := session.ledger.Emit(state.commitment); err != nil {
		session.completeFailure(state, record, "ledger_refused", "the effect commitment could not cross")
		return
	}
	record.Crossed = true
	record.Executed = true
	executionContext, cancelExecution := context.WithTimeout(session.ctx, session.hub.limits.Execution)
	result, executeErr := executeEffect(executionContext, tool.executor, cloneEffectCall(call))
	cancelExecution()
	_ = session.ledger.Complete(state.commitment, 0)
	if executeErr != nil {
		code := "effect_failed"
		message := executeErr.Error()
		if errors.Is(executeErr, context.DeadlineExceeded) {
			code, message = "effect_timeout", "the admitted effect exceeded its execution timeout"
		} else if errors.Is(executeErr, context.Canceled) {
			code, message = "effect_canceled", "the admitted effect was canceled"
		}
		session.completeFailure(state, record, code, message)
		return
	}
	wireResult, validateErr := session.validateResult(call, tool, result)
	if validateErr != nil {
		session.completeFailure(state, record, "invalid_result", validateErr.Error())
		return
	}
	session.completeState(state, wireResult, record)
}

func cloneEffectCall(call EffectCall) EffectCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func authorizeEffect(
	ctx context.Context, authority EffectAuthority, request EffectAuthorityRequest,
) (decision EffectAuthorityDecision, err error) {
	defer func() {
		if recover() != nil {
			decision = EffectAuthorityDecision{}
			err = errors.New("effect authority verifier panicked")
		}
	}()
	return authority.Authorize(ctx, request)
}

func fenceEffect(
	ctx context.Context, fence EffectTargetFence, request EffectTargetRequest,
) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("effect target fence panicked")
		}
	}()
	return fence.Admit(ctx, request)
}

func executeEffect(ctx context.Context, executor EffectExecutor, call EffectCall) (result EffectResult, err error) {
	defer func() {
		if recover() != nil {
			result = EffectResult{}
			err = errors.New("effect executor panicked")
		}
	}()
	return executor.Execute(ctx, call)
}

func effectActionKind(name string) legacyaction.Kind {
	if strings.HasPrefix(name, "computer.") {
		return legacyaction.KindComputerAction
	}
	return legacyaction.KindToolCall
}

func effectCallIdentity(call EffectCall) string {
	payload, _ := json.Marshal(struct {
		SessionID         string `json:"session_id"`
		CallID            string `json:"call_id"`
		Name              string `json:"name"`
		ArgumentsDigest   string `json:"arguments_digest"`
		DeclarationDigest string `json:"declaration_digest"`
		Target            string `json:"target"`
	}{call.SessionID, call.CallID, call.Name, call.ArgumentsDigest, call.DeclarationDigest, call.Target})
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (session *effectSession) reserveCall(
	call EffectCall, tool *compiledEffectTool,
) (*effectCallState, bool, error) {
	identity := effectCallIdentity(call)
	session.mu.Lock()
	if session.boundSessionID == "" {
		session.boundSessionID = call.SessionID
	} else if session.boundSessionID != call.SessionID {
		session.mu.Unlock()
		return nil, false, errors.New("one effect socket cannot cross realtime session scopes")
	}
	session.mu.Unlock()
	key := call.SessionID + "\x00" + call.CallID
	session.hub.mu.Lock()
	defer session.hub.mu.Unlock()
	if session.hub.closed || session.ctx.Err() != nil {
		return nil, false, errors.New("effect provider scope is closed")
	}
	if state := session.hub.calls[key]; state != nil {
		if state.identity != identity {
			return nil, false, errors.New("call ID was already admitted with a different identity")
		}
		return state, false, nil
	}
	if len(session.hub.calls) >= session.hub.limits.MaxCalls {
		return nil, false, errors.New("effect provider exhausted its bounded call history; remount before admitting a new server session")
	}
	commitmentDigest := sha256.Sum256([]byte(session.scopeID + "\x00" + call.SessionID + "\x00" + call.CallID))
	state := &effectCallState{
		identity: identity, commitment: "effect_" + hex.EncodeToString(commitmentDigest[:]),
		done: make(chan struct{}),
	}
	session.hub.calls[key] = state
	_ = tool
	return state, true, nil
}

func (session *effectSession) replayCall(
	state *effectCallState, call EffectCall, tool *compiledEffectTool, started time.Time,
) {
	select {
	case <-state.done:
	case <-session.ctx.Done():
		return
	}
	state.mu.Lock()
	message := state.result
	record := state.record
	state.mu.Unlock()
	record.ScopeID = session.scopeID
	record.SessionID = call.SessionID
	record.CallID = call.CallID
	record.Name = call.Name
	record.DeclarationDigest = tool.declaration.Digest
	record.Target = tool.declaration.Target
	record.Replayed = true
	record.Crossed = false
	record.Executed = false
	record.StartedAt = started
	record.FinishedAt = time.Now().UTC()
	session.hub.record(record)
	_ = session.write(message)
}

func (session *effectSession) confirm(
	call EffectCall, tool *compiledEffectTool,
) (bool, string, error) {
	switch tool.declaration.Confirm {
	case legacyaction.ConfirmNever:
		return true, "", nil
	case legacyaction.ConfirmPolicy:
		if session.hub.policy != nil {
			admitted, policyErr := effectPolicyDecision(session.hub.policy, EffectTargetRequest{
				Declaration: tool.declaration.clone(), Call: cloneEffectCall(call),
			})
			if policyErr != nil {
				return false, "confirmation_policy_failed", errors.New("the effect confirmation policy failed")
			}
			if admitted {
				return true, "", nil
			}
		}
	}
	nonce, err := randomEffectToken()
	if err != nil {
		return false, "confirmation_unavailable", errors.New("effect confirmation could not create a nonce")
	}
	pending := &pendingEffectConfirmation{nonce: nonce, answer: make(chan effectConfirmationAnswer, 1)}
	session.mu.Lock()
	if _, duplicate := session.decisions[call.CallID]; duplicate {
		session.mu.Unlock()
		return false, "duplicate_confirmation", errors.New("an effect confirmation is already pending for this call")
	}
	session.decisions[call.CallID] = pending
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		if session.decisions[call.CallID] == pending {
			delete(session.decisions, call.CallID)
		}
		session.mu.Unlock()
	}()
	if err := session.write(effectServerMessage{
		Type: "confirm", ID: call.CallID, Name: call.Name, Arguments: slices.Clone(call.Arguments),
		Nonce: nonce, Confirm: tool.declaration.Confirm, Target: tool.declaration.Target,
		Channel: tool.declaration.Channel,
	}); err != nil {
		return false, "confirmation_delivery_failed", errors.New("effect confirmation could not be delivered")
	}
	confirmationContext, cancel := context.WithTimeout(session.ctx, session.hub.limits.Confirmation)
	defer cancel()
	select {
	case <-confirmationContext.Done():
		if errors.Is(confirmationContext.Err(), context.DeadlineExceeded) {
			return false, "confirmation_timeout", errors.New("effect confirmation was not answered before its timeout")
		}
		return false, "confirmation_canceled", errors.New("effect confirmation was canceled")
	case answer := <-pending.answer:
		if answer.err != nil {
			return false, "confirmation_canceled", errors.New("effect confirmation was canceled")
		}
		if !answer.approved {
			return false, "confirmation_declined", nil
		}
		return true, "", nil
	}
}

func effectPolicyDecision(policy EffectPolicy, request EffectTargetRequest) (admitted bool, err error) {
	defer func() {
		if recover() != nil {
			admitted = false
			err = errors.New("effect policy panicked")
		}
	}()
	return policy(request), nil
}

func (session *effectSession) validateResult(
	call EffectCall, tool *compiledEffectTool, result EffectResult,
) (effectServerMessage, error) {
	if !utf8.ValidString(result.Output) {
		return effectServerMessage{}, errors.New("effect output is not valid UTF-8")
	}
	if int64(len(result.Output)) > session.hub.limits.MaxResultBytes {
		return effectServerMessage{}, fmt.Errorf("effect output has %d bytes; max_result_bytes is %d",
			len(result.Output), session.hub.limits.MaxResultBytes)
	}
	if result.Artifact != nil && result.Download != nil {
		return effectServerMessage{}, errors.New("effect output cannot publish both an artifact and a download")
	}
	message := effectServerMessage{
		Type: "result", ID: call.CallID, Channel: tool.declaration.Channel, Output: result.Output,
	}
	if result.Artifact != nil {
		if tool.declaration.Channel != EffectChannelArtifact {
			return effectServerMessage{}, errors.New("only an artifact-channel declaration may return an artifact reference")
		}
		stored, found := session.hub.artifacts.Lookup(result.Artifact.ID)
		if !found || stored != *result.Artifact {
			return effectServerMessage{}, errors.New("effect returned an artifact reference not owned by the mounted store")
		}
		copy := stored
		message.Artifact = &copy
	}
	if result.Download != nil {
		if tool.declaration.Channel != EffectChannelDownload {
			return effectServerMessage{}, errors.New("only a download-channel declaration may return a download reference")
		}
		stored, found := session.hub.downloads.Lookup(result.Download.ID)
		if !found || stored != *result.Download {
			return effectServerMessage{}, errors.New("effect returned a download reference not owned by the mounted store")
		}
		copy := stored
		message.Download = &copy
	}
	return message, nil
}

func (session *effectSession) completeFailure(
	state *effectCallState, record EffectAuditRecord, code, message string,
) {
	record.ErrorCode = code
	session.completeState(state, effectServerMessage{
		Type: "result", ID: record.CallID,
		Error: &effectWireError{Code: code, Message: boundedEffectMessage(message)},
	}, record)
}

func (session *effectSession) completeState(
	state *effectCallState, message effectServerMessage, record EffectAuditRecord,
) {
	record.FinishedAt = time.Now().UTC()
	state.mu.Lock()
	if state.completed {
		state.mu.Unlock()
		return
	}
	state.completed = true
	state.result = message
	state.record = record
	close(state.done)
	state.mu.Unlock()
	session.hub.record(record)
	_ = session.write(message)
}

func (session *effectSession) respondDirectFailure(
	call effectClientCall, tool *compiledEffectTool, code, message string, started time.Time,
) {
	record := EffectAuditRecord{
		ScopeID: session.scopeID, ErrorCode: code,
		StartedAt: started, FinishedAt: time.Now().UTC(),
	}
	if validEffectID(call.SessionID) {
		record.SessionID = call.SessionID
	}
	if validEffectID(call.ID) {
		record.CallID = call.ID
	}
	if validEffectName(call.Name) {
		record.Name = call.Name
	}
	channel := EffectChannel("")
	if tool != nil {
		record.DeclarationDigest = tool.declaration.Digest
		record.Target = tool.declaration.Target
		record.Confirmation = tool.declaration.Confirm
		channel = tool.declaration.Channel
	}
	session.hub.record(record)
	_ = session.write(effectServerMessage{
		Type: "result", ID: call.ID, Channel: channel,
		Error: &effectWireError{Code: code, Message: boundedEffectMessage(message)},
	})
}

var _ pluginruntime.Factory = (*DenyEffectAuthorityFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*DenyEffectAuthorityFactory)(nil)
var _ pluginruntime.Factory = (*EffectsFactory)(nil)
var _ pluginruntime.ConfigValidator = (*EffectsFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*EffectsFactory)(nil)
var _ pluginruntime.CandidateStateMigrator = effectsCandidate{}
var _ Effects = (*effectsHub)(nil)
