// Command openrealtime-livekit joins a LiveKit room as an agent participant
// and proxies it to an OpenRealtime endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	livekit "github.com/bojieli/OpenRealtime/integrations/livekit"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "openrealtime-livekit:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("openrealtime-livekit", flag.ContinueOnError)
	var (
		url       string
		room      string
		identity  string
		name      string
		endpoint  string
		tokenEnv  string
		model     string
		keyEnv    string
		secretEnv string
		verbose   bool
		video     bool
	)
	flags.StringVar(&url, "livekit-url", "", "LiveKit server URL, ws:// or wss://")
	flags.StringVar(&room, "room", "", "room to join")
	flags.StringVar(&identity, "identity", "openrealtime-agent", "participant identity")
	flags.StringVar(&name, "name", "OpenRealtime", "participant display name")
	flags.StringVar(&endpoint, "endpoint", "ws://127.0.0.1:8765/v1/realtime", "OpenRealtime protocol endpoint")
	flags.StringVar(&tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the endpoint bearer token")
	flags.StringVar(&model, "model", "", "model to request from the endpoint")
	flags.StringVar(&keyEnv, "livekit-key-env", "LIVEKIT_API_KEY", "environment variable holding the LiveKit API key")
	flags.StringVar(&secretEnv, "livekit-secret-env", "LIVEKIT_API_SECRET", "environment variable holding the LiveKit API secret")
	flags.BoolVar(&verbose, "verbose", true, "log connection and media events")
	flags.BoolVar(&video, "video", false,
		"subscribe to room video tracks and bridge VP8 key frames to the endpoint as protocol video events; "+
			"the session then declares the OpenRealtime video extension, which an endpoint without it ignores")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if room == "" || url == "" {
		return errors.New("-livekit-url and -room are required")
	}
	logf := func(string, ...any) {}
	if verbose {
		logger := log.New(os.Stderr, "", log.LstdFlags)
		logf = func(format string, values ...any) { logger.Printf(format, values...) }
	}
	agent, err := livekit.New(livekit.Config{
		URL: url, APIKey: os.Getenv(keyEnv), APISecret: os.Getenv(secretEnv),
		Room: room, Identity: identity, Name: name,
		Endpoint: endpoint, Token: os.Getenv(tokenEnv), Model: model,
		PacketDuration: 20 * time.Millisecond, Video: video, Logf: logf,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx)
}
