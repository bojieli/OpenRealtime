package runtime_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

func BenchmarkMountClosePlan64(b *testing.B) {
	plan, registry := benchmarkRuntimePlan(b, 64)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
			Plan: plan, Registry: registry,
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := mounted.Close(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLivePlan64(b *testing.B) {
	plan, registry := benchmarkRuntimePlan(b, 64)
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = mounted.Close(context.Background()) })
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		live := mounted.Live()
		if len(live.Entries) != 64 {
			b.Fatal("incomplete live snapshot")
		}
	}
}

type benchmarkFactory struct{ descriptor plugin.Descriptor }

func (factory benchmarkFactory) Descriptor() plugin.Descriptor { return factory.descriptor }

func (factory benchmarkFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	for _, requirement := range factory.descriptor.Requires {
		if _, _, _, _, found := mount.Services.Lookup(requirement.Contract.Name); !found {
			return fmt.Errorf("missing benchmark dependency")
		}
	}
	for _, service := range factory.descriptor.Provides {
		if err := mount.Publisher.Provide(service, struct{}{}); err != nil {
			return err
		}
	}
	return nil
}

func benchmarkRuntimePlan(b *testing.B, count int) (plugin.Plan, *pluginruntime.Registry) {
	b.Helper()
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	entries := make([]plugin.ProfileEntry, 0, count)
	var previous plugin.Contract
	for index := 0; index < count; index++ {
		service := plugin.Contract{
			Name: fmt.Sprintf("benchmark.runtime.service.%03d", index), Revision: 1,
			Digest: fmt.Sprintf("sha256:%064x", index+1),
		}
		descriptor := plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          fmt.Sprintf("openrealtime.benchmark.runtime_%03d", index), Revision: 1,
			Realm: plugin.ClientRealm, Platforms: []string{"portable"},
			Provides: []plugin.Contract{service},
		}
		if index > 0 {
			descriptor.Requires = []plugin.Requirement{{Contract: previous}}
		}
		factory := benchmarkFactory{descriptor: descriptor}
		if _, err := catalog.Register(descriptor); err != nil {
			b.Fatal(err)
		}
		if err := registry.Register("", factory); err != nil {
			b.Fatal(err)
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: fmt.Sprintf("plugin_%03d", index), Plugin: descriptor.Name, Scope: "root",
		})
		previous = service
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "benchmark.runtime", Revision: 1, Realm: plugin.ClientRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
	})
	if err != nil {
		b.Fatal(err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		b.Fatal(err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		b.Fatal(err)
	}
	return plan, registry
}
