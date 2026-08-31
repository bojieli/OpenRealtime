// Package graphs exposes production constructors for repository-owned graph
// artifacts. Provider, action, and presentation implementations remain
// plugins; this file embeds only the scenario conversation topology, values
// template, descriptor lock, and deployment artifact.
package graphs

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

//go:embed components/scenario-conversation/agent.ortg
//go:embed components/scenario-conversation/agent.values.yaml
//go:embed components/scenario-conversation/openrealtime.lock
//go:embed components/scenario-conversation/agent.deployment.yaml
var scenarioConversationArtifacts embed.FS

const scenarioConversationArtifactDirectory = "components/scenario-conversation/"

// ScenarioConversationArtifacts returns independent, locked artifacts with
// only the plugin-selected acoustic and retained-media bounds rewritten into
// the values document. Topology, descriptor resolution, and deployment remain
// byte-exact repository artifacts.
func ScenarioConversationArtifacts(
	source scenarioconversation.PluginConfig,
) (graphconfig.Artifacts, error) {
	config, err := scenarioconversation.NormalizePluginConfig(source)
	if err != nil {
		return graphconfig.Artifacts{}, err
	}
	read := func(name string) ([]byte, error) {
		payload, err := scenarioConversationArtifacts.ReadFile(scenarioConversationArtifactDirectory + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded scenario conversation %s: %w", name, err)
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
	if err := updateScenarioNode(document.Nodes, "admission", map[string]any{
		"threshold": config.Gate.Threshold, "prefix_padding_ms": config.Gate.PrefixPaddingMS,
		"silence_duration_ms": config.Gate.SilenceDurationMS,
		"speech_duration_ms":  config.Gate.SpeechDurationMS,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	if err := updateScenarioNode(document.Nodes, "content", map[string]any{
		"max_input_bytes": config.Media.MaxItemBytes,
		"max_pending":     config.Media.MaxPending, "max_pending_bytes": config.Media.MaxBytes,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	if err := updateScenarioNode(document.Nodes, "retention", map[string]any{
		"max_items": config.Media.MaxItems, "max_bytes": config.Media.MaxBytes,
		"max_item_bytes":    config.Media.MaxItemBytes,
		"max_active_leases": config.Media.MaxActiveLeases,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	if err := updateScenarioNode(document.Nodes, "media_resolver", map[string]any{
		"max_pending": config.Media.MaxPending, "max_bytes": config.Media.MaxBytes,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	directVisual := false
	if evidence := config.Architecture.Interaction.EvidenceCapabilities; evidence != nil {
		directVisual = evidence.DirectVisualInput
	}
	if err := updateScenarioNode(document.Nodes, "semantic_admission", map[string]any{
		"direct_visual_input":           directVisual,
		"standing_extraction":           config.SemanticAdmission.StandingExtraction,
		"verify_voice_activation":       config.SemanticAdmission.VerifyVoiceActivation,
		"verify_silent_action":          config.SemanticAdmission.VerifySilentAction,
		"minimum_activation_confidence": config.SemanticAdmission.MinimumActivationConfidence,
		"standing_memory":               config.SemanticAdmission.StandingMemory,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	values, err := json.Marshal(document)
	if err != nil {
		return graphconfig.Artifacts{}, fmt.Errorf("encode scenario conversation values artifact: %w", err)
	}
	return graphconfig.Artifacts{
		Topology:   graphconfig.Artifact{Path: "scenario-conversation/agent.ortg", Encoding: graphconfig.ORTG, Data: topology},
		Values:     graphconfig.Artifact{Path: "scenario-conversation/agent.values.json", Encoding: graphconfig.JSON, Data: values},
		Lock:       graphconfig.Artifact{Path: "scenario-conversation/openrealtime.lock", Encoding: graphconfig.JSON, Data: lock},
		Deployment: graphconfig.Artifact{Path: "scenario-conversation/agent.deployment.yaml", Encoding: graphconfig.YAML, Data: deployment},
	}, nil
}

func updateScenarioNode(
	nodes map[string]json.RawMessage, name string, selected map[string]any,
) error {
	var values map[string]any
	if err := json.Unmarshal(nodes[name], &values); err != nil || values == nil {
		if err == nil {
			err = fmt.Errorf("node values are not an object")
		}
		return fmt.Errorf("decode scenario conversation %s values: %w", name, err)
	}
	for field, value := range selected {
		if _, found := values[field]; !found {
			return fmt.Errorf("scenario conversation %s values have no selected field %q", name, field)
		}
		values[field] = value
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode scenario conversation %s values: %w", name, err)
	}
	nodes[name] = payload
	return nil
}

// ScenarioConversationApplicationRegistration connects strict serializable
// application profiles to this repository-owned artifact constructor.
func ScenarioConversationApplicationRegistration(
	config scenarioconversation.ApplicationRegistrationConfig,
) (launchprofile.Registration, error) {
	return scenarioconversation.NewApplicationRegistration(
		config, ScenarioConversationLaunchConfig,
	)
}

// ScenarioConversationLaunchConfig composes the standard fine-grained element
// library with one exact plugin selection. It is resource-free: provider
// factories, credentials, media stores, ledgers, and client sinks are acquired
// only by SessionProvider.Start.
func ScenarioConversationLaunchConfig(
	source scenarioconversation.PluginConfig,
) (graphlaunch.Config, error) {
	config, err := scenarioconversation.NormalizePluginConfig(source)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	plugin, err := scenarioconversation.NewPlugin(config)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	artifacts, err := ScenarioConversationArtifacts(config)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	descriptors, err := elements.Catalog()
	if err != nil {
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
	assembly.Dependencies = append(
		append([]graphassembly.Dependency(nil), assembly.Dependencies...),
		plugin.AssemblyDependencies()...,
	)
	return graphlaunch.Config{
		Artifacts: artifacts,
		PlanOptions: graphconfig.Options{
			Catalog: descriptors, SchemaResolver: schemas, Revision: 1,
			OptionalDependencies: []string{
				cognitionelements.MediaResolverService,
				stateelements.TrajectoryStoreService,
			},
		},
		Catalog: graphlaunch.Catalog{
			Assembly: assembly, Adapters: []graphlaunch.AdapterPlugin{plugin.AdapterPlugin()},
			MountDependencies: plugin.MountDependencies(),
		},
		Adapter: plugin.Selection(),
	}, nil
}
