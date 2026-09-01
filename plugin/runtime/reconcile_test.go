package runtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestReconcileAppliesMultiEntryCandidateAndReturnsExactReceipt(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
	before := fixture.mounted.Live()
	candidate := fixture.changedCandidate(before.Sequence)

	receipt, err := fixture.mounted.Reconcile(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	after := fixture.mounted.Live()
	want := pluginruntime.ReconcileReceipt{
		FormatVersion:   pluginruntime.ReconcileReceiptFormatVersion,
		PlanFingerprint: fixture.plan.Fingerprint,
		BeforeSequence:  before.Sequence,
		AfterSequence:   before.Sequence + 5,
		Transitions: []pluginruntime.EntryTransition{
			{
				Entry:                "alpha",
				BeforeImplementation: "alpha-v1",
				AfterImplementation:  "alpha-v2",
				BeforeRuntime:        reconcileArtifact("alpha-v1", "build:alpha-1"),
				AfterRuntime:         reconcileArtifact("alpha-v2", "build:alpha-2"),
				BeforeConfigDigest:   exactJSONDigest(`{"limit":1,"mode":"old-alpha"}`),
				AfterConfigDigest:    exactJSONDigest(`{"limit":2,"mode":"new-alpha"}`),
				BeforePermissions: []plugin.Permission{{
					Kind: "network.request", Resource: "api", Operations: []string{"read", "write"},
				}},
				AfterPermissions: []plugin.Permission{{
					Kind: "network.request", Resource: "api", Operations: []string{"write"},
				}},
			},
			{
				Entry:                "beta",
				BeforeImplementation: "beta-v1",
				AfterImplementation:  "beta-v2",
				BeforeRuntime:        reconcileArtifact("beta-v1", "build:beta-1"),
				AfterRuntime:         reconcileArtifact("beta-v2", "build:beta-2"),
				BeforeConfigDigest:   exactJSONDigest(`{"limit":1,"mode":"old-beta"}`),
				AfterConfigDigest:    exactJSONDigest(`{"limit":2,"mode":"new-beta"}`),
				BeforePermissions: []plugin.Permission{{
					Kind: "storage.file", Resource: "workspace", Operations: []string{"read"},
				}},
				AfterPermissions: []plugin.Permission{{
					Kind: "storage.file", Resource: "workspace", Operations: []string{"read", "write"},
				}},
			},
		},
	}
	if !reflect.DeepEqual(receipt, want) {
		t.Fatalf("receipt mismatch\n got: %#v\nwant: %#v", receipt, want)
	}
	if after.Sequence != receipt.AfterSequence {
		t.Fatalf("live sequence = %d, receipt after sequence = %d", after.Sequence, receipt.AfterSequence)
	}
	assertLiveImplementation(t, after, "alpha", "alpha-v2", reconcileArtifact("alpha-v2", "build:alpha-2"))
	assertLiveImplementation(t, after, "beta", "beta-v2", reconcileArtifact("beta-v2", "build:beta-2"))
	if after.Entries["alpha"].Effects != 3 || after.Entries["beta"].Effects != 2 {
		t.Fatalf("live candidate effects omit nested pre-mount ownership: alpha=%d beta=%d",
			after.Entries["alpha"].Effects, after.Entries["beta"].Effects)
	}
	assertObservedMount(t, fixture.alphaV2, `{"limit":2,"mode":"new-alpha"}`, []plugin.Permission{{
		Kind: "network.request", Resource: "api", Operations: []string{"write"},
	}}, map[string]string{})
	assertObservedMount(t, fixture.betaV2, `{"limit":2,"mode":"new-beta"}`, []plugin.Permission{{
		Kind: "storage.file", Resource: "workspace", Operations: []string{"read", "write"},
	}}, map[string]string{"client.alpha": "alpha-v2:client.alpha"})
	assertObservedPreMount(t, fixture.alphaV2, `{"limit":2,"mode":"new-alpha"}`, []plugin.Permission{{
		Kind: "network.request", Resource: "api", Operations: []string{"write"},
	}}, map[string]string{})
	assertObservedPreMount(t, fixture.betaV2, `{"limit":2,"mode":"new-beta"}`, []plugin.Permission{{
		Kind: "storage.file", Resource: "workspace", Operations: []string{"read", "write"},
	}}, map[string]string{"client.alpha": "alpha-v1:client.alpha"})
	if fixture.alphaV2.preMountDisposals() != 0 || fixture.betaV2.preMountDisposals() != 0 {
		t.Fatalf("activated candidate preparation was disposed early: alpha=%d beta=%d",
			fixture.alphaV2.preMountDisposals(), fixture.betaV2.preMountDisposals())
	}
	if fixture.alphaV1.disposals() != 1 || fixture.betaV1.disposals() != 1 {
		t.Fatalf("old composition disposal counts = alpha:%d beta:%d, want 1 each",
			fixture.alphaV1.disposals(), fixture.betaV1.disposals())
	}
	if fixture.alphaV2.mounts() != 1 || fixture.betaV2.mounts() != 1 {
		t.Fatalf("candidate mount counts = alpha:%d beta:%d, want 1 each",
			fixture.alphaV2.mounts(), fixture.betaV2.mounts())
	}
}

func TestReconcileCandidateMountFailureRestoresCompletePreviousComposition(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{betaV2Failure: errors.New("beta candidate failed")})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(context.Background(), fixture.changedCandidate(before.Sequence))
	if err == nil || !strings.Contains(err.Error(), "previous composition was restored") ||
		!strings.Contains(err.Error(), "beta candidate failed") {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("failed candidate returned a receipt: %#v", receipt)
	}
	after := fixture.mounted.Live()
	assertLiveImplementation(t, after, "alpha", "alpha-v1", reconcileArtifact("alpha-v1", "build:alpha-1"))
	assertLiveImplementation(t, after, "beta", "beta-v1", reconcileArtifact("beta-v1", "build:beta-1"))
	if after.Sequence != before.Sequence+8 {
		t.Fatalf("rollback sequence = %d, want %d", after.Sequence, before.Sequence+8)
	}
	assertObservedMount(t, fixture.alphaV1, `{"limit":1,"mode":"old-alpha"}`, []plugin.Permission{{
		Kind: "network.request", Resource: "api", Operations: []string{"read", "write"},
	}}, map[string]string{})
	assertObservedMount(t, fixture.betaV1, `{"limit":1,"mode":"old-beta"}`, []plugin.Permission{{
		Kind: "storage.file", Resource: "workspace", Operations: []string{"read"},
	}}, map[string]string{"client.alpha": "alpha-v1:client.alpha"})
	if fixture.alphaV1.mounts() != 2 || fixture.betaV1.mounts() != 2 {
		t.Fatalf("restored mount counts = alpha:%d beta:%d, want 2 each",
			fixture.alphaV1.mounts(), fixture.betaV1.mounts())
	}
	if fixture.alphaV1.disposals() != 1 || fixture.betaV1.disposals() != 1 ||
		fixture.alphaV2.disposals() != 1 || fixture.betaV2.disposals() != 1 {
		t.Fatalf("rollback disposal counts = old alpha:%d beta:%d, candidate alpha:%d beta:%d",
			fixture.alphaV1.disposals(), fixture.betaV1.disposals(),
			fixture.alphaV2.disposals(), fixture.betaV2.disposals())
	}
}

func TestReconcileActivatedPreMountResourcesFollowTheLiveLifecycle(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
	before := fixture.mounted.Live()
	if _, err := fixture.mounted.Reconcile(
		context.Background(), fixture.changedCandidate(before.Sequence),
	); err != nil {
		t.Fatal(err)
	}
	if fixture.alphaV2.preMountDisposals() != 0 || fixture.betaV2.preMountDisposals() != 0 {
		t.Fatal("activated candidate preparation was disposed before live close")
	}
	if err := fixture.mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.alphaV2.preMountDisposals() != 1 || fixture.betaV2.preMountDisposals() != 1 {
		t.Fatalf("live close candidate preparation disposals = alpha:%d beta:%d, want one each",
			fixture.alphaV2.preMountDisposals(), fixture.betaV2.preMountDisposals())
	}
	if err := fixture.mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.alphaV2.preMountDisposals() != 1 || fixture.betaV2.preMountDisposals() != 1 {
		t.Fatal("idempotent live close repeated candidate preparation disposal")
	}
}

func TestReconcileCancellationDuringCandidateMountStillRestores(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{
		cancelBetaV2Mount: cancel,
		rejectCanceled:    true,
	})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(ctx, fixture.changedCandidate(before.Sequence))
	if err == nil || !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "previous composition was restored") {
		t.Fatalf("Reconcile() error = %v, want canceled candidate with successful restoration", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("canceled candidate returned a receipt: %#v", receipt)
	}
	after := fixture.mounted.Live()
	assertLiveImplementation(t, after, "alpha", "alpha-v1", reconcileArtifact("alpha-v1", "build:alpha-1"))
	assertLiveImplementation(t, after, "beta", "beta-v1", reconcileArtifact("beta-v1", "build:beta-1"))
	assertObservedMount(t, fixture.alphaV1, `{"limit":1,"mode":"old-alpha"}`, []plugin.Permission{{
		Kind: "network.request", Resource: "api", Operations: []string{"read", "write"},
	}}, map[string]string{})
	assertObservedMount(t, fixture.betaV1, `{"limit":1,"mode":"old-beta"}`, []plugin.Permission{{
		Kind: "storage.file", Resource: "workspace", Operations: []string{"read"},
	}}, map[string]string{"client.alpha": "alpha-v1:client.alpha"})
	if fixture.alphaV1.mounts() != 2 || fixture.betaV1.mounts() != 2 {
		t.Fatalf("cancellation restoration mount counts = alpha:%d beta:%d, want 2 each",
			fixture.alphaV1.mounts(), fixture.betaV1.mounts())
	}
}

func TestReconcileCanceledBeforeSafePointDoesNotTeardown(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
	before := fixture.mounted.Live()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	receipt, err := fixture.mounted.Reconcile(ctx, fixture.changedCandidate(before.Sequence))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error = %v, want context cancellation", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("canceled candidate returned a receipt: %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
}

func TestReconcilePreMountFailureDoesNotTeardownLiveComposition(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{
		betaV2PreMountFailure: errors.New("candidate dependency is unavailable"),
	})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(
		context.Background(), fixture.changedCandidate(before.Sequence),
	)
	if err == nil || !strings.Contains(err.Error(), "entry beta pre-mount") ||
		!strings.Contains(err.Error(), "candidate dependency is unavailable") {
		t.Fatalf("Reconcile() pre-mount error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("failed pre-mount returned a receipt: %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
	if fixture.alphaV2.preMounts() != 1 || fixture.betaV2.preMounts() != 1 ||
		fixture.alphaV2.preMountDisposals() != 1 || fixture.betaV2.preMountDisposals() != 1 ||
		fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
		t.Fatalf("pre-mount failure lifecycle = alpha pre/mount/dispose %d/%d/%d, beta %d/%d/%d",
			fixture.alphaV2.preMounts(), fixture.alphaV2.mounts(), fixture.alphaV2.preMountDisposals(),
			fixture.betaV2.preMounts(), fixture.betaV2.mounts(), fixture.betaV2.preMountDisposals())
	}
}

func TestReconcileCancellationDuringPreMountDoesNotTeardownLiveComposition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{
		cancelBetaV2PreMount: cancel,
	})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(ctx, fixture.changedCandidate(before.Sequence))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() pre-mount cancellation error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("canceled pre-mount returned a receipt: %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
	if fixture.alphaV2.preMountDisposals() != 1 || fixture.betaV2.preMountDisposals() != 1 ||
		fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
		t.Fatalf("canceled pre-mount leaked or activated: alpha=%d/%d beta=%d/%d",
			fixture.alphaV2.preMountDisposals(), fixture.alphaV2.mounts(),
			fixture.betaV2.preMountDisposals(), fixture.betaV2.mounts())
	}
}

func TestReconcileRequiresPreMountSupportBeforeCandidateAcquisition(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{betaV2NoPreMount: true})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(
		context.Background(), fixture.changedCandidate(before.Sequence),
	)
	if !errors.Is(err, pluginruntime.ErrCandidatePreMountUnsupported) ||
		!strings.Contains(err.Error(), "entry beta") {
		t.Fatalf("Reconcile() unsupported pre-mount error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("unsupported pre-mount returned a receipt: %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
	if fixture.alphaV2.preMounts() != 0 || fixture.betaV2.preMounts() != 0 ||
		fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
		t.Fatalf("support preflight acquired a candidate: alpha=%d/%d beta=%d/%d",
			fixture.alphaV2.preMounts(), fixture.alphaV2.mounts(),
			fixture.betaV2.preMounts(), fixture.betaV2.mounts())
	}
}

func TestReconcileRejectsTypedNilPreMountBeforeLiveTeardown(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{betaV2TypedNil: true})
	before := fixture.mounted.Live()

	receipt, err := fixture.mounted.Reconcile(
		context.Background(), fixture.changedCandidate(before.Sequence),
	)
	if err == nil || !strings.Contains(err.Error(), "nil activation") {
		t.Fatalf("Reconcile() typed-nil pre-mount error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("typed-nil pre-mount returned a receipt: %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
	if fixture.alphaV2.preMountDisposals() != 1 || fixture.betaV2.preMountDisposals() != 1 ||
		fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
		t.Fatalf("typed-nil pre-mount leaked or activated: alpha=%d/%d beta=%d/%d",
			fixture.alphaV2.preMountDisposals(), fixture.alphaV2.mounts(),
			fixture.betaV2.preMountDisposals(), fixture.betaV2.mounts())
	}
}

func TestReconcileSuccessfulScopesOutliveRequestContext(t *testing.T) {
	type contextKey string
	const (
		realmKey   contextKey = "realm"
		requestKey contextKey = "request"
	)
	realm := context.WithValue(context.Background(), realmKey, "owner")
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{realm: realm})
	before := fixture.mounted.Live()
	request := context.WithValue(context.Background(), requestKey, "request-only")
	ctx, cancel := context.WithCancel(request)

	if _, err := fixture.mounted.Reconcile(ctx, fixture.changedCandidate(before.Sequence)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := fixture.alphaV2.latest().ctx.Err(); err != nil {
		t.Fatalf("alpha candidate scope inherited completed request context: %v", err)
	}
	if err := fixture.betaV2.latest().ctx.Err(); err != nil {
		t.Fatalf("beta candidate scope inherited completed request context: %v", err)
	}
	for _, state := range []*reconcileFactoryState{fixture.alphaV2, fixture.betaV2} {
		mountedContext := state.latest().ctx
		if got := mountedContext.Value(realmKey); got != "owner" {
			t.Fatalf("%s candidate lost realm value: %#v", state.label, got)
		}
		if got := mountedContext.Value(requestKey); got != nil {
			t.Fatalf("%s candidate retained request-only value: %#v", state.label, got)
		}
		preparedContext := state.latestPreMount().ctx
		if err := preparedContext.Err(); err != nil {
			t.Fatalf("%s pre-mount scope inherited completed request context: %v", state.label, err)
		}
		if got := preparedContext.Value(realmKey); got != "owner" {
			t.Fatalf("%s pre-mount lost realm value: %#v", state.label, got)
		}
		if got := preparedContext.Value(requestKey); got != nil {
			t.Fatalf("%s pre-mount retained request-only value: %#v", state.label, got)
		}
	}
}

func TestReconcileCanceledRealmCannotCreateReplacementScopes(t *testing.T) {
	realm, cancelRealm := context.WithCancel(context.Background())
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{realm: realm})
	before := fixture.mounted.Live()
	cancelRealm()

	receipt, err := fixture.mounted.Reconcile(
		context.Background(), fixture.changedCandidate(before.Sequence),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reconcile() error = %v, want canceled realm", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("canceled realm returned receipt %#v", receipt)
	}
	if fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
		t.Fatalf("canceled realm mounted replacements: alpha=%d beta=%d",
			fixture.alphaV2.mounts(), fixture.betaV2.mounts())
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
}

func TestReconcileRejectsStaleCandidateWithoutDisposal(t *testing.T) {
	for _, test := range []struct {
		name        string
		fingerprint func(plugin.Plan) string
		sequence    func(uint64) uint64
	}{
		{
			name:        "plan",
			fingerprint: func(plugin.Plan) string { return "sha256:" + strings.Repeat("0", 64) },
			sequence:    func(sequence uint64) uint64 { return sequence },
		},
		{
			name:        "sequence",
			fingerprint: func(plan plugin.Plan) string { return plan.Fingerprint },
			sequence:    func(sequence uint64) uint64 { return sequence + 1 },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
			before := fixture.mounted.Live()
			candidate := fixture.changedCandidate(before.Sequence)
			candidate.ExpectedPlanFingerprint = test.fingerprint(fixture.plan)
			candidate.ExpectedSequence = test.sequence(before.Sequence)

			receipt, err := fixture.mounted.Reconcile(context.Background(), candidate)
			if !errors.Is(err, pluginruntime.ErrReconcileStale) {
				t.Fatalf("Reconcile() error = %v, want ErrReconcileStale", err)
			}
			if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
				t.Fatalf("stale candidate returned receipt %#v", receipt)
			}
			assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
		})
	}
}

func TestReconcilePreflightsAllConfigAndPermissionsBeforeTeardown(t *testing.T) {
	for _, test := range []struct {
		name       string
		betaUpdate pluginruntime.EntryUpdate
		wantError  string
	}{
		{
			name: "config",
			betaUpdate: pluginruntime.EntryUpdate{
				Entry: "beta", SetConfig: true, Config: json.RawMessage(`{"mode":"reject"}`),
			},
			wantError: "configuration rejected",
		},
		{
			name: "permission",
			betaUpdate: pluginruntime.EntryUpdate{
				Entry: "beta", SetPermissions: true, Permissions: []plugin.Permission{{
					Kind: "storage.file", Resource: "workspace", Operations: []string{"delete"},
				}},
			},
			wantError: "exceeds the descriptor ceiling",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
			before := fixture.mounted.Live()
			candidate := pluginruntime.ReconcileCandidate{
				ExpectedPlanFingerprint: fixture.plan.Fingerprint,
				ExpectedSequence:        before.Sequence,
				Updates: []pluginruntime.EntryUpdate{
					{
						Entry: "alpha", SetImplementation: true, Implementation: "alpha-v2",
						SetConfig: true, Config: json.RawMessage(`{"limit":2,"mode":"new-alpha"}`),
					},
					test.betaUpdate,
				},
			}

			receipt, err := fixture.mounted.Reconcile(context.Background(), candidate)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Reconcile() error = %v, want %q", err, test.wantError)
			}
			if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
				t.Fatalf("invalid candidate returned receipt %#v", receipt)
			}
			assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
			if fixture.alphaV2.mounts() != 0 || fixture.betaV2.mounts() != 0 {
				t.Fatalf("preflight mounted candidate implementations: alpha=%d beta=%d",
					fixture.alphaV2.mounts(), fixture.betaV2.mounts())
			}
		})
	}
}

func TestReconcileNoOpDoesNotCycleLifecycle(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{})
	before := fixture.mounted.Live()
	candidate := pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: fixture.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "alpha", SetImplementation: true, Implementation: "alpha-v1",
			SetConfig: true, Config: json.RawMessage(`{"mode":"old-alpha","limit":1.0}`),
			SetPermissions: true, Permissions: []plugin.Permission{{
				Kind: "network.request", Resource: "api", Operations: []string{"read", "write"},
			}},
		}},
	}

	receipt, err := fixture.mounted.Reconcile(context.Background(), candidate)
	if !errors.Is(err, pluginruntime.ErrReconcileNoChanges) {
		t.Fatalf("Reconcile() error = %v, want ErrReconcileNoChanges", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("no-op candidate returned receipt %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
}

func TestReconcileStateSchemaFailsClosedBeforeTeardown(t *testing.T) {
	fixture := mountReconcileFixture(t, reconcileFixtureOptions{statefulAlpha: true})
	before := fixture.mounted.Live()
	candidate := fixture.changedCandidate(before.Sequence)

	receipt, err := fixture.mounted.Reconcile(context.Background(), candidate)
	if !errors.Is(err, pluginruntime.ErrStateMigrationNeeded) ||
		!strings.Contains(err.Error(), "schema.client.alpha-state") {
		t.Fatalf("Reconcile() error = %v, want state migration refusal", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) {
		t.Fatalf("stateful candidate returned receipt %#v", receipt)
	}
	assertUnchangedBeforeTeardown(t, fixture, before.Sequence)
}

func BenchmarkReconcileLeafPlan64(b *testing.B) {
	plan, registry := benchmarkRuntimePlan(b, 64)
	leaf := plan.Entries[len(plan.Entries)-1]
	const alternate = "reconcile-benchmark-alternate"
	if err := registry.Register(alternate, benchmarkFactory{descriptor: leaf.Descriptor}); err != nil {
		b.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = mounted.Close(context.Background()) })
	sequence := mounted.Live().Sequence
	implementation := alternate
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		receipt, err := mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
			ExpectedPlanFingerprint: plan.Fingerprint,
			ExpectedSequence:        sequence,
			Updates: []pluginruntime.EntryUpdate{{
				Entry: leaf.Entry.ID, SetImplementation: true, Implementation: implementation,
			}},
		})
		if err != nil {
			b.Fatal(err)
		}
		sequence = receipt.AfterSequence
		if implementation == alternate {
			implementation = leaf.Identity.Name
		} else {
			implementation = alternate
		}
	}
}

type reconcileFixtureOptions struct {
	betaV2Failure         error
	betaV2PreMountFailure error
	betaV2NoPreMount      bool
	betaV2TypedNil        bool
	statefulAlpha         bool
	cancelBetaV2Mount     context.CancelFunc
	cancelBetaV2PreMount  context.CancelFunc
	rejectCanceled        bool
	realm                 context.Context
}

type reconcileFixture struct {
	plan    plugin.Plan
	mounted *pluginruntime.Mounted
	alphaV1 *reconcileFactoryState
	alphaV2 *reconcileFactoryState
	betaV1  *reconcileFactoryState
	betaV2  *reconcileFactoryState
}

func mountReconcileFixture(t *testing.T, options reconcileFixtureOptions) reconcileFixture {
	t.Helper()
	alphaService := contract("client.alpha", 'd')
	alpha := descriptor("openrealtime.client.alpha", []plugin.Contract{alphaService}, nil)
	alphaConfig := contract("schema.client.alpha-config", 'e')
	alpha.ConfigSchema = &alphaConfig
	alpha.Permissions = []plugin.Permission{{
		Kind: "network.request", Resource: "api", Operations: []string{"read", "write"},
	}}
	if options.statefulAlpha {
		stateSchema := contract("schema.client.alpha-state", 'f')
		alpha.StateSchema = &stateSchema
	}
	beta := descriptor("openrealtime.client.beta", nil, []plugin.Requirement{{Contract: alphaService}})
	betaConfig := contract("schema.client.beta-config", '1')
	beta.ConfigSchema = &betaConfig
	beta.Permissions = []plugin.Permission{{
		Kind: "storage.file", Resource: "workspace", Operations: []string{"read", "write"},
	}}
	plan := compilePlan(t, []plugin.Descriptor{alpha, beta}, []plugin.ProfileEntry{
		{ID: "alpha", Plugin: alpha.Name, Scope: "root"},
		{ID: "beta", Plugin: beta.Name, Scope: "root"},
	})

	fixture := reconcileFixture{
		plan:    plan,
		alphaV1: &reconcileFactoryState{label: "alpha-v1", rejectCanceled: options.rejectCanceled},
		alphaV2: &reconcileFactoryState{label: "alpha-v2"},
		betaV1:  &reconcileFactoryState{label: "beta-v1", rejectCanceled: options.rejectCanceled},
		betaV2: &reconcileFactoryState{
			label: "beta-v2", fail: options.betaV2Failure,
			preMountFail: options.betaV2PreMountFailure, typedNilCandidate: options.betaV2TypedNil,
			cancelMount: options.cancelBetaV2Mount, cancelPreMount: options.cancelBetaV2PreMount,
		},
	}
	registry := pluginruntime.NewRegistry()
	mustRegisterReconcileFactory(t, registry, "alpha-v1", "build:alpha-1", reconcileFactory{
		descriptor: alpha, state: fixture.alphaV1,
	})
	mustRegisterReconcileFactory(t, registry, "alpha-v2", "build:alpha-2", reconcileFactory{
		descriptor: alpha, state: fixture.alphaV2,
	})
	mustRegisterReconcileFactory(t, registry, "beta-v1", "build:beta-1", reconcileFactory{
		descriptor: beta, state: fixture.betaV1,
	})
	betaV2Factory := reconcileFactory{descriptor: beta, state: fixture.betaV2}
	if options.betaV2NoPreMount {
		mustRegisterMountOnlyReconcileFactory(t, registry, "beta-v2", "build:beta-2",
			mountOnlyReconcileFactory{factory: betaV2Factory})
	} else {
		mustRegisterReconcileFactory(t, registry, "beta-v2", "build:beta-2", betaV2Factory)
	}
	realm := options.realm
	if realm == nil {
		realm = context.Background()
	}
	mounted, err := pluginruntime.Mount(realm, pluginruntime.Config{
		Plan: plan, Registry: registry,
		Implementations: map[string]string{"alpha": "alpha-v1", "beta": "beta-v1"},
		Values: map[string]json.RawMessage{
			"alpha": json.RawMessage(`{"limit":1,"mode":"old-alpha"}`),
			"beta":  json.RawMessage(`{"limit":1,"mode":"old-beta"}`),
		},
		Permissions: map[string][]plugin.Permission{
			"alpha": {{
				Kind: "network.request", Resource: "api", Operations: []string{"write", "read"},
			}},
			"beta": {{
				Kind: "storage.file", Resource: "workspace", Operations: []string{"read"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.mounted = mounted
	t.Cleanup(func() {
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close reconciliation fixture: %v", err)
		}
	})
	return fixture
}

func (fixture reconcileFixture) changedCandidate(sequence uint64) pluginruntime.ReconcileCandidate {
	return pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: fixture.plan.Fingerprint,
		ExpectedSequence:        sequence,
		// Deliberately reverse dependency order. The runtime owns deterministic
		// transition ordering and lifecycle ordering, not the candidate author.
		Updates: []pluginruntime.EntryUpdate{
			{
				Entry: "beta", SetImplementation: true, Implementation: "beta-v2",
				SetConfig: true, Config: json.RawMessage(`{"mode":"new-beta","limit":2.0}`),
				SetPermissions: true, Permissions: []plugin.Permission{{
					Kind: "storage.file", Resource: "workspace", Operations: []string{"write", "read"},
				}},
			},
			{
				Entry: "alpha", SetImplementation: true, Implementation: "alpha-v2",
				SetConfig: true, Config: json.RawMessage(`{"mode":"new-alpha","limit":2.0}`),
				SetPermissions: true, Permissions: []plugin.Permission{{
					Kind: "network.request", Resource: "api", Operations: []string{"write"},
				}},
			},
		},
	}
}

type reconcileMountObservation struct {
	ctx         context.Context
	config      json.RawMessage
	permissions []plugin.Permission
	required    map[string]string
}

type reconcileFactoryState struct {
	mu                   sync.Mutex
	label                string
	fail                 error
	preMountFail         error
	typedNilCandidate    bool
	cancelMount          context.CancelFunc
	cancelPreMount       context.CancelFunc
	rejectCanceled       bool
	observations         []reconcileMountObservation
	preMountObservations []reconcileMountObservation
	disposeCount         int
	preMountDisposeCount int
}

func (state *reconcileFactoryState) observe(ctx context.Context, mount pluginruntime.MountContext) {
	required := make(map[string]string)
	for _, requirement := range mount.Descriptor.Requires {
		value, _, _, _, found := mount.Services.Lookup(requirement.Contract.Name)
		if found {
			required[requirement.Contract.Name] = fmt.Sprint(value)
		}
	}
	state.mu.Lock()
	state.observations = append(state.observations, reconcileMountObservation{
		ctx: ctx, config: slices.Clone(mount.Config), permissions: cloneTestPermissions(mount.Permissions.Snapshot()),
		required: required,
	})
	state.mu.Unlock()
}

func (state *reconcileFactoryState) noteDispose() {
	state.mu.Lock()
	state.disposeCount++
	state.mu.Unlock()
}

func (state *reconcileFactoryState) observePreMount(
	ctx context.Context, candidate pluginruntime.CandidateContext,
) {
	required := make(map[string]string)
	for _, requirement := range candidate.Descriptor.Requires {
		value, _, _, _, found := candidate.Services.Lookup(requirement.Contract.Name)
		if found {
			required[requirement.Contract.Name] = fmt.Sprint(value)
		}
	}
	state.mu.Lock()
	state.preMountObservations = append(state.preMountObservations, reconcileMountObservation{
		ctx: ctx, config: slices.Clone(candidate.Config),
		permissions: cloneTestPermissions(candidate.Permissions.Snapshot()), required: required,
	})
	state.mu.Unlock()
}

func (state *reconcileFactoryState) notePreMountDispose() {
	state.mu.Lock()
	state.preMountDisposeCount++
	state.mu.Unlock()
}

func (state *reconcileFactoryState) mounts() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.observations)
}

func (state *reconcileFactoryState) disposals() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.disposeCount
}

func (state *reconcileFactoryState) preMounts() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.preMountObservations)
}

func (state *reconcileFactoryState) preMountDisposals() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.preMountDisposeCount
}

func (state *reconcileFactoryState) latest() reconcileMountObservation {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.observations) == 0 {
		return reconcileMountObservation{}
	}
	observation := state.observations[len(state.observations)-1]
	observation.config = slices.Clone(observation.config)
	observation.permissions = cloneTestPermissions(observation.permissions)
	observation.required = cloneStringMap(observation.required)
	return observation
}

func (state *reconcileFactoryState) latestPreMount() reconcileMountObservation {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.preMountObservations) == 0 {
		return reconcileMountObservation{}
	}
	observation := state.preMountObservations[len(state.preMountObservations)-1]
	observation.config = slices.Clone(observation.config)
	observation.permissions = cloneTestPermissions(observation.permissions)
	observation.required = cloneStringMap(observation.required)
	return observation
}

type reconcileFactory struct {
	descriptor plugin.Descriptor
	state      *reconcileFactoryState
}

func (factory reconcileFactory) Descriptor() plugin.Descriptor { return factory.descriptor }

func (factory reconcileFactory) ValidateConfig(raw json.RawMessage) error {
	var value struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value.Mode == "reject" {
		return errors.New("configuration rejected")
	}
	return nil
}

func (factory reconcileFactory) PreMount(
	ctx context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if _, expanded := candidate.Lifecycle.(pluginruntime.Lifecycle); expanded {
		return nil, errors.New("candidate lifecycle exposed worker authority")
	}
	if factory.state.rejectCanceled {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	factory.state.observePreMount(ctx, candidate)
	if err := candidate.Lifecycle.Defer("candidate-pre-mount", func(context.Context) error {
		factory.state.notePreMountDispose()
		return nil
	}); err != nil {
		return nil, err
	}
	if factory.state.cancelPreMount != nil {
		factory.state.cancelPreMount()
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	if factory.state.preMountFail != nil {
		return nil, factory.state.preMountFail
	}
	if factory.state.typedNilCandidate {
		var candidate *reconcileCandidateMount
		return candidate, nil
	}
	return reconcileCandidateMount{factory: factory}, nil
}

type reconcileCandidateMount struct{ factory reconcileFactory }

func (candidate reconcileCandidateMount) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

type mountOnlyReconcileFactory struct{ factory reconcileFactory }

func (factory mountOnlyReconcileFactory) Descriptor() plugin.Descriptor {
	return factory.factory.Descriptor()
}

func (factory mountOnlyReconcileFactory) ValidateConfig(raw json.RawMessage) error {
	return factory.factory.ValidateConfig(raw)
}

func (factory mountOnlyReconcileFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return factory.factory.Mount(ctx, mount)
}

func (factory reconcileFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	if factory.state.rejectCanceled {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	factory.state.observe(ctx, mount)
	for _, requirement := range factory.descriptor.Requires {
		if _, _, _, _, found := mount.Services.Lookup(requirement.Contract.Name); !found && !requirement.Optional {
			return fmt.Errorf("required service %s disappeared", requirement.Contract.Name)
		}
	}
	for _, service := range factory.descriptor.Provides {
		if err := mount.Publisher.Provide(service, factory.state.label+":"+service.Name); err != nil {
			return err
		}
	}
	// The plugin-visible name deliberately matches the runtime's nested
	// candidate label; runtime ownership must not consume this namespace.
	if err := mount.Lifecycle.Defer("candidate-pre-mount", func(context.Context) error {
		factory.state.noteDispose()
		return nil
	}); err != nil {
		return err
	}
	if factory.state.cancelMount != nil {
		factory.state.cancelMount()
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return factory.state.fail
}

func mustRegisterReconcileFactory(
	t *testing.T,
	registry *pluginruntime.Registry,
	implementation string,
	revision string,
	factory reconcileFactory,
) {
	t.Helper()
	if err := registry.RegisterArtifact(implementation, reconcileArtifact(implementation, revision), factory); err != nil {
		t.Fatal(err)
	}
}

func mustRegisterMountOnlyReconcileFactory(
	t *testing.T,
	registry *pluginruntime.Registry,
	implementation string,
	revision string,
	factory mountOnlyReconcileFactory,
) {
	t.Helper()
	if err := registry.RegisterArtifact(
		implementation, reconcileArtifact(implementation, revision), factory,
	); err != nil {
		t.Fatal(err)
	}
}

func reconcileArtifact(implementation, revision string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: "plugin://" + implementation, Revision: revision}
}

func exactJSONDigest(canonical string) string {
	digest := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertLiveImplementation(
	t *testing.T,
	live pluginruntime.Live,
	entry string,
	implementation string,
	artifact inspect.ArtifactIdentity,
) {
	t.Helper()
	got := live.Entries[entry]
	if got.State != "active" || !got.Desired || got.Implementation != implementation || got.Runtime != artifact {
		t.Fatalf("live entry %s = %#v, want active desired %s %#v", entry, got, implementation, artifact)
	}
}

func assertObservedMount(
	t *testing.T,
	state *reconcileFactoryState,
	config string,
	permissions []plugin.Permission,
	required map[string]string,
) {
	t.Helper()
	observation := state.latest()
	if string(observation.config) != config || !reflect.DeepEqual(observation.permissions, permissions) ||
		!reflect.DeepEqual(observation.required, required) {
		t.Fatalf("latest mount for %s = %#v, want config=%s permissions=%#v required=%#v",
			state.label, observation, config, permissions, required)
	}
}

func assertObservedPreMount(
	t *testing.T,
	state *reconcileFactoryState,
	config string,
	permissions []plugin.Permission,
	required map[string]string,
) {
	t.Helper()
	observation := state.latestPreMount()
	if string(observation.config) != config || !reflect.DeepEqual(observation.permissions, permissions) ||
		!reflect.DeepEqual(observation.required, required) {
		t.Fatalf("latest pre-mount for %s = %#v, want config=%s permissions=%#v required=%#v",
			state.label, observation, config, permissions, required)
	}
}

func assertUnchangedBeforeTeardown(t *testing.T, fixture reconcileFixture, sequence uint64) {
	t.Helper()
	live := fixture.mounted.Live()
	if live.Sequence != sequence {
		t.Fatalf("sequence changed from %d to %d", sequence, live.Sequence)
	}
	assertLiveImplementation(t, live, "alpha", "alpha-v1", reconcileArtifact("alpha-v1", "build:alpha-1"))
	assertLiveImplementation(t, live, "beta", "beta-v1", reconcileArtifact("beta-v1", "build:beta-1"))
	if fixture.alphaV1.mounts() != 1 || fixture.betaV1.mounts() != 1 ||
		fixture.alphaV1.disposals() != 0 || fixture.betaV1.disposals() != 0 {
		t.Fatalf("preflight cycled live composition: alpha mounts/disposals=%d/%d beta=%d/%d",
			fixture.alphaV1.mounts(), fixture.alphaV1.disposals(),
			fixture.betaV1.mounts(), fixture.betaV1.disposals())
	}
}

func cloneTestPermissions(source []plugin.Permission) []plugin.Permission {
	result := make([]plugin.Permission, len(source))
	for index := range source {
		result[index] = source[index]
		result[index].Operations = slices.Clone(source[index].Operations)
	}
	return result
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
