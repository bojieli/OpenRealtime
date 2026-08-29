package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/element"
)

type toolLookupFactory struct{}

var (
	_ element.Factory         = toolLookupFactory{}
	_ element.ConfigValidator = toolLookupFactory{}
)

func (toolLookupFactory) Descriptor() element.Descriptor { return ToolLookupDescriptor() }
func (toolLookupFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeToolLookupConfig(source)
	return err
}

func (toolLookupFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeToolLookupConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.ToolLookup %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(ToolRegistryService)
	if !found {
		return nil, fmt.Errorf("action.ToolLookup %s has no tool registries service", mount.InstanceID)
	}
	registries, ok := service.(*ToolRegistries)
	if !ok || registries == nil {
		return nil, fmt.Errorf("tool registries service has type %T", service)
	}
	set, err := registries.resolve(config.Registry)
	if err != nil {
		return nil, err
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	input, err := mount.Ports.Input("proposal")
	if err != nil {
		return nil, err
	}
	declared, err := mount.Ports.Output("declared")
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
	return &toolLookupRunner{
		set: set, serviceRevision: serviceRevision,
		emit:  emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		input: input, declared: declared, outcome: outcome, resolved: resolved,
		resolution: mount.Resolution,
	}, nil
}

type toolLookupRunner struct {
	set             toolSet
	serviceRevision uint64
	emit            emitter
	input           element.InputPort
	declared        element.OutputPort
	outcome         element.OutputPort
	resolved        element.OutputPort
	resolution      element.ResolutionReporter
}

func (runner *toolLookupRunner) Run(ctx context.Context) error {
	if err := publishResolution(ctx, runner.emit, runner.resolved, Resolution{
		Stage: "tool_lookup", Reference: runner.set.reference, Identity: "action.ToolRegistry",
		Digest: runner.set.digest, ServiceRevision: runner.serviceRevision,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ToolLookupDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"tool-declarations", "action.ToolRegistry/v1", "registry://"+runner.set.reference,
			runner.serviceRevision, runner.set.digest,
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
		admitted, ok := admittedPayload(envelope.Payload)
		if !ok {
			if err := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
				Kind: OutcomeRejected, Stage: "tool_lookup", Operation: "lookup",
				Code: "invalid_payload", Message: fmt.Sprintf("admitted proposal payload has type %T", envelope.Payload),
			}); err != nil {
				return err
			}
			continue
		}
		call := callOfAdmitted(admitted)
		tool, found, lookupErr := runner.set.lookup(call.Name)
		if lookupErr != nil || !found {
			code, message := "unknown_tool", fmt.Sprintf("tool %q is not declared", call.Name)
			if lookupErr != nil {
				code, message = "resolution_drift", lookupErr.Error()
			}
			if err := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
				Kind: OutcomeRejected, Stage: "tool_lookup", Operation: "lookup", CallID: call.CallID,
				Code: code, Message: message,
			}); err != nil {
				return err
			}
			continue
		}
		declared := DeclaredAction{
			Admitted: admitted, Confirmation: tool.spec.Confirm, Target: tool.spec.Target,
			Background: tool.spec.Background, RegistryReference: runner.set.reference,
			RegistryDigest: runner.set.digest, DeclarationDigest: tool.digest,
			DispatcherIdentity: tool.dispatcherIdentity,
		}
		if err := publishPayload(ctx, runner.emit, runner.declared, envelope, declaredType, declared, "declared"); err != nil {
			return err
		}
		if err := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeSucceeded, Stage: "tool_lookup", Operation: "lookup", CallID: call.CallID,
		}); err != nil {
			return err
		}
	}
}

type targetFenceFactory struct{}

var (
	_ element.Factory         = targetFenceFactory{}
	_ element.ConfigValidator = targetFenceFactory{}
)

func (targetFenceFactory) Descriptor() element.Descriptor { return TargetFenceDescriptor() }
func (targetFenceFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTargetFenceConfig(source)
	return err
}

func (targetFenceFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeTargetFenceConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("authority.TargetFence %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(TargetRegistryService)
	if !found {
		return nil, fmt.Errorf("authority.TargetFence %s has no target registries service", mount.InstanceID)
	}
	registries, ok := service.(*TargetRegistries)
	if !ok || registries == nil {
		return nil, fmt.Errorf("target registries service has type %T", service)
	}
	target, err := registries.resolve(config.Target)
	if err != nil {
		return nil, err
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	input, err := mount.Ports.Input("action")
	if err != nil {
		return nil, err
	}
	authorized, err := mount.Ports.Output("authorized")
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
	return &targetFenceRunner{
		target: target, serviceRevision: serviceRevision,
		emit:  emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		input: input, authorized: authorized, outcome: outcome, resolved: resolved,
		resolution: mount.Resolution,
	}, nil
}

type targetFenceRunner struct {
	target          targetEntry
	serviceRevision uint64
	emit            emitter
	input           element.InputPort
	authorized      element.OutputPort
	outcome         element.OutputPort
	resolved        element.OutputPort
	resolution      element.ResolutionReporter
}

func (runner *targetFenceRunner) Run(ctx context.Context) error {
	if err := publishResolution(ctx, runner.emit, runner.resolved, Resolution{
		Stage: "target_fence", Reference: runner.target.reference, Identity: runner.target.target.Name,
		Digest: runner.target.digest, ServiceRevision: runner.serviceRevision,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, TargetFenceDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"target-fence", "computeruse.Target/v1", "target://"+runner.target.reference,
			runner.serviceRevision, runner.target.digest,
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
		confirmed, ok := confirmedPayload(envelope.Payload)
		if !ok {
			if err := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
				Kind: OutcomeRejected, Stage: "target_fence", Operation: "fence",
				Code: "invalid_payload", Message: fmt.Sprintf("confirmed action payload has type %T", envelope.Payload),
			}); err != nil {
				return err
			}
			continue
		}
		call := callOfConfirmed(confirmed)
		if err := runner.validate(confirmed); err != nil {
			if publishErr := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
				Kind: OutcomeRejected, Stage: "target_fence", Operation: "fence", CallID: call.CallID,
				Code: "target_rejected", Message: err.Error(),
			}); publishErr != nil {
				return publishErr
			}
			continue
		}
		authorized := AuthorizedAction{Confirmed: confirmed, TargetReference: runner.target.reference, TargetDigest: runner.target.digest}
		if err := publishPayload(ctx, runner.emit, runner.authorized, envelope, authorizedType, authorized, "authorized"); err != nil {
			return err
		}
		if err := publishOutcome(ctx, runner.emit, runner.outcome, envelope, Outcome{
			Kind: OutcomeSucceeded, Stage: "target_fence", Operation: "fence", CallID: call.CallID,
		}); err != nil {
			return err
		}
	}
}

func (runner *targetFenceRunner) validate(confirmed ConfirmedAction) error {
	declared := confirmed.Declared
	call := callOfDeclared(declared)
	if !computeruse.IsAction(call.Name) {
		if declared.Target != "" && declared.Target != runner.target.target.Name {
			return fmt.Errorf("declared target %q does not match resolved target %q", declared.Target, runner.target.target.Name)
		}
		return nil
	}
	if _, standard := computeruse.Lookup(call.Name); !standard {
		return fmt.Errorf("%q is not a declared computer-use action", call.Name)
	}
	if declared.Target != runner.target.target.Name {
		return fmt.Errorf("declared target %q does not match resolved target %q", declared.Target, runner.target.target.Name)
	}
	var arguments struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return fmt.Errorf("decode target arguments: %w", err)
	}
	arguments.Source = strings.TrimSpace(arguments.Source)
	if arguments.Source == "" {
		if call.Name == computeruse.Wait {
			return nil
		}
		return fmt.Errorf("computer action %q does not name a video source", call.Name)
	}
	if !runner.target.target.Owns(arguments.Source) {
		return fmt.Errorf("target %q does not own source %q", runner.target.target.Name, arguments.Source)
	}
	return nil
}
