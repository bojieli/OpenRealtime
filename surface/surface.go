// Package surface serves the test surface: a page that exercises every
// channel this system has, in both directions, over either transport.
//
// The developer console next door is the minimal worked example of the
// protocol - one voice client, an inspector, and nothing that is not on the
// wire. This is the other thing a developer needs, and it is deliberately not
// the same thing: a bench that puts every observation channel and every action
// channel on one screen at one time, so an end-to-end run can be watched
// rather than reconstructed from an event log afterwards.
//
// Six channels go in - audio, typed text, screen, camera, a browser, and the
// results of tools - and five come out: speech, text, computer-use actions,
// tool calls, and HTML artifacts. Every one of them is the base protocol or
// the published extension. Nothing here adds a wire name, and the browser
// channel is the reason that is worth saying out loud: a browser this process
// drives is observed as an ordinary video source and acted on with the
// ordinary computer.* vocabulary, so the loop closes with no new protocol at
// all.
//
// Like the console, it runs on the developer's machine, and for the same three
// reasons: loopback is a secure context so capture works without a
// certificate, the credential stays in this process rather than entering the
// page, and the tools a session calls run where the files and the browser are.
package surface

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

	"github.com/bojieli/OpenRealtime/console"
	"github.com/coder/websocket"
)

// Config configures the surface.
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
	// Files executes the filesystem tools. Nil serves a surface with none,
	// which still exercises every channel except that one.
	Files *console.Host
	// Browser is the context this surface both observes and acts on. Nil
	// leaves the browser channel dark and says so in the page rather than
	// offering a control that cannot work.
	Browser *BrowserContext
	// Artifacts stores the HTML the agent renders. Never nil in a built
	// server; New supplies one.
	Artifacts *ArtifactStore
	// Logger receives operational messages.
	Logger *slog.Logger
	// DialTimeout bounds connecting to the endpoint.
	DialTimeout time.Duration
}

// Server is the surface's HTTP surface.
type Server struct {
	config Config
	tools  *ToolHost
}

// New validates the configuration and assembles the tool host.
func New(config Config) (*Server, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("the surface requires a protocol endpoint")
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
	if config.Artifacts == nil {
		config.Artifacts = NewArtifactStore(0)
	}
	return &Server{
		config: config,
		tools:  NewToolHost(config.Files, config.Browser, config.Artifacts),
	}, nil
}

// Handler returns the surface's routes.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", assetHandler())
	mux.HandleFunc("GET /api/config", server.describe)
	mux.HandleFunc("GET /api/session", server.session)
	mux.HandleFunc("GET /api/tools", server.toolSocket)
	mux.HandleFunc("POST /api/webrtc", server.webrtc)
	mux.HandleFunc("GET /api/browser/frame", server.browserFrame)
	mux.Handle("GET /artifacts/{id}", server.Artifacts())
	return mux
}

// Artifacts serves the rendered HTML.
//
// It is a separate route rather than a srcdoc or a blob, and that is a
// security decision rather than a stylistic one. A frame loaded from srcdoc,
// blob:, or data: inherits the embedding page's content security policy, so
// the artifact would run under the policy written for this application - and
// relaxing that policy far enough to let model-authored inline script run
// would relax it for the surface itself. A real URL carries its own policy,
// and the sandbox attribute on the frame withholds the origin, so the artifact
// executes in a context that can reach neither this page's DOM nor its
// storage.
func (server *Server) Artifacts() http.Handler { return server.config.Artifacts }

// ToolHost is what the page's tool socket dispatches to.
func (server *Server) ToolHost() *ToolHost { return server.tools }

// LoopbackOnly reports whether an address is safe for a process that can run
// commands and drive a browser.
//
// The surface executes what a session asks it to, so reachability is the whole
// of its security model. A non-loopback bind would put a shell and a browser
// on the network, and no amount of care inside the tool host makes that
// acceptable.
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
			"the surface executes tools and drives a browser on this machine, so it listens on "+
				"loopback only; %q is not a loopback address", host)
	}
	return nil
}

type describedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Confirm     string          `json:"confirm"`
	Mutating    bool            `json:"mutating"`
	// Channel is which action channel the page files this tool under. The
	// page could guess from the name, but a guess in the client and a fact in
	// the host are two statements that can disagree, and the whole point of
	// the surface is that what you see is what happened.
	Channel string `json:"channel"`
}

// describe tells the page what this surface can do.
//
// Every tool declaration comes from here rather than from the page, so what
// the session is told a tool is and what this process will run are one
// statement rather than two that can drift apart.
func (server *Server) describe(writer http.ResponseWriter, _ *http.Request) {
	described := []describedTool{}
	for _, tool := range server.tools.Tools() {
		described = append(described, describedTool{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
			Confirm: tool.Confirm, Mutating: tool.Mutating, Channel: string(tool.Channel),
		})
	}
	root := ""
	if server.config.Files != nil {
		root = server.config.Files.Root()
	}
	browser := map[string]any{"available": false}
	if server.config.Browser != nil {
		width, height := server.config.Browser.Viewport()
		browser = map[string]any{
			"available": true,
			"source":    server.config.Browser.Source(),
			"width":     width,
			"height":    height,
			"target":    server.config.Browser.TargetName(),
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"endpoint":      server.config.Endpoint,
		"model":         server.config.Model,
		"webrtc":        server.config.WebRTCEndpoint != "",
		"authenticated": server.config.Token != "",
		"root":          root,
		"tools":         described,
		"browser":       browser,
	})
}

// session relays the protocol between the page and the endpoint.
//
// It is a byte-for-byte relay in both directions. The page is the protocol
// client - it decides what to send and interprets what arrives - and this only
// carries the bytes and attaches the credential a browser cannot set on a
// WebSocket at all.
func (server *Server) session(writer http.ResponseWriter, request *http.Request) {
	local, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	// Video frames from three sources arrive on this hop, so the relay is
	// sized for them rather than for the library default of 32 KiB, which
	// would drop the connection on the first screen capture.
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
		server.config.Logger.Error("the surface could not reach the endpoint",
			"endpoint", server.config.Endpoint, "error", err)
		// The page is told in the protocol's own vocabulary, so one error path
		// in the client covers a server that refused and a server that is not
		// there.
		failure, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type": "connection_error", "code": "endpoint_unreachable",
				"message": fmt.Sprintf("the surface could not reach %s: %v", server.config.Endpoint, err),
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
		http.Error(writer, "this surface has no WebRTC endpoint configured", http.StatusNotFound)
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
		http.Error(writer, fmt.Sprintf("the surface could not reach the WebRTC adapter: %v", err),
			http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	writer.Header().Set("Content-Type", "application/sdp")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(body)
}

// browserFrame captures one frame of the browser this surface drives.
//
// The page asks for frames and sends them on as ordinary video events, rather
// than this process injecting them into the session behind the page's back.
// That keeps one protocol client in the picture: every frame the agent sees
// appears in the page's own event log next to the screen and camera frames it
// captured itself, and the browser channel is therefore inspectable in exactly
// the same way as the two channels a browser can capture natively.
func (server *Server) browserFrame(writer http.ResponseWriter, request *http.Request) {
	if server.config.Browser == nil {
		http.Error(writer, "this surface has no browser context", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	frame, width, height, err := server.config.Browser.CaptureFrame(ctx)
	if err != nil {
		server.config.Logger.Warn("browser capture failed", "error", err)
		http.Error(writer, fmt.Sprintf("could not capture the browser: %v", err), http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"source": server.config.Browser.Source(),
		"width":  width, "height": height,
		"frame":     frame,
		"format":    "jpeg",
		"url":       server.config.Browser.LastURL(),
		"timestamp": time.Now().UnixMilli(),
	})
}

// toolSocket serves one page's tool socket.
func (server *Server) toolSocket(writer http.ResponseWriter, request *http.Request) {
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	// An artifact is HTML a model wrote, and a model asked for a dashboard
	// writes more than a default read limit allows.
	connection.SetReadLimit(16 << 20)
	defer connection.CloseNow()

	if len(server.tools.Tools()) == 0 {
		_ = writeTool(request.Context(), connection, toolMessage{
			Type: messageResult, Error: "this surface was started with no tools",
		})
		return
	}
	session := &toolSession{
		host: server.tools, logger: server.config.Logger, connection: connection,
		decisions: make(map[string]*pendingDecision),
	}
	session.serve(request.Context())
}
