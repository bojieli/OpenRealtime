package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var (
	observationRevisionType = element.Revisions(
		element.Named("perception.Observation"), element.Named("perception.RevisionID"),
	)
	observationCommitOutcomeType = element.Event(element.Named("trajectory.ObservationCommitOutcome"))
)

func ObservationType() element.Type              { return observationRevisionType.Clone() }
func ObservationCommitOutcomeType() element.Type { return observationCommitOutcomeType.Clone() }

// ObservationCommitDescriptor converts provenance-carrying perception
// revisions into atomic trajectory append transactions. Store replies feed
// back explicitly, so later revisions cannot claim a predecessor committed
// until the authoritative store actually accepted it.
func ObservationCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "state.ObservationCommit",
		Revision:      2,
		Ports: []element.Port{
			{Name: "observations", Direction: element.Input, Type: observationRevisionType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "committed", Direction: element.Input, Type: commitType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "rejected", Direction: element.Input, Type: rejectionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "append", Direction: element.Output, Type: appendType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "outcome", Direction: element.Output, Type: observationCommitOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"observations", "committed", "rejected"},
			Outcomes: []string{"append", "outcome"}, MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/trajectory/observation-commit-state/v1",
		ConfigSchema: "schema://openrealtime/trajectory/observation-commit-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
	}
}

type ObservationCommitConfig struct {
	RevisionNamespace string `json:"revision_namespace,omitempty"`
}

type ObservationCommitOutcomeKind string

const (
	ObservationCommitted ObservationCommitOutcomeKind = "committed"
	ObservationRejected  ObservationCommitOutcomeKind = "rejected"
)

type ObservationCommitOutcome struct {
	Kind                ObservationCommitOutcomeKind `json:"kind"`
	TriggerItemID       string                       `json:"trigger_item_id"`
	TrajectoryItemID    string                       `json:"trajectory_item_id,omitempty"`
	StreamID            string                       `json:"stream_id,omitempty"`
	ObservationRevision uint64                       `json:"observation_revision,omitempty"`
	SourceRevision      uint64                       `json:"source_revision,omitempty"`
	StoreVersion        uint64                       `json:"store_version,omitempty"`
	Context             CommittedContext             `json:"context,omitzero"`
	Code                string                       `json:"code,omitempty"`
	Message             string                       `json:"message,omitempty"`
}

type observationCommitFactory struct{}

func (observationCommitFactory) Descriptor() element.Descriptor {
	return ObservationCommitDescriptor()
}

func (observationCommitFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeObservationCommitConfig(source)
	return err
}

func (observationCommitFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeObservationCommitConfig(mount.Config)
	if err != nil {
		return nil, err
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("observation commit requires the runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("observation commit requires the runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	observations, err := mount.Ports.Input("observations")
	if err != nil {
		return nil, err
	}
	committed, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	rejected, err := mount.Ports.Input("rejected")
	if err != nil {
		return nil, err
	}
	appendOutput, err := mount.Ports.Output("append")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &observationCommitRunner{
		instance: mount.InstanceID, namespace: config.RevisionNamespace,
		clock: clock, sequences: sequences,
		observations: observations, committed: committed, rejected: rejected,
		appendOutput: appendOutput, outcomeOutput: outcomeOutput,
		resolution:         mount.Resolution,
		pending:            make(map[string]pendingObservationCommit),
		pendingStream:      make(map[string]string),
		committedRevisions: make(map[string]map[uint64]committedObservation),
		waiting:            make(map[string][]element.Envelope),
	}, nil
}

func decodeObservationCommitConfig(source json.RawMessage) (ObservationCommitConfig, error) {
	config := ObservationCommitConfig{RevisionNamespace: "trajectory.source_revision"}
	if err := elementconfig.Decode(source, &config); err != nil {
		return ObservationCommitConfig{}, err
	}
	config.RevisionNamespace = strings.TrimSpace(config.RevisionNamespace)
	if config.RevisionNamespace == "" {
		return ObservationCommitConfig{}, errors.New("observation commit revision namespace is empty")
	}
	return config, nil
}

type observationCommitInputKind string

const (
	observationInput observationCommitInputKind = "observation"
	commitInput      observationCommitInputKind = "committed"
	rejectionInput   observationCommitInputKind = "rejected"
)

type observationCommitInput struct {
	kind     observationCommitInputKind
	envelope element.Envelope
}

type committedObservation struct {
	canonicalRevision uint64
	itemID            string
}

type pendingObservationCommit struct {
	trigger             element.Envelope
	streamID            string
	observation         perception.Observation
	canonicalRevision   uint64
	trajectoryItemID    string
	trajectoryItem      trajectory.Item
	supersededCanonical uint64
}

type observationCommitRunner struct {
	instance  string
	namespace string
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator

	observations  element.InputPort
	committed     element.InputPort
	rejected      element.InputPort
	appendOutput  element.OutputPort
	outcomeOutput element.OutputPort
	resolution    element.ResolutionReporter

	pending            map[string]pendingObservationCommit
	pendingStream      map[string]string
	committedRevisions map[string]map[uint64]committedObservation
	waiting            map[string][]element.Envelope
}

func (runner *observationCommitRunner) Run(ctx context.Context) error {
	if err := reportStateResolution(runner.resolution, ObservationCommitDescriptor()); err != nil {
		return err
	}
	inputs := make(chan observationCommitInput)
	failures := make(chan error, 3)
	ctx, cancel := context.WithCancelCause(ctx)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind observationCommitInputKind
		port element.InputPort
	}{
		{observationInput, runner.observations}, {commitInput, runner.committed},
		{rejectionInput, runner.rejected},
	} {
		wait.Add(1)
		go receiveObservationCommitInput(ctx, source.kind, source.port, inputs, failures, &wait)
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
			case observationInput:
				err = runner.acceptObservation(ctx, input.envelope)
			case commitInput:
				err = runner.acceptCommit(ctx, input.envelope)
			case rejectionInput:
				err = runner.acceptRejection(ctx, input.envelope)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *observationCommitRunner) acceptObservation(
	ctx context.Context, envelope element.Envelope,
) error {
	observation, ok := observationPayload(envelope.Payload)
	streamID := observationStreamID(envelope, observation)
	if !ok {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			Code: "invalid_payload", Message: fmt.Sprintf("observation payload has type %T", envelope.Payload),
		})
	}
	if err := observation.Validate(); err != nil {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			ObservationRevision: observation.Revision, Code: "invalid_observation", Message: err.Error(),
		})
	}
	if observation.Revision == 0 {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			Code: "missing_revision", Message: "observation revision must be positive",
		})
	}
	if envelope.SessionID == "" {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			ObservationRevision: observation.Revision,
			Code:                "missing_session", Message: "observation commit requires a non-empty session ID",
		})
	}
	if !canonicalObservationSession(envelope.SessionID) {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			ObservationRevision: observation.Revision,
			Code:                "invalid_session", Message: "observation commit session ID is not canonical",
		})
	}
	if requestID := runner.pendingStream[streamID]; requestID != "" {
		runner.waiting[streamID] = append(runner.waiting[streamID], envelope.Clone())
		return nil
	}
	return runner.startAppend(ctx, envelope, observation, streamID)
}

func (runner *observationCommitRunner) startAppend(
	ctx context.Context, trigger element.Envelope, observation perception.Observation, streamID string,
) error {
	canonicalRevision, err := runner.sequences.Next(runner.namespace)
	if err != nil {
		return err
	}
	var superseded committedObservation
	if observation.Supersedes != 0 {
		superseded = runner.committedRevisions[streamID][observation.Supersedes]
		if superseded.canonicalRevision == 0 {
			return runner.publishOutcome(ctx, trigger, ObservationCommitOutcome{
				Kind: ObservationRejected, TriggerItemID: trigger.ItemID, StreamID: streamID,
				ObservationRevision: observation.Revision, SourceRevision: canonicalRevision,
				Code:    "unknown_superseded_revision",
				Message: fmt.Sprintf("stream %q has no committed observation revision %d", streamID, observation.Supersedes),
			})
		}
	}
	trajectoryItemID := fmt.Sprintf("%s-observation-%d", runner.instance, canonicalRevision)
	item := trajectory.Item{
		ID: trajectoryItemID, Kind: trajectory.KindObservation,
		MonotonicNS: runner.clock.NowNS(), SourceRevision: canonicalRevision,
		Producer: observation.Producer(), Content: observation.Text, Observation: observation.Meta(),
		Event: &trajectory.EventMetadata{
			EventID: trigger.ItemID, Type: observationEventType(observation),
			Source: observation.Observer, Channel: observationChannel(observation, trigger),
			OccurredNS:         observation.OccurredNS,
			CorrelationID:      firstNonempty(trigger.OpportunityID, trigger.SourceID, streamID),
			SupersedesRevision: superseded.canonicalRevision,
		},
	}
	if superseded.itemID != "" {
		item.CausalParentIDs = []string{superseded.itemID}
	}
	requestID := fmt.Sprintf("%s-append-%d", runner.instance, canonicalRevision)
	pending := pendingObservationCommit{
		trigger: trigger.Clone(), streamID: streamID, observation: observation,
		canonicalRevision: canonicalRevision, trajectoryItemID: trajectoryItemID, trajectoryItem: item,
		supersededCanonical: superseded.canonicalRevision,
	}
	runner.pending[requestID] = pending
	runner.pendingStream[streamID] = requestID
	appendEnvelope := trigger.Clone()
	appendEnvelope.Type = appendType
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, trigger.ItemID)
	appendEnvelope.Payload = Append{Items: []trajectory.Item{item}}
	result, err := runner.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		delete(runner.pending, requestID)
		delete(runner.pendingStream, streamID)
		return err
	}
	if result.Delivered != 1 {
		delete(runner.pending, requestID)
		delete(runner.pendingStream, streamID)
		return fmt.Errorf("observation append %s delivered to %d lanes", requestID, result.Delivered)
	}
	return nil
}

func (runner *observationCommitRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, pending, found := runner.pendingForReply(envelope)
	if !found {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID,
			Code: "unknown_commit_reply", Message: "trajectory commit reply has no pending request",
		})
	}
	commit, ok := commitPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("trajectory commit reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if err := validateObservationCommitReply(envelope, commit, pending); err != nil {
		return fmt.Errorf("trajectory commit reply %s: %w", envelope.ItemID, err)
	}
	runner.resolvePending(requestID, pending)
	byRevision := runner.committedRevisions[pending.streamID]
	if byRevision == nil {
		byRevision = make(map[uint64]committedObservation)
		runner.committedRevisions[pending.streamID] = byRevision
	}
	byRevision[pending.observation.Revision] = committedObservation{
		canonicalRevision: pending.canonicalRevision, itemID: pending.trajectoryItemID,
	}
	if err := runner.publishOutcome(ctx, pending.trigger, ObservationCommitOutcome{
		Kind: ObservationCommitted, TriggerItemID: pending.trigger.ItemID,
		TrajectoryItemID: pending.trajectoryItemID, StreamID: pending.streamID,
		ObservationRevision: pending.observation.Revision, SourceRevision: pending.canonicalRevision,
		StoreVersion: commit.Version, Context: commit.Context,
	}); err != nil {
		return err
	}
	if pending.observation.Final {
		delete(runner.committedRevisions, pending.streamID)
		return runner.rejectWaiting(ctx, pending.streamID, "stream_finalized", "observation arrived after a final revision")
	}
	return runner.startNextWaiting(ctx, pending.streamID)
}

func (runner *observationCommitRunner) acceptRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, pending, found := runner.pendingForReply(envelope)
	if !found {
		return runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID,
			Code: "unknown_rejection_reply", Message: "trajectory rejection reply has no pending request",
		})
	}
	rejection, ok := rejectionPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("trajectory rejection reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	runner.resolvePending(requestID, pending)
	if err := runner.publishOutcome(ctx, pending.trigger, ObservationCommitOutcome{
		Kind: ObservationRejected, TriggerItemID: pending.trigger.ItemID,
		TrajectoryItemID: pending.trajectoryItemID, StreamID: pending.streamID,
		ObservationRevision: pending.observation.Revision, SourceRevision: pending.canonicalRevision,
		StoreVersion: rejection.CurrentVersion, Code: rejection.Code, Message: rejection.Message,
	}); err != nil {
		return err
	}
	return runner.rejectWaiting(ctx, pending.streamID, "predecessor_rejected",
		"an earlier observation in this revision stream was rejected")
}

func (runner *observationCommitRunner) resolvePending(
	requestID string, pending pendingObservationCommit,
) {
	delete(runner.pending, requestID)
	if runner.pendingStream[pending.streamID] == requestID {
		delete(runner.pendingStream, pending.streamID)
	}
}

func (runner *observationCommitRunner) startNextWaiting(ctx context.Context, streamID string) error {
	waiting := runner.waiting[streamID]
	if len(waiting) == 0 {
		delete(runner.waiting, streamID)
		return nil
	}
	next := waiting[0]
	if len(waiting) == 1 {
		delete(runner.waiting, streamID)
	} else {
		runner.waiting[streamID] = waiting[1:]
	}
	return runner.acceptObservation(ctx, next)
}

func (runner *observationCommitRunner) rejectWaiting(
	ctx context.Context, streamID, code, message string,
) error {
	waiting := runner.waiting[streamID]
	delete(runner.waiting, streamID)
	for _, envelope := range waiting {
		observation, _ := observationPayload(envelope.Payload)
		if err := runner.publishOutcome(ctx, envelope, ObservationCommitOutcome{
			Kind: ObservationRejected, TriggerItemID: envelope.ItemID, StreamID: streamID,
			ObservationRevision: observation.Revision, Code: code, Message: message,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (runner *observationCommitRunner) pendingForReply(
	envelope element.Envelope,
) (string, pendingObservationCommit, bool) {
	for _, candidate := range append(slices.Clone(envelope.CausalParents), replyBaseID(envelope.ItemID)) {
		if pending, found := runner.pending[candidate]; found {
			return candidate, pending, true
		}
	}
	return "", pendingObservationCommit{}, false
}

func (runner *observationCommitRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome ObservationCommitOutcome,
) error {
	envelope := cause.Clone()
	envelope.Type = observationCommitOutcomeType
	envelope.ItemID = cause.ItemID + ":observation_commit"
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	if outcome.Context.StateItemID != "" {
		envelope.CausalParents = appendUnique(envelope.CausalParents, outcome.Context.StateItemID)
	}
	envelope.Payload = outcome
	_, err := runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func validateObservationCommitReply(
	envelope element.Envelope, commit Commit, pending pendingObservationCommit,
) error {
	if !canonicalObservationSession(pending.trigger.SessionID) ||
		envelope.SessionID != pending.trigger.SessionID {
		return fmt.Errorf(
			"session %q does not match pending observation session %q",
			envelope.SessionID, pending.trigger.SessionID,
		)
	}
	if len(commit.AppendedIDs) != 1 || commit.AppendedIDs[0] != pending.trajectoryItemID {
		return fmt.Errorf(
			"appended IDs %v do not exactly attest pending observation %q",
			commit.AppendedIDs, pending.trajectoryItemID,
		)
	}
	if commit.Version == 0 || commit.Snapshot.Version != commit.Version ||
		commit.Context.Prefix.Version != commit.Version {
		return fmt.Errorf(
			"commit version %d, snapshot version %d, and context version %d do not match",
			commit.Version, commit.Snapshot.Version, commit.Context.Prefix.Version,
		)
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, commit.Context.Prefix); err != nil {
		return fmt.Errorf("committed context prefix: %w", err)
	}
	if !canonicalObservationIdentifier(commit.Context.StateItemID) {
		return errors.New("committed context has an invalid State item ID")
	}
	if !slices.Contains(envelope.CausalParents, commit.Context.StateItemID) {
		return fmt.Errorf(
			"commit envelope does not causally name State item %q",
			commit.Context.StateItemID,
		)
	}
	if len(commit.Snapshot.Items) == 0 {
		return errors.New("observation commit snapshot has no appended tail")
	}
	tail := commit.Snapshot.Items[len(commit.Snapshot.Items)-1]
	if !reflect.DeepEqual(tail, pending.trajectoryItem) {
		return fmt.Errorf(
			"commit snapshot tail does not match pending observation %q",
			pending.trajectoryItemID,
		)
	}
	return nil
}

func canonicalObservationSession(value string) bool {
	return canonicalObservationIdentifier(value)

}

func canonicalObservationIdentifier(value string) bool {
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

func receiveObservationCommitInput(
	ctx context.Context, kind observationCommitInputKind, port element.InputPort,
	output chan<- observationCommitInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- observationCommitInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func observationPayload(payload any) (perception.Observation, bool) {
	switch typed := payload.(type) {
	case perception.Observation:
		typed.Media = slices.Clone(typed.Media)
		return typed, true
	case *perception.Observation:
		if typed == nil {
			return perception.Observation{}, false
		}
		copy := *typed
		copy.Media = slices.Clone(typed.Media)
		return copy, true
	default:
		return perception.Observation{}, false
	}
}

func commitPayload(payload any) (Commit, bool) {
	switch typed := payload.(type) {
	case Commit:
		return typed, true
	case *Commit:
		if typed != nil {
			return *typed, true
		}
	}
	return Commit{}, false
}

func rejectionPayload(payload any) (Rejection, bool) {
	switch typed := payload.(type) {
	case Rejection:
		return typed, true
	case *Rejection:
		if typed != nil {
			return *typed, true
		}
	}
	return Rejection{}, false
}

func observationStreamID(envelope element.Envelope, observation perception.Observation) string {
	return firstNonempty(envelope.SourceID, envelope.RunID,
		observation.Observer+":"+observation.Source)
}

func observationEventType(observation perception.Observation) string {
	if observation.Final {
		return observation.Observer + ".endpoint"
	}
	return observation.Observer + ".revision"
}

func observationChannel(observation perception.Observation, envelope element.Envelope) string {
	return firstNonempty(observation.Source, envelope.SourceID, "observation")
}

func replyBaseID(itemID string) string {
	for _, suffix := range []string{":committed", ":rejected"} {
		if strings.HasSuffix(itemID, suffix) {
			return strings.TrimSuffix(itemID, suffix)
		}
	}
	return itemID
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
