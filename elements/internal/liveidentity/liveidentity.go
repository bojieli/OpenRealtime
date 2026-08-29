// Package liveidentity validates and reports exact runtime/provider identities
// for graph-native elements. It is internal to the element catalog so provider
// adapters share one stricter notion of "live" than ordinary symbolic
// deployment references.
package liveidentity

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

// Artifact is one immutable runtime, provider, model, device, or adapter.
type Artifact struct {
	ID       string
	Revision string
	Digest   string
}

func (artifact Artifact) Validate(label string) error {
	identity := inspect.ArtifactIdentity{
		ID: artifact.ID, Revision: artifact.Revision, Digest: artifact.Digest,
	}
	if err := identity.Validate(); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if mutableSelector(artifact.ID) || mutableSelector(artifact.Revision) {
		return fmt.Errorf("%s contains a mutable or placeholder selector", label)
	}
	return nil
}

// Capability constructs one live capability while keeping provider and
// adapter artifacts independently inspectable.
func Capability(name, contract string, provider, adapter Artifact) element.CapabilityResolution {
	return element.CapabilityResolution{
		Name: name, Contract: contract,
		ProviderID: provider.ID, ProviderRevision: provider.Revision,
		ProviderDigest: provider.Digest,
		AdapterID:      adapter.ID, AdapterRevision: adapter.Revision,
		AdapterDigest: adapter.Digest,
	}
}

// Report prevalidates the complete statement before mutating the live view.
// In particular, a bad provider identity cannot leave a node with only its
// runtime marked live.
func Report(
	reporter element.ResolutionReporter, runtime Artifact,
	capabilities []element.CapabilityResolution,
) error {
	if reporter == nil {
		return errors.New("element has no live resolution reporter")
	}
	if err := runtime.Validate("runtime artifact"); err != nil {
		return err
	}
	converted := make([]inspect.CapabilityIdentity, 0, len(capabilities))
	for _, capability := range capabilities {
		provider := Artifact{
			ID: capability.ProviderID, Revision: capability.ProviderRevision,
			Digest: capability.ProviderDigest,
		}
		if err := provider.Validate("capability " + capability.Name + " provider"); err != nil {
			return err
		}
		identity := inspect.CapabilityIdentity{
			Name: capability.Name, Contract: capability.Contract,
			Provider: inspect.ArtifactIdentity{
				ID: provider.ID, Revision: provider.Revision, Digest: provider.Digest,
			},
		}
		adapter := Artifact{
			ID: capability.AdapterID, Revision: capability.AdapterRevision,
			Digest: capability.AdapterDigest,
		}
		if adapter != (Artifact{}) {
			if err := adapter.Validate("capability " + capability.Name + " adapter"); err != nil {
				return err
			}
			identity.Adapter = &inspect.ArtifactIdentity{
				ID: adapter.ID, Revision: adapter.Revision, Digest: adapter.Digest,
			}
		}
		converted = append(converted, identity)
	}
	if _, err := inspect.CanonicalCapabilities(converted); err != nil {
		return fmt.Errorf("live capabilities: %w", err)
	}
	if err := reporter.Runtime(runtime.ID, runtime.Revision, runtime.Digest); err != nil {
		return err
	}
	return reporter.Capabilities(capabilities)
}

func mutableSelector(value string) bool {
	if value == "" {
		return false
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, placeholder := range []string{
		"latest", "current", "unknown", "unresolved", "dynamic", "live",
		"head", "main", "master", "dev", "development", "snapshot", "nightly",
		"pending", "placeholder", "todo", "tbd",
	} {
		if normalized == placeholder || strings.HasSuffix(normalized, ":"+placeholder) ||
			strings.HasSuffix(normalized, "@"+placeholder) ||
			strings.HasSuffix(normalized, "/"+placeholder) {
			return true
		}
	}
	return strings.ContainsAny(normalized, "${}<>*")
}
