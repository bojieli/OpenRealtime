package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/noisefilter"
	"github.com/bojieli/OpenRealtime/perception/voices"
	"github.com/bojieli/OpenRealtime/spoken"
)

const (
	ApplicationReference          = "application.openrealtime.scenario-conversation.v1"
	ApplicationFormatVersion      = uint64(9)
	maximumApplicationConfigBytes = 4 << 20
	maximumApplicationProviders   = 65_536
)

// ApplicationASRSelection is a serializable exact host-plugin selection. Its
// reference is a host inventory key; the application maps the matched factory
// to the graph's fixed ASRReference only after identity verification.
type ApplicationASRSelection struct {
	Reference     string                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity `json:"artifact"`
	Descriptor    v1.Descriptor            `json:"descriptor"`
	Configuration json.RawMessage          `json:"configuration,omitempty"`
}

// ApplicationSpeakerIdentitySelection pins the optional session-local
// speaker embedder. Descriptor identifies the adapter contract; Model pins the
// independently deployed embedding weights.
type ApplicationSpeakerIdentitySelection struct {
	Reference     string                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity `json:"artifact"`
	Descriptor    v1.Descriptor            `json:"descriptor"`
	Model         string                   `json:"model"`
	Configuration json.RawMessage          `json:"configuration,omitempty"`
}

// ApplicationModelSelection pins one continuation provider without retaining
// a credential, client, or factory in profile data.
type ApplicationModelSelection struct {
	Reference     string                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity `json:"artifact"`
	Descriptor    continuation.Descriptor  `json:"descriptor"`
	Configuration json.RawMessage          `json:"configuration,omitempty"`
}

// ApplicationPolicySelection pins one narrow enumerated semantic decider.
// Configuration is plugin-owned and may name a credential environment
// variable, but must never contain credential bytes.
type ApplicationPolicySelection struct {
	Reference     string                                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity                 `json:"artifact"`
	Descriptor    policyelements.SemanticDeciderDescriptor `json:"descriptor"`
	Configuration json.RawMessage                          `json:"configuration,omitempty"`
}

// ApplicationTTSSelection pins one speech provider and its fixed voice.
type ApplicationTTSSelection struct {
	Reference     string                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity `json:"artifact"`
	Descriptor    v1.Descriptor            `json:"descriptor"`
	Voice         string                   `json:"voice"`
	Configuration json.RawMessage          `json:"configuration,omitempty"`
}

// ApplicationWordTimingSelection pins the optional recogniser used only to
// locate word boundaries in the agent's own audio. Configuration remains
// plugin-owned; IntervalMS is application policy because it determines how
// often playback asks the provider to refresh a still-growing utterance.
type ApplicationWordTimingSelection struct {
	Reference     string                   `json:"reference"`
	Artifact      inspect.ArtifactIdentity `json:"artifact"`
	IntervalMS    int64                    `json:"interval_ms"`
	Configuration json.RawMessage          `json:"configuration,omitempty"`
}

// ApplicationGateSelection is the JSON-stable acoustic admission policy.
type ApplicationGateSelection struct {
	Threshold         float64 `json:"threshold"`
	PrefixPaddingMS   int     `json:"prefix_padding_ms"`
	SilenceDurationMS int     `json:"silence_duration_ms"`
	SpeechDurationMS  int     `json:"speech_duration_ms"`
}

func (selection ApplicationGateSelection) gateConfig() perception.GateConfig {
	return perception.GateConfig{
		Threshold: selection.Threshold, PrefixPaddingMS: selection.PrefixPaddingMS,
		SilenceDurationMS: selection.SilenceDurationMS, SpeechDurationMS: selection.SpeechDurationMS,
	}
}

// ApplicationConfig is the complete plugin-owned, resource-free selection
// carried by a generic graph launch profile.
type ApplicationConfig struct {
	NoiseFilter             *noisefilter.Config                  `json:"noise_filter,omitempty"`
	FormatVersion           uint64                               `json:"format_version"`
	Architecture            legacy.ArchitectureIdentity          `json:"architecture"`
	ASR                     ApplicationASRSelection              `json:"asr"`
	SpeakerIdentity         *ApplicationSpeakerIdentitySelection `json:"speaker_identity,omitempty"`
	Policy                  ApplicationPolicySelection           `json:"policy"`
	SemanticAdmission       SemanticAdmissionSelection           `json:"semantic_admission"`
	Model                   ApplicationModelSelection            `json:"model"`
	SilentModel             ApplicationModelSelection            `json:"silent_model"`
	TTS                     ApplicationTTSSelection              `json:"tts"`
	WordTiming              *ApplicationWordTimingSelection      `json:"word_timing,omitempty"`
	Tools                   []ToolDeclaration                    `json:"tools"`
	Target                  computeruse.Target                   `json:"target"`
	Gate                    ApplicationGateSelection             `json:"gate"`
	Media                   MediaLimits                          `json:"media"`
	MaxOutputTokens         int                                  `json:"max_output_tokens"`
	ContinuationInstruction string                               `json:"continuation_instruction,omitempty"`
}

// DecodeApplicationConfig strictly decodes and validates an exact selection.
// It performs no factory invocation, environment lookup, secret resolution,
// graph mount, or listener acquisition.
func DecodeApplicationConfig(source json.RawMessage) (ApplicationConfig, error) {
	if len(source) == 0 || len(source) > maximumApplicationConfigBytes {
		return ApplicationConfig{}, fmt.Errorf(
			"scenario conversation application configuration has %d bytes; expected 1..%d",
			len(source), maximumApplicationConfigBytes,
		)
	}
	var config ApplicationConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return ApplicationConfig{}, fmt.Errorf("decode scenario conversation application configuration: %w", err)
	}
	return normalizeApplicationConfig(config)
}

func normalizeApplicationConfig(source ApplicationConfig) (ApplicationConfig, error) {
	if source.NoiseFilter != nil {
		if err := source.NoiseFilter.Validate(); err != nil {
			return ApplicationConfig{}, err
		}
	}
	config := cloneApplicationConfig(source)
	if config.FormatVersion != ApplicationFormatVersion {
		return ApplicationConfig{}, fmt.Errorf(
			"scenario conversation application configuration uses format %d, want %d",
			config.FormatVersion, ApplicationFormatVersion,
		)
	}
	if _, err := resolveScenarioArchitecture(config.Architecture); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationASR(config.ASR); err != nil {
		return ApplicationConfig{}, err
	}
	architecture, err := resolveScenarioArchitecture(config.Architecture)
	if err != nil {
		return ApplicationConfig{}, err
	}
	expectsSpeakerIdentity := architecture.Interaction.EvidenceCapabilities != nil &&
		architecture.Interaction.EvidenceCapabilities.SpeakerIdentity
	if expectsSpeakerIdentity != (config.SpeakerIdentity != nil) {
		return ApplicationConfig{}, fmt.Errorf(
			"scenario conversation application architecture speaker_identity=%t but speaker identity selection present=%t",
			expectsSpeakerIdentity, config.SpeakerIdentity != nil,
		)
	}
	if config.SpeakerIdentity != nil {
		if err := validateApplicationSpeakerIdentity(*config.SpeakerIdentity); err != nil {
			return ApplicationConfig{}, err
		}
	}
	if err := validateApplicationPolicy(config.Policy); err != nil {
		return ApplicationConfig{}, err
	}
	semanticAdmission, err := normalizeSemanticAdmissionSelection(
		config.SemanticAdmission, config.Policy.Descriptor,
	)
	if err != nil {
		return ApplicationConfig{}, err
	}
	config.SemanticAdmission = semanticAdmission
	if err := validateApplicationModel(config.Model, continuation.SpeechAuthorityVoice); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationModel(config.SilentModel, continuation.SpeechAuthoritySilent); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationTTS(config.TTS); err != nil {
		return ApplicationConfig{}, err
	}
	if config.WordTiming != nil {
		if err := validateApplicationWordTiming(*config.WordTiming); err != nil {
			return ApplicationConfig{}, err
		}
	}
	config.Tools, err = normalizeToolDeclarations(config.Tools)
	if err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateActionTarget(config.Target, config.Tools, "scenario conversation application"); err != nil {
		return ApplicationConfig{}, err
	}
	if _, err := perception.NewEnergyGate(config.Gate.gateConfig(), 24_000); err != nil {
		return ApplicationConfig{}, fmt.Errorf("scenario conversation application acoustic gate: %w", err)
	}
	config.Media, err = config.Media.normalized()
	if err != nil {
		return ApplicationConfig{}, err
	}
	if config.MaxOutputTokens < 1 || config.MaxOutputTokens > 1_000_000 {
		return ApplicationConfig{}, errors.New(
			"scenario conversation application max_output_tokens must be between 1 and 1000000",
		)
	}
	if err := validateContinuationInstruction(config.ContinuationInstruction); err != nil {
		return ApplicationConfig{}, err
	}
	return config, nil
}

func validateApplicationASR(selection ApplicationASRSelection) error {
	if !canonicalIdentity(selection.Reference) {
		return errors.New("scenario conversation application ASR reference is not canonical")
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application ASR artifact: %w", err)
	}
	if err := selection.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application ASR descriptor: %w", err)
	}
	return nil
}

func validateApplicationSpeakerIdentity(selection ApplicationSpeakerIdentitySelection) error {
	if !canonicalIdentity(selection.Reference) {
		return errors.New("scenario conversation application speaker identity reference is not canonical")
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application speaker identity artifact: %w", err)
	}
	if err := selection.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application speaker identity descriptor: %w", err)
	}
	if !canonicalIdentity(selection.Model) {
		return errors.New("scenario conversation application speaker identity model is not canonical")
	}
	return nil
}

func validateApplicationPolicy(selection ApplicationPolicySelection) error {
	if !canonicalIdentity(selection.Reference) {
		return errors.New("scenario conversation application semantic policy reference is not canonical")
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application semantic policy artifact: %w", err)
	}
	if err := selection.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application semantic policy descriptor: %w", err)
	}
	return nil
}

func validateApplicationModel(
	selection ApplicationModelSelection, speechAuthority continuation.SpeechAuthority,
) error {
	if !canonicalIdentity(selection.Reference) {
		return errors.New("scenario conversation application model reference is not canonical")
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application model artifact: %w", err)
	}
	if err := continuation.ValidateDescriptor(selection.Descriptor); err != nil {
		return fmt.Errorf("scenario conversation application model descriptor: %w", err)
	}
	if selection.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose ||
		selection.Descriptor.EffectiveSpeechAuthority() != speechAuthority {
		return fmt.Errorf("scenario conversation application model must be proposal-only and %s-authoritative", speechAuthority)
	}
	return nil
}

func validateApplicationTTS(selection ApplicationTTSSelection) error {
	if !canonicalIdentity(selection.Reference) {
		return errors.New("scenario conversation application TTS reference is not canonical")
	}
	if err := selection.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application TTS artifact: %w", err)
	}
	if err := selection.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application TTS descriptor: %w", err)
	}
	if !selection.Descriptor.Capabilities.Has(v1.CapabilityPCM16Output) {
		return errors.New("scenario conversation application TTS must provide PCM16 output")
	}
	if !canonicalIdentity(selection.Voice) {
		return errors.New("scenario conversation application TTS voice is not canonical")
	}
	return nil
}

func validateApplicationWordTiming(selection ApplicationWordTimingSelection) error {
	if err := validateApplicationProviderIdentity(
		selection.Reference, selection.Artifact, "word-timing",
	); err != nil {
		return err
	}
	if selection.IntervalMS < 1 || selection.IntervalMS > int64(time.Hour/time.Millisecond) {
		return fmt.Errorf(
			"scenario conversation application word-timing interval_ms must be between 1 and %d",
			time.Hour/time.Millisecond,
		)
	}
	return nil
}

// ASRFactoryRegistration is one broad host inventory entry. The factory is
// retained but remains unopened until the selected session starts.
type ASRFactoryRegistration struct {
	ApplicationASRSelection
	Factory                func(context.Context, legacy.Options) (v1.PerceptionProvider, error)
	DescribeConfiguration  func(json.RawMessage) (v1.Descriptor, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (v1.PerceptionProvider, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

type SpeakerIdentityFactoryRegistration struct {
	ApplicationSpeakerIdentitySelection
	Factory                func(context.Context, legacy.Options) (voices.Embedder, error)
	DescribeConfiguration  func(json.RawMessage) (v1.Descriptor, string, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (voices.Embedder, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

type ModelFactoryRegistration struct {
	ApplicationModelSelection
	Factory                func(context.Context, legacy.Options) (continuation.Provider, error)
	DescribeConfiguration  func(json.RawMessage) (continuation.Descriptor, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (continuation.Provider, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

type PolicyFactoryRegistration struct {
	ApplicationPolicySelection
	Factory                func(context.Context, legacy.Options) (policyelements.SemanticDecider, error)
	DescribeConfiguration  func(json.RawMessage) (policyelements.SemanticDeciderDescriptor, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (policyelements.SemanticDecider, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

type TTSFactoryRegistration struct {
	ApplicationTTSSelection
	Factory                func(context.Context, legacy.Options) (v1.SpeechProvider, error)
	DescribeConfiguration  func(json.RawMessage) (v1.Descriptor, string, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (v1.SpeechProvider, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

// WordTimingFactoryRegistration has no graph descriptor because the provider
// emits no graph payload. Exact configuration bytes and the linked artifact
// still remain profile-pinned, and the factory stays unopened until a session
// constructs its playback sink.
type WordTimingFactoryRegistration struct {
	Reference              string
	Artifact               inspect.ArtifactIdentity
	Factory                func(context.Context, legacy.Options) (spoken.Aligner, error)
	ValidateConfiguration  func(json.RawMessage) error
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (spoken.Aligner, error)
	ReadinessConfiguration func(context.Context, json.RawMessage) error
}

// ApplicationRegistrationConfig is process-private executable inventory. The
// supplied artifacts are host/build identities; this package never invents a
// provider, adapter, or dependency artifact from configuration bytes.
type ApplicationRegistrationConfig struct {
	ApplicationArtifact inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity
	RuntimeArtifact     inspect.ArtifactIdentity
	DependencyArtifact  inspect.ArtifactIdentity
	ASR                 []ASRFactoryRegistration
	SpeakerIdentity     []SpeakerIdentityFactoryRegistration
	Policies            []PolicyFactoryRegistration
	Models              []ModelFactoryRegistration
	TTS                 []TTSFactoryRegistration
	WordTiming          []WordTimingFactoryRegistration
}

// NewApplicationRegistration creates the generic strict-profile application
// registration for this graph. Resolution exact-matches every selected
// provider before it constructs the still-resource-free launch configuration.
func NewApplicationRegistration(
	source ApplicationRegistrationConfig,
	constructor func(PluginConfig) (graphlaunch.Config, error),
) (launchprofile.Registration, error) {
	if constructor == nil {
		return launchprofile.Registration{}, errors.New(
			"scenario conversation application registration requires a launch constructor",
		)
	}
	artifacts := []struct {
		label    string
		identity inspect.ArtifactIdentity
	}{
		{label: "application", identity: source.ApplicationArtifact},
		{label: "provider", identity: source.ProviderArtifact},
		{label: "runtime", identity: source.RuntimeArtifact},
		{label: "dependency", identity: source.DependencyArtifact},
	}
	for _, artifact := range artifacts {
		if err := artifact.identity.Validate(); err != nil {
			return launchprofile.Registration{}, fmt.Errorf(
				"scenario conversation %s artifact: %w", artifact.label, err,
			)
		}
	}
	if len(source.ASR) == 0 || len(source.Policies) == 0 || len(source.Models) == 0 || len(source.TTS) == 0 ||
		len(source.ASR) > maximumApplicationProviders || len(source.Policies) > maximumApplicationProviders || len(source.Models) > maximumApplicationProviders ||
		len(source.TTS) > maximumApplicationProviders || len(source.SpeakerIdentity) > maximumApplicationProviders ||
		len(source.WordTiming) > maximumApplicationProviders {
		return launchprofile.Registration{}, errors.New(
			"scenario conversation application registration requires bounded ASR, policy, model, and TTS inventories",
		)
	}
	asr, err := snapshotASRRegistrations(source.ASR)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	speakerIdentities, err := snapshotSpeakerIdentityRegistrations(source.SpeakerIdentity)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	policies, err := snapshotPolicyRegistrations(source.Policies)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	models, err := snapshotModelRegistrations(source.Models)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	tts, err := snapshotTTSRegistrations(source.TTS)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	wordTimings, err := snapshotWordTimingRegistrations(source.WordTiming)
	if err != nil {
		return launchprofile.Registration{}, err
	}
	runtimeArtifact, dependencyArtifact := source.RuntimeArtifact, source.DependencyArtifact
	registration := launchprofile.Registration{
		Reference: ApplicationReference, Artifact: source.ApplicationArtifact,
		ProviderArtifact: source.ProviderArtifact,
		Factory: func(ctx context.Context, payload json.RawMessage) (graphlaunch.Config, error) {
			if ctx == nil {
				return graphlaunch.Config{}, errors.New("resolve scenario conversation application: nil context")
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			config, err := DecodeApplicationConfig(payload)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			asrRegistration, found := asr[config.ASR.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation ASR registry is missing %q", config.ASR.Reference,
				)
			}
			if asrRegistration.Artifact != config.ASR.Artifact {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation ASR %q artifact or descriptor drifted", config.ASR.Reference,
				)
			}
			asrDescriptor, asrFactory, asrReady, err := resolveASRRegistration(asrRegistration, config.ASR)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			var speakerPlugin *SpeakerIdentityPlugin
			var speakerReady func(context.Context) error
			if config.SpeakerIdentity != nil {
				speakerRegistration, found := speakerIdentities[config.SpeakerIdentity.Reference]
				if !found {
					return graphlaunch.Config{}, fmt.Errorf(
						"scenario conversation speaker identity registry is missing %q", config.SpeakerIdentity.Reference,
					)
				}
				if speakerRegistration.Artifact != config.SpeakerIdentity.Artifact {
					return graphlaunch.Config{}, fmt.Errorf(
						"scenario conversation speaker identity %q artifact drifted", config.SpeakerIdentity.Reference,
					)
				}
				speakerDescriptor, speakerModel, speakerFactory, ready, resolveErr :=
					resolveSpeakerIdentityRegistration(speakerRegistration, *config.SpeakerIdentity)
				if resolveErr != nil {
					return graphlaunch.Config{}, resolveErr
				}
				speakerReady = ready
				speakerPlugin = &SpeakerIdentityPlugin{
					Reference: SpeakerIdentityReference, Artifact: speakerRegistration.Artifact,
					Descriptor: cloneV1Descriptor(speakerDescriptor), Model: speakerModel,
					Factory: speakerFactory,
				}
			}
			policyRegistration, found := policies[config.Policy.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation semantic policy registry is missing %q", config.Policy.Reference,
				)
			}
			if policyRegistration.Artifact != config.Policy.Artifact {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation semantic policy %q artifact or descriptor drifted", config.Policy.Reference,
				)
			}
			policyDescriptor, policyFactory, policyReady, err := resolvePolicyRegistration(
				policyRegistration, config.Policy,
			)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			modelRegistration, found := models[config.Model.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation model registry is missing %q", config.Model.Reference,
				)
			}
			if modelRegistration.Artifact != config.Model.Artifact {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation model %q artifact or descriptor drifted", config.Model.Reference,
				)
			}
			modelDescriptor, modelFactory, modelReady, err := resolveModelRegistration(modelRegistration, config.Model)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			silentModelRegistration, found := models[config.SilentModel.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation silent model registry is missing %q", config.SilentModel.Reference,
				)
			}
			if silentModelRegistration.Artifact != config.SilentModel.Artifact {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation silent model %q artifact or descriptor drifted", config.SilentModel.Reference,
				)
			}
			silentModelDescriptor, silentModelFactory, silentModelReady, err := resolveModelRegistration(
				silentModelRegistration, config.SilentModel,
			)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			ttsRegistration, found := tts[config.TTS.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation TTS registry is missing %q", config.TTS.Reference,
				)
			}
			if ttsRegistration.Artifact != config.TTS.Artifact {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation TTS %q artifact, descriptor, or voice drifted", config.TTS.Reference,
				)
			}
			ttsDescriptor, ttsVoice, ttsFactory, ttsReady, err := resolveTTSRegistration(
				ttsRegistration, config.TTS,
			)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			var wordTimingPlugin *WordTimingPlugin
			var wordTimingReady func(context.Context) error
			if config.WordTiming != nil {
				wordTimingRegistration, found := wordTimings[config.WordTiming.Reference]
				if !found {
					return graphlaunch.Config{}, fmt.Errorf(
						"scenario conversation word-timing registry is missing %q", config.WordTiming.Reference,
					)
				}
				if wordTimingRegistration.Artifact != config.WordTiming.Artifact {
					return graphlaunch.Config{}, fmt.Errorf(
						"scenario conversation word-timing %q artifact drifted", config.WordTiming.Reference,
					)
				}
				wordTimingFactory, ready, resolveErr := resolveWordTimingRegistration(
					wordTimingRegistration, *config.WordTiming,
				)
				if resolveErr != nil {
					return graphlaunch.Config{}, resolveErr
				}
				wordTimingReady = ready
				wordTimingPlugin = &WordTimingPlugin{
					Reference: WordTimingReference, Artifact: wordTimingRegistration.Artifact,
					Interval: time.Duration(config.WordTiming.IntervalMS) * time.Millisecond,
					Factory:  wordTimingFactory,
				}
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			architecture, err := resolveScenarioArchitecture(config.Architecture)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			resolved, constructorErr := constructor(PluginConfig{
				RuntimeArtifact: runtimeArtifact, DependencyArtifact: dependencyArtifact,
				NoiseFilter:  config.NoiseFilter,
				Architecture: architecture,
				ASR: ASRPlugin{Reference: ASRReference, Artifact: asrRegistration.Artifact,
					Descriptor: cloneV1Descriptor(asrDescriptor), Factory: asrFactory},
				SpeakerIdentity: speakerPlugin,
				Policy: PolicyPlugin{Reference: PolicyReference, Artifact: policyRegistration.Artifact,
					Descriptor: policyDescriptor, Factory: policyFactory},
				SemanticAdmission: config.SemanticAdmission,
				Model: ModelPlugin{Reference: ModelReference, Artifact: modelRegistration.Artifact,
					Descriptor: modelDescriptor, Factory: modelFactory},
				SilentModel: ModelPlugin{Reference: SilentModelReference, Artifact: silentModelRegistration.Artifact,
					Descriptor: silentModelDescriptor, Factory: silentModelFactory},
				TTS: TTSPlugin{Reference: TTSReference, Artifact: ttsRegistration.Artifact,
					Descriptor: cloneV1Descriptor(ttsDescriptor), Voice: ttsVoice,
					Factory: ttsFactory},
				WordTiming: wordTimingPlugin,
				Tools:      cloneToolDeclarations(config.Tools), Target: cloneTarget(config.Target),
				Gate: config.Gate.gateConfig(), Media: config.Media,
				MaxOutputTokens:         config.MaxOutputTokens,
				ContinuationInstruction: config.ContinuationInstruction,
			})
			if cause := context.Cause(ctx); cause != nil {
				return graphlaunch.Config{}, errors.Join(cause, constructorErr)
			}
			if constructorErr == nil {
				if config.NoiseFilter != nil {
					filterConfig := *config.NoiseFilter
					resolved.Readiness = append(resolved.Readiness, graphlaunch.ReadinessCheck{
						Name: "audio-noise-filter:" + filterConfig.ModelName(), Check: func(ctx context.Context) error {
							client, err := noisefilter.New(filterConfig)
							if err != nil {
								return err
							}
							defer client.Close()
							_, err = client.Process(ctx, make([]byte, 480), 24000)
							if err == nil && client.Status().State != "healthy" {
								return fmt.Errorf("audio filter readiness: %s", client.Status().Reason)
							}
							return err
						},
					})
				}

				if asrReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "asr:" + config.ASR.Reference, Check: asrReady})
				}
				if speakerReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "speaker-identity:" + config.SpeakerIdentity.Reference, Check: speakerReady})
				}
				if policyReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "policy:" + config.Policy.Reference, Check: policyReady})
				}
				if modelReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "model:voice:" + config.Model.Reference, Check: modelReady})
				}
				if silentModelReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "model:silent:" + config.SilentModel.Reference, Check: silentModelReady})
				}
				if ttsReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "tts:" + config.TTS.Reference, Check: ttsReady})
				}
				if wordTimingReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "word-timing:" + config.WordTiming.Reference, Check: wordTimingReady})
				}
			}
			return resolved, constructorErr
		},
	}
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration}); err != nil {
		return launchprofile.Registration{}, fmt.Errorf(
			"validate scenario conversation application registration: %w", err,
		)
	}
	return registration, nil
}

func resolveScenarioArchitecture(
	identity legacy.ArchitectureIdentity,
) (projectarch.Definition, error) {
	if strings.TrimSpace(identity.ID) == "" || identity.Revision <= 0 ||
		strings.TrimSpace(identity.Fingerprint) == "" {
		return projectarch.Definition{}, errors.New(
			"scenario conversation application requires an exact architecture identity",
		)
	}
	definition, err := projectarch.Default().LookupIdentity(identity)
	if err != nil {
		return projectarch.Definition{}, fmt.Errorf(
			"scenario conversation application architecture: %w", err,
		)
	}
	ref := definition.Ref()
	baseline := ref == (projectarch.Ref{ID: "cascade.composed-policy", Revision: 1})
	directVisual := ref == (projectarch.Ref{ID: "cascade.composed-policy-direct-visual", Revision: 1})
	directVisualSpeaker := ref == (projectarch.Ref{ID: "cascade.composed-policy-direct-visual-speaker", Revision: 1})
	if (!baseline && !directVisual && !directVisualSpeaker) ||
		definition.Interaction.Mode != projectarch.InteractionComposed ||
		definition.Interaction.EvidenceCapabilities == nil ||
		definition.Interaction.Control == nil {
		return projectarch.Definition{}, fmt.Errorf(
			"scenario conversation application architecture %s is not the exact composed semantic-policy controller",
			definition.Ref(),
		)
	}
	return definition, nil
}

func resolveSpeakerIdentityRegistration(
	registration SpeakerIdentityFactoryRegistration, selection ApplicationSpeakerIdentitySelection,
) (v1.Descriptor, string, func(context.Context, legacy.Options) (voices.Embedder, error), func(context.Context) error, error) {
	if registration.DescribeConfiguration == nil {
		if len(selection.Configuration) != 0 ||
			!sameV1Descriptor(registration.Descriptor, selection.Descriptor) ||
			registration.Model != selection.Model {
			return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
				"scenario conversation speaker identity %q artifact, descriptor, or model drifted", selection.Reference,
			)
		}
		return cloneV1Descriptor(registration.Descriptor), registration.Model, registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	descriptor, model, err := registration.DescribeConfiguration(configuration)
	if err != nil {
		return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
			"scenario conversation speaker identity %q configuration: %w", selection.Reference, err,
		)
	}
	if !sameV1Descriptor(descriptor, selection.Descriptor) || model != selection.Model {
		return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
			"scenario conversation speaker identity %q descriptor or model drifted from its configuration", selection.Reference,
		)
	}
	return cloneV1Descriptor(descriptor), model,
		func(ctx context.Context, options legacy.Options) (voices.Embedder, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		},
		func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func resolveASRRegistration(
	registration ASRFactoryRegistration, selection ApplicationASRSelection,
) (v1.Descriptor, func(context.Context, legacy.Options) (v1.PerceptionProvider, error), func(context.Context) error, error) {
	if registration.DescribeConfiguration == nil {
		if len(selection.Configuration) != 0 ||
			!sameV1Descriptor(registration.Descriptor, selection.Descriptor) {
			return v1.Descriptor{}, nil, nil, fmt.Errorf(
				"scenario conversation ASR %q artifact or descriptor drifted", selection.Reference,
			)
		}
		return cloneV1Descriptor(registration.Descriptor), registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	descriptor, err := registration.DescribeConfiguration(configuration)
	if err != nil {
		return v1.Descriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation ASR %q configuration: %w", selection.Reference, err,
		)
	}
	if !sameV1Descriptor(descriptor, selection.Descriptor) {
		return v1.Descriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation ASR %q descriptor drifted from its configuration", selection.Reference,
		)
	}
	return cloneV1Descriptor(descriptor),
		func(ctx context.Context, options legacy.Options) (v1.PerceptionProvider, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		},
		func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func resolvePolicyRegistration(
	registration PolicyFactoryRegistration, selection ApplicationPolicySelection,
) (policyelements.SemanticDeciderDescriptor, func(context.Context, legacy.Options) (policyelements.SemanticDecider, error), func(context.Context) error, error) {
	if registration.DescribeConfiguration == nil {
		if len(selection.Configuration) != 0 || registration.Descriptor != selection.Descriptor {
			return policyelements.SemanticDeciderDescriptor{}, nil, nil, fmt.Errorf(
				"scenario conversation semantic policy %q artifact or descriptor drifted", selection.Reference,
			)
		}
		return registration.Descriptor, registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	descriptor, err := registration.DescribeConfiguration(configuration)
	if err != nil {
		return policyelements.SemanticDeciderDescriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation semantic policy %q configuration: %w", selection.Reference, err,
		)
	}
	if descriptor != selection.Descriptor {
		return policyelements.SemanticDeciderDescriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation semantic policy %q descriptor drifted from its configuration", selection.Reference,
		)
	}
	return descriptor,
		func(ctx context.Context, options legacy.Options) (policyelements.SemanticDecider, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		},
		func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func resolveModelRegistration(
	registration ModelFactoryRegistration, selection ApplicationModelSelection,
) (continuation.Descriptor, func(context.Context, legacy.Options) (continuation.Provider, error), func(context.Context) error, error) {
	if registration.DescribeConfiguration == nil {
		if len(selection.Configuration) != 0 || registration.Descriptor != selection.Descriptor {
			return continuation.Descriptor{}, nil, nil, fmt.Errorf(
				"scenario conversation model %q artifact or descriptor drifted", selection.Reference,
			)
		}
		return registration.Descriptor, registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	descriptor, err := registration.DescribeConfiguration(configuration)
	if err != nil {
		return continuation.Descriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation model %q configuration: %w", selection.Reference, err,
		)
	}
	if descriptor != selection.Descriptor {
		return continuation.Descriptor{}, nil, nil, fmt.Errorf(
			"scenario conversation model %q descriptor drifted from its configuration", selection.Reference,
		)
	}
	return descriptor,
		func(ctx context.Context, options legacy.Options) (continuation.Provider, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		},
		func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func resolveTTSRegistration(
	registration TTSFactoryRegistration, selection ApplicationTTSSelection,
) (v1.Descriptor, string, func(context.Context, legacy.Options) (v1.SpeechProvider, error), func(context.Context) error, error) {
	if registration.DescribeConfiguration == nil {
		if len(selection.Configuration) != 0 ||
			!sameV1Descriptor(registration.Descriptor, selection.Descriptor) ||
			registration.Voice != selection.Voice {
			return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
				"scenario conversation TTS %q artifact, descriptor, or voice drifted", selection.Reference,
			)
		}
		return cloneV1Descriptor(registration.Descriptor), registration.Voice, registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	descriptor, voice, err := registration.DescribeConfiguration(configuration)
	if err != nil {
		return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
			"scenario conversation TTS %q configuration: %w", selection.Reference, err,
		)
	}
	if !sameV1Descriptor(descriptor, selection.Descriptor) || voice != selection.Voice {
		return v1.Descriptor{}, "", nil, nil, fmt.Errorf(
			"scenario conversation TTS %q descriptor or voice drifted from its configuration", selection.Reference,
		)
	}
	return cloneV1Descriptor(descriptor), voice,
		func(ctx context.Context, options legacy.Options) (v1.SpeechProvider, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		},
		func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func resolveWordTimingRegistration(
	registration WordTimingFactoryRegistration, selection ApplicationWordTimingSelection,
) (func(context.Context, legacy.Options) (spoken.Aligner, error), func(context.Context) error, error) {
	if registration.ValidateConfiguration == nil {
		if len(selection.Configuration) != 0 || registration.Factory == nil {
			return nil, nil, fmt.Errorf(
				"scenario conversation word-timing %q artifact or configuration drifted", selection.Reference,
			)
		}
		return registration.Factory, nil, nil
	}
	configuration := slices.Clone(selection.Configuration)
	if err := registration.ValidateConfiguration(configuration); err != nil {
		return nil, nil, fmt.Errorf(
			"scenario conversation word-timing %q configuration: %w", selection.Reference, err,
		)
	}
	return func(ctx context.Context, options legacy.Options) (spoken.Aligner, error) {
			return registration.FactoryConfiguration(ctx, options, slices.Clone(configuration))
		}, func(ctx context.Context) error {
			return registration.ReadinessConfiguration(ctx, slices.Clone(configuration))
		}, nil
}

func snapshotASRRegistrations(source []ASRFactoryRegistration) (map[string]ASRFactoryRegistration, error) {
	result := make(map[string]ASRFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(registration.Reference, registration.Artifact, "ASR"); err != nil {
			return nil, fmt.Errorf("scenario conversation ASR registration %d: %w", index, err)
		}
		parameterized := registration.DescribeConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.DescribeConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil ||
				len(registration.Configuration) != 0 || !zeroV1Descriptor(registration.Descriptor) {
				return nil, fmt.Errorf("scenario conversation ASR registration %d has a partial or mixed parameterized factory", index)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation ASR registration %d has a nil factory", index)
		} else if err := validateApplicationASR(registration.ApplicationASRSelection); err != nil {
			return nil, fmt.Errorf("scenario conversation ASR registration %d: %w", index, err)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation ASR reference %q is registered more than once", registration.Reference)
		}
		registration.Descriptor = cloneV1Descriptor(registration.Descriptor)
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotSpeakerIdentityRegistrations(
	source []SpeakerIdentityFactoryRegistration,
) (map[string]SpeakerIdentityFactoryRegistration, error) {
	result := make(map[string]SpeakerIdentityFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(registration.Reference, registration.Artifact, "speaker identity"); err != nil {
			return nil, fmt.Errorf("scenario conversation speaker identity registration %d: %w", index, err)
		}
		parameterized := registration.DescribeConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.DescribeConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil ||
				len(registration.Configuration) != 0 || !zeroV1Descriptor(registration.Descriptor) ||
				registration.Model != "" {
				return nil, fmt.Errorf("scenario conversation speaker identity registration %d has a partial or mixed parameterized factory", index)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation speaker identity registration %d has a nil factory", index)
		} else if err := validateApplicationSpeakerIdentity(registration.ApplicationSpeakerIdentitySelection); err != nil {
			return nil, fmt.Errorf("scenario conversation speaker identity registration %d: %w", index, err)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation speaker identity reference %q is registered more than once", registration.Reference)
		}
		registration.Descriptor = cloneV1Descriptor(registration.Descriptor)
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotPolicyRegistrations(source []PolicyFactoryRegistration) (map[string]PolicyFactoryRegistration, error) {
	result := make(map[string]PolicyFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(registration.Reference, registration.Artifact, "semantic policy"); err != nil {
			return nil, fmt.Errorf("scenario conversation semantic policy registration %d: %w", index, err)
		}
		parameterized := registration.DescribeConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.DescribeConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil ||
				len(registration.Configuration) != 0 || registration.Descriptor != (policyelements.SemanticDeciderDescriptor{}) {
				return nil, fmt.Errorf("scenario conversation semantic policy registration %d has a partial or mixed parameterized factory", index)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation semantic policy registration %d has a nil factory", index)
		} else if err := validateApplicationPolicy(registration.ApplicationPolicySelection); err != nil {
			return nil, fmt.Errorf("scenario conversation semantic policy registration %d: %w", index, err)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation semantic policy reference %q is registered more than once", registration.Reference)
		}
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotModelRegistrations(source []ModelFactoryRegistration) (map[string]ModelFactoryRegistration, error) {
	result := make(map[string]ModelFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(registration.Reference, registration.Artifact, "model"); err != nil {
			return nil, fmt.Errorf("scenario conversation model registration %d: %w", index, err)
		}
		parameterized := registration.DescribeConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.DescribeConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil ||
				len(registration.Configuration) != 0 || registration.Descriptor != (continuation.Descriptor{}) {
				return nil, fmt.Errorf("scenario conversation model registration %d has a partial or mixed parameterized factory", index)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation model registration %d has a nil factory", index)
		} else if err := continuation.ValidateDescriptor(registration.Descriptor); err != nil {
			return nil, fmt.Errorf("scenario conversation model registration %d descriptor: %w", index, err)
		} else if registration.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose ||
			(registration.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthorityVoice &&
				registration.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent) {
			return nil, fmt.Errorf("scenario conversation model registration %d is not proposal-only voice or silent cognition", index)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation model reference %q is registered more than once", registration.Reference)
		}
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotTTSRegistrations(source []TTSFactoryRegistration) (map[string]TTSFactoryRegistration, error) {
	result := make(map[string]TTSFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(registration.Reference, registration.Artifact, "TTS"); err != nil {
			return nil, fmt.Errorf("scenario conversation TTS registration %d: %w", index, err)
		}
		parameterized := registration.DescribeConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.DescribeConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil ||
				len(registration.Configuration) != 0 || !zeroV1Descriptor(registration.Descriptor) ||
				registration.Voice != "" {
				return nil, fmt.Errorf("scenario conversation TTS registration %d has a partial or mixed parameterized factory", index)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation TTS registration %d has a nil factory", index)
		} else if err := validateApplicationTTS(registration.ApplicationTTSSelection); err != nil {
			return nil, fmt.Errorf("scenario conversation TTS registration %d: %w", index, err)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation TTS reference %q is registered more than once", registration.Reference)
		}
		registration.Descriptor = cloneV1Descriptor(registration.Descriptor)
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotWordTimingRegistrations(
	source []WordTimingFactoryRegistration,
) (map[string]WordTimingFactoryRegistration, error) {
	result := make(map[string]WordTimingFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationProviderIdentity(
			registration.Reference, registration.Artifact, "word-timing",
		); err != nil {
			return nil, fmt.Errorf(
				"scenario conversation word-timing registration %d: %w", index, err,
			)
		}
		parameterized := registration.ValidateConfiguration != nil ||
			registration.FactoryConfiguration != nil || registration.ReadinessConfiguration != nil
		if parameterized {
			if registration.ValidateConfiguration == nil || registration.FactoryConfiguration == nil ||
				registration.ReadinessConfiguration == nil || registration.Factory != nil {
				return nil, fmt.Errorf(
					"scenario conversation word-timing registration %d has a partial or mixed parameterized factory",
					index,
				)
			}
		} else if registration.Factory == nil {
			return nil, fmt.Errorf(
				"scenario conversation word-timing registration %d has a nil factory", index,
			)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf(
				"scenario conversation word-timing reference %q is registered more than once",
				registration.Reference,
			)
		}
		result[registration.Reference] = registration
	}
	return result, nil
}

func validateApplicationProviderIdentity(
	reference string, artifact inspect.ArtifactIdentity, role string,
) error {
	if !canonicalIdentity(reference) {
		return fmt.Errorf("scenario conversation application %s reference is not canonical", role)
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation application %s artifact: %w", role, err)
	}
	return nil
}

func zeroV1Descriptor(descriptor v1.Descriptor) bool {
	return descriptor.Name == "" && descriptor.Version == "" && len(descriptor.Capabilities) == 0
}

func sameV1Descriptor(left, right v1.Descriptor) bool {
	return left.Name == right.Name && left.Version == right.Version &&
		maps.Equal(left.Capabilities, right.Capabilities)
}

func cloneApplicationConfig(source ApplicationConfig) ApplicationConfig {
	if source.NoiseFilter != nil {
		value := *source.NoiseFilter
		source.NoiseFilter = &value
	}
	result := source
	result.SemanticAdmission.TranscriptEvents = cloneSemanticTranscriptEvents(
		source.SemanticAdmission.TranscriptEvents,
	)
	result.ASR.Descriptor = cloneV1Descriptor(source.ASR.Descriptor)
	result.ASR.Configuration = slices.Clone(source.ASR.Configuration)
	if source.SpeakerIdentity != nil {
		speaker := *source.SpeakerIdentity
		speaker.Descriptor = cloneV1Descriptor(source.SpeakerIdentity.Descriptor)
		speaker.Configuration = slices.Clone(source.SpeakerIdentity.Configuration)
		result.SpeakerIdentity = &speaker
	}
	if source.WordTiming != nil {
		wordTiming := *source.WordTiming
		wordTiming.Configuration = slices.Clone(source.WordTiming.Configuration)
		result.WordTiming = &wordTiming
	}
	result.Policy.Configuration = slices.Clone(source.Policy.Configuration)
	result.Model.Configuration = slices.Clone(source.Model.Configuration)
	result.SilentModel.Configuration = slices.Clone(source.SilentModel.Configuration)
	result.TTS.Descriptor = cloneV1Descriptor(source.TTS.Descriptor)
	result.TTS.Configuration = slices.Clone(source.TTS.Configuration)
	result.Tools = cloneToolDeclarations(source.Tools)
	result.Target = cloneTarget(source.Target)
	return result
}

func cloneTarget(source computeruse.Target) computeruse.Target {
	source.Sources = slices.Clone(source.Sources)
	return source
}
