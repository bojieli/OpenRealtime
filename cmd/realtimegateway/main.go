// Command realtimegateway exposes the canonical OpenRealtime cascade through
// the standard OpenAI Realtime WebSocket protocol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/realtimegateway"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "realtimegateway:", err)
		os.Exit(1)
	}
}

type options struct {
	listen          string
	publicModel     string
	tokenEnv        string
	requestTimeout  time.Duration
	shutdownTimeout time.Duration
	asrURL          string
	asrModel        string
	asrChunk        time.Duration
	fastProvider    string
	fastURL         string
	fastEndpoint    string
	fastModel       string
	fastTokenEnv    string
	fastTokens      int
	slowModel       string
	slowEndpoint    string
	slowEffort      string
	slowTokens      int
	slowPace        time.Duration
	ttsURL          string
	ttsModel        string
	ttsVoice        string
	validateWire    bool
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("realtimegateway", flag.ContinueOnError)
	var config options
	flags.StringVar(&config.listen, "listen", "127.0.0.1:8765", "loopback HTTP/WebSocket listen address")
	flags.StringVar(&config.publicModel, "model", "openrealtime-local", "Realtime compatibility model identifier")
	flags.StringVar(&config.tokenEnv, "token-env", "OPENREALTIME_GATEWAY_TOKEN", "environment variable containing the loopback bearer token")
	flags.DurationVar(&config.requestTimeout, "request-timeout", 2*time.Minute, "individual provider request timeout")
	flags.DurationVar(&config.shutdownTimeout, "shutdown-timeout", 15*time.Second, "graceful HTTP shutdown timeout")
	flags.StringVar(&config.asrURL, "asr-url", qwenasr.DefaultBaseURL, "Qwen3-ASR service base URL")
	flags.StringVar(&config.asrModel, "asr-model", qwenasr.DefaultModel, "Qwen3-ASR model identity")
	flags.DurationVar(&config.asrChunk, "asr-provider-chunk", 200*time.Millisecond, "minimum stateful ASR advance interval")
	flags.StringVar(&config.fastProvider, "fast-provider", "vllm", "fast continuation provider: vllm or gemini")
	flags.StringVar(&config.fastURL, "fast-base-url", openaicompat.DefaultBaseURL, "local OpenAI-compatible fast-model base URL")
	flags.StringVar(&config.fastEndpoint, "fast-endpoint", "", "optional Gemini fast API endpoint override")
	flags.StringVar(&config.fastModel, "fast-model", "", "fast model identity; provider default when empty")
	flags.StringVar(&config.fastTokenEnv, "fast-token-env", "QWEN_LLM_API_KEY", "optional environment variable containing the local LLM bearer token")
	flags.IntVar(&config.fastTokens, "fast-max-tokens", 32, "fast spoken micro-turn output-token limit")
	flags.StringVar(&config.slowModel, "slow-model", gemini.DefaultModel, "hosted slow model identity")
	flags.StringVar(&config.slowEndpoint, "slow-endpoint", "", "optional Gemini API endpoint override")
	flags.StringVar(&config.slowEffort, "slow-effort", "high", "slow reasoning effort: medium or high")
	flags.IntVar(&config.slowTokens, "slow-max-tokens", 2048, "slow continuation output-token limit")
	flags.DurationVar(&config.slowPace, "slow-preparation-min-interval", 0, "content-independent speculative slow launch interval")
	flags.StringVar(&config.ttsURL, "tts-url", "http://127.0.0.1:8081/v1/audio/speech", "local Fish Audio OpenAI-compatible speech endpoint")
	flags.StringVar(&config.ttsModel, "tts-model", openaitts.DefaultModel, "Fish Audio model identity")
	flags.StringVar(&config.ttsVoice, "tts-voice", "default", "Fish Audio voice or reference preset")
	flags.BoolVar(&config.validateWire, "validate-wire", true, "validate every client and server event against the pinned OpenAI schema")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("realtimegateway accepts flags only")
	}
	return serve(config)
}

func serve(config options) error {
	if strings.TrimSpace(config.listen) == "" {
		return errors.New("listen address is required")
	}
	if config.asrChunk <= 0 {
		return errors.New("ASR provider chunk must be positive")
	}
	if config.requestTimeout <= 0 || config.shutdownTimeout <= 0 {
		return errors.New("request and shutdown timeouts must be positive")
	}
	token := strings.TrimSpace(os.Getenv(config.tokenEnv))
	if token == "" {
		return fmt.Errorf("local gateway token is required in %s", config.tokenEnv)
	}
	geminiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	if geminiKey == "" {
		return errors.New("GEMINI_API_KEY is required for the canonical slow continuation")
	}
	fast, err := makeFastProvider(config, geminiKey)
	if err != nil {
		return fmt.Errorf("configure fast continuation: %w", err)
	}
	slowEffort, err := parseSlowEffort(config.slowEffort)
	if err != nil {
		return err
	}
	slow, err := gemini.New(gemini.Config{
		APIKey: geminiKey, Model: config.slowModel, Endpoint: config.slowEndpoint,
		Phase: trajectory.PhaseSlow, Effort: slowEffort,
		ToolAuthority: continuation.ToolAuthorityExecute, IncludeThoughts: false,
		RequestTimeout: config.requestTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure slow continuation: %w", err)
	}
	speech, err := openaitts.New(openaitts.Config{
		Endpoint: config.ttsURL, Model: config.ttsModel, Voice: config.ttsVoice,
		BearerToken: os.Getenv("FISH_AUDIO_API_KEY"), RequestTimeout: config.requestTimeout,
		FallbackSampleRate: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		return fmt.Errorf("configure Fish Audio speech: %w", err)
	}
	perceptionFactory := func() (v1.PerceptionProvider, error) {
		asr, err := qwenasr.New(qwenasr.Config{
			BaseURL: config.asrURL, Model: config.asrModel,
			BearerToken: os.Getenv("QWEN_ASR_API_KEY"), RequestTimeout: config.requestTimeout,
		})
		if err != nil {
			return nil, err
		}
		return asrbuffer.New(asrbuffer.Config{Provider: asr, MinimumChunk: config.asrChunk})
	}
	gateway, err := realtimegateway.New(realtimegateway.Config{
		Token: token, Model: config.publicModel, PerceptionFactory: perceptionFactory,
		FastProvider: fast, SlowProvider: slow, SpeechProvider: speech,
		FastMaxTokens: config.fastTokens, SlowMaxTokens: config.slowTokens,
		SlowPreparationMin: config.slowPace, ValidateWire: config.validateWire,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr: config.listen, Handler: gateway.Handler(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveError := make(chan error, 1)
	go func() { serveError <- httpServer.ListenAndServe() }()
	fmt.Printf("OpenRealtime gateway listening on http://%s/v1/realtime\n", config.listen)
	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), config.shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownContext)
	}
}

func makeFastProvider(config options, geminiKey string) (continuation.Provider, error) {
	provider := strings.ToLower(strings.TrimSpace(config.fastProvider))
	model := strings.TrimSpace(config.fastModel)
	switch provider {
	case "vllm":
		if model == "" {
			model = "qwen-fast"
		}
		return openaicompat.New(openaicompat.Config{
			APIKey: os.Getenv(config.fastTokenEnv), Model: model, BaseURL: config.fastURL,
			Provider: "vllm", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
			ToolAuthority: continuation.ToolAuthorityPropose,
			ThinkingMode:  openaicompat.ThinkingDisabled, DisableReasoningCapture: true,
			RequestTimeout: config.requestTimeout,
		})
	case "gemini":
		if model == "" {
			model = gemini.DefaultModel
		}
		return gemini.New(gemini.Config{
			APIKey: geminiKey, Model: model, Endpoint: config.fastEndpoint,
			Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
			ToolAuthority: continuation.ToolAuthorityPropose, IncludeThoughts: false,
			RequestTimeout: config.requestTimeout,
		})
	default:
		return nil, fmt.Errorf("fast provider must be vllm or gemini, got %q", config.fastProvider)
	}
}

func parseSlowEffort(value string) (continuation.Effort, error) {
	switch normalized := continuation.Effort(strings.ToLower(strings.TrimSpace(value))); normalized {
	case continuation.EffortMedium, continuation.EffortHigh:
		return normalized, nil
	default:
		return "", errors.New("slow effort must be medium or high")
	}
}
