// Package model implements a graph-native process/remote model boundary.
//
// The element is deliberately not an interaction policy or an architecture
// preset. It forwards exactly the descriptor ports selected by graph topology
// over a protocol-v4 session, confirms the live descriptor and capability
// identities, and preserves envelope timing/correlation metadata. Omni,
// duplex, and upstream are therefore graph arrangements over the same element
// rather than runtime species.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/sidecar"
)

const (
	DeploymentRegistryService = "model.external.deployments"
	PayloadCodecService       = "model.external.payload-codec"
	ConfigSchema              = "schema://openrealtime/model/external-config/v1"
	maxDeploymentReference    = 1024
	maxElementSettingsBytes   = 512 << 10
	maxRequiredCapabilities   = 256
)

var (
	audioInputType  = element.Stream(element.Named("audio.InputFrame"))
	videoInputType  = element.Stream(element.Named("video.InputFrame"))
	contextType     = element.State(element.Named("trajectory.Snapshot"))
	textInputType   = element.Event(element.Named("text.ContextInjection"))
	toolsType       = element.State(element.Named("tool.Catalog"))
	toolResultType  = element.Event(element.Named("tool.Result"))
	generateType    = element.Trigger(element.Named("cognition.Generate"))
	commitType      = element.Trigger(element.Named("audio.Commit"))
	interactionType = element.Trigger(element.Named("interaction.Act"))
	tickType        = element.Trigger(element.Named("timing.Tick"))
	cancelType      = element.Interrupt(element.Named("flow.RunID"))
	truncateType    = element.Event(element.Named("audio.Truncation"))

	transcriptType = element.Revisions(
		element.Named("perception.Observation"), element.Named("perception.RevisionID"),
	)
	preparedTextType = element.Segmented(
		element.Named("text.PreparedDelta"), element.Named("flow.RunID"),
	)
	preparedAudioType = element.Segmented(
		element.Named("Prepared", element.Named("speech.AudioFrame")),
		element.Named("speech.UtteranceID"),
	)
	resultType         = element.Event(element.Named("cognition.Result"))
	toolProposalType   = element.Stream(element.Named("tool.Proposal"))
	interactionActType = element.Stream(element.Named("interaction.Act"))
	nativeStateType    = element.State(element.Named("model.NativeState"))
	activityType       = element.Stream(element.Named("interaction.AcousticActivity"))
	outcomeType        = element.Event(element.Named("cognition.Outcome"))
)

// StandardDescriptor is the capability superset used by the initial
// reference graphs. Every port is optional at the descriptor level: the
// mounted graph selects a subset, and the live handshake must prove every
// selected port. This avoids manufacturing one binding package per capability
// combination while preserving startup refusal for unavailable connections.
func StandardDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "model.External",
		Revision:      1,
		Ports: []element.Port{
			{Name: "audio", Direction: element.Input, Type: audioInputType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 32},
			{Name: "video", Direction: element.Input, Type: videoInputType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 2},
			{Name: "context", Direction: element.Input, Type: contextType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 1},
			{Name: "text", Direction: element.Input, Type: textInputType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "tools", Direction: element.Input, Type: toolsType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 1},
			{Name: "tool_result", Direction: element.Input, Type: toolResultType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "trigger", Direction: element.Input, Type: generateType, Cardinality: element.One, DefaultDepth: 8},
			{Name: "commit", Direction: element.Input, Type: commitType, Cardinality: element.One, DefaultDepth: 8},
			{Name: "interaction", Direction: element.Input, Type: interactionType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "tick", Direction: element.Input, Type: tickType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: cancelType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "truncate", Direction: element.Input, Type: truncateType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "transcript", Direction: element.Output, Type: transcriptType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 32},
			{Name: "text_out", Direction: element.Output, Type: preparedTextType, Cardinality: element.One, DefaultDepth: 64},
			{Name: "audio_out", Direction: element.Output, Type: preparedAudioType, Cardinality: element.One, DefaultDepth: 64},
			{Name: "result", Direction: element.Output, Type: resultType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "tool_proposal", Direction: element.Output, Type: toolProposalType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "interaction_act", Direction: element.Output, Type: interactionActType, Cardinality: element.One, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: nativeStateType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 1},
			{Name: "activity", Direction: element.Output, Type: activityType, Cardinality: element.One, LossAllowed: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: outcomeType, Cardinality: element.One, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:     []string{"trigger", "commit", "interaction", "tick", "truncate"},
			SampledState: []string{"context", "tools"}, Interrupts: []string{"cancel"},
			Outcomes: []string{
				"transcript", "text_out", "audio_out", "result", "tool_proposal",
				"interaction_act", "state", "activity", "outcome",
			},
		},
		StateSchema:  "schema://openrealtime/model/external-session-state/v1",
		ConfigSchema: ConfigSchema,
		Dependencies: []element.Dependency{
			{Name: DeploymentRegistryService}, {Name: PayloadCodecService},
		},
		Effects: []element.Effect{{Name: "model.external.session", Reversible: true}},
	}
}

// Public type helpers support gateways and adapters without exporting mutable
// package-level Type values.
func AudioInputType() element.Type     { return audioInputType.Clone() }
func VideoInputType() element.Type     { return videoInputType.Clone() }
func ContextType() element.Type        { return contextType.Clone() }
func TextInputType() element.Type      { return textInputType.Clone() }
func ToolsType() element.Type          { return toolsType.Clone() }
func ToolResultType() element.Type     { return toolResultType.Clone() }
func GenerateType() element.Type       { return generateType.Clone() }
func CommitType() element.Type         { return commitType.Clone() }
func InteractionType() element.Type    { return interactionType.Clone() }
func TickType() element.Type           { return tickType.Clone() }
func CancelType() element.Type         { return cancelType.Clone() }
func TruncateType() element.Type       { return truncateType.Clone() }
func TranscriptType() element.Type     { return transcriptType.Clone() }
func PreparedTextType() element.Type   { return preparedTextType.Clone() }
func PreparedAudioType() element.Type  { return preparedAudioType.Clone() }
func ResultType() element.Type         { return resultType.Clone() }
func ToolProposalType() element.Type   { return toolProposalType.Clone() }
func InteractionActType() element.Type { return interactionActType.Clone() }
func NativeStateType() element.Type    { return nativeStateType.Clone() }
func ActivityType() element.Type       { return activityType.Clone() }
func OutcomeType() element.Type        { return outcomeType.Clone() }

type Config struct {
	Deployment           string                          `json:"deployment"`
	Settings             json.RawMessage                 `json:"settings,omitempty"`
	RequiredCapabilities []sidecar.CapabilityRequirement `json:"required_capabilities,omitempty"`
}

func decodeConfig(source json.RawMessage) (Config, error) {
	var config Config
	if err := elementconfig.Decode(source, &config); err != nil {
		return Config{}, err
	}
	config.Deployment = strings.TrimSpace(config.Deployment)
	if config.Deployment == "" {
		return Config{}, errors.New("external model config requires a deployment reference")
	}
	if len(config.Deployment) > maxDeploymentReference ||
		strings.ContainsAny(config.Deployment, "\x00\r\n") {
		return Config{}, errors.New("external model deployment reference is not canonical or is too large")
	}
	if len(config.Settings) == 0 {
		config.Settings = json.RawMessage("{}")
	}
	trimmed := strings.TrimSpace(string(config.Settings))
	if !json.Valid(config.Settings) || len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, errors.New("external model settings must be one JSON object")
	}
	if len(config.Settings) > maxElementSettingsBytes {
		return Config{}, fmt.Errorf("external model settings exceed %d bytes", maxElementSettingsBytes)
	}
	if len(config.RequiredCapabilities) > maxRequiredCapabilities {
		return Config{}, fmt.Errorf("external model requires more than %d capabilities",
			maxRequiredCapabilities)
	}
	config.Settings = slices.Clone(config.Settings)
	probe := sidecar.Message{
		Type: sidecar.TypeHello, Version: sidecar.VersionElementGraph,
		ElementDescriptor: &element.Descriptor{
			FormatVersion: element.DescriptorFormatVersion, Name: "probe.Element", Revision: 1,
			Ports: []element.Port{{Name: "in", Direction: element.Input, Type: element.Event(element.Named("probe.Value")), Cardinality: element.One}},
		},
		ElementConfig:        config.Settings,
		SelectedPorts:        []sidecar.PortSelection{{Name: "in", Direction: element.Input, Type: element.Event(element.Named("probe.Value"))}},
		RequiredCapabilities: config.RequiredCapabilities,
	}
	if err := probe.Validate(); err != nil {
		return Config{}, fmt.Errorf("external model capability requirements: %w", err)
	}
	config.RequiredCapabilities = slices.Clone(config.RequiredCapabilities)
	return config, nil
}

// Session is the lifecycle and frame surface shared by a spawned sidecar, a
// persistent model server, and an upstream Realtime protocol adapter.
type Session interface {
	Ready() sidecar.Message
	Frames() <-chan sidecar.Message
	Err() error
	Send(sidecar.Message) error
	Close() error
}

type Dialer func(context.Context, sidecar.Message) (Session, error)

// ClientDialer adapts the repository's process/socket sidecar client. Remote
// Realtime adapters implement Dialer directly and therefore use the same
// graph contract and attestation path without pretending to be a binding.
func ClientDialer(config sidecar.Config) Dialer {
	return func(ctx context.Context, hello sidecar.Message) (Session, error) {
		config.ProtocolVersion = sidecar.VersionElementGraph
		return sidecar.Dial(ctx, config, hello)
	}
}

type deploymentEntry struct{ dial Dialer }

// DeploymentRegistry resolves symbolic node values to deployment-owned
// connection details and credentials. Those details never enter Graph IR.
type DeploymentRegistry struct {
	mu      sync.RWMutex
	entries map[string]deploymentEntry
}

func NewDeploymentRegistry() *DeploymentRegistry {
	return &DeploymentRegistry{entries: make(map[string]deploymentEntry)}
}

func (registry *DeploymentRegistry) Register(reference string, dial Dialer) error {
	if registry == nil {
		return errors.New("register external model deployment: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" || dial == nil {
		return errors.New("external model deployment requires a reference and dialer")
	}
	if len(reference) > maxDeploymentReference || strings.ContainsAny(reference, "\x00\r\n") {
		return errors.New("external model deployment reference is not canonical or is too large")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]deploymentEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("external model deployment %q is already registered", reference)
	}
	registry.entries[reference] = deploymentEntry{dial: dial}
	return nil
}

func (registry *DeploymentRegistry) resolve(reference string) (deploymentEntry, error) {
	if registry == nil {
		return deploymentEntry{}, errors.New("external model deployment registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[strings.TrimSpace(reference)]
	registry.mu.RUnlock()
	if !found {
		return deploymentEntry{}, fmt.Errorf("external model deployment %q is not registered", reference)
	}
	return entry, nil
}

// RegisterDescriptor and RegisterFactory are isolated integration hooks. The
// repository-wide catalog intentionally remains untouched in this slice.
func RegisterDescriptor(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register external model descriptor: nil catalog")
	}
	return catalog.Register(StandardDescriptor())
}

func RegisterFactory(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register external model factory: nil registry")
	}
	return registry.Register("", Factory{})
}
