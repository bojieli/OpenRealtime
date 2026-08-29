package action

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type proposalAdmissionFactory struct{}

var (
	_ element.Factory         = proposalAdmissionFactory{}
	_ element.ConfigValidator = proposalAdmissionFactory{}
)

func (proposalAdmissionFactory) Descriptor() element.Descriptor { return ProposalAdmissionDescriptor() }
func (proposalAdmissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeProposalAdmissionConfig(source)
	return err
}

func (proposalAdmissionFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeProposalAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("authority.ProposalAdmission %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf("authority.ProposalAdmission %s has no trajectory store service", mount.InstanceID)
	}
	store, ok := service.(*trajectory.Store)
	if !ok || store == nil {
		return nil, fmt.Errorf("action trajectory store service has type %T", service)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	proposalInput, err := mount.Ports.Input("proposal")
	if err != nil {
		return nil, err
	}
	provenanceInput, err := mount.Ports.Input("provenance")
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
	admittedOutput, err := mount.Ports.Output("admitted")
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
	return &proposalAdmissionRunner{
		config: config, store: store, serviceRevision: serviceRevision,
		emit:          emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		proposalInput: proposalInput, provenanceInput: provenanceInput, cancelInput: cancelInput,
		timeoutInput: timeoutInput, admittedOutput: admittedOutput, outcomeOutput: outcomeOutput,
		resolvedOutput: resolvedOutput, pending: make(map[string]*pendingAdmission),
		terminal: newBoundedSet(config.MaxPending), resolution: mount.Resolution,
	}, nil
}

type pendingAdmission struct {
	proposal        *element.Envelope
	proposalValue   cognitionelements.ToolProposal
	provenance      *element.Envelope
	provenanceValue Provenance
}

type proposalAdmissionRunner struct {
	config          ProposalAdmissionConfig
	store           *trajectory.Store
	serviceRevision uint64
	emit            emitter

	proposalInput   element.InputPort
	provenanceInput element.InputPort
	cancelInput     element.InputPort
	timeoutInput    element.InputPort
	admittedOutput  element.OutputPort
	outcomeOutput   element.OutputPort
	resolvedOutput  element.OutputPort

	pending    map[string]*pendingAdmission
	terminal   *boundedSet
	resolution element.ResolutionReporter
}

func (runner *proposalAdmissionRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "proposal_admission", Reference: TrajectoryStoreService,
		Identity: "trajectory.Store", ServiceRevision: runner.serviceRevision,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ProposalAdmissionDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"trajectory", "trajectory.Store/v1",
			"go://github.com/bojieli/OpenRealtime/trajectory/Store",
			runner.serviceRevision, "",
		)}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 4)
	var wait sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{
		{"proposal", runner.proposalInput}, {"provenance", runner.provenanceInput},
		{"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput},
	} {
		wait.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &wait)
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
			case "proposal":
				err = runner.acceptProposal(ctx, input.envelope)
			case "provenance":
				err = runner.acceptProvenance(ctx, input.envelope)
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

func (runner *proposalAdmissionRunner) acceptProposal(ctx context.Context, envelope element.Envelope) error {
	proposal, ok := proposalPayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "proposal",
			Code: "invalid_payload", Message: fmt.Sprintf("proposal payload has type %T", envelope.Payload),
		})
	}
	callID := strings.TrimSpace(proposal.Call.CallID)
	if err := validateProposal(proposal); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "proposal", CallID: callID,
			Code: "invalid_proposal", Message: err.Error(),
		})
	}
	if strings.TrimSpace(envelope.RunID) == "" {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "proposal", CallID: callID,
			Code: "missing_model_run", Message: "proposal envelope requires the cognition run ID",
		})
	}
	if runner.terminal.contains(callID) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "proposal_admission", Operation: "proposal", CallID: callID,
			Code: "terminal_replay", Message: "proposal call ID is already terminal",
		})
	}
	pending, err := runner.pendingFor(callID)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "proposal", CallID: callID,
			Code: "capacity", Message: err.Error(),
		})
	}
	if pending.proposal != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "proposal", CallID: callID,
			Code: "duplicate_proposal", Message: "proposal call ID is already pending",
		})
	}
	copy := envelope.Clone()
	pending.proposal = &copy
	pending.proposalValue = cloneProposal(proposal)
	return runner.tryAdmit(ctx, callID)
}

func (runner *proposalAdmissionRunner) acceptProvenance(ctx context.Context, envelope element.Envelope) error {
	provenance, ok := provenancePayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "provenance",
			Code: "invalid_payload", Message: fmt.Sprintf("provenance payload has type %T", envelope.Payload),
		})
	}
	provenance.CallID = strings.TrimSpace(provenance.CallID)
	provenance.ProposalItemID = strings.TrimSpace(provenance.ProposalItemID)
	provenance.ModelRunID = strings.TrimSpace(provenance.ModelRunID)
	provenance.SessionID = strings.TrimSpace(provenance.SessionID)
	provenance.TrajectoryItem = strings.TrimSpace(provenance.TrajectoryItem)
	provenance.ContextTailItem = strings.TrimSpace(provenance.ContextTailItem)
	if provenance.CallID == "" || provenance.ProposalItemID == "" ||
		provenance.ModelRunID == "" || provenance.TrajectoryItem == "" ||
		provenance.ContextVersion == 0 || provenance.ContextTailItem == "" {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "provenance", CallID: provenance.CallID,
			Code: "invalid_provenance", Message: "provenance requires proposal, run, context, and trajectory identities",
		})
	}
	if runner.terminal.contains(provenance.CallID) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "proposal_admission", Operation: "provenance", CallID: provenance.CallID,
			Code: "terminal_replay", Message: "proposal call ID is already terminal",
		})
	}
	pending, err := runner.pendingFor(provenance.CallID)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "provenance", CallID: provenance.CallID,
			Code: "capacity", Message: err.Error(),
		})
	}
	if pending.provenance != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "provenance", CallID: provenance.CallID,
			Code: "duplicate_provenance", Message: "proposal call ID already has provenance",
		})
	}
	copy := envelope.Clone()
	pending.provenance = &copy
	pending.provenanceValue = provenance
	return runner.tryAdmit(ctx, provenance.CallID)
}

func (runner *proposalAdmissionRunner) pendingFor(callID string) (*pendingAdmission, error) {
	if pending := runner.pending[callID]; pending != nil {
		return pending, nil
	}
	if len(runner.pending) >= runner.config.MaxPending {
		return nil, fmt.Errorf("proposal admission has %d pending joins", len(runner.pending))
	}
	pending := &pendingAdmission{}
	runner.pending[callID] = pending
	return pending, nil
}

func (runner *proposalAdmissionRunner) tryAdmit(ctx context.Context, callID string) error {
	pending := runner.pending[callID]
	if pending == nil || pending.proposal == nil || pending.provenance == nil {
		return nil
	}
	delete(runner.pending, callID)
	runner.terminal.add(callID)
	proposal := cloneProposal(pending.proposalValue)
	snapshot := runner.store.Snapshot()
	if code, err := validateProvenanceBinding(*pending.proposal, *pending.provenance,
		pending.provenanceValue, snapshot); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, *pending.provenance, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "admit", CallID: callID,
			Code: code, Message: err.Error(),
		})
	}
	canonical, found := canonicalItem(snapshot.Items[:pending.provenanceValue.ContextVersion],
		pending.provenanceValue.TrajectoryItem)
	if !found {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, *pending.provenance, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "admit", CallID: callID,
			Code: "unknown_provenance", Message: "provenance does not name a canonical trajectory item",
		})
	}
	if expected := pending.provenanceValue.SourceRevision; canonical.SourceRevision != expected {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, *pending.provenance, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "admit", CallID: callID,
			Code: "source_revision_mismatch", Message: fmt.Sprintf("canonical source revision is %d, claim requires %d", canonical.SourceRevision, expected),
		})
	}
	authority := trajectory.AuthorityOf(canonical)
	allowed := canonical.Kind == trajectory.KindObservation && authority == trajectory.AuthorityUser
	if runner.config.AllowSystem {
		allowed = allowed || canonical.Kind == trajectory.KindInstruction && authority == trajectory.AuthoritySystem
	}
	if !allowed {
		code := "insufficient_authority"
		if authority == trajectory.AuthorityObserver {
			code = "observer_authority"
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, *pending.provenance, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: "admit", CallID: callID,
			Code: code, Message: fmt.Sprintf("canonical item %q carries %q authority", canonical.ID, authority),
		})
	}
	parent := pending.proposal.Clone()
	parent.CausalParents = appendUnique(parent.CausalParents, pending.provenance.ItemID)
	parent.CausalParents = appendUnique(parent.CausalParents, canonical.ID)
	admitted := AdmittedProposal{
		Proposal: proposal, ProposalItemID: pending.proposal.ItemID,
		ModelRunID: pending.proposal.RunID, SessionID: pending.proposal.SessionID,
		Authority: authority, AuthorityItemID: canonical.ID, SourceRevision: canonical.SourceRevision,
		ContextVersion:  pending.provenanceValue.ContextVersion,
		ContextTailItem: pending.provenanceValue.ContextTailItem,
	}
	if err := publishPayload(ctx, runner.emit, runner.admittedOutput, parent, admittedType, admitted, "admitted"); err != nil {
		return err
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, parent, Outcome{
		Kind: OutcomeSucceeded, Stage: "proposal_admission", Operation: "admit", CallID: callID,
	})
}

func canonicalItem(items []trajectory.Item, id string) (trajectory.Item, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return trajectory.Item{}, false
}

func validateProvenanceBinding(
	proposalEnvelope, provenanceEnvelope element.Envelope, provenance Provenance,
	snapshot trajectory.Snapshot,
) (string, error) {
	if provenance.ProposalItemID != proposalEnvelope.ItemID {
		return "proposal_identity_mismatch", fmt.Errorf("provenance names proposal %q, received %q",
			provenance.ProposalItemID, proposalEnvelope.ItemID)
	}
	if provenance.ModelRunID != proposalEnvelope.RunID || provenanceEnvelope.RunID != proposalEnvelope.RunID {
		return "model_run_mismatch", fmt.Errorf("proposal and provenance do not name one cognition run")
	}
	if provenance.SessionID != proposalEnvelope.SessionID ||
		provenanceEnvelope.SessionID != proposalEnvelope.SessionID {
		return "session_mismatch", fmt.Errorf("proposal provenance crossed a session boundary")
	}
	if !slices.Contains(provenanceEnvelope.CausalParents, proposalEnvelope.ItemID) {
		return "missing_proposal_cause", fmt.Errorf("provenance is not causally linked to proposal %q",
			proposalEnvelope.ItemID)
	}
	if provenance.ContextVersion > snapshot.Version ||
		provenance.ContextVersion > uint64(len(snapshot.Items)) {
		return "context_version_mismatch", fmt.Errorf("provenance context version %d exceeds canonical version %d",
			provenance.ContextVersion, snapshot.Version)
	}
	prefix := snapshot.Items[:provenance.ContextVersion]
	if prefix[len(prefix)-1].ID != provenance.ContextTailItem {
		return "context_tail_mismatch", fmt.Errorf("canonical context tail is %q, provenance names %q",
			prefix[len(prefix)-1].ID, provenance.ContextTailItem)
	}
	if !slices.Contains(proposalEnvelope.CausalParents, provenance.ContextTailItem) {
		return "proposal_context_mismatch", fmt.Errorf("proposal is not bound to context tail %q",
			provenance.ContextTailItem)
	}
	if !causalAncestor(prefix, provenance.TrajectoryItem, provenance.ContextTailItem) {
		return "authority_not_causal", fmt.Errorf("authority item %q is not an ancestor of context tail %q",
			provenance.TrajectoryItem, provenance.ContextTailItem)
	}
	return "", nil
}

func causalAncestor(items []trajectory.Item, ancestor, tail string) bool {
	byID := make(map[string]trajectory.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	pending := []string{tail}
	seen := make(map[string]struct{}, len(items))
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == ancestor {
			return true
		}
		if _, visited := seen[id]; visited {
			continue
		}
		seen[id] = struct{}{}
		item, found := byID[id]
		if !found {
			continue
		}
		pending = append(pending, item.CausalParentIDs...)
	}
	return false
}

func (runner *proposalAdmissionRunner) interrupt(ctx context.Context, envelope element.Envelope, operation string) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "proposal_admission", Operation: operation, Code: code, Message: err.Error(),
		})
	}
	_, pending := runner.pending[interrupt.CallID]
	delete(runner.pending, interrupt.CallID)
	wasTerminal := runner.terminal.contains(interrupt.CallID)
	runner.terminal.add(interrupt.CallID)
	kind := OutcomeCanceled
	if operation == "timeout" {
		kind = OutcomeTimedOut
	}
	if !pending && wasTerminal {
		kind = OutcomeIgnored
		code = "already_terminal"
	}
	if interrupt.Reason == "" {
		interrupt.Reason = operation
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: kind, Stage: "proposal_admission", Operation: operation, CallID: interrupt.CallID,
		Code: code, Message: interrupt.Reason,
	})
}
