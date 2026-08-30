// Package scenarioconversation adapts the stable OpenAI Realtime-compatible
// session surface to a locked, fine-grained conversational graph. Providers,
// tools, server hosts, and presentation clients remain exact plugins.
package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
)

const (
	AdapterReference = "go://github.com/bojieli/OpenRealtime/graph/binding/scenarioconversation/session-adapter/v1"
	ProfileName      = "openrealtime.scenario_conversation"
	ProfileRevision  = uint64(2)

	ASRReference          = "deployment.scenario-conversation.asr"
	PolicyReference       = "deployment.scenario-conversation.semantic-policy"
	ModelReference        = "deployment.scenario-conversation.model"
	SilentModelReference  = "deployment.scenario-conversation.model-silent"
	TTSReference          = "deployment.scenario-conversation.tts"
	PlaybackReference     = "deployment.scenario-conversation.playback"
	ToolReference         = "deployment.scenario-conversation.tools"
	TargetReference       = "deployment.scenario-conversation.target"
	LedgerReference       = "deployment.scenario-conversation.ledger"
	ConfirmationReference = "deployment.scenario-conversation.confirmation"

	SourceMicrophone = "microphone"
	SourceText       = "text"
	SourceMessage    = "message"

	defaultMediaMaxItems        = 32
	defaultMediaMaxBytes        = 64 << 20
	defaultMediaMaxItemBytes    = 32 << 20
	defaultMediaMaxPending      = 32
	defaultMediaMaxActiveLeases = 256
	maximumMediaBytes           = 1 << 30
)

// MediaLimits are copied into the graph-native media.RetainedMedia and
// media.ResolveAttachment node values and into the session resolver bridge.
// Keeping every bound in the application selection prevents the adapter from
// silently inventing a process-wide retention policy.
type MediaLimits struct {
	MaxItems        int `json:"max_items"`
	MaxBytes        int `json:"max_bytes"`
	MaxItemBytes    int `json:"max_item_bytes"`
	MaxPending      int `json:"max_pending"`
	MaxActiveLeases int `json:"max_active_leases"`
}

func (limits MediaLimits) normalized() (MediaLimits, error) {
	if limits.MaxItems == 0 {
		limits.MaxItems = defaultMediaMaxItems
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = defaultMediaMaxBytes
	}
	if limits.MaxItemBytes == 0 {
		limits.MaxItemBytes = defaultMediaMaxItemBytes
	}
	if limits.MaxPending == 0 {
		limits.MaxPending = defaultMediaMaxPending
	}
	if limits.MaxActiveLeases == 0 {
		limits.MaxActiveLeases = defaultMediaMaxActiveLeases
	}
	if limits.MaxItems < 1 || limits.MaxItems > 1_000_000 {
		return MediaLimits{}, errors.New("scenario conversation media max_items must be between 1 and 1000000")
	}
	if limits.MaxBytes < 1 || limits.MaxBytes > maximumMediaBytes {
		return MediaLimits{}, fmt.Errorf("scenario conversation media max_bytes must be between 1 and %d", maximumMediaBytes)
	}
	if limits.MaxItemBytes < 1 || limits.MaxItemBytes > limits.MaxBytes {
		return MediaLimits{}, errors.New("scenario conversation media max_item_bytes must be between 1 and max_bytes")
	}
	if limits.MaxPending < 1 || limits.MaxPending > 1_000_000 {
		return MediaLimits{}, errors.New("scenario conversation media max_pending must be between 1 and 1000000")
	}
	if limits.MaxActiveLeases < 1 || limits.MaxActiveLeases > 1_000_000 {
		return MediaLimits{}, errors.New("scenario conversation media max_active_leases must be between 1 and 1000000")
	}
	return limits, nil
}

type ASRPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor v1.Descriptor
	Factory    func(context.Context, legacy.Options) (v1.PerceptionProvider, error)
}

type ModelPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor continuation.Descriptor
	Factory    func(context.Context, legacy.Options) (continuation.Provider, error)
}

// PolicyPlugin is the exact enumerated semantic-admission provider selected
// by one launch profile. It is deliberately not a continuation provider: the
// factory can return only a policy decider and therefore cannot generate
// prose, propose tools, or acquire speech authority.
type PolicyPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor policyelements.SemanticDeciderDescriptor
	Factory    func(context.Context, legacy.Options) (policyelements.SemanticDecider, error)
}

type TTSPlugin struct {
	Reference  string
	Artifact   inspect.ArtifactIdentity
	Descriptor v1.Descriptor
	Voice      string
	Factory    func(context.Context, legacy.Options) (v1.SpeechProvider, error)
}

// ToolDeclaration is the exact client-executed action surface admitted by one
// profile. Dispatcher is deliberately absent: each mounted session installs
// its own graph-authorized client rendezvous.
type ToolDeclaration struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Parameters  json.RawMessage      `json:"parameters"`
	Confirm     legacyaction.Confirm `json:"confirm,omitempty"`
	Background  bool                 `json:"background,omitempty"`
	Target      string               `json:"target,omitempty"`
}

// PluginConfig is the immutable resource-free contribution retained by a
// launch configuration. Every factory remains unopened until session Start.
type PluginConfig struct {
	RuntimeArtifact    inspect.ArtifactIdentity
	DependencyArtifact inspect.ArtifactIdentity
	Architecture       projectarch.Definition
	ASR                ASRPlugin
	Policy             PolicyPlugin
	Model              ModelPlugin
	SilentModel        ModelPlugin
	TTS                TTSPlugin
	Tools              []ToolDeclaration
	Target             computeruse.Target
	Gate               perception.GateConfig
	Media              MediaLimits
	MaxOutputTokens    int
}

// NormalizePluginConfig validates and snapshots one complete resource-free
// plugin selection. It never invokes a provider factory; graph application
// registries use it to derive immutable values and dependency identities
// before a session is allowed to start.
func NormalizePluginConfig(source PluginConfig) (PluginConfig, error) {
	config := clonePluginConfig(source)
	architecture, err := resolveScenarioArchitecture(config.Architecture.Identity())
	if err != nil {
		return PluginConfig{}, err
	}
	config.Architecture = architecture
	if err := validatePluginConfig(config); err != nil {
		return PluginConfig{}, err
	}
	config.Tools, err = normalizeToolDeclarations(config.Tools)
	if err != nil {
		return PluginConfig{}, err
	}
	config.Media, err = config.Media.normalized()
	if err != nil {
		return PluginConfig{}, err
	}
	return config, nil
}

func clonePluginConfig(source PluginConfig) PluginConfig {
	result := source
	result.ASR.Descriptor.Capabilities = maps.Clone(source.ASR.Descriptor.Capabilities)
	result.TTS.Descriptor.Capabilities = maps.Clone(source.TTS.Descriptor.Capabilities)
	result.Tools = cloneToolDeclarations(source.Tools)
	result.Target.Sources = slices.Clone(source.Target.Sources)
	return result
}

func validatePluginConfig(config PluginConfig) error {
	if _, err := resolveScenarioArchitecture(config.Architecture.Identity()); err != nil {
		return err
	}
	if err := config.RuntimeArtifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation runtime artifact: %w", err)
	}
	if err := config.DependencyArtifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation dependency artifact: %w", err)
	}
	if err := validateASRPlugin(config.ASR); err != nil {
		return err
	}
	if err := validatePolicyPlugin(config.Policy); err != nil {
		return err
	}
	if evidence := config.Architecture.Interaction.EvidenceCapabilities; evidence != nil &&
		evidence.DirectVisualInput && !config.Policy.Descriptor.Vision {
		return errors.New("scenario conversation direct-visual architecture requires a vision-capable semantic policy")
	}
	if err := validateModelPlugin(config.Model, ModelReference, continuation.SpeechAuthorityVoice); err != nil {
		return err
	}
	if err := validateModelPlugin(config.SilentModel, SilentModelReference, continuation.SpeechAuthoritySilent); err != nil {
		return err
	}
	if config.Model.Artifact != config.SilentModel.Artifact {
		return errors.New("scenario conversation voice and silent cognition must come from one exact provider plugin artifact")
	}
	if err := validateTTSPlugin(config.TTS); err != nil {
		return err
	}
	tools, err := normalizeToolDeclarations(config.Tools)
	if err != nil {
		return err
	}
	if err := validateActionTarget(config.Target, tools, "scenario conversation action"); err != nil {
		return err
	}
	if _, err := perception.NewEnergyGate(config.Gate, 24_000); err != nil {
		return fmt.Errorf("scenario conversation acoustic gate: %w", err)
	}
	if _, err := config.Media.normalized(); err != nil {
		return err
	}
	if config.MaxOutputTokens < 1 || config.MaxOutputTokens > 1_000_000 {
		return errors.New("scenario conversation max_output_tokens must be between 1 and 1000000")
	}
	return nil
}

func validateActionTarget(target computeruse.Target, tools []ToolDeclaration, prefix string) error {
	if err := target.Validate(); err != nil {
		return fmt.Errorf("%s target: %w", prefix, err)
	}
	if !canonicalIdentity(target.Name) {
		return fmt.Errorf("%s target name is not canonical", prefix)
	}
	seenSources := make(map[string]struct{}, len(target.Sources))
	for _, source := range target.Sources {
		if !canonicalIdentity(source) {
			return fmt.Errorf("%s target source is not canonical", prefix)
		}
		if _, duplicate := seenSources[source]; duplicate {
			return fmt.Errorf("%s target source %q is repeated", prefix, source)
		}
		seenSources[source] = struct{}{}
	}
	for _, tool := range tools {
		if tool.Target != "" && tool.Target != target.Name {
			return fmt.Errorf(
				"%s tool %q selects target %q, want %q",
				prefix, tool.Name, tool.Target, target.Name,
			)
		}
	}
	return nil
}

func validateASRPlugin(plugin ASRPlugin) error {
	if !canonicalIdentity(plugin.Reference) || plugin.Factory == nil {
		return errors.New("scenario conversation ASR plugin requires a canonical reference and factory")
	}
	if plugin.Reference != ASRReference {
		return fmt.Errorf("scenario conversation ASR reference %q, want exact graph selection %q",
			plugin.Reference, ASRReference)
	}
	if err := plugin.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation ASR artifact: %w", err)
	}
	if err := plugin.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation ASR descriptor: %w", err)
	}
	return nil
}

func validatePolicyPlugin(plugin PolicyPlugin) error {
	if !canonicalIdentity(plugin.Reference) || plugin.Factory == nil {
		return errors.New("scenario conversation semantic policy plugin requires a canonical reference and factory")
	}
	if plugin.Reference != PolicyReference {
		return fmt.Errorf("scenario conversation semantic policy reference %q, want exact graph selection %q",
			plugin.Reference, PolicyReference)
	}
	if err := plugin.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation semantic policy artifact: %w", err)
	}
	if err := plugin.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation semantic policy descriptor: %w", err)
	}
	return nil
}

func validateModelPlugin(
	plugin ModelPlugin, reference string, speechAuthority continuation.SpeechAuthority,
) error {
	if !canonicalIdentity(plugin.Reference) || plugin.Factory == nil {
		return errors.New("scenario conversation model plugin requires a canonical reference and factory")
	}
	if plugin.Reference != reference {
		return fmt.Errorf("scenario conversation model reference %q, want exact graph selection %q",
			plugin.Reference, reference)
	}
	if err := plugin.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation model artifact: %w", err)
	}
	if err := continuation.ValidateDescriptor(plugin.Descriptor); err != nil {
		return fmt.Errorf("scenario conversation model descriptor: %w", err)
	}
	if plugin.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose {
		return errors.New("scenario conversation model must have proposal-only tool authority")
	}
	if plugin.Descriptor.EffectiveSpeechAuthority() != speechAuthority {
		return fmt.Errorf("scenario conversation model %q must have %s speech authority",
			plugin.Reference, speechAuthority)
	}
	return nil
}

func validateTTSPlugin(plugin TTSPlugin) error {
	if !canonicalIdentity(plugin.Reference) || plugin.Factory == nil {
		return errors.New("scenario conversation TTS plugin requires a canonical reference and factory")
	}
	if plugin.Reference != TTSReference {
		return fmt.Errorf("scenario conversation TTS reference %q, want exact graph selection %q",
			plugin.Reference, TTSReference)
	}
	if err := plugin.Artifact.Validate(); err != nil {
		return fmt.Errorf("scenario conversation TTS artifact: %w", err)
	}
	if err := plugin.Descriptor.Validate(); err != nil {
		return fmt.Errorf("scenario conversation TTS descriptor: %w", err)
	}
	if !plugin.Descriptor.Capabilities.Has(v1.CapabilityPCM16Output) {
		return errors.New("scenario conversation TTS descriptor must provide PCM16 output")
	}
	if !canonicalIdentity(plugin.Voice) {
		return errors.New("scenario conversation TTS plugin requires a canonical fixed voice")
	}
	return nil
}

func normalizeToolDeclarations(source []ToolDeclaration) ([]ToolDeclaration, error) {
	if len(source) > 4096 {
		return nil, errors.New("scenario conversation selects more than 4096 tools")
	}
	result := cloneToolDeclarations(source)
	registry := legacyaction.NewRegistry()
	seen := make(map[string]struct{}, len(result))
	for index := range result {
		declaration := &result[index]
		if !canonicalIdentity(declaration.Name) || declaration.Name != strings.TrimSpace(declaration.Name) ||
			declaration.Description == "" || declaration.Description != strings.TrimSpace(declaration.Description) {
			return nil, fmt.Errorf("scenario conversation tool %d has a non-canonical name or description", index)
		}
		if _, duplicate := seen[declaration.Name]; duplicate {
			return nil, fmt.Errorf("scenario conversation tool %q is repeated", declaration.Name)
		}
		seen[declaration.Name] = struct{}{}
		confirm, err := legacyaction.ParseConfirm(string(declaration.Confirm))
		if err != nil {
			return nil, fmt.Errorf("scenario conversation tool %q confirmation: %w", declaration.Name, err)
		}
		if confirm != legacyaction.ConfirmNever {
			return nil, fmt.Errorf("scenario conversation tool %q requires unsupported confirmation %q",
				declaration.Name, confirm)
		}
		declaration.Confirm = confirm
		if declaration.Target != "" && declaration.Target != strings.TrimSpace(declaration.Target) {
			return nil, fmt.Errorf("scenario conversation tool %q target is not canonical", declaration.Name)
		}
		var parameters map[string]json.RawMessage
		if len(declaration.Parameters) == 0 ||
			json.Unmarshal(declaration.Parameters, &parameters) != nil || parameters == nil {
			return nil, fmt.Errorf("scenario conversation tool %q parameters must be a JSON object", declaration.Name)
		}
		canonicalParameters, err := json.Marshal(parameters)
		if err != nil {
			return nil, fmt.Errorf("canonicalize scenario conversation tool %q: %w", declaration.Name, err)
		}
		declaration.Parameters = canonicalParameters
		if err := registry.Declare(legacyaction.ToolSpec{
			Name: declaration.Name, Description: declaration.Description,
			Parameters: declaration.Parameters, Confirm: declaration.Confirm,
			Background: declaration.Background, Target: declaration.Target,
		}); err != nil {
			return nil, fmt.Errorf("scenario conversation tool %q: %w", declaration.Name, err)
		}
	}
	return result, nil
}

func cloneToolDeclarations(source []ToolDeclaration) []ToolDeclaration {
	result := make([]ToolDeclaration, len(source))
	for index, declaration := range source {
		result[index] = declaration
		result[index].Parameters = slices.Clone(declaration.Parameters)
	}
	return result
}

func sameToolDeclaration(left legacyaction.ToolSpec, right ToolDeclaration) bool {
	leftConfirm, leftErr := legacyaction.ParseConfirm(string(left.Confirm))
	rightConfirm, rightErr := legacyaction.ParseConfirm(string(right.Confirm))
	if leftErr != nil || rightErr != nil || left.Name != right.Name || left.Description != right.Description ||
		leftConfirm != rightConfirm ||
		left.Background != right.Background || left.Target != right.Target {
		return false
	}
	var leftParameters, rightParameters any
	return json.Unmarshal(left.Parameters, &leftParameters) == nil &&
		json.Unmarshal(right.Parameters, &rightParameters) == nil &&
		reflect.DeepEqual(leftParameters, rightParameters)
}

func canonicalIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func canonicalMIMEType(value string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType == "" || !strings.Contains(mediaType, "/") {
		if err == nil {
			err = errors.New("media type requires a type and subtype")
		}
		return "", err
	}
	canonical := mime.FormatMediaType(strings.ToLower(mediaType), parameters)
	if canonical == "" {
		return "", errors.New("media type could not be canonicalized")
	}
	return canonical, nil
}
