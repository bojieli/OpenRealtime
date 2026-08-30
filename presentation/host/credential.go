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

func (factory *CredentialFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if factory.source == nil {
		return errors.New("realtime credential source is nil")
	}
	if factory.protected && !mount.Permissions.Allows(
		credentialPermissionKind, credentialPermissionResource, credentialPermissionOperation,
	) {
		return errors.New("realtime credential plugin lacks its deployment secret-read grant")
	}
	return mount.Publisher.Provide(presentation.CredentialContract, factory.source)
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
