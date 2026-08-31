package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	standardelements "github.com/bojieli/OpenRealtime/elements"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

// RegisterDescriptors contributes only Meeting Assistant element contracts.
// It is intentionally not called by elements.Catalog: installing this
// application plugin is an explicit deployment choice.
func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register meeting descriptors: nil catalog")
	}
	for _, descriptor := range []element.Descriptor{
		ScreenForkDescriptor(), BackgroundInjectionDescriptor(),
	} {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func DefaultDescriptorCatalog() (*resolve.Catalog, error) {
	catalog, err := standardelements.Catalog()
	if err != nil {
		return nil, err
	}
	if err := RegisterDescriptors(catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

// FactoryRegistrations returns the resource-free executable contribution for
// this plugin. Provider services are separate launch-catalog contributions.
func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	entries := []struct {
		factory  element.Factory
		artifact inspect.ArtifactIdentity
	}{
		{screenForkFactory{}, inspect.ArtifactIdentity{ID: screenForkRuntimeID, Revision: runtimeRevision}},
		{backgroundInjectionFactory{}, inspect.ArtifactIdentity{ID: backgroundRuntimeID, Revision: runtimeRevision}},
	}
	result := make([]graphruntime.FactoryRegistration, 0, len(entries))
	for _, entry := range entries {
		identity, err := entry.factory.Descriptor().Identity()
		if err != nil {
			return nil, err
		}
		result = append(result, graphruntime.FactoryRegistration{
			Profile: graphruntime.FactoryProfile{
				Reference: identity.Name, Artifact: entry.artifact,
				Transports: []string{"in-process"},
			},
			Factory: entry.factory,
		})
	}
	return result, nil
}

func AssemblyContribution() (graphassembly.Catalog, error) {
	registrations, err := FactoryRegistrations()
	if err != nil {
		return graphassembly.Catalog{}, err
	}
	return graphassembly.Catalog{Implementations: registrations}, nil
}

func reportRuntime(reporter element.ResolutionReporter, runtimeID string) error {
	if reporter == nil {
		return errors.New("meeting element has no live resolution reporter")
	}
	if err := reporter.Runtime(runtimeID, runtimeRevision, ""); err != nil {
		return err
	}
	return reporter.Capabilities([]element.CapabilityResolution{})
}

type AdapterProfileConfig struct {
	Name                string
	Revision            uint64
	Ownership           legacy.Ownership
	Voice               legacy.VoiceControl
	Stack               legacy.StackCapabilities
	MaxOutputTokens     int
	AdditionalObservers []string
}

type AdapterPluginConfig struct {
	Reference string
	Artifact  inspect.ArtifactIdentity
	Profile   AdapterProfileConfig
	Factory   graphbinding.AdapterFactory
}

// AdapterPlugin builds the graph-fingerprint-bound Realtime projection while
// leaving protocol translation in an exact, deployment-selected adapter
// factory. Constructing or binding the plugin acquires no runtime resource.
func AdapterPlugin(
	config AdapterPluginConfig,
) (graphlaunch.AdapterPlugin, graphlaunch.AdapterSelection, error) {
	config.Profile.AdditionalObservers = slices.Clone(config.Profile.AdditionalObservers)
	registration := graphbinding.AdapterRegistration{
		Reference: config.Reference, Artifact: config.Artifact, Factory: config.Factory,
	}
	if err := registration.Validate(); err != nil {
		return graphlaunch.AdapterPlugin{}, graphlaunch.AdapterSelection{},
			fmt.Errorf("meeting adapter: %w", err)
	}
	if err := validateMeetingCapabilities(config.Profile); err != nil {
		return graphlaunch.AdapterPlugin{}, graphlaunch.AdapterSelection{}, err
	}
	plugin := graphlaunch.AdapterPlugin{
		Reference: config.Reference, Artifact: config.Artifact,
		Bind: func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			if ctx == nil {
				return graphlaunch.BoundAdapter{}, errors.New("bind meeting adapter: nil context")
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			profile, err := meetingAdapterProfile(plan, config.Profile)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{Profile: profile, Registration: registration}, nil
		},
	}
	return plugin, plugin.Selection(config.Profile.Name, config.Profile.Revision), nil
}

func validateMeetingCapabilities(config AdapterProfileConfig) error {
	if !canonicalText(config.Name) || config.Revision == 0 {
		return errors.New("meeting adapter requires a canonical, revisioned profile name")
	}
	if err := config.Ownership.Validate(); err != nil {
		return fmt.Errorf("meeting adapter ownership: %w", err)
	}
	if config.Voice.InForce != "" && !canonicalText(config.Voice.InForce) {
		return errors.New("meeting adapter voice must be canonical")
	}
	if config.MaxOutputTokens < 0 {
		return errors.New("meeting adapter max output tokens cannot be negative")
	}
	if len(config.AdditionalObservers) > 255 {
		return errors.New("meeting adapter has more than 255 additional observers")
	}
	seenObservers := map[string]struct{}{"screen": {}}
	for _, observer := range config.AdditionalObservers {
		if !canonicalText(observer) {
			return errors.New("meeting adapter observer must be canonical")
		}
		if _, duplicate := seenObservers[observer]; duplicate {
			return fmt.Errorf("meeting adapter observer %q is duplicated", observer)
		}
		seenObservers[observer] = struct{}{}
	}
	required := []struct {
		name    string
		present bool
	}{
		{"audio input", config.Stack.AudioInput},
		{"audio output", config.Stack.AudioOutput},
		{"visual input", config.Stack.VisualInput},
		{"transcription", config.Stack.Transcription},
		{"turn generation", config.Stack.TurnGeneration},
		{"concurrent I/O", config.Stack.ConcurrentIO},
		{"text injection", config.Stack.TextInjection},
	}
	for _, capability := range required {
		if !capability.present {
			return fmt.Errorf("meeting adapter requires %s capability", capability.name)
		}
	}
	return nil
}

func meetingAdapterProfile(
	plan *graphconfig.Plan, config AdapterProfileConfig,
) (graphbinding.SessionAdapterProfile, error) {
	if plan == nil {
		return graphbinding.SessionAdapterProfile{}, errors.New("bind meeting adapter: nil plan")
	}
	if err := plan.Validate(); err != nil {
		return graphbinding.SessionAdapterProfile{}, fmt.Errorf("bind meeting adapter plan: %w", err)
	}
	graph := plan.Graph()
	if graph.ID != GraphID {
		return graphbinding.SessionAdapterProfile{}, fmt.Errorf(
			"meeting adapter requires graph %q, got %q", GraphID, graph.ID,
		)
	}
	if err := validateMeetingPlanReferences(plan); err != nil {
		return graphbinding.SessionAdapterProfile{}, err
	}
	mappings := []struct {
		operation graphbinding.AdapterOperation
		boundary  string
	}{
		{graphbinding.AdapterInputUpdate, "tools"},
		{graphbinding.AdapterInputAudio, "audio"},
		{graphbinding.AdapterInputVideo, "video"},
		{graphbinding.AdapterInputText, "text"},
		{graphbinding.AdapterInputToolResult, "tool_result"},
		{graphbinding.AdapterInputCommitAudio, "commit_audio"},
		{graphbinding.AdapterInputCreateResponse, "create_response"},
		{graphbinding.AdapterInputCancel, "cancel"},
		{graphbinding.AdapterInputTruncate, "truncate"},
		{graphbinding.AdapterOutputActivity, "activity"},
		{graphbinding.AdapterOutputTranscript, "transcript"},
		{graphbinding.AdapterOutputObservation, "observations"},
		{graphbinding.AdapterOutputSpeechText, "prepared_text"},
		{graphbinding.AdapterOutputSpeechAudio, "prepared_audio"},
		{graphbinding.AdapterOutputToolCalls, "tool_proposals"},
		{graphbinding.AdapterOutputTurnEnd, "foreground_outcome"},
		{graphbinding.AdapterOutputDebug, "background_outcome"},
	}
	boundaries := make(map[string]int, len(graph.Boundaries))
	for index, boundary := range graph.Boundaries {
		boundaries[boundary.Name] = index
	}
	adapterBoundaries := make([]graphbinding.AdapterBoundary, 0, len(mappings))
	for _, mapping := range mappings {
		located, found := boundaries[mapping.boundary]
		if !found {
			return graphbinding.SessionAdapterProfile{}, fmt.Errorf(
				"meeting adapter graph has no boundary %q", mapping.boundary,
			)
		}
		boundary := graph.Boundaries[located]
		adapterBoundaries = append(adapterBoundaries, graphbinding.AdapterBoundary{
			Operation: mapping.operation, Boundary: mapping.boundary,
			Direction: boundary.Direction, Type: boundary.Type,
		})
	}
	observers := append([]string{"screen"}, config.AdditionalObservers...)
	return graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          config.Name, Revision: config.Revision, GraphFingerprint: graph.Fingerprint,
		Ownership: config.Ownership,
		Capabilities: legacy.Capabilities{
			Video: true, ComputerUse: true, Observations: true, FastSlow: true,
			Observers: observers, Voice: config.Voice, ManualTurns: true,
			MaxOutputTokens: config.MaxOutputTokens, Stack: config.Stack,
		},
		Boundaries: adapterBoundaries,
	})
}

func validateMeetingPlanReferences(plan *graphconfig.Plan) error {
	wanted := []struct {
		node, field, reference string
	}{
		{"foreground", "deployment", ForegroundDeploymentReference},
		{"screen_observer", "provider", VisualProviderReference},
		{"background_model", "provider", BackgroundProviderReference},
	}
	values := plan.Values()
	decoded := make(map[string]map[string]any, len(wanted))
	for _, requirement := range wanted {
		source, found := values[requirement.node]
		if !found {
			return fmt.Errorf("meeting adapter values have no node %q", requirement.node)
		}
		fields := decoded[requirement.node]
		if fields == nil {
			if err := json.Unmarshal(source, &fields); err != nil {
				return fmt.Errorf("meeting adapter decode values for %s: %w", requirement.node, err)
			}
			decoded[requirement.node] = fields
		}
		actual, _ := fields[requirement.field].(string)
		if actual != requirement.reference {
			return fmt.Errorf("meeting adapter node %s %s reference %q, want %q",
				requirement.node, requirement.field, actual, requirement.reference)
		}
	}
	return nil
}

// ProviderConfig is an application-profile seam over the generic launcher.
// Plugins contains only extra provider/dependency/adapter contributions;
// standard and Meeting Assistant element implementations are composed here.
type ProviderConfig struct {
	Artifacts     graphconfig.Artifacts
	PlanOptions   graphconfig.Options
	Plugins       graphlaunch.Catalog
	SecretCatalog *graphsecret.Document
	Adapter       AdapterPluginConfig

	Inspection      graphruntime.InspectionConfig
	ShutdownTimeout time.Duration
	TraceRecording  *graphbinding.TraceRecordingConfig
	Readiness       []graphlaunch.ReadinessCheck
}

// LaunchConfig composes the resource-free Meeting Assistant contribution into
// the generic graph launcher contract. The returned selection remains fully
// inspectable by a server-profile registry before graphlaunch.New is called.
// It does not mount an element, invoke a dependency or adapter factory,
// resolve a secret, dial a provider, or open a listener.
func LaunchConfig(config ProviderConfig) (graphlaunch.Config, error) {
	options := config.PlanOptions
	if options.Catalog == nil {
		catalog, err := DefaultDescriptorCatalog()
		if err != nil {
			return graphlaunch.Config{}, err
		}
		options.Catalog = catalog
	}
	if options.SchemaResolver == nil {
		resolver, err := DefaultSchemaResolver()
		if err != nil {
			return graphlaunch.Config{}, err
		}
		options.SchemaResolver = resolver
	}
	if options.Loader == nil {
		options.Loader = graphcompiler.FileLoader{}
	}
	standard, err := standardelements.AssemblyCatalog()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	meeting, err := AssemblyContribution()
	if err != nil {
		return graphlaunch.Config{}, err
	}
	standard.Implementations = append(standard.Implementations, meeting.Implementations...)
	standard.Implementations = append(standard.Implementations, config.Plugins.Assembly.Implementations...)
	standard.Dependencies = append(standard.Dependencies, config.Plugins.Assembly.Dependencies...)
	standard.SecretProviders = append(standard.SecretProviders, config.Plugins.Assembly.SecretProviders...)

	adapter, selection, err := AdapterPlugin(config.Adapter)
	if err != nil {
		return graphlaunch.Config{}, err
	}
	adapters := slices.Clone(config.Plugins.Adapters)
	adapters = append(adapters, adapter)
	return graphlaunch.Config{
		Artifacts: config.Artifacts, PlanOptions: options,
		Catalog: graphlaunch.Catalog{
			Assembly: standard, Adapters: adapters,
			MountDependencies: slices.Clone(config.Plugins.MountDependencies),
		},
		SecretCatalog: config.SecretCatalog, Adapter: selection,
		Inspection: config.Inspection, ShutdownTimeout: config.ShutdownTimeout,
		TraceRecording: config.TraceRecording,
		Readiness:      slices.Clone(config.Readiness),
	}, nil
}

// NewProvider seals an exact Meeting Assistant graph provider without
// mounting an element, dialing a model, resolving a secret, or opening a
// listener. The caller may pass a broader plugin catalog; graph/launch reduces
// it to the immutable plan before creating the provider.
func NewProvider(ctx context.Context, config ProviderConfig) (graphlaunch.Result, error) {
	if ctx == nil {
		return graphlaunch.Result{}, errors.New("create meeting provider: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return graphlaunch.Result{}, err
	}
	launch, err := LaunchConfig(config)
	if err != nil {
		return graphlaunch.Result{}, err
	}
	return graphlaunch.New(ctx, launch)
}
