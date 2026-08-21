// Package console serves the developer console and executes its tools.
//
// It runs on the developer's own machine, and that placement is the whole
// design rather than a convenience.
//
// A browser will not give a page a microphone, a screen, or a camera outside a
// secure context, and 127.0.0.1 is one - so a page served from here can
// capture all three against a server running anywhere, with no certificate to
// obtain for a machine you are only testing. The credential for that server
// stays in this process and never enters the browser, because the protocol
// connection is made from here. And the tools the session calls run here too,
// which is the only place they could: a browser cannot open a file, and the
// interesting workload for a voice agent that can also act is the developer's
// own working directory.
//
// So this process is three things that want to be one: a static file server, a
// protocol proxy, and a tool host. Splitting them would mean three ports,
// three origins, and a cross-origin story for each.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Config configures the console.
type Config struct {
	// Endpoint is the OpenRealtime WebSocket endpoint sessions are proxied to.
	Endpoint string
	// WebRTCEndpoint is the adapter's SDP endpoint. Empty disables the WebRTC
	// transport in the page rather than offering one that cannot work.
	WebRTCEndpoint string
	// Token authenticates to both. It is held here and never sent to the page.
	Token string
	// Model selects the endpoint's model.
	Model string
	// Host executes tool calls. Nil serves a console with no tools, which is
	// still a complete client for everything except tool use.
	Host *Host
	// Logger receives operational messages.
	Logger *slog.Logger
	// DialTimeout bounds connecting to the endpoint.
	DialTimeout time.Duration
}

// Server is the console's HTTP surface.
type Server struct {
	config Config
}

// New validates the configuration.
func New(config Config) (*Server, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("the console requires a protocol endpoint")
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, fmt.Errorf("the endpoint must be a ws:// or wss:// URL, got %q", config.Endpoint)
	}
	if config.WebRTCEndpoint != "" {
		rtc, err := url.Parse(config.WebRTCEndpoint)
		if err != nil || (rtc.Scheme != "http" && rtc.Scheme != "https") {
			return nil, fmt.Errorf("the WebRTC endpoint must be an http:// or https:// URL, got %q",
				config.WebRTCEndpoint)
		}
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 15 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{config: config}, nil
}

// Handler returns the console's routes.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", assetHandler())
	mux.HandleFunc("GET /api/config", server.describe)
	mux.HandleFunc("GET /api/session", server.session)
	mux.HandleFunc("GET /api/tools", server.tools)
	mux.HandleFunc("POST /api/webrtc", server.webrtc)
	return mux
}

// LoopbackOnly reports whether an address is safe for a process that can run
// commands.
//
// The console executes what a session asks it to, so reachability is the whole
// of its security model. A non-loopback bind would put a shell on the network,
// and no amount of care inside the tool host makes that acceptable.
func LoopbackOnly(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port", address)
	}
	if host == "" || host == "localhost" {
		return nil
	}
	parsed := net.ParseIP(host)
	if parsed == nil || !parsed.IsLoopback() {
		return fmt.Errorf(
			"the console executes tools on this machine, so it listens on loopback only; %q is not a loopback address",
			host)
	}
	return nil
}

type describedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Confirm     string          `json:"confirm"`
	Mutating    bool            `json:"mutating"`
}

// describe tells the page what this console can do.
//
// The tool declarations come from here rather than from the page, so what the
// session is told a tool is and what this process will run are one statement.
func (server *Server) describe(writer http.ResponseWriter, _ *http.Request) {
	described := []describedTool{}
	root := ""
	if server.config.Host != nil {
		root = server.config.Host.Root()
		for _, tool := range server.config.Host.Tools() {
			described = append(described, describedTool{
				Name: tool.Name, Description: tool.Description,
				Parameters: tool.Parameters, Confirm: tool.Confirm, Mutating: tool.Mutating,
			})
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"endpoint":      server.config.Endpoint,
		"model":         server.config.Model,
		"webrtc":        server.config.WebRTCEndpoint != "",
		"authenticated": server.config.Token != "",
		"root":          root,
		"tools":         described,
	})
}

// session relays the protocol between the page and the endpoint.
//
// It is a byte-for-byte relay in both directions. The page is the protocol
// client - it decides what to send and interprets what arrives - and this only
// carries the bytes and attaches the credential the browser cannot.
func (server *Server) session(writer http.ResponseWriter, request *http.Request) {
	local, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	// Video frames arrive on this hop too, so the relay is sized for them
	// rather than for the library default, which is 32 KiB and would drop the
	// connection on the first screen capture.
	local.SetReadLimit(16 << 20)
	defer local.CloseNow()

	ctx := request.Context()
	dial, cancel := context.WithTimeout(ctx, server.config.DialTimeout)
	defer cancel()
	header := http.Header{}
	if server.config.Token != "" {
		header.Set("Authorization", "Bearer "+server.config.Token)
	}
	target := server.config.Endpoint
	if strings.TrimSpace(server.config.Model) != "" {
		target = appendModel(target, server.config.Model)
	}
	remote, _, err := websocket.Dial(dial, target, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		server.config.Logger.Error("console could not reach the endpoint",
			"endpoint", server.config.Endpoint, "error", err)
		// The page is told in the protocol's own vocabulary, so one error path
		// in the client covers a server that refused and a server that is not
		// there.
		failure, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": "connection_error", "code": "endpoint_unreachable",
				"message": fmt.Sprintf("the console could not reach %s: %v", server.config.Endpoint, err),
			},
		})
		_ = local.Write(ctx, websocket.MessageText, failure)
		_ = local.Close(websocket.StatusNormalClosure, "endpoint unreachable")
		return
	}
	remote.SetReadLimit(16 << 20)
	defer remote.CloseNow()

	relay, stop := context.WithCancel(ctx)
	defer stop()
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); defer stop(); copyMessages(relay, local, remote) }()
	go func() { defer wait.Done(); defer stop(); copyMessages(relay, remote, local) }()
	wait.Wait()
}

func appendModel(endpoint, model string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	query.Set("model", model)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func copyMessages(ctx context.Context, from, to *websocket.Conn) {
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

// webrtc forwards an SDP offer to the adapter.
//
// The media path is direct between the browser and the adapter; only the offer
// and answer come through here, because a page on this origin cannot POST to
// the adapter's without the adapter growing a cross-origin policy that exists
// solely for a development tool.
func (server *Server) webrtc(writer http.ResponseWriter, request *http.Request) {
	if server.config.WebRTCEndpoint == "" {
		http.Error(writer, "this console has no WebRTC endpoint configured", http.StatusNotFound)
		return
	}
	offer, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil || len(offer) == 0 {
		http.Error(writer, "an SDP offer is required", http.StatusBadRequest)
		return
	}
	target := server.config.WebRTCEndpoint
	if strings.TrimSpace(server.config.Model) != "" {
		if parsed, err := url.Parse(target); err == nil {
			query := parsed.Query()
			query.Set("model", server.config.Model)
			parsed.RawQuery = query.Encode()
			target = parsed.String()
		}
	}
	proxied, err := http.NewRequestWithContext(request.Context(), http.MethodPost, target,
		strings.NewReader(string(offer)))
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	proxied.Header.Set("Content-Type", "application/sdp")
	if server.config.Token != "" {
		proxied.Header.Set("Authorization", "Bearer "+server.config.Token)
	}
	response, err := http.DefaultClient.Do(proxied)
	if err != nil {
		http.Error(writer, fmt.Sprintf("the console could not reach the WebRTC adapter: %v", err),
			http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	writer.Header().Set("Content-Type", "application/sdp")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}
