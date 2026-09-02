package management

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
)

// DeploymentAuthorizer binds one externally provisioned capability to an
// immutable grant set. It is intended for process-lifetime operator access:
// the deployment owns delivery and rotation, while the server retains only
// the capability's SHA-256 lookup key. Session-scoped capabilities should use
// CapabilityRegistry instead.
type DeploymentAuthorizer struct {
	key    [sha256.Size]byte
	grants []Grant
}

// NewDeploymentAuthorizer seals one canonical mgmt_ capability without
// retaining its plaintext representation.
func NewDeploymentAuthorizer(token string, grants []Grant) (*DeploymentAuthorizer, error) {
	key, valid := capabilityKey(token)
	if !valid {
		return nil, errors.New("create deployment management authority: invalid capability")
	}
	canonical, err := canonicalGrants(grants)
	if err != nil {
		return nil, fmt.Errorf("create deployment management authority: %w", err)
	}
	return &DeploymentAuthorizer{key: key, grants: canonical}, nil
}

func (authorizer *DeploymentAuthorizer) Authorize(
	_ context.Context, request AuthorizationRequest,
) error {
	if authorizer == nil || !validOperation(request.Operation) || request.Resource == "" ||
		request.Resource != strings.TrimSpace(request.Resource) {
		return ErrUnauthorized
	}
	key, valid := capabilityKey(request.Capability)
	if !valid || subtle.ConstantTimeCompare(key[:], authorizer.key[:]) != 1 {
		return ErrUnauthorized
	}
	for _, grant := range authorizer.grants {
		if grant.Operation == request.Operation &&
			(grant.Resource == "*" || grant.Resource == request.Resource) {
			return nil
		}
	}
	return ErrUnauthorized
}

var _ Authorizer = (*DeploymentAuthorizer)(nil)
