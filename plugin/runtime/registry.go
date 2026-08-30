package runtime

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
)

type registeredFactory struct {
	factory  Factory
	identity plugin.Identity
	artifact inspect.ArtifactIdentity
}

// Registry binds deployment implementation names to exact factories and code
// artifacts. Descriptor resolution remains in plugin.Catalog; this registry is
// the private deployment plane.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]registeredFactory
}

func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]registeredFactory)}
}

// Register records declaration-level implementation evidence.
func (registry *Registry) Register(implementation string, factory Factory) error {
	return registry.register(implementation, inspect.ArtifactIdentity{}, factory)
}

// RegisterArtifact binds an implementation to exact executable/module bytes.
func (registry *Registry) RegisterArtifact(
	implementation string, artifact inspect.ArtifactIdentity, factory Factory,
) error {
	return registry.register(implementation, artifact, factory)
}

func (registry *Registry) register(
	implementation string, artifact inspect.ArtifactIdentity, factory Factory,
) error {
	if registry == nil {
		return fmt.Errorf("register plugin implementation: nil registry")
	}
	if factory == nil {
		return fmt.Errorf("register plugin implementation %q: nil factory", implementation)
	}
	descriptor := factory.Descriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("register plugin implementation %q: %w", implementation, err)
	}
	implementation = strings.TrimSpace(implementation)
	if implementation == "" {
		implementation = identity.Name
	}
	if artifact.ID == "" {
		artifact = inspect.ArtifactIdentity{ID: implementation, Digest: identity.Digest}
	} else if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register plugin implementation %q artifact: %w", implementation, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factories == nil {
		registry.factories = make(map[string]registeredFactory)
	}
	if _, exists := registry.factories[implementation]; exists {
		return fmt.Errorf("plugin implementation %q is already registered", implementation)
	}
	registry.factories[implementation] = registeredFactory{
		factory: factory, identity: identity, artifact: artifact,
	}
	return nil
}

func (registry *Registry) resolve(
	implementation string, identity plugin.Identity,
) (Factory, inspect.ArtifactIdentity, error) {
	if registry == nil {
		return nil, inspect.ArtifactIdentity{}, fmt.Errorf("resolve plugin implementation: nil registry")
	}
	if implementation == "" {
		implementation = identity.Name
	}
	registry.mu.RLock()
	entry, found := registry.factories[implementation]
	registry.mu.RUnlock()
	if !found {
		return nil, inspect.ArtifactIdentity{}, fmt.Errorf("plugin implementation %q is not registered", implementation)
	}
	liveIdentity, err := entry.factory.Descriptor().Identity()
	if err != nil {
		return nil, inspect.ArtifactIdentity{}, fmt.Errorf(
			"plugin implementation %q current descriptor: %w", implementation, err,
		)
	}
	if liveIdentity != entry.identity {
		return nil, inspect.ArtifactIdentity{}, fmt.Errorf(
			"plugin implementation %q changed its descriptor after registration: registered %#v, current %#v",
			implementation, entry.identity, liveIdentity,
		)
	}
	if entry.identity != identity {
		return nil, inspect.ArtifactIdentity{}, fmt.Errorf(
			"plugin implementation %q has descriptor %#v, plan requires %#v",
			implementation, entry.identity, identity,
		)
	}
	return entry.factory, entry.artifact, nil
}
