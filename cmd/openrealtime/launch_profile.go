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
	"strings"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/computeruse"
	scenarioconversation "github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

const launchProfileUsage = `usage: openrealtime profile scenario [flags]

Freeze one strict scenario graph/server profile against the exact running
executable and explicit provider configurations. The output is create-only and
contains no credential. Use this same executable to serve the resulting file.`

type scenarioProfileOptions struct {
	out      string
	name     string
	revision uint64

	asrProvider        string
	asrModel           string
	asrURL             string
	asrLanguage        string
	asrPartialMS       int64
	asrEndpointingMS   int64
	asrTimeoutMS       int64
	asrCadenceMS       int64
	modelProvider      string
	modelName          string
	modelURL           string
	modelEffort        string
	modelVision        bool
	modelReason        string
	modelRetainReason  bool
	modelTemperature   float64
	modelTimeoutMS     int64
	ttsProvider        string
	ttsModel           string
	ttsURL             string
	ttsVoice           string
	ttsLanguage        string
	ttsTimeoutMS       int64
	ttsSentenceWrap    bool
	ttsSentenceMinimum int

	gateThreshold      float64
	gatePrefixMS       int
	gateSilenceMS      int
	gateSpeechMS       int
	maxOutputTokens    int
	serverTokenEnv     string
	inspectionTokenTTL uint64
	maxAudioFrameBytes int
}

func defaultScenarioProfileOptions() scenarioProfileOptions {
	return scenarioProfileOptions{
		name: "openrealtime.launch.scenario-local", revision: 1,
		asrProvider: "sensevoice", asrModel: "iic/SenseVoiceSmall",
		asrURL: "http://127.0.0.1:8002/v1", asrPartialMS: 200,
		asrTimeoutMS: 30_000, asrCadenceMS: 200,
		modelProvider: "vllm", modelName: "qwen-fast",
		modelURL: "http://127.0.0.1:8000/v1", modelEffort: "minimal",
		modelVision: true, modelReason: "off", modelTemperature: 0,
		modelTimeoutMS: 30_000,
		ttsProvider:    "fish-audio", ttsModel: "fishaudio/fish-speech-1.5",
		ttsURL: "http://127.0.0.1:8123/v1/tts", ttsVoice: "default",
		ttsTimeoutMS: 30_000, ttsSentenceWrap: true, ttsSentenceMinimum: 12,
		gateThreshold: 0.5, gatePrefixMS: 300, gateSilenceMS: 500,
		gateSpeechMS: 120, maxOutputTokens: 4096,
		inspectionTokenTTL: 30_000, maxAudioFrameBytes: 1 << 20,
	}
}

func runLaunchProfile(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New(launchProfileUsage)
	}
	switch strings.ToLower(strings.TrimSpace(arguments[0])) {
	case "scenario":
		return runScenarioProfileFreeze(arguments[1:], output)
	case "help", "-h", "--help":
		fmt.Fprintln(output, launchProfileUsage)
		return nil
	default:
		return fmt.Errorf("profile kind must be scenario, got %q\n%s", arguments[0], launchProfileUsage)
	}
}

func runScenarioProfileFreeze(arguments []string, output io.Writer) error {
	options := defaultScenarioProfileOptions()
	flags := flag.NewFlagSet("openrealtime profile scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&options.out, "out", "", "new absolute launch-profile YAML path")
	flags.StringVar(&options.name, "name", options.name, "immutable profile name")
	flags.Uint64Var(&options.revision, "revision", options.revision, "positive profile revision")
	flags.StringVar(&options.asrProvider, "asr-provider", options.asrProvider, "installed ASR provider plugin")
	flags.StringVar(&options.asrModel, "asr-model", options.asrModel, "exact ASR model")
	flags.StringVar(&options.asrURL, "asr-url", options.asrURL, "exact ASR base URL")
	flags.StringVar(&options.asrLanguage, "asr-language", options.asrLanguage, "ASR language hint")
	flags.Int64Var(&options.asrPartialMS, "asr-partial-ms", options.asrPartialMS, "batch partial-transcription interval")
	flags.Int64Var(&options.asrEndpointingMS, "asr-endpointing-ms", options.asrEndpointingMS, "streaming-provider endpointing")
	flags.Int64Var(&options.asrTimeoutMS, "asr-timeout-ms", options.asrTimeoutMS, "ASR request timeout")
	flags.Int64Var(&options.asrCadenceMS, "asr-cadence-ms", options.asrCadenceMS, "ASR graph cadence")
	flags.StringVar(&options.modelProvider, "model-provider", options.modelProvider, "installed text-model provider plugin")
	flags.StringVar(&options.modelName, "model", options.modelName, "exact text model")
	flags.StringVar(&options.modelURL, "model-url", options.modelURL, "exact text-model base URL")
	flags.StringVar(&options.modelEffort, "model-effort", options.modelEffort, "canonical reasoning effort")
	flags.BoolVar(&options.modelVision, "model-vision", options.modelVision, "declare exact image-input support")
	flags.StringVar(&options.modelReason, "model-reason", options.modelReason, "reasoning control: off, on, or default")
	flags.BoolVar(&options.modelRetainReason, "model-retain-reasoning", options.modelRetainReason, "retain reasoning in the trajectory")
	flags.Float64Var(&options.modelTemperature, "model-temperature", options.modelTemperature, "sampling temperature")
	flags.Int64Var(&options.modelTimeoutMS, "model-timeout-ms", options.modelTimeoutMS, "text-model request timeout")
	flags.StringVar(&options.ttsProvider, "tts-provider", options.ttsProvider, "installed TTS provider plugin")
	flags.StringVar(&options.ttsModel, "tts-model", options.ttsModel, "exact TTS model")
	flags.StringVar(&options.ttsURL, "tts-url", options.ttsURL, "exact TTS endpoint")
	flags.StringVar(&options.ttsVoice, "tts-voice", options.ttsVoice, "fixed TTS voice")
	flags.StringVar(&options.ttsLanguage, "tts-language", options.ttsLanguage, "TTS language hint")
	flags.Int64Var(&options.ttsTimeoutMS, "tts-timeout-ms", options.ttsTimeoutMS, "TTS request timeout")
	flags.BoolVar(&options.ttsSentenceWrap, "tts-sentence-wrapping", options.ttsSentenceWrap, "synthesize prepared text by sentence")
	flags.IntVar(&options.ttsSentenceMinimum, "tts-sentence-minimum-runes", options.ttsSentenceMinimum, "minimum sentence size")
	flags.Float64Var(&options.gateThreshold, "gate-threshold", options.gateThreshold, "acoustic energy threshold")
	flags.IntVar(&options.gatePrefixMS, "gate-prefix-ms", options.gatePrefixMS, "acoustic prefix padding")
	flags.IntVar(&options.gateSilenceMS, "gate-silence-ms", options.gateSilenceMS, "silence that closes one utterance")
	flags.IntVar(&options.gateSpeechMS, "gate-speech-ms", options.gateSpeechMS, "minimum admitted speech")
	flags.IntVar(&options.maxOutputTokens, "max-output-tokens", options.maxOutputTokens, "model output-token bound")
	flags.StringVar(&options.serverTokenEnv, "token-env", options.serverTokenEnv, "optional gateway bearer-token environment name")
	flags.Uint64Var(&options.inspectionTokenTTL, "inspection-token-ttl-ms", options.inspectionTokenTTL, "runtime-inspection token lifetime")
	flags.IntVar(&options.maxAudioFrameBytes, "max-audio-frame-bytes", options.maxAudioFrameBytes, "Realtime audio-frame bound")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(options.out) == "" {
		return errors.New("profile scenario requires -out and accepts flags only")
	}
	profile, err := freezeProductionScenarioProfile(context.Background(), options)
	if err != nil {
		return err
	}
	payload, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		return fmt.Errorf("encode scenario launch profile: %w", err)
	}
	if err := writeCreateOnlyLaunchProfile(options.out, payload); err != nil {
		return err
	}
	fmt.Fprintf(output, "wrote %s\n", options.out)
	fmt.Fprintf(output, "profile     %s@%d\n", profile.Name, profile.Revision)
	fmt.Fprintf(output, "fingerprint %s\n", profile.Fingerprint)
	fmt.Fprintf(output, "plan        %s\n", profile.Plan.PlanFingerprint)
	fmt.Fprintf(output, "executable  %s\n", profile.Server.GatewayArtifact.Digest)
	fmt.Fprintf(output, "providers   asr=%s/%s model=%s/%s tts=%s/%s\n",
		options.asrProvider, options.asrModel, options.modelProvider, options.modelName,
		options.ttsProvider, options.ttsModel)
	return nil
}

func freezeProductionScenarioProfile(
	ctx context.Context, options scenarioProfileOptions,
) (launchprofile.Document, error) {
	if ctx == nil {
		return launchprofile.Document{}, errors.New("freeze production scenario profile: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return launchprofile.Document{}, err
	}
	artifacts, err := executableServeProfileArtifacts()
	if err != nil {
		return launchprofile.Document{}, err
	}
	inventory, err := newServeScenarioProviders(artifacts)
	if err != nil {
		return launchprofile.Document{}, err
	}
	host, err := newServeProfileHost(artifacts, inventory)
	if err != nil {
		return launchprofile.Document{}, err
	}
	asr, err := scenarioProfileASRSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, err
	}
	model, err := scenarioProfileModelSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, err
	}
	tts, err := scenarioProfileTTSSelection(inventory, options)
	if err != nil {
		return launchprofile.Document{}, err
	}
	contract, err := graphnative.BuildContract()
	if err != nil {
		return launchprofile.Document{}, err
	}
	application := scenarioconversation.ApplicationConfig{
		FormatVersion: scenarioconversation.ApplicationFormatVersion,
		ASR:           asr, Model: model, TTS: tts,
		Tools: []scenarioconversation.ToolDeclaration{{
			Name: "press_key", Description: "Send one reviewed keypad digit to the open call.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"digit":{"type":"string"}},"required":["digit"]}`),
			Confirm:    legacyaction.ConfirmNever,
		}},
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
		MaxOutputTokens: options.maxOutputTokens,
	}
	delegatePayload, err := json.Marshal(application)
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("encode scenario application: %w", err)
	}
	configuration, err := graphnative.FreezeApplicationConfiguration(
		contract, host.Delegate, delegatePayload,
	)
	if err != nil {
		return launchprofile.Document{}, fmt.Errorf("freeze scenario application: %w", err)
	}
	return graphnative.FreezeLaunchProfile(ctx, graphnative.LaunchProfileConfig{
		Name: options.name, Revision: options.revision, Contract: contract,
		Application: host.ScenarioSuite, Configuration: configuration,
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.scenario-graph-native", ProfileRevision: 1,
			ProviderArtifact: artifacts.ScenarioProvider, GatewayArtifact: artifacts.Gateway,
			TokenEnvironment: options.serverTokenEnv, Model: options.modelName,
			TranscriptionModel: options.asrModel, ValidateWire: true,
			InspectionTokenTTLMS: options.inspectionTokenTTL,
			MaxAudioFrameBytes:   options.maxAudioFrameBytes,
			VideoLimits:          openrealtime.DefaultLimits(),
		},
	})
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
			Language: options.asrLanguage, PartialIntervalMS: options.asrPartialMS,
			EndpointingMS: options.asrEndpointingMS, RequestTimeoutMS: options.asrTimeoutMS,
			CadenceMS: options.asrCadenceMS,
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

func scenarioProfileModelSelection(
	inventory serveScenarioProviders, options scenarioProfileOptions,
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
			RequestTimeoutMS: options.modelTimeoutMS,
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
