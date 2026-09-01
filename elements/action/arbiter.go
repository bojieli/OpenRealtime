package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
)

type actionArbiterFactory struct{}

var (
	_ element.Factory         = actionArbiterFactory{}
	_ element.ConfigValidator = actionArbiterFactory{}
)

func (actionArbiterFactory) Descriptor() element.Descriptor { return ActionArbiterDescriptor() }

func (actionArbiterFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeActionArbiterConfig(source)
	return err
}

func (actionArbiterFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeActionArbiterConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("authority.ActionArbiter %s config: %w", mount.InstanceID, err)
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
	lanes := len(candidateInput.Lanes())
	if lanes < 2 || len(proposalInput.Lanes()) != lanes || len(resultInput.Lanes()) != lanes {
		return nil, fmt.Errorf(
			"authority.ActionArbiter %s requires equal candidate, proposal, and result lane counts of at least two; got %d/%d/%d",
			mount.InstanceID, lanes, len(proposalInput.Lanes()), len(resultInput.Lanes()),
		)
	}
	selectedOutput, err := mount.Ports.Output("selected")
	if err != nil {
		return nil, err
	}
	cancelOutput, err := mount.Ports.Output("cancel_upstream")
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
	maximumOrphans := config.MaxPending * lanes
	if maximumOrphans > 4096 {
		maximumOrphans = 4096
	}
	maximumTerminalRuns := config.TerminalMemory * lanes
	if maximumTerminalRuns > 4096 {
		maximumTerminalRuns = 4096
	}
	return &actionArbiterRunner{
		config: config, expectedLanes: lanes, maximumOrphans: maximumOrphans,
		emit:           emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		candidateInput: candidateInput, proposalInput: proposalInput, resultInput: resultInput,
		selectedOutput: selectedOutput, cancelOutput: cancelOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		groups:         make(map[string]*actionArbitrationGroup, config.MaxPending),
		orphanResults:  make(map[string]actionArbitrationResult),
		terminalGroups: newBoundedSet(config.TerminalMemory),
		terminalRuns:   newBoundedSet(maximumTerminalRuns),
		canceledRuns:   newBoundedSet(maximumTerminalRuns),
		resolution:     mount.Resolution,
	}, nil
}

type actionArbitrationCandidate struct {
	envelope element.Envelope
	value    authority.Candidate
}

type actionArbitrationProposal struct {
	envelope element.Envelope
	value    AdmittedProposal
}

type actionArbitrationResult struct {
	envelope element.Envelope
	value    cognitionelements.Result
}

type actionArbitrationMember struct {
	candidate *actionArbitrationCandidate
	proposal  *actionArbitrationProposal
	result    *actionArbitrationResult
}

type actionArbitrationGroup struct {
	sessionID string
	triggerID string
	members   map[string]*actionArbitrationMember
}

type actionArbiterRunner struct {
	config         ActionArbiterConfig
	expectedLanes  int
	maximumOrphans int
	emit           emitter
	candidateInput element.InputPort
	proposalInput  element.InputPort
	resultInput    element.InputPort
	selectedOutput element.OutputPort
	cancelOutput   element.OutputPort
	outcomeOutput  element.OutputPort
	resolvedOutput element.OutputPort
	groups         map[string]*actionArbitrationGroup
	orphanResults  map[string]actionArbitrationResult
	terminalGroups *boundedSet
	terminalRuns   *boundedSet
	canceledRuns   *boundedSet
	resolution     element.ResolutionReporter
}

func (runner *actionArbiterRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "action_arbitration", Reference: "policy.first-complete-observation",
		Identity: "authority.ActionArbiter/v1", ServiceRevision: 1,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ActionArbiterDescriptor(), nil); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{
		{kind: "candidate", port: runner.candidateInput},
		{kind: "proposal", port: runner.proposalInput},
		{kind: "result", port: runner.resultInput},
	} {
		wait.Add(1)
		go receiveArbitrationInputs(ctx, input.kind, input.port, inputs, failures, &wait)
	}
	defer func() {
		cancel(nil)
		wait.Wait()
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
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func receiveArbitrationInputs(
	ctx context.Context, kind string, port element.InputPort, destination chan<- receivedInput,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, _, err := port.ReceiveAny(ctx)
		if err != nil {
			if ctx.Err() == nil {
				select {
				case failures <- fmt.Errorf("receive action arbitration %s: %w", kind, err):
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

func (runner *actionArbiterRunner) acceptCandidate(
	ctx context.Context, envelope element.Envelope,
) error {
	candidate, ok := authorityCandidatePayload(envelope.Payload)
	if !ok {
		return runner.reject(ctx, envelope, "candidate", "", "invalid_payload",
			fmt.Sprintf("candidate payload has type %T", envelope.Payload))
	}
	if code, err := validateAuthorityCandidate(envelope, candidate); err != nil {
		return runner.reject(ctx, envelope, "candidate", "", code, err.Error())
	}
	groupKey := arbitrationGroupKey(candidate.SessionID, candidate.ObservationTriggerItemID)
	if runner.terminalGroups.contains(groupKey) {
		if err := runner.cancelRun(ctx, envelope, candidate.RunID,
			"action arbitration already completed for this observation"); err != nil {
			return err
		}
		return runner.reject(ctx, envelope, "candidate", "", "terminal_observation",
			"action arbitration already completed for this observation")
	}
	group, err := runner.group(groupKey, candidate.SessionID, candidate.ObservationTriggerItemID)
	if err != nil {
		if cancelErr := runner.cancelRun(ctx, envelope, candidate.RunID,
			"action arbitration capacity exceeded"); cancelErr != nil {
			return errors.Join(err, cancelErr)
		}
		return runner.reject(ctx, envelope, "candidate", "", "capacity", err.Error())
	}
	member, err := runner.member(group, candidate.RunID)
	if err != nil {
		if cancelErr := runner.cancelRun(ctx, envelope, candidate.RunID, err.Error()); cancelErr != nil {
			return cancelErr
		}
		return runner.reject(ctx, envelope, "candidate", "", "lane_capacity", err.Error())
	}
	if member.candidate != nil {
		return runner.reject(ctx, envelope, "candidate", "", "duplicate_candidate",
			"action arbitration already received this cognition candidate")
	}
	copy := envelope.Clone()
	member.candidate = &actionArbitrationCandidate{envelope: copy, value: candidate}
	if result, found := runner.orphanResults[actionRunKey(candidate.SessionID, candidate.RunID)]; found {
		delete(runner.orphanResults, actionRunKey(candidate.SessionID, candidate.RunID))
		member.result = &result
		if code, bindingErr := validateCandidateResultBinding(
			candidate, copy, provenanceResult{envelope: result.envelope, value: result.value},
		); bindingErr != nil {
			member.result = nil
			return runner.reject(ctx, result.envelope, "result", "", code, bindingErr.Error())
		}
	}
	if member.proposal != nil {
		if code, bindingErr := validateArbitrationBinding(*member.candidate, *member.proposal); bindingErr != nil {
			return runner.reject(ctx, member.proposal.envelope, "proposal",
				member.proposal.value.Proposal.Call.CallID, code, bindingErr.Error())
		}
		return runner.selectProposal(ctx, groupKey, group, candidate.RunID, *member.proposal)
	}
	return runner.finishNoActionGroup(ctx, groupKey, group, envelope)
}

func (runner *actionArbiterRunner) acceptProposal(
	ctx context.Context, envelope element.Envelope,
) error {
	proposal, ok := admittedPayload(envelope.Payload)
	if !ok {
		return runner.reject(ctx, envelope, "proposal", "", "invalid_payload",
			fmt.Sprintf("admitted proposal payload has type %T", envelope.Payload))
	}
	callID := proposal.Proposal.Call.CallID
	if err := validateAdmittedProposal(proposal); err != nil {
		return runner.reject(ctx, envelope, "proposal", callID, "invalid_proposal", err.Error())
	}
	if strings.TrimSpace(envelope.ItemID) == "" || envelope.SessionID != proposal.SessionID ||
		envelope.RunID != proposal.ModelRunID {
		return runner.reject(ctx, envelope, "proposal", callID, "identity_mismatch",
			"admitted proposal envelope must match its non-empty session and cognition run identities")
	}
	groupKey := arbitrationGroupKey(proposal.SessionID, proposal.ObservationTriggerItemID)
	if runner.terminalGroups.contains(groupKey) {
		if err := runner.cancelRun(ctx, envelope, proposal.ModelRunID,
			"another action already won this observation race"); err != nil {
			return err
		}
		return runner.reject(ctx, envelope, "proposal", callID, "not_selected",
			"another action already won this observation race")
	}
	group, err := runner.group(groupKey, proposal.SessionID, proposal.ObservationTriggerItemID)
	if err != nil {
		if cancelErr := runner.cancelRun(ctx, envelope, proposal.ModelRunID,
			"action arbitration capacity exceeded"); cancelErr != nil {
			return errors.Join(err, cancelErr)
		}
		return runner.reject(ctx, envelope, "proposal", callID, "capacity", err.Error())
	}
	member, err := runner.member(group, proposal.ModelRunID)
	if err != nil {
		if cancelErr := runner.cancelRun(ctx, envelope, proposal.ModelRunID, err.Error()); cancelErr != nil {
			return cancelErr
		}
		return runner.reject(ctx, envelope, "proposal", callID, "lane_capacity", err.Error())
	}
	if member.proposal != nil {
		return runner.reject(ctx, envelope, "proposal", callID, "duplicate_proposal",
			"action arbitration already received this admitted proposal")
	}
	copy := envelope.Clone()
	member.proposal = &actionArbitrationProposal{envelope: copy, value: proposal}
	if member.result != nil && len(member.result.value.ToolProposals) == 0 {
		return runner.reject(ctx, envelope, "proposal", callID, "result_membership_mismatch",
			"cognition result declared no tool proposal for this action")
	}
	if member.candidate == nil {
		return nil
	}
	if code, bindingErr := validateArbitrationBinding(*member.candidate, *member.proposal); bindingErr != nil {
		return runner.reject(ctx, envelope, "proposal", callID, code, bindingErr.Error())
	}
	return runner.selectProposal(ctx, groupKey, group, proposal.ModelRunID, *member.proposal)
}

func (runner *actionArbiterRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	result, ok := arbitrationResultPayload(envelope.Payload)
	if !ok {
		return runner.reject(ctx, envelope, "result", "", "invalid_payload",
			fmt.Sprintf("cognition result payload has type %T", envelope.Payload))
	}
	if code, err := validateProvenanceResult(envelope, result); err != nil {
		return runner.reject(ctx, envelope, "result", "", code, err.Error())
	}
	if len(result.ToolProposals) != 0 {
		return nil
	}
	runKey := actionRunKey(envelope.SessionID, envelope.RunID)
	if runner.terminalRuns.contains(runKey) {
		return runner.reject(ctx, envelope, "result", "", "terminal_result",
			"action arbitration already completed for this cognition run")
	}
	for groupKey, group := range runner.groups {
		member := group.members[runKey]
		if member == nil || member.candidate == nil {
			continue
		}
		if member.result != nil {
			return runner.reject(ctx, envelope, "result", "", "duplicate_result",
				"action arbitration already received this cognition result")
		}
		copy := envelope.Clone()
		stored := actionArbitrationResult{envelope: copy, value: result}
		if code, err := validateCandidateResultBinding(
			member.candidate.value, member.candidate.envelope,
			provenanceResult{envelope: copy, value: result},
		); err != nil {
			return runner.reject(ctx, envelope, "result", "", code, err.Error())
		}
		member.result = &stored
		return runner.finishNoActionGroup(ctx, groupKey, group, envelope)
	}
	if _, duplicate := runner.orphanResults[runKey]; duplicate {
		return runner.reject(ctx, envelope, "result", "", "duplicate_result",
			"action arbitration already retains this cognition result")
	}
	if len(runner.orphanResults) >= runner.maximumOrphans {
		return runner.reject(ctx, envelope, "result", "", "capacity",
			"action arbitration retains too many results awaiting candidate evidence")
	}
	copy := envelope.Clone()
	runner.orphanResults[runKey] = actionArbitrationResult{envelope: copy, value: result}
	return nil
}

func (runner *actionArbiterRunner) group(
	key, sessionID, triggerID string,
) (*actionArbitrationGroup, error) {
	if group := runner.groups[key]; group != nil {
		if group.sessionID != sessionID || group.triggerID != triggerID {
			return nil, errors.New("action arbitration group identity collision")
		}
		return group, nil
	}
	if len(runner.groups) >= runner.config.MaxPending {
		return nil, fmt.Errorf("action arbitration retains at most %d pending observations",
			runner.config.MaxPending)
	}
	group := &actionArbitrationGroup{
		sessionID: sessionID, triggerID: triggerID,
		members: make(map[string]*actionArbitrationMember, runner.expectedLanes),
	}
	runner.groups[key] = group
	return group, nil
}

func (runner *actionArbiterRunner) member(
	group *actionArbitrationGroup, runID string,
) (*actionArbitrationMember, error) {
	key := actionRunKey(group.sessionID, runID)
	member := group.members[key]
	if member == nil {
		if len(group.members) >= runner.expectedLanes {
			return nil, fmt.Errorf("action arbitration observation has more than %d cognition lanes",
				runner.expectedLanes)
		}
		member = &actionArbitrationMember{}
		group.members[key] = member
	}
	return member, nil
}

func (runner *actionArbiterRunner) selectProposal(
	ctx context.Context, groupKey string, group *actionArbitrationGroup,
	winnerRun string, proposal actionArbitrationProposal,
) error {
	for _, member := range group.members {
		if member.candidate == nil || member.candidate.value.RunID == winnerRun {
			continue
		}
		if err := runner.cancelRun(ctx, proposal.envelope, member.candidate.value.RunID,
			"another cognition lane won the action race"); err != nil {
			return err
		}
	}
	delete(runner.groups, groupKey)
	runner.terminalGroups.add(groupKey)
	runner.rememberTerminalRuns(group)
	if err := publishPayload(ctx, runner.emit, runner.selectedOutput, proposal.envelope,
		admittedType, cloneAdmitted(proposal.value), "selected"); err != nil {
		return err
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, proposal.envelope, Outcome{
		Kind: OutcomeSucceeded, Stage: "action_arbitration", Operation: "select",
		CallID: proposal.value.Proposal.Call.CallID, Code: "first_complete",
	})
}

func (runner *actionArbiterRunner) finishNoActionGroup(
	ctx context.Context, groupKey string, group *actionArbitrationGroup, cause element.Envelope,
) error {
	if len(group.members) < runner.expectedLanes {
		return nil
	}
	for _, member := range group.members {
		if member.candidate == nil || member.result == nil || len(member.result.value.ToolProposals) != 0 {
			return nil
		}
	}
	delete(runner.groups, groupKey)
	runner.terminalGroups.add(groupKey)
	runner.rememberTerminalRuns(group)
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, cause, Outcome{
		Kind: OutcomeSucceeded, Stage: "action_arbitration", Operation: "complete",
		Code: "no_action", Message: "every cognition lane completed without an action proposal",
	})
}

func (runner *actionArbiterRunner) rememberTerminalRuns(group *actionArbitrationGroup) {
	for runKey := range group.members {
		runner.terminalRuns.add(runKey)
	}
}

func (runner *actionArbiterRunner) cancelRun(
	ctx context.Context, parent element.Envelope, runID, reason string,
) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("action arbitration cannot cancel an empty cognition run")
	}
	runKey := actionRunKey(parent.SessionID, runID)
	if runner.canceledRuns.contains(runKey) {
		return nil
	}
	runner.canceledRuns.add(runKey)
	envelope, err := runner.emit.envelope(parent, cognitionelements.CancelType(), cognitionelements.Cancel{
		RunID: runID, Reason: reason,
	}, "cancel-upstream")
	if err != nil {
		return err
	}
	envelope.RunID = runID
	envelope.CancellationScope = runID
	return broadcast(ctx, runner.cancelOutput, envelope)
}

func (runner *actionArbiterRunner) reject(
	ctx context.Context, envelope element.Envelope, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: OutcomeRejected, Stage: "action_arbitration", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

func validateArbitrationBinding(
	candidate actionArbitrationCandidate, proposal actionArbitrationProposal,
) (string, error) {
	value := proposal.value
	if candidate.value.SessionID != value.SessionID || candidate.envelope.SessionID != value.SessionID ||
		proposal.envelope.SessionID != value.SessionID {
		return "session_mismatch", errors.New("action candidate and admitted proposal name different sessions")
	}
	if candidate.value.RunID != value.ModelRunID || candidate.envelope.RunID != value.ModelRunID ||
		proposal.envelope.RunID != value.ModelRunID {
		return "model_run_mismatch", errors.New("action candidate and admitted proposal name different cognition runs")
	}
	if candidate.envelope.ItemID != value.CandidateItemID ||
		!slices.Contains(proposal.envelope.CausalParents, value.CandidateItemID) {
		return "candidate_cause_mismatch", errors.New("admitted proposal is not causally bound to its action candidate")
	}
	if candidate.value.ActivationItemID != value.ActivationItemID ||
		candidate.value.ActivationCauseItemID != value.ActivationCauseItemID ||
		candidate.value.ObservationItemID != value.AuthorityItemID ||
		candidate.value.ObservationTriggerItemID != value.ObservationTriggerItemID ||
		candidate.value.SourceRevision != value.SourceRevision ||
		candidate.value.ContextVersion != value.ContextVersion ||
		candidate.value.ContextEnvelopeItemID != value.ContextEnvelopeItemID ||
		candidate.value.ContextTailItem != value.ContextTailItem {
		return "candidate_binding_mismatch", errors.New(
			"action candidate and admitted proposal name different activation or canonical context evidence")
	}
	return "", nil
}

func arbitrationResultPayload(payload any) (cognitionelements.Result, bool) {
	switch value := payload.(type) {
	case cognitionelements.Result:
		return value, true
	case *cognitionelements.Result:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Result{}, false
}

func arbitrationGroupKey(sessionID, observationTriggerID string) string {
	return actionScopeKey(sessionID, observationTriggerID, "action-arbitration")
}
