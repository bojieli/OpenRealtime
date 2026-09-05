// Package state contains graph-native state and memory elements. These
// elements expose state transitions as typed ports while retaining the proven
// invariants of the underlying stores.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const TrajectoryStoreService = "state.trajectory.store"

const (
	trajectoryStoreImplementationRevision   = "implementation:3"
	observationCommitImplementationRevision = "implementation:4"
)

func stateRuntimeID(descriptor element.Descriptor) string {
	return "builtin://openrealtime/elements/" + descriptor.Name
}

var (
	appendType = element.Request(
		element.Named("trajectory.Append"), element.Named("flow.RequestID"),
	)
	commitType = element.Reply(
		element.Named("trajectory.Commit"), element.Named("flow.RequestID"),
	)
	rejectionType = element.Reply(
		element.Named("trajectory.Rejection"), element.Named("flow.RequestID"),
	)
	snapshotType = element.State(element.Named("trajectory.Snapshot"))
)

// Public type helpers let policy and boundary adapters construct exact graph
// envelopes without exporting mutable package-level Type values.
func AppendType() element.Type    { return appendType.Clone() }
func CommitType() element.Type    { return commitType.Clone() }
func RejectionType() element.Type { return rejectionType.Clone() }
func SnapshotType() element.Type  { return snapshotType.Clone() }

// TrajectoryStoreDescriptor is the graph contract for the canonical causal
// log. The state output is seeded, so it is an explicit causal break in a
// feedback graph. Append is the only trigger; reading a snapshot never starts
// model work by itself.
func TrajectoryStoreDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "state.TrajectoryStore",
		Revision:      2,
		Ports: []element.Port{
			{Name: "append", Direction: element.Input, Type: appendType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "snapshot", Direction: element.Output, Type: snapshotType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "committed", Direction: element.Output, Type: commitType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "rejected", Direction: element.Output, Type: rejectionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers: []string{"append"}, Outcomes: []string{"snapshot", "committed", "rejected"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/trajectory/store-state/v1",
		ConfigSchema: "schema://openrealtime/trajectory/store-config/v1",
		Dependencies: []element.Dependency{{Name: TrajectoryStoreService, Optional: true}},
		Effects:      []element.Effect{{Name: "state.trajectory.memory", Reversible: true}},
	}
}

// Append is one atomic safe-point transaction. Compare selects
// compare-and-append at ExpectedVersion; without it, the store still validates
// the complete batch atomically.
type Append struct {
	Compare         bool              `json:"compare,omitempty"`
	ExpectedVersion uint64            `json:"expected_version,omitempty"`
	Items           []trajectory.Item `json:"items"`
	// Prefix selects provenance-only history insertion against an exact earlier
	// prefix. It cannot combine with Compare. Commit timestamps advance to the
	// insertion boundary; no claim of current action authority is implied.
	Prefix *trajectory.PrefixIdentity `json:"prefix,omitempty"`
}

func (Append) InspectionCause() element.InspectionCauseKind {
	return element.CauseStateRevision
}

// CommittedContext compactly binds an accepted transition to the exact State
// envelope published for its immutable trajectory prefix.
type CommittedContext struct {
	Prefix      trajectory.PrefixIdentity `json:"prefix"`
	StateItemID string                    `json:"state_item_id"`
}

// Commit is the accepted terminal reply. Snapshot is included so the immediate
// validator can prove Context; downstream elements propagate only the compact
// identity after validating it.
type Commit struct {
	Version     uint64              `json:"version"`
	AppendedIDs []string            `json:"appended_ids"`
	Snapshot    trajectory.Snapshot `json:"snapshot"`
	Context     CommittedContext    `json:"context"`
}

func (Commit) InspectionCause() element.InspectionCauseKind {
	return element.CauseStateRevision
}

// Rejection is a typed, non-mutating terminal reply. A malformed or stale
// transaction is a policy-visible outcome, not a reason to crash the graph.
type Rejection struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	ExpectedVersion uint64 `json:"expected_version,omitempty"`
	CurrentVersion  uint64 `json:"current_version"`
	ItemIndex       *int   `json:"item_index,omitempty"`
	ItemID          string `json:"item_id,omitempty"`
}

func (Rejection) InspectionCause() element.InspectionCauseKind {
	return element.CauseStateRevision
}

var (
	_ element.InspectionCauseProvider = Append{}
	_ element.InspectionCauseProvider = Commit{}
	_ element.InspectionCauseProvider = Rejection{}
)

// TrajectoryStoreServiceValue optionally supplies an existing session store.
// SessionID is the trusted owner of that store when a consumer must reject
// cross-session control before any envelope can select runtime state. It is a
// coeffect and never enters Graph IR or configuration artifacts.
type TrajectoryStoreServiceValue struct {
	Store     *trajectory.Store
	SessionID string
}

type trajectoryStoreFactory struct{}

func (trajectoryStoreFactory) Descriptor() element.Descriptor {
	return TrajectoryStoreDescriptor()
}

func (trajectoryStoreFactory) ValidateConfig(source json.RawMessage) error {
	return decodeEmptyConfig(source)
}

func (trajectoryStoreFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	if err := decodeEmptyConfig(mount.Config); err != nil {
		return nil, fmt.Errorf("state.TrajectoryStore %s config: %w", mount.InstanceID, err)
	}
	store := trajectory.NewStore()
	if service, _, found := mount.Services.Lookup(TrajectoryStoreService); found {
		typed, ok := service.(*TrajectoryStoreServiceValue)
		if !ok || typed == nil || typed.Store == nil {
			return nil, fmt.Errorf("trajectory store service has type %T or a nil store", service)
		}
		store = typed.Store
	}
	appendInput, err := mount.Ports.Input("append")
	if err != nil {
		return nil, err
	}
	snapshotOutput, err := mount.Ports.Output("snapshot")
	if err != nil {
		return nil, err
	}
	commitOutput, err := mount.Ports.Output("committed")
	if err != nil {
		return nil, err
	}
	rejectionOutput, err := mount.Ports.Output("rejected")
	if err != nil {
		return nil, err
	}
	return &trajectoryStoreRunner{
		instance: mount.InstanceID, store: store, appendInput: appendInput,
		snapshotOutput: snapshotOutput, commitOutput: commitOutput,
		rejectionOutput: rejectionOutput, resolution: mount.Resolution,
	}, nil
}

type trajectoryStoreRunner struct {
	instance        string
	store           *trajectory.Store
	appendInput     element.InputPort
	snapshotOutput  element.OutputPort
	commitOutput    element.OutputPort
	rejectionOutput element.OutputPort
	resolution      element.ResolutionReporter
}

func (runner *trajectoryStoreRunner) Run(ctx context.Context) error {
	if err := reportStateResolution(runner.resolution, TrajectoryStoreDescriptor()); err != nil {
		return err
	}
	initial, initialIdentity, err := runner.store.SnapshotWithPrefixIdentity()
	if err != nil {
		return fmt.Errorf("identify initial trajectory state prefix: %w", err)
	}
	if _, err := runner.publishSnapshot(
		ctx, element.Envelope{}, initial, initialIdentity,
	); err != nil {
		return err
	}
	for {
		envelope, err := runner.appendInput.Receive(ctx)
		if terminal(ctx, err) {
			return nil
		}
		if err != nil {
			return err
		}
		request, ok := appendPayload(envelope.Payload)
		if !ok {
			if err := runner.reject(ctx, envelope, Rejection{
				Code: "invalid_payload", Message: fmt.Sprintf("append payload has type %T", envelope.Payload),
				CurrentVersion: runner.store.Snapshot().Version,
			}); err != nil {
				return err
			}
			continue
		}
		if err := runner.append(request); err != nil {
			if publishErr := runner.reject(ctx, envelope, rejectionFor(request, runner.store.Snapshot().Version, err)); publishErr != nil {
				return publishErr
			}
			continue
		}
		snapshot, identity, err := runner.store.SnapshotWithPrefixIdentity()
		if err != nil {
			return fmt.Errorf("identify committed trajectory state prefix: %w", err)
		}
		committedContext, err := runner.publishSnapshot(ctx, envelope, snapshot, identity)
		if err != nil {
			return err
		}
		ids := make([]string, len(request.Items))
		for index := range request.Items {
			ids[index] = request.Items[index].ID
		}
		commit := Commit{
			Version: snapshot.Version, AppendedIDs: ids, Snapshot: snapshot,
			Context: committedContext,
		}
		if err := broadcastReply(
			ctx, runner.commitOutput, envelope, commitType, "committed", commit,
			committedContext.StateItemID,
		); err != nil {
			return err
		}
	}
}

func (runner *trajectoryStoreRunner) append(request Append) error {
	if len(request.Items) == 0 {
		return errors.New("trajectory append batch is empty")
	}
	if request.Prefix != nil {
		if request.Compare || request.ExpectedVersion != request.Prefix.Version {
			return errors.New("history append requires one exact prefix basis without compare-and-append")
		}
		return runner.store.AppendBatchOnPrefix(*request.Prefix, request.Items)
	}
	if request.Compare {
		return runner.store.AppendBatchAt(request.ExpectedVersion, request.Items)
	}
	return runner.store.AppendBatch(request.Items)
}

func (runner *trajectoryStoreRunner) publishSnapshot(
	ctx context.Context, cause element.Envelope, snapshot trajectory.Snapshot,
	identity trajectory.PrefixIdentity,
) (CommittedContext, error) {
	if identity.Version != snapshot.Version {
		return CommittedContext{}, fmt.Errorf(
			"trajectory state prefix version %d does not match snapshot version %d",
			identity.Version, snapshot.Version,
		)
	}
	envelope := cause.Clone()
	envelope.Type = snapshotType
	envelope.ItemID = fmt.Sprintf("%s-snapshot-%d", runner.instance, snapshot.Version)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	}
	envelope.Payload = snapshot
	if _, err := runner.snapshotOutput.Broadcast(ctx, envelope); err != nil {
		return CommittedContext{}, err
	}
	return CommittedContext{Prefix: identity, StateItemID: envelope.ItemID}, nil
}

func (runner *trajectoryStoreRunner) reject(
	ctx context.Context, cause element.Envelope, rejection Rejection,
) error {
	return broadcastReply(ctx, runner.rejectionOutput, cause, rejectionType, "rejected", rejection)
}

func broadcastReply(
	ctx context.Context, output element.OutputPort, cause element.Envelope,
	typeOf element.Type, suffix string, payload any, causalParents ...string,
) error {
	envelope := cause.Clone()
	envelope.Type = typeOf
	envelope.ItemID = cause.ItemID + ":" + suffix
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	for _, parent := range causalParents {
		envelope.CausalParents = appendUnique(envelope.CausalParents, parent)
	}
	envelope.Payload = payload
	result, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered == 0 && len(output.Lanes()) != 0 {
		return fmt.Errorf("trajectory %s reply was not delivered", suffix)
	}
	return nil
}

func appendPayload(payload any) (Append, bool) {
	switch typed := payload.(type) {
	case Append:
		typed.Items = slices.Clone(typed.Items)
		return typed, true
	case *Append:
		if typed == nil {
			return Append{}, false
		}
		copy := *typed
		copy.Items = slices.Clone(typed.Items)
		return copy, true
	default:
		return Append{}, false
	}
}

func rejectionFor(request Append, current uint64, err error) Rejection {
	rejection := Rejection{
		Code: "invalid_batch", Message: err.Error(), CurrentVersion: current,
		ExpectedVersion: request.ExpectedVersion,
	}
	if errors.Is(err, trajectory.ErrVersionConflict) {
		rejection.Code = "version_conflict"
	}
	var itemError *trajectory.ItemError
	if errors.As(err, &itemError) {
		index := itemError.Index
		rejection.ItemIndex = &index
		rejection.ItemID = itemError.ID
	}
	return rejection
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func decodeEmptyConfig(source json.RawMessage) error {
	var config struct{}
	return elementconfig.Decode(source, &config)
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func reportStateResolution(
	reporter element.ResolutionReporter, descriptor element.Descriptor,
) error {
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: stateRuntimeID(descriptor), Revision: stateRuntimeRevision(descriptor),
	}, nil)
}

func stateRuntimeRevision(descriptor element.Descriptor) string {
	if descriptor.Name == ObservationCommitDescriptor().Name {
		return observationCommitImplementationRevision
	}
	return trajectoryStoreImplementationRevision
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{TrajectoryStoreDescriptor(), ObservationCommitDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register state descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register state factories: nil registry")
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
	entries := make([]factoryprofile.Entry, 0, len(Descriptors()))
	for _, factory := range []element.Factory{trajectoryStoreFactory{}, observationCommitFactory{}} {
		descriptor := factory.Descriptor()
		entries = append(entries, factoryprofile.Entry{Factory: factory, Artifact: inspect.ArtifactIdentity{
			ID: stateRuntimeID(descriptor), Revision: stateRuntimeRevision(descriptor),
		}})
	}
	return factoryprofile.Registrations(entries...)
}
