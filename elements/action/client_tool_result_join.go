package action

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

const (
	maximumClientToolResultJoinPending  = 64
	maximumClientToolResultJoinCanceled = 512
	maximumClientToolResultJoinTerminal = 4096
)

func ClientToolResultJoinDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "action.ClientToolResultJoin",
		Revision:      1,
		Ports: []element.Port{
			port("result", element.Input, resultType, 64),
			port("accepted", element.Input, clientToolResultAcceptedType, 64),
			port("cancel", element.Input, interruptType, 32),
			port("joined", element.Output, resultType, 64),
			port("outcome", element.Output, outcomeType, 64),
		},
		Reaction: element.Reaction{
			Triggers: []string{"result", "accepted"}, Interrupts: []string{"cancel"},
			Outcomes: []string{"joined", "outcome"}, MaxConcurrency: 1, BreaksCycles: true,
		},
		Dependencies: []element.Dependency{
			{Name: LedgerRegistryService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "action.client-tool-result.join", Reversible: true}},
	}
}

type clientToolResultJoinFactory struct{}

func (clientToolResultJoinFactory) Descriptor() element.Descriptor {
	return ClientToolResultJoinDescriptor()
}

func (clientToolResultJoinFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	ledgerService, ledgerRevision, found := mount.Services.Lookup(LedgerRegistryService)
	if !found {
		return nil, fmt.Errorf("action.ClientToolResultJoin %s has no ledger registries service", mount.InstanceID)
	}
	ledgers, ok := ledgerService.(*LedgerRegistries)
	if !ok || ledgers == nil {
		return nil, fmt.Errorf("client tool-result join ledger registries service has type %T", ledgerService)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	result, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	accepted, err := mount.Ports.Input("accepted")
	if err != nil {
		return nil, err
	}
	cancel, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	joined, err := mount.Ports.Output("joined")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &clientToolResultJoinRunner{
		ledgers: ledgers, ledgerRevision: ledgerRevision,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		resultInput: result, acceptedInput: accepted, cancelInput: cancel,
		joinedOutput: joined, outcomeOutput: outcome,
		results:          make(map[string]pendingClientExecutionResult),
		accepted:         make(map[string]pendingClientToolResultAccepted),
		canceled:         newBoundedSet(maximumClientToolResultJoinCanceled),
		terminal:         newExactClientToolResultSet(maximumClientToolResultJoinTerminal),
		terminalResults:  make(map[string]terminalClientExecutionResult),
		terminalAccepted: make(map[string]*pendingClientToolResultAccepted),
		resolution:       mount.Resolution,
	}, nil
}

type pendingClientExecutionResult struct {
	envelope element.Envelope
	result   ExecutionResult
}

type pendingClientToolResultAccepted struct {
	envelope element.Envelope
	accepted ClientToolResultAccepted
}

type terminalClientExecutionResult struct {
	envelope         element.Envelope
	resultCapability string
	origin           CompletionOrigin
}

type clientToolResultJoinRunner struct {
	ledgers          *LedgerRegistries
	ledgerRevision   uint64
	emit             emitter
	resultInput      element.InputPort
	acceptedInput    element.InputPort
	cancelInput      element.InputPort
	joinedOutput     element.OutputPort
	outcomeOutput    element.OutputPort
	results          map[string]pendingClientExecutionResult
	accepted         map[string]pendingClientToolResultAccepted
	canceled         *boundedSet
	terminal         *exactClientToolResultSet
	terminalResults  map[string]terminalClientExecutionResult
	terminalAccepted map[string]*pendingClientToolResultAccepted
	resolution       element.ResolutionReporter
}

func (runner *clientToolResultJoinRunner) Run(parent context.Context) error {
	if err := reportActionResolution(runner.resolution, ClientToolResultJoinDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"ledger", "action.Ledger/v1", "registry://"+LedgerRegistryService,
			runner.ledgerRevision, "",
		)}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	inputs := make(chan receivedInput)
	failures := make(chan error, 3)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"result", runner.resultInput}, {"accepted", runner.acceptedInput}, {"cancel", runner.cancelInput}} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	defer func() { cancel(); receivers.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			case "accepted":
				err = runner.acceptAttestation(ctx, input.envelope)
			case "cancel":
				err = runner.interrupt(ctx, input.envelope)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *clientToolResultJoinRunner) acceptResult(ctx context.Context, envelope element.Envelope) error {
	result, ok := executionResultPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "result", Code: "invalid_payload",
			Message: fmt.Sprintf("execution result payload has type %T", envelope.Payload)})
	}
	if err := validateExecutionResult(result); err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
			Code: "invalid_result", Message: err.Error()})
	}
	if err := runner.verifyResult(result); err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
			Code: "invalid_capability", Message: err.Error()})
	}
	if err := validateExecutionResultOriginSemantics(result); err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
			Code: "invalid_result_origin", Message: err.Error()})
	}
	identity, code, err := clientExecutionResultIdentity(envelope, result)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
			Code: code, Message: err.Error()})
	}
	if runner.terminal.contains(identity) {
		if existing, found := runner.terminalResults[identity]; found &&
			sameTerminalExecutionResultEnvelope(existing, envelope, result) {
			return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
				Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
				Code: "terminal_replay", Message: "exact terminal execution result was replayed"})
		}
		return runner.poison(ctx, identity, envelope, result.CallID, "terminal_result_conflict",
			"terminal scoped call received different execution-result evidence")
	}
	if runner.terminal.full() {
		return errors.New("client tool-result join exhausted exact terminal memory")
	}
	if existing, duplicate := runner.results[identity]; duplicate {
		if sameExecutionResultEnvelope(existing, envelope, result) {
			return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
				Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
				Code: "duplicate_replay", Message: "exact pending execution result was replayed"})
		}
		return runner.poison(ctx, identity, envelope, result.CallID, "conflicting_result",
			"scoped call produced conflicting execution results")
	}
	if result.CompletionOrigin == CompletionDispatcherError {
		if _, clientAccepted := runner.accepted[identity]; clientAccepted {
			return runner.poison(ctx, identity, envelope, result.CallID, "accepted_dispatcher_error",
				"dispatcher synthesized an error after accepting a client result")
		}
		if err := runner.emitJoined(ctx, identity, pendingClientExecutionResult{
			envelope: envelope.Clone(), result: result,
		}, nil); err != nil {
			return err
		}
		return nil
	}
	if _, hasCounterpart := runner.accepted[identity]; !hasCounterpart &&
		runner.pendingCount() >= maximumClientToolResultJoinPending {
		return errors.New("client tool-result join pending capacity exhausted before canonical commit")
	}
	runner.results[identity] = pendingClientExecutionResult{envelope: envelope.Clone(), result: result}
	if accepted, found := runner.accepted[identity]; found {
		return runner.emitJoined(ctx, identity, runner.results[identity], &accepted)
	}
	return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
		Stage: "client_tool_result_join", Operation: "result", CallID: result.CallID,
		Code: "awaiting_accepted", Message: "returned dispatch result awaits exact client ingress evidence"})
}

func (runner *clientToolResultJoinRunner) acceptAttestation(ctx context.Context, envelope element.Envelope) error {
	accepted, ok := clientToolResultAcceptedPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "accepted", Code: "invalid_payload",
			Message: fmt.Sprintf("client result attestation payload has type %T", envelope.Payload)})
	}
	identity, code, err := clientToolResultAcceptedIdentity(envelope, accepted)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "accepted", CallID: accepted.Receipt.CallID,
			Code: code, Message: err.Error()})
	}
	if runner.terminal.contains(identity) {
		result, resultFound := runner.terminalResults[identity]
		terminalAccepted, acceptedFound := runner.terminalAccepted[identity]
		if resultFound && result.origin == CompletionDispatcherError {
			return runner.poison(ctx, identity, envelope, accepted.Receipt.CallID,
				"accepted_after_dispatcher_error",
				"client acceptance arrived after a dispatcher-generated result was joined")
		}
		if acceptedFound && terminalAccepted != nil &&
			sameClientAcceptedEnvelope(*terminalAccepted, envelope, accepted) {
			return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
				Stage: "client_tool_result_join", Operation: "accepted", CallID: accepted.Receipt.CallID,
				Code: "terminal_replay", Message: "exact terminal client attestation was replayed"})
		}
		return runner.poison(ctx, identity, envelope, accepted.Receipt.CallID,
			"terminal_accepted_conflict",
			"terminal scoped call received different client-ingress evidence")
	}
	if runner.terminal.full() {
		return errors.New("client tool-result join exhausted exact terminal memory")
	}
	if existing, duplicate := runner.accepted[identity]; duplicate {
		if sameClientAcceptedEnvelope(existing, envelope, accepted) {
			return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
				Stage: "client_tool_result_join", Operation: "accepted", CallID: accepted.Receipt.CallID,
				Code: "duplicate_replay", Message: "exact pending client attestation was replayed"})
		}
		return runner.poison(ctx, identity, envelope, accepted.Receipt.CallID, "conflicting_accepted",
			"scoped call produced conflicting client ingress attestations")
	}
	if _, hasCounterpart := runner.results[identity]; !hasCounterpart &&
		runner.pendingCount() >= maximumClientToolResultJoinPending {
		return errors.New("client tool-result join pending capacity exhausted after API Submit")
	}
	runner.accepted[identity] = pendingClientToolResultAccepted{envelope: envelope.Clone(), accepted: accepted}
	if result, found := runner.results[identity]; found {
		acceptedEvidence := runner.accepted[identity]
		return runner.emitJoined(ctx, identity, result, &acceptedEvidence)
	}
	return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
		Stage: "client_tool_result_join", Operation: "accepted", CallID: accepted.Receipt.CallID,
		Code: "awaiting_result", Message: "accepted client result awaits ledger-authenticated dispatch completion",
		ResultDigest: accepted.Receipt.ResultDigest, IngressItemID: accepted.IngressItemID,
		AcceptedItemID: envelope.ItemID})
}

func (runner *clientToolResultJoinRunner) emitJoined(ctx context.Context, identity string,
	result pendingClientExecutionResult, accepted *pendingClientToolResultAccepted) error {
	if result.result.CompletionOrigin == CompletionReturned {
		if accepted == nil {
			return errors.New("returned dispatch result cannot join without client acceptance")
		}
		if result.envelope.ItemID == accepted.envelope.ItemID {
			return runner.poison(ctx, identity, result.envelope, result.result.CallID,
				"evidence_item_collision", "execution result and client acceptance share one immutable item ID")
		}
		if err := attestClientResultJoin(result.result, accepted.accepted); err != nil {
			return runner.poison(ctx, identity, result.envelope, result.result.CallID,
				"attestation_mismatch", err.Error())
		}
	}
	cause := result.envelope.Clone()
	cause.CausalParents = nil
	envelope, err := runner.emit.envelope(cause, resultType, cloneExecutionResult(result.result), "joined")
	if err != nil {
		return err
	}
	envelope.CausalParents = []string{result.envelope.ItemID}
	if accepted != nil {
		envelope.CausalParents = append(envelope.CausalParents,
			accepted.envelope.ItemID, accepted.accepted.IngressItemID)
	}
	if err := broadcastClientToolResultExact(ctx, runner.joinedOutput, envelope, "joined execution result"); err != nil {
		return err
	}
	terminalEnvelope := result.envelope.Clone()
	terminalEnvelope.Payload = nil
	runner.terminalResults[identity] = terminalClientExecutionResult{
		envelope: terminalEnvelope, resultCapability: result.result.ResultCapability,
		origin: result.result.CompletionOrigin,
	}
	if accepted != nil {
		acceptedEnvelope := accepted.envelope.Clone()
		acceptedEnvelope.Payload = nil
		copy := pendingClientToolResultAccepted{
			envelope: acceptedEnvelope, accepted: accepted.accepted,
		}
		runner.terminalAccepted[identity] = &copy
	} else {
		runner.terminalAccepted[identity] = nil
	}
	delete(runner.results, identity)
	delete(runner.accepted, identity)
	runner.terminal.add(identity)
	code := "dispatcher_error_joined"
	outcome := Outcome{Kind: OutcomeSucceeded, Stage: "client_tool_result_join", Operation: "join",
		CallID: result.result.CallID, Code: code}
	outcomeCause := result.envelope.Clone()
	outcomeCause.CausalParents = appendUnique(outcomeCause.CausalParents, envelope.ItemID)
	if accepted != nil {
		code = "client_result_joined"
		outcome.Code = code
		outcome.ResultDigest = accepted.accepted.Receipt.ResultDigest
		outcome.IngressItemID = accepted.accepted.IngressItemID
		outcome.AcceptedItemID = accepted.envelope.ItemID
		outcomeCause.CausalParents = appendUnique(outcomeCause.CausalParents, accepted.envelope.ItemID)
		outcomeCause.CausalParents = appendUnique(outcomeCause.CausalParents, accepted.accepted.IngressItemID)
	}
	return runner.publishOutcome(ctx, outcomeCause, outcome)
}

func (runner *clientToolResultJoinRunner) interrupt(ctx context.Context, envelope element.Envelope) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "cancel", CallID: interrupt.CallID,
			Code: code, Message: err.Error()})
	}
	identity, code, err := clientToolResultIdentity(envelope, interrupt.CallID)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result_join", Operation: "cancel", CallID: interrupt.CallID,
			Code: code, Message: err.Error()})
	}
	runner.canceled.add(identity)
	return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
		Stage: "client_tool_result_join", Operation: "cancel", CallID: interrupt.CallID,
		Code: "crossed_result_still_canonical", Message: interrupt.Reason,
		Crossed: runner.results[identity].envelope.ItemID != "" || runner.accepted[identity].envelope.ItemID != ""})
}

func (runner *clientToolResultJoinRunner) poison(ctx context.Context, identity string,
	cause element.Envelope, callID, code, message string) error {
	evidence := cause.Clone()
	if result, found := runner.terminalResults[identity]; found {
		evidence.CausalParents = appendUnique(evidence.CausalParents, result.envelope.ItemID)
		for _, parent := range result.envelope.CausalParents {
			evidence.CausalParents = appendUnique(evidence.CausalParents, parent)
		}
	}
	if accepted, found := runner.terminalAccepted[identity]; found && accepted != nil {
		evidence.CausalParents = appendUnique(evidence.CausalParents, accepted.envelope.ItemID)
		evidence.CausalParents = appendUnique(evidence.CausalParents, accepted.accepted.IngressItemID)
		for _, parent := range accepted.envelope.CausalParents {
			evidence.CausalParents = appendUnique(evidence.CausalParents, parent)
		}
	}
	if result, found := runner.results[identity]; found {
		evidence.CausalParents = appendUnique(evidence.CausalParents, result.envelope.ItemID)
		for _, parent := range result.envelope.CausalParents {
			evidence.CausalParents = appendUnique(evidence.CausalParents, parent)
		}
	}
	if accepted, found := runner.accepted[identity]; found {
		evidence.CausalParents = appendUnique(evidence.CausalParents, accepted.envelope.ItemID)
		evidence.CausalParents = appendUnique(evidence.CausalParents, accepted.accepted.IngressItemID)
		for _, parent := range accepted.envelope.CausalParents {
			evidence.CausalParents = appendUnique(evidence.CausalParents, parent)
		}
	}
	delete(runner.results, identity)
	delete(runner.accepted, identity)
	runner.terminal.add(identity)
	if err := runner.publishOutcome(ctx, evidence, Outcome{Kind: OutcomeFailed,
		Stage: "client_tool_result_join", Operation: "join", CallID: callID,
		Code: code, Message: message}); err != nil {
		return err
	}
	return fmt.Errorf("client tool-result join poisoned %s: %s", callID, message)
}

func (runner *clientToolResultJoinRunner) publishOutcome(ctx context.Context,
	cause element.Envelope, outcome Outcome) error {
	if outcome.FinishedNS == 0 {
		outcome.FinishedNS = runner.emit.clock.NowNS()
	}
	envelope, err := runner.emit.envelope(cause, outcomeType, outcome, "outcome")
	if err != nil {
		return err
	}
	return broadcastClientToolResultExact(ctx, runner.outcomeOutput, envelope, "join outcome")
}

func (runner *clientToolResultJoinRunner) verifyResult(result ExecutionResult) error {
	entry, err := runner.ledgers.resolve(result.Executable.LedgerReference)
	if err != nil {
		return err
	}
	if !entry.verifyResult(result) {
		return errors.New("execution result capability did not verify")
	}
	return nil
}

func (runner *clientToolResultJoinRunner) pendingCount() int {
	return len(runner.results) + len(runner.accepted)
}

func clientExecutionResultIdentity(envelope element.Envelope, result ExecutionResult) (string, string, error) {
	admitted := result.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	if envelope.SessionID != admitted.SessionID || envelope.RunID != admitted.ModelRunID ||
		envelope.CancellationScope != admitted.ModelRunID ||
		envelope.OpportunityID != admitted.ActivationItemID ||
		!canonicalClientToolResultIdentity(envelope.ItemID) {
		return "", "execution_address", errors.New("execution result envelope differs from its authorized session or run")
	}
	return actionScopeKey(admitted.SessionID, admitted.ModelRunID, result.CallID), "", nil
}

func clientToolResultAcceptedIdentity(envelope element.Envelope,
	accepted ClientToolResultAccepted) (string, string, error) {
	receipt := accepted.Receipt
	if !canonicalClientToolResultIdentity(receipt.ID) ||
		!canonicalClientToolResultIdentity(receipt.SessionID) ||
		!canonicalClientToolResultIdentity(receipt.RunID) ||
		!canonicalClientToolResultIdentity(receipt.CallID) ||
		!canonicalClientToolResultIdentity(receipt.Name) ||
		!canonicalClientToolResultIdentity(receipt.CommitmentID) ||
		!canonicalClientToolResultIdentity(receipt.CanonicalCallItemID) ||
		!canonicalClientToolResultIdentity(receipt.ResultDigest) ||
		envelope.SessionID != receipt.SessionID || envelope.RunID != receipt.RunID ||
		!canonicalClientToolResultIdentity(envelope.ItemID) ||
		!canonicalClientToolResultIdentity(accepted.IngressItemID) ||
		accepted.IngressItemID == envelope.ItemID ||
		envelope.CancellationScope != receipt.RunID || envelope.OpportunityID != receipt.CallID ||
		!slices.Contains(envelope.CausalParents, accepted.IngressItemID) {
		return "", "accepted_address", errors.New("client result attestation has incomplete or crossed causal identity")
	}
	return actionScopeKey(receipt.SessionID, receipt.RunID, receipt.CallID), "", nil
}

func attestClientResultJoin(result ExecutionResult, accepted ClientToolResultAccepted) error {
	receipt := accepted.Receipt
	canonicalCall := result.Executable.Canonical
	admitted := canonicalCall.Authorized.Confirmed.Declared.Admitted
	if receipt.SessionID != admitted.SessionID || receipt.RunID != admitted.ModelRunID ||
		receipt.CallID != result.CallID || receipt.Name != result.Name ||
		receipt.CommitmentID != result.CommitmentID ||
		receipt.CanonicalCallItemID != canonicalCall.TrajectoryItemID ||
		receipt.ResultDigest != ClientToolResultDigest(result.Result) {
		return errors.New("accepted client result differs from ledger-authenticated dispatch completion")
	}
	return nil
}

func clientToolResultAcceptedPayload(payload any) (ClientToolResultAccepted, bool) {
	switch value := payload.(type) {
	case ClientToolResultAccepted:
		return value, true
	case *ClientToolResultAccepted:
		if value != nil {
			return *value, true
		}
	}
	return ClientToolResultAccepted{}, false
}

func sameExecutionResultEnvelope(existing pendingClientExecutionResult,
	envelope element.Envelope, result ExecutionResult) bool {
	return sameClientToolResultEnvelopeAddress(existing.envelope, envelope) &&
		existing.result.ResultCapability == result.ResultCapability
}

func sameTerminalExecutionResultEnvelope(existing terminalClientExecutionResult,
	envelope element.Envelope, result ExecutionResult) bool {
	return sameClientToolResultEnvelopeAddress(existing.envelope, envelope) &&
		existing.resultCapability == result.ResultCapability && existing.origin == result.CompletionOrigin
}

func sameClientAcceptedEnvelope(existing pendingClientToolResultAccepted,
	envelope element.Envelope, accepted ClientToolResultAccepted) bool {
	return sameClientToolResultEnvelopeAddress(existing.envelope, envelope) && existing.accepted == accepted
}

func sameClientToolResultEnvelopeAddress(left, right element.Envelope) bool {
	return left.Type.Equal(right.Type) && left.ItemID == right.ItemID &&
		left.SessionID == right.SessionID && left.RunID == right.RunID &&
		left.SourceID == right.SourceID && left.OpportunityID == right.OpportunityID &&
		left.Sequence == right.Sequence && left.CaptureNS == right.CaptureNS &&
		left.ReceiveNS == right.ReceiveNS && left.TraceID == right.TraceID &&
		left.CancellationScope == right.CancellationScope &&
		slices.Equal(left.CausalParents, right.CausalParents)
}

var _ element.Factory = clientToolResultJoinFactory{}
var _ element.Runnable = (*clientToolResultJoinRunner)(nil)
