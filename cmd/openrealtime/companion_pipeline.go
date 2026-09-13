package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/bojieli/OpenRealtime/adapters/deepgram"
	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
	"github.com/bojieli/OpenRealtime/interaction"
)

// roomPipeline is one pipeline the companion can run. Every pipeline is a
// complete profile selection built from the room by changing what it names,
// so two pipelines differ in exactly the settings their builders change.
type roomPipeline struct {
	Name    string
	Summary string
	options func() scenarioProfileOptions
}

// roomPipelines is the list `openrealtime pipelines` prints and
// `companion -pipeline` accepts. The first is the default.
func roomPipelines() []roomPipeline {
	return []roomPipeline{
		{
			Name: "room",
			Summary: "the default: Deepgram Nova-3 in English and Mandarin lanes, local Qwen policy, " +
				"Gemini 3.8 Flash voice, Fish Speech, speaker identity",
			options: defaultRoomProfileOptions,
		},
		{
			Name: "room-flux",
			Summary: "the room recognising with Deepgram Flux (flux-general-en) instead of Nova-3; " +
				"English only, so the Mandarin translation scenario cannot pass",
			options: defaultFluxRoomProfileOptions,
		},
		{
			Name: "room-flux-eot",
			Summary: "room-flux with Flux's EndOfTurn ending the utterance instead of 500 ms of silence; " +
				"partials still reach the policy",
			options: defaultFluxEndOfTurnRoomProfileOptions,
		},
		{
			Name: "room-flux-eager",
			Summary: "room-flux with EagerEndOfTurn (threshold 0.5) ending the utterance as soon as Flux " +
				"is moderately confident; partials still reach the policy",
			options: defaultFluxEagerRoomProfileOptions,
		},
		{
			Name:    "room-filtered",
			Summary: "the room with the pre-recognition noise filter at 127.0.0.1:8125",
			options: defaultFilteredRoomProfileOptions,
		},
		{
			Name: "room-target",
			Summary: "the room extracting the enrolled speaker at 127.0.0.1:8126 before recognition, " +
				"without per-utterance speaker comparison",
			options: defaultTargetRoomProfileOptions,
		},
	}
}

func lookupRoomPipeline(name string) (roomPipeline, error) {
	pipelines := roomPipelines()
	if strings.TrimSpace(name) == "" {
		return pipelines[0], nil
	}
	names := make([]string, 0, len(pipelines))
	for _, pipeline := range pipelines {
		if pipeline.Name == name {
			return pipeline, nil
		}
		names = append(names, pipeline.Name)
	}
	return roomPipeline{}, fmt.Errorf("unknown pipeline %q; the pipelines are %s", name, strings.Join(names, ", "))
}

// roomPipelineSelection builds a pipeline's selection and applies a config
// file of profile settings on top. The file uses the flag names of
// `openrealtime profile scenario`, nested on dashes as serve's -config does,
// and a key that is not a setting is an error.
func roomPipelineSelection(name, configPath string) (scenarioProfileOptions, string, error) {
	pipeline, err := lookupRoomPipeline(name)
	if err != nil {
		return scenarioProfileOptions{}, "", err
	}
	selection := pipeline.options()
	label := pipeline.Name
	if strings.TrimSpace(configPath) != "" {
		flags := flag.NewFlagSet("pipeline config", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		bindScenarioProfileSettings(flags, &selection)
		if err := loadConfig(flags, configPath); err != nil {
			return scenarioProfileOptions{}, "", err
		}
		label += " with " + filepath.Base(configPath)
	}
	return selection, label, nil
}

// runPipelines lists the pipelines, or prints one pipeline's component
// settings in the form a -pipeline-config file takes.
func runPipelines(arguments []string, output io.Writer) error {
	switch len(arguments) {
	case 0:
		table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		for _, pipeline := range roomPipelines() {
			fmt.Fprintf(table, "%s\t%s\n", pipeline.Name, pipeline.Summary)
		}
		if err := table.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(output, "\nrun \"openrealtime pipelines NAME\" for its settings and "+
			"\"openrealtime companion -pipeline NAME\" to run it")
		return nil
	case 1:
		if strings.HasPrefix(arguments[0], "-") {
			return errors.New("usage: openrealtime pipelines [NAME]")
		}
		pipeline, err := lookupRoomPipeline(arguments[0])
		if err != nil {
			return err
		}
		describeRoomPipeline(output, pipeline)
		return nil
	default:
		return errors.New("usage: openrealtime pipelines [NAME]")
	}
}

// describeRoomPipeline writes the settings that tell pipelines apart. The
// output is itself a valid -pipeline-config: applied to any pipeline, it
// selects these components.
func describeRoomPipeline(output io.Writer, pipeline roomPipeline) {
	selection := pipeline.options()
	fmt.Fprintf(output, "# %s: %s\n", pipeline.Name, pipeline.Summary)
	text := func(name, value string) { fmt.Fprintf(output, "%s: %s\n", name, strconv.Quote(value)) }
	number := func(name string, value int64) { fmt.Fprintf(output, "%s: %d\n", name, value) }
	decimal := func(name string, value float64) {
		if value != 0 {
			fmt.Fprintf(output, "%s: %s\n", name, strconv.FormatFloat(value, 'f', -1, 64))
		}
	}
	text("architecture", selection.architecture)
	text("asr-provider", selection.asrProvider)
	text("asr-model", selection.asrModel)
	text("asr-url", selection.asrURL)
	text("asr-language", selection.asrLanguage)
	number("asr-endpointing-ms", selection.asrEndpointingMS)
	decimal("asr-eot-threshold", selection.asrEOTThreshold)
	decimal("asr-eager-eot-threshold", selection.asrEagerEOTThreshold)
	if selection.asrEOTTimeoutMS != 0 {
		number("asr-eot-timeout-ms", selection.asrEOTTimeoutMS)
	}
	number("asr-cadence-ms", selection.asrCadenceMS)
	text("asr-end-of-turn", selection.asrEndOfTurn)
	for _, keyterm := range selection.asrKeyterms {
		// A config file's asr-keyterm adds to the pipeline's own, so repeating
		// these would name them twice.
		fmt.Fprintf(output, "# asr-keyterm: %s\n", strconv.Quote(keyterm))
	}
	text("policy-provider", selection.policyProvider)
	text("policy-model", selection.policyModel)
	text("policy-url", selection.policyURL)
	text("model-provider", selection.modelProvider)
	text("model", selection.modelName)
	text("model-url", selection.modelURL)
	text("model-effort", selection.modelEffort)
	text("tts-provider", selection.ttsProvider)
	text("tts-model", selection.ttsModel)
	text("tts-url", selection.ttsURL)
	text("speaker-url", selection.speakerURL)
	text("noise-filter-url", selection.noiseFilterURL)
	if selection.noiseFilterModel != "" {
		// The filter model is fixed by the pipeline; no setting chooses it.
		fmt.Fprintf(output, "# noise filter model: %s\n", strconv.Quote(selection.noiseFilterModel))
	}
	number("gate-silence-ms", int64(selection.gateSilenceMS))
}

// defaultRoomProfileOptions is the project's default pipeline: the cascade
// the conversation room runs, accepted by the twelve scripted scenarios
// played against the real policy and voice models (docs/room.md, "The
// default pipeline"). A change to it is judged there.
func defaultRoomProfileOptions() scenarioProfileOptions {
	selection := defaultScenarioProfileOptions()
	selection.name = "openrealtime.launch.conversation-room"
	selection.architecture = "cascade.composed-policy-direct-visual-speaker@1"
	selection.asrProvider = "deepgram"
	selection.asrModel = "nova-3"
	selection.asrURL = "wss://api.deepgram.com/v1/listen"
	selection.asrLanguage = "en-US,zh-CN"
	selection.asrKeyterms = []string{"sea bass"}
	selection.asrPartialMS = 0
	selection.asrEndpointingMS = 300
	selection.asrCadenceMS = 100
	selection.speakerURL = "http://127.0.0.1:8124/embed"
	selection.modelProvider = "google"
	selection.modelName = "gemini-3.8-flash"
	selection.modelURL = "https://generativelanguage.googleapis.com/v1beta"
	// Gemini shares its output ceiling with thinking; 128 can exhaust the
	// entire response before any audible answer is generated.
	selection.maxOutputTokens = 1024
	selection.continuationInstruction = "Every word you generate is spoken aloud. Answer the latest user request directly in the language they are using unless they requested translation. Runtime notes, observation labels, and playback annotations are context, never words to read aloud. " + selection.continuationInstruction
	// Measured on 2026-09-13 on the room's own prompt, side by side in the
	// same minutes: 3.8 Flash answered in 0.7-0.9 s where 3.7 took 1.5-3.4 s,
	// and over three twelve-scenario passes its generations ran p50 1.0 s,
	// p90 2.2 s, max 3.9 s against 3.7's p50 2.0 s, p90 3.8 s that evening,
	// with the same behaviour (11/12 each pass, a different latency miss
	// each time). The lite models answer in 0.4 s and press the wrong key.
	// Earlier profiling had favoured 3.7/128 over the Flash versions of its
	// day for the same reason: first-text latency without the long tail.
	selection.modelEffort = "128"
	selection.modelReason = "on"
	// The one instruction the interaction policy reads at every event. Passed
	// explicitly, rather than left to the element default, so the frozen
	// profile records the exact text the room ran with.
	selection.transcriptRules = interaction.ChoiceInstruction
	// The word-timing role, not the speech recogniser. These name the
	// adapter's own constants rather than repeating an address, because the
	// room previously named the recogniser on :8003: it answers this route
	// with text and no word array, so every boundary fell back to the
	// proportional estimate and nothing said so.
	selection.wordTimingsURL = wordtimings.DefaultEndpoint
	selection.wordTimingsModel = wordtimings.DefaultModel
	selection.wordTimingsLanguage = "en"
	selection.wordTimingsIntervalMS = 900
	return selection
}

// defaultFluxRoomProfileOptions is the room with Deepgram Flux recognising.
// It changes the recogniser and nothing else, so a comparison with the room
// measures the recogniser. Flux has no endpointing and no Mandarin: the
// engine's acoustic gate still ends utterances, an open Flux turn is closed
// with ForceEndTurn, and the one lane is English.
func defaultFluxRoomProfileOptions() scenarioProfileOptions {
	selection := defaultRoomProfileOptions()
	selection.name = "openrealtime.launch.flux-room"
	selection.asrModel = deepgram.DefaultFluxModel
	selection.asrURL = deepgram.DefaultFluxURL
	selection.asrLanguage = "en-US"
	selection.asrEndpointingMS = 0
	return selection
}

// defaultFluxEndOfTurnRoomProfileOptions lets Flux own the endpoint: its
// EndOfTurn closes the utterance, and the acoustic gate's silence is only the
// backstop.
func defaultFluxEndOfTurnRoomProfileOptions() scenarioProfileOptions {
	selection := defaultFluxRoomProfileOptions()
	selection.name = "openrealtime.launch.flux-eot-room"
	selection.asrEndOfTurn = "end_of_turn"
	return selection
}

// defaultFluxEagerRoomProfileOptions closes the utterance on EagerEndOfTurn.
// 0.5 is inside the range Deepgram documents as 150-250 ms ahead of EndOfTurn
// at the cost of more turns ended that the person was not finished with; a
// continuation is a new utterance, and while the agent is speaking the policy
// is asked on its partials whether the person is cutting in.
func defaultFluxEagerRoomProfileOptions() scenarioProfileOptions {
	selection := defaultFluxRoomProfileOptions()
	selection.name = "openrealtime.launch.flux-eager-room"
	selection.asrEagerEOTThreshold = 0.5
	selection.asrEndOfTurn = "eager"
	return selection
}

// defaultFilteredRoomProfileOptions is a separate opt-in pipeline. Noise is
// removed before EnergyAdmission can emit speech activity or feed Deepgram.
func defaultFilteredRoomProfileOptions() scenarioProfileOptions {
	selection := defaultRoomProfileOptions()
	selection.name = "openrealtime.launch.filtered-room"
	selection.noiseFilterURL = "http://127.0.0.1:8125"
	selection.noiseFilterTimeoutMS = 50
	return selection
}

// defaultTargetRoomProfileOptions enrolls the initial voice once, then extracts
// it from competing speech before Deepgram. No per-utterance identity service.
func defaultTargetRoomProfileOptions() scenarioProfileOptions {
	selection := defaultFilteredRoomProfileOptions()
	selection.name = "openrealtime.launch.target-room"
	selection.noiseFilterURL = "http://127.0.0.1:8126"
	selection.noiseFilterModel = "real-tse"
	selection.architecture = "cascade.composed-policy-direct-visual@1"
	selection.speakerURL = ""
	return selection
}
