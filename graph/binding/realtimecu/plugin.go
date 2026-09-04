package realtimecu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	sessionmedia "github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

var dependencyNames = []string{
	actionelements.ConfirmationRegistryService,
	actionelements.LedgerRegistryService,
	actionelements.TargetRegistryService,
	actionelements.ToolRegistryService,
	actionelements.TrajectoryStoreService,
	cognitionelements.MediaResolverService,
	cognitionelements.ProviderRegistryService,
	policyelements.SemanticDeciderRegistryService,
	stateelements.TrajectoryStoreService,
}

// Plugin is a resource-free, immutable contribution to graph/launch. Its
// factories acquire session-scoped model state only after exact plan
// selection and preflight have succeeded.
type Plugin struct {
	config      PluginConfig
	artifact    inspect.ArtifactIdentity
	coordinator *bundleCoordinator
	adapter     graphlaunch.AdapterPlugin
}

// NewPlugin validates and snapshots an exact Realtime-CU adapter/dependency
// contribution. It does not invoke either provider factory.
func NewPlugin(source PluginConfig) (*Plugin, error) {
	config := clonePluginConfig(source)
	if err := validatePluginConfig(config); err != nil {
		return nil, err
	}
	artifact, err := dependencyArtifact(config)
	if err != nil {
		return nil, err
	}
	coordinator := newBundleCoordinator(config, artifact)
	plugin := &Plugin{config: config, artifact: artifact, coordinator: coordinator}
	plugin.adapter = graphlaunch.AdapterPlugin{
		Reference: AdapterReference, Artifact: config.RuntimeArtifact,
		Bind: func(ctx context.Context, plan *graphconfig.Plan) (graphlaunch.BoundAdapter, error) {
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			adapter, err := bind(plan, plugin)
			if err != nil {
				return graphlaunch.BoundAdapter{}, err
			}
			return graphlaunch.BoundAdapter{
				Profile: adapter.Profile(), Registration: adapter.Registration(),
			}, nil
		},
	}
	return plugin, nil
}

// AdapterPlugin contributes the exact profile binder to a broad launch
// catalog. The returned value closes over the plugin's immutable snapshot.
func (plugin *Plugin) AdapterPlugin() graphlaunch.AdapterPlugin {
	if plugin == nil {
		return graphlaunch.AdapterPlugin{}
	}
	return plugin.adapter
}

// Selection is the explicit graph-launch selector for this adapter.
func (plugin *Plugin) Selection() graphlaunch.AdapterSelection {
	return plugin.AdapterPlugin().Selection(ProfileName, ProfileRevision)
}

// AssemblyDependencies returns the exact mount-scoped service metadata. The
// semantic registry exposes the selected policy artifact directly; the other
// services use the composite identity of the cross-service session bundle.
func (plugin *Plugin) AssemblyDependencies() []graphassembly.Dependency {
	if plugin == nil {
		return nil
	}
	result := make([]graphassembly.Dependency, len(dependencyNames))
	for index, name := range dependencyNames {
		result[index] = graphassembly.Dependency{
			Name: name, Artifact: plugin.dependencyArtifact(name), Scope: graphconfig.DependencyScopeMount,
		}
	}
	return result
}

// MountDependencies returns one exact contribution per selected service, as
// required by graph/launch's duplicate-proof catalog surface.
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

func (plugin *Plugin) dependencyArtifact(name string) inspect.ArtifactIdentity {
	if name == policyelements.SemanticDeciderRegistryService {
		return plugin.config.SettlementPolicy.Artifact
	}
	return plugin.artifact
}

func clonePluginConfig(source PluginConfig) PluginConfig {
	result := source
	result.Observer.Sources = slices.Clone(source.Observer.Sources)
	result.Target.Sources = slices.Clone(source.Target.Sources)
	return result
}

func dependencyArtifact(config PluginConfig) (inspect.ArtifactIdentity, error) {
	payload, err := json.Marshal(struct {
		Runtime                    inspect.ArtifactIdentity                 `json:"runtime"`
		ModelReference             string                                   `json:"model_reference"`
		Model                      inspect.ArtifactIdentity                 `json:"model"`
		ModelAPI                   continuation.Descriptor                  `json:"model_descriptor"`
		SettlementPolicyReference  string                                   `json:"settlement_policy_reference"`
		SettlementPolicy           inspect.ArtifactIdentity                 `json:"settlement_policy"`
		SettlementPolicyDescriptor policyelements.SemanticDeciderDescriptor `json:"settlement_policy_descriptor"`
		Observer                   inspect.ArtifactIdentity                 `json:"observer"`
		ObserverReference          string                                   `json:"observer_reference"`
		ObserverName               string                                   `json:"observer_name"`
		ObserverSources            []string                                 `json:"observer_sources"`
		Target                     any                                      `json:"target"`
	}{
		Runtime: config.RuntimeArtifact, ModelReference: config.Model.Reference,
		Model: config.Model.Artifact, ModelAPI: config.Model.Descriptor,
		SettlementPolicyReference:  config.SettlementPolicy.Reference,
		SettlementPolicy:           config.SettlementPolicy.Artifact,
		SettlementPolicyDescriptor: config.SettlementPolicy.Descriptor,
		Observer:                   config.Observer.Artifact,
		ObserverReference:          config.Observer.Reference, ObserverName: config.Observer.Name,
		ObserverSources: canonicalStrings(config.Observer.Sources), Target: config.Target,
	})
	if err != nil {
		return inspect.ArtifactIdentity{}, fmt.Errorf("fingerprint realtime-CU dependency profile: %w", err)
	}
	digest := sha256.Sum256(payload)
	artifact := inspect.ArtifactIdentity{
		ID:       "profile://openrealtime/realtime-cu/session-dependencies",
		Revision: "v2", Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
	if err := artifact.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	return artifact, nil
}

type sessionBundle struct {
	bridge        *clientBridge
	store         *trajectory.Store
	media         *sessionmedia.MediaStore
	mediaResolver continuation.MediaResolver
	services      map[string]any
}

func newSessionBundle(
	ctx context.Context, options legacy.Options, config PluginConfig,
) (*sessionBundle, error) {
	if ctx == nil {
		return nil, errors.New("realtime-CU mount requires a context")
	}
	if !canonical(options.SessionID) {
		return nil, errors.New("realtime-CU mount requires a canonical session ID")
	}
	bridge, specs, err := newClientBridge(config.Target)
	if err != nil {
		return nil, err
	}
	store := trajectory.NewStore()
	media, err := sessionmedia.NewMediaStore(sessionmedia.MediaConfig{})
	if err != nil {
		return nil, fmt.Errorf("create realtime-CU session media store: %w", err)
	}
	ledger := legacyaction.NewLedger()
	providers := cognitionelements.NewProviderRegistry()
	var modelMu sync.Mutex
	modelOpened := false
	if err := providers.Register(ModelReference, config.Model.Descriptor, func() (continuation.Provider, error) {
		modelMu.Lock()
		if modelOpened {
			modelMu.Unlock()
			return nil, errors.New("realtime-CU model factory was requested more than once in one session")
		}
		modelOpened = true
		modelMu.Unlock()
		provider, factoryErr := config.Model.Factory(ctx, options)
		if factoryErr != nil {
			return nil, factoryErr
		}
		if err := validateProvider(config.Model, provider); err != nil {
			if closer, ok := provider.(interface{ Close() error }); ok && !reflectedNil(closer) {
				_ = closer.Close()
			}
			return nil, err
		}
		return &sessionModel{provider: provider, bridge: bridge, base: config.Model.Descriptor}, nil
	}); err != nil {
		return nil, err
	}
	semanticDeciders := policyelements.NewSemanticDeciderRegistry()
	if err := semanticDeciders.Register(
		SettlementPolicyReference, config.SettlementPolicy.Descriptor,
		func() (policyelements.SemanticDecider, error) {
			if cause := context.Cause(ctx); cause != nil {
				return nil, cause
			}
			return config.SettlementPolicy.Factory(ctx, options)
		},
	); err != nil {
		return nil, err
	}
	tools := actionelements.NewToolRegistries()
	if err := tools.Register(ToolReference, specs); err != nil {
		return nil, err
	}
	targets := actionelements.NewTargetRegistries()
	if err := targets.Register(TargetReference, config.Target); err != nil {
		return nil, err
	}
	ledgers := actionelements.NewLedgerRegistries()
	if err := ledgers.Register(LedgerReference, ledger); err != nil {
		return nil, err
	}
	confirmations := actionelements.NewConfirmationProviders()
	if err := confirmations.Register(ConfirmReference, "deny-unrequested-v1", func() (actionelements.ConfirmationProvider, error) {
		return denyConfirmation{}, nil
	}); err != nil {
		return nil, err
	}
	mediaResolver := continuation.MediaResolver(func(handle string) (continuation.Media, error) {
		if cause := context.Cause(ctx); cause != nil {
			return continuation.Media{}, fmt.Errorf(
				"resolve realtime-CU session media after lifecycle ended: %w", cause,
			)
		}
		resolved, resolveErr := media.Resolve(handle)
		if resolveErr != nil {
			return continuation.Media{}, resolveErr
		}
		if cause := context.Cause(ctx); cause != nil {
			return continuation.Media{}, fmt.Errorf(
				"resolve realtime-CU session media while lifecycle ended: %w", cause,
			)
		}
		return continuation.Media{MIMEType: resolved.Ref.MIMEType, Bytes: resolved.Bytes}, nil
	})
	services := map[string]any{
		actionelements.ConfirmationRegistryService:    confirmations,
		actionelements.LedgerRegistryService:          ledgers,
		actionelements.TargetRegistryService:          targets,
		actionelements.ToolRegistryService:            tools,
		actionelements.TrajectoryStoreService:         store,
		cognitionelements.MediaResolverService:        mediaResolver,
		cognitionelements.ProviderRegistryService:     providers,
		policyelements.SemanticDeciderRegistryService: semanticDeciders,
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{
			Store: store, SessionID: options.SessionID,
		},
	}
	return &sessionBundle{
		bridge: bridge, store: store, media: media, mediaResolver: mediaResolver, services: services,
	}, nil
}

type denyConfirmation struct{}

func (denyConfirmation) Name() string { return "deny-unrequested-v1" }
func (denyConfirmation) Confirm(context.Context, legacyaction.ConfirmationRequest) (bool, error) {
	return false, nil
}

type pendingBundle struct {
	bundle    *sessionBundle
	delivered map[string]struct{}
	stop      func() bool
}

type bundleCoordinator struct {
	mu       sync.Mutex
	config   PluginConfig
	artifact inspect.ArtifactIdentity
	pending  map[string]*pendingBundle
}

func newBundleCoordinator(config PluginConfig, artifact inspect.ArtifactIdentity) *bundleCoordinator {
	return &bundleCoordinator{config: config, artifact: artifact, pending: make(map[string]*pendingBundle)}
}

func (coordinator *bundleCoordinator) service(
	ctx context.Context, options legacy.Options, name string,
) (any, error) {
	if ctx == nil {
		return nil, errors.New("realtime-CU mount dependency: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !slices.Contains(dependencyNames, name) {
		return nil, fmt.Errorf("realtime-CU mount dependency %q is not declared", name)
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
		entry.stop = context.AfterFunc(ctx, func() {
			coordinator.discard(options.SessionID, entry)
		})
		coordinator.pending[options.SessionID] = entry
	}
	if _, duplicate := entry.delivered[name]; duplicate {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("realtime-CU mount dependency %q was acquired twice", name)
	}
	service := entry.bundle.services[name]
	if service == nil {
		coordinator.mu.Unlock()
		return nil, fmt.Errorf("realtime-CU mount dependency %q has no service", name)
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
		return nil, errors.New("realtime-CU adapter has no prepared session dependency bundle")
	}
	if len(entry.delivered) != len(dependencyNames) {
		return nil, fmt.Errorf("realtime-CU adapter dependency bundle has %d/%d services",
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
		wanted.bundle.bridge.Close(errors.New("realtime-CU session ended before adapter construction"))
	}
	coordinator.mu.Unlock()
}

var _ legacyaction.Confirmer = denyConfirmation{}
var _ legacyaction.Dispatcher = (*clientBridge)(nil)
var _ continuation.Provider = (*sessionModel)(nil)
var _ graphbinding.SessionAdapter = (*session)(nil)
