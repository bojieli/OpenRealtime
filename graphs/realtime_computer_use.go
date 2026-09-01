// Package graphs exposes production constructors for repository-owned graph
// artifacts. Provider and presentation implementations remain plugins; this
// package embeds topology, values templates, locks, deployment data, and the
// separate non-executable evidence manifest.
package graphs

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	realtimecubinding "github.com/bojieli/OpenRealtime/graph/binding/realtimecu"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

//go:embed components/realtime-computer-use/agent.ortg
//go:embed components/realtime-computer-use/agent.values.yaml
//go:embed components/realtime-computer-use/openrealtime.lock
//go:embed components/realtime-computer-use/agent.deployment.yaml
//go:embed components/realtime-computer-use/agent.evidence.yaml
var realtimeComputerUseArtifacts embed.FS

const realtimeComputerUseArtifactDirectory = "components/realtime-computer-use/"

// RealtimeComputerUseArtifacts returns independent, locked artifacts whose
// activation schema is narrowed to the supplied browser target. This is the
// only dynamic authoring step: topology, descriptor lock, and deployment stay
// byte-exact repository artifacts, while target dimensions necessarily come
// from the selected client surface.
func RealtimeComputerUseArtifacts(target computeruse.Target) (graphconfig.Artifacts, error) {
	if err := target.Validate(); err != nil {
		return graphconfig.Artifacts{}, fmt.Errorf("create realtime-CU artifacts target: %w", err)
	}
	if target.Name != strings.TrimSpace(target.Name) || strings.ContainsAny(target.Name, "\x00\r\n") {
		return graphconfig.Artifacts{}, fmt.Errorf("create realtime-CU artifacts target name %q is not canonical", target.Name)
	}
	if !slices.Equal(target.Sources, []string{realtimecubinding.SourceScreen}) {
		return graphconfig.Artifacts{}, fmt.Errorf("create realtime-CU artifacts target sources = %v, want only screen", target.Sources)
	}
	read := func(name string) ([]byte, error) {
		payload, err := realtimeComputerUseArtifacts.ReadFile(realtimeComputerUseArtifactDirectory + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded realtime-CU %s: %w", name, err)
		}
		return slices.Clone(payload), nil
	}
	topology, err := read("agent.ortg")
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	lock, err := read("openrealtime.lock")
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	deployment, err := read("agent.deployment.yaml")
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	valuesTemplate, err := read("agent.values.yaml")
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	document, err := graphvalues.ParseYAML("agent.values.yaml", valuesTemplate)
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	var activation policyelements.GenerateOnObservationConfig
	if err := json.Unmarshal(document.Nodes["activation"], &activation); err != nil {
		return graphconfig.Artifacts{}, fmt.Errorf("decode realtime-CU activation values: %w", err)
	}
	definitions, err := computeruse.DefinitionsFor(target)
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	activation.Invocation.Tools = make([]continuation.ToolDefinition, len(definitions))
	for index, definition := range definitions {
		activation.Invocation.Tools[index] = continuation.ToolDefinition{
			Name: definition.Name, Description: definition.Description,
			Parameters: slices.Clone(definition.Parameters),
		}
	}
	document.Nodes["activation"], err = json.Marshal(activation)
	if err != nil {
		return graphconfig.Artifacts{}, fmt.Errorf("encode realtime-CU activation values: %w", err)
	}
	values, err := json.Marshal(document)
	if err != nil {
		return graphconfig.Artifacts{}, fmt.Errorf("encode realtime-CU values artifact: %w", err)
	}
	return graphconfig.Artifacts{
		Topology:   graphconfig.Artifact{Path: "realtime-computer-use/agent.ortg", Encoding: graphconfig.ORTG, Data: topology},
		Values:     graphconfig.Artifact{Path: "realtime-computer-use/agent.values.json", Encoding: graphconfig.JSON, Data: values},
		Lock:       graphconfig.Artifact{Path: "realtime-computer-use/openrealtime.lock", Encoding: graphconfig.JSON, Data: lock},
		Deployment: graphconfig.Artifact{Path: "realtime-computer-use/agent.deployment.yaml", Encoding: graphconfig.YAML, Data: deployment},
	}, nil
}

// RealtimeComputerUseEvidence returns the separate empirical claims manifest
// shipped with the repository-owned graph.
func RealtimeComputerUseEvidence() (graphevidence.Document, error) {
	payload, err := realtimeComputerUseArtifacts.ReadFile(
		realtimeComputerUseArtifactDirectory + "agent.evidence.yaml",
	)
	if err != nil {
		return graphevidence.Document{}, fmt.Errorf(
			"read embedded realtime-CU agent.evidence.yaml: %w", err,
		)
	}
	return graphevidence.ParseYAML("realtime-computer-use/agent.evidence.yaml", payload)
}

// RealtimeComputerUseApplicationRegistration connects the strict,
// serializable Realtime-CU application profile to these repository-owned
// immutable graph artifacts. Host factories remain exact registrations and
// are not invoked by profile resolution.
func RealtimeComputerUseApplicationRegistration(
	config realtimecubinding.ApplicationRegistrationConfig,
) (launchprofile.Registration, error) {
	return realtimecubinding.NewApplicationRegistration(
		config, RealtimeComputerUseLaunchConfig,
	)
}

// RealtimeComputerUseLaunchConfig composes the repository's standard element
// library with one exact Realtime-CU provider/observer/target plugin. The
// returned value is resource-free and can be passed directly to graph/launch;
// model and observer factories remain unopened until SessionProvider.Start.
func RealtimeComputerUseLaunchConfig(
	config realtimecubinding.PluginConfig,
) (graphlaunch.Config, error) {
	plugin, err := realtimecubinding.NewPlugin(config)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	artifacts, err := RealtimeComputerUseArtifacts(config.Target)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	evidence, err := RealtimeComputerUseEvidence()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	descriptors, err := elements.Catalog()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	if err := realtimecubinding.RegisterElementDescriptors(descriptors); err != nil {
		return graphlaunch.Config{}, err
	}
	schemas, err := elements.StandardConfigSchemaCatalog()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	assembly, err := elements.AssemblyCatalog()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	realtimeCUImplementations, err := realtimecubinding.ElementFactoryRegistrations()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	assembly.Implementations = append(
		append([]graphruntime.FactoryRegistration(nil), assembly.Implementations...),
		realtimeCUImplementations...,
	)
	assembly.Dependencies = append(
		append([]graphassembly.Dependency(nil), assembly.Dependencies...),
		plugin.AssemblyDependencies()...,
	)
	return graphlaunch.Config{
		Artifacts: artifacts,
		GraphMetadata: graphcatalog.Metadata{
			Stage:   graphcatalog.Candidate,
			Summary: "Graph-native silent Realtime computer-use application.",
			Change:  "Initial exact production graph and configuration revision.",
			Tags:    []string{"computer-use", "realtime", "silent", "visual"},
		},
		PlanOptions: graphconfig.Options{
			Catalog: descriptors, SchemaResolver: schemas,
			OptionalDependencies: []string{
				cognitionelements.MediaResolverService,
			},
		},
		Catalog: graphlaunch.Catalog{
			Assembly: assembly, Adapters: []graphlaunch.AdapterPlugin{plugin.AdapterPlugin()},
			MountDependencies: plugin.MountDependencies(),
		},
		Evidence: evidence, Adapter: plugin.Selection(),
	}, nil
}
