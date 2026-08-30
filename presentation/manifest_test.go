package presentation_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/presentation"
)

func TestClientManifestStrictExactRoundTrip(t *testing.T) {
	asset := plugin.Asset{
		Name: "client.js", MediaType: "text/javascript", Digest: digest('a'),
	}
	plan := clientPlan(t, plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.client.shell", Revision: 1,
		Realm: plugin.ClientRealm, Platforms: []string{"browser"}, Assets: []plugin.Asset{asset},
		Permissions: []plugin.Permission{{
			Kind: "network.connect", Resource: "host-realtime", Operations: []string{"websocket"},
		}},
	})
	manifest, err := presentation.FreezeManifest(presentation.ClientManifest{
		FormatVersion: presentation.ManifestFormatVersion, Platform: "browser", Plan: plan,
		Implementations: []presentation.ManifestImplementation{{
			Entry: "shell", Implementation: "browser-shell",
			Artifact:   inspect.ArtifactIdentity{ID: "module://browser-shell", Digest: asset.Digest},
			Entrypoint: "client.js",
		}},
		Assets: []presentation.ManifestAsset{{
			Entry: "shell", Name: asset.Name, MediaType: asset.MediaType, Digest: asset.Digest,
			Path: "/client/v1/modules/" + strings.TrimPrefix(asset.Digest, "sha256:"),
		}},
		Endpoints: []presentation.ManifestEndpoint{{
			Name: "realtime", Method: "GET", Path: "/client/v1/realtime",
		}},
		Grants: []presentation.ManifestGrant{{
			Entry: "shell", Permissions: []plugin.Permission{{
				Kind: "network.connect", Resource: "host-realtime", Operations: []string{"websocket"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := presentation.MarshalManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := presentation.ParseManifest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatalf("manifest round trip changed value:\n%#v\n%#v", parsed, manifest)
	}

	duplicate := strings.Replace(string(payload), "\"platform\": \"browser\"",
		"\"platform\": \"browser\", \"platform\": \"macos\"", 1)
	if _, err := presentation.ParseManifest([]byte(duplicate)); err == nil {
		t.Fatal("manifest parser accepted duplicate field")
	}
	tampered := manifest.Clone()
	tampered.Assets[0].Path = "/client/v1/modules/not-the-digest"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "content address") {
		t.Fatalf("tampered asset path error = %v", err)
	}
	badEndpoint := manifest.Clone()
	badEndpoint.Endpoints[0].Protocol = "openrealtime.test.v1"
	badEndpoint.Endpoints[0].CatalogDigest = "sha256:" + strings.Repeat("A", 64)
	if _, err := presentation.FreezeManifest(badEndpoint); err == nil ||
		!strings.Contains(err.Error(), "catalog digest") {
		t.Fatalf("non-canonical endpoint catalog digest error = %v", err)
	}
	missing := manifest.Clone()
	missing.Implementations = nil
	missing, err = presentation.FreezeManifest(missing)
	if err == nil || !strings.Contains(err.Error(), "no implementation") {
		t.Fatalf("missing implementation error = %v", err)
	}
	excess := manifest.Clone()
	excess.Grants[0].Permissions[0].Operations = []string{"websocket", "process.exec"}
	if _, err := presentation.FreezeManifest(excess); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("excess permission error = %v", err)
	}
}

func clientPlan(t *testing.T, descriptor plugin.Descriptor) plugin.Plan {
	t.Helper()
	catalog := plugin.NewCatalog()
	if _, err := catalog.Register(descriptor); err != nil {
		t.Fatal(err)
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "browser.test", Revision: 1, Realm: plugin.ClientRealm,
		Scopes:  []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{{ID: "shell", Plugin: descriptor.Name, Scope: "root"}},
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

func digest(value byte) string { return "sha256:" + strings.Repeat(string(value), 64) }
