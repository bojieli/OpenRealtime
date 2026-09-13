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
	// The interaction policy owns the floor: it is asked on every partial
	// whether the person is cutting in, and yields when it says so. The
	// overlap element's own fallback - yield once overlap goes unclassified
	// for 800 ms - took the floor from a translation the policy had chosen
	// to keep, because the next partial, and the next decision, were a
	// second away. Unclassified overlap keeps speaking; a stop is a decision.
	//
	// The element's own classifier is switched off for the same reason. It
	// asked the decider a second, older question ("is this directed speech?")
	// beside the policy's and acted on the answer alone: in the live room it
	// cancelled a key press the policy had just chosen because the recorded
	// menu read on, cancelled a translation rule's acknowledgement because
	// the person kept talking, and cut a count the policy had decided to
	// keep, 15 ms before the policy said keep. One question, one decider.
	if err := updateScenarioNode(document.Nodes, "overlap_barge_in", scenarioOverlapValues); err != nil {
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
		if err := updateScenarioNode(document.Nodes, "overlap_barge_in", scenarioOverlapValues); err != nil {
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

// scenarioOverlapValues leaves the overlap element with no classifier and a
// fallback that keeps speaking: the floor is the interaction policy's alone.
var scenarioOverlapValues = map[string]any{"decider": "", "unclassified": "keep_speaking"}

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
	"Translate into the requested target language, not the source language; in a running translation translate only the words not yet translated, never restate one already spoken, and when the sentence ends say in a short phrase anything the earlier pieces left out, such as a verb the source puts last. " +
	"While the person is still mid-sentence, act at once whenever the words so far already hold what a standing instruction watches for - press the key for the option just named, correct the wrong date, count the animal, translate the words so far; when they hold nothing due (a rule still being set up, a story) reply with exactly <wait>, and never acknowledge, restate, or answer a sentence they have not finished. " +
	"When a deferred event or time condition is due, do the requested action now instead of acknowledging or restating its setup; while the person is only setting such a rule up (\"tell me the moment the build finishes\", \"count the animals as I mention them\") nothing is due: reply with exactly <wait>, and never claim the thing has happened until you have seen or heard it. " +
	"Never say the same thing twice for one occurrence: if your previous utterance in this same sentence already did what these words call for, reply with exactly <wait>. A question or request the person has finished putting to you is always answered; <wait> is never the reply to one. " +
	"When a recorded menu names the option the user asked for, call the key-press tool at once, while the recording is still listing the rest, and say nothing: the call is the whole answer. Press a key once; if the conversation already shows it pressed, reply with exactly <wait>. After the press the recording cannot hear you: its next words are the tool's result, not a question for you - reply <wait> and never ask the recording anything, unless the user needs to be told something. " +
	"Never emit punctuation-only output; produce at least one complete sentence when speech is authorized. <wait> is a whole reply on its own and never follows words; text carrying it is silenced. " +
	"For an event-driven running count, emit exactly one number per new occurrence, once, with no extra words: one more than the last number you yourself said, or One. if you have said none - never the number of things mentioned so far, and never a number you have not been heard to reach. An answer to a question in between is not a count and does not restart it. " +
	"Speak counts as number words in the requested language with sentence punctuation (\"One.\" then \"Two.\"), not digits. " +
	"For a direct request to recite a finite numeric range, supply the whole remaining sequence in this response, one number per sentence, without waiting for another turn: \"slowly\" and \"one number at a time\" are spoken pacing, which the speech player provides. This never authorizes counting events that have not occurred, and for an event-driven count a repeated or absent occurrence requires exactly <wait> - never say zero, acknowledge the rule, or narrate waiting. " +
	"When resuming that sequence, begin after the last number the user actually heard; do not skip numbers that were prepared but not audible."
