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
)

// runConsole serves the developer console on this machine.
//
// It is a separate command from serve, and deliberately so. A realtime server
// is one protocol on one port and has no business serving a page or holding a
// shell; this is a developer's own tool, running on a developer's own machine,
// pointed at whichever server they are testing. Keeping them apart is what
// lets the console execute tools at all - the security argument for that rests
// entirely on it being local and loopback-only, and a flag on the server would
// destroy it.
func runConsole(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime console", flag.ContinueOnError)
	var (
		listen         string
		endpoint       string
		webrtcEndpoint string
		tokenEnv       string
		model          string
		root           string
		toolNames      string
		commandTimeout time.Duration
		logLevel       string
	)
	flags.StringVar(&listen, "listen", "127.0.0.1:8767", "console listen address; loopback only")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "OpenRealtime protocol endpoint")
	flags.StringVar(&webrtcEndpoint, "webrtc", "", "WebRTC adapter SDP endpoint, such as http://127.0.0.1:8766/v1/realtime; empty offers WebSocket only")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the endpoint's bearer token; the browser never sees it")
	flags.StringVar(&model, "model", "", "model identifier to request from the endpoint")
	flags.StringVar(&root, "root", ".", "directory tools may read and write; every path resolves inside it")
	flags.StringVar(&toolNames, "tools", "read_file,list_directory,search_files",
		fmt.Sprintf("comma-separated tools to offer, \"all\", or \"none\"; available: %s",
			strings.Join(console.ToolNames(), ", ")))
	flags.DurationVar(&commandTimeout, "command-timeout", 2*time.Minute, "how long one run_command may take")
	flags.StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("console accepts flags only")
	}
	if err := console.LoopbackOnly(listen); err != nil {
		return err
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(logLevel)); err != nil {
		return fmt.Errorf("log level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	var host *console.Host
	if strings.TrimSpace(toolNames) != "none" {
		created, err := console.NewHost(console.HostConfig{
			Root:           root,
			Enabled:        console.ParseToolSelection(toolNames),
			CommandTimeout: commandTimeout,
		})
		if err != nil {
			return err
		}
		host = created
	}

	server, err := console.New(console.Config{
		Endpoint: endpoint, WebRTCEndpoint: webrtcEndpoint,
		Token: os.Getenv(tokenEnv), Model: model, Host: host, Logger: logger,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr: listen, Handler: server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- httpServer.ListenAndServe() }()

	fmt.Fprintf(output, "OpenRealtime console on http://%s\n", listen)
	fmt.Fprintf(output, "  session   %s\n", endpoint)
	if webrtcEndpoint != "" {
		fmt.Fprintf(output, "  webrtc    %s\n", webrtcEndpoint)
	}
	if host != nil {
		fmt.Fprintf(output, "  tools     %s\n", strings.Join(toolNamesOf(host), ", "))
		fmt.Fprintf(output, "  root      %s\n", host.Root())
		if mutates(host) {
			// Worth saying out loud rather than leaving in a manual. A session
			// that can write files and run commands is a different thing from
			// one that can only talk, and the person starting it should know
			// which one they just started.
			fmt.Fprintln(output,
				"\n  This console can change your files and run commands. Every such action\n"+
					"  is declared as requiring confirmation and waits for you to approve it.")
		}
	} else {
		fmt.Fprintln(output, "  tools     none")
	}
	fmt.Fprintln(output, "\nOpen the address above. Loopback counts as a secure context, so the")
	fmt.Fprintln(output, "microphone, screen, and camera work without a certificate.")

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

func toolNamesOf(host *console.Host) []string {
	names := make([]string, 0, len(host.Tools()))
	for _, tool := range host.Tools() {
		names = append(names, tool.Name)
	}
	return names
}

func mutates(host *console.Host) bool {
	for _, tool := range host.Tools() {
		if tool.Mutating {
			return true
		}
	}
	return false
}
