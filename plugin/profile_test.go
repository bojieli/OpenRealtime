package plugin_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
)

func TestProfileLockAndCompileBindExactVisibleServices(t *testing.T) {
	connection := service("client.connection", 'a')
	conversation := service("client.conversation", 'b')
	catalog := plugin.NewCatalog()
	register(t, catalog, providerDescriptor("openrealtime.client.transport", plugin.ClientRealm, connection))
	register(t, catalog, plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.conversation", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"browser", "macos"},
		Provides: []plugin.Contract{conversation},
		Requires: []plugin.Requirement{{Contract: connection}},
	})
	register(t, catalog, plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.view", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"browser", "macos"},
		Requires: []plugin.Requirement{{Contract: conversation}},
	})

	profile := freezeProfile(t, plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.developer", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}},
		// Deliberately reverse dependency order. Compile must emit provider-first
		// without changing the semantic order stored in the profile.
		Entries: []plugin.ProfileEntry{
			{ID: "view", Plugin: "openrealtime.client.view", Scope: "root"},
			{ID: "conversation", Plugin: "openrealtime.client.conversation", Scope: "root"},
			{ID: "transport", Plugin: "openrealtime.client.transport", Scope: "root"},
		},
	})
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, entry := range plan.Entries {
		order = append(order, entry.Entry.ID)
	}
	if !reflect.DeepEqual(order, []string{"transport", "conversation", "view"}) {
		t.Fatalf("mount order = %v", order)
	}
	if plan.Fingerprint == "" || plan.ProfileFingerprint != profile.Fingerprint ||
		plan.LockFingerprint != lock.Fingerprint {
		t.Fatalf("incomplete plan identity: %#v", plan)
	}
	plan.Entries[0].Descriptor.Platforms[0] = "changed"
	again, err := plugin.Compile(profile, lock, catalog)
	if err != nil || again.Entries[0].Descriptor.Platforms[0] != "go" {
		t.Fatalf("compiled plan retained caller aliases: %v, %v", again.Entries[0].Descriptor.Platforms, err)
	}
}

func TestProfileCompileExportsExactProviderService(t *testing.T) {
	connection := service("client.connection", 'a')
	catalog := plugin.NewCatalog()
	register(t, catalog, providerDescriptor("openrealtime.client.transport", plugin.ClientRealm, connection))
	profile := freezeProfile(t, plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.developer", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{{
			ID: "transport", Plugin: "openrealtime.client.transport", Scope: "root",
		}},
		Exports: []plugin.ProfileExport{{
			Name: "connection", Provider: "transport", Service: connection.Name,
		}},
	})
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Exports) != 1 || plan.Exports[0] != (plugin.PlannedExport{
		Name: "connection", Provider: "transport", Service: connection,
	}) {
		t.Fatalf("compiled exports = %#v", plan.Exports)
	}

	bad := profile.Clone()
	bad.Exports[0].Service = "client.missing"
	bad, err = plugin.FreezeProfile(bad)
	if err != nil {
		t.Fatal(err)
	}
	badLock, err := plugin.ResolveProfile(bad, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plugin.Compile(bad, badLock, catalog); err == nil || !strings.Contains(err.Error(), "does not provide") {
		t.Fatalf("missing exported service error = %v", err)
	}
}

func TestProfileScopesIsolateAndShadowServices(t *testing.T) {
	connection := service("client.connection", 'a')
	wrongConnection := service("client.connection", 'b')
	consumer := func(name string, contract plugin.Contract) plugin.Descriptor {
		return plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          name, Revision: 1, Realm: plugin.ClientRealm, Platforms: []string{"portable"},
			Requires: []plugin.Requirement{{Contract: contract}},
		}
	}
	t.Run("ancestor visible", func(t *testing.T) {
		catalog := plugin.NewCatalog()
		register(t, catalog, providerDescriptor("openrealtime.client.root_transport", plugin.ClientRealm, connection))
		register(t, catalog, consumer("openrealtime.client.session_view", connection))
		profile := scopedProfile(t, nil, []plugin.ProfileEntry{
			{ID: "transport", Plugin: "openrealtime.client.root_transport", Scope: "root"},
			{ID: "view", Plugin: "openrealtime.client.session_view", Scope: "root/session"},
		})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		if _, err := plugin.Compile(profile, lock, catalog); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("isolated", func(t *testing.T) {
		catalog := plugin.NewCatalog()
		register(t, catalog, providerDescriptor("openrealtime.client.root_transport", plugin.ClientRealm, connection))
		register(t, catalog, consumer("openrealtime.client.session_view", connection))
		profile := scopedProfile(t, []string{"client.connection"}, []plugin.ProfileEntry{
			{ID: "transport", Plugin: "openrealtime.client.root_transport", Scope: "root"},
			{ID: "view", Plugin: "openrealtime.client.session_view", Scope: "root/session"},
		})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		if _, err := plugin.Compile(profile, lock, catalog); err == nil || !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("isolated dependency error = %v", err)
		}
	})
	t.Run("nearest mismatch does not fall through", func(t *testing.T) {
		catalog := plugin.NewCatalog()
		register(t, catalog, providerDescriptor("openrealtime.client.root_transport", plugin.ClientRealm, connection))
		register(t, catalog, providerDescriptor("openrealtime.client.local_transport", plugin.ClientRealm, wrongConnection))
		register(t, catalog, consumer("openrealtime.client.session_view", connection))
		profile := scopedProfile(t, nil, []plugin.ProfileEntry{
			{ID: "root_transport", Plugin: "openrealtime.client.root_transport", Scope: "root"},
			{ID: "local_transport", Plugin: "openrealtime.client.local_transport", Scope: "root/session"},
			{ID: "view", Plugin: "openrealtime.client.session_view", Scope: "root/session"},
		})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		if _, err := plugin.Compile(profile, lock, catalog); !errors.Is(err, plugin.ErrContractMismatch) {
			t.Fatalf("nearest mismatch error = %v", err)
		}
	})
}

func TestProfileCompileRejectsAmbiguityCycleAndStaleLock(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		contract := service("client.connection", 'a')
		catalog := plugin.NewCatalog()
		register(t, catalog, providerDescriptor("openrealtime.client.transport_a", plugin.ClientRealm, contract))
		register(t, catalog, providerDescriptor("openrealtime.client.transport_b", plugin.ClientRealm, contract))
		register(t, catalog, plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.client.view", Revision: 1, Realm: plugin.ClientRealm,
			Platforms: []string{"portable"}, Requires: []plugin.Requirement{{Contract: contract}},
		})
		profile := rootProfile(t, []plugin.ProfileEntry{
			{ID: "a", Plugin: "openrealtime.client.transport_a", Scope: "root"},
			{ID: "b", Plugin: "openrealtime.client.transport_b", Scope: "root"},
			{ID: "view", Plugin: "openrealtime.client.view", Scope: "root"},
		})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		if _, err := plugin.Compile(profile, lock, catalog); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous provider error = %v", err)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		left := service("client.left", 'a')
		right := service("client.right", 'b')
		catalog := plugin.NewCatalog()
		register(t, catalog, plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.client.left", Revision: 1, Realm: plugin.ClientRealm,
			Platforms: []string{"portable"}, Provides: []plugin.Contract{left},
			Requires: []plugin.Requirement{{Contract: right}},
		})
		register(t, catalog, plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.client.right", Revision: 1, Realm: plugin.ClientRealm,
			Platforms: []string{"portable"}, Provides: []plugin.Contract{right},
			Requires: []plugin.Requirement{{Contract: left}},
		})
		profile := rootProfile(t, []plugin.ProfileEntry{
			{ID: "left", Plugin: "openrealtime.client.left", Scope: "root"},
			{ID: "right", Plugin: "openrealtime.client.right", Scope: "root"},
		})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		if _, err := plugin.Compile(profile, lock, catalog); err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("dependency cycle error = %v", err)
		}
	})

	t.Run("stale lock", func(t *testing.T) {
		catalog := plugin.NewCatalog()
		register(t, catalog, providerDescriptor("openrealtime.client.transport", plugin.ClientRealm,
			service("client.connection", 'a')))
		profile := rootProfile(t, []plugin.ProfileEntry{{
			ID: "transport", Plugin: "openrealtime.client.transport", Scope: "root",
		}})
		lock, _ := plugin.ResolveProfile(profile, catalog)
		changed := profile.Clone()
		changed.Revision = 2
		changed, _ = plugin.FreezeProfile(changed)
		if _, err := plugin.Compile(changed, lock, catalog); err == nil || !strings.Contains(err.Error(), "targets") {
			t.Fatalf("stale lock error = %v", err)
		}
	})
}

func TestProfileLayersAndStrictRoundTrip(t *testing.T) {
	base := rootProfile(t, []plugin.ProfileEntry{
		{ID: "transport", Plugin: "openrealtime.client.transport", Scope: "root"},
		{ID: "view", Plugin: "openrealtime.client.view", Scope: "root"},
	})
	replacement := plugin.ProfileEntry{
		ID: "transport", Plugin: "openrealtime.client.webrtc", Scope: "root",
	}
	addition := plugin.ProfileEntry{
		ID: "inspection", Plugin: "openrealtime.client.inspection", Scope: "root",
	}
	composed, err := plugin.Compose(base, 2, plugin.Layer{
		Name: "developer", Rows: []plugin.LayerRow{
			{ID: "transport", Entry: &replacement},
			{ID: "view", Remove: true},
			{ID: "inspection", Entry: &addition},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []plugin.ProfileEntry{replacement, addition}
	if !reflect.DeepEqual(composed.Entries, want) {
		t.Fatalf("composed entries = %#v, want %#v", composed.Entries, want)
	}
	payload, err := plugin.MarshalProfile(composed)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := plugin.ParseProfile(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, composed) {
		t.Fatalf("profile round trip changed value:\n%#v\n%#v", parsed, composed)
	}
	duplicate := strings.Replace(string(payload), "\"name\": \"browser.developer\"",
		"\"name\": \"browser.developer\", \"name\": \"other\"", 1)
	if _, err := plugin.ParseProfile([]byte(duplicate)); err == nil {
		t.Fatal("strict profile parser accepted duplicate key")
	}
}

func register(t *testing.T, catalog *plugin.Catalog, descriptor plugin.Descriptor) {
	t.Helper()
	if _, err := catalog.Register(descriptor); err != nil {
		t.Fatal(err)
	}
}

func freezeProfile(t *testing.T, profile plugin.Profile) plugin.Profile {
	t.Helper()
	result, err := plugin.FreezeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func rootProfile(t *testing.T, entries []plugin.ProfileEntry) plugin.Profile {
	t.Helper()
	return freezeProfile(t, plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.developer", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
	})
}

func scopedProfile(t *testing.T, isolate []string, entries []plugin.ProfileEntry) plugin.Profile {
	t.Helper()
	return freezeProfile(t, plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.developer", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{
			{Path: "root"}, {Path: "root/session", Isolate: isolate},
		},
		Entries: entries,
	})
}
