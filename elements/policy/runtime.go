package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

type generateOnObservationFactory struct{}

func (generateOnObservationFactory) Descriptor() element.Descriptor {
	return GenerateOnObservationDescriptor()
}

func (generateOnObservationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeGenerateOnObservationConfig(source)
	return err
}

func (generateOnObservationFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeGenerateOnObservationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.GenerateOnObservation %s config: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("generation policy has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("generation policy has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := generationPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &generateOnObservationRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		resolution: mount.Resolution, ports: ports,
		terminal: make(map[string]struct{}), preCanceled: make(map[cancellationAddress]string),
	}, nil
}

type generationPorts struct {
	committed, cancel                  element.InputPort
	trigger, authority, state, outcome element.OutputPort
}

func generationPortsFrom(ports element.Ports) (generationPorts, error) {
	if ports == nil {
		return generationPorts{}, errors.New("policy.GenerateOnObservation has nil ports")
	}
	var result generationPorts
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{
		{"committed", &result.committed}, {"cancel", &result.cancel},
	} {
		port, err := ports.Input(input.name)
		if err != nil {
			return generationPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"trigger", &result.trigger}, {"authority", &result.authority},
		{"state", &result.state}, {"outcome", &result.outcome},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return generationPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type cancellationAddress struct {
	generationID string
	streamID     string
	sessionID    string
}

type generateOnObservationRunner struct {
	instance   string
	config     GenerateOnObservationConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      generationPorts

	terminal       map[string]struct{}
	terminalOrder  []string
	preCanceled    map[cancellationAddress]string
	preCancelOrder []cancellationAddress
	state          GenerationState
}

type generationInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *generateOnObservationRunner) Run(parent context.Context) error {
	artifact := liveidentity.Artifact{ID: generationRuntimeID, Revision: policyImplementationRevision}
	if err := liveidentity.Report(runner.resolution, artifact, nil); err != nil {
		return err
	}
	runner.state = GenerationState{
		Role: runner.config.Role, TerminalMemory: runner.config.TerminalMemory,
		CancellationMemory: runner.config.CancelMemory,
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan generationInput)
	failures := make(chan error, 2)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"committed", runner.ports.committed}, {"cancel", runner.ports.cancel},
	} {
		wait.Add(1)
		go receiveGenerationInputs(ctx, source.kind, source.port, inputs, failures, &wait)
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
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "committed":
				err = runner.acceptCommit(ctx, input.envelope)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown generation policy input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *generateOnObservationRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	commit, ok := observationCommitPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_commit", fmt.Sprintf("commit payload has type %T", envelope.Payload))
	}
	if commit.Kind != stateelements.ObservationCommitted {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
			Kind: GenerationIgnored, Role: runner.config.Role, StreamID: commit.StreamID,
			SourceRevision: commit.SourceRevision, ContextVersion: commit.StoreVersion,
			TriggerItemID: commit.TriggerItemID, Code: "observation_not_committed",
			Message: "observation commit outcome did not cross the trajectory boundary",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if envelope.SessionID == "" {
		return runner.refuse(ctx, envelope, "", commit, "missing_session",
			"observation commit requires a non-empty session ID")
	}
	if err := validatePolicyIdentifier("commit session ID", envelope.SessionID, true); err != nil {
		return runner.refuse(ctx, envelope, "", commit, "invalid_commit_envelope", err.Error())
	}
	if err := validatePolicyIdentifier("commit envelope item ID", envelope.ItemID, true); err != nil {
		return runner.refuse(ctx, envelope, "", commit, "invalid_commit_envelope", err.Error())
	}
	if err := validateCommit(commit); err != nil {
		return runner.refuse(ctx, envelope, "", commit, "invalid_commit", err.Error())
	}
	runner.state.ContextVersion = commit.StoreVersion
	generationID := generationIdentifier(runner.config.Role, envelope.SessionID, commit)
	if _, terminal := runner.terminal[generationID]; terminal {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
			Kind: GenerationIgnored, GenerationID: generationID, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "duplicate_commit", Message: "observation commit already reached a terminal activation decision",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if reason, canceled := runner.takePreCancel(generationID, commit.StreamID, envelope.SessionID); canceled {
		runner.rememberTerminal(generationID)
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
			Kind: GenerationCanceled, GenerationID: generationID, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "canceled", Message: reason,
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if err := runner.emit(ctx, envelope, generationID, commit); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *generateOnObservationRunner) acceptCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	cancel, ok := generationCancelPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_cancel", fmt.Sprintf("cancel payload has type %T", envelope.Payload))
	}
	if err := validatePolicyIdentifier("cancel item ID", envelope.ItemID, true); err != nil {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_cancel", err.Error())
	}
	if envelope.SessionID == "" {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"missing_session", "generation cancellation requires a non-empty session ID")
	}
	if err := validatePolicyIdentifier("cancel session ID", envelope.SessionID, true); err != nil {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_cancel", err.Error())
	}
	if (cancel.GenerationID == "") == (cancel.StreamID == "") {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_cancel", "cancel requires exactly one generation_id or stream_id")
	}
	address := cancellationAddress{
		generationID: cancel.GenerationID, streamID: cancel.StreamID, sessionID: envelope.SessionID,
	}
	if cancel.GenerationID != "" {
		if err := validatePolicyIdentifier("generation ID", cancel.GenerationID, true); err != nil {
			return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{}, "invalid_cancel", err.Error())
		}
	} else if err := validatePolicyIdentifier("stream ID", cancel.StreamID, true); err != nil {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{}, "invalid_cancel", err.Error())
	}
	cancel.Reason = boundedPolicyReason(cancel.Reason)
	runner.recordPreCancel(address, cancel.Reason)
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
		Kind: GenerationIgnored, GenerationID: cancel.GenerationID,
		Role: runner.config.Role, StreamID: cancel.StreamID,
		Code: "cancel_recorded", Message: cancel.Reason,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *generateOnObservationRunner) emit(
	ctx context.Context, cause element.Envelope, generationID string,
	commit stateelements.ObservationCommitOutcome,
) error {
	invocation := cloneInvocation(runner.config.Invocation)
	invocation.SourceRevision = commit.SourceRevision
	version := commit.Context.Prefix.Version
	committedContext := commit.Context
	payload := cognitionelements.Generate{
		Invocation: invocation, ExpectedContextVersion: &version,
		ExpectedContextItemID: commit.Context.StateItemID,
		CommittedContext:      &committedContext,
	}
	triggerEnvelope := cause.Clone()
	triggerEnvelope.Type = generationTriggerType
	triggerEnvelope.ItemID = generationID + ":trigger"
	triggerEnvelope.RunID = generationID
	triggerEnvelope.CancellationScope = generationID
	triggerEnvelope.CausalParents = appendUnique(triggerEnvelope.CausalParents, cause.ItemID)
	triggerEnvelope.CausalParents = appendUnique(triggerEnvelope.CausalParents, commit.Context.StateItemID)
	triggerEnvelope.CausalParents = appendUnique(triggerEnvelope.CausalParents, commit.TrajectoryItemID)
	triggerEnvelope.CausalParents = appendUnique(triggerEnvelope.CausalParents, commit.TriggerItemID)
	triggerEnvelope.Payload = payload
	candidateEnvelope := cause.Clone()
	candidateEnvelope.Type = authorityCandidateType
	candidateEnvelope.ItemID = generationID + ":authority"
	candidateEnvelope.RunID = generationID
	candidateEnvelope.CancellationScope = generationID
	candidateEnvelope.CausalParents = appendUnique(candidateEnvelope.CausalParents, cause.ItemID)
	candidateEnvelope.CausalParents = appendUnique(candidateEnvelope.CausalParents, commit.Context.StateItemID)
	candidateEnvelope.CausalParents = appendUnique(candidateEnvelope.CausalParents, commit.TrajectoryItemID)
	candidateEnvelope.CausalParents = appendUnique(candidateEnvelope.CausalParents, commit.TriggerItemID)
	candidateEnvelope.Payload = authority.Candidate{
		RunID: generationID, SessionID: cause.SessionID,
		ActivationItemID: triggerEnvelope.ItemID, ActivationCauseItemID: cause.ItemID,
		ObservationItemID:        commit.TrajectoryItemID,
		ObservationTriggerItemID: commit.TriggerItemID,
		SourceRevision:           commit.SourceRevision, ContextVersion: commit.StoreVersion,
		ContextEnvelopeItemID: commit.Context.StateItemID, ContextTailItem: commit.TrajectoryItemID,
	}
	if _, err := runner.ports.trigger.Broadcast(ctx, triggerEnvelope); err != nil {
		return err
	}
	// Authority follows the trigger. A trigger without authority is fail-closed;
	// authority naming a trigger that never crossed the boundary is unsafe.
	if _, err := runner.ports.authority.Broadcast(ctx, candidateEnvelope); err != nil {
		return err
	}
	runner.rememberTerminal(generationID)
	runner.state.Emitted++
	return runner.publishOutcome(ctx, cause, GenerationOutcome{
		Kind: GenerationEmitted, GenerationID: generationID, Role: runner.config.Role,
		StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
		ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
	})
}

func (runner *generateOnObservationRunner) refuse(
	ctx context.Context, cause element.Envelope, generationID string,
	commit stateelements.ObservationCommitOutcome, code, message string,
) error {
	if generationID != "" {
		runner.rememberTerminal(generationID)
	}
	runner.state.Refused++
	if err := runner.publishOutcome(ctx, cause, GenerationOutcome{
		Kind: GenerationRefused, GenerationID: generationID, Role: runner.config.Role,
		StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
		ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
		Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *generateOnObservationRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome GenerationOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = generationOutcomeType
	envelope.ItemID = fmt.Sprintf("%s:generation_outcome:%d", cause.ItemID, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *generateOnObservationRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = generationStateType
	envelope.ItemID = fmt.Sprintf("%s:state:%d", runner.instance, sequence)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	}
	envelope.Payload = runner.state
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *generateOnObservationRunner) rememberTerminal(generationID string) {
	if generationID == "" {
		return
	}
	if _, found := runner.terminal[generationID]; !found {
		runner.terminal[generationID] = struct{}{}
		runner.terminalOrder = append(runner.terminalOrder, generationID)
	}
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *generateOnObservationRunner) recordPreCancel(
	address cancellationAddress, reason string,
) {
	if _, found := runner.preCanceled[address]; !found {
		runner.preCancelOrder = append(runner.preCancelOrder, address)
	}
	runner.preCanceled[address] = boundedPolicyReason(reason)
	for len(runner.preCancelOrder) > runner.config.CancelMemory {
		oldest := runner.preCancelOrder[0]
		runner.preCancelOrder = runner.preCancelOrder[1:]
		delete(runner.preCanceled, oldest)
	}
}

func (runner *generateOnObservationRunner) takePreCancel(
	generationID, streamID, sessionID string,
) (string, bool) {
	for _, address := range []cancellationAddress{
		{generationID: generationID, sessionID: sessionID},
		{streamID: streamID, sessionID: sessionID},
	} {
		reason, found := runner.preCanceled[address]
		if !found {
			continue
		}
		delete(runner.preCanceled, address)
		if index := slices.Index(runner.preCancelOrder, address); index >= 0 {
			runner.preCancelOrder = slices.Delete(runner.preCancelOrder, index, index+1)
		}
		return reason, true
	}
	return "", false
}

func validateCommit(commit stateelements.ObservationCommitOutcome) error {
	if commit.StoreVersion == 0 || commit.SourceRevision == 0 || commit.ObservationRevision == 0 {
		return errors.New("committed observation requires positive store, source, and observation revisions")
	}
	if err := validatePolicyIdentifier("commit trigger item ID", commit.TriggerItemID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("commit trajectory item ID", commit.TrajectoryItemID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("commit stream ID", commit.StreamID, true); err != nil {
		return err
	}
	if commit.Context.Prefix.Version != commit.StoreVersion {
		return fmt.Errorf("committed context prefix version %d does not match store version %d",
			commit.Context.Prefix.Version, commit.StoreVersion)
	}
	if err := validateCanonicalPrefixDigest(commit.Context.Prefix.Digest); err != nil {
		return err
	}
	return validatePolicyIdentifier("committed context State item ID", commit.Context.StateItemID, true)
}

func validateCanonicalPrefixDigest(digest string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) ||
		len(digest) != len(prefix)+sha256.Size*2 || digest != strings.ToLower(digest) {
		return errors.New("committed context prefix digest is not canonical SHA-256")
	}
	if _, err := hex.DecodeString(digest[len(prefix):]); err != nil {
		return errors.New("committed context prefix digest is not canonical SHA-256")
	}
	return nil
}

func generationIdentifier(
	role, sessionID string, commit stateelements.ObservationCommitOutcome,
) string {
	hash := sha256.New()
	for _, identity := range []string{role, sessionID, commit.StreamID, commit.TriggerItemID} {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	_, _ = fmt.Fprintf(hash, "%d:%d", commit.SourceRevision, commit.StoreVersion)
	return "generation:sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func observationCommitPayload(payload any) (stateelements.ObservationCommitOutcome, bool) {
	switch value := payload.(type) {
	case stateelements.ObservationCommitOutcome:
		return value, true
	case *stateelements.ObservationCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.ObservationCommitOutcome{}, false
}

func generationCancelPayload(payload any) (GenerationCancel, bool) {
	switch value := payload.(type) {
	case GenerationCancel:
		return value, true
	case *GenerationCancel:
		if value != nil {
			return *value, true
		}
	}
	return GenerationCancel{}, false
}

func receiveGenerationInputs(
	ctx context.Context, kind string, input element.InputPort,
	output chan<- generationInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if generationTerminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive generation policy %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- generationInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func generationTerminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register policy factories: nil registry")
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	return factoryprofile.Registrations(
		factoryprofile.Entry{
			Factory: generateOnObservationFactory{}, Artifact: inspect.ArtifactIdentity{
				ID: generationRuntimeID, Revision: policyImplementationRevision,
			},
		},
		factoryprofile.Entry{
			Factory: sessionInvocationFactory{}, Artifact: inspect.ArtifactIdentity{
				ID: sessionInvocationRuntimeID, Revision: sessionInvocationRuntimeRevision,
			},
		},
		factoryprofile.Entry{
			Factory: semanticAdmissionFactory{}, Artifact: inspect.ArtifactIdentity{
				ID: semanticAdmissionRuntimeID, Revision: semanticAdmissionRuntimeRevision,
			},
		},
		factoryprofile.Entry{
			Factory: temporalEvidenceAdmissionFactory{}, Artifact: inspect.ArtifactIdentity{
				ID: temporalEvidenceRuntimeID, Revision: temporalEvidenceRuntimeRevision,
			},
		},
	)
}

var (
	_ element.Factory         = generateOnObservationFactory{}
	_ element.ConfigValidator = generateOnObservationFactory{}
)
