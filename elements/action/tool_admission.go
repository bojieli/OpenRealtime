package action

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
)

type toolAdmissionFactory struct{}

var (
	_ element.Factory         = toolAdmissionFactory{}
	_ element.ConfigValidator = toolAdmissionFactory{}
	_ element.Runnable        = (*toolAdmissionRunner)(nil)
)

func (toolAdmissionFactory) Descriptor() element.Descriptor { return ToolAdmissionDescriptor() }

func (toolAdmissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeToolAdmissionConfig(source)
	return err
}

func (toolAdmissionFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeToolAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.ToolAdmission %s config: %w", mount.InstanceID, err)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	input, err := mount.Ports.Input("action")
	if err != nil {
		return nil, err
	}
	admitted, err := mount.Ports.Output("admitted")
	if err != nil {
		return nil, err
	}
	terminal, err := mount.Ports.Output("terminal")
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
	allowed := make(map[string]struct{}, len(config.AllowedTools))
	for _, name := range config.AllowedTools {
		allowed[name] = struct{}{}
	}
	denied := make(map[string]struct{}, len(config.DeniedTools))
	for _, name := range config.DeniedTools {
		denied[name] = struct{}{}
	}
	digest, err := digestJSON(config)
	if err != nil {
		return nil, fmt.Errorf("digest action.ToolAdmission %s config: %w", mount.InstanceID, err)
	}
	return &toolAdmissionRunner{
		allowed: allowed, denied: denied, policyDigest: digest,
		emit:  emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		input: input, admitted: admitted, terminal: terminal, outcome: outcome, resolved: resolved,
		resolution: mount.Resolution,
	}, nil
}

type toolAdmissionRunner struct {
	allowed, denied map[string]struct{}
	policyDigest    string
	emit            emitter
	input           element.InputPort
	admitted        element.OutputPort
	terminal        element.OutputPort
	outcome         element.OutputPort
	resolved        element.OutputPort
	resolution      element.ResolutionReporter
}

func (runner *toolAdmissionRunner) Run(ctx context.Context) error {
	if err := publishResolution(ctx, runner.emit, runner.resolved, Resolution{
		Stage: "tool_admission", Reference: "static-tool-name-policy",
		Identity: "allow-deny-v1", Digest: runner.policyDigest,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ToolAdmissionDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"tool-admission", "action.ToolAdmission/v1",
			"policy://static-tool-name-admission", 0, runner.policyDigest,
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

func (runner *toolAdmissionRunner) accept(ctx context.Context, envelope element.Envelope) error {
	declared, ok := declaredPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "", "invalid_payload",
			fmt.Sprintf("declared action payload has type %T", envelope.Payload))
	}
	call := callOfDeclared(declared)
	if err := validateDeclaredAction(declared); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, call.CallID,
			"invalid_declaration", err.Error())
	}
	if code, err := validateActionEnvelopeIdentity(envelope, declared.Admitted); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, call.CallID, code, err.Error())
	}
	code := "allowed"
	message := ""
	if _, denied := runner.denied[call.Name]; denied {
		code = "tool_denied"
		message = "the graph tool policy denies this declared tool"
	} else if len(runner.allowed) != 0 {
		if _, allowed := runner.allowed[call.Name]; !allowed {
			code = "tool_not_allowed"
			message = "the declared tool is outside the graph tool allow list"
		}
	}
	if message != "" {
		terminal := PreEffectTerminal{Kind: PreEffectToolPolicySuppressed, CallID: call.CallID}
		if err := runner.publishTerminal(ctx, envelope, terminal); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, call.CallID, code, message)
	}
	if err := publishPayload(ctx, runner.emit, runner.admitted, envelope,
		declaredType, declared, "admitted"); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, OutcomeSucceeded, call.CallID, code, "")
}

func (runner *toolAdmissionRunner) publishTerminal(
	ctx context.Context, cause element.Envelope, terminal PreEffectTerminal,
) error {
	envelope, err := runner.emit.envelope(cause, preEffectTerminalType, terminal, "terminal")
	if err != nil {
		return err
	}
	delivery, err := runner.terminal.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered < 1 || delivery.Dropped != 0 {
		return fmt.Errorf("tool-admission terminal delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (runner *toolAdmissionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, kind OutcomeKind, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcome, cause, Outcome{
		Kind: kind, Stage: "tool_admission", Operation: "admit",
		CallID: callID, Code: code, Message: message,
	})
}
