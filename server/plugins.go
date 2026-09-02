package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

const (
	sessionProviderPluginName   = "openrealtime.server.session-provider"
	sessionInspectionPluginName = "openrealtime.server.session-inspection-plane"
	realtimeGatewayPluginName   = "openrealtime.server.realtime-gateway"
)

// SessionProviderFactory publishes one already prepared session provider into
// a server realm. The implementation registry—not this descriptor—pins the
// executable artifact and permits a profile to replace the provider without
// changing the gateway plugin.
type SessionProviderFactory struct {
	descriptor plugin.Descriptor
	provider   SessionProvider
}

func NewSessionProviderFactory(provider SessionProvider) (*SessionProviderFactory, error) {
	if nilServerInterface(provider) {
		return nil, errors.New("server session-provider plugin requires a provider")
	}
	if strings.TrimSpace(provider.Name()) == "" {
		return nil, errors.New("server session-provider plugin requires a canonical provider name")
	}
	if err := gateway.ValidateSessionBinding(provider); err != nil {
		return nil, fmt.Errorf("server session-provider plugin contract: %w", err)
	}
	return &SessionProviderFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          sessionProviderPluginName,
			Revision:      2,
			Realm:         plugin.ServerRealm,
			Platforms:     []string{"go"},
			Provides:      []plugin.Contract{SessionProviderContract()},
			Lifecycle:     plugin.Lifecycle{DisposeTimeoutMS: 5_000},
		},
		provider: provider,
	}, nil
}

func (factory *SessionProviderFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *SessionProviderFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	candidate, err := factory.prepareSessionProvider()
	if err != nil {
		return err
	}
	return candidate.Activate(ctx, mount)
}

func (factory *SessionProviderFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return factory.prepareSessionProvider()
}

func (factory *SessionProviderFactory) prepareSessionProvider() (sessionProviderCandidate, error) {
	if factory == nil || nilServerInterface(factory.provider) {
		return sessionProviderCandidate{}, errors.New("mount server session-provider plugin: provider is unavailable")
	}
	if err := gateway.ValidateSessionBinding(factory.provider); err != nil {
		return sessionProviderCandidate{}, fmt.Errorf("mount server session-provider plugin: %w", err)
	}
	return sessionProviderCandidate{provider: factory.provider}, nil
}

type sessionProviderCandidate struct{ provider SessionProvider }

func (candidate sessionProviderCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	return mount.Publisher.Provide(SessionProviderContract(), candidate.provider)
}

// SessionInspectionPlaneFactory publishes one resource-free session
// inspection authority under the exact projections consumed by the
// gateway and canonical management API. Routes remain separate plugins.
type SessionInspectionPlaneFactory struct {
	descriptor plugin.Descriptor
	plane      *gateway.SessionInspectionPlane
}

func NewSessionInspectionPlaneFactory(
	ttl time.Duration,
) (*SessionInspectionPlaneFactory, error) {
	plane, err := gateway.NewSessionInspectionPlane(ttl)
	if err != nil {
		return nil, fmt.Errorf("server session-inspection plane: %w", err)
	}
	return &SessionInspectionPlaneFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          sessionInspectionPluginName,
			Revision:      1,
			Realm:         plugin.ServerRealm,
			Platforms:     []string{"go"},
			Provides: []plugin.Contract{
				SessionInspectionPlaneContract(),
				management.AuthorizerContract,
				management.SessionInspectionContract,
			},
			Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
		},
		plane: plane,
	}, nil
}

func (factory *SessionInspectionPlaneFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *SessionInspectionPlaneFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	candidate, err := factory.prepareSessionInspectionPlane()
	if err != nil {
		return err
	}
	return candidate.Activate(ctx, mount)
}

func (factory *SessionInspectionPlaneFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	return factory.prepareSessionInspectionPlane()
}

func (factory *SessionInspectionPlaneFactory) prepareSessionInspectionPlane() (
	sessionInspectionPlaneCandidate, error,
) {
	if factory == nil || factory.plane == nil {
		return sessionInspectionPlaneCandidate{}, errors.New("mount server session-inspection plugin: plane is unavailable")
	}
	if err := factory.plane.Validate(); err != nil {
		return sessionInspectionPlaneCandidate{}, fmt.Errorf("mount server session-inspection plugin: %w", err)
	}
	return sessionInspectionPlaneCandidate{
		plane:      factory.plane,
		authorizer: factory.plane.Authorizer(),
		sessions:   factory.plane.Sessions(),
	}, nil
}

type sessionInspectionPlaneCandidate struct {
	plane      *gateway.SessionInspectionPlane
	authorizer management.Authorizer
	sessions   management.SessionInspection
}

func (candidate sessionInspectionPlaneCandidate) Activate(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if err := mount.Publisher.Provide(SessionInspectionPlaneContract(), candidate.plane); err != nil {
		return err
	}
	if err := mount.Publisher.Provide(management.AuthorizerContract, candidate.authorizer); err != nil {
		return err
	}
	return mount.Publisher.Provide(management.SessionInspectionContract, candidate.sessions)
}

// GatewayFactoryConfig is private deployment configuration for the clean API
// gateway. Binding must remain nil because the compiled profile supplies the
// exact SessionProvider dependency. Other fields retain gateway's established
// compatibility, limits, telemetry, authorization, and management seams.
type GatewayFactoryConfig struct {
	Gateway gateway.Config
}

// GatewayFactory mounts the OpenRealtime protocol renderer as a replaceable
// server plugin. It opens no socket and serves no UI; the application obtains
// the exported handler only after the whole realm mounts successfully.
type GatewayFactory struct {
	descriptor plugin.Descriptor
	config     gateway.Config
}

func NewGatewayFactory(config GatewayFactoryConfig) (*GatewayFactory, error) {
	if config.Gateway.Binding != nil {
		return nil, errors.New("server gateway plugin binding comes only from its profile dependency")
	}
	if config.Gateway.SessionInspection != nil || !nilServerInterface(config.Gateway.ManagementHandler) {
		return nil, errors.New("server gateway inspection services come only from profile dependencies")
	}
	if config.Gateway.InspectionTokenTTL != 0 {
		return nil, errors.New("server gateway inspection token TTL belongs to the inspection-plane plugin")
	}
	return &GatewayFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          realtimeGatewayPluginName,
			Revision:      3,
			Realm:         plugin.ServerRealm,
			Platforms:     []string{"go"},
			Provides: []plugin.Contract{
				RealtimeEndpointContract(),
				ObservabilityEndpointsContract(),
			},
			Requires: []plugin.Requirement{
				{Contract: SessionProviderContract()},
				{Contract: SessionInspectionPlaneContract()},
				{Contract: management.HTTPHandlerContract},
			},
			Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
		},
		config: config.Gateway,
	}, nil
}

func (factory *GatewayFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *GatewayFactory) Mount(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	if factory == nil {
		return errors.New("mount server gateway plugin: nil factory")
	}
	value, contract, _, _, found := mount.Services.Lookup(SessionProviderContract().Name)
	if !found || contract != SessionProviderContract() {
		return errors.New("mount server gateway plugin: exact session provider is unavailable")
	}
	provider, ok := value.(SessionProvider)
	if !ok || nilServerInterface(provider) {
		return fmt.Errorf("mount server gateway plugin: session provider has type %T", value)
	}
	value, contract, _, _, found = mount.Services.Lookup(SessionInspectionPlaneContract().Name)
	if !found || contract != SessionInspectionPlaneContract() {
		return errors.New("mount server gateway plugin: exact session-inspection plane is unavailable")
	}
	inspection, ok := value.(*gateway.SessionInspectionPlane)
	if !ok || inspection == nil {
		return fmt.Errorf("mount server gateway plugin: session-inspection plane has type %T", value)
	}
	value, contract, _, _, found = mount.Services.Lookup(management.HTTPHandlerContract.Name)
	if !found || contract != management.HTTPHandlerContract {
		return errors.New("mount server gateway plugin: exact canonical management handler is unavailable")
	}
	managementHandler, ok := value.(http.Handler)
	if !ok || nilServerInterface(managementHandler) {
		return fmt.Errorf("mount server gateway plugin: management handler has type %T", value)
	}
	config := factory.config
	config.Binding = provider
	config.SessionInspection = inspection
	config.ManagementHandler = managementHandler
	server, err := gateway.New(config)
	if err != nil {
		return fmt.Errorf("mount server gateway plugin: %w", err)
	}
	if err := mount.Lifecycle.Defer("close-realtime-gateway", func(closeCtx context.Context) error {
		return server.Close(closeCtx)
	}); err != nil {
		return errors.Join(err, server.Close(ctx))
	}
	if err := mount.Publisher.Provide(RealtimeEndpointContract(), RealtimeEndpoint(server)); err != nil {
		return err
	}
	return mount.Publisher.Provide(
		ObservabilityEndpointsContract(), ObservabilityEndpoints(server),
	)
}

func nilServerInterface(value any) bool {
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

var (
	_ pluginruntime.Factory             = (*SessionProviderFactory)(nil)
	_ pluginruntime.CandidatePreMounter = (*SessionProviderFactory)(nil)
	_ pluginruntime.Factory             = (*SessionInspectionPlaneFactory)(nil)
	_ pluginruntime.CandidatePreMounter = (*SessionInspectionPlaneFactory)(nil)
	_ pluginruntime.Factory             = (*GatewayFactory)(nil)
)
