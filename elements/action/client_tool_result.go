package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	ClientToolResultRendezvousService = "action.client-tool-result.rendezvous"
	maximumClientToolResultPending    = 64
	maximumClientToolResultBytes      = 1 << 20
	maximumClientToolResultCanceled   = 512
	maximumClientToolResultTerminal   = 4096
)

// ErrClientToolResultNotAccepted certifies that Submit did not deliver any
// result bytes, so a corrected request may retry the same scoped call.
var ErrClientToolResultNotAccepted = errors.New("client tool result not accepted")

var (
	clientToolResultType         = element.Trigger(element.Named("action.ClientToolResult"))
	clientToolResultAcceptedType = element.Stream(element.Named("action.ClientToolResultAccepted"))
)

func ClientToolResultType() element.Type         { return clientToolResultType.Clone() }
func ClientToolResultAcceptedType() element.Type { return clientToolResultAcceptedType.Clone() }

// ClientToolResultReceipt is deployment-owned admission evidence returned by
// a session-scoped rendezvous. CommitmentID and CanonicalCallItemID bind the
// client bytes to the exact ledger-authorized call later presented to Join.
type ClientToolResultReceipt struct {
	ID                  string `json:"id"`
	SessionID           string `json:"session_id"`
	RunID               string `json:"run_id"`
	CallID              string `json:"call_id"`
	Name                string `json:"name"`
	CommitmentID        string `json:"commitment_id"`
	CanonicalCallItemID string `json:"canonical_call_item_id"`
	ResultDigest        string `json:"result_digest"`
}

// ClientToolResultCanonical is the later durable safe-point evidence. Submit
// success is not reported to the stable API as durable completion.
type ClientToolResultCanonical struct {
	Receipt                   ClientToolResultReceipt `json:"receipt"`
	CanonicalEnvelopeItemID   string                  `json:"canonical_envelope_item_id"`
	CanonicalTrajectoryItemID string                  `json:"canonical_trajectory_item_id"`
	StoreVersion              uint64                  `json:"store_version"`
}

// ClientToolResultAccepted is graph-visible evidence for the exact stable API
// ingress item. A later Join must match it before CompletionReturned can reach
// ToolResultCommit.
type ClientToolResultAccepted struct {
	Receipt       ClientToolResultReceipt `json:"receipt"`
	IngressItemID string                  `json:"ingress_item_id"`
}

// ClientToolResultRendezvous is supplied by a session-scoped transport plugin.
// Submit is the atomic linearization point: it either returns NotAccepted
// without delivery, or returns an exact receipt even if its caller context
// races after acceptance. Wait must honor mount-owned context cancellation.
type ClientToolResultRendezvous interface {
	SubmitToolResult(context.Context, string, string, trajectory.ToolResult) (ClientToolResultReceipt, error)
	WaitToolResultCanonical(context.Context, ClientToolResultReceipt) (ClientToolResultCanonical, error)
}

func ClientToolResultDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "action.ClientToolResultIngress",
		Revision:      2,
		Ports: []element.Port{
			port("result", element.Input, clientToolResultType, 64),
			port("cancel", element.Input, interruptType, 32),
			port("accepted", element.Output, clientToolResultAcceptedType, 64),
			port("outcome", element.Output, outcomeType, 64),
		},
		Reaction: element.Reaction{
			Triggers: []string{"result"}, Interrupts: []string{"cancel"},
			Outcomes: []string{"accepted", "outcome"}, MaxConcurrency: maximumClientToolResultPending,
			BreaksCycles: true,
		},
		Dependencies: []element.Dependency{
			{Name: ClientToolResultRendezvousService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "action.client-tool-result.pending", Reversible: true}},
	}
}

type clientToolResultFactory struct{}

func (clientToolResultFactory) Descriptor() element.Descriptor { return ClientToolResultDescriptor() }

func (clientToolResultFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	service, revision, found := mount.Services.Lookup(ClientToolResultRendezvousService)
	if !found {
		return nil, fmt.Errorf("action.ClientToolResultIngress %s has no rendezvous service", mount.InstanceID)
	}
	rendezvous, ok := service.(ClientToolResultRendezvous)
	if !ok || rendezvous == nil {
		return nil, fmt.Errorf("client tool-result rendezvous service has type %T", service)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	result, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	cancel, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	accepted, err := mount.Ports.Output("accepted")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &clientToolResultRunner{
		rendezvous: rendezvous, serviceRevision: revision,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		resultInput: result, cancelInput: cancel, acceptedOutput: accepted, outcomeOutput: outcome,
		submitting: make(map[string]*clientToolResultJob), pending: make(map[string]*clientToolResultJob),
		preCanceled:     make(map[string]element.Envelope),
		terminal:        newExactClientToolResultSet(maximumClientToolResultTerminal),
		terminalDigests: make(map[string]string), itemEvidence: make(map[string]clientToolResultItemEvidence),
		resolution: mount.Resolution,
	}, nil
}

type clientToolResultItemEvidence struct {
	kind     string
	identity string
	digest   string
	envelope element.Envelope
}

type clientToolResultJob struct {
	identity       string
	cause          element.Envelope
	result         trajectory.ToolResult
	digest         string
	receipt        ClientToolResultReceipt
	acceptedItemID string
	cancelCause    *element.Envelope
	cancel         context.CancelCauseFunc
}

type clientToolResultCompletion struct {
	phase     string
	job       *clientToolResultJob
	receipt   ClientToolResultReceipt
	canonical ClientToolResultCanonical
	err       error
}

type clientToolResultRunner struct {
	rendezvous      ClientToolResultRendezvous
	serviceRevision uint64
	emit            emitter
	resultInput     element.InputPort
	cancelInput     element.InputPort
	acceptedOutput  element.OutputPort
	outcomeOutput   element.OutputPort
	submitting      map[string]*clientToolResultJob
	pending         map[string]*clientToolResultJob
	preCanceled     map[string]element.Envelope
	preCancelClosed bool
	terminal        *exactClientToolResultSet
	terminalDigests map[string]string
	itemEvidence    map[string]clientToolResultItemEvidence
	resolution      element.ResolutionReporter
}

func (runner *clientToolResultRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	if err := reportActionResolution(runner.resolution, ClientToolResultDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"client-tool-result-rendezvous", "action.ClientToolResultRendezvous/v2",
			"service://"+ClientToolResultRendezvousService, runner.serviceRevision, "",
		)}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 2)
	completions := make(chan clientToolResultCompletion, maximumClientToolResultPending)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"result", runner.resultInput}, {"cancel", runner.cancelInput}} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	defer func() {
		stop(nil)
		for _, job := range runner.submitting {
			job.cancel(context.Canceled)
		}
		for _, job := range runner.pending {
			job.cancel(context.Canceled)
		}
		receivers.Wait()
		// Do not let a broken plugin that ignores Wait context hang unmount.
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			if input.kind == "result" {
				err = runner.accept(ctx, input.envelope, completions)
			} else {
				err = runner.interrupt(ctx, input.envelope)
			}
			if err != nil {
				return err
			}
		case completion := <-completions:
			if err := runner.complete(ctx, completion, completions); err != nil {
				return err
			}
		}
	}
}

func (runner *clientToolResultRunner) accept(
	ctx context.Context, envelope element.Envelope, completions chan<- clientToolResultCompletion,
) error {
	result, ok := clientToolResultPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", Code: "invalid_payload",
			Message: fmt.Sprintf("client result payload has type %T", envelope.Payload)})
	}
	if err := validateClientToolResult(result); err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "invalid_result", Message: err.Error()})
	}
	identity, code, err := clientToolResultIdentity(envelope, result.CallID)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: code, Message: err.Error()})
	}
	digest := ClientToolResultDigest(result)
	if err := runner.rememberItem(envelope, clientToolResultItemEvidence{
		kind: "result", identity: identity, digest: digest, envelope: envelope.Clone(),
	}); err != nil {
		code := "item_evidence_conflict"
		kind := OutcomeFailed
		if errors.Is(err, errClientToolResultItemCapacity) {
			code, kind = "item_capacity", OutcomeRejected
		}
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: kind,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: code, Message: err.Error(),
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	if runner.terminal.contains(identity) {
		if runner.terminalDigests[identity] != digest {
			return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeFailed,
				Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
				Code: "result_conflict", Message: "terminal call received different client result bytes",
				ResultDigest: digest, IngressItemID: envelope.ItemID})
		}
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "terminal_replay", Message: "client result is already terminal",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	if len(runner.terminal.values)+len(runner.submitting)+len(runner.pending) >=
		maximumClientToolResultTerminal {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "terminal_capacity", Message: "client result ingress exhausted its exact replay memory",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	if cancelCause, canceled := runner.preCanceled[identity]; canceled {
		delete(runner.preCanceled, identity)
		runner.markTerminal(identity, digest)
		cause := envelope.Clone()
		if canceled {
			cause.CausalParents = appendUnique(cause.CausalParents, cancelCause.ItemID)
		}
		return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeCanceled,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "pre_canceled", Message: "client result was canceled before rendezvous admission",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	if runner.preCancelClosed {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code:         "cancel_capacity_fail_closed",
			Message:      "cancellation memory is saturated; admission fails closed without claiming an exact cancel",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	if duplicate := runner.submitting[identity]; duplicate != nil {
		return runner.pendingReplayOutcome(ctx, envelope, result, digest, duplicate)
	}
	if duplicate := runner.pending[identity]; duplicate != nil {
		return runner.pendingReplayOutcome(ctx, envelope, result, digest, duplicate)
	}
	if len(runner.submitting)+len(runner.pending) >= maximumClientToolResultPending {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "capacity", Message: "client result pending capacity is exhausted",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	jobCtx, cancel := context.WithCancelCause(ctx)
	job := &clientToolResultJob{identity: identity, cause: envelope.Clone(), result: cloneToolResult(result),
		digest: digest, cancel: cancel}
	runner.submitting[identity] = job
	go func() {
		receipt, submitErr := runner.rendezvous.SubmitToolResult(
			jobCtx, envelope.SessionID, envelope.RunID, cloneToolResult(result),
		)
		select {
		case completions <- clientToolResultCompletion{phase: "submit", job: job, receipt: receipt, err: submitErr}:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (runner *clientToolResultRunner) interrupt(ctx context.Context, envelope element.Envelope) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: code, Message: err.Error()})
	}
	identity, code, err := clientToolResultIdentity(envelope, interrupt.CallID)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: code, Message: err.Error()})
	}
	if err := runner.rememberItem(envelope, clientToolResultItemEvidence{
		kind: "cancel", identity: identity, digest: interrupt.Reason, envelope: envelope.Clone(),
	}); err != nil {
		code := "item_evidence_conflict"
		kind := OutcomeFailed
		if errors.Is(err, errClientToolResultItemCapacity) {
			code, kind = "item_capacity", OutcomeRejected
		}
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: kind,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: code, Message: err.Error()})
	}
	if job := runner.submitting[identity]; job != nil {
		if job.cancelCause == nil {
			copy := envelope.Clone()
			job.cancelCause = &copy
			job.cancel(fmt.Errorf("%w: %s", ErrClientToolResultNotAccepted, interrupt.Reason))
		}
		cause := envelope.Clone()
		cause.CausalParents = appendUnique(cause.CausalParents, job.cause.ItemID)
		cause.CausalParents = appendUnique(cause.CausalParents, job.cancelCause.ItemID)
		return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: "cancellation_requested_before_accept", Message: interrupt.Reason,
			ResultDigest: job.digest, IngressItemID: job.cause.ItemID})
	}
	if job := runner.pending[identity]; job != nil {
		cause := envelope.Clone()
		cause.CausalParents = appendUnique(cause.CausalParents, job.cause.ItemID)
		cause.CausalParents = appendUnique(cause.CausalParents, job.acceptedItemID)
		return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: "accepted_cannot_rollback", Message: interrupt.Reason,
			ResultDigest: job.receipt.ResultDigest, IngressItemID: job.cause.ItemID,
			AcceptedItemID: job.acceptedItemID})
	}
	if runner.terminal.contains(identity) {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: "already_terminal", Message: interrupt.Reason})
	}
	if existing, duplicate := runner.preCanceled[identity]; duplicate {
		cause := envelope.Clone()
		cause.CausalParents = appendUnique(cause.CausalParents, existing.ItemID)
		return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code: "already_pre_canceled", Message: interrupt.Reason})
	}
	if runner.preCancelClosed || len(runner.preCanceled) >= maximumClientToolResultCanceled {
		runner.preCancelClosed = true
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeRejected,
			Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
			Code:    "cancel_capacity",
			Message: "client result cancellation memory is exhausted; future admissions fail closed"})
	}
	runner.preCanceled[identity] = envelope.Clone()
	return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeCanceled,
		Stage: "client_tool_result", Operation: "cancel", CallID: interrupt.CallID,
		Code: "pre_canceled", Message: interrupt.Reason})
}

func (runner *clientToolResultRunner) complete(ctx context.Context,
	completion clientToolResultCompletion, completions chan<- clientToolResultCompletion) error {
	if completion.phase == "submit" {
		return runner.completeSubmission(ctx, completion, completions)
	}
	return runner.completeCanonical(ctx, completion)
}

func (runner *clientToolResultRunner) completeSubmission(
	ctx context.Context, completion clientToolResultCompletion,
	completions chan<- clientToolResultCompletion,
) error {
	job := completion.job
	if runner.submitting[job.identity] != job {
		return errors.New("client tool-result submission has no exact pending job")
	}
	delete(runner.submitting, job.identity)
	job.cancel(nil)
	if completion.err != nil {
		cause := job.cause.Clone()
		if job.cancelCause != nil {
			cause.CausalParents = appendUnique(cause.CausalParents, job.cancelCause.ItemID)
		}
		if errors.Is(completion.err, ErrClientToolResultNotAccepted) {
			kind, code := OutcomeRejected, "not_accepted"
			if job.cancelCause != nil {
				kind, code = OutcomeCanceled, "pre_canceled"
				runner.markTerminal(job.identity, job.digest)
			}
			return runner.publishOutcome(ctx, cause, Outcome{Kind: kind,
				Stage: "client_tool_result", Operation: "result", CallID: job.result.CallID,
				Code: code, Message: completion.err.Error(), ResultDigest: job.digest,
				IngressItemID: job.cause.ItemID})
		}
		runner.markTerminal(job.identity, job.digest)
		return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeFailed,
			Stage: "client_tool_result", Operation: "result", CallID: job.result.CallID,
			Code: "rendezvous_failed", Message: completion.err.Error(), ResultDigest: job.digest,
			IngressItemID: job.cause.ItemID})
	}
	if err := validateClientToolResultReceipt(
		completion.receipt, job.cause.SessionID, job.cause.RunID, job.result, job.digest,
	); err != nil {
		return fmt.Errorf("client tool-result rendezvous returned invalid receipt: %w", err)
	}
	job.receipt = completion.receipt
	accepted := ClientToolResultAccepted{Receipt: job.receipt, IngressItemID: job.cause.ItemID}
	acceptedCause := job.cause.Clone()
	if job.cancelCause != nil {
		acceptedCause.CausalParents = appendUnique(acceptedCause.CausalParents, job.cancelCause.ItemID)
	}
	acceptedEnvelope, err := runner.emit.envelope(acceptedCause, clientToolResultAcceptedType, accepted, "accepted")
	if err != nil {
		return err
	}
	if err := broadcastClientToolResultExact(ctx, runner.acceptedOutput, acceptedEnvelope, "accepted attestation"); err != nil {
		return err
	}
	job.acceptedItemID = acceptedEnvelope.ItemID
	waitCtx, waitCancel := context.WithCancelCause(ctx)
	job.cancel = waitCancel
	runner.pending[job.identity] = job
	go func() {
		canonical, waitErr := runner.rendezvous.WaitToolResultCanonical(waitCtx, job.receipt)
		select {
		case completions <- clientToolResultCompletion{
			phase: "canonical", job: job, canonical: canonical, err: waitErr,
		}:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (runner *clientToolResultRunner) completeCanonical(
	ctx context.Context, completion clientToolResultCompletion,
) error {
	job := completion.job
	if runner.pending[job.identity] != job {
		return errors.New("client tool-result canonical completion has no exact pending job")
	}
	delete(runner.pending, job.identity)
	job.cancel(nil)
	runner.markTerminal(job.identity, job.digest)
	cause := job.cause.Clone()
	cause.CausalParents = appendUnique(cause.CausalParents, job.acceptedItemID)
	if job.cancelCause != nil {
		cause.CausalParents = appendUnique(cause.CausalParents, job.cancelCause.ItemID)
	}
	outcome := Outcome{Kind: OutcomeSucceeded, Stage: "client_tool_result", Operation: "result",
		CallID: job.result.CallID, ResultDigest: job.receipt.ResultDigest,
		IngressItemID: job.cause.ItemID, AcceptedItemID: job.acceptedItemID}
	if completion.err != nil {
		outcome.Kind, outcome.Code, outcome.Message = OutcomeFailed, "canonical_wait_failed", completion.err.Error()
		return runner.publishOutcome(ctx, cause, outcome)
	}
	if err := validateClientToolResultCanonical(completion.canonical, job.receipt); err != nil {
		return fmt.Errorf("client tool-result rendezvous returned invalid canonical evidence: %w", err)
	}
	cause.CausalParents = appendUnique(cause.CausalParents, completion.canonical.CanonicalEnvelopeItemID)
	outcome.CanonicalEnvelopeItemID = completion.canonical.CanonicalEnvelopeItemID
	outcome.CanonicalTrajectoryItemID = completion.canonical.CanonicalTrajectoryItemID
	outcome.CanonicalStoreVersion = completion.canonical.StoreVersion
	return runner.publishOutcome(ctx, cause, outcome)
}

func (runner *clientToolResultRunner) pendingReplayOutcome(ctx context.Context,
	envelope element.Envelope, result trajectory.ToolResult, digest string,
	pending *clientToolResultJob) error {
	if pending.digest == digest {
		return runner.publishOutcome(ctx, envelope, Outcome{Kind: OutcomeIgnored,
			Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
			Code: "already_pending", Message: "exact client result is already pending",
			ResultDigest: digest, IngressItemID: envelope.ItemID})
	}
	cause := envelope.Clone()
	cause.CausalParents = appendUnique(cause.CausalParents, pending.cause.ItemID)
	return runner.publishOutcome(ctx, cause, Outcome{Kind: OutcomeFailed,
		Stage: "client_tool_result", Operation: "result", CallID: result.CallID,
		Code: "result_conflict", Message: "pending call received different client result bytes",
		ResultDigest: digest, IngressItemID: envelope.ItemID})
}

func (runner *clientToolResultRunner) markTerminal(identity, digest string) {
	if runner.terminal.add(identity) {
		runner.terminalDigests[identity] = digest
	}
}

var errClientToolResultItemCapacity = errors.New("client result immutable item evidence capacity is exhausted")

func (runner *clientToolResultRunner) rememberItem(
	envelope element.Envelope, evidence clientToolResultItemEvidence,
) error {
	if existing, found := runner.itemEvidence[envelope.ItemID]; found {
		if existing.kind != evidence.kind || existing.identity != evidence.identity ||
			existing.digest != evidence.digest ||
			!sameClientToolResultEnvelopeAddress(existing.envelope, evidence.envelope) {
			return errors.New("one immutable client-result item ID was reused with different evidence")
		}
		return nil
	}
	if len(runner.itemEvidence) >= maximumClientToolResultTerminal {
		return errClientToolResultItemCapacity
	}
	evidence.envelope.Payload = nil
	runner.itemEvidence[envelope.ItemID] = evidence
	return nil
}

func (runner *clientToolResultRunner) publishOutcome(ctx context.Context, cause element.Envelope, outcome Outcome) error {
	if outcome.FinishedNS == 0 {
		outcome.FinishedNS = runner.emit.clock.NowNS()
	}
	envelope, err := runner.emit.envelope(cause, outcomeType, outcome, "outcome")
	if err != nil {
		return err
	}
	return broadcastClientToolResultExact(ctx, runner.outcomeOutput, envelope, "terminal outcome")
}

func broadcastClientToolResultExact(ctx context.Context, output element.OutputPort, envelope element.Envelope, label string) error {
	delivery, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("client tool-result %s delivery = %+v, want exactly one delivered consumer", label, delivery)
	}
	return nil
}

func clientToolResultIdentity(envelope element.Envelope, callID string) (string, string, error) {
	if !canonicalClientToolResultIdentity(envelope.SessionID) {
		return "", "missing_session", errors.New("client tool result requires a canonical session ID")
	}
	if !canonicalClientToolResultIdentity(envelope.RunID) {
		return "", "missing_model_run", errors.New("client tool result requires a canonical cognition run ID")
	}
	if envelope.CancellationScope != envelope.RunID {
		return "", "cancellation_scope_mismatch", errors.New("client tool result cancellation scope differs from its cognition run")
	}
	if envelope.OpportunityID != callID {
		return "", "opportunity_mismatch", errors.New("client tool result opportunity differs from its exact call")
	}
	if !canonicalClientToolResultIdentity(envelope.ItemID) || envelope.ItemID == callID {
		return "", "invalid_item_id", errors.New("client tool result requires a distinct canonical ingress item ID")
	}
	return actionScopeKey(envelope.SessionID, envelope.RunID, callID), "", nil
}

func clientToolResultPayload(payload any) (trajectory.ToolResult, bool) {
	switch value := payload.(type) {
	case trajectory.ToolResult:
		return cloneToolResult(value), true
	case *trajectory.ToolResult:
		if value != nil {
			return cloneToolResult(*value), true
		}
	}
	return trajectory.ToolResult{}, false
}

func validateClientToolResult(result trajectory.ToolResult) error {
	if !canonicalClientToolResultIdentity(result.CallID) || !canonicalClientToolResultIdentity(result.Name) {
		return errors.New("client tool result requires canonical call and tool names")
	}
	hasOutput, hasError := len(result.Output) != 0, result.Error != ""
	if hasOutput == hasError {
		return errors.New("client tool result requires exactly one output or error")
	}
	if len(result.Output)+len(result.Error) > maximumClientToolResultBytes {
		return errors.New("client tool result exceeds the maximum payload size")
	}
	if hasOutput && !json.Valid(result.Output) {
		return errors.New("client tool result output is not valid JSON")
	}
	if hasError && (result.Error != strings.TrimSpace(result.Error) || !utf8.ValidString(result.Error)) {
		return errors.New("client tool result error is not canonical UTF-8 text")
	}
	return nil
}

func canonicalClientToolResultIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

// ClientToolResultDigest binds exact bytes with a domain separator and
// length-prefixed fields, including the output-vs-error distinction.
func ClientToolResultDigest(result trajectory.ToolResult) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("openrealtime.client-tool-result/v1\x00"))
	for _, value := range [][]byte{[]byte(result.CallID), []byte(result.Name), result.Output, []byte(result.Error)} {
		_, _ = fmt.Fprintf(hash, "%d:", len(value))
		_, _ = hash.Write(value)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validateClientToolResultReceipt(receipt ClientToolResultReceipt, sessionID, runID string,
	result trajectory.ToolResult, digest string) error {
	if !canonicalClientToolResultIdentity(receipt.ID) || receipt.SessionID != sessionID ||
		receipt.RunID != runID || receipt.CallID != result.CallID || receipt.Name != result.Name ||
		!canonicalClientToolResultIdentity(receipt.CommitmentID) ||
		!canonicalClientToolResultIdentity(receipt.CanonicalCallItemID) || receipt.ResultDigest != digest {
		return errors.New("receipt differs from the submitted session, run, call, name, authority, or result digest")
	}
	return nil
}

func validateClientToolResultCanonical(canonical ClientToolResultCanonical, receipt ClientToolResultReceipt) error {
	if canonical.Receipt != receipt || !canonicalClientToolResultIdentity(canonical.CanonicalEnvelopeItemID) ||
		!canonicalClientToolResultIdentity(canonical.CanonicalTrajectoryItemID) || canonical.StoreVersion == 0 {
		return errors.New("canonical evidence differs from the accepted receipt or safe-point identity")
	}
	return nil
}

type exactClientToolResultSet struct {
	maximum int
	values  map[string]struct{}
}

func newExactClientToolResultSet(maximum int) *exactClientToolResultSet {
	return &exactClientToolResultSet{maximum: maximum, values: make(map[string]struct{})}
}

func (set *exactClientToolResultSet) contains(value string) bool {
	_, found := set.values[value]
	return found
}
func (set *exactClientToolResultSet) full() bool          { return len(set.values) >= set.maximum }
func (set *exactClientToolResultSet) remove(value string) { delete(set.values, value) }
func (set *exactClientToolResultSet) add(value string) bool {
	if set.contains(value) {
		return true
	}
	if set.full() {
		return false
	}
	set.values[value] = struct{}{}
	return true
}

var _ element.Factory = clientToolResultFactory{}
var _ element.Runnable = (*clientToolResultRunner)(nil)
