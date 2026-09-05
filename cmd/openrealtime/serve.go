package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	"github.com/bojieli/OpenRealtime/adapters/speakerid"
	"github.com/bojieli/OpenRealtime/adapters/wordtimings"
	"github.com/bojieli/OpenRealtime/admission"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/binding/clientcalls"
	"github.com/bojieli/OpenRealtime/binding/duplex"
	"github.com/bojieli/OpenRealtime/binding/omni"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/computeruse/browser"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/gateway"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/runtimeartifact"
	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/voices"
	"github.com/bojieli/OpenRealtime/policymodel"
	"github.com/bojieli/OpenRealtime/providers"
	serverprofile "github.com/bojieli/OpenRealtime/server"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
	webrtcadapter "github.com/bojieli/OpenRealtime/transport/webrtc"
	"github.com/pion/webrtc/v4"
)

type serveOptions struct {
	listen              string
	binding             string
	architectureRef     string
	architectureCatalog string
	launchProfile       string
	profile             string
	model               string
	tokenEnv            string

	requestTimeout  time.Duration
	shutdownTimeout time.Duration

	maxSessions       int
	writeTimeout      time.Duration
	keepaliveInterval time.Duration

	asrProvider    string
	asrURL         string
	asrModel       string
	asrLanguage    string
	language       string
	asrCadence     time.Duration
	asrPartial     time.Duration
	asrEndpointing time.Duration

	fastProvider string
	fastEffort   string
	fastURL      string
	fastModel    string
	fastTokenEnv string
	fastTokens   int
	fastVision   bool

	reflexProvider string
	reflexURL      string
	reflexModel    string
	reflexTokenEnv string
	reflexTokens   int
	reflexTimeout  time.Duration

	slowProvider string
	slowURL      string
	slowModel    string
	slowTokenEnv string
	slowEffort   string
	slowTokens   int
	slowVision   bool

	ttsProvider string
	ttsURL      string
	ttsModel    string
	ttsVoice    string

	upstreamProvider string
	upstreamURL      string
	upstreamModel    string
	upstreamTokenEnv string

	observers      string
	components     string
	narrator       string
	narration      string
	visionProvider string
	visionURL      string
	visionModel    string
	visionTokenEnv string

	rollout         string
	preparation     string
	preparationPace time.Duration
	cadence         time.Duration
	observation     string
	toolProgress    bool
	instruction     string
	validateWire    bool

	logFormat string
	logLevel  string

	webrtcListen   string
	webrtcSTUN     string
	webrtcOrigin   string
	webrtcICE      string
	webrtcTokenEnv string
	webrtcCodec    string

	gpuCapacity              int
	policyURL                string
	policyModel              string
	policyTokenEnv           string
	policyGuided             bool
	policies                 string
	interactionShadow        string
	speakerURL               string
	wordTimingsURL           string
	wordTimingsModel         string
	wordTimingsLanguage      string
	wordTimingsInterval      time.Duration
	interactionFloor         bool
	interactionSees          bool
	profileTurns             bool
	speakBySentence          bool
	endpointSilenceMS        int
	interactionLiveness      time.Duration
	policyReasoning          string
	transcriptPolicy         string
	transcriptPartialRules   string
	transcriptPartialActs    string
	transcriptFinalRules     string
	transcriptFinalActs      string
	transcriptTimeout        time.Duration
	transcriptExtractTimeout time.Duration
	projectionHold           time.Duration
	holdingAfter             time.Duration
	bargeIn                  string
	bargeInHold              time.Duration

	computerUse         bool
	fastComputerUse     bool
	fastBackgroundTools bool
	browserURL          string
	browserTarget       string
	computerConfirm     string

	sidecarCommand      string
	sidecarAddress      string
	sidecarFloor        string
	sidecarInteraction  string
	sidecarCapabilities string
	sidecarProtocol     int
	sidecarVoice        string

	clientToolTimeout time.Duration

	// explicit records which flags the operator actually typed.
	//
	// It is what lets an endpoint default live in the provider catalogue
	// rather than in a flag default. A flag default would silently override
	// every catalogue entry - selecting Deepgram and getting a request sent to
	// the local recogniser's address - which is the kind of configuration bug
	// that looks like a broken provider.
	explicit map[string]bool
}

// chose reports whether a flag was set on the command line.
func (options serveOptions) chose(name string) bool { return options.explicit[name] }

// override returns value only when its flag was typed, so an untyped flag
// leaves the catalogue's endpoint in place.
func (options serveOptions) override(name, value string) string {
	if options.chose(name) {
		return value
	}
	return ""
}

func runServe(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("openrealtime serve", flag.ContinueOnError)
	var options serveOptions
	flags.StringVar(&options.listen, "listen", "127.0.0.1:8765", "HTTP and WebSocket listen address")
	flags.StringVar(&options.binding, "binding", "cascade", "voice stack preset: cascade, upstream, omni, omni+text-policy, duplex, or sidecar")
	flags.StringVar(&options.architectureRef, "architecture", "", "exact architecture id@revision; resolves structural binding, ownership, capabilities, and interaction")
	flags.StringVar(&options.architectureCatalog, "architecture-catalog", "", "external architecture catalog; empty uses the repository-owned catalog")
	flags.StringVar(&options.launchProfile, "launch-profile", "", "strict graph launch profile; empty preserves the legacy serve composition")
	flags.StringVar(&options.profile, "profile", "voice", "runtime profile: voice or voice+vision")
	flags.StringVar(&options.model, "model", "openrealtime", "compatibility model identifier reported to clients")
	flags.StringVar(&options.tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token; empty disables authentication")
	flags.DurationVar(&options.requestTimeout, "request-timeout", 2*time.Minute, "per-request provider timeout")
	flags.DurationVar(&options.shutdownTimeout, "shutdown-timeout", 15*time.Second, "graceful shutdown timeout")
	// Capacity is a deployment choice, so it is a flag rather than a shipped
	// default: only the operator knows how many concurrent conversations this
	// host's models and memory can carry. Unbounded stays the default because
	// silently capping an existing deployment at some number chosen here would
	// be a worse surprise than the exhaustion it prevents - but an unbounded
	// gateway fails by exhausting the process, so a production deployment
	// should set it. /metrics reports sessions_in_flight and sessions_rejected
	// so the number can be chosen from evidence.
	flags.IntVar(&options.maxSessions, "max-sessions", 0,
		"maximum concurrent realtime sessions; 0 is unbounded")
	flags.DurationVar(&options.writeTimeout, "write-timeout", 30*time.Second,
		"how long one send to a client may take before its session ends; 0 removes the bound")
	flags.DurationVar(&options.keepaliveInterval, "keepalive-interval", 20*time.Second,
		"how long a session may be idle before the server pings it; 0 disables the ping")

	flags.StringVar(&options.asrProvider, "asr-provider", "qwen-asr",
		"recogniser provider; openrealtime providers lists them")
	flags.StringVar(&options.asrURL, "asr-url", qwenasr.DefaultBaseURL,
		"recogniser endpoint; unset selects the provider's own")
	flags.StringVar(&options.asrModel, "asr-model", qwenasr.DefaultModel,
		"recogniser model identity; unset selects the provider's default")
	flags.StringVar(&options.asrLanguage, "asr-language", "",
		"recognition language; Deepgram defaults to en-US; multi is model-specific and excludes Mandarin on Nova-3; "+
			"empty inherits -language; qwen-asr detects the language itself and refuses one")
	flags.StringVar(&options.language, "language", "",
		"legacy shared language hint for recognition and synthesis; role-specific settings take precedence")
	flags.DurationVar(&options.asrCadence, "asr-cadence", 200*time.Millisecond, "how often the recogniser is advanced")
	flags.DurationVar(&options.asrPartial, "asr-partial-interval", 0,
		"ask a batch recogniser for a hypothesis this often by re-transcribing the utterance; "+
			"0 recognises only at the endpoint, and a streaming recogniser ignores it")
	flags.DurationVar(&options.asrEndpointing, "asr-endpointing", 300*time.Millisecond,
		"silence used by Deepgram's own VAD; batch recognisers ignore it and qwen-asr, whose endpoint the engine decides, refuses it")

	// vLLM rather than the generic entry, because the default endpoint below
	// is vLLM's own port and the quickstart's local stack is vLLM. The
	// difference is that vLLM can be asked to turn thinking off: a Qwen-class
	// model with it left on writes its deliberation into the content field,
	// and a fast budget of ninety-six tokens is spent on it before an answer
	// is reached - so the default was not a slower voice, it was no voice.
	// Point -fast-provider at openai-compatible for anything that is not vLLM.
	flags.StringVar(&options.fastProvider, "fast-provider", "vllm",
		"fast provider; openrealtime providers lists them")
	flags.StringVar(&options.fastURL, "fast-url", openaicompat.DefaultBaseURL,
		"fast model base URL; unset selects the provider's own")
	flags.StringVar(&options.fastModel, "fast-model", "qwen-fast",
		"fast model identity; unset selects the provider's small model")
	flags.StringVar(&options.fastTokenEnv, "fast-token-env", "",
		"environment variable holding the fast model credential; "+
			"empty reads OPENREALTIME_FAST_API_KEY and then the provider's conventional variable")
	flags.IntVar(&options.fastTokens, "fast-max-tokens", 0,
		"ceiling on fast-phase output tokens; zero means none, which is the default "+
			"because a provider spends this allowance on thinking before it speaks")
	flags.BoolVar(&options.fastVision, "fast-sees", false,
		"the fast model accepts images; false withholds them, which a text-only model requires")

	flags.StringVar(&options.reflexProvider, "visual-reflex-provider", "vllm",
		"optional visual reflex provider; enabled by -visual-reflex-model")
	flags.StringVar(&options.reflexURL, "visual-reflex-url", openaicompat.DefaultBaseURL,
		"visual reflex model base URL; unset selects the provider's own")
	flags.StringVar(&options.reflexModel, "visual-reflex-model", "",
		"visual reflex model identity; empty disables the visual reflex lane")
	flags.StringVar(&options.reflexTokenEnv, "visual-reflex-token-env", "",
		"environment variable holding the visual reflex credential; empty reads OPENREALTIME_VISUAL_REFLEX_API_KEY and then the provider's conventional variable")
	flags.IntVar(&options.reflexTokens, "visual-reflex-max-tokens", 96,
		"visual reflex act/wait/abstain output-token limit")
	flags.DurationVar(&options.reflexTimeout, "visual-reflex-timeout", 650*time.Millisecond,
		"hard deadline for one visual reflex decision")

	flags.StringVar(&options.slowProvider, "slow-provider", "gemini",
		"slow provider; openrealtime providers lists them")
	flags.StringVar(&options.slowURL, "slow-url", openaicompat.DefaultBaseURL,
		"slow model base URL; unset selects the provider's own")
	flags.StringVar(&options.slowModel, "slow-model", gemini.DefaultModel,
		"slow model identity; unset selects the provider's large model")
	flags.StringVar(&options.slowTokenEnv, "slow-token-env", "",
		"environment variable holding the slow model credential; "+
			"empty reads OPENREALTIME_SLOW_API_KEY and then the provider's conventional variable")
	flags.StringVar(&options.fastEffort, "fast-effort", "minimal",
		"how hard the voice thinks before it speaks: minimal, low, medium, high, or a "+
			"number of thinking tokens. minimal suits a small instruct model, which has "+
			"nothing to think with; a reasoning model needs a budget to judge whether this "+
			"is a turn to speak at all. a number and a name are alternatives, never both")
	flags.StringVar(&options.slowEffort, "slow-effort", "high", "slow reasoning effort: minimal, low, medium, or high")
	flags.IntVar(&options.slowTokens, "slow-max-tokens", 2048, "slow continuation output-token limit")
	flags.BoolVar(&options.slowVision, "slow-sees", false,
		"the slow model accepts images; ignored for gemini, which always can")

	flags.StringVar(&options.ttsProvider, "tts-provider", "openai-compatible",
		"speech provider; openrealtime providers lists them")
	flags.StringVar(&options.ttsURL, "tts-url", "http://127.0.0.1:8081/v1/audio/speech",
		"speech synthesis endpoint; unset selects the provider's own")
	flags.StringVar(&options.ttsModel, "tts-model", openaitts.DefaultModel,
		"speech model identity; unset selects the provider's default")
	flags.StringVar(&options.ttsVoice, "tts-voice", "default", "speech voice or reference preset")

	flags.StringVar(&options.upstreamProvider, "upstream-provider", "openai",
		"remote realtime endpoint; openrealtime providers -role upstream lists them")
	flags.StringVar(&options.upstreamURL, "upstream-url", "",
		"remote realtime endpoint URL; unset selects the provider's own")
	flags.StringVar(&options.upstreamModel, "upstream-model", "",
		"remote model identity; unset selects the provider's default")
	flags.StringVar(&options.upstreamTokenEnv, "upstream-token-env", "",
		"environment variable holding the remote credential; "+
			"empty reads OPENREALTIME_UPSTREAM_API_KEY and then the provider's conventional variable")

	flags.StringVar(&options.observers, "observers", "audio", "observer set: audio, audio+video, or video")
	flags.StringVar(&options.components, "observer-components", "narration", "video observer components: keyframe+narration, narration, or keyframe")
	flags.StringVar(&options.narrator, "narrator", "session", "narrator composition: session or dedicated")
	flags.StringVar(&options.narration, "narration", "describe", "narration style: describe, or actionable to include control positions for computer use")
	flags.StringVar(&options.visionProvider, "vision-provider", "openai-compatible",
		"narration provider; it must speak Chat Completions and serve a model that can see")
	flags.StringVar(&options.visionURL, "vision-url", openaivision.DefaultBaseURL,
		"vision model base URL used for narration; unset selects the provider's own")
	flags.StringVar(&options.visionModel, "vision-model", "", "vision model identity; required when a video observer is enabled")
	flags.StringVar(&options.visionTokenEnv, "vision-token-env", "OPENREALTIME_VISION_API_KEY", "environment variable holding the vision model credential")
	flags.StringVar(&options.rollout, "rollout", "fast+slow", "cognition rollout: fast-only, fast+slow, or endpointed-slow-only")
	flags.StringVar(&options.preparation, "preparation", "endpoint-only",
		"speculative preparation: endpoint-only or continuous")
	flags.DurationVar(&options.preparationPace, "preparation-slow-pace", time.Second,
		"how often continuous preparation may speculatively start the slow provider; 0 is unbounded")
	flags.DurationVar(&options.cadence, "trigger-cadence", interaction.DefaultCadence, "trigger cadence")
	flags.StringVar(&options.observation, "observation-policy", "endpoint-only", "canonical observation policy: endpoint-only or stable-partial")
	flags.BoolVar(&options.toolProgress, "tool-progress", false, "let a completed tool result trigger a short spoken status")
	flags.StringVar(&options.instruction, "instructions", "", "agent instruction composed ahead of every phase instruction")
	flags.BoolVar(&options.validateWire, "validate-wire", true, "validate every protocol event against the pinned schema")
	flags.StringVar(&options.logFormat, "log-format", "text", "structured log format: text or json")
	flags.StringVar(&options.logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.StringVar(&options.webrtcListen, "webrtc-listen", "", "additional WebRTC listen address; empty disables the adapter")
	flags.StringVar(&options.webrtcSTUN, "webrtc-stun", "", "comma-separated STUN servers for the WebRTC adapter")
	flags.StringVar(&options.webrtcICE, "webrtc-ice-server", "",
		"comma-separated ICE servers with credentials as url|username|credential; repeat for more than one. TURN is what a client behind symmetric NAT needs, and STUN alone cannot give it")
	flags.StringVar(&options.webrtcCodec, "webrtc-audio-codec", "",
		"audio codec sent to a WebRTC peer: pcmu or opus. PCMU is 8 kHz and always available; opus carries the session's full 24 kHz output and needs a build tagged opus, because libopus means cgo and the release binaries are static and reproducible")
	flags.StringVar(&options.webrtcTokenEnv, "webrtc-token-env", "",
		"environment variable holding the bearer credential a WebRTC caller must present; required when -webrtc-listen is not loopback, because the adapter spends the deployment's own upstream credential")
	flags.StringVar(&options.webrtcOrigin, "webrtc-allow-origin", "",
		"comma-separated web origins allowed to POST an SDP offer, or \"*\"; empty allows none, which is right unless a browser on another origin has to reach the adapter")
	flags.BoolVar(&options.computerUse, "computer-use", false, "declare the computer.* tools against a browser target")
	flags.BoolVar(&options.fastComputerUse, "fast-computer-use", false,
		"let the fast provider execute only bounded standard computer.* actions")
	flags.BoolVar(&options.fastBackgroundTools, "fast-background-tools", false,
		"let the fast provider start only tools explicitly declared background-safe")
	flags.StringVar(&options.browserURL, "browser-devtools-url", "http://127.0.0.1:9222", "browser DevTools endpoint for computer use")
	flags.StringVar(&options.browserTarget, "browser-target", "", "connect directly to a known page WebSocket instead of discovering one")
	flags.StringVar(&options.computerConfirm, "computer-confirm", "", "override every computer.* confirmation requirement: never, policy, or always")
	flags.StringVar(&options.policies, "policy-models", "none", "comma-separated policy models: backchannel, turn-projection, overlap, interaction, all, or none")
	flags.StringVar(&options.interactionShadow, "interaction-shadow", "", "file to record shadow interaction decisions to; enabling it decides nothing")
	flags.StringVar(&options.speakerURL, "speaker-url", "", "speaker-embedding endpoint, so a voice that is not the one the session is with is not reported as the user; unset leaves that prior in place")
	flags.StringVar(&options.wordTimingsURL, "word-timings-url", "", "OpenAI-shaped transcription endpoint that returns word timestamps, used to find where in a sentence an interruption actually landed; unset models the boundary proportionally instead")
	flags.StringVar(&options.wordTimingsModel, "word-timings-model", wordtimings.DefaultModel, "recogniser the word-timing endpoint should load")
	flags.StringVar(&options.wordTimingsLanguage, "word-timings-language", "", "language hint for the word-timing endpoint; unset lets it decide")
	flags.DurationVar(&options.wordTimingsInterval, "word-timings-interval", 0, "how much new synthesised audio is worth another listen while an utterance is still being produced; zero selects one second")
	flags.BoolVar(&options.interactionFloor, "interaction-floor", false, "let the interaction model own turn-taking instead of the silence rule and the projection")
	flags.BoolVar(&options.interactionSees, "interaction-sees", false, "the interaction model can look at a frame, so pictures reach it directly instead of as a narration")
	flags.BoolVar(&options.profileTurns, "profile-turns", false, "log how long each stage of a turn took, one line per turn")
	flags.BoolVar(&options.speakBySentence, "speak-by-sentence", false, "synthesise the first sentence on its own, so speech starts before the rest is ready")
	flags.IntVar(&options.endpointSilenceMS, "endpoint-silence", 0, "how much quiet closes an utterance, in milliseconds; zero keeps the recogniser default")
	flags.DurationVar(&options.interactionLiveness, "interaction-liveness", 20*time.Second, "longest the interaction model may hold the floor past the silence threshold")
	flags.StringVar(&options.policyReasoning, "policy-reasoning", "chat_template_kwargs",
		"how the policy endpoint is told not to think: chat_template_kwargs, enable_thinking, reasoning_effort, thinking_object, or none for an instruct model")
	flags.StringVar(&options.transcriptPolicy, "transcript-policy", "none",
		"streaming transcript interaction policy: event-aware or none")
	flags.StringVar(&options.transcriptPartialRules, "transcript-partial-rules", "",
		"interaction-model instruction for provisional transcript events; required by event-aware policy")
	flags.StringVar(&options.transcriptPartialActs, "transcript-partial-acts",
		"listen,speak-through,interrupt,act-silently,keep-speaking,stop-speaking",
		"acts permitted on a provisional transcript event")
	flags.StringVar(&options.transcriptFinalRules, "transcript-final-rules", "",
		"interaction-model instruction for final transcript events; required by event-aware policy")
	flags.StringVar(&options.transcriptFinalActs, "transcript-final-acts",
		"listen,answer,act-silently,keep-speaking,stop-speaking",
		"acts permitted on a final transcript event")
	flags.DurationVar(&options.transcriptTimeout, "transcript-timeout", 250*time.Millisecond,
		"deadline for each partial or final transcript interaction decision")
	flags.DurationVar(&options.transcriptExtractTimeout, "transcript-extraction-timeout", 2*time.Second,
		"deadline for Qwen to extract and classify standing interaction rules off the live path")
	flags.DurationVar(&options.holdingAfter, "holding-after", 2500*time.Millisecond,
		"how long the reasoner may run before the voice says what is happening; 0 leaves the user in silence")
	flags.DurationVar(&options.projectionHold, "projection-hold", time.Second,
		"how much extra silence a turn-projection model may buy by judging the turn unfinished")
	flags.StringVar(&options.bargeIn, "barge-in", "immediate", "barge-in policy: immediate, sustained, or never")
	flags.DurationVar(&options.bargeInHold, "barge-in-hold", 300*time.Millisecond, "how long a sustained barge-in policy holds the floor before yielding")
	flags.IntVar(&options.gpuCapacity, "compute-capacity", 0,
		"abstract units of local model capacity; 0 leaves narration, policy models, and preparation unadmitted")
	flags.StringVar(&options.policyURL, "policy-url", policymodel.DefaultBaseURL, "policy model base URL")
	flags.StringVar(&options.policyModel, "policy-model", "", "policy model identity; required when a policy model is enabled")
	flags.StringVar(&options.policyTokenEnv, "policy-token-env", "OPENREALTIME_POLICY_API_KEY", "environment variable holding the policy model credential")
	flags.BoolVar(&options.policyGuided, "policy-guided-choice", true, "ask the policy server to constrain decoding to the enumerated options")
	flags.StringVar(&options.sidecarCommand, "sidecar", "", "command that runs a model sidecar")
	flags.StringVar(&options.sidecarAddress, "sidecar-address", "", "connect to a running sidecar as tcp:host:port or unix:/path")
	flags.DurationVar(&options.clientToolTimeout, "client-tool-timeout", clientcalls.DefaultTimeout,
		"how long a client has to return a result for a tool it executes; a negative value waits forever")
	flags.StringVar(&options.sidecarFloor, "floor", "", "who decides endpoints: engine or model; empty selects the binding's default")
	flags.StringVar(&options.sidecarInteraction, "interaction-owner", "", "who selects interaction acts for a composed sidecar: engine or model")
	flags.StringVar(&options.sidecarCapabilities, "sidecar-capabilities", "audio-input,audio-output,turn-generation", "available capabilities for -binding sidecar: audio-input, audio-output, visual-input, transcription, turn-generation, concurrent-io, native-floor, native-interaction, interaction-acts, text-injection")
	flags.IntVar(&options.sidecarProtocol, "sidecar-protocol", 0, "sidecar protocol version; 0 selects the preset default (v1, v2 for typed interaction, or v3 for visual input)")
	flags.StringVar(&options.sidecarVoice, "sidecar-voice", "",
		"voice for a model that has more than one; empty leaves the choice to the model")
	flags.SetOutput(output)
	// The file first, the command line over it. A deployment keeps its choices
	// in the file and varies one of them on the line without restating the
	// other ninety-six.
	flags.String("config", "", "path to a YAML settings file; anything given on the command line overrides it")
	path, err := configPath(arguments)
	if err != nil {
		return err
	}
	if path != "" {
		if err := loadConfig(flags, path); err != nil {
			return err
		}
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("serve accepts flags only")
	}
	options.explicit = map[string]bool{}
	flags.Visit(func(flag *flag.Flag) { options.explicit[flag.Name] = true })
	return serve(options, output)
}

// disabledWhenZero translates an operator-facing "0 means no bound" flag into
// the gateway's "negative means no bound" configuration.
func disabledWhenZero(value time.Duration) time.Duration {
	if value == 0 {
		return -1
	}
	return value
}

func serve(options serveOptions, output io.Writer) (returnErr error) {
	if strings.TrimSpace(options.listen) == "" {
		return errors.New("a listen address is required")
	}
	if options.shutdownTimeout <= 0 || options.shutdownTimeout > 2*time.Minute {
		return errors.New("shutdown timeout must be in (0,2m]")
	}
	if options.maxSessions < 0 {
		return errors.New("maximum concurrent sessions must not be negative")
	}
	if options.writeTimeout < 0 || options.keepaliveInterval < 0 {
		return errors.New("write timeout and keepalive interval must not be negative; 0 disables the bound")
	}
	logger, err := buildLogger(options)
	if err != nil {
		return err
	}
	// Install production cancellation before any strict-profile file read,
	// executable hashing, catalog validation, or graph preflight so a shutdown
	// signal can cancel the entire preparation path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Set when the models a turn waits on have answered once. Health reports
	// warming until then, because a caller that asks whether the server is
	// ready is asking whether its next turn will be answered properly.
	var warmed atomic.Bool
	profiled := strings.TrimSpace(options.launchProfile) != ""
	var (
		bundle            *serverprofile.Bundle
		providerName      string
		graphFingerprint  string
		clientModel       = options.model
		gatewayToken      string
		profileReadiness  *serveProfileReadiness
		profileChecks     []graphlaunch.ReadinessCheck
		profileGraph      *serverprofile.GraphBundle
		operatorAuthority management.Authorizer
	)
	if profiled {
		composition, err := newProductionProfiledServeComposition(
			ctx, options, logger,
		)
		if err != nil {
			return err
		}
		bundle = composition.Graph.ServerBundle
		providerName = composition.Profile.Adapter.ProfileName
		graphFingerprint = composition.Profile.Plan.PlanFingerprint
		clientModel = composition.Profile.Server.Model
		gatewayToken = composition.GatewayToken
		profileReadiness = composition.Readiness
		profileChecks = composition.Graph.Readiness
		profileGraph = composition.Graph
		operatorAuthority = composition.OperatorAuthorizer
	} else {
		bind, recogniser, err := buildBinding(options)
		if err != nil {
			return err
		}
		artifact, err := runtimeartifact.Executable("go://openrealtime/openrealtime-process")
		if err != nil {
			return fmt.Errorf("identify server plugin runtime: %w", err)
		}
		gatewayToken = os.Getenv(options.tokenEnv)
		bundle, err = serverprofile.NewBundle(serverprofile.BundleConfig{
			ProfileName: "openrealtime.server.realtime", ProfileRevision: 1,
			Provider: bind,
			Gateway: gateway.Config{
				Token: gatewayToken, Model: options.model,
				TranscriptionModel: options.asrModel, ValidateWire: options.validateWire,
				Logger:      logger,
				Recogniser:  recogniserReport(recogniser),
				Warm:        warmed.Load,
				MaxSessions: options.maxSessions,
				// The flag says 0 for "no bound" because that is what an
				// operator expects a limit of zero to mean. gateway.Config
				// reserves 0 for "use the shipped default" and spells no bound
				// as a negative, so the two are translated here rather than
				// leaving a flag whose documented value does nothing.
				WriteTimeout:      disabledWhenZero(options.writeTimeout),
				KeepaliveInterval: disabledWhenZero(options.keepaliveInterval),
			},
			ProviderArtifact: artifact, GatewayArtifact: artifact,
		})
		if err != nil {
			return err
		}
		providerName = bind.Name()
	}
	realm, err := bundle.Mount(ctx)
	if err != nil {
		return err
	}
	// The mounted server realm owns the gateway and its scoped management
	// plugins. Close it after listeners and active handlers have drained so no
	// route, capability, or session owner outlives the exact process profile.
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), options.shutdownTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, realm.Close(shutdown))
	}()
	handler := realm.Handler()
	if operatorAuthority != nil {
		operatorConfig, err := profileGraph.OperatorAPIConfig(operatorAuthority)
		if err != nil {
			return err
		}
		operatorAPI, err := managementserver.MountOperatorAPI(ctx, handler, operatorConfig)
		if err != nil {
			return err
		}
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), options.shutdownTimeout)
			defer cancel()
			returnErr = errors.Join(returnErr, operatorAPI.Close(shutdown))
		}()
		handler = operatorAPI.Handler()
	}
	httpServer := &http.Server{
		Addr: options.listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	serveError := make(chan error, 2)
	go func() { serveError <- httpServer.ListenAndServe() }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), options.shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	fmt.Fprintf(output, "OpenRealtime %s listening on http://%s/v1/realtime\n", providerName, options.listen)
	fmt.Fprintf(output, "  health   http://%s/healthz\n", options.listen)
	fmt.Fprintf(output, "  server profile %s\n", realm.Live().Fingerprint)
	if graphFingerprint != "" {
		fmt.Fprintf(output, "  graph launch %s\n", graphFingerprint)
	}
	if !profiled {
		go func() {
			warmModels(ctx, options)
			warmed.Store(true)
		}()
	} else {
		go profileReadiness.Run(ctx, profileChecks, logger)
	}

	var webrtcServer *http.Server
	if strings.TrimSpace(options.webrtcListen) != "" {
		webrtcServer, err = startWebRTC(options, gatewayToken, clientModel, serveError)
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "  webrtc   http://%s/v1/realtime/calls (SDP offer)\n", options.webrtcListen)
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

// buildBinding creates the binding and, when that binding owns perception,
// the accumulator its recognisers fold into. Every other binding returns a nil
// accumulator: there is no recogniser in the process to report on.
func buildBinding(options serveOptions) (binding.Binding, *asrbuffer.Accumulator, error) {
	var err error
	options, definition, err := applyArchitectureDefinition(options)
	if err != nil {
		return nil, nil, err
	}
	options, err = normalizeProfile(options)
	if err != nil {
		return nil, nil, err
	}
	bindingName := strings.ToLower(strings.TrimSpace(options.binding))
	if bindingName == "" {
		bindingName = "cascade"
	}
	if options.fastBackgroundTools && bindingName != "cascade" {
		return nil, nil, fmt.Errorf(
			"fast background tools are implemented by the cascade binding, not %q", bindingName)
	}
	if options.fastComputerUse && bindingName != "cascade" && bindingName != "omni" &&
		bindingName != "omni+text-policy" && bindingName != "sidecar" {
		return nil, nil, fmt.Errorf(
			"fast computer use is implemented by the cascade and sidecar bindings, not %q", bindingName)
	}
	if transcriptPolicyEnabled(options.transcriptPolicy) && bindingName != "cascade" {
		return nil, nil, fmt.Errorf(
			"the transcript-event policy is implemented by the cascade binding, not %q", bindingName)
	}
	governor, err := buildGovernor(options)
	if err != nil {
		return nil, nil, err
	}
	policies, err := buildPolicies(options, governor)
	if err != nil {
		return nil, nil, err
	}
	var bind binding.Binding
	var recogniser *asrbuffer.Accumulator
	switch bindingName {
	case "cascade":
		recogniser = asrbuffer.NewAccumulator()
		bind, err = buildCascade(options, policies, governor, recogniser)
	case "upstream":
		bind, err = buildUpstream(options)
	case "omni":
		bind, err = buildSidecarBinding(options, "omni", interaction.Policies{}, nil)
	case "omni+text-policy":
		if policies.Interaction == nil {
			return nil, nil, errors.New("omni+text-policy needs -policy-models interaction and -policy-model")
		}
		recogniser = asrbuffer.NewAccumulator()
		bind, err = buildSidecarBinding(options, "omni+text-policy", policies, recogniser)
	case "duplex":
		bind, err = buildSidecarBinding(options, "duplex", interaction.Policies{}, nil)
	case "sidecar":
		if policies.Interaction != nil {
			recogniser = asrbuffer.NewAccumulator()
		}
		bind, err = buildSidecarBinding(options, "sidecar", policies, recogniser)
	default:
		return nil, nil, fmt.Errorf("binding must be cascade, upstream, omni, omni+text-policy, duplex, or sidecar, got %q", options.binding)
	}
	if err != nil {
		return nil, nil, err
	}
	if definition != nil {
		bind, err = projectarch.Bind(*definition, bind)
		if err != nil {
			return nil, nil, err
		}
	}
	bind, err = graphbinding.New(bind)
	if err != nil {
		return nil, nil, fmt.Errorf("mount binding through Graph IR: %w", err)
	}
	return bind, recogniser, nil
}

// applyArchitectureDefinition resolves the project-level architecture before
// constructing providers. Deployment flags continue to supply concrete model
// endpoints, credentials, and voices; the catalog is authoritative for the
// structural choices that should not drift between serve and benchmark.
func applyArchitectureDefinition(
	options serveOptions,
) (serveOptions, *projectarch.Definition, error) {
	if strings.TrimSpace(options.architectureRef) == "" {
		if strings.TrimSpace(options.architectureCatalog) != "" {
			return options, nil, errors.New("-architecture-catalog requires -architecture")
		}
		return options, nil, nil
	}
	catalog, err := loadArchitectureCatalog(options.architectureCatalog)
	if err != nil {
		return options, nil, err
	}
	definition, err := catalog.Resolve(options.architectureRef)
	if err != nil {
		return options, nil, err
	}
	topology, err := definition.Topology()
	if err != nil {
		return options, nil, err
	}
	bindingName := map[projectarch.Topology]string{
		projectarch.TopologyComponents: "cascade",
		projectarch.TopologySidecar:    "sidecar",
		projectarch.TopologyUpstream:   "upstream",
	}[topology]
	if options.chose("binding") && !strings.EqualFold(strings.TrimSpace(options.binding), bindingName) {
		return options, nil, fmt.Errorf("architecture %s requires %s binding machinery, not %q",
			definition.Ref(), bindingName, options.binding)
	}
	options.binding = bindingName

	if topology == projectarch.TopologySidecar {
		floor := string(definition.Ownership.Floor)
		interactionOwner := string(definition.Ownership.Interaction)
		if options.chose("floor") && strings.TrimSpace(options.sidecarFloor) != floor {
			return options, nil, fmt.Errorf("architecture %s selects %s floor ownership", definition.Ref(), floor)
		}
		if options.chose("interaction-owner") && strings.TrimSpace(options.sidecarInteraction) != interactionOwner {
			return options, nil, fmt.Errorf("architecture %s selects %s interaction ownership", definition.Ref(), interactionOwner)
		}
		options.sidecarFloor = floor
		options.sidecarInteraction = interactionOwner
		if options.chose("sidecar-capabilities") {
			available, err := parseStackCapabilities(options.sidecarCapabilities)
			if err != nil {
				return options, nil, err
			}
			if missing := available.Missing(definition.Requires); len(missing) > 0 {
				return options, nil, fmt.Errorf("architecture %s needs sidecar capabilities %s",
					definition.Ref(), strings.Join(missing, ", "))
			}
		} else {
			options.sidecarCapabilities = strings.Join(definition.Requires.Names(), ",")
		}
		if options.chose("sidecar-protocol") && options.sidecarProtocol != definition.Interaction.ProtocolVersion {
			return options, nil, fmt.Errorf("architecture %s requires sidecar protocol v%d",
				definition.Ref(), definition.Interaction.ProtocolVersion)
		}
		options.sidecarProtocol = definition.Interaction.ProtocolVersion
	}

	hasInteraction := policySelectionHasInteraction(options.policies)
	switch definition.Interaction.Mode {
	case projectarch.InteractionTextPolicy, projectarch.InteractionComposed:
		if options.chose("policy-models") && !hasInteraction {
			return options, nil, fmt.Errorf("architecture %s requires the interaction policy model", definition.Ref())
		}
		if !hasInteraction {
			options.policies = "interaction"
		}
	case projectarch.InteractionPredicates, projectarch.InteractionNative, projectarch.InteractionRemote:
		if hasInteraction {
			return options, nil, fmt.Errorf("architecture %s does not select an external interaction model", definition.Ref())
		}
	}
	if control := definition.Interaction.Control; control != nil {
		if topology == projectarch.TopologyComponents {
			modelOwnsFloor := control.Selectors.TextPolicy && control.Arbitration == "single"
			if options.chose("interaction-floor") && options.interactionFloor != modelOwnsFloor {
				return options, nil, fmt.Errorf(
					"architecture %s selects interaction-floor=%t through %q arbitration",
					definition.Ref(), modelOwnsFloor, control.Arbitration)
			}
			options.interactionFloor = modelOwnsFloor
		} else if control.Arbitration == "predicate-floor" {
			return options, nil, fmt.Errorf(
				"architecture %s selects predicate-floor arbitration, which the %s topology cannot currently realise",
				definition.Ref(), topology)
		}
	}
	if expected := definition.Interaction.EvidenceCapabilities; expected != nil {
		if expected.Addressing {
			return options, nil, fmt.Errorf(
				"architecture %s selects addressing evidence, but no configured runtime component can produce it",
				definition.Ref())
		}
		// These composition slots currently belong to the component topology.
		// They are derived from data in the definition rather than from an
		// architecture-name switch, so an external catalog can compose another
		// supported vector without adding a new binding species.
		if topology == projectarch.TopologyComponents && definition.Interaction.UsesTextPolicy() {
			if options.chose("interaction-sees") && options.interactionSees != expected.DirectVisualInput {
				return options, nil, fmt.Errorf(
					"architecture %s selects direct_visual_input=%t",
					definition.Ref(), expected.DirectVisualInput)
			}
			options.interactionSees = expected.DirectVisualInput

			if expected.VisualDescription && !options.chose("observers") {
				options.observers = "audio+video"
			}
			observerSet, parseErr := perception.ParseObserverSet(options.observers)
			if parseErr != nil {
				return options, nil, parseErr
			}
			hasVisualDescription := observerSet != perception.SetAudioOnly
			if hasVisualDescription != expected.VisualDescription {
				return options, nil, fmt.Errorf(
					"architecture %s selects visual_description=%t, while observer set %q supplies it=%t",
					definition.Ref(), expected.VisualDescription, options.observers, hasVisualDescription)
			}
			if expected.DirectVisualInput {
				if !options.chose("observer-components") {
					options.components = string(perception.ComponentKeyframeNarration)
				}
				components, componentsErr := perception.ParseComponents(options.components)
				if componentsErr != nil {
					return options, nil, componentsErr
				}
				if components == perception.ComponentNarrationOnly {
					return options, nil, fmt.Errorf(
						"architecture %s selects direct visual input and requires retained keyframes",
						definition.Ref())
				}
			}
			hasSpeakerIdentity := strings.TrimSpace(options.speakerURL) != ""
			if expected.SpeakerIdentity && !hasSpeakerIdentity {
				return options, nil, fmt.Errorf(
					"architecture %s selects speaker_identity evidence and needs -speaker-url",
					definition.Ref())
			}
			if !expected.SpeakerIdentity && hasSpeakerIdentity {
				return options, nil, fmt.Errorf(
					"architecture %s excludes speaker_identity evidence; select a speaker-aware architecture revision",
					definition.Ref())
			}
		}
	}
	return options, &definition, nil
}

func policySelectionHasInteraction(value string) bool {
	for _, name := range strings.Split(strings.ToLower(value), ",") {
		if strings.TrimSpace(name) == "interaction" {
			return true
		}
	}
	return false
}

// recogniserReport adapts the accumulator to the gateway's reporting shape.
// A binding without a recogniser reports nothing at all rather than zeroes.
func recogniserReport(accumulator *asrbuffer.Accumulator) func() gateway.RecogniserSnapshot {
	if accumulator == nil {
		return nil
	}
	return func() gateway.RecogniserSnapshot {
		snapshot := accumulator.Snapshot()
		return gateway.RecogniserSnapshot{
			Utterances: snapshot.Utterances, InFlight: snapshot.InFlight,
			AdvanceInvocations:   snapshot.AdvanceInvocations,
			AdvanceFailures:      snapshot.AdvanceFailures,
			AdvanceElapsedNS:     snapshot.AdvanceElapsedNS,
			AdvanceMeanElapsedNS: snapshot.MeanAdvanceNS(),
			AdvanceMaxElapsedNS:  snapshot.AdvanceMaxElapsedNS,

			FinalizeInvocations:   snapshot.FinalizeInvocations,
			FinalizeFailures:      snapshot.FinalizeFailures,
			FinalizeElapsedNS:     snapshot.FinalizeElapsedNS,
			FinalizeMeanElapsedNS: snapshot.MeanFinalizeNS(),
			FinalizeMaxElapsedNS:  snapshot.FinalizeMaxElapsedNS,
		}
	}
}

// buildGovernor creates the one compute budget everything local shares.
//
// A deployment that runs its recogniser, its fast model, its synthesiser, its
// narrator, and its policy model on one GPU has one resource, and giving each
// of them a private budget is how a screen description ends up delaying a
// spoken turn. One governor, declared classes, cooperative preemption.
//
// It is off by default because it cannot be sized for you: the unit is
// abstract, the right number depends on the machine and the models, and a
// governor with a made-up capacity would throttle a deployment that was
// perfectly healthy. A deployment that is contending states its own number.
func buildGovernor(options serveOptions) (*admission.Governor, error) {
	if options.gpuCapacity <= 0 {
		return nil, nil
	}
	reserved := options.gpuCapacity / 2
	if reserved < 1 {
		reserved = 1
	}
	governor, err := admission.NewGovernor(admission.Config{
		Capacity: options.gpuCapacity, ReservedInteractive: reserved,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the compute governor: %w", err)
	}
	return governor, nil
}

func buildPolicies(options serveOptions, governor *admission.Governor) (interaction.Policies, error) {
	policies := interaction.Defaults()
	rollout, err := interaction.ParseRollout(options.rollout, interaction.RolloutOptions{
		ToolResultProgress: options.toolProgress,
	})
	if err != nil {
		return interaction.Policies{}, err
	}
	policies.Rollout = rollout
	policies.Trigger = interaction.NewFixedCadenceTrigger(options.cadence)
	switch strings.ToLower(strings.TrimSpace(options.preparation)) {
	case "", "endpoint-only", "none", "off":
		policies.Preparation = interaction.NewEndpointPreparation()
	case "continuous":
		policies.Preparation = interaction.NewContinuousPreparation(options.preparationPace)
	default:
		return interaction.Policies{}, fmt.Errorf(
			"preparation must be endpoint-only or continuous, got %q", options.preparation)
	}
	switch strings.ToLower(strings.TrimSpace(options.bargeIn)) {
	case "", "immediate":
		policies.BargeIn = interaction.NewImmediateBargeIn()
	case "sustained":
		policies.BargeIn = interaction.NewSustainedBargeIn(options.bargeInHold)
	case "never":
		policies.BargeIn = interaction.NewNeverBargeIn()
	default:
		return interaction.Policies{}, fmt.Errorf(
			"barge-in must be immediate, sustained, or never, got %q", options.bargeIn)
	}
	// The stable-partial observation policy exists to answer before the user
	// has finished, so it needs a deferral policy that will act before the user
	// has finished. Composing the pair here rather than leaving the operator to
	// discover the contradiction is the whole reason this function exists: the
	// binding refuses the incoherent pair, and a flag that produced a refusal
	// would be a flag nobody could use.
	if partial, err := cascade.ParseObservationPolicy(options.observation); err != nil {
		return interaction.Policies{}, err
	} else if partial == cascade.ObservationStablePartial {
		policies.Deferral = interaction.NewDuplexDeferral(interaction.DeferralOptions{
			AllowWhileUserSpeaking: true,
		})
	}
	if err := applyPolicyModels(&policies, options, governor); err != nil {
		return interaction.Policies{}, err
	}
	return policies, nil
}

// applyPolicyModels installs the small models that make the two judgement
// calls the rules are brittle at.
//
// They are additive: with none configured, backchannel is off and projection
// falls back to silence-only endpointing. The system runs either way; it is
// simply less alive without them, and that is the trade a deployment gets to
// make rather than one the build makes for it.
func applyPolicyModels(
	policies *interaction.Policies, options serveOptions, governor *admission.Governor,
) error {
	var backchannel, projection, overlap, wholeDecision bool
	for _, name := range strings.Split(strings.ToLower(strings.TrimSpace(options.policies)), ",") {
		switch strings.TrimSpace(name) {
		case "", "none":
		case "backchannel":
			backchannel = true
		case "turn-projection", "projection":
			projection = true
		case "overlap":
			overlap = true
		case "interaction":
			wholeDecision = true
		case "all", "both":
			backchannel, projection, overlap = true, true, true
		default:
			return fmt.Errorf(
				"policy models must be backchannel, turn-projection, overlap, interaction, all, or none, got %q", name)
		}
	}
	eventAware, err := parseTranscriptPolicy(options.transcriptPolicy)
	if err != nil {
		return err
	}
	if eventAware {
		if options.interactionFloor {
			return errors.New("event-aware transcript policy and -interaction-floor are parallel floor policies; select one")
		}
		observation, err := cascade.ParseObservationPolicy(options.observation)
		if err != nil {
			return err
		}
		if observation != cascade.ObservationEndpointOnly {
			return errors.New("event-aware transcript policy owns partial-event actions and requires endpoint-only canonical observations")
		}
		recogniser, err := providers.LookupASR(options.asrProvider)
		if err != nil {
			return err
		}
		if !recogniser.Streaming {
			return fmt.Errorf(
				"event-aware transcript policy needs a streaming recogniser; %q is batch", recogniser.Name)
		}
	}
	if !backchannel && !projection && !overlap && !wholeDecision && !eventAware {
		return nil
	}
	if strings.TrimSpace(options.policyModel) == "" {
		return errors.New("enabling a policy model needs -policy-model")
	}
	policyClient := func(timeout time.Duration) (*policymodel.Client, error) {
		return policymodel.New(policymodel.Config{
			BaseURL: options.policyURL, Model: options.policyModel,
			APIKey: os.Getenv(options.policyTokenEnv), GuidedChoice: options.policyGuided,
			Reasoning: openaicompat.ReasoningControl(options.policyReasoning),
			Timeout:   timeout,
			// Interactive: above speculative preparation, below the foreground
			// continuation. A backchannel that arrives after the moment for it has
			// passed is worse than no backchannel.
			Governor: governor, Class: admission.ClassInteractive,
		})
	}
	liveTimeout := time.Duration(0)
	if eventAware {
		liveTimeout = options.transcriptTimeout
	}
	decider, err := policyClient(liveTimeout)
	if err != nil {
		return err
	}
	if wholeDecision {
		model, err := interaction.NewInteractionModel(decider)
		if err != nil {
			return err
		}
		policies.Interaction = model
		if path := strings.TrimSpace(options.interactionShadow); path != "" {
			recorder, err := newShadowRecorder(path)
			if err != nil {
				return err
			}
			policies.ShadowInteraction = recorder
		}
	}
	if eventAware {
		partialActs, err := parseTranscriptActs(options.transcriptPartialActs)
		if err != nil {
			return fmt.Errorf("partial transcript acts: %w", err)
		}
		finalActs, err := parseTranscriptActs(options.transcriptFinalActs)
		if err != nil {
			return fmt.Errorf("final transcript acts: %w", err)
		}
		policy, err := interaction.NewTranscriptEventPolicy(decider, interaction.TranscriptEventOptions{
			Partial: interaction.TranscriptEventRules{
				Instruction: options.transcriptPartialRules, Acts: partialActs,
				Timeout: options.transcriptTimeout,
			},
			Final: interaction.TranscriptEventRules{
				Instruction: options.transcriptFinalRules, Acts: finalActs,
				Timeout: options.transcriptTimeout,
			},
		})
		if err != nil {
			return err
		}
		policies.TranscriptEvents = policy
	}
	if backchannel {
		policy, err := interaction.NewModelBackchannel(decider, interaction.BackchannelOptions{})
		if err != nil {
			return err
		}
		policies.Backchannel = policy
	}
	if overlap {
		policy, err := interaction.NewModelOverlapClassifier(decider)
		if err != nil {
			return err
		}
		policies.Overlap = policy
	}
	if projection {
		policy, err := interaction.NewModelProjection(decider, interaction.ProjectionOptions{})
		if err != nil {
			return err
		}
		policies.TurnProjection = policy
		// The floor consults the projection, so installing one without
		// rebuilding the floor would leave it unused - which is the kind of
		// silent no-op a measured factor must never be.
		policies.Floor = interaction.NewEngineFloor(interaction.EngineFloorOptions{
			Projection: policy, ProjectionHold: options.projectionHold,
		})
	}
	if wholeDecision || eventAware {
		// Extraction shares the endpoint but not the call shape: it needs a
		// policy back, which no enumeration can contain. Event-aware decisions
		// stay under their live deadline, while extraction is off the audio path
		// and needs enough time for its follow-up count/restriction/scope reads.
		// Sharing the 250 ms HTTP client silently turned those reads into their
		// false defaults under ordinary local contention.
		extractionGenerator := interaction.Generator(decider)
		if eventAware {
			extractionClient, err := policyClient(options.transcriptExtractTimeout)
			if err != nil {
				return err
			}
			extractionGenerator = extractionClient
		}
		extractor, err := interaction.NewExtractor(extractionGenerator)
		if err != nil {
			return err
		}
		policies.Extraction = extractor
	}
	// The act floor is installed last because it replaces the floor outright,
	// and the projection above rebuilds one. Installing it earlier would leave
	// it configured and overwritten - a flag that reports success and changes
	// nothing, which is the failure mode a measured factor must never have.
	if wholeDecision && options.interactionFloor {
		minimumAnswerSilence := time.Duration(options.endpointSilenceMS) * time.Millisecond
		floor, err := interaction.NewActFloor(policies.Interaction, interaction.ActFloorOptions{
			Liveness: options.interactionLiveness, SilenceDuration: minimumAnswerSilence,
		})
		if err != nil {
			return err
		}
		policies.Floor = floor
		bargeIn, err := interaction.NewActBargeIn(
			policies.Interaction, interaction.ActBargeInOptions{})
		if err != nil {
			return err
		}
		policies.BargeIn = bargeIn
	}
	return nil
}

func parseTranscriptPolicy(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "off":
		return false, nil
	case "event-aware", "events", "streaming-events":
		return true, nil
	default:
		return false, fmt.Errorf(
			"transcript policy must be event-aware or none, got %q", value)
	}
}

func transcriptPolicyEnabled(value string) bool {
	enabled, _ := parseTranscriptPolicy(value)
	return enabled
}

func parseTranscriptActs(value string) ([]interaction.Act, error) {
	var acts []interaction.Act
	for _, name := range strings.Split(value, ",") {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		acts = append(acts, interaction.Act(trimmed))
	}
	if len(acts) == 0 {
		return nil, errors.New("at least one act is required")
	}
	return acts, nil
}

func buildCascade(
	options serveOptions, policies interaction.Policies, governor *admission.Governor,
	recogniserMetrics *asrbuffer.Accumulator,
) (binding.Binding, error) {
	fast, err := buildFast(options)
	if err != nil {
		return nil, fmt.Errorf("configure the fast provider: %w", err)
	}
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	reflex, err := buildVisualReflex(options)
	if err != nil {
		return nil, fmt.Errorf("configure the visual reflex provider: %w", err)
	}
	speech, err := providers.NewTTS(providers.TTSRequest{
		Provider: options.ttsProvider, Model: options.override("tts-model", options.ttsModel),
		Voice: options.ttsVoice, BaseURL: options.override("tts-url", options.ttsURL),
		APIKey: os.Getenv("OPENREALTIME_TTS_API_KEY"), Language: options.language,
		OutputSampleRateHz: 24_000, RequestTimeout: options.requestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("configure speech synthesis: %w", err)
	}
	if options.speakBySentence {
		// A streaming synthesiser still reads the whole text before it emits
		// anything: measured, the wait for the first byte was 289ms for a
		// word and 911ms for a sentence. Handing it the first sentence first
		// is worth three to six hundred milliseconds on every spoken turn.
		speech = bysentence.Provider{Inner: speech}
	}
	observation, err := cascade.ParseObservationPolicy(options.observation)
	if err != nil {
		return nil, err
	}
	observers, narrator, err := buildObservers(options, governor)
	if err != nil {
		return nil, err
	}
	computer, err := buildComputerUse(options)
	if err != nil {
		return nil, err
	}
	defaults, err := defaultObserverSet(options.observers)
	if err != nil {
		return nil, err
	}
	recognise, err := buildRecogniser(options)
	if err != nil {
		return nil, fmt.Errorf("configure the recogniser: %w", err)
	}
	probe, err := recognise()
	if err != nil {
		return nil, fmt.Errorf("describe the recogniser: %w", err)
	}
	perceptionDescriptor := probe.Descriptor()
	if closer, ok := probe.(io.Closer); ok {
		_ = closer.Close()
	}
	var listener voices.Embedder
	var speakerDescriptor v1.Descriptor
	if endpoint := strings.TrimSpace(options.speakerURL); endpoint != "" {
		embedder, err := speakerid.New(speakerid.Config{Endpoint: endpoint})
		if err != nil {
			return nil, fmt.Errorf("configure the speaker embedding: %w", err)
		}
		listener = embedder
		speakerDescriptor = v1.Descriptor{
			Name: "speakerid/http", Version: speakerid.AdapterVersion,
			Capabilities: v1.Capabilities{},
		}
	}
	var wordTimings spoken.Aligner
	if endpoint := strings.TrimSpace(options.wordTimingsURL); endpoint != "" {
		aligner, err := wordtimings.New(wordtimings.Config{
			Endpoint: endpoint, Model: options.wordTimingsModel,
			Language: options.wordTimingsLanguage,
		})
		if err != nil {
			return nil, fmt.Errorf("configure the word-timing recogniser: %w", err)
		}
		wordTimings = aligner
	}
	return cascade.New(cascade.Config{
		Profile:                   options.profile,
		WordTimings:               wordTimings,
		WordTimingInterval:        options.wordTimingsInterval,
		Voices:                    listener,
		SpeakerIdentityDescriptor: speakerDescriptor,
		ClientToolTimeout:         options.clientToolTimeout,
		Observers:                 observers, DefaultObservers: defaults, Tools: computer.specs,
		Narrator:            narrator,
		DeciderSees:         options.interactionSees,
		ProfileTurns:        options.profileTurns,
		HoldLimit:           options.interactionLiveness,
		EndpointSilenceMS:   options.endpointSilenceMS,
		FastComputerUse:     options.fastComputerUse,
		FastBackgroundTools: options.fastBackgroundTools,
		Governor:            governor,
		ConfirmPolicy:       computer.policy,
		// Every executed action is already a trajectory item with causal
		// parents. This is the operational mirror of that, so an operator
		// reading logs can see a refusal without reading a transcript.
		ActionAudit: func(record action.Record) {
			fmt.Fprintf(os.Stderr, "tool-dispatch %s %s phase=%s target=%s confirmed=%t executed=%t error=%q\n",
				record.Name, record.CallID, record.ProducerPhase, record.Target,
				record.Confirmed, record.Executed, record.Error)
		},
		Perception: func() (v1.PerceptionProvider, error) {
			recogniser, err := recognise()
			if err != nil {
				return nil, err
			}
			return recogniserMetrics.New(asrbuffer.Config{Provider: recogniser, MinimumChunk: options.asrCadence})
		},
		PerceptionDescriptor: perceptionDescriptor,
		ASRCadence:           options.asrCadence, HoldingAfter: options.holdingAfter,
		Fast: fast, Slow: slow, Speech: speech,
		Voice:         options.ttsVoice,
		FastMaxTokens: options.fastTokens, SlowMaxTokens: options.slowTokens,
		VisualReflex: reflex, VisualReflexMaxTokens: options.reflexTokens,
		VisualReflexTimeout: options.reflexTimeout,
		Policies:            policies, ObservationPolicy: observation,
		AgentInstruction: options.instruction,
	})
}

func buildUpstream(options serveOptions) (binding.Binding, error) {
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{
		Provider: options.upstreamProvider,
		Model:    options.override("upstream-model", options.upstreamModel),
		URL:      options.override("upstream-url", options.upstreamURL),
		APIKey:   roleCredential(options, "upstream-token-env", options.upstreamTokenEnv, "OPENREALTIME_UPSTREAM_API_KEY"),
	})
	if err != nil {
		return nil, err
	}
	return upstream.New(upstream.Config{
		URL: settings.URL, Model: settings.Model, Token: settings.Token,
		Header: settings.Header, EventAliases: settings.EventAliases,
		Handoff: settings.Handoff, Dial: settings.Dial, Slow: slow,
		SlowMaxTokens: options.slowTokens, AgentInstruction: options.instruction,
		ClientToolTimeout: options.clientToolTimeout,
	})
}

// buildFast configures the voice.
//
// It is always silent-capable and proposal-only by default. The explicit
// fast-computer-use mode grants execution authority at the descriptor while
// cognition supplies only an exact server-owned computer-action allowlist at
// eligible safe points. It also does not reason - minimal effort with the
// provider's thinking switch turned off - because the fast phase exists to
// answer the question that was actually asked, now.
func buildFast(options serveOptions) (continuation.Provider, error) {
	authority := continuation.ToolAuthorityPropose
	if options.fastComputerUse || options.fastBackgroundTools {
		authority = continuation.ToolAuthorityExecute
	}
	// Minimal unless the deployment asks for more. A small instruct model has
	// nothing to think with and the budget is wasted latency; a reasoning
	// model given none makes the judgement this phase exists for badly.
	// Measured on eight turns replayed from real runs: the local instruct
	// model scores four, Gemini 3.5 Flash with no thinking scores four, and
	// the same model with a 512-token budget scores five - getting both
	// interpreting cases right, which five rewordings of the prompt could not.
	// It costs 1781ms a turn against 30ms, which is the trade a deployment
	// makes rather than one this code should make for it.
	// Empty is the flag's default rather than an error: a caller building
	// options in code has not asked for anything, and the answer to that is
	// what the flag would have given them.
	wanted := strings.TrimSpace(options.fastEffort)
	if wanted == "" {
		wanted = string(continuation.EffortMinimal)
	}
	fastEffort, err := parseEffort(wanted)
	if err != nil {
		return nil, err
	}
	return providers.NewLLM(providers.LLMRequest{
		Provider: options.fastProvider,
		Model:    modelOverride(options, "fast-model", options.fastModel, options.fastProvider, trajectory.PhaseFast),
		BaseURL:  options.override("fast-url", options.fastURL),
		APIKey:   roleCredential(options, "fast-token-env", options.fastTokenEnv, "OPENREALTIME_FAST_API_KEY"),
		Phase:    trajectory.PhaseFast, Effort: fastEffort,
		ToolAuthority:   authority,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
		Reason:          providers.ReasonOff,
		// The voice makes latency- and silence-controlling decisions. Sampling
		// the same complete question into both speech and <wait> is not useful
		// diversity; reserve provider-default sampling for the slow reasoner.
		Temperature:    float64Pointer(0),
		Vision:         visionOverride(options, "fast-sees", options.fastVision),
		RequestTimeout: options.requestTimeout,
	})
}

// warmModels sends one throwaway token through each model that a turn waits
// on, so the first caller does not pay for the last mile of loading them.
//
// A local server answers its health check long before it answers a request at
// speed: weights are mapped, the graph is not captured, the prefix cache is
// empty. Measured on the control scenario at ten repeats, the first run failed
// and the other nine passed, every time - the opening turn came back reasoner
// led at two seconds where the rest were the voice alone at seventy
// milliseconds, and the answer landed outside the window.
//
// It runs in the background because a server that refuses to listen until its
// models are warm is a server that looks broken for a minute, and the first
// caller is better served by a slow answer than by a refused connection.
//
// The recogniser already does this for itself. This is the same courtesy from
// the side that calls it.
func warmModels(ctx context.Context, options serveOptions) {
	warm := func(baseURL, model, tokenEnv, fallbackEnv string) {
		baseURL = strings.TrimSpace(baseURL)
		model = strings.TrimSpace(model)
		if baseURL == "" || model == "" {
			return
		}
		body, err := json.Marshal(map[string]any{
			"model": model, "max_tokens": 1, "temperature": 0,
			"messages": []map[string]string{{"role": "user", "content": "hello"}},
		})
		if err != nil {
			return
		}
		timed, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(timed, http.MethodPost,
			strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return
		}
		request.Header.Set("Content-Type", "application/json")
		key := os.Getenv(strings.TrimSpace(tokenEnv))
		if key == "" && fallbackEnv != "" {
			key = os.Getenv(fallbackEnv)
		}
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	// The voice and the model that decides whether it speaks. Both sit on the
	// path between hearing a word and answering it; the reasoner does not, and
	// warming a metered cloud endpoint would be somebody else's money.
	warm(options.fastURL, options.fastModel, options.fastTokenEnv, "OPENREALTIME_FAST_API_KEY")
	if options.policyURL != options.fastURL || options.policyModel != options.fastModel {
		warm(options.policyURL, options.policyModel, options.policyTokenEnv, "")
	}
}

func float64Pointer(value float64) *float64 { return &value }

// buildSlow configures the background reasoner. It is always silent: its
// output is voiced by a fast continuation, never spoken directly.
func buildSlow(options serveOptions) (continuation.Provider, error) {
	effort, err := parseEffort(options.slowEffort)
	if err != nil {
		return nil, err
	}
	return providers.NewLLM(providers.LLMRequest{
		Provider: options.slowProvider,
		Model:    modelOverride(options, "slow-model", options.slowModel, options.slowProvider, trajectory.PhaseSlow),
		BaseURL:  options.override("slow-url", options.slowURL),
		APIKey:   roleCredential(options, "slow-token-env", options.slowTokenEnv, "OPENREALTIME_SLOW_API_KEY"),
		Phase:    trajectory.PhaseSlow, Effort: effort,
		ToolAuthority:   continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
		Reason:          providers.ReasonOn,
		Vision:          visionOverride(options, "slow-sees", options.slowVision),
		RequestTimeout:  options.requestTimeout,
	})
}

// buildVisualReflex configures the optional one-shot visual action role. It
// is deliberately independent from buildFast: changing this role must not
// change the voice provider, prompt, token budget, or speech authority.
func buildVisualReflex(options serveOptions) (continuation.Provider, error) {
	if strings.TrimSpace(options.reflexModel) == "" {
		return nil, nil
	}
	sees := true
	return providers.NewLLM(providers.LLMRequest{
		Provider: options.reflexProvider,
		Model: modelOverride(
			options, "visual-reflex-model", options.reflexModel,
			options.reflexProvider, trajectory.PhaseFast),
		BaseURL: options.override("visual-reflex-url", options.reflexURL),
		APIKey: roleCredential(
			options, "visual-reflex-token-env", options.reflexTokenEnv,
			"OPENREALTIME_VISUAL_REFLEX_API_KEY"),
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority:   continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
		Reason:          providers.ReasonOff,
		Vision:          &sees,
		RequestTimeout:  options.reflexTimeout,
	})
}

// buildRecogniser resolves perception.
//
// It returns a factory because a recogniser owns one utterance: a WebSocket to
// Deepgram, a session on the local service, or a buffer being re-transcribed.
// One instance shared across concurrent utterances would interleave them.
func buildRecogniser(options serveOptions) (func() (v1.PerceptionProvider, error), error) {
	return providers.NewASRFactory(providers.ASRRequest{
		Provider: options.asrProvider,
		Model:    options.override("asr-model", options.asrModel),
		BaseURL:  options.override("asr-url", options.asrURL),
		APIKey:   os.Getenv("OPENREALTIME_ASR_API_KEY"),
		Language: recogniserLanguage(options), PartialInterval: options.asrPartial,
		Endpointing:    recogniserEndpointing(options),
		RequestTimeout: recogniserTimeout(options.asrCadence, options.requestTimeout),
	})
}

// recogniserLanguage keeps the historical shared -language setting as a
// fallback while giving recognition its own YAML-addressable choice. Deepgram
// otherwise relies on a service default that is easy to mistake for automatic
// multilingual recognition; make the actual default explicit and let a
// deployment opt into `multi` after checking the model's supported set (which
// does not include Mandarin for Nova-3) and measuring it on its languages.
func recogniserLanguage(options serveOptions) string {
	if language := strings.TrimSpace(options.asrLanguage); language != "" {
		// An explicit role-specific choice always reaches the provider
		// layer, which refuses it for a recogniser that cannot honour it.
		return language
	}
	if language := strings.TrimSpace(options.language); language != "" &&
		providers.ASRAcceptsLanguage(options.asrProvider) {
		// The shared legacy hint also configures synthesis. A recogniser
		// that detects the language itself simply does not take part in it,
		// rather than turning a synthesis choice into a recognition refusal.
		return language
	}
	if recogniser, err := providers.LookupASR(options.asrProvider); err == nil &&
		recogniser.Dialect == providers.DialectDeepgramListen {
		return "en-US"
	}
	return ""
}

// recogniserEndpointing forwards -asr-endpointing only where it means
// something.
//
// The flag's default exists for Deepgram, whose own VAD reads it. The default
// recogniser leaves endpointing to the engine's acoustic gate, and the
// provider layer refuses a value it cannot honour - so the default must not
// reach it, while an operator who actually typed the flag for such a
// recogniser is told it does nothing rather than having it dropped.
func recogniserEndpointing(options serveOptions) time.Duration {
	if options.chose("asr-endpointing") {
		return options.asrEndpointing
	}
	if recogniser, err := providers.LookupASR(options.asrProvider); err == nil &&
		recogniser.Dialect == providers.DialectDeepgramListen {
		return options.asrEndpointing
	}
	return 0
}

// recogniserTimeout bounds one advance of the recogniser.
//
// The shared provider timeout is sized for a reasoning model, which may
// legitimately think for a minute. A streaming recogniser advancing every two
// hundred milliseconds is a different kind of thing entirely, and giving it the
// same budget means a recogniser that has stopped answering can consume two
// minutes of a live conversation before anything is said about it - the client
// hears nothing, the log says nothing, and the session simply produces no
// turns. That failure was found in the field, not in a test: a local recogniser
// wedged on a concurrent request and every session against it went quiet for as
// long as anyone was willing to wait.
//
// The bound comes from the cadence rather than from a second knob, because the
// cadence is already the statement of how often this provider is expected to
// answer. A recogniser that has not answered in many multiples of its own
// cadence is not slow; it is not answering. The floor keeps a very short
// cadence from producing a bound that a healthy provider would trip on, and the
// shared timeout still caps it so that configuring a long cadence cannot
// quietly exceed the deployment's own limit.
func recogniserTimeout(cadence, shared time.Duration) time.Duration {
	const (
		multiple = 20
		floor    = 5 * time.Second
	)
	bound := time.Duration(multiple) * cadence
	if bound < floor {
		bound = floor
	}
	if shared > 0 && bound > shared {
		bound = shared
	}
	return bound
}

// roleCredential resolves a model credential.
//
// Two sources, in the order an operator would expect: the variable they named
// on the command line, then the role's own variable for running two profiles
// of one vendor on separate keys. Returning empty is not a failure - it hands
// the lookup to the catalogue, which is what makes `-slow-provider anthropic`
// work with nothing but ANTHROPIC_API_KEY in the environment.
//
// An untyped flag deliberately contributes nothing. Its default names one
// provider's variable, and reading that for whichever provider was actually
// selected would send, say, a Gemini key to Anthropic and report it as an
// authentication failure at the vendor rather than a configuration error here.
func roleCredential(options serveOptions, flagName, namedEnv, roleEnv string) string {
	if options.chose(flagName) {
		return strings.TrimSpace(os.Getenv(namedEnv))
	}
	return strings.TrimSpace(os.Getenv(roleEnv))
}

// modelOverride resolves which model identity to send.
//
// A typed flag always wins. An untyped one is only meaningful for a provider
// the catalogue has no default model for - the local servers, which serve
// whatever they were started with - and there the flag's own default is this
// project's local convention. For a provider that does name a default, an
// untyped flag must contribute nothing, or selecting a new provider would
// silently keep the previous one's model and fail at the vendor.
func modelOverride(
	options serveOptions, flagName, value, provider string, phase trajectory.Phase,
) string {
	if options.chose(flagName) {
		return value
	}
	entry, err := providers.LookupLLM(provider)
	if err != nil {
		return ""
	}
	catalogued := entry.SlowModel
	if phase == trajectory.PhaseFast {
		catalogued = entry.FastModel
	}
	if catalogued != "" {
		return ""
	}
	return value
}

// visionOverride reports an explicit sight declaration, or nil to take the
// provider catalogue's.
//
// The flags default to false, and a false default that overrode the catalogue
// would withhold images from every multimodal provider unless the operator
// remembered a flag. So an untyped flag defers, and a typed one decides.
func visionOverride(options serveOptions, flagName string, value bool) *bool {
	if !options.chose(flagName) {
		return nil
	}
	return &value
}

func parseEffort(value string) (continuation.Effort, error) {
	return continuation.ParseEffort(value)
}

// buildObservers composes the session's perception.
//
// The observer set and its components are measured factors, so they are flags
// rather than build-time choices: a cell of the measurement matrix is a
// command line.
// defaultObserverSet turns the configured level of factor F3 into the
// binding's default selection.
//
// The video-only level has to actually remove the recogniser. Before this, it
// added a video observer and left the audio one running, so it was the
// audio+video level with a different name on the report - a factor level that
// silently equals another level cannot measure anything.
func defaultObserverSet(configured string) ([]string, error) {
	set, err := perception.ParseObserverSet(configured)
	if err != nil {
		return nil, err
	}
	switch set {
	case perception.SetVideoOnly:
		return []string{"video"}, nil
	default:
		// Audio-only and audio+video are both "everything this deployment
		// configured", because the deployment only configures a video observer
		// for the second one.
		return nil, nil
	}
}

// buildObservers returns the video observer factories and the narrator behind
// them.
//
// The narrator comes back because a picture does not only arrive on a video
// stream. A client can put one straight into the conversation, and on that
// path there was nothing to turn it into text - so everything in the session
// that cannot see, the interaction model first among them, was handed "the
// user attached an image" and asked to decide from that.
func buildObservers(
	options serveOptions, governor *admission.Governor,
) ([]perception.Factory, perception.Narrator, error) {
	set, err := perception.ParseObserverSet(options.observers)
	if err != nil {
		return nil, nil, err
	}
	if set == perception.SetAudioOnly {
		return nil, nil, nil
	}
	components, err := perception.ParseComponents(options.components)
	if err != nil {
		return nil, nil, err
	}
	if components == perception.ComponentKeyframeOnly {
		// The low-latency visual reflex needs pixels, not a description produced
		// before it may start. A static observation keeps the canonical event and
		// media lifecycle intact without placing another model call on that path.
		narrator := perception.StaticNarrator{Text: "A new screen state was captured."}
		return []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Narrator: narrator, AttachKeyframes: true,
		})}, narrator, nil
	}
	label, narration, err := narratorComposition(options)
	if err != nil {
		return nil, nil, err
	}
	vision, err := providers.NewVision(providers.VisionRequest{
		Provider: narration.provider, Model: narration.model, BaseURL: narration.url,
		APIKey: os.Getenv(narration.tokenEnv), RequestTimeout: options.requestTimeout,
	})
	if err != nil {
		return nil, nil, err
	}
	prompt := ""
	switch strings.ToLower(strings.TrimSpace(options.narration)) {
	case "", "describe":
	case "actionable":
		prompt = perception.ActionableNarrationPrompt
	default:
		return nil, nil, fmt.Errorf("narration must be describe or actionable, got %q", options.narration)
	}
	var narrator perception.Narrator
	narrator, err = perception.NewNarrator(perception.NarratorConfig{
		Vision: vision, Prompt: prompt, Label: label,
		// Background: a description of the screen is worth having and never
		// worth delaying a spoken turn for.
		Governor: governor, Class: admission.ClassBackground,
	})
	if err != nil {
		return nil, nil, err
	}
	return []perception.Factory{perception.VideoFactory(perception.VideoConfig{
		Narrator:        narrator,
		AttachKeyframes: components != perception.ComponentNarrationOnly,
	})}, narrator, nil
}

// narratorSource is which endpoint narrates.
type narratorSource struct {
	provider string
	url      string
	model    string
	tokenEnv string
}

// narratorComposition resolves which model narrates, and what to call it.
//
// The two levels of this factor were a label until now: both asked for
// -vision-model and both pointed at whatever it named, so a report could say
// "session" about a separate model nobody in the session was using. A level
// that is a string rather than a difference measures nothing.
//
// A session narrator is the session's own model narrating as a side-output.
// That is the configuration the measured result used and the one that avoids
// putting a second model in the loop, so it takes the fast provider's endpoint
// and model rather than asking for them again. It requires that provider to be
// able to see, because a model that cannot is not narrating anything: the
// deployment wanted the dedicated level and should say so.
func narratorComposition(options serveOptions) (string, narratorSource, error) {
	switch strings.ToLower(strings.TrimSpace(options.narrator)) {
	case "session", "":
		fast, err := providers.LookupLLM(options.fastProvider)
		if err != nil {
			return "", narratorSource{}, err
		}
		sees := fast.Vision
		if options.chose("fast-sees") {
			sees = options.fastVision
		}
		if !sees {
			return "", narratorSource{}, errors.New(
				"a session narrator is the session's own model narrating as a side-output, and " +
					"this one is configured as text-only: pass -fast-sees if it can see, or " +
					"-narrator dedicated with -vision-model to use a separate one")
		}
		// The narrator client speaks Chat Completions and nothing else. A
		// fast provider on another dialect can still run the session; it just
		// cannot double as the narrator, and saying which flag fixes that is
		// more use than a decode error on the first video frame.
		if fast.Dialect != providers.DialectOpenAIChat {
			return "", narratorSource{}, fmt.Errorf(
				"a session narrator needs an OpenAI-compatible fast provider, and %q speaks %s; "+
					"use -narrator dedicated with -vision-model", fast.Name, fast.Dialect)
		}
		return "session", narratorSource{
			provider: options.fastProvider,
			url:      options.override("fast-url", options.fastURL),
			model:    modelOverride(options, "fast-model", options.fastModel, options.fastProvider, trajectory.PhaseFast),
			tokenEnv: options.fastTokenEnv,
		}, nil
	case "dedicated":
		if strings.TrimSpace(options.visionModel) == "" {
			return "", narratorSource{}, errors.New(
				"a dedicated narrator needs -vision-model: narration is what a video observer produces")
		}
		return "dedicated", narratorSource{
			provider: options.visionProvider,
			url:      options.override("vision-url", options.visionURL),
			model:    options.visionModel,
			tokenEnv: options.visionTokenEnv,
		}, nil
	default:
		return "", narratorSource{}, fmt.Errorf(
			"narrator must be session or dedicated, got %q", options.narrator)
	}
}

// startWebRTC brings up the transport adapter.
//
// It connects to this server's own protocol endpoint over a real WebSocket
// rather than reaching into it, which is the whole point: the adapter is a
// client, and running it in the same process changes nothing about that.
func startWebRTC(
	options serveOptions, gatewayToken, model string, serveError chan error,
) (*http.Server, error) {
	adapter, err := newWebRTCAdapter(options, gatewayToken, model)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Addr: options.webrtcListen, Handler: adapter.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { serveError <- server.ListenAndServe() }()
	return server, nil
}

func newWebRTCAdapter(
	options serveOptions, gatewayToken, model string,
) (*webrtcadapter.Adapter, error) {
	var iceServers []webrtc.ICEServer
	for _, server := range strings.Split(options.webrtcSTUN, ",") {
		server = strings.TrimSpace(server)
		if server == "" {
			continue
		}
		iceServers = append(iceServers, webrtc.ICEServer{URLs: []string{server}})
	}
	credentialed, err := parseICEServers(options.webrtcICE)
	if err != nil {
		return nil, err
	}
	iceServers = append(iceServers, credentialed...)
	clientCredential, err := webrtcClientCredential(options)
	if err != nil {
		return nil, err
	}
	codec, err := webrtcadapter.ParseAudioCodec(options.webrtcCodec)
	if err != nil {
		return nil, err
	}
	adapter, err := webrtcadapter.New(webrtcadapter.Config{
		Endpoint:         "ws://" + webrtcUpstreamAuthority(options.listen) + "/v1/realtime",
		AudioCodec:       codec,
		Token:            gatewayToken,
		Model:            model,
		ICEServers:       iceServers,
		AllowedOrigins:   splitList(options.webrtcOrigin),
		ClientCredential: clientCredential,
	})
	if err != nil {
		return nil, fmt.Errorf("configure the WebRTC adapter: %w", err)
	}
	return adapter, nil
}

// buildSidecarBinding configures a model that lives behind a process boundary.
// Named presets remain convenient defaults; the generic sidecar case parses a
// capability vector and independent floor/interaction owners, so a new
// combination is configuration rather than a new switch branch.
func buildSidecarBinding(
	options serveOptions, name string, policies interaction.Policies,
	recogniserMetrics *asrbuffer.Accumulator,
) (binding.Binding, error) {
	if strings.TrimSpace(options.sidecarCommand) == "" && strings.TrimSpace(options.sidecarAddress) == "" {
		return nil, fmt.Errorf("the %s binding needs -sidecar or -sidecar-address", name)
	}
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	config := sidecarbinding.Config{
		Sidecar: sidecar.Config{
			Command:         strings.Fields(options.sidecarCommand),
			Address:         options.sidecarAddress,
			ProtocolVersion: options.sidecarProtocol,
			Logf: func(format string, values ...any) {
				fmt.Fprintf(os.Stderr, format+"\n", values...)
			},
		},
		Instructions: options.instruction, Slow: slow, SlowMaxTokens: options.slowTokens,
		Voice:             options.sidecarVoice,
		ClientToolTimeout: options.clientToolTimeout,
		FastComputerUse:   options.fastComputerUse,
	}
	capabilities, err := parseStackCapabilities(options.sidecarCapabilities)
	if err != nil {
		return nil, err
	}
	config.ModelCapabilities = capabilities
	computer, err := buildComputerUse(options)
	if err != nil {
		return nil, err
	}
	config.Tools = computer.specs
	config.ConfirmPolicy = computer.policy
	config.ActionAudit = func(record action.Record) {
		fmt.Fprintf(os.Stderr, "tool-dispatch %s %s phase=%s target=%s confirmed=%t executed=%t error=%q\n",
			record.Name, record.CallID, record.ProducerPhase, record.Target,
			record.Confirmed, record.Executed, record.Error)
	}
	if capabilities.VisualInput && config.Sidecar.ProtocolVersion == 0 {
		config.Sidecar.ProtocolVersion = sidecar.VersionMultimodal
	}
	if policies.Interaction != nil {
		recognise, err := buildRecogniser(options)
		if err != nil {
			return nil, fmt.Errorf("configure interaction recogniser: %w", err)
		}
		if recogniserMetrics == nil {
			return nil, errors.New("engine interaction policy requires recogniser metrics")
		}
		// Capture the descriptor now, before the first utterance lazily opens its
		// recogniser. Constructors validate configuration but do no recognition;
		// this gives session evidence an exact adapter/model revision without
		// moving a network call onto startup or sharing utterance state.
		probe, err := recognise()
		if err != nil {
			return nil, fmt.Errorf("describe interaction recogniser: %w", err)
		}
		config.InteractionPerceptionDescriptor = probe.Descriptor()
		if closer, ok := probe.(io.Closer); ok {
			_ = closer.Close()
		}
		config.Policies = policies
		config.InteractionCadence = options.asrCadence
		config.InteractionPerception = func() (v1.PerceptionProvider, error) {
			provider, err := recognise()
			if err != nil {
				return nil, err
			}
			return recogniserMetrics.New(asrbuffer.Config{
				Provider: provider, MinimumChunk: options.asrCadence,
			})
		}
		if config.Sidecar.ProtocolVersion == 0 {
			config.Sidecar.ProtocolVersion = sidecar.VersionInteraction
		}
	}
	floor := strings.ToLower(strings.TrimSpace(options.sidecarFloor))
	switch name {
	case "omni":
		if owner := strings.TrimSpace(options.sidecarInteraction); owner != "" && owner != "engine" {
			return nil, errors.New("the omni preset selects engine interaction; use -binding sidecar for another composition")
		}
		if floor == "model" {
			return omni.NewWithModelFloor(config)
		}
		if floor != "" && floor != "engine" {
			return nil, fmt.Errorf("floor must be engine or model, got %q", floor)
		}
		return omni.New(config)
	case "omni+text-policy":
		if floor != "" && floor != "engine" {
			return nil, errors.New("omni+text-policy selects an engine floor; use -binding sidecar for another composition")
		}
		if owner := strings.TrimSpace(options.sidecarInteraction); owner != "" && owner != "engine" {
			return nil, errors.New("omni+text-policy selects engine interaction")
		}
		return omni.NewWithTextPolicy(config)
	case "duplex":
		if owner := strings.TrimSpace(options.sidecarInteraction); owner != "" && owner != "model" {
			return nil, errors.New("the duplex preset selects model interaction; use -binding sidecar for another composition")
		}
		if floor == "engine" {
			return duplex.NewWithEngineFloor(config)
		}
		if floor != "" && floor != "model" {
			return nil, fmt.Errorf("floor must be engine or model, got %q", floor)
		}
		return duplex.New(config)
	case "sidecar":
		capabilities, err := parseStackCapabilities(options.sidecarCapabilities)
		if err != nil {
			return nil, err
		}
		floorOwner, err := parseSidecarOwner("floor", floor, binding.OwnerEngine)
		if err != nil {
			return nil, err
		}
		interactionOwner, err := parseSidecarOwner(
			"interaction", strings.ToLower(strings.TrimSpace(options.sidecarInteraction)), binding.OwnerEngine)
		if err != nil {
			return nil, err
		}
		spec := sidecarbinding.Spec{
			Name: "sidecar",
			Ownership: binding.Ownership{
				Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
				SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
				Interaction: interactionOwner, Floor: floorOwner,
			},
			Capabilities: capabilities,
		}
		if config.Policies.Interaction != nil {
			config.Policies = sidecarbinding.ExternalInteractionPolicies(spec, config.Policies)
		}
		return sidecarbinding.New(spec, config)
	default:
		return nil, fmt.Errorf("unknown sidecar preset %q", name)
	}
}

func parseSidecarOwner(field, value string, fallback binding.Owner) (binding.Owner, error) {
	switch value {
	case "":
		return fallback, nil
	case "engine":
		return binding.OwnerEngine, nil
	case "model":
		return binding.OwnerModel, nil
	default:
		return "", fmt.Errorf("%s owner must be engine or model, got %q", field, value)
	}
}

func parseStackCapabilities(raw string) (binding.StackCapabilities, error) {
	var capabilities binding.StackCapabilities
	for _, item := range strings.Split(strings.ToLower(raw), ",") {
		name := strings.ReplaceAll(strings.TrimSpace(item), "_", "-")
		switch name {
		case "":
		case "audio-input":
			capabilities.AudioInput = true
		case "audio-output":
			capabilities.AudioOutput = true
		case "visual-input":
			capabilities.VisualInput = true
		case "transcription":
			capabilities.Transcription = true
		case "turn-generation":
			capabilities.TurnGeneration = true
		case "concurrent-io", "full-duplex":
			capabilities.ConcurrentIO = true
		case "native-floor":
			capabilities.NativeFloor = true
		case "native-interaction":
			capabilities.NativeInteraction = true
		case "interaction-acts":
			capabilities.InteractionActs = true
		case "text-injection":
			capabilities.TextInjection = true
		default:
			return binding.StackCapabilities{}, fmt.Errorf("unknown sidecar capability %q", item)
		}
	}
	return capabilities, nil
}

// buildComputerUse declares the action vocabulary against a real target.
//
// Blast radius is bounded by construction: the target is a browser context
// with a declared coordinate space, and an action naming anything else is
// refused before it reaches the browser. There is no ambient-desktop option,
// and that is not an omission.
// computerUseTools is the server-side computer-use surface plus the answer to
// its own confirmation requirement. The two are returned together because they
// are not separable: the namespace declares "policy" on every action that
// changes anything, and a namespace with no policy behind it is a namespace
// whose actions all deny.
type computerUseTools struct {
	specs  []action.ToolSpec
	policy action.PolicyDecision
}

func buildComputerUse(options serveOptions) (computerUseTools, error) {
	if !options.computerUse {
		return computerUseTools{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	surface, err := browser.Connect(ctx, browser.Config{
		DevToolsURL: options.browserURL, TargetURL: options.browserTarget,
	})
	if err != nil {
		return computerUseTools{}, fmt.Errorf("connect the computer-use target: %w", err)
	}
	width, height, err := surface.Viewport(ctx)
	if err != nil {
		return computerUseTools{}, fmt.Errorf("read the target viewport: %w", err)
	}
	target := computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: width, Height: height,
	}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target: target, Surface: surface,
		Audit: func(record computeruse.Record) {
			fmt.Fprintf(os.Stderr, "computer-use %s %s source=%s error=%q\n",
				record.Name, record.CallID, record.Source, record.Error)
		},
	})
	if err != nil {
		return computerUseTools{}, err
	}
	var overrides map[string]action.Confirm
	if declared := strings.TrimSpace(options.computerConfirm); declared != "" {
		confirm, err := action.ParseConfirm(declared)
		if err != nil {
			return computerUseTools{}, err
		}
		if confirm == action.ConfirmAlways {
			// Refusing here rather than at dispatch is the point. Nothing in
			// this binary can answer an "always" requirement, so accepting the
			// flag would bring up a server whose agent silently cannot press
			// anything - which looks like a broken model rather than a
			// configuration nobody could satisfy.
			return computerUseTools{}, errors.New(
				"-computer-confirm always needs a confirmer, and this server has none to offer: " +
					"every action would be denied at dispatch. Use policy, which admits actions inside the " +
					"declared target, or never, which admits them unconditionally")
		}
		overrides = make(map[string]action.Confirm, len(computeruse.Names()))
		for _, name := range computeruse.Names() {
			overrides[name] = confirm
		}
	}
	specs, err := computeruse.Specs(target, dispatcher, overrides)
	if err != nil {
		return computerUseTools{}, err
	}
	return computerUseTools{specs: specs, policy: computeruse.TargetPolicy(target)}, nil
}

// buildLogger configures structured logging.
//
// Text by default because a person reading a terminal is the common case, and
// JSON when something is going to collect it. Neither ever carries
// conversation content: a log that leaked what was said would be a worse
// problem than having no log.
func buildLogger(options serveOptions) (*slog.Logger, error) {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(options.logLevel)) {
	case "debug":
		level = slog.LevelDebug
	case "", "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("log level must be debug, info, warn, or error, got %q", options.logLevel)
	}
	handlerOptions := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(options.logFormat)) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, handlerOptions)), nil
	case "", "text":
		return slog.New(slog.NewTextHandler(os.Stderr, handlerOptions)), nil
	default:
		return nil, fmt.Errorf("log format must be text or json, got %q", options.logFormat)
	}
}

// splitList turns a comma-separated flag into a list, dropping empties so a
// trailing comma does not become an origin nobody meant to allow.
// parseICEServers reads "url|username|credential" entries. STUN needs no
// credential and TURN always does, so the two cannot share one flag shape.
func parseICEServers(value string) ([]webrtc.ICEServer, error) {
	var servers []webrtc.ICEServer
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.Split(entry, "|")
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
		}
		if fields[0] == "" {
			return nil, fmt.Errorf("ICE server %q has no URL", entry)
		}
		server := webrtc.ICEServer{URLs: []string{fields[0]}}
		switch len(fields) {
		case 1:
		case 3:
			if fields[1] == "" || fields[2] == "" {
				return nil, fmt.Errorf("ICE server %q needs both a username and a credential", entry)
			}
			server.Username = fields[1]
			server.Credential = fields[2]
			server.CredentialType = webrtc.ICECredentialTypePassword
		default:
			return nil, fmt.Errorf(
				"ICE server %q must be url or url|username|credential", entry,
			)
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// webrtcClientCredential resolves the credential a caller must present, and
// refuses a routable listener that has none. The adapter opens sessions with
// the deployment's own upstream token, so an unauthenticated routable
// listener hands that spend to anyone who can reach the port.
func webrtcClientCredential(options serveOptions) (string, error) {
	credential := ""
	if name := strings.TrimSpace(options.webrtcTokenEnv); name != "" {
		credential = strings.TrimSpace(os.Getenv(name))
		if credential == "" {
			return "", fmt.Errorf(
				"-webrtc-token-env names %q, which is empty or unset", name,
			)
		}
	}
	if credential != "" {
		return credential, nil
	}
	// No listener means no port anyone can reach: the adapter is being built
	// for in-process use, and there is nothing to protect.
	if strings.TrimSpace(options.webrtcListen) == "" {
		return "", nil
	}
	host, _, err := net.SplitHostPort(options.webrtcListen)
	if err != nil {
		return "", fmt.Errorf("WebRTC listen address %q is not host:port", options.webrtcListen)
	}
	if host == "localhost" {
		return "", nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", nil
	}
	return "", fmt.Errorf(
		"WebRTC listen address %q is routable and has no credential: set -webrtc-token-env, "+
			"because this endpoint opens sessions with the deployment's own upstream token",
		options.webrtcListen,
	)
}

// webrtcUpstreamAuthority turns a wildcard bind into an address the adapter
// can actually dial. Binding 0.0.0.0 states where the server listens, not a
// host anything connects to.
func webrtcUpstreamAuthority(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return net.JoinHostPort("127.0.0.1", port)
		}
		return net.JoinHostPort("::1", port)
	}
	if host == "" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}

func splitList(value string) []string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
