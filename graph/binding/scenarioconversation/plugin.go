package scenarioconversation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var dependencyNames = []string{
	actionelements.ConfirmationRegistryService,
	actionelements.ClientToolResultRendezvousService,
	actionelements.LedgerRegistryService,
	actionelements.TargetRegistryService,
	actionelements.ToolRegistryService,
	actionelements.TrajectoryStoreService,
	cognitionelements.MediaResolverService,
	cognitionelements.ProviderRegistryService,
	perceptionelements.ASRProviderRegistryService,
	policyelements.SemanticDeciderRegistryService,
	speechelements.IrreversibilityLedgerService,
	speechelements.PlaybackSinkRegistryService,
	speechelements.TTSProviderRegistryService,
	stateelements.TrajectoryStoreService,
}

// Plugin is a resource-free launch contribution. Its coordinator constructs
// exactly one cross-service bundle per Start call and never at profile decode,
// application resolution, plan creation, or preflight.
type Plugin struct {
	config      PluginConfig
	coordinator *bundleCoordinator
	adapter     graphlaunch.AdapterPlugin
}

func NewPlugin(source PluginConfig) (*Plugin, error) {
	config, err := NormalizePluginConfig(source)
	if err != nil {
		return nil, err
	}
	plugin := &Plugin{config: config}
	plugin.coordinator = newBundleCoordinator(config)
	plugin.adapter = graphlaunch.AdapterPlugin{
		Reference: AdapterReference, Artifact: config.RuntimeArtifact,
		Bind: func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			if ctx == nil {
				return graphlaunch.BoundAdapter{}, errors.New("bind scenario conversation adapter: nil context")
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			bound, err := bind(plan, plugin)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{Profile: bound.Profile(), Registration: bound.Registration()}, nil
		},
	}
	return plugin, nil
}

func (plugin *Plugin) AdapterPlugin() graphlaunch.AdapterPlugin {
	if plugin == nil {
		return graphlaunch.AdapterPlugin{}
	}
	return plugin.adapter
}

func (plugin *Plugin) Selection() graphlaunch.AdapterSelection {
	return plugin.AdapterPlugin().Selection(ProfileName, ProfileRevision)
}

func (plugin *Plugin) AssemblyDependencies() []graphassembly.Dependency {
	if plugin == nil {
		return nil
	}
	result := make([]graphassembly.Dependency, len(dependencyNames))
	for index, name := range dependencyNames {
		result[index] = graphassembly.Dependency{
			Name: name, Artifact: plugin.dependencyArtifact(name),
			Scope: graphconfig.DependencyScopeMount,
		}
	}
	return result
}

func (plugin *Plugin) MountDependencies() []graphlaunch.MountDependencyPlugin {
	if plugin == nil {
		return nil
	}
	result := make([]graphlaunch.MountDependencyPlugin, len(dependencyNames))
	for index, name := range dependencyNames {
		name := name
		result[index] = graphlaunch.MountDependencyPlugin{
			Name: name, Artifact: plugin.dependencyArtifact(name),
			Factory: func(ctx context.Context, options legacy.Options) ([]graphruntime.PreparedMountDependency, error) {
				service, err := plugin.coordinator.service(ctx, options, name)
				if err != nil {
					return nil, err
				}
				return []graphruntime.PreparedMountDependency{{
					Name: name, Artifact: plugin.dependencyArtifact(name), Service: service,
				}}, nil
			},
		}
	}
	return result
}

// dependencyArtifact binds the three credential/resource-owning provider
// registries to the exact provider plugin artifacts selected by the
// application. The remaining registries and session-local stores are one
// implementation bundle and retain its separately pinned artifact identity.
func (plugin *Plugin) dependencyArtifact(name string) inspect.ArtifactIdentity {
	switch name {
	case cognitionelements.ProviderRegistryService:
		return plugin.config.Model.Artifact
	case policyelements.SemanticDeciderRegistryService:
		return plugin.config.Policy.Artifact
	case perceptionelements.ASRProviderRegistryService:
		return plugin.config.ASR.Artifact
	case speechelements.TTSProviderRegistryService:
		return plugin.config.TTS.Artifact
	default:
		return plugin.config.DependencyArtifact
	}
}

type sessionBundle struct {
	bridge       *clientBridge
	media        *mediaResolverBridge
	presentation *presentationState
	playback     *sessionPlaybackSink
	store        *trajectory.Store
	services     map[string]any
}

func newSessionBundle(
	ctx context.Context, options legacy.Options, config PluginConfig,
) (*sessionBundle, error) {
	if ctx == nil || !canonicalIdentity(options.SessionID) || options.Sink == nil {
		return nil, errors.New("scenario conversation mount requires context, sink, and canonical session ID")
	}
	bridge := newClientBridge()
	mediaBridge, err := newMediaResolverBridge(options.SessionID, config.Media)
	if err != nil {
		return nil, err
	}
	store := trajectory.NewStore()
	presentation := newPresentationState(options.Settings)
	actionLedger := legacyaction.NewLedger()
	speechLedger := legacyaction.NewLedger()

	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register(ModelReference, config.Model.Descriptor, func() (continuation.Provider, error) {
		return config.Model.Factory(ctx, options)
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	if err := providers.Register(SilentModelReference, config.SilentModel.Descriptor, func() (continuation.Provider, error) {
		return config.SilentModel.Factory(ctx, options)
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	semanticDeciders := policyelements.NewSemanticDeciderRegistry()
	if err := semanticDeciders.Register(PolicyReference, config.Policy.Descriptor, func() (policyelements.SemanticDecider, error) {
		return config.Policy.Factory(ctx, options)
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	asrProviders := perceptionelements.NewASRProviderRegistry()
	if err := asrProviders.Register(ASRReference, cloneV1Descriptor(config.ASR.Descriptor), func() (v1.PerceptionProvider, error) {
		return config.ASR.Factory(ctx, options)
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	ttsProviders := speechelements.NewTTSProviderRegistry()
	if err := ttsProviders.Register(TTSReference, cloneV1Descriptor(config.TTS.Descriptor), func() (v1.SpeechProvider, error) {
		return config.TTS.Factory(ctx, options)
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	playback := speechelements.NewPlaybackSinkRegistry()
	playbackDescriptor := scenarioPlaybackDescriptor()
	playbackSink := newSessionPlaybackSink(ctx, options.Sink, playbackDescriptor, presentation)
	if err := playback.Register(PlaybackReference, playbackDescriptor, func() (speechelements.PlaybackSink, error) {
		return playbackSink, nil
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}

	tools := actionelements.NewToolRegistries()
	specs := make([]legacyaction.ToolSpec, len(config.Tools))
	for index, declaration := range config.Tools {
		specs[index] = legacyaction.ToolSpec{
			Name: declaration.Name, Description: declaration.Description,
			Parameters: slices.Clone(declaration.Parameters), Confirm: declaration.Confirm,
			ArgumentNormalizers: slices.Clone(declaration.ArgumentNormalizers),
			Background:          declaration.Background, Target: declaration.Target, Dispatcher: bridge,
		}
	}
	if err := tools.Register(ToolReference, specs); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	targets := actionelements.NewTargetRegistries()
	if err := targets.Register(TargetReference, config.Target); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	ledgers := actionelements.NewLedgerRegistries()
	if err := ledgers.Register(LedgerReference, actionLedger); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	confirmations := actionelements.NewConfirmationProviders()
	if err := confirmations.Register(ConfirmationReference, "deny-unrequested-v1", func() (actionelements.ConfirmationProvider, error) {
		return denyUnrequestedConfirmation{}, nil
	}); err != nil {
		mediaBridge.Close(err)
		return nil, err
	}
	services := map[string]any{
		actionelements.ConfirmationRegistryService:       confirmations,
		actionelements.ClientToolResultRendezvousService: bridge,
		actionelements.LedgerRegistryService:             ledgers,
		actionelements.TargetRegistryService:             targets,
		actionelements.ToolRegistryService:               tools,
		actionelements.TrajectoryStoreService:            store,
		cognitionelements.MediaResolverService:           continuation.MediaResolver(mediaBridge.Resolve),
		cognitionelements.ProviderRegistryService:        providers,
		perceptionelements.ASRProviderRegistryService:    asrProviders,
		policyelements.SemanticDeciderRegistryService:    semanticDeciders,
		speechelements.IrreversibilityLedgerService:      speechLedger,
		speechelements.PlaybackSinkRegistryService:       playback,
		speechelements.TTSProviderRegistryService:        ttsProviders,
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{
			Store: store, SessionID: options.SessionID,
		},
	}
	return &sessionBundle{
		bridge: bridge, media: mediaBridge, presentation: presentation,
		playback: playbackSink, store: store, services: services,
	}, nil
}

func (bundle *sessionBundle) Close(cause error) error {
	if bundle == nil {
		return nil
	}
	bundle.bridge.Close(cause)
	bundle.media.Close(cause)
	if bundle.playback != nil {
		return bundle.playback.Close()
	}
	return nil
}

type denyUnrequestedConfirmation struct{}

func (denyUnrequestedConfirmation) Name() string { return "deny-unrequested-v1" }
func (denyUnrequestedConfirmation) Confirm(context.Context, legacyaction.ConfirmationRequest) (bool, error) {
	return false, nil
}

type pendingBundle struct {
	bundle    *sessionBundle
	delivered map[string]struct{}
	stop      func() bool
}

type bundleCoordinator struct {
	mu      sync.Mutex
	config  PluginConfig
	pending map[string]*pendingBundle
}

func newBundleCoordinator(config PluginConfig) *bundleCoordinator {
	return &bundleCoordinator{config: config, pending: make(map[string]*pendingBundle)}
}

func (coordinator *bundleCoordinator) service(
	ctx context.Context, options legacy.Options, name string,
) (any, error) {
	if ctx == nil {
		return nil, errors.New("scenario conversation mount dependency: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !slices.Contains(dependencyNames, name) {
		return nil, fmt.Errorf("scenario conversation mount dependency %q is not declared", name)
	}
	coordinator.mu.Lock()
	entry := coordinator.pending[options.SessionID]
	if entry == nil {
		bundle, err := newSessionBundle(ctx, options, coordinator.config)
		if err != nil {
			coordinator.mu.Unlock()
			return nil, err
		}
		entry = &pendingBundle{bundle: bundle, delivered: make(map[string]struct{})}
		entry.stop = context.AfterFunc(ctx, func() { coordinator.discard(options.SessionID, entry) })
		coordinator.pending[options.SessionID] = entry
	}
	if _, duplicate := entry.delivered[name]; duplicate {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("scenario conversation mount dependency %q was acquired twice", name)
	}
	service := entry.bundle.services[name]
	if service == nil {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("scenario conversation mount dependency %q has no service", name)
	}
	entry.delivered[name] = struct{}{}
	coordinator.mu.Unlock()
	return service, nil
}

func (coordinator *bundleCoordinator) take(sessionID string) (*sessionBundle, error) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	entry := coordinator.pending[sessionID]
	if entry == nil {
		return nil, errors.New("scenario conversation adapter has no prepared session dependency bundle")
	}
	if len(entry.delivered) != len(dependencyNames) {
		return nil, fmt.Errorf("scenario conversation dependency bundle has %d/%d services",
			len(entry.delivered), len(dependencyNames))
	}
	delete(coordinator.pending, sessionID)
	entry.stop()
	return entry.bundle, nil
}

func (coordinator *bundleCoordinator) discard(sessionID string, wanted *pendingBundle) {
	coordinator.mu.Lock()
	if coordinator.pending[sessionID] == wanted {
		delete(coordinator.pending, sessionID)
		_ = wanted.bundle.Close(errors.New("scenario conversation session ended before adapter construction"))
	}
	coordinator.mu.Unlock()
}

func cloneV1Descriptor(descriptor v1.Descriptor) v1.Descriptor {
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	return descriptor
}

var _ legacyaction.Confirmer = denyUnrequestedConfirmation{}
var _ graphbinding.SessionAdapter = (*session)(nil)
