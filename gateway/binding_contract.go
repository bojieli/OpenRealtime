package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

// SessionBinding is the protocol renderer's complete session-creation seam.
// Conversation policy, topology, ownership, and capability selection are not
// methods on this interface. Graph-native providers expose those facts through
// their exact frozen SessionAdapterProfile; retained legacy providers are
// admitted only through the separately validated legacy fallback below.
type SessionBinding interface {
	Name() string
	Start(context.Context, legacy.Options) (legacy.Runtime, error)
}

type graphSessionBinding interface {
	SessionBinding
	Graph() ir.Graph
	SessionAdapterProfile() graphbinding.SessionAdapterProfile
}

type sessionBindingContract struct {
	ownership    legacy.Ownership
	capabilities legacy.Capabilities
	graphProfile *graphbinding.SessionAdapterProfile
}

// ValidateSessionBinding validates the immutable, resource-free protocol
// contract of a session provider. Server plugin assembly uses this before it
// publishes a provider; New repeats the validation at the final gateway seam.
func ValidateSessionBinding(source SessionBinding) error {
	_, err := resolveSessionBindingContract(source)
	return err
}

func resolveSessionBindingContract(source SessionBinding) (sessionBindingContract, error) {
	if nilInterface(source) {
		return sessionBindingContract{}, errors.New("session binding is nil")
	}
	name := strings.TrimSpace(source.Name())
	if name == "" || name != source.Name() {
		return sessionBindingContract{}, fmt.Errorf("session binding has invalid name %q", source.Name())
	}
	if native, ok := source.(graphSessionBinding); ok {
		profile := native.SessionAdapterProfile()
		if profile.Name != name {
			return sessionBindingContract{}, fmt.Errorf(
				"graph session binding name %q does not match adapter profile %q", name, profile.Name,
			)
		}
		if err := profile.ValidateGraph(native.Graph()); err != nil {
			return sessionBindingContract{}, fmt.Errorf("graph session binding %s adapter contract: %w", name, err)
		}
		profile = profile.Clone()
		return sessionBindingContract{
			ownership: profile.Ownership, capabilities: cloneBindingCapabilities(profile.Capabilities),
			graphProfile: &profile,
		}, nil
	}

	// Legacy launches remain reachable until the separately tracked Phase 8
	// deletion gate. Resolve their dynamic methods exactly once at gateway
	// construction so one session cannot negotiate a different projection from
	// another merely because a legacy implementation changed its answer.
	compatibility, ok := source.(legacy.Binding)
	if !ok {
		return sessionBindingContract{}, fmt.Errorf(
			"session binding %q exposes neither an exact graph adapter profile nor the retained legacy contract",
			name,
		)
	}
	ownership := compatibility.Ownership().Effective()
	if err := ownership.Validate(); err != nil {
		return sessionBindingContract{}, fmt.Errorf("legacy session binding %s ownership: %w", name, err)
	}
	capabilities := cloneBindingCapabilities(compatibility.Capabilities())
	if capabilities.MaxOutputTokens < 0 {
		return sessionBindingContract{}, fmt.Errorf(
			"legacy session binding %s has negative output-token limit", name,
		)
	}
	return sessionBindingContract{ownership: ownership, capabilities: capabilities}, nil
}

func cloneBindingCapabilities(source legacy.Capabilities) legacy.Capabilities {
	result := source
	result.Observers = slices.Clone(source.Observers)
	return result
}

func (contract sessionBindingContract) clone() sessionBindingContract {
	result := contract
	result.capabilities = cloneBindingCapabilities(contract.capabilities)
	if contract.graphProfile != nil {
		profile := contract.graphProfile.Clone()
		result.graphProfile = &profile
	}
	return result
}
