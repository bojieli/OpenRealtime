package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception"
)

const (
	ApplicationReference          = "application.openrealtime.scenario-conversation.v1"
	ApplicationFormatVersion      = uint64(1)
	maximumApplicationConfigBytes = 4 << 20
	maximumApplicationProviders   = 65_536
)

// ApplicationASRSelection is a serializable exact host-plugin selection. Its
// reference is a host inventory key; the application maps the matched factory
// to the graph's fixed ASRReference only after identity verification.
type ApplicationASRSelection struct {
	Reference  string                   `json:"reference"`
	Artifact   inspect.ArtifactIdentity `json:"artifact"`
	Descriptor v1.Descriptor            `json:"descriptor"`
}

// ApplicationModelSelection pins one continuation provider without retaining
// a credential, client, or factory in profile data.
type ApplicationModelSelection struct {
	Reference  string                   `json:"reference"`
	Artifact   inspect.ArtifactIdentity `json:"artifact"`
	Descriptor continuation.Descriptor  `json:"descriptor"`
}

// ApplicationTTSSelection pins one speech provider and its fixed voice.
type ApplicationTTSSelection struct {
	Reference  string                   `json:"reference"`
	Artifact   inspect.ArtifactIdentity `json:"artifact"`
	Descriptor v1.Descriptor            `json:"descriptor"`
	Voice      string                   `json:"voice"`
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
	FormatVersion   uint64                    `json:"format_version"`
	ASR             ApplicationASRSelection   `json:"asr"`
	Model           ApplicationModelSelection `json:"model"`
	TTS             ApplicationTTSSelection   `json:"tts"`
	Tools           []ToolDeclaration         `json:"tools"`
	Target          computeruse.Target        `json:"target"`
	Gate            ApplicationGateSelection  `json:"gate"`
	Media           MediaLimits               `json:"media"`
	MaxOutputTokens int                       `json:"max_output_tokens"`
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
	if err := validateApplicationASR(config.ASR); err != nil {
		return ApplicationConfig{}, err
	}
	if err := validateApplicationModel(config.Model); err != nil {
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

func validateApplicationModel(selection ApplicationModelSelection) error {
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
		selection.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthorityVoice {
		return errors.New("scenario conversation application model must be proposal-only and voice-authoritative")
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
	Factory func(context.Context, legacy.Options) (v1.PerceptionProvider, error)
}

type ModelFactoryRegistration struct {
	ApplicationModelSelection
	Factory func(context.Context, legacy.Options) (continuation.Provider, error)
}

type TTSFactoryRegistration struct {
	ApplicationTTSSelection
	Factory func(context.Context, legacy.Options) (v1.SpeechProvider, error)
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
	if len(source.ASR) == 0 || len(source.Models) == 0 || len(source.TTS) == 0 ||
		len(source.ASR) > maximumApplicationProviders || len(source.Models) > maximumApplicationProviders ||
		len(source.TTS) > maximumApplicationProviders {
		return launchprofile.Registration{}, errors.New(
			"scenario conversation application registration requires bounded ASR, model, and TTS inventories",
		)
	}
	asr, err := snapshotASRRegistrations(source.ASR)
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
			if asrRegistration.Artifact != config.ASR.Artifact ||
				!sameV1Descriptor(asrRegistration.Descriptor, config.ASR.Descriptor) {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation ASR %q artifact or descriptor drifted", config.ASR.Reference,
				)
			}
			modelRegistration, found := models[config.Model.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation model registry is missing %q", config.Model.Reference,
				)
			}
			if modelRegistration.Artifact != config.Model.Artifact ||
				modelRegistration.Descriptor != config.Model.Descriptor {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation model %q artifact or descriptor drifted", config.Model.Reference,
				)
			}
			ttsRegistration, found := tts[config.TTS.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation TTS registry is missing %q", config.TTS.Reference,
				)
			}
			if ttsRegistration.Artifact != config.TTS.Artifact ||
				!sameV1Descriptor(ttsRegistration.Descriptor, config.TTS.Descriptor) ||
				ttsRegistration.Voice != config.TTS.Voice {
				return graphlaunch.Config{}, fmt.Errorf(
					"scenario conversation TTS %q artifact, descriptor, or voice drifted", config.TTS.Reference,
				)
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			resolved, constructorErr := constructor(PluginConfig{
				RuntimeArtifact: runtimeArtifact, DependencyArtifact: dependencyArtifact,
				ASR: ASRPlugin{Reference: ASRReference, Artifact: asrRegistration.Artifact,
					Descriptor: cloneV1Descriptor(asrRegistration.Descriptor), Factory: asrRegistration.Factory},
				Model: ModelPlugin{Reference: ModelReference, Artifact: modelRegistration.Artifact,
					Descriptor: modelRegistration.Descriptor, Factory: modelRegistration.Factory},
				TTS: TTSPlugin{Reference: TTSReference, Artifact: ttsRegistration.Artifact,
					Descriptor: cloneV1Descriptor(ttsRegistration.Descriptor), Voice: ttsRegistration.Voice,
					Factory: ttsRegistration.Factory},
				Tools: cloneToolDeclarations(config.Tools), Target: cloneTarget(config.Target),
				Gate: config.Gate.gateConfig(), Media: config.Media,
				MaxOutputTokens: config.MaxOutputTokens,
			})
			if cause := context.Cause(ctx); cause != nil {
				return graphlaunch.Config{}, errors.Join(cause, constructorErr)
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

func snapshotASRRegistrations(source []ASRFactoryRegistration) (map[string]ASRFactoryRegistration, error) {
	result := make(map[string]ASRFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationASR(registration.ApplicationASRSelection); err != nil {
			return nil, fmt.Errorf("scenario conversation ASR registration %d: %w", index, err)
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation ASR registration %d has a nil factory", index)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation ASR reference %q is registered more than once", registration.Reference)
		}
		registration.Descriptor = cloneV1Descriptor(registration.Descriptor)
		result[registration.Reference] = registration
	}
	return result, nil
}

func snapshotModelRegistrations(source []ModelFactoryRegistration) (map[string]ModelFactoryRegistration, error) {
	result := make(map[string]ModelFactoryRegistration, len(source))
	for index, registration := range source {
		if err := validateApplicationModel(registration.ApplicationModelSelection); err != nil {
			return nil, fmt.Errorf("scenario conversation model registration %d: %w", index, err)
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation model registration %d has a nil factory", index)
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
		if err := validateApplicationTTS(registration.ApplicationTTSSelection); err != nil {
			return nil, fmt.Errorf("scenario conversation TTS registration %d: %w", index, err)
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf("scenario conversation TTS registration %d has a nil factory", index)
		}
		if _, duplicate := result[registration.Reference]; duplicate {
			return nil, fmt.Errorf("scenario conversation TTS reference %q is registered more than once", registration.Reference)
		}
		registration.Descriptor = cloneV1Descriptor(registration.Descriptor)
		result[registration.Reference] = registration
	}
	return result, nil
}

func sameV1Descriptor(left, right v1.Descriptor) bool {
	return left.Name == right.Name && left.Version == right.Version &&
		maps.Equal(left.Capabilities, right.Capabilities)
}

func cloneApplicationConfig(source ApplicationConfig) ApplicationConfig {
	result := source
	result.ASR.Descriptor = cloneV1Descriptor(source.ASR.Descriptor)
	result.TTS.Descriptor = cloneV1Descriptor(source.TTS.Descriptor)
	result.Tools = cloneToolDeclarations(source.Tools)
	result.Target = cloneTarget(source.Target)
	return result
}

func cloneTarget(source computeruse.Target) computeruse.Target {
	source.Sources = slices.Clone(source.Sources)
	return source
}
