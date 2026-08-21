package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
	webrtcadapter "github.com/bojieli/OpenRealtime/transport/webrtc"
	"github.com/pion/webrtc/v4"
)

type serveOptions struct {
	listen   string
	binding  string
	model    string
	tokenEnv string

	requestTimeout  time.Duration
	shutdownTimeout time.Duration

	asrURL     string
	asrModel   string
	asrCadence time.Duration

	fastProvider string
	fastURL      string
	fastModel    string
	fastTokenEnv string
	fastTokens   int

	slowProvider string
	slowURL      string
	slowModel    string
	slowTokenEnv string
	slowEffort   string
	slowTokens   int

	ttsURL   string
	ttsModel string
	ttsVoice string

	upstreamURL      string
	upstreamModel    string
	upstreamTokenEnv string

	observers      string
	components     string
	narrator       string
	visionURL      string
	visionModel    string
	visionTokenEnv string

	rollout      string
	cadence      time.Duration
	observation  string
	toolProgress bool
	instruction  string
	validateWire bool

	webrtcListen string
	webrtcSTUN   string
}

func runServe(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime serve", flag.ContinueOnError)
	var options serveOptions
	flags.StringVar(&options.listen, "listen", "127.0.0.1:8765", "HTTP and WebSocket listen address")
	flags.StringVar(&options.binding, "binding", "cascade", "voice stack: cascade or upstream")
	flags.StringVar(&options.model, "model", "openrealtime", "compatibility model identifier reported to clients")
	flags.StringVar(&options.tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token; empty disables authentication")
	flags.DurationVar(&options.requestTimeout, "request-timeout", 2*time.Minute, "per-request provider timeout")
	flags.DurationVar(&options.shutdownTimeout, "shutdown-timeout", 15*time.Second, "graceful shutdown timeout")

	flags.StringVar(&options.asrURL, "asr-url", qwenasr.DefaultBaseURL, "streaming recogniser base URL")
	flags.StringVar(&options.asrModel, "asr-model", qwenasr.DefaultModel, "recogniser model identity")
	flags.DurationVar(&options.asrCadence, "asr-cadence", 200*time.Millisecond, "how often the recogniser is advanced")

	flags.StringVar(&options.fastProvider, "fast-provider", "openai-compatible", "fast provider: openai-compatible or gemini")
	flags.StringVar(&options.fastURL, "fast-url", openaicompat.DefaultBaseURL, "fast model base URL")
	flags.StringVar(&options.fastModel, "fast-model", "qwen-fast", "fast model identity")
	flags.StringVar(&options.fastTokenEnv, "fast-token-env", "OPENREALTIME_FAST_API_KEY", "environment variable holding the fast model credential")
	flags.IntVar(&options.fastTokens, "fast-max-tokens", 96, "fast spoken turn output-token limit")

	flags.StringVar(&options.slowProvider, "slow-provider", "gemini", "slow provider: gemini or openai-compatible")
	flags.StringVar(&options.slowURL, "slow-url", openaicompat.DefaultBaseURL, "slow model base URL when it is OpenAI-compatible")
	flags.StringVar(&options.slowModel, "slow-model", gemini.DefaultModel, "slow model identity")
	flags.StringVar(&options.slowTokenEnv, "slow-token-env", "GEMINI_API_KEY", "environment variable holding the slow model credential")
	flags.StringVar(&options.slowEffort, "slow-effort", "high", "slow reasoning effort: minimal, low, medium, or high")
	flags.IntVar(&options.slowTokens, "slow-max-tokens", 2048, "slow continuation output-token limit")

	flags.StringVar(&options.ttsURL, "tts-url", "http://127.0.0.1:8081/v1/audio/speech", "speech synthesis endpoint")
	flags.StringVar(&options.ttsModel, "tts-model", openaitts.DefaultModel, "speech model identity")
	flags.StringVar(&options.ttsVoice, "tts-voice", "default", "speech voice or reference preset")

	flags.StringVar(&options.upstreamURL, "upstream-url", "", "remote Realtime endpoint for the upstream binding")
	flags.StringVar(&options.upstreamModel, "upstream-model", "", "remote model identity")
	flags.StringVar(&options.upstreamTokenEnv, "upstream-token-env", "OPENAI_API_KEY", "environment variable holding the remote credential")

	flags.StringVar(&options.observers, "observers", "audio", "observer set: audio, audio+video, or video")
	flags.StringVar(&options.components, "observer-components", "narration", "video observer components: keyframe+narration, narration, or keyframe")
	flags.StringVar(&options.narrator, "narrator", "session", "narrator composition: session or dedicated")
	flags.StringVar(&options.visionURL, "vision-url", openaivision.DefaultBaseURL, "vision model base URL used for narration")
	flags.StringVar(&options.visionModel, "vision-model", "", "vision model identity; required when a video observer is enabled")
	flags.StringVar(&options.visionTokenEnv, "vision-token-env", "OPENREALTIME_VISION_API_KEY", "environment variable holding the vision model credential")
	flags.StringVar(&options.rollout, "rollout", "fast+slow", "cognition rollout: fast-only, fast+slow, or endpointed-slow-only")
	flags.DurationVar(&options.cadence, "trigger-cadence", interaction.DefaultCadence, "trigger cadence")
	flags.StringVar(&options.observation, "observation-policy", "endpoint-only", "canonical observation policy: endpoint-only or stable-partial")
	flags.BoolVar(&options.toolProgress, "tool-progress", false, "let a completed tool result trigger a short spoken status")
	flags.StringVar(&options.instruction, "instructions", "", "agent instruction composed ahead of every phase instruction")
	flags.BoolVar(&options.validateWire, "validate-wire", true, "validate every protocol event against the pinned schema")
	flags.StringVar(&options.webrtcListen, "webrtc-listen", "", "additional WebRTC listen address; empty disables the adapter")
	flags.StringVar(&options.webrtcSTUN, "webrtc-stun", "", "comma-separated STUN servers for the WebRTC adapter")
	flags.SetOutput(output)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve accepts flags only")
	}
	return serve(options, output)
}

func serve(options serveOptions, output io.Writer) error {
	if strings.TrimSpace(options.listen) == "" {
		return errors.New("a listen address is required")
	}
	bind, err := buildBinding(options)
	if err != nil {
		return err
	}
	server, err := gateway.New(gateway.Config{
		Binding: bind, Token: os.Getenv(options.tokenEnv), Model: options.model,
		TranscriptionModel: options.asrModel, ValidateWire: options.validateWire,
	})
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr: options.listen, Handler: server.Handler(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveError := make(chan error, 2)
	go func() { serveError <- httpServer.ListenAndServe() }()
	fmt.Fprintf(output, "OpenRealtime %s listening on http://%s/v1/realtime\n", bind.Name(), options.listen)
	fmt.Fprintf(output, "  health   http://%s/healthz\n", options.listen)

	var webrtcServer *http.Server
	if strings.TrimSpace(options.webrtcListen) != "" {
		webrtcServer, err = startWebRTC(options, serveError)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "  webrtc   http://%s/v1/realtime (SDP offer)\n", options.webrtcListen)
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), options.shutdownTimeout)
			defer cancel()
			_ = webrtcServer.Shutdown(shutdown)
		}()
	}
	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), options.shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}

func buildBinding(options serveOptions) (binding.Binding, error) {
	policies, err := buildPolicies(options)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(options.binding)) {
	case "cascade", "":
		return buildCascade(options, policies)
	case "upstream":
		return buildUpstream(options)
	default:
		return nil, fmt.Errorf("binding must be cascade or upstream, got %q", options.binding)
	}
}

func buildPolicies(options serveOptions) (interaction.Policies, error) {
	policies := interaction.Defaults()
	rollout, err := interaction.ParseRollout(options.rollout, interaction.RolloutOptions{
		ToolResultProgress: options.toolProgress,
	})
	if err != nil {
		return interaction.Policies{}, err
	}
	policies.Rollout = rollout
	policies.Trigger = interaction.NewFixedCadenceTrigger(options.cadence)
	return policies, nil
}

func buildCascade(options serveOptions, policies interaction.Policies) (binding.Binding, error) {
	fast, err := buildFast(options)
	if err != nil {
		return nil, fmt.Errorf("configure the fast provider: %w", err)
	}
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	speech, err := openaitts.New(openaitts.Config{
		Endpoint: options.ttsURL, Model: options.ttsModel, Voice: options.ttsVoice,
		BearerToken: os.Getenv("OPENREALTIME_TTS_API_KEY"), RequestTimeout: options.requestTimeout,
		FallbackSampleRate: 24_000, OutputSampleRateHz: 24_000,
	})
	if err != nil {
		return nil, fmt.Errorf("configure speech synthesis: %w", err)
	}
	observation, err := cascade.ParseObservationPolicy(options.observation)
	if err != nil {
		return nil, err
	}
	observers, err := buildObservers(options)
	if err != nil {
		return nil, err
	}
	return cascade.New(cascade.Config{
		Observers: observers,
		Perception: func() (v1.PerceptionProvider, error) {
			recogniser, err := qwenasr.New(qwenasr.Config{
				BaseURL: options.asrURL, Model: options.asrModel,
				BearerToken: os.Getenv("OPENREALTIME_ASR_API_KEY"), RequestTimeout: options.requestTimeout,
			})
			if err != nil {
				return nil, err
			}
			return asrbuffer.New(asrbuffer.Config{Provider: recogniser, MinimumChunk: options.asrCadence})
		},
		ASRCadence: options.asrCadence,
		Fast:       fast, Slow: slow, Speech: speech,
		FastMaxTokens: options.fastTokens, SlowMaxTokens: options.slowTokens,
		Policies: policies, ObservationPolicy: observation,
		AgentInstruction: options.instruction,
	})
}

func buildUpstream(options serveOptions) (binding.Binding, error) {
	if strings.TrimSpace(options.upstreamURL) == "" {
		return nil, errors.New("the upstream binding requires -upstream-url")
	}
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	return upstream.New(upstream.Config{
		URL: options.upstreamURL, Model: options.upstreamModel,
		Token: os.Getenv(options.upstreamTokenEnv), Slow: slow,
		SlowMaxTokens: options.slowTokens, AgentInstruction: options.instruction,
	})
}

// buildFast configures the voice.
//
// It is always silent-capable and always proposal-only: the fast provider
// cannot call tools, and that is a property of the descriptor here rather than
// a convention the prompt is trusted to follow.
func buildFast(options serveOptions) (continuation.Provider, error) {
	switch strings.ToLower(strings.TrimSpace(options.fastProvider)) {
	case "openai-compatible", "vllm", "":
		return openaicompat.New(openaicompat.Config{
			APIKey: os.Getenv(options.fastTokenEnv), Model: options.fastModel, BaseURL: options.fastURL,
			Provider: "openai-compatible", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
			ToolAuthority: continuation.ToolAuthorityPropose, SpeechAuthority: continuation.SpeechAuthorityVoice,
			ThinkingMode: openaicompat.ThinkingDisabled, DisableReasoningCapture: true,
			RequestTimeout: options.requestTimeout,
		})
	case "gemini":
		key := os.Getenv(options.slowTokenEnv)
		return gemini.New(gemini.Config{
			APIKey: key, Model: options.fastModel, Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
			SpeechAuthority: continuation.SpeechAuthorityVoice, RequestTimeout: options.requestTimeout,
		})
	default:
		return nil, fmt.Errorf("fast provider must be openai-compatible or gemini, got %q", options.fastProvider)
	}
}

// buildSlow configures the background reasoner. It is always silent: its
// output is voiced by a fast continuation, never spoken directly.
func buildSlow(options serveOptions) (continuation.Provider, error) {
	effort, err := parseEffort(options.slowEffort)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(options.slowProvider)) {
	case "gemini", "":
		key := os.Getenv(options.slowTokenEnv)
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("the slow provider needs a credential in %s", options.slowTokenEnv)
		}
		return gemini.New(gemini.Config{
			APIKey: key, Model: options.slowModel, Phase: trajectory.PhaseSlow, Effort: effort,
			ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
			IncludeThoughts: false, RequestTimeout: options.requestTimeout,
		})
	case "openai-compatible", "vllm":
		return openaicompat.New(openaicompat.Config{
			APIKey: os.Getenv(options.slowTokenEnv), Model: options.slowModel, BaseURL: options.slowURL,
			Provider: "openai-compatible", Phase: trajectory.PhaseSlow, Effort: effort,
			ToolAuthority: continuation.ToolAuthorityExecute, SpeechAuthority: continuation.SpeechAuthoritySilent,
			RequestTimeout: options.requestTimeout,
		})
	default:
		return nil, fmt.Errorf("slow provider must be gemini or openai-compatible, got %q", options.slowProvider)
	}
}

func parseEffort(value string) (continuation.Effort, error) {
	switch effort := continuation.Effort(strings.ToLower(strings.TrimSpace(value))); effort {
	case continuation.EffortMinimal, continuation.EffortLow, continuation.EffortMedium, continuation.EffortHigh:
		return effort, nil
	default:
		return "", fmt.Errorf("reasoning effort must be minimal, low, medium, or high, got %q", value)
	}
}

// buildObservers composes the session's perception.
//
// The observer set and its components are measured factors, so they are flags
// rather than build-time choices: a cell of the measurement matrix is a
// command line.
func buildObservers(options serveOptions) ([]perception.Factory, error) {
	set, err := perception.ParseObserverSet(options.observers)
	if err != nil {
		return nil, err
	}
	if set == perception.SetAudioOnly {
		return nil, nil
	}
	components, err := perception.ParseComponents(options.components)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.visionModel) == "" {
		return nil, errors.New("a video observer needs -vision-model: narration is what it produces")
	}
	vision, err := openaivision.New(openaivision.Config{
		BaseURL: options.visionURL, Model: options.visionModel,
		APIKey: os.Getenv(options.visionTokenEnv), RequestTimeout: options.requestTimeout,
	})
	if err != nil {
		return nil, err
	}
	var narrator perception.Narrator
	switch strings.ToLower(strings.TrimSpace(options.narrator)) {
	case "session", "":
		narrator, err = perception.NewSessionNarrator(vision)
	case "dedicated":
		narrator, err = perception.NewDedicatedNarrator(vision)
	default:
		return nil, fmt.Errorf("narrator must be session or dedicated, got %q", options.narrator)
	}
	if err != nil {
		return nil, err
	}
	if components == perception.ComponentKeyframeOnly {
		// Keyframe-only is a measurement level, not a working configuration:
		// it asks what images are worth with no persistent text at all.
		narrator = perception.StaticNarrator{Text: "A new screen state was captured."}
	}
	return []perception.Factory{perception.VideoFactory(perception.VideoConfig{
		Narrator:        narrator,
		AttachKeyframes: components != perception.ComponentNarrationOnly,
	})}, nil
}

// startWebRTC brings up the transport adapter.
//
// It connects to this server's own protocol endpoint over a real WebSocket
// rather than reaching into it, which is the whole point: the adapter is a
// client, and running it in the same process changes nothing about that.
func startWebRTC(options serveOptions, serveError chan error) (*http.Server, error) {
	var iceServers []webrtc.ICEServer
	for _, server := range strings.Split(options.webrtcSTUN, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{server}})
	}
	adapter, err := webrtcadapter.New(webrtcadapter.Config{
		Endpoint:   "ws://" + options.listen + "/v1/realtime",
		Token:      os.Getenv(options.tokenEnv),
		Model:      options.model,
		ICEServers: iceServers,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the WebRTC adapter: %w", err)
	}
	server := &http.Server{
		Addr: options.webrtcListen, Handler: adapter.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { serveError <- server.ListenAndServe() }()
	return server, nil
}
