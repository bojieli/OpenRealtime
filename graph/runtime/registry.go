package runtime

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

type registeredFactory struct {
	factory  element.Factory
	identity element.Identity
	artifact inspect.ArtifactIdentity
	evidence inspect.ResolutionEvidence
}

// Registry maps deployment implementation references to factories. A factory
// is admitted only after its immutable descriptor identity is computed.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]registeredFactory
}

func NewRegistry() *Registry { return &Registry{factories: make(map[string]registeredFactory)} }

func (registry *Registry) Register(implementation string, factory element.Factory) error {
	return registry.register(implementation, inspect.ArtifactIdentity{}, inspect.EvidenceDeclared, factory)
}

// RegisterArtifact binds an element factory to an exact deployment artifact.
// Register remains source-compatible and records only a declaration-level
// identity; production plugins should use this method so live inspection can
// prove which binary/image/module supplied the implementation.
func (registry *Registry) RegisterArtifact(
	implementation string, artifact inspect.ArtifactIdentity, factory element.Factory,
) error {
	return registry.register(implementation, artifact, inspect.EvidenceRegistered, factory)
}

func (registry *Registry) register(
	implementation string, artifact inspect.ArtifactIdentity,
	evidence inspect.ResolutionEvidence, factory element.Factory,
) error {
	if factory == nil {
		return fmt.Errorf("register element implementation %q: nil factory", implementation)
	}
	descriptor := factory.Descriptor()
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("register element implementation %q: %w", implementation, err)
	}
	implementation = strings.TrimSpace(implementation)
	if implementation == "" {
		implementation = identity.Name
	}
	if artifact.ID == "" {
		artifact = inspect.ArtifactIdentity{
			ID: implementation, Digest: identity.Digest,
		}
	} else if err := artifact.Validate(); err != nil {
		return fmt.Errorf("register element implementation %q artifact: %w", implementation, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factories == nil {
		registry.factories = make(map[string]registeredFactory)
	}
	if _, exists := registry.factories[implementation]; exists {
		return fmt.Errorf("element implementation %q is already registered", implementation)
	}
	registry.factories[implementation] = registeredFactory{
		factory: factory, identity: identity, artifact: artifact, evidence: evidence,
	}
	return nil
}

func (registry *Registry) resolve(
	implementation string, identity element.Identity,
) (element.Factory, element.Descriptor, inspect.ArtifactIdentity, inspect.ResolutionEvidence, error) {
	if registry == nil {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", fmt.Errorf("element factory registry is nil")
	}
	if implementation == "" {
		implementation = identity.Name
	}
	registry.mu.RLock()
	registered, found := registry.factories[implementation]
	registry.mu.RUnlock()
	if !found {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", fmt.Errorf("element implementation %q is not registered", implementation)
	}
	// Re-read the descriptor to detect a mutable or stateful factory that no
	// longer matches what registration attested.
	descriptor := registered.factory.Descriptor()
	current, err := descriptor.Identity()
	if err != nil {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", fmt.Errorf("element implementation %q descriptor changed to an invalid value: %w", implementation, err)
	}
	if current != registered.identity {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", fmt.Errorf("element implementation %q descriptor mutated after registration: was %+v, now %+v",
			implementation, registered.identity, current)
	}
	if current != identity {
		return nil, element.Descriptor{}, inspect.ArtifactIdentity{}, "", fmt.Errorf("element implementation %q provides %+v, graph requires %+v",
			implementation, current, identity)
	}
	return registered.factory, descriptor.Clone(), registered.artifact, registered.evidence, nil
}
