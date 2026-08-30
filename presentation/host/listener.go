package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

const (
	listenPermissionKind      = "network.listen"
	listenPermissionResource  = "loopback"
	listenPermissionOperation = "http"
)

type ListenerInfo struct {
	Network string `json:"network"`
	Address string `json:"address"`
	URL     string `json:"url"`
}

type listenerConfig struct {
	Address             string `json:"address"`
	ReadHeaderTimeoutMS int64  `json:"read_header_timeout_ms,omitempty"`
	ShutdownTimeoutMS   int64  `json:"shutdown_timeout_ms,omitempty"`
}

type ListenerFactory struct{ descriptor plugin.Descriptor }

func NewLoopbackListenerFactory() *ListenerFactory {
	schema := presentation.ListenerConfigContract
	return &ListenerFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.loopback-listener", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides:     []plugin.Contract{presentation.ListenerContract},
		Requires:     []plugin.Requirement{{Contract: presentation.HTTPHandlerContract}},
		ConfigSchema: &schema,
		Permissions: []plugin.Permission{{
			Kind: listenPermissionKind, Resource: listenPermissionResource,
			Operations: []string{listenPermissionOperation},
		}},
	}}
}

func (factory *ListenerFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *ListenerFactory) ValidateConfig(raw json.RawMessage) error {
	_, err := parseListenerConfig(raw)
	return err
}

func (factory *ListenerFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if !mount.Permissions.Allows(listenPermissionKind, listenPermissionResource, listenPermissionOperation) {
		return errors.New("presentation listener lacks its deployment loopback-listen grant")
	}
	config, err := parseListenerConfig(mount.Config)
	if err != nil {
		return err
	}
	value, contract, _, _, found := mount.Services.Lookup(presentation.HTTPHandlerContract.Name)
	if !found || contract != presentation.HTTPHandlerContract {
		return errors.New("presentation HTTP handler is unavailable")
	}
	handler, ok := value.(http.Handler)
	if !ok || handler == nil {
		return errors.New("presentation HTTP handler has the wrong Go type")
	}
	listener, err := net.Listen("tcp", config.Address)
	if err != nil {
		return fmt.Errorf("listen for presentation host: %w", err)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: time.Duration(config.ReadHeaderTimeoutMS) * time.Millisecond,
	}
	if err := mount.Lifecycle.Defer("http-listener", func(context.Context) error {
		err := listener.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}); err != nil {
		_ = listener.Close()
		return err
	}
	if err := mount.Publisher.Provide(presentation.ListenerContract, ListenerInfo{
		Network: listener.Addr().Network(), Address: listener.Addr().String(),
		URL: "http://" + listener.Addr().String(),
	}); err != nil {
		return err
	}
	if err := mount.Lifecycle.Go("http-serve", func(context.Context) error {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}); err != nil {
		return err
	}
	return mount.Lifecycle.Go("http-shutdown", func(ctx context.Context) error {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(
			context.Background(), time.Duration(config.ShutdownTimeoutMS)*time.Millisecond,
		)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	})
}

func parseListenerConfig(raw []byte) (listenerConfig, error) {
	if err := strictjson.Validate(raw); err != nil {
		return listenerConfig{}, fmt.Errorf("presentation listener config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config listenerConfig
	if err := decoder.Decode(&config); err != nil {
		return listenerConfig{}, fmt.Errorf("presentation listener config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return listenerConfig{}, errors.New("presentation listener config has trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return listenerConfig{}, fmt.Errorf("presentation listener config trailing data: %w", err)
	}
	if err := validateLoopbackAddress(config.Address); err != nil {
		return listenerConfig{}, err
	}
	if config.ReadHeaderTimeoutMS == 0 {
		config.ReadHeaderTimeoutMS = 5_000
	}
	if config.ShutdownTimeoutMS == 0 {
		config.ShutdownTimeoutMS = 5_000
	}
	if config.ReadHeaderTimeoutMS < 1 || config.ReadHeaderTimeoutMS > 120_000 ||
		config.ShutdownTimeoutMS < 1 || config.ShutdownTimeoutMS > 120_000 {
		return listenerConfig{}, errors.New("presentation listener timeouts must be in [1,120000] milliseconds")
	}
	return config, nil
}

func validateLoopbackAddress(address string) error {
	if address == "" || address != strings.TrimSpace(address) {
		return errors.New("presentation listener requires a canonical host:port address")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("presentation listener address %q is not host:port", address)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("presentation listener address %q has an invalid port", address)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("presentation listener address %q is not loopback", address)
	}
	return nil
}
