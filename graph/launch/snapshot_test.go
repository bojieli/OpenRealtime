package launch

import (
	"testing"

	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

func TestSnapshotConfigOwnsArtifactOptionalDependencyAndSecretContainers(t *testing.T) {
	source := Config{
		Artifacts: graphconfig.Artifacts{
			Topology: graphconfig.Artifact{Data: []byte("topology")},
			Values:   graphconfig.Artifact{Data: []byte("values")},
		},
		PlanOptions: graphconfig.Options{OptionalDependencies: []string{"service.one"}},
		Evidence: graphevidence.Document{
			APIVersion: graphevidence.APIVersion, Graph: "snapshot",
			Profiles: []graphevidence.Profile{{Name: "profile.one"}},
		},
		SecretCatalog: &graphsecret.Document{
			APIVersion: graphsecret.APIVersion, Catalog: "snapshot",
			Secrets: map[string]graphsecret.Binding{
				"secret://snapshot/token": {Provider: "test", Locator: "TOKEN"},
			},
		},
	}
	snapshot := snapshotConfig(source)
	source.Artifacts.Topology.Data[0] = 'X'
	source.Artifacts.Values.Data[0] = 'X'
	source.PlanOptions.OptionalDependencies[0] = "service.redirected"
	source.Evidence.Profiles[0].Name = "redirected"
	binding := source.SecretCatalog.Secrets["secret://snapshot/token"]
	binding.Locator = "REDIRECTED"
	source.SecretCatalog.Secrets["secret://snapshot/token"] = binding

	if string(snapshot.Artifacts.Topology.Data) != "topology" ||
		string(snapshot.Artifacts.Values.Data) != "values" ||
		snapshot.PlanOptions.OptionalDependencies[0] != "service.one" ||
		snapshot.Evidence.Profiles[0].Name != "profile.one" ||
		snapshot.SecretCatalog.Secrets["secret://snapshot/token"].Locator != "TOKEN" {
		t.Fatalf("snapshot aliases caller state: %+v", snapshot)
	}
}
