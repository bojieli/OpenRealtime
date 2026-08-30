package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func TestMountPublishesExactServicesAndDisposesDependentsFirst(t *testing.T) {
	connection := contract("client.connection", 'a')
	conversation := contract("client.conversation", 'b')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{connection}, nil)
	middle := descriptor("openrealtime.client.conversation", []plugin.Contract{conversation},
		[]plugin.Requirement{{Contract: connection}})
	consumer := descriptor("openrealtime.client.view", nil,
		[]plugin.Requirement{{Contract: conversation}})
	plan := compilePlan(t, []plugin.Descriptor{provider, middle, consumer}, []plugin.ProfileEntry{
		{ID: "view", Plugin: consumer.Name, Scope: "root"},
		{ID: "conversation", Plugin: middle.Name, Scope: "root"},
		{ID: "transport", Plugin: provider.Name, Scope: "root"},
	})

	order := &orderedLog{}
	transportState := &factoryState{label: "transport", order: order}
	conversationState := &factoryState{label: "conversation", order: order}
	viewState := &factoryState{label: "view", order: order}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: transportState})
	mustRegister(t, registry, "", testFactory{descriptor: middle, state: conversationState})
	mustRegister(t, registry, "", testFactory{descriptor: consumer, state: viewState})

	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	live := mounted.Live()
	if live.Fingerprint != plan.Fingerprint || live.Realm != plugin.ClientRealm || live.State != "active" {
		t.Fatalf("live identity = %#v", live)
	}
	for _, id := range []string{"transport", "conversation", "view"} {
		entry := live.Entries[id]
		if entry.State != "active" || !entry.Desired || entry.Identity.Name == "" {
			t.Fatalf("live entry %s = %#v", id, entry)
		}
	}
	if again := mounted.Live(); again.Sequence != live.Sequence {
		t.Fatalf("reading live state changed sequence from %d to %d", live.Sequence, again.Sequence)
	}
	if got := conversationState.requiredValue("client.connection"); got != "transport:client.connection" {
		t.Fatalf("conversation observed connection = %#v", got)
	}
	if got := viewState.requiredValue("client.conversation"); got != "conversation:client.conversation" {
		t.Fatalf("view observed conversation = %#v", got)
	}

	closeContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := mounted.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	if got := order.snapshot(); !equalStrings(got, []string{"view", "conversation", "transport"}) {
		t.Fatalf("dispose order = %v", got)
	}
	if err := mounted.Close(closeContext); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	closed := mounted.Live()
	if closed.State != "closed" || len(closed.Entries["transport"].Services) != 0 ||
		closed.Entries["transport"].Workers != 0 {
		t.Fatalf("closed live state = %#v", closed)
	}
}

func TestMountedExportIsExplicitExactAndTracksProviderLifecycle(t *testing.T) {
	connection := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{connection}, nil)
	plan := compileRealmPlanWithExports(t, plugin.ClientRealm, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "transport", Plugin: provider.Name, Scope: "root",
	}}, []plugin.ProfileExport{{
		Name: "connection", Provider: "transport", Service: connection.Name,
	}})
	state := &factoryState{label: "transport", order: &orderedLog{}}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: state})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{Plan: plan, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	value, gotContract, providerID, revision, err := mounted.Export("connection")
	if err != nil {
		t.Fatal(err)
	}
	if value != "transport:client.connection" || gotContract != connection || providerID != "transport" || revision == 0 {
		t.Fatalf("export = (%#v, %#v, %q, %d)", value, gotContract, providerID, revision)
	}
	if boundary := mounted.Live().Exports["connection"]; !boundary.Available ||
		boundary.Provider != "transport" || boundary.Contract != connection || boundary.Revision != revision {
		t.Fatalf("live export = %#v", boundary)
	}
	if _, _, _, _, err := mounted.Export("private"); !errors.Is(err, pluginruntime.ErrUnknownExport) {
		t.Fatalf("unknown export error = %v", err)
	}
	if err := mounted.Unmount(context.Background(), "transport"); err != nil {
		t.Fatal(err)
	}
	if _, gotContract, providerID, revision, err := mounted.Export("connection"); !errors.Is(err, pluginruntime.ErrExportUnavailable) || gotContract != connection || providerID != "transport" || revision != 0 {
		t.Fatalf("inactive export = (%#v, %q, %d, %v)", gotContract, providerID, revision, err)
	}
	if boundary := mounted.Live().Exports["connection"]; boundary.Available || boundary.Revision != 0 {
		t.Fatalf("inactive live export = %#v", boundary)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMountFailureUnwindsPublishedServicesAndEffects(t *testing.T) {
	service := contract("host.transport", 'a')
	provider := hostDescriptor("openrealtime.host.transport", []plugin.Contract{service}, nil)
	plan := compileHostPlan(t, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "transport", Plugin: provider.Name, Scope: "root",
	}})
	state := &factoryState{label: "transport", order: &orderedLog{}, fail: errors.New("dial failed")}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: state})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	}); err == nil || !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("Mount() error = %v", err)
	}
	if state.disposals() != 1 {
		t.Fatalf("failed mount disposals = %d, want 1", state.disposals())
	}

	missingState := &factoryState{label: "missing", order: &orderedLog{}, skipPublish: true}
	missingRegistry := pluginruntime.NewRegistry()
	mustRegister(t, missingRegistry, "", testFactory{descriptor: provider, state: missingState})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: missingRegistry,
	}); err == nil || !strings.Contains(err.Error(), "without publishing") {
		t.Fatalf("missing publication error = %v", err)
	}
	if missingState.disposals() != 1 {
		t.Fatalf("missing publication disposals = %d, want 1", missingState.disposals())
	}
}

func TestDependencyLossCascadesAndActivationRemountsDesiredConsumers(t *testing.T) {
	connection := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{connection}, nil)
	consumer := descriptor("openrealtime.client.view", nil,
		[]plugin.Requirement{{Contract: connection}})
	optional := descriptor("openrealtime.client.telemetry", nil,
		[]plugin.Requirement{{Contract: connection, Optional: true}})
	plan := compilePlan(t, []plugin.Descriptor{provider, consumer, optional}, []plugin.ProfileEntry{
		{ID: "transport", Plugin: provider.Name, Scope: "root"},
		{ID: "view", Plugin: consumer.Name, Scope: "root"},
		{ID: "telemetry", Plugin: optional.Name, Scope: "root"},
	})
	providerState := &factoryState{label: "transport", order: &orderedLog{}}
	viewState := &factoryState{label: "view", order: &orderedLog{}}
	optionalState := &factoryState{label: "telemetry", order: &orderedLog{}}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: providerState})
	mustRegister(t, registry, "", testFactory{descriptor: consumer, state: viewState})
	mustRegister(t, registry, "", testFactory{descriptor: optional, state: optionalState})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{Plan: plan, Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if err := mounted.Unmount(context.Background(), "transport"); err != nil {
		t.Fatal(err)
	}
	live := mounted.Live()
	if live.Entries["transport"].State != "inactive" || live.Entries["transport"].Desired ||
		live.Entries["view"].State != "inactive" || !live.Entries["view"].Desired ||
		live.Entries["telemetry"].State != "active" {
		t.Fatalf("dependency-loss state = %#v", live.Entries)
	}
	if _, _, _, _, found := optionalState.lastServices().Lookup("client.connection"); found {
		t.Fatal("optional consumer retained a removed provider service")
	}
	if err := mounted.Activate(context.Background(), "transport"); err != nil {
		t.Fatal(err)
	}
	live = mounted.Live()
	if live.Entries["transport"].State != "active" || live.Entries["view"].State != "active" {
		t.Fatalf("reactivated state = %#v", live.Entries)
	}
	if providerState.mounts() != 2 || viewState.mounts() != 2 || optionalState.mounts() != 1 {
		t.Fatalf("mount counts: provider=%d view=%d optional=%d",
			providerState.mounts(), viewState.mounts(), optionalState.mounts())
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementFailureRollsBackExactImplementation(t *testing.T) {
	connection := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{connection}, nil)
	consumer := descriptor("openrealtime.client.view", nil,
		[]plugin.Requirement{{Contract: connection}})
	plan := compilePlan(t, []plugin.Descriptor{provider, consumer}, []plugin.ProfileEntry{
		{ID: "transport", Plugin: provider.Name, Scope: "root"},
		{ID: "view", Plugin: consumer.Name, Scope: "root"},
	})
	original := &factoryState{label: "original", order: &orderedLog{}}
	candidate := &factoryState{label: "candidate", order: &orderedLog{}, fail: errors.New("candidate failed")}
	replacement := &factoryState{label: "replacement", order: &orderedLog{}}
	view := &factoryState{label: "view", order: &orderedLog{}}
	registry := pluginruntime.NewRegistry()
	mustRegisterArtifact(t, registry, "transport-v1", "build:1", testFactory{descriptor: provider, state: original})
	mustRegisterArtifact(t, registry, "transport-bad", "build:bad", testFactory{descriptor: provider, state: candidate})
	mustRegisterArtifact(t, registry, "transport-v2", "build:2", testFactory{descriptor: provider, state: replacement})
	mustRegister(t, registry, "", testFactory{descriptor: consumer, state: view})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, Implementations: map[string]string{"transport": "transport-v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mounted.Replace(context.Background(), "transport", "transport-bad"); err == nil || !strings.Contains(err.Error(), "previous implementation was restored") {
		t.Fatalf("failed replacement error = %v", err)
	}
	live := mounted.Live()
	if live.Entries["transport"].Implementation != "transport-v1" ||
		live.Entries["transport"].Runtime.Revision != "build:1" ||
		live.Entries["transport"].State != "active" || live.Entries["view"].State != "active" {
		t.Fatalf("rollback live state = %#v", live.Entries)
	}
	if err := mounted.Replace(context.Background(), "transport", "transport-v2"); err != nil {
		t.Fatal(err)
	}
	live = mounted.Live()
	if live.Entries["transport"].Implementation != "transport-v2" ||
		live.Entries["transport"].Runtime.Revision != "build:2" ||
		live.Entries["view"].State != "active" {
		t.Fatalf("replacement live state = %#v", live.Entries)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderWorkerFailureRemovesServiceAndQuiescesDependents(t *testing.T) {
	connection := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{connection}, nil)
	consumer := descriptor("openrealtime.client.view", nil,
		[]plugin.Requirement{{Contract: connection}})
	plan := compilePlan(t, []plugin.Descriptor{provider, consumer}, []plugin.ProfileEntry{
		{ID: "transport", Plugin: provider.Name, Scope: "root"},
		{ID: "view", Plugin: consumer.Name, Scope: "root"},
	})
	failWorker := make(chan struct{})
	providerState := &factoryState{
		label: "transport", order: &orderedLog{}, failWorker: failWorker,
	}
	viewState := &factoryState{label: "view", order: &orderedLog{}}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: providerState})
	mustRegister(t, registry, "", testFactory{descriptor: consumer, state: viewState})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	close(failWorker)
	deadline := time.Now().Add(time.Second)
	for {
		live := mounted.Live()
		if live.Entries["transport"].State == "failed" && live.Entries["view"].State == "inactive" {
			if len(live.Entries["transport"].Services) != 0 || !live.Entries["view"].Desired {
				t.Fatalf("failed provider retained service or disabled consumer: %#v", live.Entries)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker failure did not cascade: %#v", live.Entries)
		}
		time.Sleep(time.Millisecond)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownReportsUnresponsiveScopedWorkerAndStillDisposes(t *testing.T) {
	service := contract("host.transport", 'a')
	provider := hostDescriptor("openrealtime.host.transport", []plugin.Contract{service}, nil)
	plan := compileHostPlan(t, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "transport", Plugin: provider.Name, Scope: "root",
	}})
	release := make(chan struct{})
	state := &factoryState{label: "transport", order: &orderedLog{}, stubborn: release}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: state})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry, ShutdownTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mounted.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "live workers") {
		t.Fatalf("Close() error = %v", err)
	}
	if state.disposals() != 1 || len(mounted.Live().Entries["transport"].Services) != 0 {
		t.Fatalf("unresponsive worker retained effects: disposal=%d live=%#v",
			state.disposals(), mounted.Live().Entries["transport"])
	}
	close(release)
}

func TestMountRejectsUndeclaredValuesAndTypedNilService(t *testing.T) {
	service := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{service}, nil)
	plan := compilePlan(t, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "transport", Plugin: provider.Name, Scope: "root",
	}})
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: &factoryState{
		label: "transport", order: &orderedLog{},
	}})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{"transport": json.RawMessage(`{"token":"secret"}`)},
	}); err == nil || !strings.Contains(err.Error(), "declares no config schema") {
		t.Fatalf("undeclared values error = %v", err)
	}

	var typedNil *struct{}
	nilRegistry := pluginruntime.NewRegistry()
	mustRegister(t, nilRegistry, "", testFactory{descriptor: provider, state: &factoryState{
		label: "transport", order: &orderedLog{}, serviceValue: typedNil,
	}})
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: nilRegistry,
	}); err == nil || !strings.Contains(err.Error(), "nil service") {
		t.Fatalf("typed nil publication error = %v", err)
	}
}

func TestMountBindsOnlyDeploymentPermissionsWithinDescriptorCeiling(t *testing.T) {
	service := contract("host.routes", 'a')
	provider := hostDescriptor("openrealtime.host.router", []plugin.Contract{service}, nil)
	provider.Permissions = []plugin.Permission{{
		Kind: "network.listen", Resource: "loopback", Operations: []string{"http", "websocket"},
	}}
	plan := compileHostPlan(t, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "router", Plugin: provider.Name, Scope: "root",
	}})
	state := &factoryState{label: "router", order: &orderedLog{}}
	registry := pluginruntime.NewRegistry()
	mustRegister(t, registry, "", testFactory{descriptor: provider, state: state})
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Permissions: map[string][]plugin.Permission{"router": {{
			Kind: "network.listen", Resource: "loopback", Operations: []string{"http"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	permissions := state.lastPermissions()
	if permissions == nil || !permissions.Allows("network.listen", "loopback", "http") ||
		permissions.Allows("network.listen", "loopback", "websocket") {
		t.Fatalf("bound permissions = %#v", permissions)
	}
	snapshot := permissions.Snapshot()
	snapshot[0].Operations[0] = "websocket"
	if !permissions.Allows("network.listen", "loopback", "http") {
		t.Fatal("permission snapshot retained runtime-owned aliases")
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name, grant := range map[string]plugin.Permission{
		"resource":  {Kind: "network.listen", Resource: "public", Operations: []string{"http"}},
		"operation": {Kind: "network.listen", Resource: "loopback", Operations: []string{"udp"}},
		"authority": {Kind: "network.listen", Resource: "loopback", Operations: []string{"http"}, Authority: "authority.network"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
				Plan: plan, Registry: registry,
				Permissions: map[string][]plugin.Permission{"router": {grant}},
			}); err == nil || !strings.Contains(err.Error(), "ceiling") && !strings.Contains(err.Error(), "authority") {
				t.Fatalf("permission ceiling error = %v", err)
			}
		})
	}
}

func TestMountRejectsFactoryDescriptorDriftAfterRegistration(t *testing.T) {
	service := contract("client.connection", 'a')
	provider := descriptor("openrealtime.client.transport", []plugin.Contract{service}, nil)
	plan := compilePlan(t, []plugin.Descriptor{provider}, []plugin.ProfileEntry{{
		ID: "transport", Plugin: provider.Name, Scope: "root",
	}})
	factory := &mutableFactory{testFactory: testFactory{
		descriptor: provider,
		state:      &factoryState{label: "transport", order: &orderedLog{}},
	}}
	registry := pluginruntime.NewRegistry()
	if err := registry.Register("", factory); err != nil {
		t.Fatal(err)
	}
	factory.descriptor.Platforms = []string{"browser"}
	if _, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	}); err == nil || !strings.Contains(err.Error(), "changed its descriptor") {
		t.Fatalf("descriptor drift error = %v", err)
	}
}

type orderedLog struct {
	mu     sync.Mutex
	values []string
}

func (log *orderedLog) append(value string) {
	log.mu.Lock()
	log.values = append(log.values, value)
	log.mu.Unlock()
}

func (log *orderedLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.values...)
}

type factoryState struct {
	mu           sync.Mutex
	label        string
	order        *orderedLog
	mountCount   int
	disposeCount int
	required     map[string]any
	services     pluginruntime.Services
	permissions  pluginruntime.Permissions
	fail         error
	skipPublish  bool
	serviceValue any
	stubborn     <-chan struct{}
	failWorker   <-chan struct{}
}

func (state *factoryState) noteMount(services pluginruntime.Services) {
	state.mu.Lock()
	state.mountCount++
	state.services = services
	state.required = make(map[string]any)
	state.mu.Unlock()
}

func (state *factoryState) notePermissions(permissions pluginruntime.Permissions) {
	state.mu.Lock()
	state.permissions = permissions
	state.mu.Unlock()
}

func (state *factoryState) noteRequired(name string, value any) {
	state.mu.Lock()
	state.required[name] = value
	state.mu.Unlock()
}

func (state *factoryState) noteDispose() {
	state.mu.Lock()
	state.disposeCount++
	state.mu.Unlock()
	state.order.append(state.label)
}

func (state *factoryState) mounts() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.mountCount
}

func (state *factoryState) disposals() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.disposeCount
}

func (state *factoryState) requiredValue(name string) any {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.required[name]
}

func (state *factoryState) lastServices() pluginruntime.Services {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.services
}

func (state *factoryState) lastPermissions() pluginruntime.Permissions {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.permissions
}

type testFactory struct {
	descriptor plugin.Descriptor
	state      *factoryState
}

type mutableFactory struct{ testFactory }

func (factory testFactory) Descriptor() plugin.Descriptor { return factory.descriptor }

func (factory testFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	factory.state.noteMount(mount.Services)
	factory.state.notePermissions(mount.Permissions)
	for _, requirement := range factory.descriptor.Requires {
		value, contract, _, _, found := mount.Services.Lookup(requirement.Contract.Name)
		if !found {
			if requirement.Optional {
				continue
			}
			return errors.New("required service disappeared during mount")
		}
		if contract != requirement.Contract {
			return errors.New("runtime returned a changed service contract")
		}
		factory.state.noteRequired(requirement.Contract.Name, value)
	}
	if !factory.state.skipPublish {
		for _, service := range factory.descriptor.Provides {
			value := factory.state.serviceValue
			if value == nil {
				value = factory.state.label + ":" + service.Name
			}
			if err := mount.Publisher.Provide(service, value); err != nil {
				return err
			}
		}
	}
	if err := mount.Lifecycle.Defer("test", func(context.Context) error {
		factory.state.noteDispose()
		return nil
	}); err != nil {
		return err
	}
	if factory.state.stubborn != nil {
		if err := mount.Lifecycle.Go("stubborn", func(context.Context) error {
			<-factory.state.stubborn
			return nil
		}); err != nil {
			return err
		}
	}
	if factory.state.failWorker != nil {
		if err := mount.Lifecycle.Go("failing", func(context.Context) error {
			<-factory.state.failWorker
			return errors.New("transport worker failed")
		}); err != nil {
			return err
		}
	}
	return factory.state.fail
}

func descriptor(
	name string, provides []plugin.Contract, requires []plugin.Requirement,
) plugin.Descriptor {
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          name, Revision: 1, Realm: plugin.ClientRealm,
		Platforms: []string{"portable"}, Provides: provides, Requires: requires,
	}
}

func hostDescriptor(
	name string, provides []plugin.Contract, requires []plugin.Requirement,
) plugin.Descriptor {
	result := descriptor(name, provides, requires)
	result.Realm = plugin.PresentationHostRealm
	result.Platforms = []string{"go"}
	return result
}

func contract(name string, value byte) plugin.Contract {
	return plugin.Contract{Name: name, Revision: 1, Digest: "sha256:" + strings.Repeat(string(value), 64)}
}

func compilePlan(
	t *testing.T, descriptors []plugin.Descriptor, entries []plugin.ProfileEntry,
) plugin.Plan {
	t.Helper()
	return compileRealmPlan(t, plugin.ClientRealm, descriptors, entries)
}

func compileHostPlan(
	t *testing.T, descriptors []plugin.Descriptor, entries []plugin.ProfileEntry,
) plugin.Plan {
	t.Helper()
	return compileRealmPlan(t, plugin.PresentationHostRealm, descriptors, entries)
}

func compileRealmPlan(
	t *testing.T, realm plugin.Realm, descriptors []plugin.Descriptor, entries []plugin.ProfileEntry,
) plugin.Plan {
	return compileRealmPlanWithExports(t, realm, descriptors, entries, nil)
}

func compileRealmPlanWithExports(
	t *testing.T,
	realm plugin.Realm,
	descriptors []plugin.Descriptor,
	entries []plugin.ProfileEntry,
	exports []plugin.ProfileExport,
) plugin.Plan {
	t.Helper()
	catalog := plugin.NewCatalog()
	for _, value := range descriptors {
		if _, err := catalog.Register(value); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "test.profile", Revision: 1, Realm: realm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries, Exports: exports,
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
	return plan
}

func mustRegister(t *testing.T, registry *pluginruntime.Registry, name string, factory testFactory) {
	t.Helper()
	if err := registry.Register(name, factory); err != nil {
		t.Fatal(err)
	}
}

func mustRegisterArtifact(
	t *testing.T, registry *pluginruntime.Registry, name, revision string, factory testFactory,
) {
	t.Helper()
	if err := registry.RegisterArtifact(name, inspect.ArtifactIdentity{
		ID: "plugin://" + name, Revision: revision,
	}, factory); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
