package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

// TokenResolver resolves one explicitly named secret only after the complete
// application profile and graph plan have passed exact identity validation.
// A launcher normally supplies an environment-backed implementation; the
// server package deliberately does not read process environment implicitly.
type TokenResolver func(context.Context, string) (string, error)

// ProfileGraphBundleConfig composes a strict launch profile with a broad host
// plugin registry and non-behavioral observability hooks. Fields owned by the
// profile (provider, protocol identity, limits, validation, token, inspection
// plane, and effect authority) must remain empty in Gateway.
type ProfileGraphBundleConfig struct {
	Profile      launchprofile.Document
	Applications *launchprofile.Registry
	// GatewayArtifact is the host-installed executable identity. The profile
	// selects it, but cannot attest its own installed bytes.
	GatewayArtifact inspect.ArtifactIdentity
	Gateway         gateway.Config
	ResolveToken    TokenResolver
}

// NewProfileGraphBundle resolves a checked application plugin, seals its
// exact graph-native provider, resolves the optional server token, and then
// compiles the descriptor-locked server realm. It opens no listener and
// acquires no per-session graph resource.
func NewProfileGraphBundle(
	ctx context.Context, config ProfileGraphBundleConfig,
) (*GraphBundle, error) {
	if ctx == nil {
		return nil, errors.New("compose profiled graph server bundle: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := validateProfileGatewayHooks(config.Gateway); err != nil {
		return nil, err
	}
	if err := config.GatewayArtifact.Validate(); err != nil {
		return nil, fmt.Errorf("compose profiled graph server bundle: host gateway artifact: %w", err)
	}
	if config.GatewayArtifact != config.Profile.Server.GatewayArtifact {
		return nil, errors.New("compose profiled graph server bundle: installed gateway artifact drifted from the launch profile")
	}
	launched, err := config.Applications.Resolve(ctx, config.Profile)
	if err != nil {
		return nil, fmt.Errorf("compose profiled graph server bundle: %w", err)
	}

	token := ""
	if environment := config.Profile.Server.TokenEnvironment; environment != "" {
		if config.ResolveToken == nil {
			return nil, errors.New("compose profiled graph server bundle: token environment requires an explicit resolver")
		}
		token, err = config.ResolveToken(ctx, environment)
		if err != nil {
			return nil, fmt.Errorf("compose profiled graph server bundle: resolve token environment %s: %w", environment, err)
		}
		if token == "" || token != strings.TrimSpace(token) || len(token) > 64<<10 ||
			strings.ContainsAny(token, "\x00\r\n") {
			return nil, fmt.Errorf("compose profiled graph server bundle: token environment %s resolved a non-canonical secret", environment)
		}
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}

	profile := config.Profile.Server
	gatewayConfig := config.Gateway
	gatewayConfig.Token = token
	gatewayConfig.Model = profile.Model
	gatewayConfig.TranscriptionModel = profile.TranscriptionModel
	gatewayConfig.ValidateWire = profile.ValidateWire
	gatewayConfig.InspectionTokenTTL = time.Duration(profile.InspectionTokenTTLMS) * time.Millisecond
	gatewayConfig.MaxAudioFrameBytes = profile.MaxAudioFrameBytes
	gatewayConfig.VideoLimits = profile.VideoLimits
	bundle, err := NewBundle(BundleConfig{
		ProfileName: profile.ProfileName, ProfileRevision: profile.ProfileRevision,
		Provider: launched.Binding, Gateway: gatewayConfig,
		ProviderArtifact: profile.ProviderArtifact, GatewayArtifact: config.GatewayArtifact,
	})
	if err != nil {
		return nil, fmt.Errorf("compose profiled graph server bundle: %w", err)
	}
	return &GraphBundle{
		GraphPlan: launched.Plan, ServerBundle: bundle,
		Readiness: launched.Readiness,
	}, nil
}

func validateProfileGatewayHooks(config gateway.Config) error {
	if !nilServerInterface(config.Binding) {
		return errors.New("compose profiled graph server bundle: gateway binding belongs to graph launch")
	}
	if config.Token != "" || config.Model != "" || config.TranscriptionModel != "" ||
		config.ValidateWire || config.InspectionTokenTTL != 0 || config.MaxAudioFrameBytes != 0 ||
		config.VideoLimits.Format != "" || config.VideoLimits.FPSCap != 0 ||
		config.VideoLimits.MaxDimension != 0 || config.VideoLimits.MaxFrameBytes != 0 {
		return errors.New("compose profiled graph server bundle: protocol settings belong to the launch profile")
	}
	if config.SessionInspection != nil || !nilServerInterface(config.ManagementHandler) ||
		config.ServerProfile != nil {
		return errors.New("compose profiled graph server bundle: management and live profile authority belong to the server realm")
	}
	if !nilServerInterface(config.ClientEffectIssuer) {
		return errors.New("compose profiled graph server bundle: client effect authority requires a profile-selected plugin")
	}
	return nil
}
