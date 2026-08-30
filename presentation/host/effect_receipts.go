package host

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

// EffectReceiptAuthorityFactory is the descriptor-visible host verifier for
// server-issued client-effect receipts. EffectsFactory consumes only the
// EffectAuthorityContract, so deployments can replace this verifier without
// replacing declarations, routes, stores, confirmation, or executors.
type EffectReceiptAuthorityFactory struct {
	descriptor plugin.Descriptor
	provider   effectauthority.EffectReceiptVerifierProvider
	identity   string
	closeOnce  sync.Once
	closeErr   error
}

// NewEffectReceiptAuthorityFactory locks one lifecycle-aware verifier behind
// an independently selectable plugin descriptor. The provider identity is
// non-secret and is checked again for every receipt to reject live drift.
func NewEffectReceiptAuthorityFactory(
	provider effectauthority.EffectReceiptVerifierProvider,
) (*EffectReceiptAuthorityFactory, error) {
	if nilEffectReceiptProvider(provider) {
		return nil, errors.New("presentation effect-receipt authority requires a verifier provider")
	}
	identity := strings.TrimSpace(provider.Identity())
	if identity == "" || identity != provider.Identity() || len(identity) > 512 {
		return nil, errors.New("presentation effect-receipt verifier identity is not canonical")
	}
	descriptor, err := (plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.effect-receipt-authority", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:  []plugin.Contract{presentation.EffectAuthorityContract},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}).Canonical()
	if err != nil {
		return nil, fmt.Errorf("presentation effect-receipt authority descriptor: %w", err)
	}
	return &EffectReceiptAuthorityFactory{
		descriptor: descriptor, provider: provider, identity: identity,
	}, nil
}

func (factory *EffectReceiptAuthorityFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *EffectReceiptAuthorityFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if factory == nil || nilEffectReceiptProvider(factory.provider) {
		return errors.New("presentation effect-receipt authority has no verifier provider")
	}
	if factory.provider.Identity() != factory.identity {
		return errors.New("presentation effect-receipt verifier identity drifted before mount")
	}
	if err := mount.Lifecycle.Defer("effect-receipt-key", func(context.Context) error {
		factory.closeOnce.Do(func() { factory.closeErr = factory.provider.Close() })
		return factory.closeErr
	}); err != nil {
		return err
	}
	service := &effectReceiptAuthority{provider: factory.provider, identity: factory.identity}
	return mount.Publisher.Provide(presentation.EffectAuthorityContract, EffectAuthority(service))
}

type effectReceiptAuthority struct {
	provider effectauthority.EffectReceiptVerifierProvider
	identity string
}

func (authority *effectReceiptAuthority) Authorize(
	ctx context.Context, request EffectAuthorityRequest,
) (EffectAuthorityDecision, error) {
	if authority == nil || nilEffectReceiptProvider(authority.provider) ||
		authority.identity == "" || authority.provider.Identity() != authority.identity {
		return EffectAuthorityDecision{}, errors.New("effect-receipt verifier provider drifted")
	}
	claims := effectauthority.EffectReceiptClaims{
		SessionID: request.SessionID, CallID: request.CallID, Name: request.Name,
		ArgumentsDigest: request.ArgumentsDigest, DeclarationDigest: request.DeclarationDigest,
		Target: request.Target,
	}
	if err := authority.provider.VerifyEffectReceipt(ctx, request.Evidence, claims); err != nil {
		return EffectAuthorityDecision{}, err
	}
	if authority.provider.Identity() != authority.identity {
		return EffectAuthorityDecision{}, errors.New("effect-receipt verifier provider drifted during verification")
	}
	return EffectAuthorityDecision{
		SessionID: claims.SessionID, CallID: claims.CallID, Name: claims.Name,
		ArgumentsDigest: claims.ArgumentsDigest, DeclarationDigest: claims.DeclarationDigest,
		Target: claims.Target,
	}, nil
}

func nilEffectReceiptProvider(value effectauthority.EffectReceiptVerifierProvider) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ pluginruntime.Factory = (*EffectReceiptAuthorityFactory)(nil)
var _ EffectAuthority = (*effectReceiptAuthority)(nil)
