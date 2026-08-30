package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

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
)

const (
	ApplicationReference          = "application.openrealtime.scenario-conversation.v1"
	ApplicationFormatVersion      = uint64(3)
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
	FormatVersion   uint64                      `json:"format_version"`
	Architecture    legacy.ArchitectureIdentity `json:"architecture"`
	ASR             ApplicationASRSelection     `json:"asr"`
	Policy          ApplicationPolicySelection  `json:"policy"`
	Model           ApplicationModelSelection   `json:"model"`
	SilentModel     ApplicationModelSelection   `json:"silent_model"`
	TTS             ApplicationTTSSelection     `json:"tts"`
	Tools           []ToolDeclaration           `json:"tools"`
	Target          computeruse.Target          `json:"target"`
	Gate            ApplicationGateSelection    `json:"gate"`
	Media           MediaLimits                 `json:"media"`
	MaxOutputTokens int                         `json:"max_output_tokens"`
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
	if err := validateApplicationPolicy(config.Policy); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationModel(config.Model, continuation.SpeechAuthorityVoice); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationModel(config.SilentModel, continuation.SpeechAuthoritySilent); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationTTS(config.TTS); err != nil {
		return ApplicationConfig{}, err
	}
	var err error
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

// ASRFactoryRegistration is one broad host inventory entry. The factory is
// retained but remains unopened until the selected session starts.
type ASRFactoryRegistration struct {
	ApplicationASRSelection
	Factory                func(context.Context, legacy.Options) (v1.PerceptionProvider, error)
	DescribeConfiguration  func(json.RawMessage) (v1.Descriptor, error)
	FactoryConfiguration   func(context.Context, legacy.Options, json.RawMessage) (v1.PerceptionProvider, error)
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

// ApplicationRegistrationConfig is process-private executable inventory. The
// supplied artifacts are host/build identities; this package never invents a
// provider, adapter, or dependency artifact from configuration bytes.
type ApplicationRegistrationConfig struct {
	ApplicationArtifact inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity
	RuntimeArtifact     inspect.ArtifactIdentity
	DependencyArtifact  inspect.ArtifactIdentity
	ASR                 []ASRFactoryRegistration
	Policies            []PolicyFactoryRegistration
	Models              []ModelFactoryRegistration
	TTS                 []TTSFactoryRegistration
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
		len(source.TTS) > maximumApplicationProviders {
		return launchprofile.Registration{}, errors.New(
			"scenario conversation application registration requires bounded ASR, policy, model, and TTS inventories",
		)
	}
	asr, err := snapshotASRRegistrations(source.ASR)
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
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			architecture, err := resolveScenarioArchitecture(config.Architecture)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			resolved, constructorErr := constructor(PluginConfig{
				RuntimeArtifact: runtimeArtifact, DependencyArtifact: dependencyArtifact,
				Architecture: architecture,
				ASR: ASRPlugin{Reference: ASRReference, Artifact: asrRegistration.Artifact,
					Descriptor: cloneV1Descriptor(asrDescriptor), Factory: asrFactory},
				Policy: PolicyPlugin{Reference: PolicyReference, Artifact: policyRegistration.Artifact,
					Descriptor: policyDescriptor, Factory: policyFactory},
				Model: ModelPlugin{Reference: ModelReference, Artifact: modelRegistration.Artifact,
					Descriptor: modelDescriptor, Factory: modelFactory},
				SilentModel: ModelPlugin{Reference: SilentModelReference, Artifact: silentModelRegistration.Artifact,
					Descriptor: silentModelDescriptor, Factory: silentModelFactory},
				TTS: TTSPlugin{Reference: TTSReference, Artifact: ttsRegistration.Artifact,
					Descriptor: cloneV1Descriptor(ttsDescriptor), Voice: ttsVoice,
					Factory: ttsFactory},
				Tools: cloneToolDeclarations(config.Tools), Target: cloneTarget(config.Target),
				Gate: config.Gate.gateConfig(), Media: config.Media,
				MaxOutputTokens: config.MaxOutputTokens,
			})
			if cause := context.Cause(ctx); cause != nil {
				return graphlaunch.Config{}, errors.Join(cause, constructorErr)
			}
			if constructorErr == nil {
				if asrReady != nil {
					resolved.Readiness = append(resolved.Readiness,
						graphlaunch.ReadinessCheck{Name: "asr:" + config.ASR.Reference, Check: asrReady})
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
	if definition.Ref() != (projectarch.Ref{ID: "cascade.composed-policy", Revision: 1}) ||
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
	result := source
	result.ASR.Descriptor = cloneV1Descriptor(source.ASR.Descriptor)
	result.ASR.Configuration = slices.Clone(source.ASR.Configuration)
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
