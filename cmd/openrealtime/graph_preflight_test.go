package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func TestGraphPreflightSealsBuiltInCatalogWithoutMountOrLegacyServe(t *testing.T) {
	directory := t.TempDir()
	topologyPath := filepath.Join(directory, "agent.ortg")
	valuesPath := filepath.Join(directory, "agent.values.yaml")
	deploymentPath := filepath.Join(directory, "agent.deployment.yaml")
	lockPath := filepath.Join(directory, "openrealtime.lock")
	mustWriteGraphPreflightFile(t, topologyPath, `graph native_preflight {
    video.FrameIngress :: ingress;

    input frame = ingress.frame_in;
    input reference = ingress.reference_in;
    output frames = ingress.frames;
    output references = ingress.references;
}
`)
	mustWriteGraphPreflightFile(t, valuesPath, `apiVersion: openrealtime.ai/config/v1alpha1
graph: native_preflight
nodes: {}
`)
	mustWriteGraphPreflightFile(t, deploymentPath, `apiVersion: openrealtime.ai/deployment/v1alpha1
graph: native_preflight
nodes: {}
`)
	var stdout, stderr bytes.Buffer
	if err := runGraph([]string{"update", "-lock", lockPath, topologyPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runGraph([]string{
		"preflight", "-lock", lockPath, "-values", valuesPath,
		"-deployment", deploymentPath, topologyPath,
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var public graphruntime.PublicPreparation
	if err := json.Unmarshal(stdout.Bytes(), &public); err != nil {
		t.Fatalf("decode public preflight %q: %v", stdout.String(), err)
	}
	if public.Plan.GraphID != "native_preflight" || len(public.Nodes) != 1 ||
		public.Nodes[0].Reference != "video.FrameIngress" ||
		public.Nodes[0].Transport != "in-process" || len(public.Dependencies) != 0 {
		t.Fatalf("public graph preflight = %+v", public)
	}
	for _, forbidden := range []string{"secret://", "locator", "serve", "binding"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("public graph preflight contains %q: %s", forbidden, stdout.String())
		}
	}
}

func TestGraphPreflightResolvesStandardSchemaAndRunsExactFactoryValidator(t *testing.T) {
	directory := t.TempDir()
	topologyPath := filepath.Join(directory, "agent.ortg")
	valuesPath := filepath.Join(directory, "agent.values.yaml")
	deploymentPath := filepath.Join(directory, "agent.deployment.yaml")
	lockPath := filepath.Join(directory, "openrealtime.lock")
	mustWriteGraphPreflightFile(t, topologyPath, `graph schema_preflight {
    video.AdaptiveObservation :: policy;

    input source = policy.source;
    input frames = policy.frames;
    input references = policy.references;
    input tick = policy.tick;
    input refresh = policy.refresh;
    input end = policy.end;
    input cancel = policy.cancel;
    output observe = policy.observe;
    output observe_reference = policy.observe_reference;
    output observer_refresh = policy.observer_refresh;
    output observer_close = policy.observer_close;
    output observer_cancel = policy.observer_cancel;
    output state = policy.state;
    output decision = policy.decision;
    output outcome = policy.outcome;
}
`)
	mustWriteGraphPreflightFile(t, valuesPath, `apiVersion: openrealtime.ai/config/v1alpha1
graph: schema_preflight
nodes:
  policy:
    source: screen
`)
	mustWriteGraphPreflightFile(t, deploymentPath, `apiVersion: openrealtime.ai/deployment/v1alpha1
graph: schema_preflight
nodes: {}
`)
	var stdout, stderr bytes.Buffer
	if err := runGraph([]string{"update", "-lock", lockPath, topologyPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runGraph([]string{
		"preflight", "-lock", lockPath, "-values", valuesPath,
		"-deployment", deploymentPath, topologyPath,
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var public graphruntime.PublicPreparation
	if err := json.Unmarshal(stdout.Bytes(), &public); err != nil {
		t.Fatalf("decode public preflight %q: %v", stdout.String(), err)
	}
	if public.Plan.ValuesSchemaDigest == "" || public.Plan.PlanFingerprint == "" ||
		len(public.Nodes) != 1 || public.Nodes[0].Reference != "video.AdaptiveObservation" ||
		len(public.Dependencies) != 1 || public.Dependencies[0].Name != graphruntime.SequenceServiceName {
		t.Fatalf("schema-backed public preflight = %+v", public)
	}

	// The individual integer bounds are valid Draft 2020-12 structure, while
	// max_interval_ms >= min_interval_ms is an exact factory semantic.
	mustWriteGraphPreflightFile(t, valuesPath, `apiVersion: openrealtime.ai/config/v1alpha1
graph: schema_preflight
nodes:
  policy:
    source: screen
    min_interval_ms: 20
    max_interval_ms: 10
`)
	stdout.Reset()
	err := runGraph([]string{
		"preflight", "-lock", lockPath, "-values", valuesPath,
		"-deployment", deploymentPath, topologyPath,
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "max_interval_ms must be between min_interval_ms") {
		t.Fatalf("factory semantic validation error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("refused preflight emitted public preparation: %s", stdout.String())
	}
}

func TestGraphPreflightSealsShippedStandardOnlyComponents(t *testing.T) {
	repository := filepath.Join("..", "..")
	for _, component := range []string{
		"acoustic-endpoint",
		"generation-policy",
		"multimodal-content",
	} {
		t.Run(component, func(t *testing.T) {
			directory := filepath.Join(repository, "graphs", "components", component)
			var stdout, stderr bytes.Buffer
			err := runGraph([]string{
				"preflight",
				"-lock", filepath.Join(directory, "openrealtime.lock"),
				"-values", filepath.Join(directory, "agent.values.yaml"),
				"-deployment", filepath.Join(directory, "agent.deployment.yaml"),
				"-secrets", filepath.Join(directory, "agent.secrets.yaml"),
				filepath.Join(directory, "agent.ortg"),
			}, &stdout, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			var public graphruntime.PublicPreparation
			if err := json.Unmarshal(stdout.Bytes(), &public); err != nil {
				t.Fatalf("decode public preflight: %v\n%s", err, stdout.String())
			}
			if public.Plan.ValuesSchemaDigest == "" || public.Plan.PlanFingerprint == "" ||
				len(public.Nodes) == 0 {
				t.Fatalf("shipped public preparation = %+v", public)
			}
		})
	}
}

func TestGraphPreflightRejectsImplementationOutsideExplicitExecutableCatalog(t *testing.T) {
	directory := t.TempDir()
	topologyPath := filepath.Join(directory, "agent.ortg")
	valuesPath := filepath.Join(directory, "agent.values.yaml")
	deploymentPath := filepath.Join(directory, "agent.deployment.yaml")
	lockPath := filepath.Join(directory, "openrealtime.lock")
	mustWriteGraphPreflightFile(t, topologyPath, `graph native_preflight_missing {
    video.FrameIngress :: ingress;
    input frame = ingress.frame_in;
    input reference = ingress.reference_in;
    output frames = ingress.frames;
    output references = ingress.references;
}
`)
	mustWriteGraphPreflightFile(t, valuesPath, `apiVersion: openrealtime.ai/config/v1alpha1
graph: native_preflight_missing
nodes: {}
`)
	mustWriteGraphPreflightFile(t, deploymentPath, `apiVersion: openrealtime.ai/deployment/v1alpha1
graph: native_preflight_missing
nodes:
  ingress:
    implementation: plugin/missing
`)
	var stdout, stderr bytes.Buffer
	if err := runGraph([]string{"update", "-lock", lockPath, topologyPath}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err := runGraph([]string{
		"preflight", "-lock", lockPath, "-values", valuesPath,
		"-deployment", deploymentPath, topologyPath,
	}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `implementation "plugin/missing" is absent from discovery`) {
		t.Fatalf("missing executable plugin error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed preflight emitted output: %s", stdout.String())
	}
}

func TestGraphInventoryReportsExactUnimplementedPluginSurfaces(t *testing.T) {
	var stdout bytes.Buffer
	if err := runGraph([]string{"inventory"}, &stdout, &stdout); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`"implementations"`, `"runtime.clock"`, `"action.tool.registries"`,
		`"cognition.media.resolver"`, `"config_schemas"`,
		`"schema://openrealtime/cognition/text-model-config/v1"`,
	} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("graph inventory is missing %s: %s", required, stdout.String())
		}
	}
}

func mustWriteGraphPreflightFile(t testing.TB, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
