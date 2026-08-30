package plugin_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
)

func TestDescriptorIdentityIsCanonicalAndRecursivelyIndependent(t *testing.T) {
	contractA := service("client.connection", 'a')
	contractB := service("client.inspect", 'b')
	left := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.shell", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"macos", "browser"},
		Provides: []plugin.Contract{contractB, contractA},
		Events: []plugin.Event{
			{Contract: service("session.event", 'c'), Direction: plugin.Consumes},
			{Contract: service("client.ready", 'd'), Direction: plugin.Publishes},
		},
		Permissions: []plugin.Permission{
			{Kind: "media.capture", Resource: "microphone", Operations: []string{"stop", "start"}},
			{Kind: "inspection.read", Resource: "session", Operations: []string{"snapshot"}},
		},
		Assets: []plugin.Asset{
			{Name: "shell.js", MediaType: "text/javascript", Digest: sha('e')},
			{Name: "shell.css", MediaType: "text/css", Digest: sha('f')},
		},
	}
	right := left.Clone()
	right.Platforms[0], right.Platforms[1] = right.Platforms[1], right.Platforms[0]
	right.Provides[0], right.Provides[1] = right.Provides[1], right.Provides[0]
	right.Events[0], right.Events[1] = right.Events[1], right.Events[0]
	right.Permissions[0], right.Permissions[1] = right.Permissions[1], right.Permissions[0]
	right.Permissions[1].Operations[0], right.Permissions[1].Operations[1] =
		right.Permissions[1].Operations[1], right.Permissions[1].Operations[0]
	right.Assets[0], right.Assets[1] = right.Assets[1], right.Assets[0]

	leftIdentity, err := left.Identity()
	if err != nil {
		t.Fatal(err)
	}
	rightIdentity, err := right.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if leftIdentity != rightIdentity {
		t.Fatalf("declaration order changed identity: %#v != %#v", leftIdentity, rightIdentity)
	}

	clone := left.Clone()
	clone.Platforms[0] = "changed"
	clone.Provides[0].Name = "changed"
	clone.Permissions[0].Operations[0] = "changed"
	clone.Assets[0].Name = "changed.js"
	again, err := left.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if again != leftIdentity {
		t.Fatal("descriptor clone retained caller-owned slices")
	}
}

func TestDescriptorRejectsInvalidTrustAndLifecycleContracts(t *testing.T) {
	state := service("client.state.schema", 'd')
	base := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.reducer", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"portable"},
		Provides: []plugin.Contract{service("client.reducer", 'a')},
	}
	tests := []struct {
		name string
		edit func(*plugin.Descriptor)
		want string
	}{
		{name: "realm", edit: func(value *plugin.Descriptor) { value.Realm = "ui" }, want: "invalid realm"},
		{name: "platform", edit: func(value *plugin.Descriptor) { value.Platforms = nil }, want: "no platforms"},
		{name: "duplicate service", edit: func(value *plugin.Descriptor) {
			value.Provides = append(value.Provides, value.Provides[0])
		}, want: "more than once"},
		{name: "self dependency", edit: func(value *plugin.Descriptor) {
			value.Requires = []plugin.Requirement{{Contract: value.Provides[0]}}
		}, want: "both provides and requires"},
		{name: "state transition", edit: func(value *plugin.Descriptor) {
			value.Lifecycle.Snapshot = true
		}, want: "without a state schema"},
		{name: "restore", edit: func(value *plugin.Descriptor) {
			value.StateSchema = &state
			value.Lifecycle.Restore = true
		}, want: "restore without snapshot"},
		{name: "asset traversal", edit: func(value *plugin.Descriptor) {
			value.Assets = []plugin.Asset{{Name: "../shell.js", MediaType: "text/javascript", Digest: sha('e')}}
		}, want: "invalid asset name"},
		{name: "permission operation", edit: func(value *plugin.Descriptor) {
			value.Permissions = []plugin.Permission{{Kind: "media.capture", Resource: "camera"}}
		}, want: "no operations"},
		{name: "undeclared authority", edit: func(value *plugin.Descriptor) {
			value.Permissions = []plugin.Permission{{
				Kind: "effect.browser", Resource: "target/browser-1",
				Operations: []string{"click"}, Authority: "authority.browser",
			}}
		}, want: "undeclared authority service"},
		{name: "optional authority", edit: func(value *plugin.Descriptor) {
			authority := service("authority.browser", 'e')
			value.Requires = []plugin.Requirement{{Contract: authority, Optional: true}}
			value.Permissions = []plugin.Permission{{
				Kind: "effect.browser", Resource: "target/browser-1",
				Operations: []string{"click"}, Authority: authority.Name,
			}}
		}, want: "optional authority service"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base.Clone()
			test.edit(&value)
			if err := value.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestCatalogPinsImmutableDescriptorRevisions(t *testing.T) {
	catalog := plugin.NewCatalog()
	descriptor := providerDescriptor("openrealtime.host.transport", plugin.PresentationHostRealm,
		service("host.transport", 'a'))
	identity, err := catalog.Register(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Register(descriptor); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate registration error = %v", err)
	}
	changed := descriptor.Clone()
	changed.Platforms = []string{"browser"}
	if _, err := catalog.Register(changed); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mutated revision error = %v", err)
	}
	resolved, err := catalog.Resolve(identity)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Platforms[0] = "changed"
	again, err := catalog.Resolve(identity)
	if err != nil || again.Platforms[0] != "go" {
		t.Fatalf("catalog returned aliased descriptor: %#v, %v", again.Platforms, err)
	}
	wrong := identity
	wrong.Digest = sha('f')
	if _, err := catalog.Resolve(wrong); err == nil || !strings.Contains(err.Error(), "lock requires") {
		t.Fatalf("wrong digest error = %v", err)
	}
}

func providerDescriptor(name string, realm plugin.Realm, contract plugin.Contract) plugin.Descriptor {
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          name, Revision: 1, Realm: realm, Platforms: []string{"go"},
		Provides: []plugin.Contract{contract},
	}
}

func service(name string, value byte) plugin.Contract {
	return plugin.Contract{Name: name, Revision: 1, Digest: sha(value)}
}

func sha(value byte) string { return "sha256:" + strings.Repeat(string(value), 64) }
