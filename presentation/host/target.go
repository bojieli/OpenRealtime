package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// relayTarget is the private view used by relay implementations. It is always
// projected from the exact mounted EndpointTarget; it never reconstructs or
// derives an endpoint and is not a second mounted contract.
type relayTarget struct {
	WebSocket   string
	WebRTC      string
	Model       string
	DialTimeout time.Duration
	ReadLimit   int64
}

// EndpointDirectoryConfig is the strict deployment value accepted by
// NewEndpointDirectoryFactory. Every endpoint is explicit; websocket shortcut
// and same-origin fields are rejected as unknown JSON.
type EndpointDirectoryConfig struct {
	Endpoints     []presentation.Endpoint `json:"endpoints"`
	Model         string                  `json:"model,omitempty"`
	DialTimeoutMS int64                   `json:"dial_timeout_ms,omitempty"`
	ReadLimit     int64                   `json:"read_limit_bytes,omitempty"`
}

type TargetFactory struct {
	descriptor plugin.Descriptor
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
	return mount.Publisher.Provide(presentation.EndpointDirectoryContract, target)
}

func (factory *TargetFactory) parse(raw []byte) (EndpointTarget, error) {
	if factory == nil {
		return EndpointTarget{}, errors.New("endpoint target factory is nil")
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

func lookupTargetEndpoint(
	services pluginruntime.Services,
	name presentation.EndpointName,
	protocol string,
) (relayTarget, presentation.Endpoint, error) {
	target, err := lookupEndpointTarget(services)
	if err != nil {
		return relayTarget{}, presentation.Endpoint{}, err
	}
	endpoint, err := target.Endpoint(name, protocol)
	if err != nil {
		return relayTarget{}, presentation.Endpoint{}, err
	}
	projection, err := projectRelayTarget(target)
	if err != nil {
		return relayTarget{}, presentation.Endpoint{}, err
	}
	return projection, endpoint, nil
}

func projectRelayTarget(target EndpointTarget) (relayTarget, error) {
	result := relayTarget{
		Model: target.model, DialTimeout: target.dialTimeout, ReadLimit: target.readLimit,
	}
	if endpoint, found := target.directory.Lookup(presentation.EndpointRealtimeWebSocket); found {
		if endpoint.Protocol != presentation.ProtocolRealtimeWebSocket {
			return relayTarget{}, errors.New("presentation WebSocket endpoint protocol changed")
		}
		result.WebSocket = endpoint.URL
	}
	if endpoint, found := target.directory.Lookup(presentation.EndpointRealtimeWebRTC); found {
		if endpoint.Protocol != presentation.ProtocolRealtimeWebRTC {
			return relayTarget{}, errors.New("presentation WebRTC endpoint protocol changed")
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
