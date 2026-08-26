package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/adapters/gemini"
	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	"github.com/bojieli/OpenRealtime/admission"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
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
	browserdemo "github.com/bojieli/OpenRealtime/examples/browser"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/policymodel"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
	webrtcadapter "github.com/bojieli/OpenRealtime/transport/webrtc"
	"github.com/pion/webrtc/v4"
)

type serveOptions struct {
	listen   string
	binding  string
	profile  string
	model    string
	tokenEnv string

	requestTimeout  time.Duration
	shutdownTimeout time.Duration

	asrProvider string
	asrURL      string
	asrModel    string
	language    string
	asrCadence  time.Duration
	asrPartial  time.Duration

	fastProvider string
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
	demo            bool

	logFormat string
	logLevel  string

	webrtcListen string
	webrtcSTUN   string
	webrtcOrigin string

	gpuCapacity         int
	policyURL           string
	policyModel         string
	policyTokenEnv      string
	policyGuided        bool
	policies            string
	interactionShadow   string
	interactionFloor    bool
	interactionSees     bool
	profileTurns        bool
	speakBySentence     bool
	interactionLiveness time.Duration
	policyReasoning     string
	projectionHold      time.Duration
	holdingAfter        time.Duration
	bargeIn             string
	bargeInHold         time.Duration

	computerUse     bool
	fastComputerUse bool
	browserURL      string
	browserTarget   string
	computerConfirm string

	sidecarCommand string
	sidecarAddress string
	sidecarFloor   string
	sidecarVoice   string

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
	flags.StringVar(&options.binding, "binding", "cascade", "voice stack: cascade, upstream, omni, or duplex")
	flags.StringVar(&options.profile, "profile", "voice", "runtime profile: voice or voice+vision")
	flags.StringVar(&options.model, "model", "openrealtime", "compatibility model identifier reported to clients")
	flags.StringVar(&options.tokenEnv, "token-env", "OPENREALTIME_TOKEN", "environment variable holding the bearer token; empty disables authentication")
	flags.DurationVar(&options.requestTimeout, "request-timeout", 2*time.Minute, "per-request provider timeout")
	flags.DurationVar(&options.shutdownTimeout, "shutdown-timeout", 15*time.Second, "graceful shutdown timeout")

	flags.StringVar(&options.asrProvider, "asr-provider", "qwen-asr",
		"recogniser provider; openrealtime providers lists them")
	flags.StringVar(&options.asrURL, "asr-url", qwenasr.DefaultBaseURL,
		"recogniser endpoint; unset selects the provider's own")
	flags.StringVar(&options.asrModel, "asr-model", qwenasr.DefaultModel,
		"recogniser model identity; unset selects the provider's default")
	flags.StringVar(&options.language, "language", "",
		"language hint for the recogniser and the synthesiser; empty lets each decide")
	flags.DurationVar(&options.asrCadence, "asr-cadence", 200*time.Millisecond, "how often the recogniser is advanced")
	flags.DurationVar(&options.asrPartial, "asr-partial-interval", 0,
		"ask a batch recogniser for a hypothesis this often by re-transcribing the utterance; "+
			"0 recognises only at the endpoint, and a streaming recogniser ignores it")

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
	flags.IntVar(&options.fastTokens, "fast-max-tokens", 96, "fast spoken turn output-token limit")
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
	flags.IntVar(&options.reflexTokens, "visual-reflex-max-tokens", 48,
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
	flags.BoolVar(&options.demo, "demo", false, "serve the browser demo at /demo")
	flags.StringVar(&options.logFormat, "log-format", "text", "structured log format: text or json")
	flags.StringVar(&options.logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	flags.StringVar(&options.webrtcListen, "webrtc-listen", "", "additional WebRTC listen address; empty disables the adapter")
	flags.StringVar(&options.webrtcSTUN, "webrtc-stun", "", "comma-separated STUN servers for the WebRTC adapter")
	flags.StringVar(&options.webrtcOrigin, "webrtc-allow-origin", "",
		"comma-separated web origins allowed to POST an SDP offer, or \"*\"; empty allows none, which is right unless a browser on another origin has to reach the adapter")
	flags.BoolVar(&options.computerUse, "computer-use", false, "declare the computer.* tools against a browser target")
	flags.BoolVar(&options.fastComputerUse, "fast-computer-use", false,
		"let the fast provider execute only bounded standard computer.* actions")
	flags.StringVar(&options.browserURL, "browser-devtools-url", "http://127.0.0.1:9222", "browser DevTools endpoint for computer use")
	flags.StringVar(&options.browserTarget, "browser-target", "", "connect directly to a known page WebSocket instead of discovering one")
	flags.StringVar(&options.computerConfirm, "computer-confirm", "", "override every computer.* confirmation requirement: never, policy, or always")
	flags.StringVar(&options.policies, "policy-models", "none", "comma-separated policy models: backchannel, turn-projection, overlap, interaction, all, or none")
	flags.StringVar(&options.interactionShadow, "interaction-shadow", "", "file to record shadow interaction decisions to; enabling it decides nothing")
	flags.BoolVar(&options.interactionFloor, "interaction-floor", false, "let the interaction model own turn-taking instead of the silence rule and the projection")
	flags.BoolVar(&options.interactionSees, "interaction-sees", false, "the interaction model can look at a frame, so pictures reach it directly instead of as a narration")
	flags.BoolVar(&options.profileTurns, "profile-turns", false, "log how long each stage of a turn took, one line per turn")
	flags.BoolVar(&options.speakBySentence, "speak-by-sentence", false, "synthesise the first sentence on its own, so speech starts before the rest is ready")
	flags.DurationVar(&options.interactionLiveness, "interaction-liveness", 20*time.Second, "longest the interaction model may hold the floor past the silence threshold")
	flags.StringVar(&options.policyReasoning, "policy-reasoning", "chat_template_kwargs",
		"how the policy endpoint is told not to think: chat_template_kwargs, enable_thinking, reasoning_effort, thinking_object, or none for an instruct model")
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
	flags.StringVar(&options.sidecarCommand, "sidecar", "", "command that runs the model sidecar for the omni and duplex bindings")
	flags.StringVar(&options.sidecarAddress, "sidecar-address", "", "connect to a running sidecar as tcp:host:port or unix:/path")
	flags.DurationVar(&options.clientToolTimeout, "client-tool-timeout", clientcalls.DefaultTimeout,
		"how long a client has to return a result for a tool it executes; a negative value waits forever")
	flags.StringVar(&options.sidecarFloor, "floor", "", "who decides endpoints: engine or model; empty selects the binding's default")
	flags.StringVar(&options.sidecarVoice, "sidecar-voice", "",
		"voice for a model that has more than one; empty leaves the choice to the model")
	flags.SetOutput(output)
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

func serve(options serveOptions, output io.Writer) error {
	if strings.TrimSpace(options.listen) == "" {
		return errors.New("a listen address is required")
	}
	logger, err := buildLogger(options)
	if err != nil {
		return err
	}
	bind, recogniser, err := buildBinding(options)
	if err != nil {
		return err
	}
	server, err := gateway.New(gateway.Config{
		Binding: bind, Token: os.Getenv(options.tokenEnv), Model: options.model,
		TranscriptionModel: options.asrModel, ValidateWire: options.validateWire,
		Logger: logger, Demo: demoHandler(options.demo),
		Recogniser: recogniserReport(recogniser),
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

// buildBinding creates the binding and, when that binding owns perception,
// the accumulator its recognisers fold into. Every other binding returns a nil
// accumulator: there is no recogniser in the process to report on.
func buildBinding(options serveOptions) (binding.Binding, *asrbuffer.Accumulator, error) {
	var err error
	options, err = normalizeProfile(options)
	if err != nil {
		return nil, nil, err
	}
	bindingName := strings.ToLower(strings.TrimSpace(options.binding))
	if bindingName == "" {
		bindingName = "cascade"
	}
	if options.fastComputerUse && bindingName != "cascade" {
		return nil, nil, fmt.Errorf(
			"fast computer use is implemented by the cascade binding, not %q", bindingName)
	}
	governor, err := buildGovernor(options)
	if err != nil {
		return nil, nil, err
	}
	policies, err := buildPolicies(options, governor)
	if err != nil {
		return nil, nil, err
	}
	switch bindingName {
	case "cascade":
		recogniser := asrbuffer.NewAccumulator()
		bind, err := buildCascade(options, policies, governor, recogniser)
		if err != nil {
			return nil, nil, err
		}
		return bind, recogniser, nil
	case "upstream":
		bind, err := buildUpstream(options)
		return bind, nil, err
	case "omni":
		bind, err := buildSidecarBinding(options, "omni")
		return bind, nil, err
	case "duplex":
		bind, err := buildSidecarBinding(options, "duplex")
		return bind, nil, err
	default:
		return nil, nil, fmt.Errorf("binding must be cascade, upstream, omni, or duplex, got %q", options.binding)
	}
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
	if !backchannel && !projection && !overlap && !wholeDecision {
		return nil
	}
	if strings.TrimSpace(options.policyModel) == "" {
		return errors.New("enabling a policy model needs -policy-model")
	}
	decider, err := policymodel.New(policymodel.Config{
		BaseURL: options.policyURL, Model: options.policyModel,
		APIKey: os.Getenv(options.policyTokenEnv), GuidedChoice: options.policyGuided,
		Reasoning: openaicompat.ReasoningControl(options.policyReasoning),
		// Interactive: above speculative preparation, below the foreground
		// continuation. A backchannel that arrives after the moment for it has
		// passed is worse than no backchannel.
		Governor: governor, Class: admission.ClassInteractive,
	})
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
	if wholeDecision {
		// Extraction shares the endpoint but not the call shape: it needs a
		// policy back, which no enumeration can contain.
		extractor, err := interaction.NewExtractor(decider)
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
		floor, err := interaction.NewActFloor(policies.Interaction, interaction.ActFloorOptions{
			Liveness: options.interactionLiveness,
		})
		if err != nil {
			return err
		}
		policies.Floor = floor
	}
	return nil
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
	return cascade.New(cascade.Config{
		Profile:           options.profile,
		ClientToolTimeout: options.clientToolTimeout,
		Observers:         observers, DefaultObservers: defaults, Tools: computer.specs,
		Narrator:        narrator,
		DeciderSees:     options.interactionSees,
		ProfileTurns:    options.profileTurns,
		FastComputerUse: options.fastComputerUse,
		Governor:        governor,
		ConfirmPolicy:   computer.policy,
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
		ASRCadence: options.asrCadence, HoldingAfter: options.holdingAfter,
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
	if options.fastComputerUse {
		authority = continuation.ToolAuthorityExecute
	}
	return providers.NewLLM(providers.LLMRequest{
		Provider: options.fastProvider,
		Model:    modelOverride(options, "fast-model", options.fastModel, options.fastProvider, trajectory.PhaseFast),
		BaseURL:  options.override("fast-url", options.fastURL),
		APIKey:   roleCredential(options, "fast-token-env", options.fastTokenEnv, "OPENREALTIME_FAST_API_KEY"),
		Phase:    trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority:   authority,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
		Reason:          providers.ReasonOff,
		Vision:          visionOverride(options, "fast-sees", options.fastVision),
		RequestTimeout:  options.requestTimeout,
	})
}

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
		Language: options.language, PartialInterval: options.asrPartial,
		RequestTimeout: recogniserTimeout(options.asrCadence, options.requestTimeout),
	})
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
		Endpoint:       "ws://" + options.listen + "/v1/realtime",
		Token:          os.Getenv(options.tokenEnv),
		Model:          options.model,
		ICEServers:     iceServers,
		AllowedOrigins: splitList(options.webrtcOrigin),
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

// buildSidecarBinding configures a model that lives behind a process boundary.
//
// The floor flag is the F5 comparison made available on the command line: an
// Omni model defaults to the engine's floor because voice activity detection
// mis-endpoints on spelled identifiers, and a duplex model defaults to its own
// because turn-taking is in its weights. Either can be flipped, which is what
// makes the claim measurable rather than asserted.
func buildSidecarBinding(options serveOptions, name string) (binding.Binding, error) {
	if strings.TrimSpace(options.sidecarCommand) == "" && strings.TrimSpace(options.sidecarAddress) == "" {
		return nil, fmt.Errorf("the %s binding needs -sidecar or -sidecar-address", name)
	}
	slow, err := buildSlow(options)
	if err != nil {
		return nil, fmt.Errorf("configure the slow provider: %w", err)
	}
	config := sidecarbinding.Config{
		Sidecar: sidecar.Config{
			Command: strings.Fields(options.sidecarCommand),
			Address: options.sidecarAddress,
			Logf: func(format string, values ...any) {
				fmt.Fprintf(os.Stderr, format+"\n", values...)
			},
		},
		Instructions: options.instruction, Slow: slow, SlowMaxTokens: options.slowTokens,
		Voice:             options.sidecarVoice,
		ClientToolTimeout: options.clientToolTimeout,
	}
	floor := strings.ToLower(strings.TrimSpace(options.sidecarFloor))
	switch name {
	case "omni":
		if floor == "model" {
			return omni.NewWithModelFloor(config)
		}
		return omni.New(config)
	default:
		if floor == "engine" {
			return duplex.NewWithEngineFloor(config)
		}
		return duplex.New(config)
	}
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

// demoHandler returns the browser demo when an operator asked for it.
//
// Off by default: a realtime server's job is one protocol on one port, and a
// page that appears on every deployment is surface nobody asked for.
func demoHandler(enabled bool) http.Handler {
	if !enabled {
		return nil
	}
	return browserdemo.Handler()
}

// splitList turns a comma-separated flag into a list, dropping empties so a
// trailing comma does not become an origin nobody meant to allow.
func splitList(value string) []string {
	var result []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
