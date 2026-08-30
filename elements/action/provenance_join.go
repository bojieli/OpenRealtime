package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type provenanceJoinFactory struct{}

var (
	_ element.Factory         = provenanceJoinFactory{}
	_ element.ConfigValidator = provenanceJoinFactory{}
)

func (provenanceJoinFactory) Descriptor() element.Descriptor { return ProvenanceJoinDescriptor() }
func (provenanceJoinFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeProvenanceJoinConfig(source)
	return err
}

func (provenanceJoinFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeProvenanceJoinConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("authority.ProvenanceJoin %s config: %w", mount.InstanceID, err)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	candidateInput, err := mount.Ports.Input("candidate")
	if err != nil {
		return nil, err
	}
	proposalInput, err := mount.Ports.Input("proposal")
	if err != nil {
		return nil, err
	}
	resultInput, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	timeoutInput, err := mount.Ports.Input("timeout")
	if err != nil {
		return nil, err
	}
	provenanceOutput, err := mount.Ports.Output("provenance")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	resolvedOutput, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	return &provenanceJoinRunner{
		config:         config,
		emit:           emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		candidateInput: candidateInput, proposalInput: proposalInput, resultInput: resultInput,
		cancelInput: cancelInput, timeoutInput: timeoutInput,
		provenanceOutput: provenanceOutput, outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		pending: make(map[string]*pendingProvenanceRun, config.MaxPending), callRuns: make(map[string]string),
		pendingRunSessions: make(map[string]string),
		terminalRuns:       newBoundedSet(config.TerminalMemory), terminalCalls: newBoundedSet(config.TerminalMemory),
		resolution: mount.Resolution,
	}, nil
}

type provenanceProposal struct {
	envelope element.Envelope
	value    cognitionelements.ToolProposal
}

type provenanceResult struct {
	envelope element.Envelope
	value    cognitionelements.Result
	byCallID map[string]cognitionelements.ToolProposal
	digest   string
}

type pendingProvenanceRun struct {
	sessionID         string
	runID             string
	candidateEnvelope *element.Envelope
	candidate         authority.Candidate
	result            *provenanceResult
	proposals         map[string]provenanceProposal
	finished          map[string]struct{}
}

type provenanceJoinRunner struct {
	config ProvenanceJoinConfig
	emit   emitter

	candidateInput   element.InputPort
	proposalInput    element.InputPort
	resultInput      element.InputPort
	cancelInput      element.InputPort
	timeoutInput     element.InputPort
	provenanceOutput element.OutputPort
	outcomeOutput    element.OutputPort
	resolvedOutput   element.OutputPort

	pending            map[string]*pendingProvenanceRun
	pendingRunSessions map[string]string
	callRuns           map[string]string
	terminalRuns       *boundedSet
	terminalCalls      *boundedSet
	resolution         element.ResolutionReporter
}

func (runner *provenanceJoinRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "provenance_join", Reference: "authority.Candidate+cognition.Result+tool.Proposal",
		Identity: "authority.ProvenanceJoin", ServiceRevision: 1,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ProvenanceJoinDescriptor(), nil); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 5)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{
		{"candidate", runner.candidateInput}, {"proposal", runner.proposalInput},
		{"result", runner.resultInput}, {"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput},
	} {
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
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "candidate":
				err = runner.acceptCandidate(ctx, input.envelope)
			case "proposal":
				err = runner.acceptProposal(ctx, input.envelope)
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			case "cancel", "timeout":
				err = runner.interrupt(ctx, input.envelope, input.kind)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *provenanceJoinRunner) acceptCandidate(ctx context.Context, envelope element.Envelope) error {
	candidate, valid := authorityCandidatePayload(envelope.Payload)
	if !valid {
		return runner.reject(ctx, envelope, "candidate", "", "invalid_payload",
			fmt.Sprintf("authority candidate payload has type %T", envelope.Payload))
	}
	runID := strings.TrimSpace(candidate.RunID)
	if code, err := validateAuthorityCandidate(envelope, candidate); err != nil {
		return runner.reject(ctx, envelope, "candidate", "", code, err.Error())
	}
	runKey := actionRunKey(candidate.SessionID, runID)
	if sessionID, found := runner.pendingRunSessions[runID]; found && sessionID != candidate.SessionID {
		return runner.reject(ctx, envelope, "candidate", "", "session_mismatch",
			"cognition run ID is already pending in another session")
	}
	if runner.terminalRuns.contains(runKey) {
		return runner.ignore(ctx, envelope, "candidate", "", "terminal_replay",
			"candidate cognition run is already terminal")
	}
	pending, err := runner.pendingForRun(candidate.SessionID, runID)
	if err != nil {
		return runner.reject(ctx, envelope, "candidate", "", "capacity", err.Error())
	}
	if pending.candidateEnvelope != nil {
		return runner.reject(ctx, envelope, "candidate", "", "duplicate_candidate",
			"cognition run already has an activation candidate")
	}
	copy := envelope.Clone()
	pending.candidateEnvelope = &copy
	pending.candidate = candidate
	return runner.tryJoin(ctx, runKey)
}

func (runner *provenanceJoinRunner) acceptProposal(ctx context.Context, envelope element.Envelope) error {
	proposal, valid := proposalPayload(envelope.Payload)
	callID := strings.TrimSpace(proposal.Call.CallID)
	if !valid {
		return runner.reject(ctx, envelope, "proposal", callID, "invalid_payload",
			fmt.Sprintf("proposal payload has type %T", envelope.Payload))
	}
	if err := validateProposal(proposal); err != nil {
		return runner.reject(ctx, envelope, "proposal", callID, "invalid_proposal", err.Error())
	}
	runID := strings.TrimSpace(envelope.RunID)
	if strings.TrimSpace(envelope.ItemID) == "" || runID == "" || strings.TrimSpace(envelope.SessionID) == "" {
		return runner.reject(ctx, envelope, "proposal", callID, "missing_identity",
			"proposal envelope requires item, non-empty session, and cognition-run identities")
	}
	runKey := actionRunKey(envelope.SessionID, runID)
	callKey := actionScopeKey(envelope.SessionID, runID, callID)
	if sessionID, found := runner.pendingRunSessions[runID]; found && sessionID != envelope.SessionID {
		return runner.reject(ctx, envelope, "proposal", callID, "session_mismatch",
			"cognition run ID is already pending in another session")
	}
	if runner.terminalCalls.contains(callKey) || runner.terminalRuns.contains(runKey) {
		return runner.ignore(ctx, envelope, "proposal", callID, "terminal_replay",
			"proposal call or cognition run is already terminal")
	}
	if otherRun, found := runner.callRuns[callKey]; found && otherRun != runKey {
		return runner.reject(ctx, envelope, "proposal", callID, "call_id_collision",
			"proposal call ID is already bound to another cognition scope")
	}
	pending, err := runner.pendingForRun(envelope.SessionID, runID)
	if err != nil {
		return runner.reject(ctx, envelope, "proposal", callID, "capacity", err.Error())
	}
	if _, duplicate := pending.proposals[callID]; duplicate {
		return runner.reject(ctx, envelope, "proposal", callID, "duplicate_proposal",
			"proposal call ID is already pending")
	}
	if pending.result != nil {
		member, found := pending.result.byCallID[callID]
		if !found || !sameProposal(proposal, member) {
			runner.terminalCalls.add(callKey)
			if found {
				pending.finished[callID] = struct{}{}
			}
			if err := runner.reject(ctx, envelope, "proposal", callID, "proposal_membership_mismatch",
				"proposal is not a byte-exact member of the cognition result"); err != nil {
				return err
			}
			if provenanceRunComplete(pending) {
				runner.finishRun(runKey, pending)
			}
			return nil
		}
	}
	runner.callRuns[callKey] = runKey
	pending.proposals[callID] = provenanceProposal{envelope: envelope.Clone(), value: proposal}
	return runner.tryJoin(ctx, runKey)
}

func (runner *provenanceJoinRunner) acceptResult(ctx context.Context, envelope element.Envelope) error {
	result, valid := cognitionResultForProvenance(envelope.Payload)
	if !valid {
		return runner.reject(ctx, envelope, "result", "", "invalid_payload",
			fmt.Sprintf("cognition result payload has type %T", envelope.Payload))
	}
	runID := strings.TrimSpace(result.RunID)
	if code, err := validateProvenanceResult(envelope, result); err != nil {
		return runner.reject(ctx, envelope, "result", "", code, err.Error())
	}
	resultDigest, err := digestJSON(result)
	if err != nil {
		return runner.reject(ctx, envelope, "result", "", "result_digest_failed", err.Error())
	}
	runKey := actionRunKey(envelope.SessionID, runID)
	if sessionID, found := runner.pendingRunSessions[runID]; found && sessionID != envelope.SessionID {
		return runner.reject(ctx, envelope, "result", "", "session_mismatch",
			"cognition run ID is already pending in another session")
	}
	if runner.terminalRuns.contains(runKey) {
		return runner.ignore(ctx, envelope, "result", "", "result_replay",
			"cognition run is already terminal")
	}
	pending, err := runner.pendingForRun(envelope.SessionID, runID)
	if err != nil {
		return runner.reject(ctx, envelope, "result", "", "capacity", err.Error())
	}
	if pending.result != nil {
		return runner.reject(ctx, envelope, "result", "", "duplicate_result",
			"cognition run already supplied a result")
	}
	byCallID := make(map[string]cognitionelements.ToolProposal, len(result.ToolProposals))
	for _, proposal := range result.ToolProposals {
		callID := strings.TrimSpace(proposal.Call.CallID)
		callKey := actionScopeKey(envelope.SessionID, runID, callID)
		if _, duplicate := byCallID[callID]; duplicate {
			return runner.reject(ctx, envelope, "result", callID, "duplicate_result_call",
				"cognition result repeats a tool call ID")
		}
		if otherRun, found := runner.callRuns[callKey]; found && otherRun != runKey {
			return runner.reject(ctx, envelope, "result", callID, "call_id_collision",
				"result call ID is already bound to another cognition scope")
		}
		byCallID[callID] = cloneProposal(proposal)
	}
	for callID, proposal := range pending.proposals {
		callKey := actionScopeKey(envelope.SessionID, runID, callID)
		member, found := byCallID[callID]
		if !found || !sameProposal(proposal.value, member) {
			delete(pending.proposals, callID)
			delete(runner.callRuns, callKey)
			if found {
				pending.finished[callID] = struct{}{}
			}
			runner.terminalCalls.add(callKey)
			if err := runner.reject(ctx, proposal.envelope, "join", callID, "proposal_membership_mismatch",
				"proposal is not a byte-exact member of the cognition result"); err != nil {
				return err
			}
		}
	}
	pending.result = &provenanceResult{
		envelope: envelope.Clone(), value: result, byCallID: byCallID, digest: resultDigest,
	}
	for callID := range byCallID {
		if runner.terminalCalls.contains(actionScopeKey(envelope.SessionID, runID, callID)) {
			pending.finished[callID] = struct{}{}
		}
	}
	if len(byCallID) == 0 {
		runner.finishRun(runKey, pending)
		return runner.ignore(ctx, envelope, "result", "", "no_tool_proposals",
			"cognition result contains no tool proposals")
	}
	if provenanceRunComplete(pending) {
		runner.finishRun(runKey, pending)
		return nil
	}
	return runner.tryJoin(ctx, runKey)
}

func validateAuthorityCandidate(envelope element.Envelope, candidate authority.Candidate) (string, error) {
	if strings.TrimSpace(envelope.ItemID) == "" || strings.TrimSpace(envelope.RunID) == "" ||
		strings.TrimSpace(envelope.SessionID) == "" {
		return "missing_identity", errors.New("candidate envelope requires item, non-empty session, and run identities")
	}
	if strings.TrimSpace(candidate.RunID) == "" || candidate.RunID != envelope.RunID {
		return "model_run_mismatch", errors.New("candidate payload and envelope must name one cognition run")
	}
	if strings.TrimSpace(candidate.SessionID) == "" || candidate.SessionID != envelope.SessionID {
		return "session_mismatch", errors.New("candidate payload and envelope must name one non-empty session")
	}
	for name, value := range map[string]string{
		"activation item": candidate.ActivationItemID, "activation cause": candidate.ActivationCauseItemID,
		"observation item": candidate.ObservationItemID, "observation trigger": candidate.ObservationTriggerItemID,
		"context envelope": candidate.ContextEnvelopeItemID, "context tail": candidate.ContextTailItem,
	} {
		if strings.TrimSpace(value) == "" {
			return "invalid_candidate", fmt.Errorf("candidate requires a non-empty %s identity", name)
		}
	}
	if candidate.SourceRevision == 0 || candidate.ContextVersion == 0 {
		return "invalid_candidate", errors.New("candidate requires positive source and context revisions")
	}
	if envelope.ItemID == candidate.ActivationItemID {
		return "invalid_candidate", errors.New("candidate and activation trigger identities must be distinct")
	}
	for _, cause := range []string{candidate.ActivationCauseItemID, candidate.ObservationItemID,
		candidate.ObservationTriggerItemID, candidate.ContextEnvelopeItemID, candidate.ContextTailItem} {
		if !slices.Contains(envelope.CausalParents, cause) {
			return "candidate_cause_mismatch", fmt.Errorf("candidate envelope is not causally bound to %q", cause)
		}
	}
	return "", nil
}

func validateProvenanceResult(envelope element.Envelope, result cognitionelements.Result) (string, error) {
	runID := strings.TrimSpace(result.RunID)
	if runID == "" || strings.TrimSpace(envelope.RunID) == "" || envelope.RunID != runID {
		return "model_run_mismatch", errors.New("result payload and envelope must name one cognition run")
	}
	if strings.TrimSpace(envelope.SessionID) == "" {
		return "missing_session", errors.New("cognition result requires a non-empty session ID")
	}
	if strings.TrimSpace(result.ProviderReference) == "" {
		return "missing_provider_reference", errors.New("cognition result requires a deployment provider reference")
	}
	if err := continuation.ValidateDescriptor(result.Descriptor); err != nil {
		return "invalid_descriptor", fmt.Errorf("invalid cognition descriptor: %w", err)
	}
	if err := continuation.ValidateInvocation(result.Invocation, result.Descriptor); err != nil {
		return "invalid_invocation", fmt.Errorf("invalid cognition invocation: %w", err)
	}
	outputProposals := make([]cognitionelements.ToolProposal, 0, len(result.ToolProposals))
	for index, output := range result.Outputs {
		if output.Kind != cognitionelements.PreparedTool {
			continue
		}
		if output.Proposal == nil || output.Text != "" {
			return "invalid_result", fmt.Errorf("tool output %d is malformed", index)
		}
		outputProposals = append(outputProposals, cloneProposal(*output.Proposal))
	}
	if len(outputProposals) != len(result.ToolProposals) {
		return "result_membership_mismatch", errors.New("ordered outputs and aggregate tool proposals disagree")
	}
	seen := make(map[string]struct{}, len(result.ToolProposals))
	for index, proposal := range result.ToolProposals {
		if err := validateProposal(proposal); err != nil {
			return "invalid_result_proposal", fmt.Errorf("tool proposal %d: %w", index, err)
		}
		if _, duplicate := seen[proposal.Call.CallID]; duplicate {
			return "duplicate_result_call", fmt.Errorf("tool proposal %d repeats call ID %q", index, proposal.Call.CallID)
		}
		seen[proposal.Call.CallID] = struct{}{}
		if proposal.ProviderAuthority != result.Descriptor.EffectiveToolAuthority() {
			return "provider_authority_mismatch", fmt.Errorf(
				"tool proposal %d claims %q authority, descriptor grants %q",
				index, proposal.ProviderAuthority, result.Descriptor.EffectiveToolAuthority())
		}
		declared := slices.ContainsFunc(result.Invocation.Tools, func(tool continuation.ToolDefinition) bool {
			return tool.Name == proposal.Call.Name
		})
		if !declared {
			return "undeclared_result_proposal", fmt.Errorf(
				"tool proposal %d names %q outside the invocation declaration", index, proposal.Call.Name)
		}
		if !sameProposal(outputProposals[index], proposal) {
			return "result_membership_mismatch", errors.New("ordered outputs and aggregate tool proposals differ")
		}
	}
	// Observation context is execution authority, not a prerequisite for a
	// speech-only continuation. Explicit response.create runs deliberately
	// carry no observation authority; they must remain visible to this join so
	// a forged streamed proposal cannot hide from the aggregate result, but a
	// result with no tool proposal has nothing to authorize. Validate the
	// complete result membership above before making this authority decision.
	if len(result.ToolProposals) > 0 && result.Interrupted {
		return "interrupted_result", errors.New("an interrupted cognition result cannot authorize a tool proposal")
	}
	if len(result.ToolProposals) > 0 &&
		(result.ContextVersion == 0 || strings.TrimSpace(result.ContextTailID) == "" ||
			result.Invocation.SourceRevision == 0) {
		return "missing_context", errors.New("result requires a positive context version, tail, and source revision")
	}
	return "", nil
}

func (runner *provenanceJoinRunner) pendingForRun(sessionID, runID string) (*pendingProvenanceRun, error) {
	runKey := actionRunKey(sessionID, runID)
	if pending := runner.pending[runKey]; pending != nil {
		return pending, nil
	}
	if len(runner.pending) >= runner.config.MaxPending {
		return nil, fmt.Errorf("provenance join has %d pending cognition runs", len(runner.pending))
	}
	pending := &pendingProvenanceRun{
		sessionID: sessionID, runID: runID,
		proposals: make(map[string]provenanceProposal), finished: make(map[string]struct{}),
	}
	runner.pending[runKey] = pending
	runner.pendingRunSessions[runID] = sessionID
	return pending, nil
}

func (runner *provenanceJoinRunner) tryJoin(ctx context.Context, runKey string) error {
	pending := runner.pending[runKey]
	if pending == nil || pending.candidateEnvelope == nil || pending.result == nil {
		return nil
	}
	if code, err := validateCandidateResultBinding(pending.candidate, *pending.candidateEnvelope, *pending.result); err != nil {
		runner.finishRun(runKey, pending)
		return runner.reject(ctx, pending.result.envelope, "join", "", code, err.Error())
	}
	for callID, proposal := range pending.proposals {
		callKey := actionScopeKey(pending.sessionID, pending.runID, callID)
		if _, done := pending.finished[callID]; done {
			continue
		}
		member, found := pending.result.byCallID[callID]
		if !found || !sameProposal(proposal.value, member) {
			delete(runner.callRuns, callKey)
			pending.finished[callID] = struct{}{}
			runner.terminalCalls.add(callKey)
			if err := runner.reject(ctx, proposal.envelope, "join", callID, "proposal_membership_mismatch",
				"proposal is not a byte-exact member of the cognition result"); err != nil {
				return err
			}
			continue
		}
		if code, err := validateCandidateProposalBinding(pending.candidate, proposal.envelope); err != nil {
			delete(runner.callRuns, callKey)
			pending.finished[callID] = struct{}{}
			runner.terminalCalls.add(callKey)
			if publishErr := runner.reject(ctx, proposal.envelope, "join", callID, code, err.Error()); publishErr != nil {
				return publishErr
			}
			continue
		}
		provenance := Provenance{
			CallID: callID, ProposalItemID: proposal.envelope.ItemID,
			CandidateItemID: pending.candidateEnvelope.ItemID, ResultItemID: pending.result.envelope.ItemID,
			ModelRunID: pending.runID, SessionID: pending.candidate.SessionID,
			ActivationItemID:         pending.candidate.ActivationItemID,
			ActivationCauseItemID:    pending.candidate.ActivationCauseItemID,
			ObservationItemID:        pending.candidate.ObservationItemID,
			ObservationTriggerItemID: pending.candidate.ObservationTriggerItemID,
			SourceRevision:           pending.candidate.SourceRevision, ContextVersion: pending.candidate.ContextVersion,
			ContextEnvelopeItemID: pending.candidate.ContextEnvelopeItemID,
			ContextTailItem:       pending.candidate.ContextTailItem,
			ProviderReference:     pending.result.value.ProviderReference,
			ModelResultDigest:     pending.result.digest,
			ModelProducer: trajectory.Producer{
				Phase:           pending.result.value.Descriptor.Phase,
				Provider:        pending.result.value.Descriptor.Provider,
				Model:           pending.result.value.Descriptor.Model,
				ReasoningEffort: string(pending.result.value.Descriptor.Effort),
				SpeechAuthority: string(pending.result.value.Descriptor.EffectiveSpeechAuthority()),
			},
		}
		parent := proposal.envelope.Clone()
		parent.CausalParents = appendUnique(parent.CausalParents, pending.candidateEnvelope.ItemID)
		parent.CausalParents = appendUnique(parent.CausalParents, pending.result.envelope.ItemID)
		if err := publishPayload(ctx, runner.emit, runner.provenanceOutput, parent,
			provenanceType, provenance, "provenance"); err != nil {
			return err
		}
		if err := publishOutcome(ctx, runner.emit, runner.outcomeOutput, parent, Outcome{
			Kind: OutcomeSucceeded, Stage: "provenance_join", Operation: "join", CallID: callID,
		}); err != nil {
			return err
		}
		delete(runner.callRuns, callKey)
		pending.finished[callID] = struct{}{}
		runner.terminalCalls.add(callKey)
	}
	if provenanceRunComplete(pending) {
		runner.finishRun(runKey, pending)
	}
	return nil
}

func validateCandidateResultBinding(
	candidate authority.Candidate, candidateEnvelope element.Envelope, result provenanceResult,
) (string, error) {
	if candidate.RunID != result.value.RunID || result.envelope.RunID != candidate.RunID {
		return "model_run_mismatch", errors.New("candidate and result name different cognition runs")
	}
	if candidate.SessionID == "" || candidateEnvelope.SessionID != candidate.SessionID ||
		result.envelope.SessionID != candidate.SessionID {
		return "session_mismatch", errors.New("candidate and result must name one non-empty session")
	}
	if candidate.ContextVersion != result.value.ContextVersion || candidate.ContextTailItem != result.value.ContextTailID {
		return "context_mismatch", errors.New("candidate and result name different canonical context prefixes")
	}
	if candidate.SourceRevision != result.value.Invocation.SourceRevision {
		return "source_revision_mismatch", errors.New("candidate and result name different source revisions")
	}
	for _, cause := range candidateModelCauses(candidate) {
		if !slices.Contains(result.envelope.CausalParents, cause) {
			return "result_cause_mismatch", fmt.Errorf("result is not causally bound to candidate evidence %q", cause)
		}
	}
	return "", nil
}

func validateCandidateProposalBinding(candidate authority.Candidate, proposal element.Envelope) (string, error) {
	if proposal.RunID != candidate.RunID {
		return "model_run_mismatch", errors.New("candidate and proposal name different cognition runs")
	}
	if candidate.SessionID == "" || proposal.SessionID != candidate.SessionID {
		return "session_mismatch", errors.New("candidate and proposal must name one non-empty session")
	}
	for _, cause := range candidateModelCauses(candidate) {
		if !slices.Contains(proposal.CausalParents, cause) {
			return "proposal_cause_mismatch", fmt.Errorf("proposal is not causally bound to candidate evidence %q", cause)
		}
	}
	return "", nil
}

func candidateModelCauses(candidate authority.Candidate) []string {
	return []string{
		candidate.ActivationItemID, candidate.ActivationCauseItemID, candidate.ObservationItemID,
		candidate.ObservationTriggerItemID, candidate.ContextEnvelopeItemID, candidate.ContextTailItem,
	}
}

func (runner *provenanceJoinRunner) interrupt(
	ctx context.Context, envelope element.Envelope, operation string,
) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return runner.reject(ctx, envelope, operation, "", code, err.Error())
	}
	callKey, identityCode, err := interruptIdentity(envelope, interrupt)
	if err != nil {
		return runner.reject(ctx, envelope, operation, interrupt.CallID, identityCode, err.Error())
	}
	runKey := actionRunKey(envelope.SessionID, envelope.RunID)
	already := runner.terminalCalls.contains(callKey)
	matched := false
	if boundRun, found := runner.callRuns[callKey]; found {
		matched = true
		pending := runner.pending[boundRun]
		delete(runner.callRuns, callKey)
		delete(pending.proposals, interrupt.CallID)
		if pending.result != nil {
			if _, expected := pending.result.byCallID[interrupt.CallID]; expected {
				pending.finished[interrupt.CallID] = struct{}{}
			}
		}
		runner.terminalCalls.add(callKey)
		if provenanceRunComplete(pending) {
			runner.finishRun(boundRun, pending)
		}
	} else if pending := runner.pending[runKey]; pending != nil && pending.result != nil {
		if _, expected := pending.result.byCallID[interrupt.CallID]; expected {
			matched = true
			pending.finished[interrupt.CallID] = struct{}{}
			if provenanceRunComplete(pending) {
				runner.finishRun(runKey, pending)
			}
		}
	}
	if !matched {
		runner.terminalCalls.add(callKey)
	}
	kind := OutcomeCanceled
	if operation == "timeout" {
		kind = OutcomeTimedOut
	}
	if !matched && already {
		kind, code = OutcomeIgnored, "already_terminal"
	}
	if interrupt.Reason == "" {
		interrupt.Reason = operation
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: kind, Stage: "provenance_join", Operation: operation,
		CallID: interrupt.CallID, Code: code, Message: interrupt.Reason,
	})
}

func provenanceRunComplete(pending *pendingProvenanceRun) bool {
	if pending == nil || pending.result == nil {
		return false
	}
	for callID := range pending.result.byCallID {
		if _, finished := pending.finished[callID]; !finished {
			return false
		}
	}
	return true
}

func (runner *provenanceJoinRunner) finishRun(runKey string, pending *pendingProvenanceRun) {
	delete(runner.pending, runKey)
	delete(runner.pendingRunSessions, pending.runID)
	runner.terminalRuns.add(runKey)
	for callID := range pending.proposals {
		callKey := actionScopeKey(pending.sessionID, pending.runID, callID)
		delete(runner.callRuns, callKey)
		runner.terminalCalls.add(callKey)
	}
	if pending.result != nil {
		for callID := range pending.result.byCallID {
			runner.terminalCalls.add(actionScopeKey(pending.sessionID, pending.runID, callID))
		}
	}
}

func (runner *provenanceJoinRunner) reject(
	ctx context.Context, envelope element.Envelope, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: OutcomeRejected, Stage: "provenance_join", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

func (runner *provenanceJoinRunner) ignore(
	ctx context.Context, envelope element.Envelope, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: OutcomeIgnored, Stage: "provenance_join", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

func cognitionResultForProvenance(payload any) (cognitionelements.Result, bool) {
	var result cognitionelements.Result
	switch value := payload.(type) {
	case cognitionelements.Result:
		result = value
	case *cognitionelements.Result:
		if value == nil {
			return cognitionelements.Result{}, false
		}
		result = *value
	default:
		return cognitionelements.Result{}, false
	}
	result.Outputs = slices.Clone(result.Outputs)
	for index := range result.Outputs {
		if result.Outputs[index].Proposal != nil {
			proposal := cloneProposal(*result.Outputs[index].Proposal)
			result.Outputs[index].Proposal = &proposal
		}
	}
	result.ToolProposals = slices.Clone(result.ToolProposals)
	for index := range result.ToolProposals {
		result.ToolProposals[index] = cloneProposal(result.ToolProposals[index])
	}
	return result, true
}

func sameProposal(left, right cognitionelements.ToolProposal) bool {
	return left.Declared == right.Declared && left.ProviderAuthority == right.ProviderAuthority &&
		left.Call.CallID == right.Call.CallID && left.Call.Name == right.Call.Name &&
		bytes.Equal(left.Call.Arguments, right.Call.Arguments)
}
