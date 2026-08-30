package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	defaultDialTimeoutMS = 15_000
	defaultReadLimit     = 16 << 20
	maxReadLimit         = 64 << 20
)

// EndpointTarget is the immutable endpoint-directory service published to
// host plugins. Its fields are private so a consumer cannot mutate the mounted
// directory or claim a different target identity. Accessors return snapshots.
type EndpointTarget struct {
	directory   presentation.EndpointDirectory
	model       string
	dialTimeout time.Duration
	readLimit   int64
}

func (target EndpointTarget) Directory() presentation.EndpointDirectory {
	return target.directory.Clone()
}

func (target EndpointTarget) Model() string              { return target.model }
func (target EndpointTarget) DialTimeout() time.Duration { return target.dialTimeout }
func (target EndpointTarget) ReadLimit() int64           { return target.readLimit }
func (target EndpointTarget) Fingerprint() string        { return target.directory.Fingerprint }
func (target EndpointTarget) Endpoint(
	name presentation.EndpointName, protocol string,
) (presentation.Endpoint, error) {
	return target.directory.Require(name, protocol)
}

// RealtimeTarget is the compatibility projection used by relay internals and
// downstream embedders. Mounted relays populate WebSocket and WebRTC only from
// the exact EndpointTarget service; they never reconstruct either URL.
type RealtimeTarget struct {
	WebSocket   string
	WebRTC      string
	Model       string
	DialTimeout time.Duration
	ReadLimit   int64
}

// EndpointDirectoryConfig is the strict deployment value accepted by
// NewEndpointDirectoryFactory. Every endpoint is explicit; legacy websocket
// or same-origin fields are rejected as unknown JSON.
type EndpointDirectoryConfig struct {
	Endpoints     []presentation.Endpoint `json:"endpoints"`
	Model         string                  `json:"model,omitempty"`
	DialTimeoutMS int64                   `json:"dial_timeout_ms,omitempty"`
	ReadLimit     int64                   `json:"read_limit_bytes,omitempty"`
}

type targetConfig struct {
	WebSocket     string `json:"websocket"`
	WebRTC        string `json:"webrtc,omitempty"`
	Model         string `json:"model,omitempty"`
	DialTimeoutMS int64  `json:"dial_timeout_ms,omitempty"`
	ReadLimit     int64  `json:"read_limit_bytes,omitempty"`
}

// LegacyEndpointDefaults is the explicit compatibility policy used to
// reconstruct old same-origin endpoints. Empty paths remain absent. In
// particular, effects and resources are never invented unless a caller names
// those legacy defaults deliberately.
type LegacyEndpointDefaults struct {
	ManagementPath string
	EffectsPath    string
	ArtifactsPath  string
	DownloadsPath  string
}

// LegacyOpenRealtimeEndpointDefaults preserves only the management inference
// performed by the old target/management-relay pair. It does not advertise an
// effects or resource endpoint.
func LegacyOpenRealtimeEndpointDefaults() LegacyEndpointDefaults {
	return LegacyEndpointDefaults{ManagementPath: "/openrealtime/v1"}
}

type TargetFactory struct {
	descriptor     plugin.Descriptor
	legacyDefaults *LegacyEndpointDefaults
}

// NewEndpointDirectoryFactory constructs the strict production target. Its
// configuration must carry a closed explicit endpoint list.
func NewEndpointDirectoryFactory() *TargetFactory {
	schema := presentation.EndpointDirectoryConfigContract
	return &TargetFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.endpoint-directory", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.EndpointDirectoryContract}, ConfigSchema: &schema,
	}}
}

// NewLegacySameOriginTargetFactory makes compatibility inference an explicit
// deployment choice. New profiles should use NewEndpointDirectoryFactory.
func NewLegacySameOriginTargetFactory(defaults LegacyEndpointDefaults) (*TargetFactory, error) {
	if err := validateLegacyEndpointDefaults(defaults); err != nil {
		return nil, err
	}
	return newLegacySameOriginTargetFactory(defaults), nil
}

func newLegacySameOriginTargetFactory(defaults LegacyEndpointDefaults) *TargetFactory {
	copy := defaults
	schema := presentation.RealtimeTargetConfigContract
	return &TargetFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.realtime-target", Revision: 2,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Provides: []plugin.Contract{
				presentation.EndpointDirectoryContract,
				presentation.RealtimeTargetContract,
			},
			ConfigSchema: &schema,
		},
		legacyDefaults: &copy,
	}
}

// NewTargetFactory is retained for source compatibility. It is deliberately a
// named legacy adapter, not the constructor new profiles should select.
// Deprecated: use NewEndpointDirectoryFactory with explicit endpoints.
func NewTargetFactory() *TargetFactory {
	return newLegacySameOriginTargetFactory(LegacyOpenRealtimeEndpointDefaults())
}

func (factory *TargetFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *TargetFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := factory.parse(raw)
	return err
}

func (factory *TargetFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	target, err := factory.parse(mount.Config)
	if err != nil {
		return err
	}
	if err := mount.Publisher.Provide(presentation.EndpointDirectoryContract, target); err != nil {
		return err
	}
	if factory.legacyDefaults == nil {
		return nil
	}
	legacy, err := projectRealtimeTarget(target)
	if err != nil {
		return err
	}
	return mount.Publisher.Provide(presentation.RealtimeTargetContract, legacy)
}

func (factory *TargetFactory) parse(raw []byte) (EndpointTarget, error) {
	if factory == nil {
		return EndpointTarget{}, errors.New("endpoint target factory is nil")
	}
	if factory.legacyDefaults != nil {
		return parseLegacyTargetConfig(raw, *factory.legacyDefaults)
	}
	return parseEndpointDirectoryConfig(raw)
}

func parseEndpointDirectoryConfig(raw []byte) (EndpointTarget, error) {
	var config EndpointDirectoryConfig
	if err := decodeTargetConfig(raw, &config, "endpoint directory config"); err != nil {
		return EndpointTarget{}, err
	}
	directory, err := presentation.FreezeEndpointDirectory(config.Endpoints)
	if err != nil {
		return EndpointTarget{}, fmt.Errorf("endpoint directory config: %w", err)
	}
	return newEndpointTarget(directory, config.Model, config.DialTimeoutMS, config.ReadLimit)
}

func parseLegacyTargetConfig(raw []byte, defaults LegacyEndpointDefaults) (EndpointTarget, error) {
	var config targetConfig
	if err := decodeTargetConfig(raw, &config, "legacy realtime target config"); err != nil {
		return EndpointTarget{}, err
	}
	websocket, err := validateTargetURL(config.WebSocket, "ws", "wss")
	if err != nil {
		return EndpointTarget{}, fmt.Errorf("legacy realtime target WebSocket: %w", err)
	}
	endpoints := []presentation.Endpoint{{
		Name: presentation.EndpointRealtimeWebSocket, Protocol: presentation.ProtocolRealtimeWebSocket,
		URL: websocket,
	}}
	if config.WebRTC != "" {
		webrtc, validateErr := validateTargetURL(config.WebRTC, "http", "https")
		if validateErr != nil {
			return EndpointTarget{}, fmt.Errorf("legacy realtime target WebRTC: %w", validateErr)
		}
		endpoints = append(endpoints, presentation.Endpoint{
			Name: presentation.EndpointRealtimeWebRTC, Protocol: presentation.ProtocolRealtimeWebRTC,
			URL: webrtc,
		})
	}
	derived, err := deriveLegacySameOriginEndpoints(websocket, defaults)
	if err != nil {
		return EndpointTarget{}, err
	}
	endpoints = append(endpoints, derived...)
	directory, err := presentation.FreezeEndpointDirectory(endpoints)
	if err != nil {
		return EndpointTarget{}, fmt.Errorf("legacy realtime target endpoint directory: %w", err)
	}
	return newEndpointTarget(directory, config.Model, config.DialTimeoutMS, config.ReadLimit)
}

func decodeTargetConfig(raw []byte, destination any, label string) error {
	if err := strictjson.Validate(raw); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("%s has trailing JSON value", label)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s trailing data: %w", label, err)
	}
	return nil
}

func newEndpointTarget(
	directory presentation.EndpointDirectory,
	model string,
	dialTimeoutMS int64,
	readLimit int64,
) (EndpointTarget, error) {
	if err := directory.Validate(); err != nil {
		return EndpointTarget{}, fmt.Errorf("endpoint target directory: %w", err)
	}
	if model != strings.TrimSpace(model) || strings.ContainsAny(model, "\x00\r\n") {
		return EndpointTarget{}, errors.New("endpoint target model is not canonical")
	}
	if dialTimeoutMS == 0 {
		dialTimeoutMS = defaultDialTimeoutMS
	}
	if dialTimeoutMS < 1 || dialTimeoutMS > 120_000 {
		return EndpointTarget{}, errors.New("endpoint target dial_timeout_ms must be in [1,120000]")
	}
	if readLimit == 0 {
		readLimit = defaultReadLimit
	}
	if readLimit < 1024 || readLimit > maxReadLimit {
		return EndpointTarget{}, fmt.Errorf(
			"endpoint target read_limit_bytes must be in [1024,%d]", maxReadLimit,
		)
	}
	return EndpointTarget{
		directory: directory.Clone(), model: model,
		dialTimeout: time.Duration(dialTimeoutMS) * time.Millisecond, readLimit: readLimit,
	}, nil
}

func validateLegacyEndpointDefaults(defaults LegacyEndpointDefaults) error {
	for name, path := range map[string]string{
		"management": defaults.ManagementPath,
		"effects":    defaults.EffectsPath,
		"artifacts":  defaults.ArtifactsPath,
		"downloads":  defaults.DownloadsPath,
	} {
		if path == "" {
			continue
		}
		if path != strings.TrimSpace(path) || !strings.HasPrefix(path, "/") ||
			(len(path) > 1 && strings.HasSuffix(path, "/")) || strings.ContainsAny(path, "\x00\r\n?#") {
			return fmt.Errorf("legacy %s endpoint default %q is not a canonical absolute path", name, path)
		}
		for _, segment := range strings.Split(path, "/") {
			if segment == "." || segment == ".." {
				return fmt.Errorf("legacy %s endpoint default contains a relative segment", name)
			}
		}
	}
	return nil
}

func deriveLegacySameOriginEndpoints(
	websocket string, defaults LegacyEndpointDefaults,
) ([]presentation.Endpoint, error) {
	parsed, err := url.Parse(websocket)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, errors.New("legacy same-origin endpoint adapter has an invalid WebSocket target")
	}
	parsed.User, parsed.RawPath, parsed.RawQuery, parsed.Fragment = nil, "", "", ""
	derived := make([]presentation.Endpoint, 0, 4)
	add := func(name presentation.EndpointName, protocol, path, scheme string) {
		if path == "" {
			return
		}
		copy := *parsed
		copy.Scheme = scheme
		copy.Path = path
		derived = append(derived, presentation.Endpoint{Name: name, Protocol: protocol, URL: copy.String()})
	}
	httpScheme := "http"
	if parsed.Scheme == "wss" {
		httpScheme = "https"
	}
	add(presentation.EndpointManagement, presentation.ProtocolManagement, defaults.ManagementPath, httpScheme)
	add(presentation.EndpointEffects, presentation.ProtocolClientEffects, defaults.EffectsPath, parsed.Scheme)
	add(presentation.EndpointArtifacts, presentation.ProtocolHostArtifacts, defaults.ArtifactsPath, httpScheme)
	add(presentation.EndpointDownloads, presentation.ProtocolHostDownloads, defaults.DownloadsPath, httpScheme)
	return derived, nil
}

func validateTargetURL(raw string, schemes ...string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", fmt.Errorf("invalid URL %q", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid credential-free URL %q", raw)
	}
	allowed := false
	for _, scheme := range schemes {
		allowed = allowed || parsed.Scheme == scheme
	}
	if !allowed {
		return "", fmt.Errorf("URL %q uses unsupported scheme", raw)
	}
	return parsed.String(), nil
}

func lookupTarget(services pluginruntime.Services) (RealtimeTarget, error) {
	target, err := lookupEndpointTarget(services)
	if err != nil {
		return RealtimeTarget{}, err
	}
	return projectRealtimeTarget(target)
}

func lookupTargetEndpoint(
	services pluginruntime.Services,
	name presentation.EndpointName,
	protocol string,
) (RealtimeTarget, presentation.Endpoint, error) {
	target, err := lookupEndpointTarget(services)
	if err != nil {
		return RealtimeTarget{}, presentation.Endpoint{}, err
	}
	endpoint, err := target.Endpoint(name, protocol)
	if err != nil {
		return RealtimeTarget{}, presentation.Endpoint{}, err
	}
	projection, err := projectRealtimeTarget(target)
	if err != nil {
		return RealtimeTarget{}, presentation.Endpoint{}, err
	}
	return projection, endpoint, nil
}

func projectRealtimeTarget(target EndpointTarget) (RealtimeTarget, error) {
	result := RealtimeTarget{
		Model: target.model, DialTimeout: target.dialTimeout, ReadLimit: target.readLimit,
	}
	if endpoint, found := target.directory.Lookup(presentation.EndpointRealtimeWebSocket); found {
		if endpoint.Protocol != presentation.ProtocolRealtimeWebSocket {
			return RealtimeTarget{}, errors.New("presentation WebSocket endpoint protocol changed")
		}
		result.WebSocket = endpoint.URL
	}
	if endpoint, found := target.directory.Lookup(presentation.EndpointRealtimeWebRTC); found {
		if endpoint.Protocol != presentation.ProtocolRealtimeWebRTC {
			return RealtimeTarget{}, errors.New("presentation WebRTC endpoint protocol changed")
		}
		result.WebRTC = endpoint.URL
	}
	return result, nil
}

func lookupEndpointTarget(services pluginruntime.Services) (EndpointTarget, error) {
	value, contract, _, _, found := services.Lookup(presentation.EndpointDirectoryContract.Name)
	if !found || contract != presentation.EndpointDirectoryContract {
		return EndpointTarget{}, errors.New("presentation endpoint directory is unavailable")
	}
	target, ok := value.(EndpointTarget)
	if !ok {
		return EndpointTarget{}, errors.New("presentation endpoint directory has the wrong Go type")
	}
	if err := target.directory.Validate(); err != nil {
		return EndpointTarget{}, fmt.Errorf("presentation endpoint directory changed after mount: %w", err)
	}
	target.directory = target.directory.Clone()
	return target, nil
}
