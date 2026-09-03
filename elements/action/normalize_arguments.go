package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/internal/toolargs"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const maximumNormalizationJSONBytes = toolargs.MaximumJSONBytes

type normalizeArgumentsFactory struct{}

var (
	_ element.Factory         = normalizeArgumentsFactory{}
	_ element.ConfigValidator = normalizeArgumentsFactory{}
)

func (normalizeArgumentsFactory) Descriptor() element.Descriptor {
	return NormalizeArgumentsDescriptor()
}

func (normalizeArgumentsFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeNormalizeArgumentsConfig(source)
	return err
}

func (normalizeArgumentsFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	if _, err := decodeNormalizeArgumentsConfig(mount.Config); err != nil {
		return nil, fmt.Errorf("action.NormalizeArguments %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(ToolRegistryService)
	if !found {
		return nil, fmt.Errorf("action.NormalizeArguments %s has no tool registries service", mount.InstanceID)
	}
	registries, ok := service.(*ToolRegistries)
	if !ok || registries == nil {
		return nil, fmt.Errorf("tool registries service has type %T", service)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	input, err := mount.Ports.Input("action")
	if err != nil {
		return nil, err
	}
	normalized, err := mount.Ports.Output("normalized")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	resolved, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	return &normalizeArgumentsRunner{
		registries: registries, serviceRevision: serviceRevision,
		emit:  emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		input: input, normalized: normalized, outcome: outcome, resolved: resolved,
		resolution: mount.Resolution,
	}, nil
}

type normalizeArgumentsRunner struct {
	registries      *ToolRegistries
	serviceRevision uint64
	emit            emitter
	input           element.InputPort
	normalized      element.OutputPort
	outcome         element.OutputPort
	resolved        element.OutputPort
	resolution      element.ResolutionReporter
}

func (runner *normalizeArgumentsRunner) Run(ctx context.Context) error {
	if err := publishResolution(ctx, runner.emit, runner.resolved, Resolution{
		Stage: "argument_normalization", Reference: ToolRegistryService,
		Identity:        "closed-tool-argument-normalizers-v2",
		ServiceRevision: runner.serviceRevision,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, NormalizeArgumentsDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"argument-normalization", "action.ArgumentNormalization/v1",
			"registry://"+ToolRegistryService, runner.serviceRevision, "",
		)}); err != nil {
		return err
	}
	for {
		envelope, err := runner.input.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := runner.accept(ctx, envelope); err != nil {
			return err
		}
	}
}

func (runner *normalizeArgumentsRunner) accept(ctx context.Context, envelope element.Envelope) error {
	declared, ok := declaredPayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize",
			Code: "invalid_payload", Message: fmt.Sprintf("declared action payload has type %T", envelope.Payload),
		})
	}
	call := callOfAdmitted(declared.Admitted)
	if err := validateDeclaredAction(declared); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "invalid_declaration", Message: err.Error(),
		})
	}
	if code, err := validateActionEnvelopeIdentity(envelope, declared.Admitted); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: code, Message: err.Error(),
		})
	}
	set, err := runner.registries.resolve(declared.RegistryReference)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "resolution_drift", Message: err.Error(),
		})
	}
	if set.digest != declared.RegistryDigest {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "resolution_drift", Message: "declared tool registry digest differs from the live registry",
		})
	}
	tool, found, err := set.lookup(call.Name)
	if err != nil || !found {
		message := fmt.Sprintf("tool %q is not declared", call.Name)
		if err != nil {
			message = err.Error()
		}
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "resolution_drift", Message: message,
		})
	}
	if tool.digest != declared.DeclarationDigest {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "resolution_drift", Message: "declared tool digest differs from the live declaration",
		})
	}
	normalized, changed, err := normalizeDeclaredAction(declared, tool.spec)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "argument_normalization", Operation: "normalize", CallID: call.CallID,
			Code: "normalization_refused", Message: err.Error(),
		})
	}
	resultCode := "unchanged"
	if changed {
		resultCode = "normalized"
	}
	if err := publishPayload(ctx, runner.emit, runner.normalized, envelope, declaredType, normalized, resultCode); err != nil {
		return err
	}
	return publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
		Kind: OutcomeSucceeded, Stage: "argument_normalization", Operation: "normalize",
		CallID: call.CallID, Code: resultCode,
	})
}

type normalizationSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

type normalizationProperty struct {
	Type json.RawMessage `json:"type"`
}

func normalizeDeclaredAction(declared DeclaredAction, spec legacyaction.ToolSpec) (DeclaredAction, bool, error) {
	if declared.EffectiveCall != nil || declared.Normalization != nil {
		return DeclaredAction{}, false, errors.New("declared action already carries argument normalization")
	}
	if len(spec.Parameters) > maximumNormalizationJSONBytes {
		return DeclaredAction{}, false, fmt.Errorf("tool schema exceeds %d bytes", maximumNormalizationJSONBytes)
	}
	limits := strictjson.Limits{
		MaxInputBytes: maximumNormalizationJSONBytes, MaxDepth: 32, MaxTokens: 16_384,
		MaxObjectMembers: 4_096, MaxArrayElements: 4_096, MaxKeyBytes: 4_096,
		MaxTotalKeyBytes: maximumNormalizationJSONBytes, MaxWorkBytes: 8 * maximumNormalizationJSONBytes,
	}
	if err := strictjson.ValidateWithLimits(spec.Parameters, limits); err != nil {
		return DeclaredAction{}, false, fmt.Errorf("tool schema: %w", err)
	}
	var schema normalizationSchema
	if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
		return DeclaredAction{}, false, fmt.Errorf("decode tool schema: %w", err)
	}
	if len(spec.ArgumentNormalizers) == 0 {
		return declared, false, nil
	}
	if schema.Type != "object" || schema.Properties == nil {
		return DeclaredAction{}, false, errors.New("normalizable tool schema must declare an object with properties")
	}

	original := declared.Admitted.Proposal.Call
	normalizers := slices.Clone(spec.ArgumentNormalizers)
	sort.Slice(normalizers, func(i, j int) bool { return normalizers[i].Argument < normalizers[j].Argument })
	rules := make([]toolargs.Rule, 0, len(normalizers))
	last := ""
	for index, configured := range normalizers {
		name := configured.Argument
		if name == "" || name != strings.TrimSpace(name) || (index > 0 && name == last) {
			return DeclaredAction{}, false, errors.New("tool argument normalizers must name unique canonical arguments")
		}
		last = name
		if !toolargs.Supported(configured.Normalizer) {
			return DeclaredAction{}, false, fmt.Errorf("tool property %q names unsupported normalizer %q", name, configured.Normalizer)
		}
		rawProperty, declaredProperty := schema.Properties[name]
		if !declaredProperty {
			return DeclaredAction{}, false, fmt.Errorf("tool normalizer names undeclared property %q", name)
		}
		if err := strictjson.ValidateWithLimits(rawProperty, limits); err != nil {
			return DeclaredAction{}, false, fmt.Errorf("tool property %q: %w", name, err)
		}
		var property normalizationProperty
		if err := json.Unmarshal(rawProperty, &property); err != nil {
			return DeclaredAction{}, false, fmt.Errorf("decode tool property %q: %w", name, err)
		}
		var propertyType string
		if err := json.Unmarshal(property.Type, &propertyType); err != nil {
			return DeclaredAction{}, false, fmt.Errorf("tool property %q has no single declared type", name)
		}
		switch configured.Normalizer {
		case legacyaction.ToolParameterCompactASCIIAlphanumericV1:
			if propertyType != "string" {
				return DeclaredAction{}, false,
					fmt.Errorf("tool property %q compact identifier normalizer requires type string", name)
			}
		case legacyaction.ToolParameterCoordinatePairXYV1:
			if name != "x" || propertyType != "integer" {
				return DeclaredAction{}, false,
					errors.New("coordinate pair normalizer requires integer property x")
			}
			y, found := schema.Properties["y"]
			if !found {
				return DeclaredAction{}, false,
					errors.New("coordinate pair normalizer requires declared property y")
			}
			if err := strictjson.ValidateWithLimits(y, limits); err != nil {
				return DeclaredAction{}, false, fmt.Errorf("tool property y: %w", err)
			}
			var yProperty normalizationProperty
			var yType string
			if err := json.Unmarshal(y, &yProperty); err != nil ||
				json.Unmarshal(yProperty.Type, &yType) != nil || yType != "integer" {
				return DeclaredAction{}, false,
					errors.New("coordinate pair normalizer requires integer property y")
			}
			if !slices.Contains(schema.Required, "x") || !slices.Contains(schema.Required, "y") {
				return DeclaredAction{}, false,
					errors.New("coordinate pair normalizer requires x and y in the schema required set")
			}
		}
		rules = append(rules, toolargs.Rule{Argument: name, Normalizer: configured.Normalizer})
	}
	effectiveArguments, changedRules, err := toolargs.Apply(original.Arguments, rules)
	if err != nil {
		return DeclaredAction{}, false, err
	}
	if len(changedRules) == 0 {
		return declared, false, nil
	}
	rewrites := make([]ArgumentRewrite, len(changedRules))
	for index, rule := range changedRules {
		rewrites[index] = ArgumentRewrite{Argument: rule.Argument, Normalizer: rule.Normalizer}
	}
	effective := cloneToolCall(original)
	effective.Arguments = effectiveArguments
	declared.EffectiveCall = &effective
	declared.Normalization = &ArgumentNormalization{
		Rewrites:                 rewrites,
		OriginalArgumentsDigest:  argumentBytesDigest(original.Arguments),
		EffectiveArgumentsDigest: argumentBytesDigest(effective.Arguments),
		RegistryReference:        declared.RegistryReference, RegistryDigest: declared.RegistryDigest,
		DeclarationDigest: declared.DeclarationDigest,
	}
	return declared, true, nil
}

// validateDeclaredNormalizationAgainstSpec replays the deterministic
// derivation at the final deployment-attestation boundary. A forged effective
// call or annotation claim therefore cannot survive merely because its
// before/after digests are internally consistent.
func validateDeclaredNormalizationAgainstSpec(declared DeclaredAction, spec legacyaction.ToolSpec) error {
	if declared.EffectiveCall == nil && declared.Normalization == nil {
		return nil
	}
	base := cloneDeclared(declared)
	base.EffectiveCall = nil
	base.Normalization = nil
	derived, changed, err := normalizeDeclaredAction(base, spec)
	if err != nil {
		return fmt.Errorf("revalidate argument normalization: %w", err)
	}
	if !changed || derived.EffectiveCall == nil || derived.Normalization == nil {
		return errors.New("argument normalization evidence claims a transformation the declaration does not produce")
	}
	if declared.EffectiveCall.CallID != derived.EffectiveCall.CallID ||
		declared.EffectiveCall.Name != derived.EffectiveCall.Name ||
		!bytes.Equal(declared.EffectiveCall.Arguments, derived.EffectiveCall.Arguments) {
		return errors.New("effective call differs from deterministic schema normalization")
	}
	left, right := declared.Normalization, derived.Normalization
	if left.OriginalArgumentsDigest != right.OriginalArgumentsDigest ||
		left.EffectiveArgumentsDigest != right.EffectiveArgumentsDigest ||
		left.RegistryReference != right.RegistryReference || left.RegistryDigest != right.RegistryDigest ||
		left.DeclarationDigest != right.DeclarationDigest || !slices.Equal(left.Rewrites, right.Rewrites) {
		return errors.New("argument normalization evidence differs from deterministic schema normalization")
	}
	return nil
}

func argumentBytesDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func normalizationCall(value DeclaredAction) trajectory.ToolCall {
	if value.EffectiveCall != nil {
		return cloneToolCall(*value.EffectiveCall)
	}
	return cloneToolCall(value.Admitted.Proposal.Call)
}
