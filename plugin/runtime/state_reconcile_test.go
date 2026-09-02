package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestReconcileMigratesStateAndReturnsPayloadFreeEvidence(t *testing.T) {
	descriptor := statefulDescriptor("openrealtime.client.stateful", nil)
	plan := compilePlan(t, []plugin.Descriptor{descriptor}, []plugin.ProfileEntry{{
		ID: "stateful", Plugin: descriptor.Name, Scope: "root",
	}})
	v1 := newStatefulFactoryState(`{"counter":7}`)
	v2 := newStatefulFactoryState(`{}`)
	v2.migrate = func(migration pluginruntime.StateMigration) (json.RawMessage, error) {
		if v1.disposeCalls() != 1 || v2.preMountDisposeCalls() != 0 {
			return nil, errors.New("migrator did not run between retirement and candidate activation")
		}
		if migration.EntryID != "stateful" || migration.Schema != *descriptor.StateSchema ||
			migration.SourceImplementation != "stateful-v1" ||
			string(migration.Snapshot) != `{"counter":7}` {
			return nil, errors.New("migrator received inexact predecessor evidence")
		}
		return json.RawMessage(`{"owner":"v2","counter":7.0}`), nil
	}
	registry := pluginruntime.NewRegistry()
	registerStateFactory(t, registry, "stateful-v1", "build:stateful-1", statefulFactory{
		descriptor: descriptor, state: v1, supportsMigration: true,
	})
	registerStateFactory(t, registry, "stateful-v2", "build:stateful-2", statefulFactory{
		descriptor: descriptor, state: v2, supportsMigration: true,
	})
	mounted := mountStatePlan(t, plan, registry, map[string]string{"stateful": "stateful-v1"})
	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "stateful", SetImplementation: true, Implementation: "stateful-v2",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTransfer := []pluginruntime.EntryStateTransfer{{
		Entry: "stateful", Schema: *descriptor.StateSchema,
		BeforeStateDigest:      exactJSONDigest(`{"counter":7}`),
		AfterStateDigest:       exactJSONDigest(`{"counter":7,"owner":"v2"}`),
		MigratorImplementation: "stateful-v2",
	}}
	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		!reflect.DeepEqual(receipt.StateTransfers, wantTransfer) {
		t.Fatalf("state migration receipt = %#v, want transfers %#v", receipt, wantTransfer)
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "counter") || strings.Contains(string(encoded), "owner") {
		t.Fatalf("state migration receipt leaked snapshot payload: %s", encoded)
	}
	if got := v2.currentSnapshot(); got != `{"counter":7,"owner":"v2"}` {
		t.Fatalf("candidate restored state = %s", got)
	}
	if v1.snapshotCalls() != 1 || v2.migrationCalls() != 1 || v2.restoreCalls() != 1 {
		t.Fatalf("state lifecycle counts snapshot=%d migrate=%d restore=%d",
			v1.snapshotCalls(), v2.migrationCalls(), v2.restoreCalls())
	}
	assertLiveImplementation(t, mounted.Live(), "stateful", "stateful-v2",
		reconcileArtifact("stateful-v2", "build:stateful-2"))
}

func TestStatefulMountRequiresDeclaredSnapshotRegistration(t *testing.T) {
	descriptor := statefulDescriptor("openrealtime.client.stateful-no-registration", nil)
	plan := compilePlan(t, []plugin.Descriptor{descriptor}, []plugin.ProfileEntry{{
		ID: "stateful", Plugin: descriptor.Name, Scope: "root",
	}})
	registry := pluginruntime.NewRegistry()
	state := &reconcileFactoryState{label: "stateful"}
	mustRegisterReconcileFactory(t, registry, "stateful", "build:stateful", reconcileFactory{
		descriptor: descriptor, state: state,
	})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, Implementations: map[string]string{"stateful": "stateful"},
	})
	if mounted != nil || err == nil || !strings.Contains(err.Error(), "did not register a state snapshot") {
		t.Fatalf("stateful mount without snapshot registration = mounted %v error %v", mounted, err)
	}
}

func TestReconcilePreservesUnchangedStatefulDependent(t *testing.T) {
	service := contract("client.state-provider", '7')
	provider := descriptor("openrealtime.client.state-provider", []plugin.Contract{service}, nil)
	dependent := statefulDescriptor("openrealtime.client.state-dependent", []plugin.Requirement{{
		Contract: service,
	}})
	plan := compilePlan(t, []plugin.Descriptor{provider, dependent}, []plugin.ProfileEntry{
		{ID: "provider", Plugin: provider.Name, Scope: "root"},
		{ID: "dependent", Plugin: dependent.Name, Scope: "root"},
	})
	providerV1 := &reconcileFactoryState{label: "provider-v1"}
	providerV2 := &reconcileFactoryState{label: "provider-v2"}
	dependentV1 := newStatefulFactoryState(`{"committed":11}`)
	registry := pluginruntime.NewRegistry()
	mustRegisterReconcileFactory(t, registry, "provider-v1", "build:provider-1", reconcileFactory{
		descriptor: provider, state: providerV1,
	})
	mustRegisterReconcileFactory(t, registry, "provider-v2", "build:provider-2", reconcileFactory{
		descriptor: provider, state: providerV2,
	})
	registerStateFactory(t, registry, "dependent-v1", "build:dependent-1", statefulFactory{
		descriptor: dependent, state: dependentV1, supportsMigration: true,
	})
	mounted := mountStatePlan(t, plan, registry, map[string]string{
		"provider": "provider-v1", "dependent": "dependent-v1",
	})
	before := mounted.Live()
	receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: plan.Fingerprint, ExpectedSequence: before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "provider", SetImplementation: true, Implementation: "provider-v2",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []pluginruntime.EntryStateTransfer{{
		Entry: "dependent", Schema: *dependent.StateSchema,
		BeforeStateDigest: exactJSONDigest(`{"committed":11}`),
		AfterStateDigest:  exactJSONDigest(`{"committed":11}`),
	}}
	if !reflect.DeepEqual(receipt.StateTransfers, want) {
		t.Fatalf("dependent state transfer = %#v, want %#v", receipt.StateTransfers, want)
	}
	if got := dependentV1.currentSnapshot(); got != `{"committed":11}` ||
		dependentV1.snapshotCalls() != 1 || dependentV1.restoreCalls() != 1 {
		t.Fatalf("dependent state after provider replacement = %s snapshot=%d restore=%d",
			got, dependentV1.snapshotCalls(), dependentV1.restoreCalls())
	}
	assertLiveImplementation(t, mounted.Live(), "provider", "provider-v2",
		reconcileArtifact("provider-v2", "build:provider-2"))
}

func TestReconcileRefusesMissingStateMigratorBeforeSnapshotOrTeardown(t *testing.T) {
	fixture := mountSingleStatefulFixture(t, stateFixtureOptions{candidateMigration: false})
	before := fixture.mounted.Live()
	receipt, err := fixture.reconcile()
	if !errors.Is(err, pluginruntime.ErrStateMigrationNeeded) ||
		!strings.Contains(err.Error(), "has no migrator") {
		t.Fatalf("missing migrator error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("missing migrator returned receipt %#v", receipt)
	}
	if fixture.v1.snapshotCalls() != 0 || fixture.v1.disposeCalls() != 0 ||
		fixture.v2.preMountDisposeCalls() != 1 {
		t.Fatalf("missing migrator crossed safe point: snapshot=%d old-dispose=%d candidate-dispose=%d",
			fixture.v1.snapshotCalls(), fixture.v1.disposeCalls(), fixture.v2.preMountDisposeCalls())
	}
	assertStateLiveUnchanged(t, fixture, before.Sequence)
}

func TestReconcileSnapshotFailureLeavesLiveCompositionUntouched(t *testing.T) {
	fixture := mountSingleStatefulFixture(t, stateFixtureOptions{
		candidateMigration: true, snapshotErr: errors.New("snapshot unavailable"),
	})
	before := fixture.mounted.Live()
	receipt, err := fixture.reconcile()
	if err == nil || !strings.Contains(err.Error(), "snapshot unavailable") {
		t.Fatalf("snapshot failure error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("snapshot failure returned receipt %#v", receipt)
	}
	if fixture.v1.disposeCalls() != 0 || fixture.v2.preMountDisposeCalls() != 1 {
		t.Fatalf("snapshot failure cycled live state: old-dispose=%d candidate-dispose=%d",
			fixture.v1.disposeCalls(), fixture.v2.preMountDisposeCalls())
	}
	assertStateLiveUnchanged(t, fixture, before.Sequence)
}

func TestReconcileMigrationFailureRestoresExactPredecessorState(t *testing.T) {
	fixture := mountSingleStatefulFixture(t, stateFixtureOptions{
		candidateMigration: true, migrationErr: errors.New("migration refused"),
	})
	receipt, err := fixture.reconcile()
	if err == nil || !strings.Contains(err.Error(), "migration refused") {
		t.Fatalf("migration failure error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("migration failure returned receipt %#v", receipt)
	}
	assertLiveImplementation(t, fixture.mounted.Live(), "stateful", "stateful-v1",
		reconcileArtifact("stateful-v1", "build:stateful-1"))
	if got := fixture.v1.currentSnapshot(); got != `{"counter":3}` ||
		fixture.v1.snapshotCalls() != 1 || fixture.v1.restoreCalls() != 1 ||
		fixture.v1.disposeCalls() != 1 || fixture.v2.preMountDisposeCalls() != 1 {
		t.Fatalf("migration rollback state=%s snapshot=%d restore=%d dispose=%d candidate-dispose=%d",
			got, fixture.v1.snapshotCalls(), fixture.v1.restoreCalls(),
			fixture.v1.disposeCalls(), fixture.v2.preMountDisposeCalls())
	}
}

func TestReconcileCandidateMustConsumeMigratedStateOrRollsBack(t *testing.T) {
	fixture := mountSingleStatefulFixture(t, stateFixtureOptions{
		candidateMigration: true, candidateSkipsRestore: true,
	})
	receipt, err := fixture.reconcile()
	if err == nil || !strings.Contains(err.Error(), "did not consume restored state") ||
		!strings.Contains(err.Error(), "previous composition was restored") {
		t.Fatalf("unconsumed restore error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("unconsumed restore returned receipt %#v", receipt)
	}
	assertLiveImplementation(t, fixture.mounted.Live(), "stateful", "stateful-v1",
		reconcileArtifact("stateful-v1", "build:stateful-1"))
	if got := fixture.v1.currentSnapshot(); got != `{"counter":3}` || fixture.v1.restoreCalls() != 1 {
		t.Fatalf("activation rollback state=%s restores=%d", got, fixture.v1.restoreCalls())
	}
}

func TestReconcileStatePayloadsAreStrictBoundedAndRollbackSafely(t *testing.T) {
	oversized := json.RawMessage(`{"blob":"` +
		strings.Repeat("x", pluginruntime.MaximumStateSnapshotBytes) + `"}`)
	tests := []struct {
		name             string
		snapshot         json.RawMessage
		migrationOutput  json.RawMessage
		want             string
		crossesSafePoint bool
	}{
		{name: "snapshot duplicate", snapshot: json.RawMessage(`{"counter":3,"counter":4}`), want: "duplicate"},
		{name: "snapshot array", snapshot: json.RawMessage(`[3]`), want: "JSON object"},
		{name: "snapshot oversized", snapshot: oversized, want: "limit"},
		{name: "migration duplicate", migrationOutput: json.RawMessage(`{"counter":3,"counter":4}`), want: "duplicate", crossesSafePoint: true},
		{name: "migration array", migrationOutput: json.RawMessage(`[3]`), want: "JSON object", crossesSafePoint: true},
		{name: "migration oversized", migrationOutput: oversized, want: "limit", crossesSafePoint: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := mountSingleStatefulFixture(t, stateFixtureOptions{
				candidateMigration: true, snapshot: test.snapshot,
				migrationOutput: test.migrationOutput,
			})
			receipt, err := fixture.reconcile()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("state validation error = %v, want %q", err, test.want)
			}
			if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
				t.Fatalf("invalid state returned receipt %#v", receipt)
			}
			assertLiveImplementation(t, fixture.mounted.Live(), "stateful", "stateful-v1",
				reconcileArtifact("stateful-v1", "build:stateful-1"))
			if test.crossesSafePoint {
				if fixture.v1.disposeCalls() != 1 || fixture.v1.restoreCalls() != 1 {
					t.Fatalf("invalid migrated state did not restore predecessor: dispose=%d restore=%d",
						fixture.v1.disposeCalls(), fixture.v1.restoreCalls())
				}
			} else if fixture.v1.disposeCalls() != 0 || fixture.v1.restoreCalls() != 0 {
				t.Fatalf("invalid snapshot crossed safe point: dispose=%d restore=%d",
					fixture.v1.disposeCalls(), fixture.v1.restoreCalls())
			}
		})
	}
}

func TestLegacyReplaceRefusesStatefulDependentBeforeTeardown(t *testing.T) {
	service := contract("client.legacy-state-provider", 'a')
	provider := descriptor("openrealtime.client.legacy-state-provider", []plugin.Contract{service}, nil)
	dependent := statefulDescriptor("openrealtime.client.legacy-state-dependent", []plugin.Requirement{{
		Contract: service,
	}})
	plan := compilePlan(t, []plugin.Descriptor{provider, dependent}, []plugin.ProfileEntry{
		{ID: "provider", Plugin: provider.Name, Scope: "root"},
		{ID: "dependent", Plugin: dependent.Name, Scope: "root"},
	})
	providerV1 := &reconcileFactoryState{label: "provider-v1"}
	providerV2 := &reconcileFactoryState{label: "provider-v2"}
	dependentV1 := newStatefulFactoryState(`{"committed":17}`)
	registry := pluginruntime.NewRegistry()
	mustRegisterReconcileFactory(t, registry, "provider-v1", "build:provider-1", reconcileFactory{
		descriptor: provider, state: providerV1,
	})
	mustRegisterReconcileFactory(t, registry, "provider-v2", "build:provider-2", reconcileFactory{
		descriptor: provider, state: providerV2,
	})
	registerStateFactory(t, registry, "dependent-v1", "build:dependent-1", statefulFactory{
		descriptor: dependent, state: dependentV1, supportsMigration: true,
	})
	mounted := mountStatePlan(t, plan, registry, map[string]string{
		"provider": "provider-v1", "dependent": "dependent-v1",
	})
	before := mounted.Live()
	err := mounted.Replace(context.Background(), "provider", "provider-v2")
	if !errors.Is(err, pluginruntime.ErrStateMigrationNeeded) ||
		!strings.Contains(err.Error(), "legacy-state-dependent-state") {
		t.Fatalf("legacy stateful replacement error = %v", err)
	}
	if live := mounted.Live(); live.Sequence != before.Sequence {
		t.Fatalf("legacy replace changed sequence from %d to %d", before.Sequence, live.Sequence)
	}
	if dependentV1.snapshotCalls() != 0 || dependentV1.disposeCalls() != 0 {
		t.Fatalf("legacy replace cycled stateful dependent: snapshot=%d dispose=%d",
			dependentV1.snapshotCalls(), dependentV1.disposeCalls())
	}
}

type stateFixtureOptions struct {
	candidateMigration    bool
	candidateSkipsRestore bool
	snapshotErr           error
	migrationErr          error
	snapshot              json.RawMessage
	migrationOutput       json.RawMessage
}

type singleStatefulFixture struct {
	plan    plugin.Plan
	mounted *pluginruntime.Mounted
	v1      *statefulFactoryState
	v2      *statefulFactoryState
}

func mountSingleStatefulFixture(
	t *testing.T, options stateFixtureOptions,
) singleStatefulFixture {
	t.Helper()
	descriptor := statefulDescriptor("openrealtime.client.stateful-fixture", nil)
	plan := compilePlan(t, []plugin.Descriptor{descriptor}, []plugin.ProfileEntry{{
		ID: "stateful", Plugin: descriptor.Name, Scope: "root",
	}})
	v1 := newStatefulFactoryState(`{"counter":3}`)
	if len(options.snapshot) > 0 {
		v1.current = slices.Clone(options.snapshot)
	}
	v1.snapshotErr = options.snapshotErr
	v2 := newStatefulFactoryState(`{}`)
	v2.skipRestore = options.candidateSkipsRestore
	v2.migrate = func(migration pluginruntime.StateMigration) (json.RawMessage, error) {
		if options.migrationErr != nil {
			return nil, options.migrationErr
		}
		if len(options.migrationOutput) > 0 {
			return slices.Clone(options.migrationOutput), nil
		}
		return slices.Clone(migration.Snapshot), nil
	}
	registry := pluginruntime.NewRegistry()
	registerStateFactory(t, registry, "stateful-v1", "build:stateful-1", statefulFactory{
		descriptor: descriptor, state: v1, supportsMigration: true,
	})
	registerStateFactory(t, registry, "stateful-v2", "build:stateful-2", statefulFactory{
		descriptor: descriptor, state: v2, supportsMigration: options.candidateMigration,
	})
	return singleStatefulFixture{
		plan:    plan,
		mounted: mountStatePlan(t, plan, registry, map[string]string{"stateful": "stateful-v1"}),
		v1:      v1, v2: v2,
	}
}

func (fixture singleStatefulFixture) reconcile() (pluginruntime.ReconcileReceipt, error) {
	return fixture.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: fixture.plan.Fingerprint,
		ExpectedSequence:        fixture.mounted.Live().Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "stateful", SetImplementation: true, Implementation: "stateful-v2",
		}},
	})
}

func assertStateLiveUnchanged(t *testing.T, fixture singleStatefulFixture, sequence uint64) {
	t.Helper()
	if live := fixture.mounted.Live(); live.Sequence != sequence {
		t.Fatalf("stateful live sequence changed from %d to %d", sequence, live.Sequence)
	}
	assertLiveImplementation(t, fixture.mounted.Live(), "stateful", "stateful-v1",
		reconcileArtifact("stateful-v1", "build:stateful-1"))
}

func statefulDescriptor(name string, requirements []plugin.Requirement) plugin.Descriptor {
	suffix := strings.TrimPrefix(name, "openrealtime.client.")
	stateService := contract("client."+suffix+".state", '9')
	result := descriptor(name, []plugin.Contract{stateService}, requirements)
	schema := contract("schema."+suffix+"-state", '8')
	result.StateSchema = &schema
	result.Lifecycle.Snapshot = true
	result.Lifecycle.Restore = true
	return result
}

func mountStatePlan(
	t *testing.T,
	plan plugin.Plan,
	registry *pluginruntime.Registry,
	implementations map[string]string,
) *pluginruntime.Mounted {
	t.Helper()
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, Implementations: implementations,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close state migration fixture: %v", err)
		}
	})
	return mounted
}

type statefulFactory struct {
	descriptor        plugin.Descriptor
	state             *statefulFactoryState
	supportsMigration bool
}

func (factory statefulFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory statefulFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if mount.State == nil {
		return errors.New("stateful mount omitted state lifecycle")
	}
	if !factory.state.skipRestore {
		restored, available, err := mount.State.Restored()
		if err != nil {
			return err
		}
		if available {
			factory.state.restore(restored)
		}
	}
	if err := mount.State.Snapshot(func(context.Context) (json.RawMessage, error) {
		return factory.state.snapshot()
	}); err != nil {
		return err
	}
	for _, service := range factory.descriptor.Provides {
		if err := mount.Publisher.Provide(service, factory.state); err != nil {
			return err
		}
	}
	if err := mount.Lifecycle.Defer("stateful-dispose", func(context.Context) error {
		factory.state.noteDispose()
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (factory statefulFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	factory.state.notePreMount()
	if err := candidate.Lifecycle.Defer("stateful-candidate", func(context.Context) error {
		factory.state.notePreMountDispose()
		return nil
	}); err != nil {
		return nil, err
	}
	if factory.supportsMigration {
		return migratingStatefulCandidate{factory: factory}, nil
	}
	return statefulCandidate{factory: factory}, nil
}

type statefulCandidate struct{ factory statefulFactory }

func (candidate statefulCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

type migratingStatefulCandidate struct{ factory statefulFactory }

func (candidate migratingStatefulCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

func (candidate migratingStatefulCandidate) MigrateState(
	_ context.Context, migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	return candidate.factory.state.migrateState(migration)
}

type statefulFactoryState struct {
	mu sync.Mutex

	current           json.RawMessage
	migrate           func(pluginruntime.StateMigration) (json.RawMessage, error)
	snapshotErr       error
	skipRestore       bool
	snapshots         int
	restores          int
	migrations        int
	disposals         int
	preMounts         int
	preMountDisposals int
}

func newStatefulFactoryState(current string) *statefulFactoryState {
	return &statefulFactoryState{current: json.RawMessage(current)}
}

func (state *statefulFactoryState) snapshot() (json.RawMessage, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.snapshots++
	return slices.Clone(state.current), state.snapshotErr
}

func (state *statefulFactoryState) restore(snapshot json.RawMessage) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.restores++
	state.current = slices.Clone(snapshot)
}

func (state *statefulFactoryState) migrateState(
	migration pluginruntime.StateMigration,
) (json.RawMessage, error) {
	state.mu.Lock()
	state.migrations++
	migrate := state.migrate
	state.mu.Unlock()
	if migrate == nil {
		return slices.Clone(migration.Snapshot), nil
	}
	return migrate(migration)
}

func (state *statefulFactoryState) noteDispose() {
	state.mu.Lock()
	state.disposals++
	state.mu.Unlock()
}

func (state *statefulFactoryState) notePreMount() {
	state.mu.Lock()
	state.preMounts++
	state.mu.Unlock()
}

func (state *statefulFactoryState) notePreMountDispose() {
	state.mu.Lock()
	state.preMountDisposals++
	state.mu.Unlock()
}

func (state *statefulFactoryState) currentSnapshot() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return string(state.current)
}

func (state *statefulFactoryState) snapshotCalls() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.snapshots
}

func (state *statefulFactoryState) restoreCalls() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.restores
}

func (state *statefulFactoryState) migrationCalls() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.migrations
}

func (state *statefulFactoryState) disposeCalls() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.disposals
}

func (state *statefulFactoryState) preMountDisposeCalls() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.preMountDisposals
}

func registerStateFactory(
	t *testing.T,
	registry *pluginruntime.Registry,
	implementation string,
	revision string,
	factory statefulFactory,
) {
	t.Helper()
	if err := registry.RegisterArtifact(
		implementation, reconcileArtifact(implementation, revision), factory,
	); err != nil {
		t.Fatal(err)
	}
}
