package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/macos"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	presentationbrowser "github.com/bojieli/OpenRealtime/presentation/browser"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

const (
	companionDefaultServerAddress = "127.0.0.1:8765"
	companionDefaultWebRTCAddress = "127.0.0.1:8766"
	companionMaximumReadyBytes    = 1 << 20
)

var errCompanionPermanentReadiness = errors.New(
	"companion readiness response is permanently incompatible",
)

type companionClient string

const (
	companionClientBrowser companionClient = "browser"
	companionClientMacOS   companionClient = "macos"
	companionClientBoth    companionClient = "both"
	companionClientNone    companionClient = "none"
)

type companionOptions struct {
	serverAddress       string
	webRTCAddress       string
	presentationAddress string
	model               string
	tokenEnvironment    string
	client              companionClient
	macOSApplication    string
	readyTimeout        time.Duration
	shutdownTimeout     time.Duration
	serveArguments      []string
}

type companionReady struct {
	ServerURL               string
	WebRTCURL               string
	PresentationURL         string
	ManagementURL           string
	BrowserManifest         presentation.ClientManifest
	NativeEndpointDirectory presentation.EndpointDirectory
	NativeEndpointFile      string
}

type companionRuntime struct {
	executable    string
	prefix        []string
	environment   []string
	goos          string
	httpClient    *http.Client
	childOutput   io.Writer
	launchBrowser func(context.Context, string) error
	launchMacOS   func(context.Context, string, string) error
	onReady       func(companionReady)
}

// runCompanion supervises the existing public server and presentation-host
// commands. The gateway remains presentation-free: browser and native routes
// are mounted by a separate descriptor-locked process.
func runCompanion(arguments []string, output io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve companion executable: %w", err)
	}
	runtime := companionRuntime{
		executable:  executable,
		environment: os.Environ(),
		goos:        runtime.GOOS,
		httpClient: &http.Client{
			Timeout: 2 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		childOutput:   output,
		launchBrowser: launchCompanionBrowser,
		launchMacOS:   launchCompanionMacOS,
	}
	err = runCompanionContext(ctx, arguments, output, runtime)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runCompanionContext(
	ctx context.Context,
	arguments []string,
	output io.Writer,
	runtime companionRuntime,
) (returnErr error) {
	if ctx == nil {
		return errors.New("companion context is required")
	}
	if output == nil {
		return errors.New("companion output is required")
	}
	options, err := parseCompanionOptions(arguments, output)
	if err != nil {
		return err
	}
	runtime, err = normalizeCompanionRuntime(runtime)
	if err != nil {
		return err
	}
	if (options.client == companionClientMacOS || options.client == companionClientBoth) && runtime.goos != "darwin" {
		return errors.New("the macOS companion client can only be launched on macOS; use -client browser or -client none on this host")
	}
	if options.client == companionClientMacOS || options.client == companionClientBoth {
		if err := preflightCompanionMacOSApplication(options.macOSApplication); err != nil {
			return err
		}
	}

	browserBundle, err := presentationbrowser.ObserverDeveloperWebRTCBundle()
	if err != nil {
		return fmt.Errorf("build companion browser profile: %w", err)
	}
	ready := companionReady{
		ServerURL:       "http://" + options.serverAddress,
		WebRTCURL:       "http://" + options.webRTCAddress + "/v1/realtime/calls",
		PresentationURL: "http://" + options.presentationAddress,
		ManagementURL:   "http://" + options.serverAddress + management.APIPrefix,
		BrowserManifest: browserBundle.Manifest.Clone(),
	}
	ready.NativeEndpointDirectory, err = companionNativeEndpointDirectory(ready.PresentationURL)
	if err != nil {
		return err
	}
	if options.presentationAddress != presentation.DefaultLoopbackHostAddress {
		ready.NativeEndpointFile, err = writeCompanionNativeEndpointDirectory(ready.NativeEndpointDirectory)
		if err != nil {
			return err
		}
		defer func() {
			returnErr = errors.Join(returnErr, os.RemoveAll(filepath.Dir(ready.NativeEndpointFile)))
		}()
	}

	serialized := &companionSerializedWriter{writer: runtime.childOutput}
	serverOutput := newCompanionPrefixWriter(serialized, "serve")
	presentationOutput := newCompanionPrefixWriter(serialized, "present")

	// With no explicit server composition, the room uses the same graph-native
	// application as the twelve interaction scenarios. Never fall back to a
	// hosted Realtime provider when a local model service is unavailable.
	cleanupProfile, profiled, err := prepareCompanionPipeline(ctx, &options)
	if err != nil {
		return err
	}
	defer cleanupProfile()
	serverArguments := []string{
		"serve",
		"-listen", options.serverAddress,
		"-webrtc-listen", options.webRTCAddress,
		"-webrtc-allow-origin", ready.PresentationURL,
		"-shutdown-timeout", options.shutdownTimeout.String(),
	}
	if !profiled {
		serverArguments = append(serverArguments, "-model", options.model, "-token-env", options.tokenEnvironment)
	}
	serverArguments = append(serverArguments, options.serveArguments...)
	server, err := startCompanionProcess(runtime, serverArguments, serverOutput)
	if err != nil {
		return fmt.Errorf("start companion server: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, server.stop(options.shutdownTimeout))
		serverOutput.flush()
	}()
	readyContext, cancelReady := context.WithTimeout(ctx, options.readyTimeout)
	defer cancelReady()
	// The children read the credential out of the inherited environment by
	// name; this reads the same variable so the readiness probes can present
	// it. It is never placed in argv or printed.
	credential := strings.TrimSpace(os.Getenv(options.tokenEnvironment))
	if err := waitCompanionHTTPReady(
		readyContext, runtime.httpClient, ready.ServerURL+"/healthz", credential, server,
		func(status int, header http.Header, body []byte) error {
			return validateCompanionServerHealth(status, header, body, options.model)
		},
	); err != nil {
		return fmt.Errorf("companion server readiness: %w", err)
	}
	if err := waitCompanionHTTPReady(
		readyContext, runtime.httpClient, "http://"+options.webRTCAddress+"/healthz", credential, server,
		func(status int, header http.Header, body []byte) error {
			return validateCompanionWebRTCHealth(
				status, header, body, "ws://"+options.serverAddress+"/v1/realtime",
			)
		},
	); err != nil {
		return fmt.Errorf("companion WebRTC readiness: %w", err)
	}

	presentationArguments := []string{
		"present",
		"-listen", options.presentationAddress,
		"-endpoint", "ws://" + options.serverAddress + "/v1/realtime",
		"-webrtc-endpoint", ready.WebRTCURL,
		"-management-endpoint", ready.ManagementURL,
		"-model", options.model,
		"-token-env", options.tokenEnvironment,
		"-client-profile", "browser-developer-webrtc",
		"-native-websocket-relay",
		"-shutdown-timeout", options.shutdownTimeout.String(),
	}
	presentationProcess, err := startCompanionProcess(runtime, presentationArguments, presentationOutput)
	if err != nil {
		return fmt.Errorf("start companion presentation host: %w", err)
	}
	defer func() {
		// Presentation routes must disappear before the server API they target.
		returnErr = errors.Join(presentationProcess.stop(options.shutdownTimeout), returnErr)
		presentationOutput.flush()
	}()
	if err := waitCompanionHTTPReady(
		readyContext, runtime.httpClient, ready.PresentationURL+"/client/v1/manifest",
		// The presentation host is not the Realtime gateway and holds no
		// bearer token of its own; its manifest is served to the local
		// browser.
		"", presentationProcess,
		func(status int, header http.Header, body []byte) error {
			if status != http.StatusOK {
				return fmt.Errorf("manifest status is %d", status)
			}
			mediaType := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
			if mediaType != "application/json" {
				return permanentCompanionReadiness(fmt.Errorf("manifest media type is %q", mediaType))
			}
			manifest, parseErr := presentation.ParseManifest(body)
			if parseErr != nil {
				return permanentCompanionReadiness(fmt.Errorf("parse manifest: %w", parseErr))
			}
			if manifest.Fingerprint != ready.BrowserManifest.Fingerprint ||
				manifest.Plan.Fingerprint != ready.BrowserManifest.Plan.Fingerprint {
				return permanentCompanionReadiness(
					errors.New("served manifest does not match browser-developer-webrtc"),
				)
			}
			return nil
		},
	); err != nil {
		return fmt.Errorf("companion presentation readiness: %w", err)
	}

	fmt.Fprintln(output, "OpenRealtime companion ready")
	fmt.Fprintf(output, "  server       %s/v1/realtime\n", ready.ServerURL)
	fmt.Fprintf(output, "  WebRTC       %s\n", ready.WebRTCURL)
	fmt.Fprintf(output, "  browser      %s\n", ready.PresentationURL)
	fmt.Fprintf(output, "  management   %s\n", ready.ManagementURL)
	fmt.Fprintf(output, "  browser plan %s\n", ready.BrowserManifest.Plan.Fingerprint)
	fmt.Fprintf(output, "  native endpoints %s\n", ready.NativeEndpointDirectory.Fingerprint)
	if ready.NativeEndpointFile != "" {
		fmt.Fprintf(output, "  native file  %s\n", ready.NativeEndpointFile)
	} else {
		fmt.Fprintln(output, "  native file  bundled observer-developer directory")
	}
	fmt.Fprintf(output, "  client       %s\n", options.client)
	if runtime.onReady != nil {
		runtime.onReady(ready)
	}
	if options.client == companionClientBrowser || options.client == companionClientBoth {
		if err := runtime.launchBrowser(ctx, ready.PresentationURL); err != nil {
			return fmt.Errorf("launch browser companion: %w", err)
		}
	}
	if options.client == companionClientMacOS || options.client == companionClientBoth {
		if err := runtime.launchMacOS(ctx, options.macOSApplication, ready.NativeEndpointFile); err != nil {
			return fmt.Errorf("launch macOS companion: %w", err)
		}
	}

	select {
	case <-ctx.Done():
		return nil
	case <-server.done:
		return fmt.Errorf("companion server exited before shutdown: %w", server.result())
	case <-presentationProcess.done:
		return fmt.Errorf("companion presentation host exited before shutdown: %w", presentationProcess.result())
	}
}

func parseCompanionOptions(arguments []string, output io.Writer) (companionOptions, error) {
	var options companionOptions
	supervisor, serve := splitCompanionArguments(arguments)
	flags := flag.NewFlagSet("openrealtime companion", flag.ContinueOnError)
	client := string(companionClientBrowser)
	flags.StringVar(&options.serverAddress, "server-listen", companionDefaultServerAddress,
		"clean Realtime server listen address; loopback only")
	flags.StringVar(&options.webRTCAddress, "webrtc-listen", companionDefaultWebRTCAddress,
		"separate browser WebRTC adapter listen address; loopback only")
	flags.StringVar(&options.presentationAddress, "presentation-listen", presentation.DefaultLoopbackHostAddress,
		"standalone browser/native presentation host listen address; loopback only")
	flags.StringVar(&options.model, "model", "openrealtime", "model identifier shared by server and clients")
	flags.StringVar(&options.tokenEnvironment, "token-env", "OPENREALTIME_TOKEN",
		"environment variable holding the server credential; the value is never placed in argv or output")
	flags.StringVar(&client, "client", client,
		"client launch policy: browser, macos, both, or none")
	flags.StringVar(&options.macOSApplication, "macos-app", "macos/.build/OpenRealtime Developer.app",
		"macOS application bundle used by -client macos or both")
	flags.DurationVar(&options.readyTimeout, "ready-timeout", 2*time.Minute,
		"bounded server, WebRTC, and presentation readiness deadline")
	flags.DurationVar(&options.shutdownTimeout, "shutdown-timeout", 5*time.Second,
		"bounded graceful shutdown deadline for each child")
	flags.SetOutput(output)
	if err := flags.Parse(supervisor); err != nil {
		return companionOptions{}, err
	}
	if flags.NArg() != 0 {
		return companionOptions{}, errors.New("companion serve flags must follow a literal -- separator")
	}
	options.client = companionClient(client)
	switch options.client {
	case companionClientBrowser, companionClientMacOS, companionClientBoth, companionClientNone:
	default:
		return companionOptions{}, fmt.Errorf("unknown companion client %q", client)
	}
	if options.readyTimeout <= 0 || options.readyTimeout > 10*time.Minute {
		return companionOptions{}, errors.New("companion ready timeout must be in (0,10m]")
	}
	if options.shutdownTimeout <= 0 || options.shutdownTimeout > 2*time.Minute {
		return companionOptions{}, errors.New("companion shutdown timeout must be in (0,2m]")
	}
	if err := validateCompanionEnvironmentName(options.tokenEnvironment); err != nil {
		return companionOptions{}, err
	}
	addresses := []struct{ name, value string }{
		{"server", options.serverAddress}, {"WebRTC", options.webRTCAddress},
		{"presentation", options.presentationAddress},
	}
	seen := make(map[string]string, len(addresses))
	for _, address := range addresses {
		canonical, err := validateCompanionLoopbackAddress(address.value)
		if err != nil {
			return companionOptions{}, fmt.Errorf("companion %s address: %w", address.name, err)
		}
		if previous, found := seen[canonical]; found {
			return companionOptions{}, fmt.Errorf("companion %s and %s addresses must be distinct", previous, address.name)
		}
		seen[canonical] = address.name
	}
	if err := rejectCompanionOwnedServeFlags(serve); err != nil {
		return companionOptions{}, err
	}
	options.serveArguments = append([]string(nil), serve...)
	return options, nil
}

// prepareCompanionPipeline keeps model and bearer identity aligned with strict
// profiles; legacy flags must not override a frozen server configuration.
func prepareCompanionPipeline(ctx context.Context, options *companionOptions) (func(), bool, error) {
	noop := func() {}
	explicit := false
	profilePath := ""
	for i, argument := range options.serveArguments {
		name, value, equals := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		if !equals && i+1 < len(options.serveArguments) {
			value = options.serveArguments[i+1]
		}
		switch name {
		case "binding":
			if value == "upstream" {
				return noop, false, errors.New("the companion room requires an OpenRealtime pipeline; hosted upstream bindings are not supported")
			}
			explicit = true
		case "config":
			explicit = true
			payload, err := os.ReadFile(value)
			if err != nil {
				return noop, false, err
			}
			var config map[string]any
			if err := yaml.Unmarshal(payload, &config); err != nil {
				return noop, false, err
			}
			if config["binding"] == "upstream" {
				return noop, false, errors.New("the companion room requires an OpenRealtime pipeline; hosted upstream bindings are not supported")
			}
		case "launch-profile":
			profilePath = value
			explicit = true
		}
	}
	if profilePath != "" {
		profile, err := readServeLaunchProfile(ctx, profilePath)
		if err != nil {
			return noop, false, err
		}
		options.model = profile.Server.Model
		options.tokenEnvironment = profile.Server.TokenEnvironment
		return noop, true, nil
	}
	if explicit {
		return noop, false, nil
	}
	selection := defaultRoomProfileOptions()
	if strings.TrimSpace(os.Getenv(options.tokenEnvironment)) != "" {
		selection.serverTokenEnv = options.tokenEnvironment
	}
	profile, _, err := freezeProductionScenarioProfile(ctx, selection)
	if err != nil {
		return noop, false, fmt.Errorf("prepare scenario room pipeline: %w", err)
	}
	profile.Server.Model = options.model
	profile, err = launchprofile.Freeze(profile)
	if err != nil {
		return noop, false, err
	}
	payload, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		return noop, false, err
	}
	directory, err := os.MkdirTemp("", "openrealtime-room-")
	if err != nil {
		return noop, false, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "scenario.yaml")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		cleanup()
		return noop, false, err
	}
	options.serveArguments = append(options.serveArguments, "-launch-profile", path)
	return cleanup, true, nil
}

func splitCompanionArguments(arguments []string) (supervisor, serve []string) {
	for index, argument := range arguments {
		if argument == "--" {
			return append([]string(nil), arguments[:index]...), append([]string(nil), arguments[index+1:]...)
		}
	}
	return append([]string(nil), arguments...), nil
}

func rejectCompanionOwnedServeFlags(arguments []string) error {
	owned := map[string]struct{}{
		"listen": {}, "webrtc-listen": {}, "webrtc-allow-origin": {},
		"model": {}, "token-env": {}, "shutdown-timeout": {},
	}
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name := strings.TrimLeft(argument, "-")
		if before, _, found := strings.Cut(name, "="); found {
			name = before
		}
		if _, found := owned[name]; found {
			return fmt.Errorf("serve flag %q is owned by companion and cannot follow --", name)
		}
	}
	return nil
}

func validateCompanionEnvironmentName(value string) error {
	if value == "" {
		return nil
	}
	for index, character := range value {
		if (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return errors.New("companion token environment name is invalid")
	}
	return nil
}

func validateCompanionLoopbackAddress(value string) (string, error) {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || port == "" {
		return "", errors.New("must be an explicit host:port")
	}
	address := net.ParseIP(host)
	if address == nil || !address.IsLoopback() {
		return "", errors.New("must use an explicit loopback IP address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || strconv.Itoa(portNumber) != port {
		return "", errors.New("port must be a canonical integer in [1,65535]")
	}
	return net.JoinHostPort(address.String(), port), nil
}

func preflightCompanionMacOSApplication(path string) error {
	if path == "" || path != strings.TrimSpace(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("companion macOS application path is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("preflight companion macOS application: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || filepath.Ext(path) != ".app" {
		return errors.New("companion macOS application must be an exact .app directory, not a symlink")
	}
	return nil
}

type companionServerHealth struct {
	Status   string `json:"status"`
	Model    string `json:"model"`
	Binding  string `json:"binding"`
	Protocol struct {
		OpenAIRealtime string `json:"openai_realtime"`
		OpenRealtime   struct {
			Version uint64 `json:"version"`
		} `json:"openrealtime"`
	} `json:"protocol"`
	ServerProfile *struct {
		FormatVersion uint64       `json:"format_version"`
		Realm         plugin.Realm `json:"realm"`
		State         string       `json:"state"`
		Fingerprint   string       `json:"fingerprint"`
	} `json:"server_profile"`
}

type companionWebRTCHealth struct {
	Status    string `json:"status"`
	Transport string `json:"transport"`
	Codec     string `json:"codec"`
	Endpoint  string `json:"endpoint"`
}

func validateCompanionServerHealth(
	status int, header http.Header, body []byte, model string,
) error {
	if status != http.StatusOK {
		return fmt.Errorf("health status is %d", status)
	}
	if err := validateCompanionJSONMediaType(header, "server health"); err != nil {
		return err
	}
	var health companionServerHealth
	if err := decodeCompanionReadinessJSON(body, &health); err != nil {
		return fmt.Errorf("decode server health: %w", err)
	}
	if health.Status != "ok" || health.Model != model || strings.TrimSpace(health.Binding) == "" ||
		health.Protocol.OpenAIRealtime != "pinned" ||
		health.Protocol.OpenRealtime.Version != openrealtime.Version {
		return errors.New("server health does not attest the expected active server profile and model")
	}
	profile := health.ServerProfile
	if profile == nil || profile.FormatVersion != pluginruntime.LiveFormatVersion ||
		profile.Realm != plugin.ServerRealm || profile.State != "active" ||
		!management.CanonicalDigest(profile.Fingerprint) {
		return errors.New("server health does not contain the exact active server profile")
	}
	return nil
}

func validateCompanionWebRTCHealth(
	status int, header http.Header, body []byte, upstream string,
) error {
	if status != http.StatusOK {
		return fmt.Errorf("WebRTC health status is %d", status)
	}
	if err := validateCompanionJSONMediaType(header, "WebRTC health"); err != nil {
		return err
	}
	var health companionWebRTCHealth
	if err := decodeCompanionReadinessJSON(body, &health); err != nil {
		return fmt.Errorf("decode WebRTC health: %w", err)
	}
	if health.Status != "ok" || health.Transport != "webrtc" ||
		health.Codec != "audio/PCMU" || health.Endpoint != upstream {
		return errors.New("WebRTC health does not attest the expected transport and upstream")
	}
	return nil
}

func validateCompanionJSONMediaType(header http.Header, name string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		return fmt.Errorf("%s media type is %q", name, mediaType)
	}
	return nil
}

func decodeCompanionReadinessJSON(body []byte, destination any) error {
	if err := strictjson.Validate(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("readiness response has a trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func normalizeCompanionRuntime(value companionRuntime) (companionRuntime, error) {
	if value.executable == "" {
		return companionRuntime{}, errors.New("companion executable is required")
	}
	if value.goos == "" {
		value.goos = runtime.GOOS
	}
	if value.httpClient == nil {
		value.httpClient = &http.Client{Timeout: 2 * time.Second}
	}
	copyClient := *value.httpClient
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	value.httpClient = &copyClient
	if value.childOutput == nil {
		value.childOutput = io.Discard
	}
	if value.launchBrowser == nil {
		value.launchBrowser = func(context.Context, string) error { return nil }
	}
	if value.launchMacOS == nil {
		value.launchMacOS = func(context.Context, string, string) error { return nil }
	}
	return value, nil
}

func companionNativeEndpointDirectory(hostURL string) (presentation.EndpointDirectory, error) {
	endpoints := []presentation.Endpoint{
		{
			Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
			URL: strings.Replace(hostURL, "http://", "ws://", 1) + "/client/v1/realtime",
		},
		{
			Name: presentation.EndpointManagement, Protocol: presentation.ProtocolManagement,
			URL: hostURL + "/client/v1/management",
		},
	}
	directory, err := presentation.FreezeEndpointDirectory(endpoints)
	if err != nil {
		return presentation.EndpointDirectory{}, fmt.Errorf("freeze companion native endpoints: %w", err)
	}
	if _, err := macos.NewNativeBundleForDistributionWithEndpointDirectory(
		macos.NativeObserverDeveloperDistribution, directory,
	); err != nil {
		return presentation.EndpointDirectory{}, fmt.Errorf("validate companion native endpoints: %w", err)
	}
	return directory, nil
}

func writeCompanionNativeEndpointDirectory(directory presentation.EndpointDirectory) (string, error) {
	directoryPath, err := os.MkdirTemp("", "openrealtime-companion-")
	if err != nil {
		return "", fmt.Errorf("create native endpoint directory: %w", err)
	}
	path := filepath.Join(directoryPath, "native-observer-endpoints.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(directoryPath)
		return "", fmt.Errorf("create native endpoint file: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	writeErr := encoder.Encode(directory)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	writeErr = errors.Join(writeErr, file.Close())
	if writeErr != nil {
		_ = os.RemoveAll(directoryPath)
		return "", fmt.Errorf("write native endpoint file: %w", writeErr)
	}
	return path, nil
}

type companionProcess struct {
	command *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func startCompanionProcess(
	runtime companionRuntime, arguments []string, output io.Writer,
) (*companionProcess, error) {
	commandArguments := append(append([]string(nil), runtime.prefix...), arguments...)
	command := exec.Command(runtime.executable, commandArguments...)
	command.Env = append([]string(nil), runtime.environment...)
	command.Stdout, command.Stderr = output, output
	configureCompanionProcess(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &companionProcess{command: command, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process, nil
}

func (process *companionProcess) result() error {
	err := process.waitError()
	if err == nil {
		return errors.New("process exited")
	}
	return err
}

func (process *companionProcess) waitError() error {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.err
}

func (process *companionProcess) stop(timeout time.Duration) error {
	select {
	case <-process.done:
		return process.killGroup()
	default:
	}
	if err := interruptCompanionProcess(process.command.Process); err != nil &&
		!errors.Is(err, os.ErrProcessDone) && !companionProcessMissing(err) {
		return fmt.Errorf("interrupt process: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
		return errors.Join(process.waitError(), process.killGroup())
	case <-timer.C:
		if err := process.killGroup(); err != nil {
			return fmt.Errorf("kill process after shutdown timeout: %w", err)
		}
		<-process.done
		return errors.New("process exceeded companion shutdown timeout and was killed")
	}
}

func (process *companionProcess) killGroup() error {
	err := killCompanionProcess(process.command.Process)
	if err == nil || errors.Is(err, os.ErrProcessDone) || companionProcessMissing(err) {
		return nil
	}
	return err
}

// waitCompanionHTTPReady polls a child's readiness endpoint until it attests
// what this launch expects.
//
// The credential is presented rather than omitted because the health payload
// the validators read - the model, the active server profile, its fingerprint -
// is only served to an authorized caller once a token is configured. A
// companion that launched its children with a token and then probed them
// anonymously would read the liveness half of the answer, find no profile in
// it, and report a readiness failure for a server that was ready.
func waitCompanionHTTPReady(
	ctx context.Context,
	client *http.Client,
	url string,
	credential string,
	process *companionProcess,
	validate func(int, http.Header, []byte) error,
) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "application/json")
		if credential != "" {
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, companionMaximumReadyBytes+1))
			closeErr := response.Body.Close()
			switch {
			case readErr != nil:
				lastErr = readErr
			case closeErr != nil:
				lastErr = closeErr
			case len(body) > companionMaximumReadyBytes:
				lastErr = errors.New("readiness response exceeds limit")
			default:
				lastErr = validate(response.StatusCode, response.Header.Clone(), body)
				if lastErr == nil {
					select {
					case <-process.done:
						return fmt.Errorf("process exited while publishing readiness: %w", process.result())
					default:
						return nil
					}
				}
				if errors.Is(lastErr, errCompanionPermanentReadiness) {
					return lastErr
				}
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			return errors.Join(ctx.Err(), lastErr)
		case <-process.done:
			return fmt.Errorf("process exited before readiness: %w", process.result())
		case <-ticker.C:
		}
	}
}

func permanentCompanionReadiness(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", errCompanionPermanentReadiness, err)
}

type companionSerializedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *companionSerializedWriter) write(value []byte) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_, _ = writer.writer.Write(value)
}

type companionPrefixWriter struct {
	mu      sync.Mutex
	sink    *companionSerializedWriter
	prefix  string
	pending []byte
}

func newCompanionPrefixWriter(sink *companionSerializedWriter, prefix string) *companionPrefixWriter {
	return &companionPrefixWriter{sink: sink, prefix: "[" + prefix + "] "}
}

func (writer *companionPrefixWriter) Write(value []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending = append(writer.pending, value...)
	for {
		index := strings.IndexByte(string(writer.pending), '\n')
		if index < 0 {
			break
		}
		line := append([]byte(writer.prefix), writer.pending[:index+1]...)
		writer.sink.write(line)
		writer.pending = append(writer.pending[:0], writer.pending[index+1:]...)
	}
	return len(value), nil
}

func (writer *companionPrefixWriter) flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.pending) == 0 {
		return
	}
	line := append([]byte(writer.prefix), writer.pending...)
	line = append(line, '\n')
	writer.sink.write(line)
	writer.pending = nil
}

func launchCompanionBrowser(ctx context.Context, endpoint string) error {
	var name string
	var arguments []string
	switch runtime.GOOS {
	case "darwin":
		name, arguments = "open", []string{endpoint}
	case "linux":
		name, arguments = "xdg-open", []string{endpoint}
	case "windows":
		name, arguments = "rundll32", []string{"url.dll,FileProtocolHandler", endpoint}
	default:
		return fmt.Errorf("browser launch is unsupported on %s", runtime.GOOS)
	}
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, name, arguments...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func launchCompanionMacOS(ctx context.Context, application, endpointFile string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("macOS companion launch is unavailable on this host")
	}
	arguments := companionMacOSLaunchArguments(application, endpointFile)
	commandContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, "open", arguments...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func companionMacOSLaunchArguments(application, endpointFile string) []string {
	arguments := []string{"-n", application, "--env", "OPENREALTIME_NATIVE_PROFILE=observer-developer"}
	if endpointFile != "" {
		arguments = append(arguments, "--env", "OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY="+endpointFile)
	}
	return arguments
}
