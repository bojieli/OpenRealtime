package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/plugin"
	serverplugin "github.com/bojieli/OpenRealtime/server"
)

func TestServerBundleMountsOnlyExactRuntimeArtifactsBeforeServing(t *testing.T) {
	providerArtifact := serverArtifact("go://openrealtime/test/server-process", "build-provider", "c")
	gatewayArtifact := serverArtifact("go://openrealtime/test/server-process", "build-gateway", "d")
	bundle, err := serverplugin.NewBundle(serverplugin.BundleConfig{
		ProfileName: "openrealtime.server.test-bundle", ProfileRevision: 7,
		Provider:         serverTestProvider{},
		ProviderArtifact: providerArtifact, GatewayArtifact: gatewayArtifact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Profile.Revision != 7 || bundle.Profile.Fingerprint == "" ||
		bundle.Lock.Fingerprint == "" || bundle.Plan.Fingerprint == "" ||
		bundle.Plan.Realm != plugin.ServerRealm {
		t.Fatalf("compiled server bundle = %+v %+v %+v", bundle.Profile, bundle.Lock, bundle.Plan)
	}
	realm, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := realm.Close(ctx); err != nil {
			t.Errorf("close server bundle: %v", err)
		}
	})

	live := realm.Live()
	if live.Fingerprint != bundle.Plan.Fingerprint || live.Realm != plugin.ServerRealm ||
		live.Entries["sessions"].Runtime != providerArtifact ||
		live.Entries["gateway"].Runtime != gatewayArtifact ||
		live.Entries["http-router"].Runtime != gatewayArtifact ||
		live.Entries["inspection"].Runtime != gatewayArtifact ||
		live.Entries["realtime"].Runtime != gatewayArtifact ||
		live.Entries["observability"].Runtime != gatewayArtifact ||
		live.Entries["session-api"].Runtime != gatewayArtifact ||
		len(live.Entries) != 7 ||
		!live.Exports[serverplugin.RealtimeHTTPExport].Available {
		t.Fatalf("mounted server bundle evidence = %+v", live)
	}
	httpServer := httptest.NewServer(realm.Handler())
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health map[string]any
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || health["binding"] != "server-profile-test" {
		t.Fatalf("bundle health = status %d payload %+v", response.StatusCode, health)
	}
	profile, ok := health["server_profile"].(map[string]any)
	if !ok || profile["fingerprint"] != bundle.Plan.Fingerprint || profile["realm"] != "server" {
		t.Fatalf("bundle health omitted mounted server evidence: %+v", health)
	}
	if _, err := bundle.Mount(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "already mounted") {
		t.Fatalf("second server realm mount error = %v", err)
	}
}

func TestServerBundleFailsClosedOnMissingOrMutableIdentity(t *testing.T) {
	valid := serverArtifact("go://openrealtime/test/server-process", "build-1", "e")
	tests := []struct {
		name   string
		mutate func(*serverplugin.BundleConfig)
		want   string
	}{
		{name: "profile", mutate: func(config *serverplugin.BundleConfig) {
			config.ProfileName = ""
		}, want: "profile name"},
		{name: "revision", mutate: func(config *serverplugin.BundleConfig) {
			config.ProfileRevision = 0
		}, want: "positive profile revision"},
		{name: "provider artifact", mutate: func(config *serverplugin.BundleConfig) {
			config.ProviderArtifact = serverArtifact("go://openrealtime/test/server-process", "latest", "e")
		}, want: "mutable or placeholder"},
		{name: "gateway artifact", mutate: func(config *serverplugin.BundleConfig) {
			config.GatewayArtifact = serverArtifact("go://openrealtime/test/server-process", "build-1", "z")
		}, want: "invalid SHA-256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := serverplugin.BundleConfig{
				ProfileName: "openrealtime.server.valid", ProfileRevision: 1,
				Provider: serverTestProvider{}, ProviderArtifact: valid, GatewayArtifact: valid,
			}
			test.mutate(&config)
			if _, err := serverplugin.NewBundle(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("server bundle error = %v, want %q", err, test.want)
			}
		})
	}
}
