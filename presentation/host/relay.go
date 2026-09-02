package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
	"github.com/coder/websocket"
)

const (
	connectPermissionKind     = "network.connect"
	connectPermissionResource = "realtime-endpoint"
	websocketOperation        = "websocket"
	httpOperation             = "http"
)

// WebSocketRelayFactory provides the optional credential-holding public-wire
// relay at /client/v1/realtime. The relay never interprets successful protocol
// events and cannot access an in-process gateway.
type WebSocketRelayFactory struct {
	descriptor plugin.Descriptor
	logger     *slog.Logger
}

func NewWebSocketRelayFactory(logger *slog.Logger) *WebSocketRelayFactory {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &WebSocketRelayFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.websocket-relay", Revision: 2,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Requires: []plugin.Requirement{
				{Contract: presentation.HTTPRoutesContract},
				{Contract: presentation.EndpointDirectoryContract},
				{Contract: presentation.CredentialContract},
			},
			Permissions: []plugin.Permission{{
				Kind: connectPermissionKind, Resource: connectPermissionResource,
				Operations: []string{websocketOperation},
			}},
		},
		logger: logger,
	}
}

func (factory *WebSocketRelayFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *WebSocketRelayFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	target, credential, err := factory.prepareWebSocketRelay(mount.Services, mount.Permissions)
	if err != nil {
		return err
	}
	return factory.mountWebSocketRelay(mount, target, credential)
}

func (factory *WebSocketRelayFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	_, _, err := factory.prepareWebSocketRelay(
		candidate.Services, candidate.Permissions,
	)
	if err != nil {
		return nil, err
	}
	return webSocketRelayCandidate{factory: factory}, nil
}

func (factory *WebSocketRelayFactory) prepareWebSocketRelay(
	services pluginruntime.Services, permissions pluginruntime.Permissions,
) (relayTarget, CredentialSource, error) {
	if !permissions.Allows(connectPermissionKind, connectPermissionResource, websocketOperation) {
		return relayTarget{}, nil, errors.New("WebSocket relay lacks its deployment network-connect grant")
	}
	target, endpoint, err := lookupTargetEndpoint(
		services,
		presentation.EndpointRealtimeWebSocket,
		presentation.ProtocolRealtimeWebSocket,
	)
	if err != nil {
		return relayTarget{}, nil, err
	}
	target.WebSocket = endpoint.URL
	credential, err := lookupCredential(services)
	if err != nil {
		return relayTarget{}, nil, err
	}
	return target, credential, nil
}

func (factory *WebSocketRelayFactory) mountWebSocketRelay(
	mount pluginruntime.MountContext,
	target relayTarget,
	credential CredentialSource,
) error {
	var nextSession atomic.Uint64
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		done := make(chan struct{})
		workerName := fmt.Sprintf("websocket-relay-session-%d", nextSession.Add(1))
		if err := mount.Lifecycle.Go(workerName, func(lifecycleContext context.Context) error {
			defer close(done)
			sessionContext, cancel := context.WithCancel(lifecycleContext)
			stopRequest := context.AfterFunc(request.Context(), cancel)
			defer func() {
				stopRequest()
				cancel()
			}()
			factory.relayWebSocket(sessionContext, target, credential, writer, request)
			return nil
		}); err != nil {
			http.Error(writer, "the presentation relay lifecycle is unavailable", http.StatusServiceUnavailable)
			return
		}
		<-done
	})
	return registerRoutes(mount, []Route{{
		Pattern: "GET /client/v1/realtime", Handler: handler,
	}})
}

type webSocketRelayCandidate struct {
	factory *WebSocketRelayFactory
}

func (candidate webSocketRelayCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

func (factory *WebSocketRelayFactory) relayWebSocket(
	ctx context.Context,
	target relayTarget,
	credential CredentialSource,
	writer http.ResponseWriter,
	request *http.Request,
) {
	local, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	local.SetReadLimit(target.ReadLimit)
	defer local.CloseNow()

	authorization, err := credential.Authorization(ctx)
	if err != nil || strings.ContainsAny(authorization, "\r\n") {
		factory.logger.Error("presentation relay could not obtain its credential", "error", err)
		writeRelayFailure(ctx, local, "credential_unavailable", "the presentation relay could not authenticate")
		return
	}
	dialContext, cancel := context.WithTimeout(ctx, target.DialTimeout)
	defer cancel()
	header := http.Header{}
	if authorization != "" {
		header.Set("Authorization", authorization)
	}
	endpoint := appendTargetModel(target.WebSocket, target.Model)
	remote, _, err := websocket.Dial(dialContext, endpoint, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		factory.logger.Error("presentation relay could not reach realtime endpoint",
			"endpoint", target.WebSocket, "error", err)
		writeRelayFailure(ctx, local, "endpoint_unreachable", "the realtime endpoint is unavailable")
		return
	}
	remote.SetReadLimit(target.ReadLimit)
	defer remote.CloseNow()

	relayContext, stop := context.WithCancel(ctx)
	defer stop()
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		defer stop()
		copyWebSocket(relayContext, local, remote)
	}()
	go func() {
		defer wait.Done()
		defer stop()
		copyWebSocket(relayContext, remote, local)
	}()
	wait.Wait()
}

func appendTargetModel(endpoint, model string) string {
	if model == "" {
		return endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	query.Set("model", model)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func writeRelayFailure(ctx context.Context, connection *websocket.Conn, code, message string) {
	payload, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type": "connection_error", "code": code, "message": message,
		},
	})
	_ = connection.Write(ctx, websocket.MessageText, payload)
	_ = connection.Close(websocket.StatusNormalClosure, "relay connection failed")
}

func copyWebSocket(ctx context.Context, from, to *websocket.Conn) {
	for {
		kind, payload, err := from.Read(ctx)
		if err != nil {
			return
		}
		if err := to.Write(ctx, kind, payload); err != nil {
			return
		}
	}
}

// WebRTCRelayFactory forwards browser SDP offers over HTTP while leaving the
// negotiated media and data paths directly between client and adapter.
type WebRTCRelayFactory struct {
	descriptor plugin.Descriptor
	client     *http.Client
	logger     *slog.Logger
}

func NewWebRTCRelayFactory(client *http.Client, logger *slog.Logger) *WebRTCRelayFactory {
	if client == nil {
		client = http.DefaultClient
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &WebRTCRelayFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          "openrealtime.presentation.host.webrtc-relay", Revision: 2,
			Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
			Requires: []plugin.Requirement{
				{Contract: presentation.HTTPRoutesContract},
				{Contract: presentation.EndpointDirectoryContract},
				{Contract: presentation.CredentialContract},
			},
			Permissions: []plugin.Permission{{
				Kind: connectPermissionKind, Resource: connectPermissionResource,
				Operations: []string{httpOperation},
			}},
		},
		client: &copyClient, logger: logger,
	}
}

func (factory *WebRTCRelayFactory) Descriptor() plugin.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *WebRTCRelayFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	if !mount.Permissions.Allows(connectPermissionKind, connectPermissionResource, httpOperation) {
		return errors.New("WebRTC relay lacks its deployment network-connect grant")
	}
	target, endpoint, err := lookupTargetEndpoint(
		mount.Services,
		presentation.EndpointRealtimeWebRTC,
		presentation.ProtocolRealtimeWebRTC,
	)
	if err != nil {
		return err
	}
	target.WebRTC = endpoint.URL
	credential, err := lookupCredential(mount.Services)
	if err != nil {
		return err
	}
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		factory.relayWebRTC(target, credential, writer, request)
	})
	return registerRoutes(mount, []Route{{
		Pattern: "POST /client/v1/realtime/calls", Handler: handler,
	}})
}

func (factory *WebRTCRelayFactory) relayWebRTC(
	target relayTarget,
	credential CredentialSource,
	writer http.ResponseWriter,
	request *http.Request,
) {
	offer, err := io.ReadAll(io.LimitReader(request.Body, 1<<20+1))
	if err != nil || len(offer) == 0 || len(offer) > 1<<20 {
		http.Error(writer, "a bounded SDP offer is required", http.StatusBadRequest)
		return
	}
	authorization, err := credential.Authorization(request.Context())
	if err != nil || strings.ContainsAny(authorization, "\r\n") {
		factory.logger.Error("presentation WebRTC relay could not obtain its credential", "error", err)
		http.Error(writer, "relay credential unavailable", http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), target.DialTimeout)
	defer cancel()
	proxied, err := http.NewRequestWithContext(ctx, http.MethodPost,
		appendTargetModel(target.WebRTC, target.Model), strings.NewReader(string(offer)))
	if err != nil {
		http.Error(writer, "invalid relay target", http.StatusInternalServerError)
		return
	}
	proxied.Header.Set("Content-Type", "application/sdp")
	if authorization != "" {
		proxied.Header.Set("Authorization", authorization)
	}
	response, err := factory.client.Do(proxied)
	if err != nil {
		factory.logger.Error("presentation relay could not reach WebRTC endpoint",
			"endpoint", target.WebRTC, "error", err)
		http.Error(writer, "the WebRTC endpoint is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if readErr != nil || len(body) > 1<<20 {
		http.Error(writer, "the WebRTC endpoint returned an invalid answer", http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "application/sdp")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

var _ pluginruntime.Factory = (*WebSocketRelayFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*WebSocketRelayFactory)(nil)
var _ pluginruntime.Factory = (*WebRTCRelayFactory)(nil)

func relayPermission(operation string) plugin.Permission {
	return plugin.Permission{
		Kind: connectPermissionKind, Resource: connectPermissionResource, Operations: []string{operation},
	}
}

func validateRelayEndpoint(target relayTarget) error {
	if target.WebSocket == "" {
		return fmt.Errorf("realtime relay target has no WebSocket endpoint")
	}
	return nil
}
