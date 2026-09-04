package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	ObservationCommitReference = "state.RealtimeComputerUseObservationCommit"
	observationCommitRuntimeID = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/observation-commit/v1"
	maximumObservationQueue    = 256
)

// ObservationCommitDescriptor is the Realtime-CU-specific canonical ingress
// contract. Unlike the generic observation commit node, it carries two exact
// causal edges needed by a multi-step computer-use session:
//
//   - every screen/camera observation is a descendant of the latest committed
//     user task; and
//   - a post-effect screen observation retains the exact canonical tool-result
//     parent supplied by the adapter.
//
// The shared trajectory store still validates every parent. No observed text
// is promoted to user authority, and camera input is forbidden from naming an
// effect result.
func ObservationCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          ObservationCommitReference,
		Revision:      1,
		Ports: []element.Port{
			{Name: "observations", Direction: element.Input, Type: stateelements.ObservationType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "committed", Direction: element.Input, Type: stateelements.CommitType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "rejected", Direction: element.Input, Type: stateelements.RejectionType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "append", Direction: element.Output, Type: stateelements.AppendType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "outcome", Direction: element.Output, Type: stateelements.ObservationCommitOutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"observations", "committed", "rejected"},
			Outcomes: []string{"append", "outcome"}, MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/realtime-cu/observation-commit-state/v1",
		ConfigSchema: "schema://openrealtime/trajectory/observation-commit-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: stateelements.TrajectoryStoreService},
		},
		Effects: []element.Effect{{Name: "realtime-cu.observation-causality.memory", Reversible: true}},
	}
}

type observationCommitFactory struct{}

func (observationCommitFactory) Descriptor() element.Descriptor { return ObservationCommitDescriptor() }

func (observationCommitFactory) ValidateConfig(source json.RawMessage) error {
	config := stateelements.ObservationCommitConfig{RevisionNamespace: "trajectory.source_revision"}
	if err := decodeExactJSON(source, &config); err != nil {
		return err
	}
	if !canonical(config.RevisionNamespace) {
		return errors.New("revision namespace must be canonical and non-empty")
	}
	return nil
}

func (observationCommitFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config := stateelements.ObservationCommitConfig{RevisionNamespace: "trajectory.source_revision"}
	if err := decodeExactJSON(mount.Config, &config); err != nil {
		return nil, fmt.Errorf("%s %s config: %w", ObservationCommitReference, mount.InstanceID, err)
	}
	if !canonical(config.RevisionNamespace) {
		return nil, errors.New("revision namespace must be canonical and non-empty")
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("Realtime-CU observation commit has no runtime clock")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("Realtime-CU observation commit clock has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("Realtime-CU observation commit has no runtime sequence allocator")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("Realtime-CU observation commit sequence allocator has type %T", sequenceValue)
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, errors.New("Realtime-CU observation commit has no canonical trajectory store")
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("Realtime-CU observation commit trajectory store has type %T", storeValue)
	}
	ports, err := observationCommitPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &observationCommitRunner{
		instance: mount.InstanceID, namespace: config.RevisionNamespace,
		clock: clock, sequences: sequences, store: storeService.Store,
		resolution: mount.Resolution, ports: ports,
		lastSeen:  make(map[string]uint64),
		committed: make(map[string]map[uint64]committedObservation),
	}, nil
}

type observationCommitPorts struct {
	observations, committed, rejected element.InputPort
	appendOutput, outcomeOutput       element.OutputPort
}

func observationCommitPortsFrom(ports element.Ports) (observationCommitPorts, error) {
	if ports == nil {
		return observationCommitPorts{}, errors.New("Realtime-CU observation commit has nil ports")
	}
	result := observationCommitPorts{}
	inputs := []struct {
		name string
		set  *element.InputPort
	}{
		{"observations", &result.observations},
		{"committed", &result.committed},
		{"rejected", &result.rejected},
	}
	for _, input := range inputs {
		port, err := ports.Input(input.name)
		if err != nil {
			return observationCommitPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"append", &result.appendOutput}, {"outcome", &result.outcomeOutput},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return observationCommitPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type committedObservation struct {
	canonicalRevision uint64
	itemID            string
}

type queuedObservation struct {
	envelope    element.Envelope
	observation perception.Observation
	streamID    string
}

type pendingObservation struct {
	queuedObservation
	requestID         string
	canonicalRevision uint64
	trajectoryItemID  string
	item              trajectory.Item
}

type observationCommitRunner struct {
	instance   string
	namespace  string
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	store      *trajectory.Store
	resolution element.ResolutionReporter
	ports      observationCommitPorts

	pending    *pendingObservation
	queue      []queuedObservation
	lastSeen   map[string]uint64
	committed  map[string]map[uint64]committedObservation
	lastUserID string
}

type observationCommitInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *observationCommitRunner) Run(parent context.Context) error {
	if err := reportElementRuntime(runner.resolution, observationCommitRuntimeID,
		"implementation:1", ObservationCommitDescriptor()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan observationCommitInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"observation", runner.ports.observations},
		{"commit", runner.ports.committed},
		{"rejection", runner.ports.rejected},
	} {
		wait.Add(1)
		go receiveObservationCommitInputs(ctx, source.kind, source.port, inputs, failures, &wait)
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
			case "observation":
				err = runner.acceptObservation(ctx, input.envelope)
			case "commit":
				err = runner.acceptCommit(ctx, input.envelope)
			case "rejection":
				err = runner.acceptRejection(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown Realtime-CU observation input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *observationCommitRunner) acceptObservation(
	ctx context.Context, envelope element.Envelope,
) error {
	observation, ok := observationPayload(envelope.Payload)
	streamID := firstCanonical(envelope.SourceID, envelope.RunID,
		observation.Observer+":"+observation.Source)
	reject := func(code, message string) error {
		return runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationRejected, TriggerItemID: envelope.ItemID,
			StreamID: streamID, ObservationRevision: observation.Revision,
			Code: code, Message: message,
		})
	}
	if !ok {
		return reject("invalid_payload", fmt.Sprintf("observation payload has type %T", envelope.Payload))
	}
	if err := observation.Validate(); err != nil {
		return reject("invalid_observation", err.Error())
	}
	if !canonical(envelope.ItemID) || !canonical(envelope.SessionID) || !canonical(streamID) {
		return reject("invalid_identity", "observation requires canonical item, session, and stream identities")
	}
	if observation.Revision == 0 || observation.Revision <= runner.lastSeen[streamID] {
		return reject("revision_replay", "observation revisions must strictly increase within one source stream")
	}
	if err := validateObservationAuthoritySource(observation); err != nil {
		return reject("authority_source_mismatch", err.Error())
	}
	if err := runner.validateIngressParents(envelope, observation); err != nil {
		return reject("invalid_causal_parent", err.Error())
	}
	runner.lastSeen[streamID] = observation.Revision
	queued := queuedObservation{
		envelope: envelope.Clone(), observation: observation, streamID: streamID,
	}
	if runner.pending != nil {
		if len(runner.queue) >= maximumObservationQueue {
			return errors.New("Realtime-CU observation commit queue reached its bounded capacity")
		}
		runner.queue = append(runner.queue, queued)
		return nil
	}
	return runner.startObservation(ctx, queued)
}

func validateObservationAuthoritySource(observation perception.Observation) error {
	switch observation.Source {
	case SourceMicrophone, "text":
		if observation.Authority != trajectory.AuthorityUser {
			return fmt.Errorf("source %q must carry user authority", observation.Source)
		}
	case SourceScreen, SourceCamera:
		if observation.Authority != trajectory.AuthorityObserver {
			return fmt.Errorf("source %q must carry observer authority", observation.Source)
		}
	default:
		return fmt.Errorf("source %q is outside the Realtime-CU media contract", observation.Source)
	}
	return nil
}

func (runner *observationCommitRunner) validateIngressParents(
	envelope element.Envelope, observation perception.Observation,
) error {
	parents := slices.Clone(envelope.CausalParents)
	if observation.Source != SourceScreen && len(parents) != 0 {
		return fmt.Errorf("source %q cannot name an effect-result parent", observation.Source)
	}
	if len(parents) > 1 {
		return errors.New("a screen observation may name at most one effect-result parent")
	}
	if len(parents) == 0 {
		return nil
	}
	if !canonical(parents[0]) {
		return errors.New("screen effect-result parent is not canonical")
	}
	snapshot := runner.store.Snapshot()
	parent, found := trajectoryItem(snapshot, parents[0])
	if !found || parent.Kind != trajectory.KindToolResult || parent.ToolResult == nil {
		return fmt.Errorf("screen parent %q is not a canonical tool result", parents[0])
	}
	return nil
}

func (runner *observationCommitRunner) startObservation(
	ctx context.Context, queued queuedObservation,
) error {
	canonicalRevision, err := runner.sequences.Next(runner.namespace)
	if err != nil {
		return err
	}
	var superseded committedObservation
	if queued.observation.Supersedes != 0 {
		superseded = runner.committed[queued.streamID][queued.observation.Supersedes]
		if superseded.itemID == "" {
			if err := runner.publishOutcome(ctx, queued.envelope, stateelements.ObservationCommitOutcome{
				Kind: stateelements.ObservationRejected, TriggerItemID: queued.envelope.ItemID,
				StreamID: queued.streamID, ObservationRevision: queued.observation.Revision,
				SourceRevision: canonicalRevision, Code: "unknown_superseded_revision",
				Message: "observation supersedes no committed revision in its exact stream",
			}); err != nil {
				return err
			}
			return runner.startNext(ctx)
		}
	}
	parents := []string(nil)
	parents = appendUniqueString(parents, superseded.itemID)
	if queued.observation.Authority == trajectory.AuthorityObserver {
		parents = appendUniqueString(parents, runner.lastUserID)
	}
	for _, parent := range queued.envelope.CausalParents {
		parents = appendUniqueString(parents, parent)
	}
	committedNS := runner.clock.NowNS()
	occurredNS := queued.observation.OccurredNS
	// Audio/video observations and timestamped typed input retain their source
	// clock exactly. Direct callers may omit the optional typed-input time, so
	// establish one only when a final user observation would otherwise become an
	// untimed durable intent. The value one is the earliest valid runtime-clock
	// instant and keeps an injected clock that starts at zero from producing
	// invalid evidence.
	if queued.observation.Authority == trajectory.AuthorityUser &&
		queued.observation.Final && occurredNS == 0 {
		occurredNS = queued.envelope.CaptureNS
		if occurredNS == 0 {
			occurredNS = committedNS
		}
		if occurredNS == 0 {
			occurredNS = 1
		}
	}
	trajectoryItemID := fmt.Sprintf("%s-observation-%d", runner.instance, canonicalRevision)
	item := trajectory.Item{
		ID: trajectoryItemID, Kind: trajectory.KindObservation,
		MonotonicNS: committedNS, CausalParentIDs: parents,
		SourceRevision: canonicalRevision, Producer: queued.observation.Producer(),
		Content: queued.observation.Text, Observation: queued.observation.Meta(),
		Event: &trajectory.EventMetadata{
			EventID: queued.envelope.ItemID,
			Type: map[bool]string{true: queued.observation.Observer + ".endpoint",
				false: queued.observation.Observer + ".revision"}[queued.observation.Final],
			Source:     queued.observation.Observer,
			Channel:    firstCanonical(queued.observation.Source, queued.envelope.SourceID, "observation"),
			OccurredNS: occurredNS,
			// The recognizer stream is stable while text, language, and speaker
			// attribution remain revisable evidence.
			CorrelationID:      queued.streamID,
			SupersedesRevision: superseded.canonicalRevision,
		},
	}
	requestID := fmt.Sprintf("%s-append-%d", runner.instance, canonicalRevision)
	runner.pending = &pendingObservation{
		queuedObservation: queued, requestID: requestID,
		canonicalRevision: canonicalRevision, trajectoryItemID: trajectoryItemID, item: item,
	}
	appendEnvelope := queued.envelope.Clone()
	appendEnvelope.Type = stateelements.AppendType()
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUniqueString(appendEnvelope.CausalParents, queued.envelope.ItemID)
	appendEnvelope.Payload = stateelements.Append{Items: []trajectory.Item{item}}
	result, err := runner.ports.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		runner.pending = nil
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		runner.pending = nil
		return fmt.Errorf("Realtime-CU observation append delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	return nil
}

func (runner *observationCommitRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	pending := runner.pending
	if pending == nil || !replyMatches(envelope, pending.requestID) {
		return runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationRejected, TriggerItemID: envelope.ItemID,
			Code: "unknown_commit_reply", Message: "commit reply has no matching Realtime-CU append",
		})
	}
	commit, ok := stateCommitPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("Realtime-CU trajectory commit %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if err := validateObservationCommit(pending, commit); err != nil {
		return fmt.Errorf("Realtime-CU trajectory commit %s: %w", envelope.ItemID, err)
	}
	byRevision := runner.committed[pending.streamID]
	if byRevision == nil {
		byRevision = make(map[uint64]committedObservation)
		runner.committed[pending.streamID] = byRevision
	}
	byRevision[pending.observation.Revision] = committedObservation{
		canonicalRevision: pending.canonicalRevision, itemID: pending.trajectoryItemID,
	}
	if pending.observation.Authority == trajectory.AuthorityUser {
		runner.lastUserID = pending.trajectoryItemID
	}
	if pending.observation.Final {
		delete(runner.committed, pending.streamID)
	}
	runner.pending = nil
	if err := runner.publishOutcome(ctx, pending.envelope, stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: pending.envelope.ItemID,
		TrajectoryItemID: pending.trajectoryItemID, StreamID: pending.streamID,
		ObservationRevision: pending.observation.Revision,
		SourceRevision:      pending.canonicalRevision, StoreVersion: commit.Version,
		Context: commit.Context,
	}); err != nil {
		return err
	}
	return runner.startNext(ctx)
}

func validateObservationCommit(pending *pendingObservation, commit stateelements.Commit) error {
	if pending == nil || len(commit.AppendedIDs) != 1 || commit.AppendedIDs[0] != pending.trajectoryItemID {
		return errors.New("commit does not name the exact pending observation item")
	}
	if commit.Version == 0 || commit.Snapshot.Version != commit.Version ||
		commit.Context.Prefix.Version != commit.Version || !canonical(commit.Context.StateItemID) {
		return errors.New("commit omits its exact positive snapshot/context identity")
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, commit.Context.Prefix); err != nil {
		return fmt.Errorf("commit prefix: %w", err)
	}
	item, found := trajectoryItem(commit.Snapshot, pending.trajectoryItemID)
	if !found || item.Kind != trajectory.KindObservation ||
		!slices.Equal(item.CausalParentIDs, pending.item.CausalParentIDs) {
		return errors.New("commit snapshot changed the pending observation or its causal parents")
	}
	return nil
}

func (runner *observationCommitRunner) acceptRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	pending := runner.pending
	if pending == nil || !replyMatches(envelope, pending.requestID) {
		return runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{
			Kind: stateelements.ObservationRejected, TriggerItemID: envelope.ItemID,
			Code: "unknown_rejection_reply", Message: "rejection reply has no matching Realtime-CU append",
		})
	}
	rejection, ok := stateRejectionPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("Realtime-CU trajectory rejection %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	runner.pending = nil
	if err := runner.publishOutcome(ctx, pending.envelope, stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationRejected, TriggerItemID: pending.envelope.ItemID,
		TrajectoryItemID: pending.trajectoryItemID, StreamID: pending.streamID,
		ObservationRevision: pending.observation.Revision,
		SourceRevision:      pending.canonicalRevision, StoreVersion: rejection.CurrentVersion,
		Code: rejection.Code, Message: rejection.Message,
	}); err != nil {
		return err
	}
	return runner.startNext(ctx)
}

func (runner *observationCommitRunner) startNext(ctx context.Context) error {
	if runner.pending != nil || len(runner.queue) == 0 {
		return nil
	}
	next := runner.queue[0]
	runner.queue = slices.Delete(runner.queue, 0, 1)
	return runner.startObservation(ctx, next)
}

func (runner *observationCommitRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome stateelements.ObservationCommitOutcome,
) error {
	envelope := cause.Clone()
	envelope.Type = stateelements.ObservationCommitOutcomeType()
	envelope.ItemID = cause.ItemID + ":realtime-cu-observation-commit"
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	if outcome.Context.StateItemID != "" {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, outcome.Context.StateItemID)
	}
	envelope.Payload = outcome
	result, err := runner.ports.outcomeOutput.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("Realtime-CU observation outcome delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	return nil
}

func receiveObservationCommitInputs(
	ctx context.Context, kind string, input element.InputPort,
	output chan<- observationCommitInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalAdapterError(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive Realtime-CU observation %s: %w", kind, err):
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

func stateCommitPayload(payload any) (stateelements.Commit, bool) {
	switch value := payload.(type) {
	case stateelements.Commit:
		return value, true
	case *stateelements.Commit:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.Commit{}, false
}

func stateRejectionPayload(payload any) (stateelements.Rejection, bool) {
	switch value := payload.(type) {
	case stateelements.Rejection:
		return value, true
	case *stateelements.Rejection:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.Rejection{}, false
}

func replyMatches(envelope element.Envelope, requestID string) bool {
	if slices.Contains(envelope.CausalParents, requestID) {
		return true
	}
	for _, suffix := range []string{":committed", ":rejected"} {
		if strings.TrimSuffix(envelope.ItemID, suffix) == requestID {
			return true
		}
	}
	return false
}

func trajectoryItem(snapshot trajectory.Snapshot, id string) (trajectory.Item, bool) {
	for _, item := range snapshot.Items {
		if item.ID == id {
			return item, true
		}
	}
	return trajectory.Item{}, false
}

func appendUniqueString(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func firstCanonical(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func reportElementRuntime(
	reporter element.ResolutionReporter, id, revision string, descriptor element.Descriptor,
) error {
	if reporter == nil {
		return errors.New("Realtime-CU element has no live resolution reporter")
	}
	identity, err := descriptor.Identity()
	if err != nil {
		return err
	}
	artifact := inspect.ArtifactIdentity{ID: id, Revision: revision, Digest: identity.Digest}
	if err := artifact.Validate(); err != nil {
		return err
	}
	if err := reporter.Runtime(artifact.ID, artifact.Revision, artifact.Digest); err != nil {
		return err
	}
	return reporter.Capabilities(nil)
}

func decodeExactJSON(source json.RawMessage, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(source)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("configuration contains a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode trailing configuration: %w", err)
	}
	return nil
}

var _ element.ConfigValidator = observationCommitFactory{}
var _ element.Factory = observationCommitFactory{}
