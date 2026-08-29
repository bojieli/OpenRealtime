package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type runtimeDependencies struct {
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator
}

func reportActionResolution(
	reporter element.ResolutionReporter, descriptor element.Descriptor,
	capabilities []element.CapabilityResolution,
) error {
	if reporter == nil {
		return fmt.Errorf("%s has no live resolution reporter", descriptor.Name)
	}
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("resolve %s runtime identity: %w", descriptor.Name, err)
	}
	if err := reporter.Runtime(
		"go://github.com/bojieli/OpenRealtime/elements/action/"+descriptor.Name,
		fmt.Sprintf("descriptor:%d", descriptor.Revision), identity.Digest,
	); err != nil {
		return err
	}
	return reporter.Capabilities(capabilities)
}

func actionCapability(
	name, contract, providerID string, serviceRevision uint64, digest string,
) element.CapabilityResolution {
	return element.CapabilityResolution{
		Name: name, Contract: contract, ProviderID: providerID,
		ProviderRevision: fmt.Sprintf("service:%d", serviceRevision), ProviderDigest: digest,
	}
}

func resolveRuntimeDependencies(services element.Services) (runtimeDependencies, error) {
	clockService, _, found := services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return runtimeDependencies{}, errors.New("action element has no runtime clock service")
	}
	clock, ok := clockService.(graphruntime.Clock)
	if !ok || reflectedNil(clock) {
		return runtimeDependencies{}, fmt.Errorf("runtime clock service has type %T", clockService)
	}
	sequenceService, _, found := services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return runtimeDependencies{}, errors.New("action element has no runtime sequence service")
	}
	sequences, ok := sequenceService.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return runtimeDependencies{}, fmt.Errorf("runtime sequence service has type %T", sequenceService)
	}
	return runtimeDependencies{clock: clock, sequences: sequences}, nil
}

type emitter struct {
	instance  string
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator
}

func (emitter emitter) envelope(parent element.Envelope, valueType element.Type, payload any, label string) (element.Envelope, error) {
	sequence, err := emitter.sequences.Next(emitter.instance + "." + label)
	if err != nil {
		return element.Envelope{}, err
	}
	result := parent.Clone()
	result.Type = valueType.Clone()
	result.ItemID = fmt.Sprintf("%s/%s/%d", emitter.instance, label, sequence)
	result.Payload = payload
	result.CausalParents = appendUnique(result.CausalParents, parent.ItemID)
	return result, nil
}

func (emitter emitter) resolution(value Resolution) (element.Envelope, error) {
	sequence, err := emitter.sequences.Next(emitter.instance + ".resolution")
	if err != nil {
		return element.Envelope{}, err
	}
	return element.Envelope{
		Type: resolutionType.Clone(), ItemID: fmt.Sprintf("%s/resolution/%d", emitter.instance, sequence),
		ReceiveNS: emitter.clock.NowNS(), Payload: value,
	}, nil
}

func broadcast(ctx context.Context, output element.OutputPort, envelope element.Envelope) error {
	_, err := output.Broadcast(ctx, envelope)
	return err
}

func publishResolution(ctx context.Context, emit emitter, output element.OutputPort, resolution Resolution) error {
	envelope, err := emit.resolution(resolution)
	if err != nil {
		return err
	}
	return broadcast(ctx, output, envelope)
}

func publishPayload(
	ctx context.Context, emit emitter, output element.OutputPort, parent element.Envelope,
	valueType element.Type, payload any, label string,
) error {
	envelope, err := emit.envelope(parent, valueType, payload, label)
	if err != nil {
		return err
	}
	return broadcast(ctx, output, envelope)
}

func publishOutcome(
	ctx context.Context, emit emitter, output element.OutputPort, parent element.Envelope, outcome Outcome,
) error {
	if outcome.FinishedNS == 0 {
		outcome.FinishedNS = emit.clock.NowNS()
	}
	return publishPayload(ctx, emit, output, parent, outcomeType, outcome, "outcome")
}

type receivedInput struct {
	kind     string
	envelope element.Envelope
}

func receiveInputs(
	ctx context.Context, kind string, port element.InputPort, destination chan<- receivedInput,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				select {
				case failures <- fmt.Errorf("receive %s: %w", kind, err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case destination <- receivedInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func proposalPayload(payload any) (cognitionelements.ToolProposal, bool) {
	switch value := payload.(type) {
	case cognitionelements.ToolProposal:
		return cloneProposal(value), true
	case *cognitionelements.ToolProposal:
		if value != nil {
			return cloneProposal(*value), true
		}
	}
	return cognitionelements.ToolProposal{}, false
}

func provenancePayload(payload any) (Provenance, bool) {
	switch value := payload.(type) {
	case Provenance:
		return value, true
	case *Provenance:
		if value != nil {
			return *value, true
		}
	}
	return Provenance{}, false
}

func admittedPayload(payload any) (AdmittedProposal, bool) {
	switch value := payload.(type) {
	case AdmittedProposal:
		return cloneAdmitted(value), true
	case *AdmittedProposal:
		if value != nil {
			return cloneAdmitted(*value), true
		}
	}
	return AdmittedProposal{}, false
}

func declaredPayload(payload any) (DeclaredAction, bool) {
	switch value := payload.(type) {
	case DeclaredAction:
		return cloneDeclared(value), true
	case *DeclaredAction:
		if value != nil {
			return cloneDeclared(*value), true
		}
	}
	return DeclaredAction{}, false
}

func confirmedPayload(payload any) (ConfirmedAction, bool) {
	switch value := payload.(type) {
	case ConfirmedAction:
		return cloneConfirmed(value), true
	case *ConfirmedAction:
		if value != nil {
			return cloneConfirmed(*value), true
		}
	}
	return ConfirmedAction{}, false
}

func authorizedPayload(payload any) (AuthorizedAction, bool) {
	switch value := payload.(type) {
	case AuthorizedAction:
		return cloneAuthorized(value), true
	case *AuthorizedAction:
		if value != nil {
			return cloneAuthorized(*value), true
		}
	}
	return AuthorizedAction{}, false
}

func executablePayload(payload any) (ExecutableAction, bool) {
	switch value := payload.(type) {
	case ExecutableAction:
		return cloneExecutable(value), true
	case *ExecutableAction:
		if value != nil {
			return cloneExecutable(*value), true
		}
	}
	return ExecutableAction{}, false
}

func interruptAddress(envelope element.Envelope) (Interrupt, string, error) {
	var interrupt Interrupt
	switch value := envelope.Payload.(type) {
	case Interrupt:
		interrupt = value
	case *Interrupt:
		if value == nil {
			return Interrupt{}, "invalid_payload", errors.New("interrupt payload is nil")
		}
		interrupt = *value
	default:
		return Interrupt{}, "invalid_payload", fmt.Errorf("interrupt payload has type %T", envelope.Payload)
	}
	interrupt.CallID = strings.TrimSpace(interrupt.CallID)
	envelopeAddress := strings.TrimSpace(envelope.RunID)
	if interrupt.CallID == "" {
		interrupt.CallID = envelopeAddress
	} else if envelopeAddress != "" && envelopeAddress != interrupt.CallID {
		return Interrupt{}, "conflicting_call_id", fmt.Errorf("interrupt call ID %q conflicts with envelope address %q",
			interrupt.CallID, envelopeAddress)
	}
	if interrupt.CallID == "" {
		return Interrupt{}, "missing_call_id", errors.New("interrupt requires a call ID")
	}
	interrupt.Reason = strings.TrimSpace(interrupt.Reason)
	return interrupt, "", nil
}

func validateToolCall(call trajectory.ToolCall) error {
	if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool call arguments must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &object); err != nil || object == nil {
		return errors.New("tool call arguments must be one JSON object")
	}
	return nil
}

func validateProposal(proposal cognitionelements.ToolProposal) error {
	if err := validateToolCall(proposal.Call); err != nil {
		return err
	}
	if !proposal.Declared {
		return errors.New("provider emitted a tool absent from its invocation declaration")
	}
	switch proposal.ProviderAuthority {
	case continuation.ToolAuthorityPropose, continuation.ToolAuthorityExecute:
		return nil
	default:
		return fmt.Errorf("provider authority %q cannot emit a tool proposal", proposal.ProviderAuthority)
	}
}

func cloneProposal(proposal cognitionelements.ToolProposal) cognitionelements.ToolProposal {
	proposal.Call = cloneToolCall(proposal.Call)
	return proposal
}

func cloneToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func cloneToolResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = slices.Clone(result.Output)
	return result
}

func cloneAdmitted(value AdmittedProposal) AdmittedProposal {
	value.Proposal = cloneProposal(value.Proposal)
	return value
}

func cloneDeclared(value DeclaredAction) DeclaredAction {
	value.Admitted = cloneAdmitted(value.Admitted)
	return value
}

func cloneConfirmed(value ConfirmedAction) ConfirmedAction {
	value.Declared = cloneDeclared(value.Declared)
	return value
}

func cloneAuthorized(value AuthorizedAction) AuthorizedAction {
	value.Confirmed = cloneConfirmed(value.Confirmed)
	return value
}

func cloneExecutable(value ExecutableAction) ExecutableAction {
	value.Authorized = cloneAuthorized(value.Authorized)
	return value
}

func callOfAdmitted(value AdmittedProposal) trajectory.ToolCall { return value.Proposal.Call }
func callOfDeclared(value DeclaredAction) trajectory.ToolCall   { return value.Admitted.Proposal.Call }
func callOfConfirmed(value ConfirmedAction) trajectory.ToolCall {
	return callOfDeclared(value.Declared)
}
func callOfAuthorized(value AuthorizedAction) trajectory.ToolCall {
	return callOfConfirmed(value.Confirmed)
}
func callOfExecutable(value ExecutableAction) trajectory.ToolCall {
	return callOfAuthorized(value.Authorized)
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

type boundedSet struct {
	limit int
	order []string
	items map[string]struct{}
}

func newBoundedSet(limit int) *boundedSet {
	return &boundedSet{limit: limit, items: make(map[string]struct{})}
}

func (set *boundedSet) add(value string) {
	if _, found := set.items[value]; found {
		return
	}
	set.items[value] = struct{}{}
	set.order = append(set.order, value)
	for len(set.order) > set.limit {
		delete(set.items, set.order[0])
		set.order = set.order[1:]
	}
}

func (set *boundedSet) contains(value string) bool {
	_, found := set.items[value]
	return found
}
