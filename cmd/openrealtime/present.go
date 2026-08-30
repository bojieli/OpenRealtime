package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	presentationhost "github.com/bojieli/OpenRealtime/presentation/host"
)

type namedPresentationFactory struct {
	id      string
	factory pluginruntime.Factory
}

// runPresent mounts a standalone host and browser-client composition. It uses
// only the public realtime endpoint and does not import gateway or session
// implementation packages.
func runPresent(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime present", flag.ContinueOnError)
	var listen, endpoint, webrtcEndpoint, managementEndpoint, tokenEnv, model, logLevel, clientProfile string
	var shutdownTimeout time.Duration
	flags.StringVar(&listen, "listen", presentation.DefaultLoopbackHostAddress, "presentation host listen address; loopback only")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "public OpenRealtime WebSocket endpoint")
	flags.StringVar(&webrtcEndpoint, "webrtc-endpoint", "", "public WebRTC adapter SDP endpoint used by a WebRTC client profile")
	flags.StringVar(&managementEndpoint, "management-endpoint", "",
		"explicit OpenRealtime management API base used by developer profiles (for example http://127.0.0.1:8765/openrealtime/v1)")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the endpoint bearer token; empty disables authentication")
	flags.StringVar(&model, "model", "", "model identifier requested from the endpoint")
	flags.StringVar(&clientProfile, "client-profile", "browser-minimal",
		"locked observer client profile: browser-minimal, browser-developer, or browser-developer-webrtc")
	flags.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.DurationVar(&shutdownTimeout, "shutdown-timeout", 5*time.Second, "bounded plugin shutdown timeout")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("present accepts flags only")
	}
	if shutdownTimeout <= 0 || shutdownTimeout > 2*time.Minute {
		return errors.New("presentation shutdown timeout must be in (0,2m]")
	}
	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	bundle, err := selectPresentationBundle(clientProfile, webrtcEndpoint)
	if err != nil {
		return err
	}
	router := presentationhost.NewRouterFactory()
	target := presentationhost.NewEndpointDirectoryFactory()
	var relay pluginruntime.Factory
	if clientProfile == "browser-developer-webrtc" {
		relay = presentationhost.NewWebRTCRelayFactory(nil, logger)
	} else {
		relay = presentationhost.NewWebSocketRelayFactory(logger)
	}
	listener := presentationhost.NewLoopbackListenerFactory()
	var credential *presentationhost.CredentialFactory
	token := ""
	if tokenEnv != "" {
		token = os.Getenv(tokenEnv)
	}
	if token == "" {
		credential = presentationhost.NewAnonymousCredentialFactory()
	} else {
		credential, err = presentationhost.NewBearerCredentialFactory(token)
		if err != nil {
			return err
		}
	}
	instances := []namedPresentationFactory{
		{id: "shell", factory: bundle.Shell},
		{id: "manifest", factory: bundle.ManifestHost},
		{id: "modules", factory: bundle.ModuleStore},
		{id: "relay", factory: relay},
		{id: "target", factory: target},
		{id: "credential", factory: credential},
		{id: "listener", factory: listener},
		{id: "router", factory: router},
	}
	if clientProfile == "browser-developer" || clientProfile == "browser-developer-webrtc" {
		instances = append(instances,
			namedPresentationFactory{id: "management-relay", factory: presentationhost.NewManagementRelayFactory(nil, logger)},
		)
	}
	hostProfileName := "openrealtime.host." + clientProfile
	plan, registry, err := compilePresentationHost(hostProfileName, instances)
	if err != nil {
		return err
	}
	targetConfig, err := presentationEndpointConfig(
		clientProfile, endpoint, webrtcEndpoint, managementEndpoint, model,
	)
	if err != nil {
		return err
	}
	targetValues, err := json.Marshal(targetConfig)
	if err != nil {
		return fmt.Errorf("encode presentation endpoint directory: %w", err)
	}
	if err := target.ValidateConfig(targetValues); err != nil {
		return fmt.Errorf("presentation endpoint directory: %w", err)
	}
	listenerValues, _ := json.Marshal(map[string]any{
		"address": listen, "shutdown_timeout_ms": shutdownTimeout.Milliseconds(),
	})
	relayOperation := "websocket"
	if clientProfile == "browser-developer-webrtc" {
		relayOperation = "http"
	}
	permissions := map[string][]plugin.Permission{
		"relay": {{
			Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{relayOperation},
		}},
		"listener": {{
			Kind: "network.listen", Resource: "loopback", Operations: []string{"http"},
		}},
	}
	if clientProfile == "browser-developer" || clientProfile == "browser-developer-webrtc" {
		permissions["management-relay"] = []plugin.Permission{{
			Kind: "network.connect", Resource: "realtime-endpoint", Operations: []string{"management"},
		}}
	}
	if token != "" {
		permissions["credential"] = []plugin.Permission{{
			Kind: "secret.read", Resource: "realtime-credential", Operations: []string{"read"},
		}}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	values := map[string]json.RawMessage{"target": targetValues, "listener": listenerValues}
	mounted, err := pluginruntime.Mount(ctx, pluginruntime.Config{
		Plan: plan, Registry: registry,
		Values:      values,
		Permissions: permissions, ShutdownTimeout: shutdownTimeout,
	})
	if err != nil {
		return err
	}
	value, contract, _, _, err := mounted.Export("listener")
	if err != nil {
		_ = mounted.Close(context.Background())
		return fmt.Errorf("presentation listener export: %w", err)
	}
	if contract != presentation.ListenerContract {
		_ = mounted.Close(context.Background())
		return fmt.Errorf("presentation listener export has contract %s, want %s",
			contract.Name, presentation.ListenerContract.Name)
	}
	info, ok := value.(presentationhost.ListenerInfo)
	if !ok {
		_ = mounted.Close(context.Background())
		return errors.New("presentation listener export has the wrong implementation type")
	}
	fmt.Fprintf(output, "OpenRealtime presentation host on %s\n", info.URL)
	if clientProfile == "browser-developer-webrtc" {
		fmt.Fprintf(output, "  WebRTC         %s\n", webrtcEndpoint)
	} else {
		fmt.Fprintf(output, "  realtime       %s\n", endpoint)
	}
	if managementEndpoint != "" {
		fmt.Fprintf(output, "  management     %s\n", managementEndpoint)
	}
	fmt.Fprintf(output, "  host profile   %s\n", plan.Fingerprint)
	fmt.Fprintf(output, "  client profile %s\n", bundle.Plan.Fingerprint)
	fmt.Fprintf(output, "  client preset  %s\n", clientProfile)
	if token != "" {
		fmt.Fprintln(output, "  credential     protected host-side bearer token")
	} else {
		fmt.Fprintln(output, "  credential     anonymous")
	}
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return mounted.Close(shutdown)
}

// presentationEndpointConfig is the CLI profile's complete public-wire
// directory. It deliberately does not derive management from realtime, or a
// WebRTC endpoint from WebSocket (or vice versa). Endpoint presence therefore
// remains an inspectable deployment capability rather than a UI convention.
func presentationEndpointConfig(
	clientProfile, websocketEndpoint, webrtcEndpoint, managementEndpoint, model string,
) (presentationhost.EndpointDirectoryConfig, error) {
	config := presentationhost.EndpointDirectoryConfig{Model: model}
	switch clientProfile {
	case "browser-minimal", "browser-developer":
		config.Endpoints = append(config.Endpoints, presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: websocketEndpoint,
		})
	case "browser-developer-webrtc":
		config.Endpoints = append(config.Endpoints, presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: webrtcEndpoint,
		})
	default:
		return presentationhost.EndpointDirectoryConfig{}, fmt.Errorf(
			"unknown client profile %q", clientProfile,
		)
	}
	if clientProfile == "browser-developer" || clientProfile == "browser-developer-webrtc" {
		if managementEndpoint == "" {
			return presentationhost.EndpointDirectoryConfig{}, fmt.Errorf(
				"%s requires -management-endpoint", clientProfile,
			)
		}
		config.Endpoints = append(config.Endpoints, presentation.Endpoint{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: managementEndpoint,
		})
	}
	if _, err := presentation.FreezeEndpointDirectory(config.Endpoints); err != nil {
		return presentationhost.EndpointDirectoryConfig{}, fmt.Errorf(
			"%s endpoint directory: %w", clientProfile, err,
		)
	}
	return config, nil
}

// selectPresentationBundle keeps the standalone command's normal profiles
// observer-only. Local effects are a separate deployment composition because
// they require an explicit receipt issuer/verifier authority; a UI preset must
// never manufacture or silently replace that authority.
func selectPresentationBundle(clientProfile, webrtcEndpoint string) (*presentationbrowser.Bundle, error) {
	var (
		bundle *presentationbrowser.Bundle
		err    error
	)
	switch clientProfile {
	case "browser-minimal":
		bundle, err = presentationbrowser.MinimalBundle()
	case "browser-developer":
		bundle, err = presentationbrowser.ObserverDeveloperBundle()
	case "browser-developer-webrtc":
		if webrtcEndpoint == "" {
			return nil, errors.New("browser-developer-webrtc requires -webrtc-endpoint")
		}
		bundle, err = presentationbrowser.ObserverDeveloperWebRTCBundle()
	default:
		return nil, fmt.Errorf("unknown client profile %q", clientProfile)
	}
	if err != nil {
		return nil, fmt.Errorf("build %s bundle: %w", clientProfile, err)
	}
	return bundle, nil
}

func compilePresentationHost(
	profileName string,
	instances []namedPresentationFactory,
) (plugin.Plan, *pluginruntime.Registry, error) {
	if profileName == "" {
		return plugin.Plan{}, nil, errors.New("presentation host profile name is required")
	}
	catalog := plugin.NewCatalog()
	registry := pluginruntime.NewRegistry()
	entries := make([]plugin.ProfileEntry, 0, len(instances))
	seen := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.id == "" || instance.factory == nil {
			return plugin.Plan{}, nil, errors.New("presentation host instance requires an ID and factory")
		}
		if _, duplicate := seen[instance.id]; duplicate {
			return plugin.Plan{}, nil, fmt.Errorf("presentation host repeats instance %q", instance.id)
		}
		seen[instance.id] = struct{}{}
		descriptor := instance.factory.Descriptor()
		if _, err := catalog.Register(descriptor); err != nil {
			return plugin.Plan{}, nil, err
		}
		if err := registry.Register("", instance.factory); err != nil {
			return plugin.Plan{}, nil, err
		}
		entries = append(entries, plugin.ProfileEntry{
			ID: instance.id, Plugin: descriptor.Name, Scope: "root",
		})
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          profileName, Revision: 1,
		Realm: plugin.PresentationHostRealm, Scopes: []plugin.ProfileScope{{Path: "root"}},
		Entries: entries, Exports: []plugin.ProfileExport{
			{Name: "http", Provider: "router", Service: presentation.HTTPHandlerContract.Name},
			{Name: "listener", Provider: "listener", Service: presentation.ListenerContract.Name},
		},
	})
	if err != nil {
		return plugin.Plan{}, nil, err
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return plugin.Plan{}, nil, err
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		return plugin.Plan{}, nil, err
	}
	return plan, registry, nil
}
