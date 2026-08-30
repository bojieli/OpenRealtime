package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	serverplugin "github.com/bojieli/OpenRealtime/server"
)

func TestServerHTTPPluginsFailCompilationOnMissingExactDependencies(t *testing.T) {
	provider, err := serverplugin.NewSessionProviderFactory(serverTestProvider{})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := serverplugin.NewSessionInspectionPlaneFactory(0)
	if err != nil {
		t.Fatal(err)
	}
	gatewayFactory, err := serverplugin.NewGatewayFactory(serverplugin.GatewayFactoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		factories []pluginruntime.Factory
		want      string
	}{
		{
			name: "gateway inspection plane", factories: []pluginruntime.Factory{
				serverplugin.NewHTTPRouterFactory(), provider, gatewayFactory,
			}, want: serverplugin.SessionInspectionPlaneContract().Name,
		},
		{
			name: "gateway management handler", factories: []pluginruntime.Factory{
				provider, inspection, gatewayFactory,
			}, want: management.HTTPHandlerContract.Name,
		},
		{
			name: "realtime route registry",
			factories: []pluginruntime.Factory{
				serverplugin.NewRealtimeRouteFactory(),
			},
			want: serverplugin.HTTPRoutesContract().Name,
		},
		{
			name: "observability endpoint",
			factories: []pluginruntime.Factory{
				serverplugin.NewHTTPRouterFactory(), serverplugin.NewObservabilityRouteFactory(),
			},
			want: serverplugin.ObservabilityEndpointsContract().Name,
		},
		{
			name: "canonical session authority",
			factories: []pluginruntime.Factory{
				serverplugin.NewHTTPRouterFactory(), managementserver.NewSessionAPIFactory(),
			},
			want: management.AuthorizerContract.Name,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := compileHTTPPluginProfile(test.factories)
			if err == nil || !strings.Contains(err.Error(), "requires unavailable service "+test.want) {
				t.Fatalf("compile error = %v, want unavailable %s", err, test.want)
			}
		})
	}
}

func TestServerHTTPRouteConflictFailsRealmMountAndRollsBack(t *testing.T) {
	factories := completeServerFactories(t)
	factories["conflict"] = newConflictingHealthFactory()
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	artifact := serverArtifact("go://openrealtime/test/conflicting-server", "build-1", "6")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, artifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if mounted != nil || err == nil ||
		!strings.Contains(err.Error(), `HTTP route "GET /healthz" is already owned by observability`) {
		t.Fatalf("conflicting server route mount = realm %v error %v", mounted, err)
	}
}

func TestStableServerRouterSurvivesObservabilityPluginReplacement(t *testing.T) {
	factories := completeServerFactories(t)
	plan := serverPlan(t, factories, true)
	registry := pluginruntime.NewRegistry()
	originalArtifact := serverArtifact("go://openrealtime/test/server-v1", "build-1", "7")
	for _, factory := range factories {
		if err := registry.RegisterArtifact(factory.Descriptor().Name, originalArtifact, factory); err != nil {
			t.Fatal(err)
		}
	}
	replacement := newReplacementObservabilityFactory(
		serverplugin.NewObservabilityRouteFactory().Descriptor(),
	)
	replacementArtifact := serverArtifact("go://openrealtime/test/operations-v2", "build-2", "8")
	if err := registry.RegisterArtifact("test/observability-v2", replacementArtifact, replacement); err != nil {
		t.Fatal(err)
	}
	conflictingReplacement := newConflictingObservabilityReplacement(
		serverplugin.NewObservabilityRouteFactory().Descriptor(),
	)
	if err := registry.RegisterArtifact(
		"test/observability-conflict",
		serverArtifact("go://openrealtime/test/operations-conflict", "build-2", "9"),
		conflictingReplacement,
	); err != nil {
		t.Fatal(err)
	}
	mounted, err := pluginruntime.Mount(context.Background(), pluginruntime.Config{
		Plan: plan, Registry: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := mounted.Close(ctx); err != nil {
			t.Errorf("close replacement test realm: %v", err)
		}
	})
	value, contract, provider, revision, err := mounted.Export(serverplugin.RealtimeHTTPExport)
	if err != nil {
		t.Fatal(err)
	}
	service, ok := value.(serverplugin.RealtimeHTTP)
	if !ok || contract != serverplugin.RealtimeHTTPContract() || provider != "http-router" {
		t.Fatalf("root HTTP export = %T %+v %q", value, contract, provider)
	}
	httpServer := httptest.NewServer(service.Handler())
	defer httpServer.Close()
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	if err := mounted.Replace(
		context.Background(), "observability", "test/observability-conflict",
	); err == nil || !strings.Contains(err.Error(), "previous implementation was restored") ||
		!strings.Contains(err.Error(), `HTTP route "GET /v1/realtime" is already owned by realtime`) {
		t.Fatalf("conflicting observability replacement error = %v", err)
	}
	assertHTTPStatus(t, httpServer.URL+"/healthz", http.StatusOK)
	if live := mounted.Live(); live.Entries["observability"].Implementation !=
		serverplugin.NewObservabilityRouteFactory().Descriptor().Name {
		t.Fatalf("failed replacement was not rolled back: %+v", live.Entries["observability"])
	}

	if err := mounted.Replace(context.Background(), "observability", "test/observability-v2"); err != nil {
		t.Fatal(err)
	}
	assertHTTPStatus(t, httpServer.URL+"/healthz", 218)
	assertHTTPStatus(t, httpServer.URL+"/metrics", 219)
	afterValue, afterContract, afterProvider, afterRevision, err := mounted.Export(
		serverplugin.RealtimeHTTPExport,
	)
	if err != nil {
		t.Fatal(err)
	}
	if afterValue != value || afterContract != contract || afterProvider != provider ||
		afterRevision != revision {
		t.Fatalf("stable router export changed across operations replacement: before=%T/%+v/%s/%d after=%T/%+v/%s/%d",
			value, contract, provider, revision,
			afterValue, afterContract, afterProvider, afterRevision)
	}
	live := mounted.Live()
	if live.Entries["observability"].Implementation != "test/observability-v2" ||
		live.Entries["observability"].Runtime != replacementArtifact ||
		live.Entries["http-router"].Runtime != originalArtifact {
		t.Fatalf("replacement live evidence = %+v", live)
	}
}

func compileHTTPPluginProfile(factories []pluginruntime.Factory) (plugin.Plan, error) {
	catalog := plugin.NewCatalog()
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	for index, factory := range factories {
		if _, err := catalog.Register(factory.Descriptor()); err != nil {
			return plugin.Plan{}, err
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: fmt.Sprintf("entry-%d", index), Plugin: factory.Descriptor().Name, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "openrealtime.server.http-plugin-negative", Revision: 1,
		Realm: plugin.ServerRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries,
	})
	if err != nil {
		return plugin.Plan{}, err
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return plugin.Plan{}, err
	}
	return plugin.Compile(profile, lock, catalog)
}

type conflictingHealthFactory struct{ descriptor plugin.Descriptor }

func newConflictingHealthFactory() *conflictingHealthFactory {
	return &conflictingHealthFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.server.test-conflicting-health", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Requires: []plugin.Requirement{{Contract: serverplugin.HTTPRoutesContract()}},
	}}
}

func (factory *conflictingHealthFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *conflictingHealthFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return httpservice.RegisterRoutes(mount, serverplugin.HTTPRoutesContract(), []httpservice.Route{{
		Pattern: "GET /healthz", Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusTeapot)
		}),
	}})
}

type replacementObservabilityFactory struct{ descriptor plugin.Descriptor }

func newReplacementObservabilityFactory(
	descriptor plugin.Descriptor,
) *replacementObservabilityFactory {
	return &replacementObservabilityFactory{descriptor: descriptor}
}

type conflictingObservabilityReplacement struct{ descriptor plugin.Descriptor }

func newConflictingObservabilityReplacement(
	descriptor plugin.Descriptor,
) *conflictingObservabilityReplacement {
	return &conflictingObservabilityReplacement{descriptor: descriptor}
}

func (factory *conflictingObservabilityReplacement) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *conflictingObservabilityReplacement) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return httpservice.RegisterRoutes(mount, serverplugin.HTTPRoutesContract(), []httpservice.Route{{
		Pattern: "GET /v1/realtime", Handler: http.NotFoundHandler(),
	}})
}

func (factory *replacementObservabilityFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *replacementObservabilityFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if value, contract, _, _, found := mount.Services.Lookup(
		serverplugin.ObservabilityEndpointsContract().Name,
	); !found || contract != serverplugin.ObservabilityEndpointsContract() || value == nil {
		return fmt.Errorf("replacement observability endpoint dependency is unavailable")
	}
	return httpservice.RegisterRoutes(mount, serverplugin.HTTPRoutesContract(), []httpservice.Route{
		{Pattern: "GET /healthz", Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(218)
		})},
		{Pattern: "GET /metrics", Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(219)
		})},
	})
}

func assertHTTPStatus(t *testing.T, endpoint string, want int) {
	t.Helper()
	response, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", endpoint, response.StatusCode, want)
	}
}
