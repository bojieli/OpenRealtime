// Package graphs exposes production constructors for repository-owned graph
// artifacts. Provider, action, and presentation implementations remain
// plugins; this file embeds only the scenario conversation topology, values
// template, descriptor lock, deployment artifact, and evidence manifest.
package graphs

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
)

//go:embed components/scenario-conversation/agent.ortg
//go:embed components/scenario-conversation/agent.values.yaml
//go:embed components/scenario-conversation/openrealtime.lock
//go:embed components/scenario-conversation/agent.deployment.yaml
//go:embed components/scenario-conversation/noise-filter.lock
//go:embed components/scenario-conversation/agent.evidence.yaml
var scenarioConversationArtifacts embed.FS

const scenarioConversationArtifactDirectory = "components/scenario-conversation/"

// ScenarioConversationArtifacts returns independent, locked artifacts with
// only the plugin-selected acoustic and retained-media bounds rewritten into
// the values document. Topology, descriptor resolution, and deployment remain
// byte-exact repository artifacts unless the explicitly selected noise filter
// adds its locked pre-admission node.
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
		"direct_visual_input": directVisual,
		"standing_extraction": config.SemanticAdmission.StandingExtraction,
		"standing_memory":     config.SemanticAdmission.StandingMemory,
		"rules":               config.SemanticAdmission.Rules,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}
	// Every transcript revision reaches the policy. The question it answers is
	// the same for a provisional hypothesis as for a settled utterance, and a
	// policy that only sees finals cannot count while somebody is still
	// talking.
	if err := updateScenarioNode(document.Nodes, "audio_final_gate", map[string]any{
		"admit_provisional": true,
	}); err != nil {
		return graphconfig.Artifacts{}, err
	}

	if config.NoiseFilter != nil {
		// The opt-in topology has no raw-audio path around the filter. Existing
		// profiles retain their exact topology and values.
		if strings.Count(string(topology), "    input audio = admission.audio;") != 1 {
			return graphconfig.Artifacts{}, fmt.Errorf("filtered topology requires one exact raw audio ingress")
		}
		topology = []byte(strings.Replace(string(topology), "    input audio = admission.audio;",
			"    acoustic.NoiseFilter :: noise_filter;\n    noise_filter.filtered -> admission.audio;\n    input audio = noise_filter.audio;", 1))
		document.Nodes["noise_filter"], err = json.Marshal(config.NoiseFilter)
		if err != nil {
			return graphconfig.Artifacts{}, err
		}
		if err := updateScenarioNode(document.Nodes, "overlap_barge_in", map[string]any{"unclassified": "keep_speaking"}); err != nil {
			return graphconfig.Artifacts{}, err
		}
		baseLock, err := resolve.ParseLock(lock)
		if err != nil {
			return graphconfig.Artifacts{}, err
		}
		filterBytes, err := read("noise-filter.lock")
		if err != nil {
			return graphconfig.Artifacts{}, err
		}
		filterLock, err := resolve.ParseLock(filterBytes)
		if err != nil {
			return graphconfig.Artifacts{}, err
		}
		baseLock.Entries = append(baseLock.Entries, filterLock.Entries...)
		lock, err = baseLock.Marshal()
		if err != nil {
			return graphconfig.Artifacts{}, err
		}
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

// ScenarioConversationEvidence returns the separate, non-executable empirical
// claims manifest shipped beside the graph artifacts.
func ScenarioConversationEvidence() (graphevidence.Document, error) {
	payload, err := scenarioConversationArtifacts.ReadFile(
		scenarioConversationArtifactDirectory + "agent.evidence.yaml",
	)
	if err != nil {
		return graphevidence.Document{}, fmt.Errorf(
			"read embedded scenario conversation agent.evidence.yaml: %w", err,
		)
	}
	return graphevidence.ParseYAML("scenario-conversation/agent.evidence.yaml", payload)
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
	evidence, err := ScenarioConversationEvidence()
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
		GraphMetadata: graphcatalog.Metadata{
			Stage:   graphcatalog.Candidate,
			Summary: "Graph-native scenario conversation application.",
			Change:  "Initial exact production graph and configuration revision.",
			Tags:    []string{"audio", "conversation", "multimodal", "scenario"},
		},
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
		Evidence: evidence, Adapter: plugin.Selection(),
	}, nil
}

// ProductionContinuationInstruction is the instruction the conversation
// room's voice model reads, shared by the profile freezer and the live
// benchmarks so a benchmark measures the deployed instruction and not a copy.
const ProductionContinuationInstruction = "Ground every response in canonical evidence already received. " +
	"Never invent, predict, quote, or role-play a future user or other-speaker turn, timing annotation, or stage direction. " +
	"Be concise unless the user explicitly requested detail. " +
	"Name the concrete matched item, for example \"the sea bass,\" instead of saying only \"that is the one.\" " +
	"Put the decisive requested fact in the first clause; for a correction, start with the corrected fact, for example \"The deadline is the third, not the thirteenth.\" " +
	"Translate into the requested target language, not the source language. " +
	"When a deferred event or time condition is due, execute the requested action now; do not merely acknowledge, confirm, restate, or narrate its setup. " +
	"Never emit punctuation-only output; always produce at least one complete lexical sentence when speech is authorized. " +
	"For an event-driven running count, emit exactly one updated count for each new occurrence: one number, once, with no repeated sentence or extra words, continuing from counts that were already audible. The count continues from the last count you spoke even when you answered something else in between; an answer to a question is not a count and does not restart it. " +
	"Speak counts as number words in the requested language with sentence punctuation, for example \"One.\" then \"Two.\" in English, rather than bare digit strings. " +
	"For a direct request to recite a finite numeric range, supply the complete remaining sequence in this response, one number per sentence, without waiting for another user turn. \"Slowly\" and \"one number at a time\" specify spoken pacing, not one number per response. The speech player paces and interrupts the stream; generate all remaining numbers through the requested endpoint now. This rule never authorizes counting events that have not occurred. For an event-driven count, a repeated occurrence or no new occurrence requires exactly <wait> and nothing else; never say zero, acknowledge the rule, or narrate waiting. " +
	"When resuming that sequence, begin after the last number the user actually heard; do not skip numbers that were prepared but not audible."
