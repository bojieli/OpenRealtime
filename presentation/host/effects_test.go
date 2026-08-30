package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/coder/websocket"
)

type exactTestEffectAuthority struct{ calls atomic.Int64 }

func (authority *exactTestEffectAuthority) Authorize(
	_ context.Context, request EffectAuthorityRequest,
) (EffectAuthorityDecision, error) {
	authority.calls.Add(1)
	if request.Evidence == "hang" {
		return EffectAuthorityDecision{}, context.DeadlineExceeded
	}
	if request.Evidence != "signed" && request.Evidence != "mismatch" {
		return EffectAuthorityDecision{}, errors.New("unsigned")
	}
	decision := EffectAuthorityDecision{
		SessionID: request.SessionID, CallID: request.CallID, Name: request.Name,
		ArgumentsDigest: request.ArgumentsDigest, DeclarationDigest: request.DeclarationDigest,
		Target: request.Target,
	}
	if request.Evidence == "mismatch" {
		decision.Target += "-widened"
	}
	return decision, nil
}

type testEffectAuthorityFactory struct {
	descriptor plugin.Descriptor
	value      any
}

func newTestEffectAuthorityFactory(value any) *testEffectAuthorityFactory {
	descriptor, err := (plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.test-effect-authority", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.EffectAuthorityContract},
	}).Canonical()
	if err != nil {
		panic(err)
	}
	return &testEffectAuthorityFactory{descriptor: descriptor, value: value}
}

func (factory *testEffectAuthorityFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *testEffectAuthorityFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return mount.Publisher.Provide(presentation.EffectAuthorityContract, factory.value)
}

func TestEffectsDescriptorLocksDeclarationsDependenciesAndPermissionCeilings(t *testing.T) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := factory.Descriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "openrealtime.presentation.host.effects" ||
		descriptor.Realm != plugin.PresentationHostRealm ||
		!slices.Equal(descriptor.Platforms, []string{"go"}) {
		t.Fatalf("effects descriptor placement = %#v", descriptor)
	}
	if !slices.Equal(descriptor.Provides, []plugin.Contract{presentation.EffectsContract}) ||
		descriptor.ConfigSchema == nil || *descriptor.ConfigSchema != presentation.EffectsConfigContract {
		t.Fatalf("effects descriptor contracts = %#v", descriptor)
	}
	for _, contract := range []plugin.Contract{
		presentation.HTTPRoutesContract, presentation.ArtifactStoreContract, presentation.DownloadStoreContract,
		presentation.EffectAuthorityContract,
	} {
		if !slices.ContainsFunc(descriptor.Requires, func(requirement plugin.Requirement) bool {
			return requirement.Contract == contract && !requirement.Optional
		}) {
			t.Fatalf("effects descriptor lacks exact dependency %s: %#v", contract.Name, descriptor.Requires)
		}
	}
	wantPermissions := []plugin.Permission{
		{Kind: effectPermissionKind, Resource: artifactEffectResource, Operations: []string{effectOperation}},
		{Kind: effectPermissionKind, Resource: downloadEffectResource, Operations: []string{effectOperation}},
	}
	if !slices.EqualFunc(descriptor.Permissions, wantPermissions, equalPermission) {
		t.Fatalf("effects permission ceiling = %#v, want %#v", descriptor.Permissions, wantPermissions)
	}
	if len(descriptor.Assets) != 1 || descriptor.Assets[0].Digest != contentDigest(factory.DeclarationsDocument()) ||
		descriptor.Assets[0].Name != "effects/declarations.v1.json" {
		t.Fatalf("declaration catalog asset = %#v", descriptor.Assets)
	}
	if factory.CatalogDigest() != descriptor.Assets[0].Digest {
		t.Fatalf("catalog digest = %q, want descriptor asset %q",
			factory.CatalogDigest(), descriptor.Assets[0].Digest)
	}
	defaultDigest, err := DefaultEffectsCatalogDigest()
	if err != nil || defaultDigest != factory.CatalogDigest() {
		t.Fatalf("default catalog digest = %q, %v", defaultDigest, err)
	}
	var catalog struct {
		FormatVersion uint64              `json:"format_version"`
		Tools         []EffectDeclaration `json:"tools"`
	}
	if err := json.Unmarshal(factory.DeclarationsDocument(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.FormatVersion != 1 || len(catalog.Tools) != 2 ||
		catalog.Tools[0].Name != "display_artifact" || catalog.Tools[1].Name != "publish_download" {
		t.Fatalf("locked declarations = %#v", catalog)
	}
	for _, declaration := range catalog.Tools {
		if declaration.Digest == "" || declaration.SessionConfirm != legacyaction.ConfirmNever {
			t.Fatalf("declaration is not exact and locally confirmed: %#v", declaration)
		}
	}
	identity, err := descriptor.Identity()
	if err != nil || identity.Digest == "" || descriptor.Lifecycle.DisposeTimeoutMS == 0 {
		t.Fatalf("effects identity/lifecycle = %#v, %v", identity, err)
	}

	mutated := factory.Descriptor()
	mutated.Permissions[0].Operations[0] = "widened"
	if factory.Descriptor().Permissions[0].Operations[0] != effectOperation {
		t.Fatal("factory descriptor was mutable through a clone")
	}
}

func TestDenyEffectAuthorityIsAnIndependentFailClosedPlugin(t *testing.T) {
	factory := NewDenyEffectAuthorityFactory()
	descriptor := factory.Descriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Name != "openrealtime.presentation.host.deny-effect-authority" ||
		descriptor.Realm != plugin.PresentationHostRealm ||
		!slices.Equal(descriptor.Provides, []plugin.Contract{presentation.EffectAuthorityContract}) ||
		len(descriptor.Requires) != 0 || len(descriptor.Permissions) != 0 {
		t.Fatalf("deny authority descriptor = %#v", descriptor)
	}
	identity, err := descriptor.Identity()
	if err != nil || identity.Digest == "" || descriptor.Lifecycle.DisposeTimeoutMS == 0 {
		t.Fatalf("deny authority identity/lifecycle = %#v, %v", identity, err)
	}
	decision, err := (denyEffectAuthority{}).Authorize(context.Background(), EffectAuthorityRequest{})
	if err == nil || decision != (EffectAuthorityDecision{}) {
		t.Fatalf("deny authority decision = %#v, %v", decision, err)
	}
}

func equalPermission(left, right plugin.Permission) bool {
	return left.Kind == right.Kind && left.Resource == right.Resource && left.Authority == right.Authority &&
		slices.Equal(left.Operations, right.Operations)
}

func attemptEffectMount(
	t *testing.T,
	effects *EffectsFactory,
	authority pluginruntime.Factory,
	grantEffects bool,
) (*pluginruntime.Mounted, error) {
	t.Helper()
	router := NewRouterFactory()
	artifacts := NewArtifactStoreFactory()
	downloads := NewDownloadStoreFactory()
	factories := []pluginruntime.Factory{router, artifacts, downloads, authority, effects}
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	ids := []string{"router", "artifacts", "downloads", "authority", "effects"}
	for index, factory := range factories {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			t.Fatal(err)
		}
		if err := registry.Register("", factory); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: ids[index], Plugin: factory.Descriptor().Name, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion, Name: "presentation.effects.mount-attempt", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
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
	permissions := map[string][]plugin.Permission{
		"artifacts": artifacts.Descriptor().Permissions,
		"downloads": downloads.Descriptor().Permissions,
	}
	if grantEffects {
		permissions["effects"] = effects.Descriptor().Permissions
	}
	return pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"artifacts": json.RawMessage(`{}`), "downloads": json.RawMessage(`{}`),
			"effects": json.RawMessage(`{}`),
		},
		Permissions: permissions,
	})
}

func TestEffectsMountRequiresExactGrantsAndTypedAuthority(t *testing.T) {
	effects, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mounted, err := attemptEffectMount(t, effects, NewDenyEffectAuthorityFactory(), false); err == nil {
		_ = mounted.Close(context.Background())
		t.Fatal("effects mounted without its declared effect grants")
	} else if !strings.Contains(err.Error(), "lack deployment grant") {
		t.Fatalf("missing grant error = %v", err)
	}
	wrongType := newTestEffectAuthorityFactory(struct{}{})
	if mounted, err := attemptEffectMount(t, effects, wrongType, true); err == nil {
		_ = mounted.Close(context.Background())
		t.Fatal("effects mounted a wrong-typed authority service")
	} else if !strings.Contains(err.Error(), "wrong Go type") {
		t.Fatalf("wrong authority type error = %v", err)
	}
}

type effectLookupFixture struct {
	value    any
	contract plugin.Contract
	found    bool
}

func (fixture effectLookupFixture) Lookup(string) (any, plugin.Contract, string, uint64, bool) {
	return fixture.value, fixture.contract, "authority", 1, fixture.found
}

func TestEffectAuthorityLookupRejectsMissingWrongContractAndWrongType(t *testing.T) {
	tests := []effectLookupFixture{
		{found: false},
		{found: true, contract: presentation.EffectsContract, value: denyEffectAuthority{}},
		{found: true, contract: presentation.EffectAuthorityContract, value: struct{}{}},
	}
	for _, fixture := range tests {
		if authority, err := lookupEffectAuthority(fixture); err == nil || authority != nil {
			t.Fatalf("unsafe authority lookup = %T, %v for %#v", authority, err, fixture)
		}
	}
}

func TestEffectsConfigurationIsStrictAndHardBounded(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "defaults", raw: `{}`},
		{name: "explicit", raw: `{"max_sessions":2,"max_message_bytes":65536,"max_result_bytes":32,"max_in_flight":2,"max_calls":3,"max_audit_records":4,"confirmation_timeout_ms":100,"execution_timeout_ms":100}`},
		{name: "duplicate", raw: `{"max_sessions":2,"max_sessions":3}`, wantErr: "duplicate"},
		{name: "unknown", raw: `{"unbounded":true}`, wantErr: "unknown field"},
		{name: "null", raw: `null`, wantErr: "object"},
		{name: "array", raw: `[]`, wantErr: "object"},
		{name: "small message", raw: `{"max_message_bytes":1024}`, wantErr: "max_message_bytes"},
		{name: "large message", raw: `{"max_message_bytes":50331649}`, wantErr: "max_message_bytes"},
		{name: "result at message", raw: `{"max_message_bytes":65536,"max_result_bytes":65536}`, wantErr: "max_result_bytes"},
		{name: "calls below flight", raw: `{"max_in_flight":3,"max_calls":2}`, wantErr: "max_calls"},
		{name: "terminal memory product", raw: `{"max_calls":16384}`, wantErr: "times max_result_bytes"},
		{name: "short confirmation", raw: `{"confirmation_timeout_ms":99}`, wantErr: "confirmation_timeout_ms"},
		{name: "long execution", raw: `{"execution_timeout_ms":300001}`, wantErr: "execution_timeout_ms"},
		{name: "trailing", raw: `{} {}`, wantErr: "trailing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits, err := parseEffectConfig([]byte(test.raw))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("config error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if limits.MaxSessions < 1 || limits.MaxMessageBytes < minimumEffectMessageBytes ||
				limits.MaxCalls < limits.MaxInFlight || limits.Confirmation <= 0 || limits.Execution <= 0 {
				t.Fatalf("invalid effective limits = %#v", limits)
			}
		})
	}
}

func TestEffectToolConstructionRejectsAmbiguousOrWidenableDeclarations(t *testing.T) {
	valid := EffectTool{
		Declaration: EffectDeclaration{
			Name: "fixture.run", Description: "Run a fixture.", Channel: EffectChannelTool,
			Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Executor: EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
			return EffectResult{}, nil
		}),
	}
	tests := []struct {
		name   string
		mutate func(*EffectTool)
	}{
		{name: "open schema", mutate: func(tool *EffectTool) {
			tool.Declaration.Parameters = json.RawMessage(`{"type":"object"}`)
		}},
		{name: "duplicate schema key", mutate: func(tool *EffectTool) {
			tool.Declaration.Parameters = json.RawMessage(`{"type":"object","type":"object","additionalProperties":false}`)
		}},
		{name: "schema reference", mutate: func(tool *EffectTool) {
			tool.Declaration.Parameters = json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"https://attacker.invalid/schema"}},"additionalProperties":false}`)
		}},
		{name: "remote confirmation", mutate: func(tool *EffectTool) {
			tool.Declaration.SessionConfirm = legacyaction.ConfirmAlways
		}},
		{name: "target without fence", mutate: func(tool *EffectTool) { tool.Declaration.Target = "browser-1" }},
		{name: "fence without target", mutate: func(tool *EffectTool) {
			tool.TargetFence = EffectTargetFenceFunc(func(context.Context, EffectTargetRequest) error { return nil })
		}},
		{name: "missing executor", mutate: func(tool *EffectTool) { tool.Executor = nil }},
		{name: "duplicate builtin", mutate: func(tool *EffectTool) { tool.Declaration.Name = "display_artifact" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Declaration = valid.Declaration.clone()
			test.mutate(&candidate)
			if _, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{candidate}}); err == nil {
				t.Fatal("unsafe effect declaration was accepted")
			}
		})
	}
}

type mountedEffectTestHost struct {
	mounted *pluginruntime.Mounted
	server  *httptest.Server
	effects Effects
}

func mountEffectTestHost(
	t *testing.T, factory *EffectsFactory, authority EffectAuthority, effectsValue json.RawMessage,
) *mountedEffectTestHost {
	t.Helper()
	if effectsValue == nil {
		effectsValue = json.RawMessage(`{}`)
	}
	router := NewRouterFactory()
	artifacts := NewArtifactStoreFactory()
	downloads := NewDownloadStoreFactory()
	var authorityFactory pluginruntime.Factory = NewDenyEffectAuthorityFactory()
	if authority != nil {
		authorityFactory = newTestEffectAuthorityFactory(authority)
	}
	factories := []pluginruntime.Factory{router, artifacts, downloads, authorityFactory, factory}
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	for _, implementation := range factories {
		if _, err := catalog.Register(implementation.Descriptor()); err != nil {
			t.Fatal(err)
		}
		if err := registry.Register("", implementation); err != nil {
			t.Fatal(err)
		}
	}
	entries := []plugin.ProfileEntry{
		{ID: "router", Plugin: router.Descriptor().Name, Scope: "root"},
		{ID: "artifacts", Plugin: artifacts.Descriptor().Name, Scope: "root"},
		{ID: "downloads", Plugin: downloads.Descriptor().Name, Scope: "root"},
		{ID: "authority", Plugin: authorityFactory.Descriptor().Name, Scope: "root"},
		{ID: "effects", Plugin: factory.Descriptor().Name, Scope: "root"},
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion, Name: "presentation.effects.test", Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries,
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
	permissions := make(map[string][]plugin.Permission)
	for entry, descriptor := range map[string]plugin.Descriptor{
		"artifacts": artifacts.Descriptor(), "downloads": downloads.Descriptor(), "effects": factory.Descriptor(),
	} {
		permissions[entry] = descriptor.Clone().Permissions
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values: map[string]json.RawMessage{
			"artifacts": json.RawMessage(`{}`), "downloads": json.RawMessage(`{}`), "effects": effectsValue,
		},
		Permissions: permissions,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		_ = mounted.Close(context.Background())
		t.Fatal(err)
	}
	value, contract, _, _, err := mounted.Export("effects")
	if err != nil || contract != presentation.EffectsContract {
		_ = mounted.Close(context.Background())
		t.Fatalf("effect export = %T/%#v, %v", value, contract, err)
	}
	server := httptest.NewServer(handler)
	host := &mountedEffectTestHost{mounted: mounted, server: server, effects: value.(Effects)}
	t.Cleanup(func() {
		server.Close()
		if closeErr := mounted.Close(context.Background()); closeErr != nil {
			t.Errorf("close effect host: %v", closeErr)
		}
	})
	return host
}

type effectTestSocket struct {
	t          *testing.T
	connection *websocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc
	ready      effectServerMessage
}

func dialEffectTestSocket(t *testing.T, server *httptest.Server) *effectTestSocket {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	connection, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(server.URL, "http")+"/client/v1/effects", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	socket := &effectTestSocket{t: t, connection: connection, ctx: ctx, cancel: cancel}
	socket.ready = socket.receive()
	if socket.ready.Type != "ready" || socket.ready.ScopeID == "" {
		_ = connection.CloseNow()
		cancel()
		t.Fatalf("effect ready = %#v", socket.ready)
	}
	t.Cleanup(func() {
		_ = connection.CloseNow()
		cancel()
	})
	return socket
}

func (socket *effectTestSocket) send(value any) {
	socket.t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		socket.t.Fatal(err)
	}
	if err := socket.connection.Write(socket.ctx, websocket.MessageText, payload); err != nil {
		socket.t.Fatal(err)
	}
}

func (socket *effectTestSocket) sendRaw(kind websocket.MessageType, payload []byte) {
	socket.t.Helper()
	if err := socket.connection.Write(socket.ctx, kind, payload); err != nil {
		socket.t.Fatal(err)
	}
}

func (socket *effectTestSocket) receive() effectServerMessage {
	socket.t.Helper()
	_, payload, err := socket.connection.Read(socket.ctx)
	if err != nil {
		socket.t.Fatal(err)
	}
	var message effectServerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		socket.t.Fatalf("decode effect response %q: %v", payload, err)
	}
	return message
}

func effectFixtureCall(sessionID, id, name string, arguments any, authority string) map[string]any {
	return map[string]any{
		"type": "call", "session_id": sessionID, "id": id, "name": name,
		"arguments": arguments, "authority": authority,
	}
}

func TestEffectsExecuteArtifactAndDownloadThroughSameCleanHost(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)
	if len(socket.ready.Tools) != 2 || socket.ready.Tools[0].Name != "display_artifact" ||
		socket.ready.Tools[1].Name != "publish_download" || socket.ready.Version != 1 ||
		socket.ready.CatalogDigest != factory.CatalogDigest() {
		t.Fatalf("ready declarations = %#v", socket.ready.Tools)
	}
	artifactArguments := map[string]any{
		"artifact_id": "totals", "title": "Totals", "html": "<p>42</p>",
	}
	socket.send(effectFixtureCall("sess_fixture", "call-artifact", "display_artifact", artifactArguments, "signed"))
	artifactResult := socket.receive()
	if artifactResult.Type != "result" || artifactResult.Error != nil || artifactResult.Artifact == nil ||
		artifactResult.Artifact.ID != "totals" || artifactResult.Artifact.Version != 1 {
		t.Fatalf("artifact result = %#v", artifactResult)
	}
	response, err := http.Get(host.server.URL + artifactResult.Artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "42") {
		t.Fatalf("artifact resource status=%d body=%q", response.StatusCode, body)
	}

	downloadArguments := map[string]any{
		"artifact_id": "report", "filename": "report.csv", "media_type": "text/csv", "text": "a,b\n1,2\n",
	}
	socket.send(effectFixtureCall("sess_fixture", "call-download", "publish_download", downloadArguments, "signed"))
	downloadResult := socket.receive()
	if downloadResult.Error != nil || downloadResult.Download == nil || downloadResult.Download.ID != "report" {
		t.Fatalf("download result = %#v", downloadResult)
	}
	response, err = http.Get(host.server.URL + downloadResult.Download.Path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "a,b\n1,2\n" ||
		!strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download resource status=%d headers=%v body=%q", response.StatusCode, response.Header, body)
	}

	// Exact duplicate: authority is rechecked, but dispatch and publication do
	// not repeat. This also proves result metadata comes from the store.
	socket.send(effectFixtureCall("sess_fixture", "call-artifact", "display_artifact", artifactArguments, "signed"))
	replayed := socket.receive()
	if replayed.Error != nil || replayed.Artifact == nil || replayed.Artifact.Version != 1 {
		t.Fatalf("replayed artifact = %#v", replayed)
	}
	socket.send(effectFixtureCall("sess_fixture", "call-artifact", "display_artifact", map[string]any{
		"artifact_id": "totals", "title": "Totals", "html": "<p>widened</p>",
	}, "signed"))
	collision := socket.receive()
	if collision.Error == nil || collision.Error.Code != "idempotency_refused" {
		t.Fatalf("changed call identity = %#v", collision)
	}
	if authority.calls.Load() != 4 {
		t.Fatalf("authority checks = %d, want one per call", authority.calls.Load())
	}
	stats := host.effects.Stats()
	if stats.Executed != 2 || stats.Replayed != 1 || stats.Refused != 1 {
		t.Fatalf("effect stats = %#v", stats)
	}

	if err := host.mounted.Unmount(context.Background(), "effects"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, host.server.URL+"/client/v1/effects", http.StatusNotFound)
	assertStatus(t, host.server.URL+artifactResult.Artifact.Path, http.StatusOK)
	if stats := host.effects.Stats(); !stats.Closed || stats.ActiveSessions != 0 || stats.InFlight != 0 {
		t.Fatalf("withdrawn effect provider retained scoped work: %#v", stats)
	}
}

func TestEffectAuthorityDependencyLossDisposesRouteAndSessionsThenRemounts(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)
	oldEffects := host.effects
	if err := host.mounted.Unmount(context.Background(), "authority"); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, host.server.URL+"/client/v1/effects", http.StatusNotFound)
	if stats := oldEffects.Stats(); !stats.Closed || stats.ActiveSessions != 0 || stats.InFlight != 0 {
		t.Fatalf("authority loss retained effect scope: %#v", stats)
	}
	if live := host.mounted.Live(); live.Entries["authority"].State != "inactive" ||
		live.Entries["effects"].State != "inactive" {
		t.Fatalf("authority dependency loss state = %#v", live.Entries)
	}
	_ = socket.connection.CloseNow()
	if err := host.mounted.Activate(context.Background(), "authority"); err != nil {
		t.Fatal(err)
	}
	value, contract, _, _, err := host.mounted.Export("effects")
	if err != nil || contract != presentation.EffectsContract {
		t.Fatalf("reactivated effect export = %T/%#v, %v", value, contract, err)
	}
	if value.(Effects) == oldEffects || value.(Effects).Stats().Closed {
		t.Fatal("reactivation reused the disposed effect scope")
	}
	reactivated := dialEffectTestSocket(t, host.server)
	reactivated.send(effectFixtureCall("sess_reactivated", "artifact", "display_artifact", map[string]any{
		"artifact_id": "reactivated", "title": "Reactivated", "html": "<p>ready</p>",
	}, "signed"))
	if result := reactivated.receive(); result.Error != nil || result.Artifact == nil {
		t.Fatalf("reactivated effect result = %#v", result)
	}
}

func fixtureEffectTool(
	name string, confirm legacyaction.Confirm, target string, counter *atomic.Int64,
) EffectTool {
	var fence EffectTargetFence
	if target != "" {
		fence, _ = ExactArgumentTargetFence("source")
	}
	return EffectTool{
		Declaration: EffectDeclaration{
			Name: name, Description: "Execute one bounded fixture effect.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"source":{"type":"string"},"value":{"type":"string","maxLength":64}},
				"required":["source","value"],"additionalProperties":false
			}`),
			Confirm: confirm, Target: target, Mutating: true, Channel: EffectChannelComputer,
		},
		Executor: EffectExecutorFunc(func(_ context.Context, call EffectCall) (EffectResult, error) {
			counter.Add(1)
			return EffectResult{Output: `{"status":"clicked","call_id":"` + call.CallID + `"}`}, nil
		}),
		TargetFence: fence,
	}
}

func TestEffectsKeepAuthorityConfirmationTargetAndIdempotencyHostOwned(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var executions atomic.Int64
	tool := fixtureEffectTool("computer.fixture_click", legacyaction.ConfirmAlways, "browser-1", &executions)
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)
	arguments := map[string]any{"source": "browser-1", "value": "first"}

	// Forged evidence is rejected before a confirmation request can make it
	// look authoritative to a person.
	socket.send(effectFixtureCall("sess_boundary", "unauthorized", tool.Declaration.Name, arguments, "forged"))
	unauthorized := socket.receive()
	if unauthorized.Type != "result" || unauthorized.Error == nil ||
		unauthorized.Error.Code != "authority_denied" || executions.Load() != 0 {
		t.Fatalf("unauthorized call = %#v, executions=%d", unauthorized, executions.Load())
	}
	socket.send(effectFixtureCall("sess_boundary", "mismatched", tool.Declaration.Name, arguments, "mismatch"))
	mismatched := socket.receive()
	if mismatched.Error == nil || mismatched.Error.Code != "authority_mismatch" || executions.Load() != 0 {
		t.Fatalf("mismatched authority = %#v", mismatched)
	}

	// Target is sourced from the declaration. A signed call whose arguments
	// escape it is refused without asking or executing.
	socket.send(effectFixtureCall("sess_boundary", "outside", tool.Declaration.Name,
		map[string]any{"source": "screen-2", "value": "outside"}, "signed"))
	outside := socket.receive()
	if outside.Error == nil || outside.Error.Code != "target_rejected" || executions.Load() != 0 {
		t.Fatalf("outside target = %#v", outside)
	}

	socket.send(effectFixtureCall("sess_boundary", "confirmed", tool.Declaration.Name, arguments, "signed"))
	confirmation := socket.receive()
	if confirmation.Type != "confirm" || confirmation.Confirm != legacyaction.ConfirmAlways ||
		confirmation.Target != "browser-1" || confirmation.Nonce == "" || executions.Load() != 0 {
		t.Fatalf("host confirmation = %#v, executions=%d", confirmation, executions.Load())
	}
	wrongNonce := strings.Repeat("0", 32)
	if wrongNonce == confirmation.Nonce {
		wrongNonce = strings.Repeat("1", 32)
	}
	socket.send(map[string]any{
		"type": "decide", "id": "confirmed", "nonce": wrongNonce, "approved": true,
	})
	wrongDecision := socket.receive()
	if wrongDecision.Type != "error" || wrongDecision.Error == nil ||
		wrongDecision.Error.Code != "confirmation_mismatch" || executions.Load() != 0 {
		t.Fatalf("wrong confirmation receipt = %#v", wrongDecision)
	}
	socket.send(map[string]any{
		"type": "decide", "id": "confirmed", "nonce": confirmation.Nonce, "approved": true,
	})
	result := socket.receive()
	if result.Type != "result" || result.Error != nil || executions.Load() != 1 {
		t.Fatalf("confirmed effect = %#v, executions=%d", result, executions.Load())
	}

	// An exact retry rechecks authority and returns the terminal result without
	// issuing another confirmation or crossing the effect boundary again.
	socket.send(effectFixtureCall("sess_boundary", "confirmed", tool.Declaration.Name, arguments, "signed"))
	replay := socket.receive()
	if replay.Type != "result" || replay.Error != nil || executions.Load() != 1 {
		t.Fatalf("idempotent replay = %#v, executions=%d", replay, executions.Load())
	}
	socket.send(effectFixtureCall("sess_boundary", "confirmed", tool.Declaration.Name,
		map[string]any{"source": "browser-1", "value": "changed"}, "signed"))
	collision := socket.receive()
	if collision.Error == nil || collision.Error.Code != "idempotency_refused" || executions.Load() != 1 {
		t.Fatalf("call identity collision = %#v", collision)
	}

	// The wire schema has no confirmation or target override. Even values that
	// agree with the host declaration are refused as client-owned widening.
	socket.send(map[string]any{
		"type": "call", "session_id": "sess_boundary", "id": "widen", "name": tool.Declaration.Name,
		"arguments": arguments, "authority": "signed", "confirm": "never", "target": "browser-1",
	})
	widened := socket.receive()
	if widened.Error == nil || widened.Error.Code != "invalid_call" || executions.Load() != 1 {
		t.Fatalf("client declaration override = %#v", widened)
	}

	socket.send(effectFixtureCall("sess_boundary", "declined", tool.Declaration.Name, arguments, "signed"))
	declinePrompt := socket.receive()
	socket.send(map[string]any{
		"type": "decide", "id": "declined", "nonce": declinePrompt.Nonce, "approved": false,
	})
	declined := socket.receive()
	if declined.Error == nil || declined.Error.Code != "confirmation_declined" || executions.Load() != 1 {
		t.Fatalf("declined effect = %#v", declined)
	}

	stats := host.effects.Stats()
	if stats.Executed != 1 || stats.Replayed != 1 || stats.Refused < 6 {
		t.Fatalf("boundary stats = %#v", stats)
	}
	for _, record := range host.effects.Audit() {
		encoded, _ := json.Marshal(record)
		if strings.Contains(string(encoded), "signed") || strings.Contains(string(encoded), "first") {
			t.Fatalf("audit retained authority or arguments: %s", encoded)
		}
	}
}

func TestUnansweredEffectConfirmationBecomesAnExplicitResult(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var executions atomic.Int64
	tool := fixtureEffectTool("fixture.confirm", legacyaction.ConfirmAlways, "browser-1", &executions)
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, json.RawMessage(`{
		"confirmation_timeout_ms":100,"execution_timeout_ms":1000
	}`))
	socket := dialEffectTestSocket(t, host.server)
	socket.send(effectFixtureCall("sess_timeout", "unanswered", tool.Declaration.Name,
		map[string]any{"source": "browser-1", "value": "wait"}, "signed"))
	if prompt := socket.receive(); prompt.Type != "confirm" {
		t.Fatalf("confirmation prompt = %#v", prompt)
	}
	started := time.Now()
	result := socket.receive()
	if result.Type != "result" || result.Error == nil || result.Error.Code != "confirmation_timeout" ||
		executions.Load() != 0 {
		t.Fatalf("unanswered result = %#v, executions=%d", result, executions.Load())
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("confirmation timeout elapsed = %s", elapsed)
	}
}

func TestEffectAuthorityAndExecutorTimeoutsBecomeExplicitTerminalResults(t *testing.T) {
	t.Run("authority", func(t *testing.T) {
		authority := EffectAuthorityFunc(func(ctx context.Context, _ EffectAuthorityRequest) (EffectAuthorityDecision, error) {
			<-ctx.Done()
			return EffectAuthorityDecision{}, ctx.Err()
		})
		factory, err := NewEffectsFactory(EffectsOptions{})
		if err != nil {
			t.Fatal(err)
		}
		host := mountEffectTestHost(t, factory, authority, json.RawMessage(`{"execution_timeout_ms":100}`))
		socket := dialEffectTestSocket(t, host.server)
		socket.send(effectFixtureCall("sess_authority_timeout", "authority-timeout", "display_artifact", map[string]any{
			"artifact_id": "timeout", "title": "Timeout", "html": "x",
		}, "signed"))
		result := socket.receive()
		if result.Error == nil || result.Error.Code != "authority_timeout" || host.effects.Stats().Executed != 0 {
			t.Fatalf("authority timeout = %#v stats=%#v", result, host.effects.Stats())
		}
	})

	t.Run("executor", func(t *testing.T) {
		authority := &exactTestEffectAuthority{}
		var executions atomic.Int64
		tool := fixtureEffectTool("fixture.timeout", legacyaction.ConfirmNever, "", &executions)
		tool.Executor = EffectExecutorFunc(func(ctx context.Context, _ EffectCall) (EffectResult, error) {
			executions.Add(1)
			<-ctx.Done()
			return EffectResult{}, ctx.Err()
		})
		factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
		if err != nil {
			t.Fatal(err)
		}
		host := mountEffectTestHost(t, factory, authority, json.RawMessage(`{"execution_timeout_ms":100}`))
		socket := dialEffectTestSocket(t, host.server)
		socket.send(effectFixtureCall("sess_executor_timeout", "executor-timeout", tool.Declaration.Name,
			map[string]any{"source": "none", "value": "timeout"}, "signed"))
		result := socket.receive()
		if result.Error == nil || result.Error.Code != "effect_timeout" || executions.Load() != 1 {
			t.Fatalf("executor timeout = %#v executions=%d", result, executions.Load())
		}
		audit := host.effects.Audit()
		if len(audit) == 0 || !audit[len(audit)-1].Crossed || !audit[len(audit)-1].Executed {
			t.Fatalf("executor timeout lost irreversible crossing: %#v", audit)
		}
	})
}

func TestNilEffectAuthorityIsDenyAll(t *testing.T) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, nil, nil)
	socket := dialEffectTestSocket(t, host.server)
	socket.send(effectFixtureCall("sess_deny", "call-deny", "display_artifact", map[string]any{
		"artifact_id": "denied", "title": "Denied", "html": "<p>must not publish</p>",
	}, "anything"))
	result := socket.receive()
	if result.Error == nil || result.Error.Code != "authority_denied" || host.effects.Stats().Executed != 0 {
		t.Fatalf("deny-all result = %#v stats=%#v", result, host.effects.Stats())
	}
	assertStatus(t, host.server.URL+"/client/v1/artifacts/denied", http.StatusNotFound)
}

func TestEffectIdempotencySurvivesSocketReplacementAndConcurrentClients(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var executions atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	tool := fixtureEffectTool("fixture.once", legacyaction.ConfirmNever, "", &executions)
	tool.Executor = EffectExecutorFunc(func(_ context.Context, call EffectCall) (EffectResult, error) {
		if executions.Add(1) == 1 {
			close(started)
		}
		<-release
		return EffectResult{Output: `{"call_id":"` + call.CallID + `"}`}, nil
	})
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	first := dialEffectTestSocket(t, host.server)
	second := dialEffectTestSocket(t, host.server)
	call := effectFixtureCall("sess_reconnect", "same-call", tool.Declaration.Name,
		map[string]any{"source": "ignored", "value": "same"}, "signed")
	first.send(call)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first effect never began")
	}
	second.send(call)
	close(release)
	firstResult := first.receive()
	secondResult := second.receive()
	if firstResult.Error != nil || secondResult.Error != nil || executions.Load() != 1 {
		t.Fatalf("concurrent duplicate results first=%#v second=%#v executions=%d",
			firstResult, secondResult, executions.Load())
	}
	_ = first.connection.CloseNow()

	// A replacement socket still sees the hub-owned terminal result. Closing
	// the original client scope therefore cannot reopen the effect boundary.
	replacement := dialEffectTestSocket(t, host.server)
	replacement.send(call)
	replayed := replacement.receive()
	if replayed.Error != nil || executions.Load() != 1 {
		t.Fatalf("replacement replay = %#v executions=%d", replayed, executions.Load())
	}
	replacement.send(effectFixtureCall("sess_reconnect", "same-call", tool.Declaration.Name,
		map[string]any{"source": "ignored", "value": "changed"}, "signed"))
	collision := replacement.receive()
	if collision.Error == nil || collision.Error.Code != "idempotency_refused" || executions.Load() != 1 {
		t.Fatalf("replacement identity collision = %#v", collision)
	}
	if stats := host.effects.Stats(); stats.Executed != 1 || stats.Replayed != 2 {
		t.Fatalf("hub idempotency stats = %#v", stats)
	}
}

func TestEffectClientSessionsKeepConfirmationAndCleanupScopesIndependent(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var executions atomic.Int64
	tool := fixtureEffectTool("fixture.isolated", legacyaction.ConfirmAlways, "browser-1", &executions)
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	first := dialEffectTestSocket(t, host.server)
	second := dialEffectTestSocket(t, host.server)
	arguments := map[string]any{"source": "browser-1", "value": "isolate"}
	first.send(effectFixtureCall("sess_first", "same-id", tool.Declaration.Name, arguments, "signed"))
	second.send(effectFixtureCall("sess_second", "same-id", tool.Declaration.Name, arguments, "signed"))
	firstPrompt := first.receive()
	secondPrompt := second.receive()
	if firstPrompt.Nonce == "" || secondPrompt.Nonce == "" || firstPrompt.Nonce == secondPrompt.Nonce {
		t.Fatalf("confirmation scopes share a nonce: first=%#v second=%#v", firstPrompt, secondPrompt)
	}
	second.send(map[string]any{
		"type": "decide", "id": "same-id", "nonce": firstPrompt.Nonce, "approved": true,
	})
	if mismatch := second.receive(); mismatch.Error == nil || mismatch.Error.Code != "confirmation_mismatch" {
		t.Fatalf("cross-session decision = %#v", mismatch)
	}
	_ = first.connection.CloseNow()
	second.send(map[string]any{
		"type": "decide", "id": "same-id", "nonce": secondPrompt.Nonce, "approved": true,
	})
	if result := second.receive(); result.Error != nil || executions.Load() != 1 {
		t.Fatalf("surviving session result = %#v executions=%d", result, executions.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := host.effects.Stats()
		if stats.ActiveSessions <= 1 && stats.InFlight == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if stats := host.effects.Stats(); stats.ActiveSessions < 1 || stats.InFlight != 0 {
		t.Fatalf("session cleanup stats = %#v", stats)
	}
}

func TestUnmountCancelsEffectWorkDrainsSessionsAndRemovesOnlyItsRoute(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	started := make(chan struct{})
	stopped := make(chan struct{})
	var executions atomic.Int64
	tool := fixtureEffectTool("fixture.blocking", legacyaction.ConfirmNever, "", &executions)
	tool.Executor = EffectExecutorFunc(func(ctx context.Context, _ EffectCall) (EffectResult, error) {
		executions.Add(1)
		close(started)
		<-ctx.Done()
		close(stopped)
		return EffectResult{}, ctx.Err()
	})
	factory, err := NewEffectsFactory(EffectsOptions{Tools: []EffectTool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)
	socket.send(effectFixtureCall("sess_unmount", "blocking", tool.Declaration.Name,
		map[string]any{"source": "none", "value": "block"}, "signed"))
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking effect never started")
	}
	if err := host.mounted.Unmount(context.Background(), "effects"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("unmount did not cancel the scoped executor")
	}
	if stats := host.effects.Stats(); !stats.Closed || stats.ActiveSessions != 0 || stats.InFlight != 0 {
		t.Fatalf("unmounted effect scope = %#v", stats)
	}
	assertStatus(t, host.server.URL+"/client/v1/effects", http.StatusNotFound)
	// The independent stores remain mounted; withdrawing an effect provider
	// cannot dispose their routes.
	assertStatus(t, host.server.URL+"/client/v1/artifacts/missing", http.StatusNotFound)
	if executions.Load() != 1 {
		t.Fatalf("blocking effect executions = %d", executions.Load())
	}
}

func TestEffectsRejectMalformedOversizedAndAdversarialMessages(t *testing.T) {
	authority := &exactTestEffectAuthority{}
	var hugeExecutions atomic.Int64
	huge := fixtureEffectTool("fixture.huge", legacyaction.ConfirmNever, "", &hugeExecutions)
	huge.Executor = EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
		hugeExecutions.Add(1)
		return EffectResult{Output: strings.Repeat("x", defaultMaxEffectResultBytes+1)}, nil
	})
	var panicExecutions atomic.Int64
	panicking := fixtureEffectTool("fixture.panic", legacyaction.ConfirmNever, "", &panicExecutions)
	panicking.Executor = EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
		panicExecutions.Add(1)
		panic("secret panic payload")
	})
	var forgedExecutions atomic.Int64
	forged := fixtureEffectTool("fixture.forged_reference", legacyaction.ConfirmNever, "", &forgedExecutions)
	forged.Executor = EffectExecutorFunc(func(context.Context, EffectCall) (EffectResult, error) {
		forgedExecutions.Add(1)
		return EffectResult{Artifact: &Artifact{ID: "not-owned", Path: "/client/v1/artifacts/not-owned"}}, nil
	})
	factory, err := NewEffectsFactory(EffectsOptions{
		Tools: []EffectTool{huge, panicking, forged},
	})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, authority, nil)
	socket := dialEffectTestSocket(t, host.server)

	socket.sendRaw(websocket.MessageText, []byte(`{"type":"call","type":"call","session_id":"sess_bad","id":"duplicate","name":"display_artifact","arguments":{},"authority":"signed"}`))
	if message := socket.receive(); message.Type != "error" || message.Error == nil || message.Error.Code != "invalid_message" {
		t.Fatalf("duplicate-key response = %#v", message)
	}
	socket.send(map[string]any{
		"type": "call", "session_id": "sess_bad", "id": "extra-field", "name": "display_artifact",
		"arguments": map[string]any{}, "authority": "signed", "unbounded": true,
	})
	if message := socket.receive(); message.Type != "result" || message.Error == nil || message.Error.Code != "invalid_call" {
		t.Fatalf("unknown-field response = %#v", message)
	}
	socket.send(effectFixtureCall("sess_bad", "unknown-tool", "attacker.exfiltrate",
		map[string]any{}, "signed"))
	if message := socket.receive(); message.Error == nil || message.Error.Code != "unknown_tool" {
		t.Fatalf("unknown-tool response = %#v", message)
	}
	socket.send(effectFixtureCall("sess_bad", "bad-schema", "display_artifact",
		map[string]any{"artifact_id": "x", "title": "X", "html": "x", "extra": true}, "signed"))
	if message := socket.receive(); message.Error == nil || message.Error.Code != "invalid_arguments" {
		t.Fatalf("open arguments response = %#v", message)
	}
	socket.send(effectFixtureCall("sess_bad", "bad-base64", "publish_download", map[string]any{
		"artifact_id": "x", "filename": "x.bin", "media_type": "application/octet-stream", "base64": "@@@",
	}, "signed"))
	if message := socket.receive(); message.Error == nil || message.Error.Code != "effect_failed" {
		t.Fatalf("invalid base64 response = %#v", message)
	}
	arguments := map[string]any{"source": "none", "value": "adversarial"}
	socket.send(effectFixtureCall("sess_bad", "huge", huge.Declaration.Name, arguments, "signed"))
	if message := socket.receive(); message.Error == nil || message.Error.Code != "invalid_result" || hugeExecutions.Load() != 1 {
		t.Fatalf("oversized result response = %#v executions=%d", message, hugeExecutions.Load())
	}
	socket.send(effectFixtureCall("sess_bad", "panic", panicking.Declaration.Name, arguments, "signed"))
	panicResult := socket.receive()
	if panicResult.Error == nil || panicResult.Error.Code != "effect_failed" ||
		strings.Contains(panicResult.Error.Message, "secret") || panicExecutions.Load() != 1 {
		t.Fatalf("panicking executor response = %#v executions=%d", panicResult, panicExecutions.Load())
	}
	socket.send(effectFixtureCall("sess_bad", "forged", forged.Declaration.Name, arguments, "signed"))
	if message := socket.receive(); message.Error == nil || message.Error.Code != "invalid_result" || forgedExecutions.Load() != 1 {
		t.Fatalf("forged reference response = %#v executions=%d", message, forgedExecutions.Load())
	}
	socket.send(map[string]any{
		"type": "decide", "id": "nothing-pending", "nonce": strings.Repeat("0", 32), "approved": true,
	})
	if message := socket.receive(); message.Type != "error" || message.Error == nil || message.Error.Code != "unknown_decision" {
		t.Fatalf("orphan decision response = %#v", message)
	}

	// The handler distrusts forwarding headers and reads the transport peer.
	handler, err := HTTPHandler(host.mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/client/v1/effects", nil)
	request.RemoteAddr = "198.51.100.9:4242"
	request.Header.Set("X-Forwarded-For", "127.0.0.1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback effect route status = %d", response.Code)
	}
}

func TestEffectSocketEnforcesItsWireMessageBound(t *testing.T) {
	factory, err := NewEffectsFactory(EffectsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	host := mountEffectTestHost(t, factory, &exactTestEffectAuthority{}, json.RawMessage(`{
		"max_message_bytes":65536,"max_result_bytes":1024
	}`))
	socket := dialEffectTestSocket(t, host.server)
	payload := []byte(`{"type":"call","session_id":"sess_large","id":"large","name":"display_artifact","arguments":{},"authority":"` +
		strings.Repeat("x", 70<<10) + `"}`)
	socket.sendRaw(websocket.MessageText, payload)
	if _, _, err := socket.connection.Read(socket.ctx); err == nil {
		t.Fatal("oversized effect message did not close the bounded socket")
	}
}

func FuzzEffectWireMessageDecoding(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"type":"call","session_id":"sess_fuzz","id":"call_fuzz","name":"display_artifact","arguments":{"artifact_id":"x","title":"X","html":"x"},"authority":"signed"}`),
		[]byte(`{"type":"decide","id":"call_fuzz","nonce":"00000000000000000000000000000000","approved":false}`),
		[]byte(`{"type":"call","type":"decide"}`),
		[]byte(`null`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maximumEffectMessageBytes+1 {
			t.Skip()
		}
		envelope, err := decodeEffectEnvelope(payload)
		if err != nil {
			return
		}
		switch envelope.Type {
		case "call":
			_, _ = decodeEffectCall(payload)
		case "decide":
			_, _ = decodeEffectDecision(payload)
		}
	})
}

func FuzzEffectArgumentAdmission(f *testing.F) {
	tools, err := builtinEffectTools()
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{
		[]byte(`{"artifact_id":"x","title":"X","html":"<p>x</p>"}`),
		[]byte(`{"artifact_id":"x","title":"X","html":"x","extra":true}`),
		[]byte(`{"artifact_id":"x","artifact_id":"y","title":"X","html":"x"}`),
		[]byte(`null`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, arguments []byte) {
		if len(arguments) > maximumEffectParametersBytes {
			t.Skip()
		}
		_, _, _ = canonicalEffectArguments(tools[0].schema, arguments)
	})
}
