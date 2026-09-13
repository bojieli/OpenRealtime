package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/fdbv3"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/noisefilter"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

const launchProfileUsage = `usage: openrealtime profile <scenario|filtered-room|target-room|meeting|realtime-cu> [flags]

Freeze one strict graph/server profile against the exact running executable
and explicit provider configurations. Outputs are create-only and contain no
credential. Use this same executable to serve the resulting file.`

// productionScenarioContinuationInstruction is graphs.ProductionContinuationInstruction,
// kept under its old name for the profile freezer and its tests.
const productionScenarioContinuationInstruction = graphs.ProductionContinuationInstruction

type scenarioProfileOptions struct {
	noiseFilterURL       string
	noiseFilterModel     string
	noiseFilterTimeoutMS int
	out                  string
	graphOut             string
	valuesOut            string
	resolutionOut        string
	executionOut         string
	name                 string
	revision             uint64
	architecture         string
	cases                []string

	asrProvider           string
	asrModel              string
	asrURL                string
	asrLanguage           string
	asrKeyterms           []string
	asrPartialMS          int64
	asrEndpointingMS      int64
	asrEOTThreshold       float64
	asrEagerEOTThreshold  float64
	asrEOTTimeoutMS       int64
	asrTimeoutMS          int64
	asrCadenceMS          int64
	speakerURL            string
	speakerModel          string
	speakerTimeoutMS      int64
	modelProvider         string
	modelName             string
	modelURL              string
	modelEffort           string
	modelVision           bool
	modelReason           string
	modelRetainReason     bool
	modelTemperature      float64
	modelTimeoutMS        int64
	policyProvider        string
	policyModel           string
	policyURL             string
	policyTimeoutMS       int64
	policyVision          bool
	policyGuided          bool
	policyReasoning       string
	policyTokenEnv        string
	transcriptRules       string
	ttsProvider           string
	ttsModel              string
	ttsURL                string
	ttsVoice              string
	ttsLanguage           string
	ttsTimeoutMS          int64
	ttsSentenceWrap       bool
	ttsSentenceMinimum    int
	wordTimingsURL        string
	wordTimingsModel      string
	wordTimingsLanguage   string
	wordTimingsIntervalMS int64
	wordTimingsTimeoutMS  int64

	gateThreshold           float64
	gatePrefixMS            int
	gateSilenceMS           int
	gateSpeechMS            int
	maxOutputTokens         int
	continuationInstruction string
	fdbv3Dataset            string
	serverTokenEnv          string
	operatorCapabilityEnv   string
	inspectionTokenTTL      uint64
	maxAudioFrameBytes      int
}

func defaultScenarioProfileOptions() scenarioProfileOptions {
	return scenarioProfileOptions{
		name: "openrealtime.launch.scenario-local", revision: 1,
		noiseFilterTimeoutMS: 50,
		architecture:         "cascade.composed-policy-direct-visual@1",
		asrProvider:          "sensevoice", asrModel: "iic/SenseVoiceSmall",
		asrURL: "http://127.0.0.1:8002/v1", asrPartialMS: 200,
		asrTimeoutMS: 30_000, asrCadenceMS: 200,
		speakerModel: "speechbrain/spkrec-ecapa-voxceleb", speakerTimeoutMS: 5_000,
		modelProvider: "vllm", modelName: "qwen-fast",
		modelURL: "http://127.0.0.1:8000/v1", modelEffort: "minimal",
		modelVision: true, modelReason: "off", modelTemperature: 0,
		modelTimeoutMS: 30_000,
		policyProvider: "vllm", policyModel: "qwen-fast",
		policyURL: "http://127.0.0.1:8000/v1", policyTimeoutMS: 2_000,
		policyVision: true, policyGuided: true, policyReasoning: "chat_template_kwargs",
		ttsProvider: "fish-audio", ttsModel: "fishaudio/fish-speech-1.5",
		ttsURL: "http://127.0.0.1:8123/v1/tts", ttsVoice: "default",
		ttsTimeoutMS: 30_000, ttsSentenceWrap: true, ttsSentenceMinimum: 12,
		wordTimingsModel:      wordtimings.DefaultModel,
		wordTimingsIntervalMS: 1_000, wordTimingsTimeoutMS: 10_000,
		gateThreshold: 0.5, gatePrefixMS: 300, gateSilenceMS: 500,
		// The acknowledgement scenario deliberately asks for a long answer,
		// then speaks over it. A 512-token response took 176 seconds to finish
		// through the production sentence/TTS path and crossed the exact
		// three-minute task bound. 128 tokens still spans every scripted
		// acknowledgement while keeping one turn bounded under provider load.
		gateSpeechMS: 120, maxOutputTokens: 128,
		continuationInstruction: productionScenarioContinuationInstruction,
		// Runtime evidence is read only after an entire authored conversation
		// and its trailing quiet period. Five minutes covers the client's
		// two-minute per-attempt deadline without making the capability durable.
		inspectionTokenTTL: 300_000, maxAudioFrameBytes: 1 << 20,
	}
}

func runLaunchProfile(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(launchProfileUsage)
	}
	switch strings.ToLower(strings.TrimSpace(arguments[0])) {
	case "target-room":
		return runScenarioProfileFreezeWithOptions(arguments[1:], output, defaultTargetRoomProfileOptions())
	case "filtered-room":
		return runScenarioProfileFreezeWithOptions(arguments[1:], output, defaultFilteredRoomProfileOptions())
	case "scenario":
		return runScenarioProfileFreeze(arguments[1:], output)
	case "meeting", "meeting-assistant":
		return runMeetingProfileFreeze(arguments[1:], output)
	case "realtime-cu", "realtime-computer-use", "computer-use":
		return runRealtimeCUProfileFreeze(arguments[1:], output)
	case "help", "-h", "--help":
		fmt.Fprintln(output, launchProfileUsage)
		return nil
	default:
		return fmt.Errorf("profile kind must be scenario, filtered-room, target-room, meeting, or realtime-cu, got %q\n%s", arguments[0], launchProfileUsage)
	}
}

func runScenarioProfileFreeze(arguments []string, output io.Writer) error {
	return runScenarioProfileFreezeWithOptions(arguments, output, defaultScenarioProfileOptions())
}

func runScenarioProfileFreezeWithOptions(arguments []string, output io.Writer, options scenarioProfileOptions) error {
	requireNoiseFilter := options.noiseFilterURL != ""
	flags := flag.NewFlagSet("openrealtime profile scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.noiseFilterURL, "noise-filter-url", options.noiseFilterURL, "pre-ASR audio filter service base URL")
	flags.IntVar(&options.noiseFilterTimeoutMS, "noise-filter-timeout-ms", options.noiseFilterTimeoutMS, "strict per-ingress-packet filtering deadline, 1..50ms")
	flags.Func("case", "exact scenario name; repeat to freeze a diagnostic subset (default: all cases)", func(name string) error {
		options.cases = append(options.cases, name)
		return nil
	})
	flags.StringVar(&options.out, "out", "", "new absolute launch-profile YAML path")
	flags.StringVar(&options.graphOut, "graph-out", "", "new absolute exact bound Graph IR JSON path")
	flags.StringVar(&options.valuesOut, "values-out", "", "new absolute exact element-values JSON path")
	flags.StringVar(&options.resolutionOut, "resolution-out", "", "new absolute expected live-resolution JSON path")
	flags.StringVar(&options.executionOut, "execution-out", "", "new absolute reviewed execution-requirement JSON path")
	flags.StringVar(&options.name, "name", options.name, "immutable profile name")
	flags.Uint64Var(&options.revision, "revision", options.revision, "positive profile revision")
	flags.StringVar(&options.architecture, "architecture", options.architecture, "exact interaction architecture id@revision")
	flags.StringVar(&options.asrProvider, "asr-provider", options.asrProvider, "installed ASR provider plugin")
	flags.StringVar(&options.asrModel, "asr-model", options.asrModel, "exact ASR model")
	flags.StringVar(&options.asrURL, "asr-url", options.asrURL, "exact ASR base URL")
	flags.StringVar(&options.asrLanguage, "asr-language", options.asrLanguage, "ASR language hint")
	flags.Func("asr-keyterm", "Deepgram Nova-3 vocabulary hint; repeat for multiple phrases", func(value string) error {
		options.asrKeyterms = append(options.asrKeyterms, value)
		return nil
	})
	flags.Int64Var(&options.asrPartialMS, "asr-partial-ms", options.asrPartialMS, "batch partial-transcription interval")
	flags.Int64Var(&options.asrEndpointingMS, "asr-endpointing-ms", options.asrEndpointingMS, "streaming-provider endpointing")
	flags.Float64Var(&options.asrEOTThreshold, "asr-eot-threshold", options.asrEOTThreshold,
		"Deepgram Flux EndOfTurn confidence, 0.5 to 1.0; 0 keeps the service default")
	flags.Float64Var(&options.asrEagerEOTThreshold, "asr-eager-eot-threshold", options.asrEagerEOTThreshold,
		"Deepgram Flux EagerEndOfTurn confidence, 0.3 to 0.9; 0 disables EagerEndOfTurn and TurnResumed")
	flags.Int64Var(&options.asrEOTTimeoutMS, "asr-eot-timeout-ms", options.asrEOTTimeoutMS,
		"Deepgram Flux silence that ends a turn regardless, 500 to 60000; 0 keeps the service default")
	flags.Int64Var(&options.asrTimeoutMS, "asr-timeout-ms", options.asrTimeoutMS, "ASR request timeout")
	flags.Int64Var(&options.asrCadenceMS, "asr-cadence-ms", options.asrCadenceMS, "ASR graph cadence")
	flags.StringVar(&options.speakerURL, "speaker-url", options.speakerURL, "speaker-embedding endpoint ending in /embed")
	flags.StringVar(&options.speakerModel, "speaker-model", options.speakerModel, "exact speaker-embedding model")
	flags.Int64Var(&options.speakerTimeoutMS, "speaker-timeout-ms", options.speakerTimeoutMS, "speaker-embedding request timeout")
	flags.StringVar(&options.modelProvider, "model-provider", options.modelProvider, "installed text-model provider plugin")
	flags.StringVar(&options.modelName, "model", options.modelName, "exact text model")
	flags.StringVar(&options.modelURL, "model-url", options.modelURL, "exact text-model base URL")
	flags.StringVar(&options.modelEffort, "model-effort", options.modelEffort, "canonical reasoning effort")
	flags.BoolVar(&options.modelVision, "model-vision", options.modelVision, "declare exact image-input support")
	flags.StringVar(&options.modelReason, "model-reason", options.modelReason, "reasoning control: off, on, or default")
	flags.BoolVar(&options.modelRetainReason, "model-retain-reasoning", options.modelRetainReason, "retain reasoning in the trajectory")
	flags.Float64Var(&options.modelTemperature, "model-temperature", options.modelTemperature, "sampling temperature")
	flags.Int64Var(&options.modelTimeoutMS, "model-timeout-ms", options.modelTimeoutMS, "text-model request timeout")
	flags.StringVar(&options.policyProvider, "policy-provider", options.policyProvider, "installed enumerated semantic-policy provider plugin")
	flags.StringVar(&options.policyModel, "policy-model", options.policyModel, "exact semantic-policy model")
	flags.StringVar(&options.policyURL, "policy-url", options.policyURL, "exact semantic-policy base URL")
	flags.Int64Var(&options.policyTimeoutMS, "policy-timeout-ms", options.policyTimeoutMS, "semantic-policy decision timeout")
	flags.BoolVar(&options.policyVision, "policy-vision", options.policyVision, "declare exact semantic-policy image-input support")
	flags.BoolVar(&options.policyGuided, "policy-guided-choice", options.policyGuided, "request provider-side enumerated-choice decoding")
	flags.StringVar(&options.policyReasoning, "policy-reasoning", options.policyReasoning, "semantic-policy reasoning control")
	flags.StringVar(&options.policyTokenEnv, "policy-token-env", options.policyTokenEnv, "optional provider-owned semantic-policy credential environment name")
	flags.StringVar(&options.transcriptRules, "transcript-rules", options.transcriptRules,
		"interaction-policy instruction read at every transcript, visual, and quiet event; empty selects the built-in rules")
	flags.StringVar(&options.ttsProvider, "tts-provider", options.ttsProvider, "installed TTS provider plugin")
	flags.StringVar(&options.ttsModel, "tts-model", options.ttsModel, "exact TTS model")
	flags.StringVar(&options.ttsURL, "tts-url", options.ttsURL, "exact TTS endpoint")
	flags.StringVar(&options.ttsVoice, "tts-voice", options.ttsVoice, "fixed TTS voice")
	flags.StringVar(&options.ttsLanguage, "tts-language", options.ttsLanguage, "TTS language hint")
	flags.Int64Var(&options.ttsTimeoutMS, "tts-timeout-ms", options.ttsTimeoutMS, "TTS request timeout")
	flags.BoolVar(&options.ttsSentenceWrap, "tts-sentence-wrapping", options.ttsSentenceWrap, "synthesize prepared text by sentence")
	flags.IntVar(&options.ttsSentenceMinimum, "tts-sentence-minimum-runes", options.ttsSentenceMinimum, "minimum sentence size")
	flags.StringVar(&options.wordTimingsURL, "word-timings-url", options.wordTimingsURL,
		"optional OpenAI-shaped transcription endpoint that returns word timestamps")
	flags.StringVar(&options.wordTimingsModel, "word-timings-model", options.wordTimingsModel,
		"exact recogniser served by the word-timing endpoint")
	flags.StringVar(&options.wordTimingsLanguage, "word-timings-language", options.wordTimingsLanguage,
		"word-timing recogniser language hint")
	flags.Int64Var(&options.wordTimingsIntervalMS, "word-timings-interval-ms", options.wordTimingsIntervalMS,
		"new synthesised audio required before refreshing word timings")
	flags.Int64Var(&options.wordTimingsTimeoutMS, "word-timings-timeout-ms", options.wordTimingsTimeoutMS,
		"word-timing request timeout")
	flags.Float64Var(&options.gateThreshold, "gate-threshold", options.gateThreshold, "acoustic energy threshold")
	flags.IntVar(&options.gatePrefixMS, "gate-prefix-ms", options.gatePrefixMS, "acoustic prefix padding")
	flags.IntVar(&options.gateSilenceMS, "gate-silence-ms", options.gateSilenceMS, "silence that closes one utterance")
	flags.IntVar(&options.gateSpeechMS, "gate-speech-ms", options.gateSpeechMS, "minimum admitted speech")
	flags.IntVar(&options.maxOutputTokens, "max-output-tokens", options.maxOutputTokens, "model output-token bound")
	flags.StringVar(&options.continuationInstruction, "continuation-instruction", options.continuationInstruction,
		"profile-owned continuation evidence and deferred-action policy")
	flags.StringVar(&options.fdbv3Dataset, "fdbv3-dataset", options.fdbv3Dataset, "released FDB v3 dataset whose exact tool union replaces the scenario-suite tools")
	flags.StringVar(&options.serverTokenEnv, "token-env", options.serverTokenEnv, "optional gateway bearer-token environment name")
	flags.StringVar(&options.operatorCapabilityEnv, "operator-capability-env", options.operatorCapabilityEnv,
		"optional separate mgmt_ operator-capability environment name")
	flags.Uint64Var(&options.inspectionTokenTTL, "inspection-token-ttl-ms", options.inspectionTokenTTL, "runtime-inspection token lifetime")
	flags.IntVar(&options.maxAudioFrameBytes, "max-audio-frame-bytes", options.maxAudioFrameBytes, "Realtime audio-frame bound")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if requireNoiseFilter && strings.TrimSpace(options.noiseFilterURL) == "" {
		return errors.New("filtered room profiles require a noise-filter-url")
	}
	if flags.NArg() != 0 || strings.TrimSpace(options.out) == "" {
		return errors.New("profile scenario requires -out and accepts flags only")
	}
	if (strings.TrimSpace(options.graphOut) == "") != (strings.TrimSpace(options.valuesOut) == "") {
		return errors.New("profile scenario requires -graph-out and -values-out together")
	}
	if (strings.TrimSpace(options.resolutionOut) == "") != (strings.TrimSpace(options.executionOut) == "") {
		return errors.New("profile scenario requires -resolution-out and -execution-out together")
	}
	if options.resolutionOut != "" && options.graphOut == "" {
		return errors.New("profile scenario execution outputs require -graph-out and -values-out")
	}
	paths := []string{options.out, options.graphOut, options.valuesOut, options.resolutionOut, options.executionOut}
	seenPaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, duplicate := seenPaths[path]; duplicate {
			return errors.New("profile scenario output paths must be distinct")
		}
		seenPaths[path] = struct{}{}
	}
	profile, launched, err := freezeProductionScenarioProfile(context.Background(), options)
	if err != nil {
		return err
	}
	plan := launched.Plan
	if plan == nil {
		return errors.New("frozen scenario profile has no graph plan")
	}
	profilePayload, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		return fmt.Errorf("encode scenario launch profile: %w", err)
	}
	var graphPayload, valuesPayload, resolutionPayload, executionPayload []byte
	if options.graphOut != "" {
		graphPayload, err = plan.Graph().Marshal()
		if err != nil {
			return fmt.Errorf("encode scenario bound Graph IR: %w", err)
		}
		values := graphvalues.Document{
			APIVersion: graphvalues.APIVersion, Graph: plan.Graph().ID, Nodes: plan.Values(),
		}
		valuesPayload, err = json.MarshalIndent(values, "", "  ")
		if err != nil {
			return fmt.Errorf("encode scenario element values: %w", err)
		}
		valuesPayload = append(valuesPayload, '\n')
		bound, err := graphvalues.Bind(plan.Graph(), values)
		if err != nil || bound.Graph.Fingerprint != plan.Graph().Fingerprint {
			return errors.New("freeze scenario values did not reproduce the exact bound graph")
		}
		if options.resolutionOut != "" {
			configuration := bench.ArtifactIdentity{
				ID: "values://" + plan.Graph().ID, Revision: graphvalues.APIVersion,
				Digest: bound.Fingerprint,
			}
			resolution, err := probeScenarioExpectedResolution(
				context.Background(), launched.Binding, plan, configuration, legacy.Settings{
					Modalities: []string{"audio"},
					Gate: perception.GateConfig{
						Threshold: options.gateThreshold, PrefixPaddingMS: options.gatePrefixMS,
						SilenceDurationMS: options.gateSilenceMS, SpeechDurationMS: options.gateSpeechMS,
					},
				},
			)
			if err != nil {
				return err
			}
			requirement, err := bench.RequireGraph(plan.Graph(), configuration, resolution)
			if err != nil {
				return fmt.Errorf("author scenario execution requirement: %w", err)
			}
			resolutionPayload, err = bench.MarshalExpectedResolution(resolution)
			if err != nil {
				return err
			}
			executionPayload, err = bench.MarshalExecutionRequirement(requirement)
			if err != nil {
				return err
			}
		}
	}
	if options.graphOut != "" {
		if err := writeCreateOnlyLaunchProfile(options.graphOut, graphPayload); err != nil {
			return fmt.Errorf("write scenario bound Graph IR: %w", err)
		}
		if err := writeCreateOnlyLaunchProfile(options.valuesOut, valuesPayload); err != nil {
			return fmt.Errorf("write scenario element values: %w", err)
		}
	}
	if options.resolutionOut != "" {
		if err := writeCreateOnlyLaunchProfile(options.resolutionOut, resolutionPayload); err != nil {
			return fmt.Errorf("write scenario expected live resolution: %w", err)
		}
		if err := writeCreateOnlyLaunchProfile(options.executionOut, executionPayload); err != nil {
			return fmt.Errorf("write scenario execution requirement: %w", err)
		}
	}
	// The launch profile is the publication marker: exact companions are closed
	// and durable before it becomes visible.
	if err := writeCreateOnlyLaunchProfile(options.out, profilePayload); err != nil {
		return fmt.Errorf("write scenario launch profile: %w", err)
	}
	fmt.Fprintf(output, "wrote %s\n", options.out)
	if options.graphOut != "" {
		fmt.Fprintf(output, "graph       %s\n", options.graphOut)
		fmt.Fprintf(output, "values      %s\n", options.valuesOut)
	}
	if options.resolutionOut != "" {
		fmt.Fprintf(output, "resolution  %s\n", options.resolutionOut)
		fmt.Fprintf(output, "execution   %s\n", options.executionOut)
	}
	fmt.Fprintf(output, "profile     %s@%d\n", profile.Name, profile.Revision)
	fmt.Fprintf(output, "fingerprint %s\n", profile.Fingerprint)
	fmt.Fprintf(output, "plan        %s\n", profile.Plan.PlanFingerprint)
	fmt.Fprintf(output, "executable  %s\n", profile.Server.GatewayArtifact.Digest)
	fmt.Fprintf(output, "providers   asr=%s/%s policy=%s/%s model=%s/%s tts=%s/%s\n",
		options.asrProvider, options.asrModel, options.policyProvider, options.policyModel,
		options.modelProvider, options.modelName,
		options.ttsProvider, options.ttsModel)
	return nil
}

func freezeProductionScenarioProfile(
	ctx context.Context, options scenarioProfileOptions,
) (launchprofile.Document, graphlaunch.Result, error) {
	if ctx == nil {
		return launchprofile.Document{}, graphlaunch.Result{}, errors.New("freeze production scenario profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	contract, err := graphnative.BuildContract(options.cases...)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	host, err := newServeProfileHost(artifacts, inventory)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	asr, err := scenarioProfileASRSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	speakerIdentity, err := scenarioProfileSpeakerIdentitySelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	policy, err := scenarioProfilePolicySelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	model, err := scenarioProfileModelSelection(inventory, options, "voice")
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	tts, err := scenarioProfileTTSSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	wordTiming, err := scenarioProfileWordTimingSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	tools, err := productionProfileToolDeclarations(options.fdbv3Dataset)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	architecture, err := projectarch.Default().Resolve(options.architecture)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("resolve scenario architecture: %w", err)
	}
	application := scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		Architecture:  architecture.Identity(),
		ASR:           asr, SpeakerIdentity: speakerIdentity,
		Policy: policy, Model: model, TTS: tts,
		WordTiming: wordTiming,
		SemanticAdmission: scenarioconversation.SemanticAdmissionSelection{
			StandingExtraction: true, StandingMemory: 64, Rules: options.transcriptRules,
		},
		Tools: tools,
		Target: computeruse.Target{
			Name: "scenario-client", Sources: []string{scenarioconversation.SourceMessage},
			Width: 64, Height: 48,
		},
		Gate: scenarioconversation.ApplicationGateSelection{
			Threshold: options.gateThreshold, PrefixPaddingMS: options.gatePrefixMS,
			SilenceDurationMS: options.gateSilenceMS, SpeechDurationMS: options.gateSpeechMS,
		},
		Media: scenarioconversation.MediaLimits{
			MaxItems: 8, MaxBytes: 4 << 20, MaxItemBytes: 2 << 20,
			MaxPending: 8, MaxActiveLeases: 16,
		},
		MaxOutputTokens:         options.maxOutputTokens,
		ContinuationInstruction: options.continuationInstruction,
	}
	if options.noiseFilterURL != "" {
		application.NoiseFilter = &noisefilter.Config{URL: options.noiseFilterURL, TimeoutMS: options.noiseFilterTimeoutMS, Model: options.noiseFilterModel}
	}
	delegatePayload, err := json.Marshal(application)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("encode scenario application: %w", err)
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, host.Delegate, delegatePayload,
	)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("freeze scenario application: %w", err)
	}
	profile, err := graphnative.FreezeLaunchProfile(ctx, graphnative.LaunchProfileConfig{
		Name: options.name, Revision: options.revision, Contract: contract,
		Application: host.ScenarioSuite, Configuration: configuration,
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.scenario-graph-native", ProfileRevision: 1,
			ProviderArtifact: artifacts.ScenarioProvider, GatewayArtifact: artifacts.Gateway,
			TokenEnvironment:              options.serverTokenEnv,
			OperatorCapabilityEnvironment: options.operatorCapabilityEnv,
			Model:                         options.modelName,
			TranscriptionModel:            options.asrModel, ValidateWire: true,
			InspectionTokenTTLMS: options.inspectionTokenTTL,
			MaxAudioFrameBytes:   options.maxAudioFrameBytes,
			VideoLimits:          openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, err
	}
	launchConfig, err := host.ScenarioSuite.Factory(ctx, profile.Application.Configuration)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("prepare frozen scenario launch: %w", err)
	}
	launched, err := graphlaunch.New(ctx, launchConfig)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("prepare frozen scenario binding: %w", err)
	}
	composition, err := newProfiledServeComposition(ctx, profile, host, nil)
	if err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("prepare frozen scenario profile artifacts: %w", err)
	}
	if composition.Graph == nil || composition.Graph.GraphPlan == nil {
		return launchprofile.Document{}, graphlaunch.Result{}, errors.New("prepare frozen scenario profile artifacts: no graph plan")
	}
	plan := launched.Plan
	if plan == nil || composition.Graph.GraphPlan.Identity() != plan.Identity() {
		return launchprofile.Document{}, graphlaunch.Result{}, errors.New("frozen scenario launch preparations disagree")
	}
	if err := plan.Validate(); err != nil {
		return launchprofile.Document{}, graphlaunch.Result{}, fmt.Errorf("validate frozen scenario graph plan: %w", err)
	}
	if plan.Identity() != profile.Plan {
		return launchprofile.Document{}, graphlaunch.Result{}, errors.New("frozen scenario profile and prepared graph plan disagree")
	}
	return profile, launched, nil
}

// productionScenarioToolDeclarations derives the profile-owned client action
// surface from the benchmark suite that will submit it. The graph adapter
// exact-matches names, descriptions, schemas, and confirmation policy during
// session.update, so a hand-copied declaration would make the recorded-menu
// case fail before any behavior could be measured.
func productionProfileToolDeclarations(
	fdbv3Dataset string,
) ([]scenarioconversation.ToolDeclaration, error) {
	if fdbv3Dataset != "" {
		return productionFDBV3ToolDeclarations(fdbv3Dataset)
	}
	return productionScenarioToolDeclarations()
}

func productionScenarioToolDeclarations() ([]scenarioconversation.ToolDeclaration, error) {
	byName := make(map[string]scenarioconversation.ToolDeclaration)
	var order []string
	for _, item := range scenario.Suite() {
		for _, tool := range item.Tools {
			canonical, err := tool.FunctionDeclaration()
			if err != nil {
				return nil, err
			}
			declaration := scenarioconversation.ToolDeclaration{
				Name: canonical.Name, Description: canonical.Description,
				Parameters: canonical.Parameters, Confirm: legacyaction.ConfirmNever,
			}
			if existing, found := byName[tool.Name]; found {
				if existing.Description != declaration.Description ||
					!bytes.Equal(existing.Parameters, declaration.Parameters) {
					return nil, fmt.Errorf("scenario tool %q has conflicting declarations", tool.Name)
				}
				continue
			}
			byName[tool.Name] = declaration
			order = append(order, tool.Name)
		}
	}
	result := make([]scenarioconversation.ToolDeclaration, 0, len(order))
	for _, name := range order {
		result = append(result, byName[name])
	}
	return result, nil
}

// productionFDBV3ToolDeclarations derives the application-owned action
// surface from the same catalog builder the benchmark uses. The profile then
// exact-matches each session.update declaration, while ordinary scenario
// profiles retain only their much smaller authored tool surface.
func productionFDBV3ToolDeclarations(
	dataset string,
) ([]scenarioconversation.ToolDeclaration, error) {
	if strings.TrimSpace(dataset) == "" || dataset != strings.TrimSpace(dataset) {
		return nil, errors.New("scenario FDB v3 tool selection requires a canonical dataset path")
	}
	tasks, err := fdbv3.LoadReleased(dataset)
	if err != nil {
		return nil, fmt.Errorf("load scenario FDB v3 tool dataset: %w", err)
	}
	return fdbV3ToolDeclarations(tasks)
}

// fdbV3ToolDeclarations is the pure catalog-to-declaration projection used by
// focused tests. Production callers must enter through
// productionFDBV3ToolDeclarations so an incomplete or byte-drifted dataset
// cannot freeze a benchmark profile.
func fdbV3ToolDeclarations(tasks []fdbv3.Task) ([]scenarioconversation.ToolDeclaration, error) {
	if len(tasks) == 0 {
		return nil, errors.New("scenario FDB v3 tool dataset contains no released tasks")
	}
	catalog, err := fdbv3.Catalog(tasks)
	if err != nil {
		return nil, fmt.Errorf("derive scenario FDB v3 tool catalog: %w", err)
	}
	declarations := make([]scenarioconversation.ToolDeclaration, 0, len(catalog))
	for index, raw := range catalog {
		var tool struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, fmt.Errorf("decode scenario FDB v3 tool %d: %w", index, err)
		}
		if tool.Type != "function" {
			return nil, fmt.Errorf("scenario FDB v3 tool %d has type %q, want function", index, tool.Type)
		}
		normalizers := fdbv3.ArgumentNormalizers(tool.Name)
		argumentNormalizers := make([]legacyaction.ToolArgumentNormalizer, len(normalizers))
		for normalizerIndex, normalizer := range normalizers {
			argumentNormalizers[normalizerIndex] = legacyaction.ToolArgumentNormalizer{
				Argument: normalizer.Argument, Normalizer: normalizer.Normalizer,
			}
		}
		declarations = append(declarations, scenarioconversation.ToolDeclaration{
			Name: tool.Name, Description: tool.Description,
			Parameters: tool.Parameters, ArgumentNormalizers: argumentNormalizers,
			Confirm: legacyaction.ConfirmNever,
		})
	}
	return declarations, nil
}

func scenarioProfileASRSelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
) (scenarioconversation.ApplicationASRSelection, error) {
	reference := serveProviderReference("asr", options.asrProvider)
	for _, registration := range inventory.ASR {
		if registration.Reference != reference {
			continue
		}
		raw, err := json.Marshal(serveASRConfiguration{
			FormatVersion: 1, Model: options.asrModel, BaseURL: options.asrURL,
			Language: options.asrLanguage, Keyterms: slices.Clone(options.asrKeyterms),
			PartialIntervalMS: options.asrPartialMS,
			EndpointingMS:     options.asrEndpointingMS, RequestTimeoutMS: options.asrTimeoutMS,
			CadenceMS:    options.asrCadenceMS,
			EOTThreshold: options.asrEOTThreshold, EagerEOTThreshold: options.asrEagerEOTThreshold,
			EOTTimeoutMS: options.asrEOTTimeoutMS,
		})
		if err != nil {
			return scenarioconversation.ApplicationASRSelection{}, err
		}
		descriptor, err := registration.DescribeConfiguration(raw)
		if err != nil {
			return scenarioconversation.ApplicationASRSelection{}, fmt.Errorf("describe scenario ASR: %w", err)
		}
		return scenarioconversation.ApplicationASRSelection{
			Reference: reference, Artifact: registration.Artifact,
			Descriptor: descriptor, Configuration: raw,
		}, nil
	}
	return scenarioconversation.ApplicationASRSelection{}, fmt.Errorf("scenario ASR inventory is missing %q", reference)
}

func scenarioProfileSpeakerIdentitySelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
) (*scenarioconversation.ApplicationSpeakerIdentitySelection, error) {
	if strings.TrimSpace(options.speakerURL) == "" {
		return nil, nil
	}
	reference := serveProviderReference("speaker-identity", "speakerid")
	for _, registration := range inventory.SpeakerIdentity {
		if registration.Reference != reference {
			continue
		}
		raw, err := json.Marshal(serveSpeakerIdentityConfiguration{
			FormatVersion: serveProviderConfigurationVersion,
			Endpoint:      options.speakerURL, Model: options.speakerModel,
			RequestTimeoutMS: options.speakerTimeoutMS,
		})
		if err != nil {
			return nil, err
		}
		descriptor, model, err := registration.DescribeConfiguration(raw)
		if err != nil {
			return nil, fmt.Errorf("describe scenario speaker identity: %w", err)
		}
		return &scenarioconversation.ApplicationSpeakerIdentitySelection{
			Reference: reference, Artifact: registration.Artifact,
			Descriptor: descriptor, Model: model, Configuration: raw,
		}, nil
	}
	return nil, fmt.Errorf("scenario speaker identity inventory is missing %q", reference)
}

func scenarioProfileModelSelection(
	inventory serveScenarioProviders, options scenarioProfileOptions, speechAuthority string,
) (scenarioconversation.ApplicationModelSelection, error) {
	reference := serveProviderReference("model", options.modelProvider)
	for _, registration := range inventory.Models {
		if registration.Reference != reference {
			continue
		}
		vision, retain, temperature := options.modelVision, options.modelRetainReason, options.modelTemperature
		raw, err := json.Marshal(serveModelConfiguration{
			FormatVersion: 1, Model: options.modelName, BaseURL: options.modelURL,
			Effort: options.modelEffort, Vision: &vision, Reason: options.modelReason,
			RetainReasoning: &retain, Temperature: &temperature,
			RequestTimeoutMS: options.modelTimeoutMS, SpeechAuthority: speechAuthority,
		})
		if err != nil {
			return scenarioconversation.ApplicationModelSelection{}, err
		}
		descriptor, err := registration.DescribeConfiguration(raw)
		if err != nil {
			return scenarioconversation.ApplicationModelSelection{}, fmt.Errorf("describe scenario model: %w", err)
		}
		return scenarioconversation.ApplicationModelSelection{
			Reference: reference, Artifact: registration.Artifact,
			Descriptor: descriptor, Configuration: raw,
		}, nil
	}
	return scenarioconversation.ApplicationModelSelection{}, fmt.Errorf("scenario model inventory is missing %q", reference)
}

func scenarioProfilePolicySelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
) (scenarioconversation.ApplicationPolicySelection, error) {
	reference := serveProviderReference("policy", options.policyProvider)
	for _, registration := range inventory.Policies {
		if registration.Reference != reference {
			continue
		}
		guided, vision := options.policyGuided, options.policyVision
		raw, err := json.Marshal(servePolicyConfiguration{
			FormatVersion: servePolicyConfigurationVersion, Model: options.policyModel, BaseURL: options.policyURL,
			RequestTimeoutMS: options.policyTimeoutMS, Vision: &vision, GuidedChoice: &guided,
			Reasoning: options.policyReasoning, TokenEnvironment: options.policyTokenEnv,
		})
		if err != nil {
			return scenarioconversation.ApplicationPolicySelection{}, err
		}
		descriptor, err := registration.DescribeConfiguration(raw)
		if err != nil {
			return scenarioconversation.ApplicationPolicySelection{}, fmt.Errorf("describe scenario semantic policy: %w", err)
		}
		return scenarioconversation.ApplicationPolicySelection{
			Reference: reference, Artifact: registration.Artifact,
			Descriptor: descriptor, Configuration: raw,
		}, nil
	}
	return scenarioconversation.ApplicationPolicySelection{}, fmt.Errorf("scenario semantic policy inventory is missing %q", reference)
}

func scenarioProfileTTSSelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
) (scenarioconversation.ApplicationTTSSelection, error) {
	reference := serveProviderReference("tts", options.ttsProvider)
	for _, registration := range inventory.TTS {
		if registration.Reference != reference {
			continue
		}
		wrap := options.ttsSentenceWrap
		raw, err := json.Marshal(serveTTSConfiguration{
			FormatVersion: 1, Model: options.ttsModel, BaseURL: options.ttsURL,
			Voice: options.ttsVoice, Language: options.ttsLanguage,
			OutputSampleRateHz: 24_000, RequestTimeoutMS: options.ttsTimeoutMS,
			SentenceWrapping: &wrap, SentenceMinimumRunes: options.ttsSentenceMinimum,
		})
		if err != nil {
			return scenarioconversation.ApplicationTTSSelection{}, err
		}
		descriptor, voice, err := registration.DescribeConfiguration(raw)
		if err != nil {
			return scenarioconversation.ApplicationTTSSelection{}, fmt.Errorf("describe scenario TTS: %w", err)
		}
		return scenarioconversation.ApplicationTTSSelection{
			Reference: reference, Artifact: registration.Artifact,
			Descriptor: descriptor, Voice: voice, Configuration: raw,
		}, nil
	}
	return scenarioconversation.ApplicationTTSSelection{}, fmt.Errorf("scenario TTS inventory is missing %q", reference)
}

func scenarioProfileWordTimingSelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
) (*scenarioconversation.ApplicationWordTimingSelection, error) {
	if strings.TrimSpace(options.wordTimingsURL) == "" {
		return nil, nil
	}
	reference := serveProviderReference("word-timing", "openai-compatible")
	for _, registration := range inventory.WordTiming {
		if registration.Reference != reference {
			continue
		}
		raw, err := json.Marshal(serveWordTimingConfiguration{
			FormatVersion:    serveProviderConfigurationVersion,
			Endpoint:         options.wordTimingsURL,
			Model:            options.wordTimingsModel,
			Language:         options.wordTimingsLanguage,
			RequestTimeoutMS: options.wordTimingsTimeoutMS,
		})
		if err != nil {
			return nil, err
		}
		if registration.ValidateConfiguration == nil {
			return nil, errors.New("scenario word-timing inventory has no configuration validator")
		}
		if err := registration.ValidateConfiguration(raw); err != nil {
			return nil, fmt.Errorf("describe scenario word timing: %w", err)
		}
		return &scenarioconversation.ApplicationWordTimingSelection{
			Reference: reference, Artifact: registration.Artifact,
			IntervalMS: options.wordTimingsIntervalMS, Configuration: raw,
		}, nil
	}
	return nil, fmt.Errorf("scenario word-timing inventory is missing %q", reference)
}

func writeCreateOnlyLaunchProfile(path string, payload []byte) (resultErr error) {
	if len(payload) == 0 || len(path) == 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == filepath.Dir(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("launch-profile output must be a clean absolute non-root path")
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	for current := parentPath; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("launch-profile output parent contains a symlink or non-directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return errors.New("open launch-profile output parent")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close launch-profile output parent"))
		}
	}()
	parentIdentity, err := root.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if err != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentIdentity, visibleParent) {
		return errors.New("launch-profile output parent changed while opening")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create launch-profile output exclusively")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = root.Remove(name)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return errors.New("write launch-profile output")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync launch-profile output")
	}
	if err := fileidentity.RequireSingleLink(file); err != nil {
		return errors.New("launch-profile output acquired an external link")
	}
	if err := file.Close(); err != nil {
		return errors.New("close launch-profile output")
	}
	retained, err := root.ReadFile(name)
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("reopen exact launch-profile output")
	}
	directory, err := root.Open(".")
	if err != nil {
		return errors.New("open launch-profile output parent for sync")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("sync launch-profile output parent")
	}
	after, err := root.Lstat(name)
	visibleParent, visibleErr = os.Lstat(parentPath)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		after.Mode().Perm()&0o077 != 0 || visibleErr != nil ||
		visibleParent.Mode()&os.ModeSymlink != 0 || !visibleParent.IsDir() ||
		!os.SameFile(parentIdentity, visibleParent) {
		return errors.New("launch-profile output identity changed during publication")
	}
	complete = true
	return nil
}
