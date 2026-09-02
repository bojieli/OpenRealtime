package host

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

func replacementEffectTool(counter *atomic.Int64, output string) EffectTool {
	return EffectTool{
		Declaration: EffectDeclaration{
			Name:        "computer.replacement_fixture",
			Description: "Execute one replacement-safe irreversible fixture.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"value":{"type":"string","maxLength":64}},
				"required":["value"],"additionalProperties":false
			}`),
			Confirm: legacyaction.ConfirmNever, Mutating: true, Channel: EffectChannelComputer,
		},
		Executor: EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
			counter.Add(1)
			return EffectResult{Output: output}, nil
		}),
	}
}

type effectsReplacementHost struct {
	mounted                 *pluginruntime.Mounted
	plan                    plugin.Plan
	server                  *httptest.Server
	handler                 http.Handler
	effects                 Effects
	httpRevision            uint64
	effectsRevision         uint64
	originalImplementation  string
	candidateImplementation string
	originalArtifact        inspect.ArtifactIdentity
	candidateArtifact       inspect.ArtifactIdentity
}

func mountEffectsReplacementHost(
	t *testing.T,
	original *EffectsFactory,
	candidate *EffectsFactory,
	authority EffectAuthority,
	values json.RawMessage,
) effectsReplacementHost {
	t.Helper()
	if values == nil {
		values = json.RawMessage(`{}`)
	}
	originalIdentity, err := original.Descriptor().Identity()
	if err != nil {
		t.Fatal(err)
	}
	candidateIdentity, err := candidate.Descriptor().Identity()
	if err != nil || candidateIdentity != originalIdentity {
		t.Fatalf("effects replacement descriptor identity = %#v, %v; want %#v",
			candidateIdentity, err, originalIdentity)
	}
	router := NewRouterFactory()
	artifacts := NewArtifactStoreFactory()
	downloads := NewDownloadStoreFactory()
	authorityFactory := newTestEffectAuthorityFactory(authority)
	catalog := plugin.NewCatalog()
	for _, factory := range []pluginruntime.Factory{router, artifacts, downloads, authorityFactory, original} {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "presentation.effects-replacement.test", Revision: 1,
		Realm:  plugin.PresentationHostRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{
			{ID: "router", Plugin: router.Descriptor().Name, Scope: "root"},
			{ID: "artifacts", Plugin: artifacts.Descriptor().Name, Scope: "root"},
			{ID: "downloads", Plugin: downloads.Descriptor().Name, Scope: "root"},
			{ID: "authority", Plugin: authorityFactory.Descriptor().Name, Scope: "root"},
			{ID: "effects", Plugin: original.Descriptor().Name, Scope: "root"},
		},
		Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "effects", Provider: "effects", Service: presentation.EffectsContract.Name},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	const candidateImplementation = "openrealtime.presentation.host.effects-v2"
	originalArtifact := hostTestArtifact("go://host-effects-v1", "build-1", "4")
	candidateArtifact := hostTestArtifact("go://host-effects-v2", "build-2", "6")
	registry := pluginruntime.NewRegistry()
	for _, row := range []struct {
		implementation string
		artifact       inspect.ArtifactIdentity
		factory        pluginruntime.Factory
	}{
		{router.Descriptor().Name, hostTestArtifact("go://host-router", "build-1", "1"), router},
		{artifacts.Descriptor().Name, hostTestArtifact("go://host-artifacts", "build-1", "2"), artifacts},
		{downloads.Descriptor().Name, hostTestArtifact("go://host-downloads", "build-1", "3"), downloads},
		{authorityFactory.Descriptor().Name, hostTestArtifact("go://host-authority", "build-1", "5"), authorityFactory},
		{original.Descriptor().Name, originalArtifact, original},
		{candidateImplementation, candidateArtifact, candidate},
	} {
		if err := registry.RegisterArtifact(row.implementation, row.artifact, row.factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"artifacts": json.RawMessage(`{}`), "downloads": json.RawMessage(`{}`),
			"effects": values,
		},
		Permissions: map[string][]plugin.Permission{
			"artifacts": artifacts.Descriptor().Permissions,
			"downloads": downloads.Descriptor().Permissions,
			"effects":   original.Descriptor().Permissions,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mounted.Close(context.Background()); err != nil {
			t.Errorf("close effects replacement host: %v", err)
		}
	})
	httpValue, httpContract, httpProvider, httpRevision, err := mounted.Export("http")
	if err != nil || httpContract != presentation.HTTPHandlerContract || httpProvider != "router" {
		t.Fatalf("effects replacement HTTP export = %T/%+v/%s/%d, %v",
			httpValue, httpContract, httpProvider, httpRevision, err)
	}
	handler, ok := httpValue.(http.Handler)
	if !ok {
		t.Fatalf("effects replacement HTTP value = %T", httpValue)
	}
	effectsValue, effectsContract, effectsProvider, effectsRevision, err := mounted.Export("effects")
	if err != nil || effectsContract != presentation.EffectsContract || effectsProvider != "effects" {
		t.Fatalf("effects replacement export = %T/%+v/%s/%d, %v",
			effectsValue, effectsContract, effectsProvider, effectsRevision, err)
	}
	service, ok := effectsValue.(Effects)
	if !ok {
		t.Fatalf("effects replacement value = %T", effectsValue)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return effectsReplacementHost{
		mounted: mounted, plan: plan, server: server, handler: handler, effects: service,
		httpRevision: httpRevision, effectsRevision: effectsRevision,
		originalImplementation:  original.Descriptor().Name,
		candidateImplementation: candidateImplementation,
		originalArtifact:        originalArtifact, candidateArtifact: candidateArtifact,
	}
}

func TestEffectsReplacementMigratesTerminalIdempotencyStateAtSafePoint(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var originalExecutions atomic.Int64
	var candidateExecutions atomic.Int64
	const terminalOutput = "private terminal effect output 8V"
	original, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{
		replacementEffectTool(&originalExecutions, terminalOutput),
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{
		replacementEffectTool(&candidateExecutions, "candidate execution must not run"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectsReplacementHost(t, original, candidate, authority, nil)
	predecessorSocket := dialEffectTestSocket(t, host.server)

	beforeRefusal := host.mounted.Live()
	refused, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        beforeRefusal.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "effects", SetImplementation: true,
			Implementation: host.candidateImplementation,
			SetPermissions: true, Permissions: []plugin.Permission{},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "lack deployment grant") {
		t.Fatalf("permissionless effects candidate error = %v", err)
	}
	if !reflect.DeepEqual(refused, pluginruntime.ReconcileReceipt{}) ||
		host.mounted.Live().Sequence != beforeRefusal.Sequence {
		t.Fatalf("permissionless effects candidate crossed safe point: receipt=%#v live=%+v",
			refused, host.mounted.Live())
	}

	arguments := map[string]any{"value": "private-call-argument-8V"}
	predecessorSocket.send(effectFixtureCall(
		"sess_retain_8V", "call_retain_8V", "computer.replacement_fixture", arguments, "signed",
	))
	wantResult := predecessorSocket.receive()
	if wantResult.Type != "result" || wantResult.Error != nil || wantResult.Output != terminalOutput ||
		originalExecutions.Load() != 1 || candidateExecutions.Load() != 0 {
		t.Fatalf("predecessor terminal result = %#v executions=%d/%d",
			wantResult, originalExecutions.Load(), candidateExecutions.Load())
	}
	artifactArguments := map[string]any{
		"artifact_id": "retained_effect_artifact_8V",
		"title":       "Private retained effect title 8V",
		"html":        "<main>private retained effect HTML 8V</main>",
	}
	predecessorSocket.send(effectFixtureCall(
		"sess_retain_8V", "call_artifact_8V", "display_artifact", artifactArguments, "signed",
	))
	wantArtifactResult := predecessorSocket.receive()
	if wantArtifactResult.Type != "result" || wantArtifactResult.Error != nil ||
		wantArtifactResult.Artifact == nil || wantArtifactResult.Artifact.Version != 1 {
		t.Fatalf("predecessor artifact result = %#v", wantArtifactResult)
	}
	wantStats := host.effects.Stats()
	wantAudit := host.effects.Audit()
	if len(wantAudit) != 2 || !wantAudit[0].Executed || !wantAudit[1].Executed ||
		wantAudit[0].Replayed || wantAudit[1].Replayed {
		t.Fatalf("predecessor effects audit = %#v", wantAudit)
	}

	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "effects", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := predecessorSocket.connection.Read(predecessorSocket.ctx); err == nil {
		t.Fatal("effects replacement left the predecessor socket active")
	}
	if stats := host.effects.Stats(); !stats.Closed || stats.ActiveSessions != 0 || stats.InFlight != 0 {
		t.Fatalf("retired effects provider retained scoped work: %#v", stats)
	}
	afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err :=
		host.mounted.Export("http")
	if err != nil || afterHTTPValue != host.handler || afterHTTPContract != presentation.HTTPHandlerContract ||
		afterHTTPProvider != "router" || afterHTTPRevision != host.httpRevision {
		t.Fatalf("stable HTTP export after effects replacement = %T/%+v/%s/%d, %v",
			afterHTTPValue, afterHTTPContract, afterHTTPProvider, afterHTTPRevision, err)
	}
	afterEffectsValue, afterEffectsContract, afterEffectsProvider, afterEffectsRevision, err :=
		host.mounted.Export("effects")
	if err != nil || afterEffectsContract != presentation.EffectsContract ||
		afterEffectsProvider != "effects" || afterEffectsRevision <= host.effectsRevision {
		t.Fatalf("replacement effects export = %T/%+v/%s/%d, %v",
			afterEffectsValue, afterEffectsContract, afterEffectsProvider, afterEffectsRevision, err)
	}
	replacement, ok := afterEffectsValue.(Effects)
	if !ok || replacement == host.effects {
		t.Fatalf("replacement effects service = %T", afterEffectsValue)
	}
	gotStats := replacement.Stats()
	wantStats.ActiveSessions = 0
	wantStats.InFlight = 0
	wantStats.Closed = false
	if !reflect.DeepEqual(gotStats, wantStats) {
		t.Fatalf("replacement effects stats = %#v, want %#v", gotStats, wantStats)
	}
	if got := replacement.Audit(); !reflect.DeepEqual(got, wantAudit) {
		t.Fatalf("replacement effects audit = %#v, want %#v", got, wantAudit)
	}

	if receipt.FormatVersion != pluginruntime.ReconcileReceiptFormatVersion ||
		receipt.PlanFingerprint != host.plan.Fingerprint || receipt.BeforeSequence != before.Sequence ||
		receipt.AfterSequence != host.mounted.Live().Sequence || len(receipt.Transitions) != 1 ||
		len(receipt.Retirements) != 1 || len(receipt.StateTransfers) != 1 {
		t.Fatalf("effects replacement receipt = %#v", receipt)
	}
	transition := receipt.Transitions[0]
	if transition.Entry != "effects" ||
		transition.BeforeImplementation != host.originalImplementation ||
		transition.AfterImplementation != host.candidateImplementation ||
		transition.BeforeRuntime != host.originalArtifact || transition.AfterRuntime != host.candidateArtifact {
		t.Fatalf("effects replacement transition = %#v", transition)
	}
	retirement := receipt.Retirements[0]
	if retirement.Entry != "effects" || retirement.RetiredScopes == 0 ||
		retirement.ClosedScopes != retirement.RetiredScopes || retirement.RemainingWorkers != 0 ||
		retirement.RemainingEffects != 0 || retirement.RemainingChildScopes != 0 ||
		retirement.RemainingServices != 0 {
		t.Fatalf("effects replacement retirement = %#v", retirement)
	}
	transfer := receipt.StateTransfers[0]
	if transfer.Entry != "effects" || transfer.Schema != presentation.EffectsStateContract ||
		transfer.BeforeStateDigest == "" || transfer.BeforeStateDigest != transfer.AfterStateDigest ||
		transfer.MigratorImplementation != host.candidateImplementation {
		t.Fatalf("effects replacement state transfer = %#v", transfer)
	}
	encodedReceipt, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{
		"sess_retain_8V", "call_retain_8V", "call_artifact_8V", "private-call-argument-8V",
		terminalOutput, "retained_effect_artifact_8V", "Private retained effect title 8V",
		"private retained effect HTML 8V", "signed",
	} {
		if strings.Contains(string(encodedReceipt), private) {
			t.Fatalf("effects replacement receipt exposed %q: %s", private, encodedReceipt)
		}
	}

	replacementSocket := dialEffectTestSocket(t, host.server)
	replacementSocket.send(effectFixtureCall(
		"sess_retain_8V", "call_retain_8V", "computer.replacement_fixture", arguments, "signed",
	))
	replayed := replacementSocket.receive()
	if !reflect.DeepEqual(replayed, wantResult) || originalExecutions.Load() != 1 ||
		candidateExecutions.Load() != 0 {
		t.Fatalf("migrated idempotent replay = %#v, want %#v; executions=%d/%d",
			replayed, wantResult, originalExecutions.Load(), candidateExecutions.Load())
	}
	replacementSocket.send(effectFixtureCall(
		"sess_retain_8V", "call_artifact_8V", "display_artifact", artifactArguments, "signed",
	))
	replayedArtifact := replacementSocket.receive()
	if !reflect.DeepEqual(replayedArtifact, wantArtifactResult) || replayedArtifact.Artifact == nil ||
		replayedArtifact.Artifact.Version != 1 {
		t.Fatalf("migrated artifact replay = %#v, want %#v", replayedArtifact, wantArtifactResult)
	}
	afterReplay := replacement.Stats()
	if afterReplay.Calls != wantStats.Calls+2 || afterReplay.Results != wantStats.Results+2 ||
		afterReplay.Authorized != wantStats.Authorized+2 || afterReplay.Executed != wantStats.Executed ||
		afterReplay.Replayed != wantStats.Replayed+2 || afterReplay.Refused != wantStats.Refused {
		t.Fatalf("migrated replay stats = %#v, predecessor %#v", afterReplay, wantStats)
	}
	replayAudit := replacement.Audit()
	if len(replayAudit) != len(wantAudit)+2 || !reflect.DeepEqual(replayAudit[:len(wantAudit)], wantAudit) ||
		!replayAudit[len(replayAudit)-2].Replayed || replayAudit[len(replayAudit)-2].Executed ||
		!replayAudit[len(replayAudit)-1].Replayed || replayAudit[len(replayAudit)-1].Executed {
		t.Fatalf("migrated replay audit = %#v", replayAudit)
	}

	replacementSocket.send(effectFixtureCall(
		"sess_retain_8V", "call_retain_8V", "computer.replacement_fixture",
		map[string]any{"value": "changed-identity"}, "signed",
	))
	collision := replacementSocket.receive()
	if collision.Error == nil || collision.Error.Code != "idempotency_refused" ||
		candidateExecutions.Load() != 0 {
		t.Fatalf("migrated CallID collision = %#v executions=%d", collision, candidateExecutions.Load())
	}

	if err := host.mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHostRealmClosed(t, host.mounted.Live(), host.plan.Fingerprint)
}

func TestEffectsReplacementOversizedSnapshotResumesLiveAdmission(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var originalExecutions atomic.Int64
	var candidateExecutions atomic.Int64
	largeOutput := strings.Repeat("z", 540_000)
	original, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{
		replacementEffectTool(&originalExecutions, largeOutput),
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{
		replacementEffectTool(&candidateExecutions, "candidate execution must not run"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	values := json.RawMessage(`{
		"max_sessions":2,"max_message_bytes":2097152,"max_result_bytes":600000,
		"max_in_flight":1,"max_calls":4,"max_audit_records":8
	}`)
	host := mountEffectsReplacementHost(t, original, candidate, authority, values)
	socket := dialEffectTestSocket(t, host.server)
	socket.connection.SetReadLimit(2 << 20)
	for _, id := range []string{"large_state_1", "large_state_2"} {
		socket.send(effectFixtureCall(
			"sess_oversized", id, "computer.replacement_fixture",
			map[string]any{"value": id}, "signed",
		))
		if result := socket.receive(); result.Error != nil || len(result.Output) != len(largeOutput) {
			t.Fatalf("large predecessor result %s = error %#v, bytes %d",
				id, result.Error, len(result.Output))
		}
	}
	before := host.mounted.Live()
	receipt, err := host.mounted.Reconcile(context.Background(), pluginruntime.ReconcileCandidate{
		ExpectedPlanFingerprint: host.plan.Fingerprint,
		ExpectedSequence:        before.Sequence,
		Updates: []pluginruntime.EntryUpdate{{
			Entry: "effects", SetImplementation: true,
			Implementation: host.candidateImplementation,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot plugin state") ||
		!strings.Contains(err.Error(), "migration limit") {
		t.Fatalf("oversized effects snapshot error = %v", err)
	}
	if !reflect.DeepEqual(receipt, pluginruntime.ReconcileReceipt{}) ||
		host.mounted.Live().Sequence != before.Sequence {
		t.Fatalf("oversized effects snapshot crossed safe point: receipt=%#v live=%+v",
			receipt, host.mounted.Live())
	}
	value, contract, provider, revision, err := host.mounted.Export("effects")
	if err != nil || value != host.effects || contract != presentation.EffectsContract ||
		provider != "effects" || revision != host.effectsRevision || host.effects.Stats().Closed {
		t.Fatalf("effects export after oversized refusal = %T/%+v/%s/%d, %v stats=%#v",
			value, contract, provider, revision, err, host.effects.Stats())
	}
	socket.send(effectFixtureCall(
		"sess_oversized", "after_refusal", "computer.replacement_fixture",
		map[string]any{"value": "admission-resumed"}, "signed",
	))
	if result := socket.receive(); result.Error != nil || len(result.Output) != len(largeOutput) ||
		originalExecutions.Load() != 3 || candidateExecutions.Load() != 0 {
		t.Fatalf("effects admission after snapshot refusal = error %#v bytes=%d executions=%d/%d",
			result.Error, len(result.Output), originalExecutions.Load(), candidateExecutions.Load())
	}
}

func TestEffectsStateQuiescenceDrainsAndResumesAdmission(t *testing.T) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := parseEffectConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	hub, err := newEffectsHub(
		context.Background(), limits, cloneCompiledEffectTools(factory.tools),
		denyEffectAuthority{}, nil, factory.CatalogDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	hub.artifacts = newArtifactStore(artifactStorePolicy.defaults, time.Now)
	hub.downloads = newDownloadStore(downloadStorePolicy.defaults, time.Now)
	if !hub.callStarted() {
		t.Fatal("initial effects call was not admitted")
	}
	type quiesceResult struct {
		resume pluginruntime.StateResumer
		err    error
	}
	result := make(chan quiesceResult, 1)
	go func() {
		resume, err := hub.quiesceState(context.Background())
		result <- quiesceResult{resume: resume, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.mu.Lock()
		quiescing := hub.quiescing
		hub.mu.Unlock()
		if quiescing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("effects quiescer did not close admission")
		}
		time.Sleep(time.Millisecond)
	}
	if hub.callStarted() {
		t.Fatal("effects quiescer admitted a new call")
	}
	select {
	case early := <-result:
		t.Fatalf("effects quiescer returned before the admitted call drained: %#v", early)
	default:
	}
	hub.callReleased()
	completed := <-result
	if completed.err != nil || completed.resume == nil {
		t.Fatalf("effects quiescence = %T, %v", completed.resume, completed.err)
	}
	raw, err := hub.snapshotState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeEffectsHubState(
		raw, limits, cloneCompiledEffectTools(factory.tools), factory.CatalogDigest(),
	)
	if err != nil || state.Stats.Calls != 1 || len(state.Audit) != 0 || len(state.Calls) != 0 {
		t.Fatalf("quiesced effects snapshot = %#v, %v", state, err)
	}
	for _, forbidden := range []string{"arguments", "authority", "nonce", "active_sessions", "in_flight"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("effects snapshot retained forbidden field %q: %s", forbidden, raw)
		}
	}
	if hub.callStarted() {
		t.Fatal("effects snapshot gate reopened before its resumer")
	}
	if err := completed.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := completed.resume(context.Background()); err != nil {
		t.Fatalf("idempotent effects resumer: %v", err)
	}
	if !hub.callStarted() {
		t.Fatal("effects resumer did not reopen admission")
	}
	hub.callReleased()
	if err := hub.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEffectsStateMigrationRejectsMalformedOrIncompatibleState(t *testing.T) {
	var executions atomic.Int64
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{
		replacementEffectTool(&executions, "terminal"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := parseEffectConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	candidate := effectsCandidate{
		entryID: "effects", limits: limits, tools: cloneCompiledEffectTools(factory.tools),
		catalogDigest: factory.CatalogDigest(),
	}
	tool := effectToolMap(candidate.tools)["computer.replacement_fixture"]
	if tool == nil {
		t.Fatal("replacement fixture declaration is unavailable")
	}
	record := effectAuditState{
		ScopeID: strings.Repeat("a", 32), SessionID: "sess_state", CallID: "call_state",
		Name: tool.declaration.Name, DeclarationDigest: tool.declaration.Digest,
		Confirmation: tool.declaration.Confirm,
		Authorized:   true, Confirmed: true, Crossed: true, Executed: true,
		StartedAt: "2026-09-02T00:00:00Z", FinishedAt: "2026-09-02T00:00:01Z",
	}
	validState := effectsHubState{
		FormatVersion: effectsHubStateFormatVersion, CatalogDigest: factory.CatalogDigest(),
		Stats: effectsDurableStats{Calls: 1, Results: 1, Authorized: 1, Executed: 1},
		Audit: []effectAuditState{record},
		Calls: []effectsHubStateCall{{
			SessionID: "sess_state", CallID: "call_state",
			Identity: "sha256:" + strings.Repeat("b", 64),
			Result: effectResultState{
				Type: "result", ID: "call_state", Channel: tool.declaration.Channel,
				Output: "private terminal state",
			},
			Record: record,
		}},
	}
	validRaw, err := json.Marshal(validState)
	if err != nil {
		t.Fatal(err)
	}
	validMigration := pluginruntime.StateMigration{
		EntryID: "effects", Schema: presentation.EffectsStateContract,
		SourceImplementation: factory.Descriptor().Name, Snapshot: validRaw,
	}
	if migrated, err := candidate.MigrateState(context.Background(), validMigration); err != nil {
		t.Fatalf("valid effects state migration: %v", err)
	} else if decoded, decodeErr := decodeEffectsHubState(
		migrated, limits, candidate.tools, candidate.catalogDigest,
	); decodeErr != nil || len(decoded.Calls) != 1 || decoded.Calls[0].CallID != "call_state" {
		t.Fatalf("valid migrated effects state = %#v, %v", decoded, decodeErr)
	}

	marshalMutation := func(mutate func(*effectsHubState)) json.RawMessage {
		copy := validState
		copy.Audit = append([]effectAuditState(nil), validState.Audit...)
		copy.Calls = append([]effectsHubStateCall(nil), validState.Calls...)
		mutate(&copy)
		raw, err := json.Marshal(copy)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name    string
		mutate  func(*pluginruntime.StateMigration)
		wantErr string
	}{
		{
			name: "wrong entry", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) { migration.EntryID = "authority" },
		},
		{
			name: "wrong schema", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Schema = presentation.ArtifactStoreStateContract
			},
		},
		{
			name: "duplicate field", wantErr: "duplicate",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = append(json.RawMessage(`{"format_version":1,`), validRaw[1:]...)
			},
		},
		{
			name: "authority field", wantErr: "unknown field",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = append(json.RawMessage(`{"authority":"signed",`), validRaw[1:]...)
			},
		},
		{
			name: "catalog mismatch", wantErr: "identity",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = marshalMutation(func(state *effectsHubState) {
					state.CatalogDigest = "sha256:" + strings.Repeat("c", 64)
				})
			},
		},
		{
			name: "counter mismatch", wantErr: "inconsistent",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = marshalMutation(func(state *effectsHubState) {
					state.Stats.Results = 0
				})
			},
		},
		{
			name: "declaration mismatch", wantErr: "immutable effect declaration",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = marshalMutation(func(state *effectsHubState) {
					state.Calls[0].Record.DeclarationDigest = "sha256:" + strings.Repeat("d", 64)
				})
			},
		},
		{
			name: "noncanonical time", wantErr: "finish time",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = marshalMutation(func(state *effectsHubState) {
					state.Calls[0].Record.FinishedAt = "2026-09-02T01:00:01+01:00"
				})
			},
		},
		{
			name: "oversized result", wantErr: "bounded output",
			mutate: func(migration *pluginruntime.StateMigration) {
				migration.Snapshot = marshalMutation(func(state *effectsHubState) {
					state.Calls[0].Result.Output = strings.Repeat("x", int(limits.MaxResultBytes)+1)
				})
			},
		},
		{
			name: "nonce field", wantErr: "unknown field",
			mutate: func(migration *pluginruntime.StateMigration) {
				source := strings.Replace(
					string(validRaw), `"result":{"type":`,
					`"result":{"nonce":"forbidden","type":`, 1,
				)
				migration.Snapshot = json.RawMessage(source)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			migration := validMigration
			test.mutate(&migration)
			migrated, err := candidate.MigrateState(context.Background(), migration)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) || migrated != nil {
				t.Fatalf("malformed effects migration = %s, %v; want %q",
					migrated, err, test.wantErr)
			}
		})
	}
}
