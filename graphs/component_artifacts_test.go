package graphs_test

import (
	"os"
	"path/filepath"
	"testing"

	graphdeployment "github.com/bojieli/OpenRealtime/graph/deployment"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestEveryShippedComponentHasSeparateTruthfulDeploymentSecretAndEvidenceArtifacts(t *testing.T) {
	entries, err := os.ReadDir("components")
	if err != nil {
		t.Fatal(err)
	}
	components := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		components++
		t.Run(entry.Name(), func(t *testing.T) {
			directory := filepath.Join("components", entry.Name())
			topologyBody := mustReadComponentArtifact(t, filepath.Join(directory, "agent.ortg"))
			topology, err := syntax.Parse(filepath.Join(directory, "agent.ortg"), topologyBody)
			if err != nil {
				t.Fatal(err)
			}
			graphID := topology.Graph.Name

			deploymentBody := mustReadComponentArtifact(t, filepath.Join(directory, "agent.deployment.yaml"))
			deployment, err := graphdeployment.ParseYAML("agent.deployment.yaml", deploymentBody)
			if err != nil {
				t.Fatal(err)
			}
			if deployment.Graph != graphID || len(deployment.Nodes) != 0 {
				t.Fatalf("default deployment = %+v, graph = %s", deployment, graphID)
			}

			secretBody := mustReadComponentArtifact(t, filepath.Join(directory, "agent.secrets.yaml"))
			secrets, err := graphsecret.ParseYAML("agent.secrets.yaml", secretBody)
			if err != nil {
				t.Fatal(err)
			}
			if secrets.Catalog != graphID || len(secrets.Secrets) != 0 {
				t.Fatalf("default secret catalog = %+v, graph = %s", secrets, graphID)
			}

			evidenceBody := mustReadComponentArtifact(t, filepath.Join(directory, "agent.evidence.yaml"))
			evidence, err := graphevidence.ParseYAML("agent.evidence.yaml", evidenceBody)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Graph != graphID || evidence.Profiles == nil || len(evidence.Profiles) != 0 {
				t.Fatalf("default evidence manifest = %+v, graph = %s", evidence, graphID)
			}
		})
	}
	if components != 15 {
		t.Fatalf("validated %d shipped component directories, want 15", components)
	}
}

func mustReadComponentArtifact(t testing.TB, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
