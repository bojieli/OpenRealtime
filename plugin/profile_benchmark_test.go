package plugin_test

import (
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
)

func BenchmarkCompileProfile64(b *testing.B) {
	catalog, profile := benchmarkProfile(b, 64)
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := plugin.Compile(profile, lock, catalog); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkProfile(b *testing.B, count int) (*plugin.Catalog, plugin.Profile) {
	b.Helper()
	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, count)
	var previous plugin.Contract
	for index := 0; index < count; index++ {
		service := plugin.Contract{
			Name: fmt.Sprintf("benchmark.service.%03d", index), Revision: 1,
			Digest: fmt.Sprintf("sha256:%064x", index+1),
		}
		descriptor := plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          fmt.Sprintf("openrealtime.benchmark.plugin_%03d", index), Revision: 1,
			Realm: plugin.ClientRealm, Platforms: []string{"portable"},
			Provides: []plugin.Contract{service},
		}
		if index > 0 {
			descriptor.Requires = []plugin.Requirement{{Contract: previous}}
		}
		if _, err := catalog.Register(descriptor); err != nil {
			b.Fatal(err)
		}
		entries = append([]plugin.ProfileEntry{{
			ID: fmt.Sprintf("plugin_%03d", index), Plugin: descriptor.Name, Scope: "root",
		}}, entries...)
		previous = service
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "benchmark.profile", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
	})
	if err != nil {
		b.Fatal(err)
	}
	return catalog, profile
}
