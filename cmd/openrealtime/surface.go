package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/console"
	"github.com/bojieli/OpenRealtime/surface"
)

// runSurface serves the test surface on this machine.
//
// It is a third command rather than a flag on either of the other two, and for
// the same reason the console is its own command: a realtime server is one
// protocol on one port and has no business serving a page, holding a shell, or
// driving a browser. This is a developer's own tool, running on a developer's
// own machine, pointed at whichever server they are testing.
//
// What separates it from the console is what it is for. The console is the
// minimal complete client - the worked example someone reads while
// implementing one. This is the bench: every channel the system has, in both
// directions, on one screen, so an end-to-end run can be watched rather than
// reconstructed afterwards.
func runSurface(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime surface", flag.ContinueOnError)
	var (
		listen         string
		endpoint       string
		webrtcEndpoint string
		tokenEnv       string
		model          string
		root           string
		toolNames      string
		commandTimeout time.Duration
		browserURL     string
		browserTarget  string
		browserSource  string
		startURL       string
		logLevel       string
	)
	flags.StringVar(&listen, "listen", "127.0.0.1:8768", "surface listen address; loopback only")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "OpenRealtime protocol endpoint")
	flags.StringVar(&webrtcEndpoint, "webrtc", "",
		"WebRTC adapter SDP endpoint, such as http://127.0.0.1:8766/v1/realtime/calls; empty offers WebSocket only")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN",
		"environment variable holding the endpoint's bearer token; the browser never sees it")
	flags.StringVar(&model, "model", "", "model identifier to request from the endpoint")
	flags.StringVar(&root, "root", ".", "directory tools may read and write; every path resolves inside it")
	flags.StringVar(&toolNames, "tools", "read_file,list_directory,search_files",
		fmt.Sprintf("comma-separated filesystem tools to offer, \"all\", or \"none\"; available: %s",
			strings.Join(console.ToolNames(), ", ")))
	flags.DurationVar(&commandTimeout, "command-timeout", 2*time.Minute, "how long one run_command may take")
	flags.StringVar(&browserURL, "browser-devtools-url", "",
		"DevTools endpoint of a browser to observe and act on, such as http://127.0.0.1:9222; "+
			"empty leaves the browser channel unattached")
	flags.StringVar(&browserTarget, "browser-target", "",
		"connect directly to a known page WebSocket instead of discovering one")
	flags.StringVar(&browserSource, "browser-source", "browser",
		"video source name the browser's frames arrive under, and the source computer.* actions name")
	flags.StringVar(&startURL, "browser-start-url", "", "navigate the browser here before the first frame")
	flags.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("surface accepts flags only")
	}
	if err := surface.LoopbackOnly(listen); err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var files *console.Host
	if strings.TrimSpace(toolNames) != "none" {
		created, err := console.NewHost(console.HostConfig{
			Root: root, Enabled: console.ParseToolSelection(toolNames), CommandTimeout: commandTimeout,
		})
		if err != nil {
			return err
		}
		files = created
	}

	var browserContext *surface.BrowserContext
	if strings.TrimSpace(browserURL) != "" || strings.TrimSpace(browserTarget) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		connected, err := surface.ConnectBrowser(ctx, surface.BrowserConfig{
			DevToolsURL: browserURL, TargetURL: browserTarget,
			Source: browserSource, StartURL: startURL,
		})
		cancel()
		if err != nil {
			// Refusing to start is right. A surface that came up with the
			// browser channel quietly missing would be a bench that reports a
			// channel silent when what happened is that it was never attached,
			// and that is the one failure this whole page exists to make
			// impossible.
			return fmt.Errorf("attach the browser channel: %w", err)
		}
		browserContext = connected
		defer func() { _ = browserContext.Close() }()
	}

	server, err := surface.New(surface.Config{
		Endpoint: endpoint, WebRTCEndpoint: webrtcEndpoint,
		Token: os.Getenv(tokenEnv), Model: model,
		Files: files, Browser: browserContext, Logger: logger,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- httpServer.ListenAndServe() }()

	describeSurface(output, describedSurface{
		listen: listen, endpoint: endpoint, webrtc: webrtcEndpoint,
		files: files, browser: browserContext, tools: server.ToolHost(),
	})

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}

type describedSurface struct {
	listen, endpoint, webrtc string
	files                    *console.Host
	browser                  *surface.BrowserContext
	tools                    *surface.ToolHost
}

// describeSurface says what just started, channel by channel.
//
// The list is not decoration. Every line is a channel that is either attached
// or is not, and a person reading a run afterwards needs to know which - a
// camera nobody plugged in and a camera the server ignored produce the same
// empty card.
func describeSurface(output io.Writer, described describedSurface) {
	fmt.Fprintf(output, "OpenRealtime surface on http://%s\n", described.listen)
	fmt.Fprintf(output, "  session   %s\n", described.endpoint)
	if described.webrtc != "" {
		fmt.Fprintf(output, "  webrtc    %s\n", described.webrtc)
	}

	byChannel := map[surface.Channel][]string{}
	for _, tool := range described.tools.Tools() {
		byChannel[tool.Channel] = append(byChannel[tool.Channel], tool.Name)
	}
	if described.files != nil {
		fmt.Fprintf(output, "  tools     %s\n", strings.Join(byChannel[surface.ChannelTool], ", "))
		fmt.Fprintf(output, "  root      %s\n", described.files.Root())
	} else {
		fmt.Fprintln(output, "  tools     none")
	}
	fmt.Fprintf(output, "  artifact  %s\n", strings.Join(byChannel[surface.ChannelArtifact], ", "))
	if described.browser != nil {
		width, height := described.browser.Viewport()
		fmt.Fprintf(output, "  browser   source %q, %d×%d, %d computer-use actions\n",
			described.browser.Source(), width, height, len(byChannel[surface.ChannelComputer]))
		if location := described.browser.LastURL(); location != "" {
			fmt.Fprintf(output, "            at %s\n", location)
		}
	} else {
		fmt.Fprintln(output, "  browser   not attached; pass -browser-devtools-url to observe and act on one")
	}

	// Worth saying out loud rather than leaving in a manual. A session that can
	// change files and click things in a browser is a different thing from one
	// that can only talk, and the person starting it should know which one they
	// just started - which means naming what is actually declared. A warning
	// that lists powers this process does not have is a warning that gets read
	// once and then ignored.
	var mutating []string
	for _, tool := range described.tools.Tools() {
		if tool.Channel == surface.ChannelTool && tool.Mutating {
			mutating = append(mutating, tool.Name)
		}
	}
	browserTarget := ""
	if described.browser != nil {
		browserTarget = described.browser.TargetName()
	}
	if warning := describePowers(mutating, browserTarget); warning != "" {
		fmt.Fprintln(output, "\n  "+warning)
	}
	fmt.Fprintln(output, "\nOpen the address above. Loopback counts as a secure context, so the")
	fmt.Fprintln(output, "microphone, screen, and camera work without a certificate.")
}

// describePowers names what this surface can actually do to the machine.
//
// It is built from what was declared rather than written once and left, so a
// read-only surface does not warn about writing files and a surface with no
// browser does not warn about clicking. A warning that lists powers the
// process does not have is a warning that gets read once and then ignored.
func describePowers(mutatingFileTools []string, browserTarget string) string {
	var lines []string
	switch {
	case len(mutatingFileTools) > 0 && browserTarget != "":
		lines = append(lines, fmt.Sprintf(
			"This surface can act on your machine (%s) and on the browser it is attached to.",
			strings.Join(mutatingFileTools, ", ")))
	case len(mutatingFileTools) > 0:
		lines = append(lines, fmt.Sprintf(
			"This surface can change your files (%s).", strings.Join(mutatingFileTools, ", ")))
	case browserTarget != "":
		lines = append(lines, "This surface can act on the browser it is attached to.")
	default:
		return ""
	}
	if len(mutatingFileTools) > 0 {
		lines = append(lines, "Those changes wait for you to approve them.")
	}
	if browserTarget != "" {
		lines = append(lines, fmt.Sprintf(
			"Computer-use actions run without asking, bounded by the declared context (%s).",
			browserTarget))
	}
	return strings.Join(lines, "\n  ")
}
