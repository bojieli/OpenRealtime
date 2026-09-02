package host

import (
	"context"
	"errors"
	"strings"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	credentialPermissionKind      = "secret.read"
	credentialPermissionResource  = "realtime-credential"
	credentialPermissionOperation = "read"
)

// CredentialSource supplies an authorization header only inside the host. A
// manifest, client module, or HTTP response never receives this service.
type CredentialSource interface {
	Authorization(context.Context) (string, error)
}

type staticCredential string

func (credential staticCredential) Authorization(context.Context) (string, error) {
	return string(credential), nil
}

type CredentialFactory struct {
	descriptor plugin.Descriptor
	source     CredentialSource
	protected  bool
}

func NewAnonymousCredentialFactory() *CredentialFactory {
	return newCredentialFactory("openrealtime.presentation.host.anonymous-credential", staticCredential(""), false)
}

func NewBearerCredentialFactory(token string) (*CredentialFactory, error) {
	if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("realtime bearer credential is empty or invalid")
	}
	return newCredentialFactory(
		"openrealtime.presentation.host.secret-credential", staticCredential("Bearer "+token), true,
	), nil
}

func newCredentialFactory(name string, source CredentialSource, protected bool) *CredentialFactory {
	descriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          name, Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.CredentialContract},
	}
	if protected {
		descriptor.Permissions = []plugin.Permission{{
			Kind: credentialPermissionKind, Resource: credentialPermissionResource,
			Operations: []string{credentialPermissionOperation},
		}}
	}
	return &CredentialFactory{descriptor: descriptor, source: source, protected: protected}
}

func (factory *CredentialFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *CredentialFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	candidate, err := factory.prepareCredential(mount.Permissions)
	if err != nil {
		return err
	}
	return candidate.Activate(ctx, mount)
}

func (factory *CredentialFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return factory.prepareCredential(candidate.Permissions)
}

func (factory *CredentialFactory) prepareCredential(
	permissions pluginruntime.Permissions,
) (credentialCandidate, error) {
	if factory.source == nil {
		return credentialCandidate{}, errors.New("realtime credential source is nil")
	}
	if factory.protected && !permissions.Allows(
		credentialPermissionKind, credentialPermissionResource, credentialPermissionOperation,
	) {
		return credentialCandidate{}, errors.New("realtime credential plugin lacks its deployment secret-read grant")
	}
	return credentialCandidate{source: factory.source}, nil
}

type credentialCandidate struct{ source CredentialSource }

func (candidate credentialCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return mount.Publisher.Provide(presentation.CredentialContract, candidate.source)
}

func lookupCredential(services pluginruntime.Services) (CredentialSource, error) {
	value, contract, _, _, found := services.Lookup(presentation.CredentialContract.Name)
	if !found || contract != presentation.CredentialContract {
		return nil, errors.New("presentation realtime credential is unavailable")
	}
	source, ok := value.(CredentialSource)
	if !ok || source == nil {
		return nil, errors.New("presentation realtime credential has the wrong Go type")
	}
	return source, nil
}

var _ pluginruntime.Factory = (*CredentialFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*CredentialFactory)(nil)
