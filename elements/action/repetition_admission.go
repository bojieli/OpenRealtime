package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/element"
)

type repetitionAdmissionFactory struct{}

var (
	_ element.Factory         = repetitionAdmissionFactory{}
	_ element.ConfigValidator = repetitionAdmissionFactory{}
	_ element.Runnable        = (*repetitionAdmissionRunner)(nil)
)

func (repetitionAdmissionFactory) Descriptor() element.Descriptor {
	return RepetitionAdmissionDescriptor()
}

func (repetitionAdmissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeRepetitionAdmissionConfig(source)
	return err
}

func (repetitionAdmissionFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeRepetitionAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.RepetitionAdmission %s config: %w", mount.InstanceID, err)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	ports, err := repetitionAdmissionPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	repeatable := make(map[string]struct{}, len(config.RepeatableTools))
	for _, name := range config.RepeatableTools {
		repeatable[name] = struct{}{}
	}
	return &repetitionAdmissionRunner{
		config: config, repeatable: repeatable,
		emit:  emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		ports: ports, resolution: mount.Resolution,
		succeeded: newSuccessfulEffectMemory(config.MaxTrackedEffects),
	}, nil
}

type repetitionAdmissionPorts struct {
	action, result                               element.InputPort
	admitted, canonicalResult, terminal, outcome element.OutputPort
	resolved                                     element.OutputPort
}

func repetitionAdmissionPortsFrom(ports element.Ports) (repetitionAdmissionPorts, error) {
	if ports == nil {
		return repetitionAdmissionPorts{}, errors.New("action.RepetitionAdmission has nil ports")
	}
	var result repetitionAdmissionPorts
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{{"action", &result.action}, {"result", &result.result}} {
		port, err := ports.Input(input.name)
		if err != nil {
			return repetitionAdmissionPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"admitted", &result.admitted}, {"canonical_result", &result.canonicalResult},
		{"terminal", &result.terminal}, {"outcome", &result.outcome},
		{"resolved", &result.resolved},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return repetitionAdmissionPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type repetitionAdmissionRunner struct {
	config     RepetitionAdmissionConfig
	repeatable map[string]struct{}
	emit       emitter
	ports      repetitionAdmissionPorts
	resolution element.ResolutionReporter
	succeeded  *successfulEffectMemory
}

func (runner *repetitionAdmissionRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.ports.resolved, Resolution{
		Stage: "repetition_admission", Reference: string(runner.config.Mode),
		Identity: "successful-semantic-effect-v1",
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, RepetitionAdmissionDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"repetition-admission", "action.RepetitionAdmission/v1",
			"policy://"+string(runner.config.Mode), 0, "",
		)}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 2)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"action", runner.ports.action}, {"result", runner.ports.result}} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "action":
				err = runner.acceptAction(ctx, input.envelope)
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown repetition admission input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *repetitionAdmissionRunner) acceptAction(
	ctx context.Context, envelope element.Envelope,
) error {
	declared, ok := declaredPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "admit", "", "invalid_payload",
			fmt.Sprintf("declared action payload has type %T", envelope.Payload))
	}
	call := callOfDeclared(declared)
	if err := validateDeclaredAction(declared); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "admit", call.CallID,
			"invalid_declaration", err.Error())
	}
	if code, err := validateActionEnvelopeIdentity(envelope, declared.Admitted); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "admit", call.CallID, code, err.Error())
	}
	if runner.config.Mode == RepetitionAdmissionAllow {
		return runner.admit(ctx, envelope, declared, call.CallID, "allowed")
	}
	if _, repeatable := runner.repeatable[call.Name]; repeatable {
		return runner.admit(ctx, envelope, declared, call.CallID, "repeatable_tool")
	}
	identity, err := repetitionEffectIdentity(declared)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "admit", call.CallID,
			"invalid_effect_identity", err.Error())
	}
	if !runner.succeeded.suppresses(identity) {
		return runner.admit(ctx, envelope, declared, call.CallID, "not_previously_succeeded")
	}
	code := "successful_effect_replay"
	message := "the same semantic effect already succeeded under this user intent"
	if runner.succeeded.saturated {
		code = "effect_memory_capacity_closed"
		message = "successful-effect memory reached its bound; protected effects are fail-closed"
	}
	terminal := PreEffectTerminal{Kind: PreEffectRepetitionSuppressed, CallID: call.CallID}
	if err := runner.publishTerminal(ctx, envelope, terminal); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "admit", call.CallID, code, message)
}

func (runner *repetitionAdmissionRunner) publishTerminal(
	ctx context.Context, cause element.Envelope, terminal PreEffectTerminal,
) error {
	envelope, err := runner.emit.envelope(cause, preEffectTerminalType, terminal, "terminal")
	if err != nil {
		return err
	}
	delivery, err := runner.ports.terminal.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered < 1 || delivery.Dropped != 0 {
		return fmt.Errorf("repetition terminal delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (runner *repetitionAdmissionRunner) admit(
	ctx context.Context, envelope element.Envelope, declared DeclaredAction, callID, code string,
) error {
	if err := publishPayload(ctx, runner.emit, runner.ports.admitted, envelope,
		declaredType, declared, "admitted"); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, OutcomeSucceeded, "admit", callID, code, "")
}

func (runner *repetitionAdmissionRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	canonical, ok := canonicalResultPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", "", "invalid_payload",
			fmt.Sprintf("canonical result payload has type %T", envelope.Payload))
	}
	result := canonical.Execution
	if err := validateExecutionResult(result); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID,
			"invalid_result", err.Error())
	}
	if strings.TrimSpace(canonical.TrajectoryItemID) == "" || canonical.StoreVersion == 0 {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID,
			"invalid_canonical_result", "canonical result requires trajectory item and store version")
	}
	declared := result.Executable.Canonical.Authorized.Confirmed.Declared
	if code, err := validateActionEnvelopeIdentity(envelope, declared.Admitted); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID, code, err.Error())
	}
	code := "error_retryable"
	_, repeatable := runner.repeatable[callOfDeclared(declared).Name]
	if result.Result.Error == "" && runner.config.Mode != RepetitionAdmissionAllow && !repeatable {
		identity, err := repetitionEffectIdentity(declared)
		if err != nil {
			return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID,
				"invalid_effect_identity", err.Error())
		}
		// This mutation intentionally precedes the result broadcast. Any observer
		// consequence activated by that forwarded safe point therefore sees replay
		// memory already established.
		if runner.succeeded.record(identity) {
			code = "success_recorded"
		} else {
			code = "success_capacity_closed"
		}
	} else if result.Result.Error == "" {
		code = "success_untracked"
	}
	if err := publishPayload(ctx, runner.emit, runner.ports.canonicalResult, envelope,
		canonicalResultType, canonical, "canonical_result"); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, OutcomeSucceeded, "result", result.CallID, code, "")
}

// successfulEffectMemory never evicts a successful effect and thereby makes
// it executable again. Once exact storage is exhausted, it closes admission
// for all protected effects on this mounted session. That conservative bit is
// bounded and preserves at-most-once safety at the cost of availability until
// the session is remounted or configured with a larger bound.
type successfulEffectMemory struct {
	limit     int
	items     map[string]struct{}
	saturated bool
}

func newSuccessfulEffectMemory(limit int) *successfulEffectMemory {
	return &successfulEffectMemory{limit: limit, items: make(map[string]struct{}, limit)}
}

// record reports whether the exact identity was retained. False means the
// fail-closed saturation bit was set instead.
func (memory *successfulEffectMemory) record(identity string) bool {
	if memory.saturated {
		return false
	}
	if _, found := memory.items[identity]; found {
		return true
	}
	if len(memory.items) >= memory.limit {
		memory.saturated = true
		return false
	}
	memory.items[identity] = struct{}{}
	return true
}

func (memory *successfulEffectMemory) suppresses(identity string) bool {
	if memory.saturated {
		return true
	}
	_, found := memory.items[identity]
	return found
}

func (runner *repetitionAdmissionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, kind OutcomeKind, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.ports.outcome, cause, Outcome{
		Kind: kind, Stage: "repetition_admission", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

// repetitionEffectIdentity deliberately excludes provider-local CallID and
// cognition RunID. Fresh proposals for the same effective action under one
// durable authority observation must converge on the same identity.
func repetitionEffectIdentity(declared DeclaredAction) (string, error) {
	call := callOfDeclared(declared)
	canonicalArguments, _, err := authority.CanonicalEffectArguments(call.Arguments)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, field := range [][]byte{
		[]byte(declared.Admitted.SessionID),
		[]byte(declared.Admitted.AuthorityItemID),
		[]byte(declared.Target),
		[]byte(call.Name),
		[]byte(declared.DeclarationDigest),
		canonicalArguments,
	} {
		_, _ = fmt.Fprintf(hash, "%d:", len(field))
		_, _ = hash.Write(field)
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}
