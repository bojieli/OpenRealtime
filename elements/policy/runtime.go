package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
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
		pending: make(map[string]pendingGeneration), terminal: make(map[string]struct{}),
		preCanceled: make(map[cancellationAddress]string),
	}, nil
}

type generationPorts struct {
	context, committed, cancel element.InputPort
	trigger, state, outcome    element.OutputPort
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
		{"context", &result.context}, {"committed", &result.committed}, {"cancel", &result.cancel},
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
		{"trigger", &result.trigger}, {"state", &result.state}, {"outcome", &result.outcome},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return generationPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type pendingGeneration struct {
	id     string
	cause  element.Envelope
	commit stateelements.ObservationCommitOutcome
}

type cancellationAddress struct {
	generationID string
	streamID     string
}

type generateOnObservationRunner struct {
	instance   string
	config     GenerateOnObservationConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      generationPorts

	hasContext      bool
	contextVersion  uint64
	contextEnvelope element.Envelope
	contextBinding  trajectoryContextBinding
	pending         map[string]pendingGeneration
	pendingOrder    []string
	terminal        map[string]struct{}
	terminalOrder   []string
	preCanceled     map[cancellationAddress]string
	preCancelOrder  []cancellationAddress
	state           GenerationState
}

// trajectoryContextBinding is the minimum immutable evidence needed to join a
// commit outcome to a State envelope. The policy deliberately does not retain
// the trajectory payload or conversation content.
type trajectoryContextBinding struct {
	version        uint64
	tailID         string
	tailKind       trajectory.Kind
	sourceRevision uint64
	eventID        string
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
		Role: runner.config.Role, MaxPending: runner.config.MaxPending,
		TerminalMemory:     runner.config.TerminalMemory,
		CancellationMemory: runner.config.CancelMemory,
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan generationInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"context", runner.ports.context}, {"committed", runner.ports.committed}, {"cancel", runner.ports.cancel},
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
			case "context":
				err = runner.acceptContext(ctx, input.envelope)
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

func (runner *generateOnObservationRunner) acceptContext(
	ctx context.Context, envelope element.Envelope,
) error {
	snapshot, ok := trajectorySnapshotPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_context", fmt.Sprintf("context payload has type %T", envelope.Payload))
	}
	if uint64(len(snapshot.Items)) != snapshot.Version {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_context", "trajectory snapshot version does not equal its item count")
	}
	if err := validatePolicyIdentifier("context item ID", envelope.ItemID, true); err != nil {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_context_identity", err.Error())
	}
	if runner.hasContext && snapshot.Version < runner.contextVersion {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"context_regression", fmt.Sprintf("context version regressed from %d to %d", runner.contextVersion, snapshot.Version))
	}
	if runner.hasContext && snapshot.Version == runner.contextVersion &&
		envelope.ItemID != runner.contextEnvelope.ItemID {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"context_identity_drift", fmt.Sprintf("context version %d changed identity from %q to %q",
				snapshot.Version, runner.contextEnvelope.ItemID, envelope.ItemID))
	}
	runner.hasContext = true
	runner.contextVersion = snapshot.Version
	runner.contextEnvelope = envelope.Clone()
	runner.contextEnvelope.Payload = nil
	runner.contextBinding = trajectoryContextBinding{version: snapshot.Version}
	if snapshot.Version > 0 {
		tail := snapshot.Items[snapshot.Version-1]
		runner.contextBinding.tailID = tail.ID
		runner.contextBinding.tailKind = tail.Kind
		runner.contextBinding.sourceRevision = tail.SourceRevision
		if tail.Event != nil {
			runner.contextBinding.eventID = tail.Event.EventID
		}
	}
	runner.state.ContextVersion = snapshot.Version
	if err := runner.releaseReady(ctx); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
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
	if err := validateCommit(commit); err != nil {
		return runner.refuse(ctx, envelope, "", commit, "invalid_commit", err.Error())
	}
	generationID := generationIdentifier(runner.config.Role, commit)
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
	if _, pending := runner.pending[generationID]; pending {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
			Kind: GenerationIgnored, GenerationID: generationID, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "duplicate_pending", Message: "observation commit already has a pending activation",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if reason, canceled := runner.takePreCancel(generationID, commit.StreamID); canceled {
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
	if len(runner.pending) >= runner.config.MaxPending {
		return runner.refuse(ctx, envelope, generationID, commit, "pending_limit",
			"generation policy pending bound reached")
	}
	runner.pending[generationID] = pendingGeneration{
		id: generationID, cause: envelope.Clone(), commit: commit,
	}
	runner.pendingOrder = append(runner.pendingOrder, generationID)
	if err := runner.releaseReady(ctx); err != nil {
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
	if (cancel.GenerationID == "") == (cancel.StreamID == "") {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{},
			"invalid_cancel", "cancel requires exactly one generation_id or stream_id")
	}
	address := cancellationAddress{generationID: cancel.GenerationID, streamID: cancel.StreamID}
	if cancel.GenerationID != "" {
		if err := validatePolicyIdentifier("generation ID", cancel.GenerationID, true); err != nil {
			return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{}, "invalid_cancel", err.Error())
		}
	} else if err := validatePolicyIdentifier("stream ID", cancel.StreamID, true); err != nil {
		return runner.refuse(ctx, envelope, "", stateelements.ObservationCommitOutcome{}, "invalid_cancel", err.Error())
	}
	cancel.Reason = boundedPolicyReason(cancel.Reason)
	matched := make([]string, 0)
	for _, generationID := range runner.pendingOrder {
		pending, found := runner.pending[generationID]
		if !found {
			continue
		}
		if address.generationID == generationID ||
			address.streamID != "" && address.streamID == pending.commit.StreamID {
			matched = append(matched, generationID)
		}
	}
	if len(matched) == 0 {
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
	for _, generationID := range matched {
		pending := runner.pending[generationID]
		runner.removePending(generationID)
		runner.rememberTerminal(generationID)
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, envelope, GenerationOutcome{
			Kind: GenerationCanceled, GenerationID: generationID,
			Role: runner.config.Role, StreamID: pending.commit.StreamID,
			SourceRevision: pending.commit.SourceRevision,
			ContextVersion: pending.commit.StoreVersion,
			TriggerItemID:  pending.commit.TriggerItemID,
			Code:           "canceled", Message: cancel.Reason,
		}); err != nil {
			return err
		}
	}
	return runner.publishState(ctx, envelope)
}

func (runner *generateOnObservationRunner) releaseReady(ctx context.Context) error {
	if !runner.hasContext {
		return nil
	}
	for _, generationID := range slices.Clone(runner.pendingOrder) {
		pending, found := runner.pending[generationID]
		if !found {
			continue
		}
		switch {
		case runner.contextVersion < pending.commit.StoreVersion:
			continue
		case runner.contextVersion > pending.commit.StoreVersion:
			runner.removePending(generationID)
			runner.rememberTerminal(generationID)
			runner.state.Refused++
			if err := runner.publishOutcome(ctx, pending.cause, GenerationOutcome{
				Kind: GenerationRefused, GenerationID: generationID, Role: runner.config.Role,
				StreamID: pending.commit.StreamID, SourceRevision: pending.commit.SourceRevision,
				ContextVersion: runner.contextVersion, TriggerItemID: pending.commit.TriggerItemID,
				Code: "context_version_passed",
				Message: fmt.Sprintf("required context version %d was passed by version %d",
					pending.commit.StoreVersion, runner.contextVersion),
			}); err != nil {
				return err
			}
		default:
			if err := validateContextCommit(runner.contextBinding, pending.commit); err != nil {
				runner.removePending(generationID)
				runner.rememberTerminal(generationID)
				runner.state.Refused++
				if publishErr := runner.publishOutcome(ctx, pending.cause, GenerationOutcome{
					Kind: GenerationRefused, GenerationID: generationID, Role: runner.config.Role,
					StreamID: pending.commit.StreamID, SourceRevision: pending.commit.SourceRevision,
					ContextVersion: pending.commit.StoreVersion,
					TriggerItemID:  pending.commit.TriggerItemID,
					Code:           "context_commit_mismatch", Message: err.Error(),
				}); publishErr != nil {
					return publishErr
				}
				continue
			}
			if err := runner.emit(ctx, pending); err != nil {
				return err
			}
		}
	}
	return nil
}

func (runner *generateOnObservationRunner) emit(
	ctx context.Context, pending pendingGeneration,
) error {
	invocation := cloneInvocation(runner.config.Invocation)
	invocation.SourceRevision = pending.commit.SourceRevision
	version := pending.commit.StoreVersion
	payload := cognitionelements.Generate{
		Invocation: invocation, ExpectedContextVersion: &version,
		ExpectedContextItemID: runner.contextEnvelope.ItemID,
	}
	envelope := pending.cause.Clone()
	envelope.Type = generationTriggerType
	envelope.ItemID = pending.id + ":trigger"
	envelope.RunID = pending.id
	envelope.CancellationScope = pending.id
	envelope.CausalParents = appendUnique(envelope.CausalParents, pending.cause.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, runner.contextEnvelope.ItemID)
	envelope.Payload = payload
	if _, err := runner.ports.trigger.Broadcast(ctx, envelope); err != nil {
		return err
	}
	runner.removePending(pending.id)
	runner.rememberTerminal(pending.id)
	runner.state.Emitted++
	return runner.publishOutcome(ctx, pending.cause, GenerationOutcome{
		Kind: GenerationEmitted, GenerationID: pending.id, Role: runner.config.Role,
		StreamID: pending.commit.StreamID, SourceRevision: pending.commit.SourceRevision,
		ContextVersion: pending.commit.StoreVersion, TriggerItemID: pending.commit.TriggerItemID,
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
	runner.state.Pending = make([]PendingGeneration, 0, len(runner.pending))
	for _, pending := range runner.pending {
		runner.state.Pending = append(runner.state.Pending, PendingGeneration{
			GenerationID: pending.id, StreamID: pending.commit.StreamID,
			ContextVersion: pending.commit.StoreVersion,
		})
	}
	sort.Slice(runner.state.Pending, func(left, right int) bool {
		return runner.state.Pending[left].GenerationID < runner.state.Pending[right].GenerationID
	})
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

func (runner *generateOnObservationRunner) removePending(generationID string) {
	delete(runner.pending, generationID)
	if index := slices.Index(runner.pendingOrder, generationID); index >= 0 {
		runner.pendingOrder = slices.Delete(runner.pendingOrder, index, index+1)
	}
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
	generationID, streamID string,
) (string, bool) {
	for _, address := range []cancellationAddress{{generationID: generationID}, {streamID: streamID}} {
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
	return validatePolicyIdentifier("commit stream ID", commit.StreamID, true)
}

// validateContextCommit binds typed commit evidence to the immutable item at
// the exact released trajectory version. A matching version number alone is
// insufficient: independently wired or compromised producers could otherwise
// activate a model against a different prefix with the same length.
func validateContextCommit(
	binding trajectoryContextBinding, commit stateelements.ObservationCommitOutcome,
) error {
	if binding.version != commit.StoreVersion {
		return fmt.Errorf("context version %d does not contain committed store version %d",
			binding.version, commit.StoreVersion)
	}
	if commit.StoreVersion == 0 {
		return errors.New("committed observation cannot bind to an empty context")
	}
	if binding.tailID != commit.TrajectoryItemID {
		return fmt.Errorf("context item %q does not match committed trajectory item %q",
			binding.tailID, commit.TrajectoryItemID)
	}
	if binding.tailKind != trajectory.KindObservation {
		return fmt.Errorf("committed trajectory item %q has kind %q, want observation",
			binding.tailID, binding.tailKind)
	}
	if binding.sourceRevision != commit.SourceRevision {
		return fmt.Errorf("context source revision %d does not match committed revision %d",
			binding.sourceRevision, commit.SourceRevision)
	}
	if binding.eventID != commit.TriggerItemID {
		return fmt.Errorf("context item %q does not attest commit trigger %q",
			binding.tailID, commit.TriggerItemID)
	}
	return nil
}

func generationIdentifier(role string, commit stateelements.ObservationCommitOutcome) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d",
		role, commit.StreamID, commit.TriggerItemID, commit.SourceRevision, commit.StoreVersion)))
	return "generation:sha256:" + hex.EncodeToString(digest[:])
}

func trajectorySnapshotPayload(payload any) (trajectory.Snapshot, bool) {
	switch value := payload.(type) {
	case trajectory.Snapshot:
		return value, true
	case *trajectory.Snapshot:
		if value != nil {
			return *value, true
		}
	}
	return trajectory.Snapshot{}, false
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
	return registry.RegisterArtifact("", inspect.ArtifactIdentity{
		ID: generationRuntimeID, Revision: policyImplementationRevision,
	}, generateOnObservationFactory{})
}

var (
	_ element.Factory         = generateOnObservationFactory{}
	_ element.ConfigValidator = generateOnObservationFactory{}
)
