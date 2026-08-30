package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const (
	// SecretServiceName is the explicit, node-scoped mount service used to
	// resolve reviewed deployment secret slots. It is runtime-owned so a
	// deployment ServiceSet cannot shadow it with an unsealed implementation.
	SecretServiceName = "runtime.secrets"

	maximumMountedSecretHandles = 4_096
)

var ErrSecretResolution = errors.New("secret provider resolution failed")

// SecretAccess resolves only the slot names bound to the calling node. The
// deployment reference and provider locator are deliberately absent from this
// API; callers receive an erasable handle and safe provider evidence only.
type SecretAccess interface {
	Resolve(context.Context, string) (*graphsecret.Value, SecretResolution, error)
}

// SecretResolution is safe to retain in inspection and benchmark evidence.
// BindingFingerprint covers provider plus locator without revealing either
// the locator or the secret reference. It never hashes credential bytes.
type SecretResolution struct {
	BindingFingerprint string
	Provider           string
	ProviderRuntime    inspect.ArtifactIdentity
}

type nodeSecretAccess struct {
	node      string
	bindings  map[string]string
	store     *graphsecret.Store
	lifecycle element.Lifecycle
	mounted   *Mounted

	mu       sync.Mutex
	attempts int
}

func (access *nodeSecretAccess) Resolve(
	ctx context.Context,
	slot string,
) (*graphsecret.Value, SecretResolution, error) {
	if access == nil || access.store == nil || access.lifecycle == nil || access.mounted == nil {
		return nil, SecretResolution{}, errors.New("resolve secret slot: service is unavailable")
	}
	if ctx == nil {
		return nil, SecretResolution{}, errors.New("resolve secret slot: nil context")
	}
	if err := canonicalRuntimeText("secret slot", slot); err != nil {
		return nil, SecretResolution{}, err
	}
	reference, found := access.bindings[slot]
	if !found {
		return nil, SecretResolution{}, fmt.Errorf("resolve secret slot %q: slot is not bound for this node", slot)
	}

	access.mu.Lock()
	if access.attempts >= maximumMountedSecretHandles {
		access.mu.Unlock()
		return nil, SecretResolution{}, fmt.Errorf(
			"resolve secret slot %q: mount handle limit %d reached",
			slot,
			maximumMountedSecretHandles,
		)
	}
	access.attempts++
	access.mu.Unlock()

	value, resolution, err := access.store.Resolve(ctx, reference)
	if err != nil {
		// Store/provider errors can contain a private reference or a
		// provider-authored locator. Preserve cancellation semantics, but do
		// not carry the external error text across the node-scoped boundary.
		switch {
		case errors.Is(err, context.Canceled):
			return nil, SecretResolution{}, context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return nil, SecretResolution{}, context.DeadlineExceeded
		default:
			return nil, SecretResolution{}, fmt.Errorf("resolve secret slot %q: %w", slot, ErrSecretResolution)
		}
	}
	if resolution.Reference != reference {
		_ = value.Close()
		return nil, SecretResolution{}, fmt.Errorf("resolve secret slot %q: catalog returned inconsistent evidence", slot)
	}
	safe := SecretResolution{
		BindingFingerprint: resolution.BindingDigest,
		Provider:           resolution.Provider,
		ProviderRuntime:    resolution.ProviderRuntime,
	}
	if err := access.lifecycle.Defer("secret:"+slot, func(context.Context) error {
		return value.Close()
	}); err != nil {
		_ = value.Close()
		return nil, SecretResolution{}, fmt.Errorf("resolve secret slot %q: register erasure: %w", slot, err)
	}
	if err := access.mounted.recordSecretResolution(inspect.SecretProviderEvidence{
		Node: access.node, Slot: slot, BindingFingerprint: safe.BindingFingerprint,
		Provider: safe.Provider, Runtime: safe.ProviderRuntime,
	}); err != nil {
		_ = value.Close()
		return nil, SecretResolution{}, fmt.Errorf("resolve secret slot %q: retain provider evidence: %w", slot, err)
	}
	return value, safe, nil
}

// nodeMountServices exposes runtime.secrets only to the node whose private
// binding map backs the access object. Other services retain their mount-wide
// revisions and values.
type nodeMountServices struct {
	base    element.Services
	secrets SecretAccess
}

func (services nodeMountServices) Lookup(name string) (any, uint64, bool) {
	if name == SecretServiceName {
		if services.secrets == nil {
			return nil, 0, false
		}
		return services.secrets, 1, true
	}
	if services.base == nil {
		return nil, 0, false
	}
	return services.base.Lookup(name)
}

func (mounted *Mounted) recordSecretResolution(evidence inspect.SecretProviderEvidence) error {
	if mounted == nil {
		return errors.New("nil mounted graph")
	}
	mounted.liveMu.Lock()
	if mounted.deploymentEvidence == nil {
		mounted.liveMu.Unlock()
		return errors.New("mounted graph has no deployment evidence")
	}
	for _, previous := range mounted.deploymentEvidence.Secrets {
		if previous.Node != evidence.Node || previous.Slot != evidence.Slot {
			continue
		}
		mounted.liveMu.Unlock()
		if previous != evidence {
			return errors.New("secret provider evidence changed within one mount")
		}
		return nil
	}
	candidate := mounted.deploymentEvidence.Clone()
	candidate.Secrets = append(candidate.Secrets, evidence)
	canonical, err := inspect.CanonicalDeploymentEvidence(candidate)
	if err != nil {
		mounted.liveMu.Unlock()
		return err
	}
	mounted.deploymentEvidence = &canonical
	mounted.liveMu.Unlock()
	if mounted.recorder != nil {
		mounted.recorder.signal()
	}
	return nil
}
