package graphnative

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	SessionAdapterReference = "go://github.com/bojieli/OpenRealtime/meeting/graphnative/session-adapter/v1"
	SessionProfileName      = "openrealtime.meeting-assistant.graph-native"
	SessionProfileRevision  = uint64(1)
)

var meetingSessionDependencyNames = []string{
	modelelements.DeploymentRegistryService,
	modelelements.PayloadCodecService,
	perceptionelements.VisualProviderRegistryService,
	cognitionelements.ProviderRegistryService,
	stateelements.TrajectoryStoreService,
}

// ForegroundBindingFactory constructs one session-scoped implementation of
// the stable voice-stack contract. The returned binding is exposed only
// through model.External protocol v4; it never becomes a second server or a
// Meeting-specific wire protocol.
type ForegroundBindingFactory func(context.Context, legacy.Options) (legacy.Binding, error)

// ForegroundPlugin pins the live binding contract and the three independently
// observable identities in its protocol-v4 readiness proof. Artifact is the
// mount-dependency identity. ProviderArtifact identifies the complete selected
// voice stack, RuntimeArtifact identifies the wrapper executing it, and
// WireAdapterArtifact identifies its payload translation.
type ForegroundPlugin struct {
	Artifact            inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity
	RuntimeArtifact     inspect.ArtifactIdentity
	WireAdapterArtifact inspect.ArtifactIdentity
	BindingName         string
	Ownership           legacy.Ownership
	Capabilities        legacy.Capabilities
	Descriptor          continuation.Descriptor
	Factory             ForegroundBindingFactory
}

// VisualPlugin contributes exactly one adaptive-observation narrator selected
// by the immutable Meeting graph.
type VisualPlugin struct {
	Artifact   inspect.ArtifactIdentity
	Descriptor perceptionelements.VisualProviderDescriptor
	Factory    func(context.Context, legacy.Options) (perceptionelements.VisualProvider, error)
}

// BackgroundPlugin contributes exactly one silent continuation provider. Its
// output can reach the foreground only through meeting.BackgroundInjection.
type BackgroundPlugin struct {
	Artifact   inspect.ArtifactIdentity
	Descriptor continuation.Descriptor
	Factory    func(context.Context, legacy.Options) (continuation.Provider, error)
}

// SessionPluginConfig is a resource-free executable inventory for one exact
// Meeting Assistant deployment. Constructing the plugin validates and clones
// metadata only; provider factories are called after a session graph mounts.
type SessionPluginConfig struct {
	AdapterArtifact inspect.ArtifactIdentity
	Adapter         SessionAdapterConfig
	Foreground      ForegroundPlugin
	Visual          VisualPlugin
	Background      BackgroundPlugin
}

// SessionPlugin contributes the concrete Realtime adapter and all four exact
// mount-scoped services selected by the Meeting graph.
type SessionPlugin struct {
	config      SessionPluginConfig
	adapter     AdapterPluginConfig
	coordinator *meetingStoreCoordinator
}

// NewSessionPlugin validates and snapshots a production Meeting contribution
// without acquiring a recognizer, model, narrator, synthesizer, or listener.
func NewSessionPlugin(source SessionPluginConfig) (*SessionPlugin, error) {
	config := cloneSessionPluginConfig(source)
	if err := validateSessionPluginConfig(config); err != nil {
		return nil, err
	}
	plugin := &SessionPlugin{config: config, coordinator: newMeetingStoreCoordinator()}
	adapter := AdapterPluginConfig{
		Reference: SessionAdapterReference,
		Artifact:  config.AdapterArtifact,
		Profile: AdapterProfileConfig{
			Name: SessionProfileName, Revision: SessionProfileRevision,
			Ownership:           config.Foreground.Ownership,
			Voice:               config.Foreground.Capabilities.Voice,
			Stack:               config.Foreground.Capabilities.Stack,
			MaxOutputTokens:     config.Foreground.Capabilities.MaxOutputTokens,
			AdditionalObservers: []string{"meeting.foreground.asr"},
		},
		Factory: plugin.sessionAdapterFactory(),
	}
	if _, _, err := AdapterPlugin(adapter); err != nil {
		return nil, fmt.Errorf("meeting session adapter plugin: %w", err)
	}
	plugin.adapter = adapter
	return plugin, nil
}

// AdapterConfig returns the exact application-host adapter contribution.
func (plugin *SessionPlugin) AdapterConfig() AdapterPluginConfig {
	if plugin == nil {
		return AdapterPluginConfig{}
	}
	return cloneAdapterPluginConfig(plugin.adapter)
}

// AssemblyDependencies returns the four exact, mount-scoped service
// identities. No service value or provider resource exists at this stage.
func (plugin *SessionPlugin) AssemblyDependencies() []graphassembly.Dependency {
	if plugin == nil {
		return nil
	}
	artifacts := plugin.dependencyArtifacts()
	result := make([]graphassembly.Dependency, 0, len(meetingSessionDependencyNames))
	for _, name := range meetingSessionDependencyNames {
		result = append(result, graphassembly.Dependency{
			Name: name, Artifact: artifacts[name], Scope: graphconfig.DependencyScopeMount,
		})
	}
	return result
}

// MountDependencies returns independent exact service contributions. Every
// call creates a fresh per-session registry, while provider acquisition stays
// behind the graph element that owns that provider's lifecycle.
func (plugin *SessionPlugin) MountDependencies() []graphlaunch.MountDependencyPlugin {
	if plugin == nil {
		return nil
	}
	artifacts := plugin.dependencyArtifacts()
	result := make([]graphlaunch.MountDependencyPlugin, 0, len(meetingSessionDependencyNames))
	for _, dependencyName := range meetingSessionDependencyNames {
		name := dependencyName
		artifact := artifacts[name]
		result = append(result, graphlaunch.MountDependencyPlugin{
			Name: name, Artifact: artifact,
			Factory: func(ctx context.Context, options legacy.Options) ([]graphruntime.PreparedMountDependency, error) {
				service, err := plugin.mountService(ctx, options, name)
				if err != nil {
					return nil, err
				}
				return []graphruntime.PreparedMountDependency{{
					Name: name, Artifact: artifact, Service: service,
				}}, nil
			},
		})
	}
	return result
}

func (plugin *SessionPlugin) dependencyArtifacts() map[string]inspect.ArtifactIdentity {
	return map[string]inspect.ArtifactIdentity{
		modelelements.DeploymentRegistryService:          plugin.config.Foreground.Artifact,
		modelelements.PayloadCodecService:                plugin.config.Foreground.WireAdapterArtifact,
		perceptionelements.VisualProviderRegistryService: plugin.config.Visual.Artifact,
		cognitionelements.ProviderRegistryService:        plugin.config.Background.Artifact,
		stateelements.TrajectoryStoreService:             plugin.config.AdapterArtifact,
	}
}

func (plugin *SessionPlugin) mountService(
	ctx context.Context, options legacy.Options, name string,
) (any, error) {
	if ctx == nil {
		return nil, errors.New("create meeting session dependency: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.SessionID) == "" || options.Sink == nil {
		return nil, errors.New("create meeting session dependency: sink and canonical session ID are required")
	}
	switch name {
	case modelelements.DeploymentRegistryService:
		registry := modelelements.NewDeploymentRegistry()
		err := registry.Register(ForegroundDeploymentReference,
			func(sessionCtx context.Context, hello sidecar.Message) (modelelements.Session, error) {
				return plugin.newForegroundSession(sessionCtx, hello, options)
			})
		if err != nil {
			return nil, err
		}
		return registry, nil
	case modelelements.PayloadCodecService:
		return modelelements.NewStandardJSONCodec(), nil
	case perceptionelements.VisualProviderRegistryService:
		registry := perceptionelements.NewVisualProviderRegistry()
		err := registry.Register(VisualProviderReference, plugin.config.Visual.Descriptor,
			func() (perceptionelements.VisualProvider, error) {
				provider, err := plugin.config.Visual.Factory(ctx, cloneLegacyOptions(options))
				if err != nil {
					return nil, err
				}
				if reflectedMeetingNil(provider) {
					return nil, errors.New("meeting visual provider factory returned nil")
				}
				return provider, nil
			})
		if err != nil {
			return nil, err
		}
		return registry, nil
	case cognitionelements.ProviderRegistryService:
		registry := cognitionelements.NewProviderRegistry()
		err := registry.Register(BackgroundProviderReference, plugin.config.Background.Descriptor,
			func() (continuation.Provider, error) {
				provider, err := plugin.config.Background.Factory(ctx, cloneLegacyOptions(options))
				if err != nil {
					return nil, err
				}
				if reflectedMeetingNil(provider) {
					return nil, errors.New("meeting background provider factory returned nil")
				}
				if !reflect.DeepEqual(provider.Descriptor(), plugin.config.Background.Descriptor) {
					if closer, ok := provider.(interface{ Close() error }); ok && !reflectedMeetingNil(closer) {
						_ = closer.Close()
					}
					return nil, errors.New("meeting background provider descriptor drifted")
				}
				return provider, nil
			})
		if err != nil {
			return nil, err
		}
		return registry, nil
	case stateelements.TrajectoryStoreService:
		store, err := plugin.coordinator.prepare(ctx, options.SessionID)
		if err != nil {
			return nil, err
		}
		return &stateelements.TrajectoryStoreServiceValue{Store: store}, nil
	default:
		return nil, fmt.Errorf("meeting session dependency %q is not declared", name)
	}
}

func validateSessionPluginConfig(config SessionPluginConfig) error {
	for label, artifact := range map[string]inspect.ArtifactIdentity{
		"adapter":                 config.AdapterArtifact,
		"foreground dependency":   config.Foreground.Artifact,
		"foreground provider":     config.Foreground.ProviderArtifact,
		"foreground runtime":      config.Foreground.RuntimeArtifact,
		"foreground wire adapter": config.Foreground.WireAdapterArtifact,
		"visual":                  config.Visual.Artifact,
		"background":              config.Background.Artifact,
	} {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("meeting %s artifact: %w", label, err)
		}
	}
	if !canonicalText(config.Foreground.BindingName) || config.Foreground.Factory == nil {
		return errors.New("meeting foreground plugin requires a canonical binding name and factory")
	}
	if err := config.Foreground.Ownership.Validate(); err != nil {
		return fmt.Errorf("meeting foreground ownership: %w", err)
	}
	if err := validateForegroundCapabilities(config.Foreground.Capabilities); err != nil {
		return err
	}
	if err := continuation.ValidateDescriptor(config.Foreground.Descriptor); err != nil {
		return fmt.Errorf("meeting foreground descriptor: %w", err)
	}
	if config.Foreground.Descriptor.Phase != trajectory.PhaseFast ||
		config.Foreground.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose ||
		config.Foreground.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthorityVoice {
		return errors.New("meeting foreground descriptor must be a proposal-only, voiced fast provider")
	}
	if config.Visual.Factory == nil {
		return errors.New("meeting visual plugin requires a factory")
	}
	visualProbe := perceptionelements.NewVisualProviderRegistry()
	if err := visualProbe.Register(VisualProviderReference, config.Visual.Descriptor,
		func() (perceptionelements.VisualProvider, error) {
			return nil, errors.New("metadata validation must not invoke the visual factory")
		}); err != nil {
		return fmt.Errorf("meeting visual descriptor: %w", err)
	}
	if err := continuation.ValidateDescriptor(config.Background.Descriptor); err != nil {
		return fmt.Errorf("meeting background descriptor: %w", err)
	}
	if config.Background.Descriptor.Phase != trajectory.PhaseSlow ||
		config.Background.Descriptor.EffectiveToolAuthority() != continuation.ToolAuthorityPropose ||
		config.Background.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		return errors.New("meeting background descriptor must be a proposal-only, silent slow provider")
	}
	if config.Background.Factory == nil {
		return errors.New("meeting background plugin requires a factory")
	}
	if config.Adapter.FrameRateMilliHz < 0 || config.Adapter.FrameRateMilliHz > 1_000_000 {
		return errors.New("meeting adapter frame rate must be zero or between 1 and 1000000 millihertz")
	}
	return nil
}

func validateForegroundCapabilities(capabilities legacy.Capabilities) error {
	required := legacy.StackCapabilities{
		AudioInput: true, AudioOutput: true, VisualInput: true,
		Transcription: true, TurnGeneration: true, ConcurrentIO: true, TextInjection: true,
	}
	if !capabilities.Stack.Satisfies(required) {
		return fmt.Errorf("meeting foreground stack misses %v", capabilities.Stack.Missing(required))
	}
	if !capabilities.Video || !capabilities.Observations || !capabilities.ManualTurns {
		return errors.New("meeting foreground requires video, observations, and manual-turn support")
	}
	if capabilities.MaxOutputTokens < 0 {
		return errors.New("meeting foreground maximum output tokens cannot be negative")
	}
	if capabilities.Voice.InForce != "" && !canonicalText(capabilities.Voice.InForce) {
		return errors.New("meeting foreground voice is not canonical")
	}
	seen := make(map[string]struct{}, len(capabilities.Observers))
	for _, observer := range capabilities.Observers {
		if !canonicalText(observer) {
			return errors.New("meeting foreground observer name is not canonical")
		}
		if _, duplicate := seen[observer]; duplicate {
			return fmt.Errorf("meeting foreground observer %q is duplicated", observer)
		}
		seen[observer] = struct{}{}
	}
	return nil
}

func cloneSessionPluginConfig(source SessionPluginConfig) SessionPluginConfig {
	result := source
	result.Adapter.Status.Observers = slices.Clone(source.Adapter.Status.Observers)
	result.Foreground.Capabilities.Observers = slices.Clone(source.Foreground.Capabilities.Observers)
	return result
}

func cloneLegacyOptions(source legacy.Options) legacy.Options {
	result := source
	result.Settings = legacy.CloneSettings(source.Settings)
	if source.Policies != nil {
		policies := *source.Policies
		result.Policies = &policies
	}
	return result
}

func reflectedMeetingNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type pendingMeetingStore struct {
	store *trajectory.Store
	stop  func() bool
}

type meetingStoreCoordinator struct {
	mu      sync.Mutex
	pending map[string]*pendingMeetingStore
}

func newMeetingStoreCoordinator() *meetingStoreCoordinator {
	return &meetingStoreCoordinator{pending: make(map[string]*pendingMeetingStore)}
}

func (coordinator *meetingStoreCoordinator) prepare(
	ctx context.Context, sessionID string,
) (*trajectory.Store, error) {
	if coordinator == nil || ctx == nil || !canonicalText(sessionID) {
		return nil, errors.New("prepare meeting trajectory store: context and canonical session ID are required")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if _, duplicate := coordinator.pending[sessionID]; duplicate {
		return nil, errors.New("prepare meeting trajectory store: session store was requested twice")
	}
	entry := &pendingMeetingStore{store: trajectory.NewStore()}
	entry.stop = context.AfterFunc(ctx, func() { coordinator.discard(sessionID, entry) })
	coordinator.pending[sessionID] = entry
	return entry.store, nil
}

func (coordinator *meetingStoreCoordinator) take(sessionID string) (*trajectory.Store, error) {
	if coordinator == nil || !canonicalText(sessionID) {
		return nil, errors.New("take meeting trajectory store: canonical session ID is required")
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	entry := coordinator.pending[sessionID]
	if entry == nil {
		return nil, errors.New("take meeting trajectory store: graph did not select the shared store dependency")
	}
	delete(coordinator.pending, sessionID)
	entry.stop()
	return entry.store, nil
}

func (coordinator *meetingStoreCoordinator) discard(sessionID string, wanted *pendingMeetingStore) {
	coordinator.mu.Lock()
	if coordinator.pending[sessionID] == wanted {
		delete(coordinator.pending, sessionID)
	}
	coordinator.mu.Unlock()
}
