package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/bysentence"
	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/bench"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/interaction"
	meetinggraph "github.com/bojieli/OpenRealtime/meeting/graphnative"
	"github.com/bojieli/OpenRealtime/perception"
	openrealtime "github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	meetingLocalModelProvider = "vllm"
	meetingLocalModelName     = "qwen-fast"
	meetingLocalModelURL      = "http://127.0.0.1:8000/v1"
	meetingLocalASRProvider   = "sensevoice"
	meetingLocalASRModel      = "iic/SenseVoiceSmall"
	meetingLocalASRURL        = "http://127.0.0.1:8002/v1"
	meetingLocalTTSProvider   = "fish-audio"
	meetingLocalTTSModel      = "fishaudio/fish-speech-1.5"
	meetingLocalTTSURL        = "http://127.0.0.1:8123/v1/tts"

	meetingBackgroundProvider = "google"
	meetingBackgroundModel    = "gemini-3.7-flash"
	meetingBackgroundURL      = "https://generativelanguage.googleapis.com/v1beta"

	meetingForegroundBindingName = "openrealtime.meeting-assistant.local-foreground"
	meetingVisualProviderName    = "openrealtime.meeting-assistant.local-vision"

	meetingApplicationArtifactID = "go://github.com/bojieli/OpenRealtime/graphs/meeting-assistant/application/v1"
	meetingProviderArtifactID    = "go://github.com/bojieli/OpenRealtime/meeting/graphnative/session-provider/v1"
	meetingAdapterArtifactID     = "go://github.com/bojieli/OpenRealtime/meeting/graphnative/session-adapter/v1"
	meetingRuntimeArtifactID     = "go://github.com/bojieli/OpenRealtime/meeting/graphnative/foreground-runtime/v1"
	meetingWireArtifactID        = "go://github.com/bojieli/OpenRealtime/meeting/graphnative/foreground-wire/v1"
)

type meetingDeploymentIdentities struct {
	Model      inspect.ArtifactIdentity `json:"model"`
	ASR        inspect.ArtifactIdentity `json:"asr"`
	TTS        inspect.ArtifactIdentity `json:"tts"`
	Vision     inspect.ArtifactIdentity `json:"vision"`
	Background inspect.ArtifactIdentity `json:"background"`
}

// meetingDeploymentVerifier is the production proof seam for the provider
// plug-ins selected by the Meeting application. Resolve derives identities
// from live backends; Verify binds those opaque proofs to profile freeze,
// readiness, and session resource creation. Caller-authored identity strings
// are never treated as deployment evidence.
type meetingDeploymentVerifier interface {
	Resolve(context.Context) (meetingDeploymentIdentities, error)
	Verify(context.Context, meetingDeploymentIdentities) error
}

type meetingDeploymentComponentVerifier interface {
	VerifyForeground(context.Context, meetingDeploymentIdentities) error
	VerifyVision(context.Context, meetingDeploymentIdentities) error
	VerifyBackground(context.Context, meetingDeploymentIdentities) error
}

func verifyMeetingForegroundDeployments(
	ctx context.Context, verifier meetingDeploymentVerifier, expected meetingDeploymentIdentities,
) error {
	if components, ok := verifier.(meetingDeploymentComponentVerifier); ok {
		return components.VerifyForeground(ctx, expected)
	}
	return verifier.Verify(ctx, expected)
}

func verifyMeetingVisionDeployment(
	ctx context.Context, verifier meetingDeploymentVerifier, expected meetingDeploymentIdentities,
) error {
	if components, ok := verifier.(meetingDeploymentComponentVerifier); ok {
		return components.VerifyVision(ctx, expected)
	}
	return verifier.Verify(ctx, expected)
}

func verifyMeetingBackgroundSelection(
	ctx context.Context, verifier meetingDeploymentVerifier, expected meetingDeploymentIdentities,
) error {
	if components, ok := verifier.(meetingDeploymentComponentVerifier); ok {
		return components.VerifyBackground(ctx, expected)
	}
	return verifier.Verify(ctx, expected)
}

func nilMeetingDeploymentInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (identities meetingDeploymentIdentities) validate() error {
	for _, deployment := range []struct {
		name     string
		identity inspect.ArtifactIdentity
	}{
		{name: "model", identity: identities.Model},
		{name: "ASR", identity: identities.ASR},
		{name: "TTS", identity: identities.TTS},
		{name: "vision", identity: identities.Vision},
		{name: "background", identity: identities.Background},
	} {
		if err := deployment.identity.Validate(); err != nil ||
			deployment.identity.Revision == "" || deployment.identity.Digest == "" {
			return fmt.Errorf(
				"Meeting Assistant %s deployment requires an exact ID, revision, and digest",
				deployment.name,
			)
		}
	}
	return nil
}

type meetingLocalConfiguration struct {
	FormatVersion uint64                       `json:"format_version"`
	Executable    inspect.ArtifactIdentity     `json:"executable"`
	Foreground    meetingLocalForegroundConfig `json:"foreground"`
	Background    meetingLocalBackgroundConfig `json:"background"`
}

type meetingLocalForegroundConfig struct {
	ModelProvider string `json:"model_provider"`
	Model         string `json:"model"`
	ModelURL      string `json:"model_url"`
	ASRProvider   string `json:"asr_provider"`
	ASRModel      string `json:"asr_model"`
	ASRURL        string `json:"asr_url"`
	TTSProvider   string `json:"tts_provider"`
	TTSModel      string `json:"tts_model"`
	TTSURL        string `json:"tts_url"`
	TTSVoice      string `json:"tts_voice,omitempty"`
	VisionModel   string `json:"vision_model"`
	VisionURL     string `json:"vision_url"`

	MaxOutputTokens       int   `json:"max_output_tokens"`
	VisualReflexMaxTokens int   `json:"visual_reflex_max_output_tokens"`
	VisualReflexTimeoutMS int64 `json:"visual_reflex_timeout_ms"`
	ASRCadenceMS          int64 `json:"asr_cadence_ms"`
	FrameRateMilliHz      int   `json:"frame_rate_millihz"`
	RequestTimeoutMS      int64 `json:"request_timeout_ms"`
	SentenceMinRunes      int   `json:"sentence_min_runes"`
	AttachKeyframes       bool  `json:"attach_keyframes"`
	ExternalVideoGate     bool  `json:"external_video_gate"`

	ModelDeployment  inspect.ArtifactIdentity `json:"model_deployment"`
	ASRDeployment    inspect.ArtifactIdentity `json:"asr_deployment"`
	TTSDeployment    inspect.ArtifactIdentity `json:"tts_deployment"`
	VisionDeployment inspect.ArtifactIdentity `json:"vision_deployment"`
}

type meetingLocalBackgroundConfig struct {
	Provider         string                   `json:"provider"`
	Model            string                   `json:"model"`
	BaseURL          string                   `json:"base_url"`
	Effort           string                   `json:"effort"`
	RetainReasoning  bool                     `json:"retain_reasoning"`
	RequestTimeoutMS int64                    `json:"request_timeout_ms"`
	Deployment       inspect.ArtifactIdentity `json:"deployment"`
}

func defaultMeetingLocalConfiguration(
	executable inspect.ArtifactIdentity, deployments meetingDeploymentIdentities,
) meetingLocalConfiguration {
	return meetingLocalConfiguration{
		FormatVersion: 2,
		Executable:    executable,
		Foreground: meetingLocalForegroundConfig{
			ModelProvider: meetingLocalModelProvider, Model: meetingLocalModelName,
			ModelURL:    meetingLocalModelURL,
			ASRProvider: meetingLocalASRProvider, ASRModel: meetingLocalASRModel,
			ASRURL:      meetingLocalASRURL,
			TTSProvider: meetingLocalTTSProvider, TTSModel: meetingLocalTTSModel,
			TTSURL: meetingLocalTTSURL, TTSVoice: "default",
			VisionModel: meetingLocalModelName, VisionURL: meetingLocalModelURL,
			MaxOutputTokens: 512, VisualReflexMaxTokens: 96, VisualReflexTimeoutMS: 2_000,
			ASRCadenceMS: 200, FrameRateMilliHz: 5_000,
			RequestTimeoutMS: 30_000, SentenceMinRunes: 12,
			AttachKeyframes: true, ExternalVideoGate: true,
			ModelDeployment: deployments.Model, ASRDeployment: deployments.ASR,
			TTSDeployment: deployments.TTS, VisionDeployment: deployments.Vision,
		},
		Background: meetingLocalBackgroundConfig{
			Provider: meetingBackgroundProvider, Model: meetingBackgroundModel,
			BaseURL: meetingBackgroundURL, Effort: string(continuation.EffortMinimal),
			RetainReasoning: true, RequestTimeoutMS: 60_000,
			Deployment: deployments.Background,
		},
	}
}

type serveMeetingRegistration struct {
	Application   launchprofile.Registration
	Adapter       graphlaunch.AdapterSelection
	Provider      inspect.ArtifactIdentity
	Configuration meetingLocalConfiguration
}

func newServeMeetingRegistration(
	ctx context.Context, executable inspect.ArtifactIdentity, deployments meetingDeploymentIdentities,
	verifier meetingDeploymentVerifier,
) (serveMeetingRegistration, error) {
	if ctx == nil || nilMeetingDeploymentInterface(verifier) {
		return serveMeetingRegistration{}, errors.New(
			"Meeting Assistant registration requires a deployment verifier",
		)
	}
	if cause := context.Cause(ctx); cause != nil {
		return serveMeetingRegistration{}, cause
	}
	if err := executable.Validate(); err != nil {
		return serveMeetingRegistration{}, fmt.Errorf("Meeting Assistant profile executable: %w", err)
	}
	if err := deployments.validate(); err != nil {
		return serveMeetingRegistration{}, err
	}
	if err := verifier.Verify(ctx, deployments); err != nil {
		return serveMeetingRegistration{}, fmt.Errorf(
			"verify Meeting Assistant registration deployments: %w", err,
		)
	}
	configuration := defaultMeetingLocalConfiguration(executable, deployments)
	linked := func(id string) (inspect.ArtifactIdentity, error) {
		artifact := inspect.ArtifactIdentity{ID: id, Digest: executable.Digest}
		if err := artifact.Validate(); err != nil {
			return inspect.ArtifactIdentity{}, err
		}
		return artifact, nil
	}
	applicationArtifact, err := linked(meetingApplicationArtifactID)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	providerArtifact, err := linked(meetingProviderArtifactID)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	adapterArtifact, err := linked(meetingAdapterArtifactID)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	runtimeArtifact, err := linked(meetingRuntimeArtifactID)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	wireArtifact, err := linked(meetingWireArtifactID)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	foregroundArtifact, err := meetingConfigurationArtifact(
		"profile://openrealtime/meeting-assistant/local-foreground-composition/v1",
		configuration.Foreground, executable,
	)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	visualArtifact, err := meetingConfigurationArtifact(
		"profile://openrealtime/meeting-assistant/local-visual-composition/v1",
		struct {
			Model      string                   `json:"model"`
			URL        string                   `json:"url"`
			Deployment inspect.ArtifactIdentity `json:"deployment"`
		}{
			Model:      configuration.Foreground.VisionModel,
			URL:        configuration.Foreground.VisionURL,
			Deployment: configuration.Foreground.VisionDeployment,
		},
		executable,
	)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	backgroundArtifact, err := meetingConfigurationArtifact(
		"profile://openrealtime/meeting-assistant/gemini-background-composition/v1",
		configuration.Background, executable,
	)
	if err != nil {
		return serveMeetingRegistration{}, err
	}

	foregroundDescriptor, err := providers.DescribeLLM(meetingForegroundLLMRequest(configuration.Foreground))
	if err != nil {
		return serveMeetingRegistration{}, fmt.Errorf("describe Meeting foreground model: %w", err)
	}
	backgroundDescriptor, err := providers.DescribeLLM(meetingBackgroundLLMRequest(configuration.Background))
	if err != nil {
		return serveMeetingRegistration{}, fmt.Errorf("describe Meeting background model: %w", err)
	}
	visualDescriptor := perceptionelements.VisualProviderDescriptor{
		Name: meetingVisualProviderName, Revision: deployments.Vision.Revision,
		Digest: deployments.Vision.Digest,
	}
	foregroundCapabilities := meetingForegroundCapabilities(configuration.Foreground)
	foregroundFactory := func(ctx context.Context, options legacy.Options) (legacy.Binding, error) {
		if err := verifyMeetingForegroundDeployments(ctx, verifier, deployments); err != nil {
			return nil, fmt.Errorf(
				"verify Meeting Assistant foreground deployments at session open: %w", err,
			)
		}
		return newMeetingForegroundBinding(ctx, configuration.Foreground, options)
	}
	visualFactory := func(
		ctx context.Context, _ legacy.Options,
	) (perceptionelements.VisualProvider, error) {
		if err := verifyMeetingVisionDeployment(ctx, verifier, deployments); err != nil {
			return nil, fmt.Errorf(
				"verify Meeting Assistant visual deployment at session open: %w", err,
			)
		}
		return newMeetingVisualProvider(ctx, configuration.Foreground, visualDescriptor)
	}
	backgroundFactory := func(
		ctx context.Context, _ legacy.Options,
	) (continuation.Provider, error) {
		if err := profileProviderContext(ctx); err != nil {
			return nil, err
		}
		if err := verifyMeetingBackgroundSelection(ctx, verifier, deployments); err != nil {
			return nil, fmt.Errorf(
				"verify Meeting Assistant background selection at session open: %w", err,
			)
		}
		request := meetingBackgroundLLMRequest(configuration.Background)
		return providers.NewLLM(request)
	}
	readiness := []graphlaunch.ReadinessCheck{
		{
			Name: "meeting.foreground",
			Check: func(ctx context.Context) error {
				binding, err := foregroundFactory(ctx, legacy.Options{})
				if err != nil {
					return err
				}
				return closeReadinessResource(binding)
			},
		},
		{
			Name: "meeting.visual",
			Check: func(ctx context.Context) error {
				provider, err := visualFactory(ctx, legacy.Options{})
				if err != nil {
					return err
				}
				return closeReadinessResource(provider)
			},
		},
		{
			Name: "meeting.background.gemini-3.7-flash",
			Check: func(ctx context.Context) error {
				provider, err := backgroundFactory(ctx, legacy.Options{})
				if err != nil {
					return err
				}
				return closeReadinessResource(provider)
			},
		},
	}
	registration, err := graphs.MeetingAssistantApplicationRegistration(
		graphs.MeetingAssistantRegistrationConfig{
			ApplicationArtifact: applicationArtifact, ProviderArtifact: providerArtifact,
			Session: meetinggraph.SessionPluginConfig{
				AdapterArtifact: adapterArtifact,
				Adapter: meetinggraph.SessionAdapterConfig{
					FrameRateMilliHz: configuration.Foreground.FrameRateMilliHz,
					Status: legacy.Status{
						Fast:           foregroundDescriptor.Provider + "/" + foregroundDescriptor.Model,
						Slow:           backgroundDescriptor.Provider + "/" + backgroundDescriptor.Model,
						Perception:     meetingLocalASRProvider + "/" + meetingLocalASRModel,
						VisualNarrator: meetingVisualProviderName,
						Speech:         meetingLocalTTSProvider + "/" + meetingLocalTTSModel,
					},
				},
				Foreground: meetinggraph.ForegroundPlugin{
					Artifact: foregroundArtifact, ProviderArtifact: foregroundArtifact,
					RuntimeArtifact: runtimeArtifact, WireAdapterArtifact: wireArtifact,
					BindingName:  meetingForegroundBindingName,
					Ownership:    meetingForegroundOwnership(),
					Capabilities: foregroundCapabilities,
					Descriptor:   foregroundDescriptor,
					Factory:      foregroundFactory,
				},
				Visual: meetinggraph.VisualPlugin{
					Artifact: visualArtifact, Descriptor: visualDescriptor, Factory: visualFactory,
				},
				Background: meetinggraph.BackgroundPlugin{
					Artifact: backgroundArtifact, Descriptor: backgroundDescriptor,
					Factory: backgroundFactory,
				},
			},
			Readiness: readiness,
		},
	)
	if err != nil {
		return serveMeetingRegistration{}, err
	}
	return serveMeetingRegistration{
		Application: registration.Application, Adapter: registration.Adapter,
		Provider: providerArtifact, Configuration: configuration,
	}, nil
}

func meetingConfigurationArtifact(
	id string, config any, executable inspect.ArtifactIdentity,
) (inspect.ArtifactIdentity, error) {
	payload, err := json.Marshal(struct {
		Executable inspect.ArtifactIdentity `json:"executable"`
		Config     any                      `json:"config"`
	}{Executable: executable, Config: config})
	if err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	digest := sha256.Sum256(payload)
	artifact := inspect.ArtifactIdentity{
		ID: id, Revision: "v1", Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
	if err := artifact.Validate(); err != nil {
		return inspect.ArtifactIdentity{}, err
	}
	return artifact, nil
}

func meetingForegroundLLMRequest(config meetingLocalForegroundConfig) providers.LLMRequest {
	vision := true
	temperature := 0.0
	return providers.LLMRequest{
		Provider: config.ModelProvider, Model: config.Model, BaseURL: config.ModelURL,
		APIKey: os.Getenv("OPENREALTIME_LOCAL_API_KEY"), Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthorityVoice, Reason: providers.ReasonOff,
		Vision: &vision, RetainReasoning: false, Temperature: &temperature,
		RequestTimeout: time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}
}

// meetingVisualReflexLLMRequest gives the existing local multimodal plug-in a
// separate, tightly bounded action role. It is not a second voice and cannot
// invent a server-side action surface: cascade exposes only caller-declared,
// target-bound computer actions to this silent provider.
func meetingVisualReflexLLMRequest(config meetingLocalForegroundConfig) providers.LLMRequest {
	vision := true
	temperature := 0.0
	return providers.LLMRequest{
		Provider: config.ModelProvider, Model: config.Model, BaseURL: config.ModelURL,
		APIKey: os.Getenv("OPENREALTIME_LOCAL_API_KEY"), Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent, Reason: providers.ReasonOff,
		Vision: &vision, RetainReasoning: false, Temperature: &temperature,
		RequestTimeout: time.Duration(config.VisualReflexTimeoutMS) * time.Millisecond,
	}
}

func meetingBackgroundLLMRequest(config meetingLocalBackgroundConfig) providers.LLMRequest {
	vision := true
	return providers.LLMRequest{
		Provider: config.Provider, Model: config.Model, BaseURL: config.BaseURL,
		Phase: trajectory.PhaseSlow, Effort: continuation.Effort(config.Effort),
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
		Reason:          providers.ReasonOn, Vision: &vision,
		RetainReasoning: config.RetainReasoning,
		RequestTimeout:  time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}
}

func meetingASRRequest(config meetingLocalForegroundConfig) providers.ASRRequest {
	return providers.ASRRequest{
		Provider: config.ASRProvider, Model: config.ASRModel, BaseURL: config.ASRURL,
		APIKey:          os.Getenv("OPENREALTIME_ASR_API_KEY"),
		PartialInterval: time.Duration(config.ASRCadenceMS) * time.Millisecond,
		RequestTimeout:  time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}
}

func meetingTTSRequest(config meetingLocalForegroundConfig) providers.TTSRequest {
	return providers.TTSRequest{
		Provider: config.TTSProvider, Model: config.TTSModel, BaseURL: config.TTSURL,
		Voice: config.TTSVoice, APIKey: os.Getenv("OPENREALTIME_TTS_API_KEY"),
		OutputSampleRateHz: 24_000,
		RequestTimeout:     time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}
}

func meetingForegroundOwnership() legacy.Ownership {
	return legacy.Ownership{
		Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
		SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
		Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
	}
}

func meetingForegroundCapabilities(config meetingLocalForegroundConfig) legacy.Capabilities {
	return legacy.Capabilities{
		Video: true, ComputerUse: true, Observations: true, FastSlow: false,
		Observers: []string{"audio", "screen"}, ManualTurns: true,
		MaxOutputTokens: config.MaxOutputTokens,
		Stack: legacy.StackCapabilities{
			AudioInput: true, AudioOutput: true, VisualInput: true,
			Transcription: true, TurnGeneration: true, ConcurrentIO: true,
			TextInjection: true,
		},
	}
}

type meetingDormantProvider struct {
	descriptor continuation.Descriptor
}

// meetingForegroundRollout is the profile's narrow local control policy. It
// never schedules the private cascade slow slot (the graph owns background
// cognition), but unlike the generic fast-only control condition it gives a
// successful tool result one fast turn in which to continue an ordered action
// chain or speak the grounded result.
type meetingForegroundRollout struct{}

func (meetingForegroundRollout) Name() string { return "meeting-fast-tool-continuations" }

func (meetingForegroundRollout) Plan(input interaction.RolloutInput) []interaction.Step {
	if input.Cause.ToolError {
		return []interaction.Step{{Kind: interaction.StepFast, Reason: interaction.ReasonToolFailure}}
	}
	if input.Cause.Observation || input.Cause.CompositeResume || input.Cause.ToolResult {
		reason := "answer now"
		switch {
		case input.Cause.CompositeResume:
			reason = interaction.ReasonCompositeResume
		case input.Cause.ToolResult:
			reason = "continue after foreground tool result"
		}
		return []interaction.Step{{Kind: interaction.StepFast, Reason: reason}}
	}
	return nil
}

func meetingDormantDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "openrealtime-graph", Model: "background-owned-by-meeting-graph",
		Phase: trajectory.PhaseSlow, Effort: continuation.EffortMinimal, Streaming: true,
		// Cascade validates its dormant slot against the live client tool
		// catalog before the fast-only rollout is selected. Execution authority
		// is required for that slot, while Continue still fails closed if policy
		// drift ever schedules it: the graph's independent background node is
		// the only real slow provider.
		ToolAuthority:   continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func (provider meetingDormantProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (meetingDormantProvider) Continue(
	context.Context, continuation.Request, continuation.Emit,
) (continuation.Completion, error) {
	return continuation.Completion{},
		errors.New("Meeting foreground slow lane is disabled; the graph owns background cognition")
}

type meetingForegroundBinding struct {
	inner        legacy.Binding
	capabilities legacy.Capabilities
	policies     interaction.Policies
}

func newMeetingForegroundBinding(
	ctx context.Context, config meetingLocalForegroundConfig, _ legacy.Options,
) (legacy.Binding, error) {
	if err := profileProviderContext(ctx); err != nil {
		return nil, err
	}
	if config.VisualReflexMaxTokens <= 0 || config.VisualReflexTimeoutMS <= 0 {
		return nil, errors.New("Meeting visual reflex requires positive token and timeout bounds")
	}
	fast, err := providers.NewLLM(meetingForegroundLLMRequest(config))
	if err != nil {
		return nil, fmt.Errorf("create Meeting foreground model: %w", err)
	}
	visualReflex, err := providers.NewLLM(meetingVisualReflexLLMRequest(config))
	if err != nil {
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("create Meeting visual reflex model: %w", err)
	}
	asr, err := providers.NewASRFactory(meetingASRRequest(config))
	if err != nil {
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("create Meeting foreground ASR: %w", err)
	}
	asrDescriptor, err := providers.DescribeASR(meetingASRRequest(config))
	if err != nil {
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("describe Meeting foreground ASR: %w", err)
	}
	speech, err := providers.NewTTS(meetingTTSRequest(config))
	if err != nil {
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("create Meeting foreground TTS: %w", err)
	}
	vision, err := openaivision.New(openaivision.Config{
		BaseURL: config.VisionURL, Model: config.VisionModel,
		APIKey: os.Getenv("OPENREALTIME_LOCAL_API_KEY"), MaxOutputTokens: 2048,
		RequestTimeout: time.Duration(config.RequestTimeoutMS) * time.Millisecond,
		Temperature:    0,
	})
	if err != nil {
		_ = closeReadinessResource(speech)
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("create Meeting foreground vision: %w", err)
	}
	narrator, err := perception.NewNarrator(perception.NarratorConfig{
		Vision: vision, Label: "meeting-foreground-dedicated",
	})
	if err != nil {
		_ = closeReadinessResource(speech)
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, err
	}
	policies := interaction.Defaults()
	policies.Rollout = meetingForegroundRollout{}
	dormant := meetingDormantProvider{descriptor: meetingDormantDescriptor()}
	inner, err := cascade.New(cascade.Config{
		Profile: "voice+vision", Perception: asr,
		PerceptionDescriptor: asrDescriptor,
		Narrator:             narrator, DeciderSees: true,
		Fast: fast, Slow: dormant, FastMaxTokens: config.MaxOutputTokens,
		SlowMaxTokens: 1, Speech: bysentence.Provider{
			Inner: speech, Minimum: config.SentenceMinRunes,
		},
		Voice: config.TTSVoice,
		// The Meeting profile deliberately gives its local, attested foreground
		// provider only the two bounded client-declared action lanes. Standard
		// computer actions still require an exact target and confirm=never; other
		// tools must explicitly opt into the background-safe contract. Keeping
		// the allowlist in cascade preserves the caller-owned tool catalog rather
		// than building a Meeting UI or action surface into the server.
		FastComputerUse:     true,
		FastBackgroundTools: true,
		VisualReflex:        visualReflex, VisualReflexMaxTokens: config.VisualReflexMaxTokens,
		VisualReflexTimeout: time.Duration(config.VisualReflexTimeoutMS) * time.Millisecond,
		Policies:            policies, ObservationPolicy: cascade.ObservationEndpointOnly,
		ASRCadence: time.Duration(config.ASRCadenceMS) * time.Millisecond,
		Observers: []perception.Factory{perception.VideoFactory(perception.VideoConfig{
			Name: "screen", Sources: []string{"screen"}, ExternalCadence: config.ExternalVideoGate,
			ChangeThreshold: 0.02, Narrator: narrator, AttachKeyframes: config.AttachKeyframes,
		})},
		DefaultObservers: []string{"audio", "screen"},
		AgentInstruction: "Assist with the live meeting. Ground answers in the shared transcript and screen observations, honor later corrections, and stay concise.",
	})
	if err != nil {
		_ = closeReadinessResource(speech)
		_ = closeReadinessResource(visualReflex)
		_ = closeReadinessResource(fast)
		return nil, fmt.Errorf("compose Meeting foreground binding: %w", err)
	}
	return &meetingForegroundBinding{
		inner: inner, capabilities: meetingForegroundCapabilities(config), policies: policies,
	}, nil
}

func (binding *meetingForegroundBinding) Name() string { return meetingForegroundBindingName }

func (binding *meetingForegroundBinding) Ownership() legacy.Ownership {
	return meetingForegroundOwnership()
}

func (binding *meetingForegroundBinding) Capabilities() legacy.Capabilities {
	capabilities := binding.capabilities
	capabilities.Observers = slices.Clone(binding.capabilities.Observers)
	return capabilities
}

func (binding *meetingForegroundBinding) Start(
	ctx context.Context, options legacy.Options,
) (legacy.Runtime, error) {
	if binding == nil || binding.inner == nil {
		return nil, errors.New("start Meeting foreground: nil binding")
	}
	options.Settings = legacy.CloneSettings(options.Settings)
	policies := binding.policies
	options.Policies = &policies
	runtime, err := binding.inner.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	return &meetingForegroundRuntime{
		Runtime: runtime, capabilities: binding.Capabilities(),
	}, nil
}

type meetingForegroundRuntime struct {
	legacy.Runtime
	capabilities legacy.Capabilities
}

func (runtime *meetingForegroundRuntime) Status() legacy.Status {
	status := runtime.Runtime.Status()
	status.Binding = meetingForegroundBindingName
	status.Stack = runtime.capabilities.Stack
	status.Slow = ""
	return status
}

type meetingVisualProvider struct {
	perception.Narrator
	descriptor perceptionelements.VisualProviderDescriptor
}

func (provider *meetingVisualProvider) Name() string {
	return provider.descriptor.Name
}

func (provider *meetingVisualProvider) Descriptor() perceptionelements.VisualProviderDescriptor {
	return provider.descriptor
}

func newMeetingVisualProvider(
	ctx context.Context, config meetingLocalForegroundConfig,
	descriptor perceptionelements.VisualProviderDescriptor,
) (perceptionelements.VisualProvider, error) {
	if err := profileProviderContext(ctx); err != nil {
		return nil, err
	}
	vision, err := openaivision.New(openaivision.Config{
		BaseURL: config.VisionURL, Model: config.VisionModel,
		APIKey: os.Getenv("OPENREALTIME_LOCAL_API_KEY"), MaxOutputTokens: 2048,
		RequestTimeout: time.Duration(config.RequestTimeoutMS) * time.Millisecond,
		Temperature:    0,
	})
	if err != nil {
		return nil, err
	}
	narrator, err := perception.NewNarrator(perception.NarratorConfig{
		Vision: vision, Label: "meeting-graph-dedicated",
	})
	if err != nil {
		return nil, err
	}
	return &meetingVisualProvider{Narrator: narrator, descriptor: descriptor}, nil
}

type meetingProfileOptions struct {
	out           string
	graphOut      string
	valuesOut     string
	resolutionOut string
	executionOut  string
	name          string
	revision      uint64
	tokenEnv      string
	inspectionTTL uint64
	maxAudioBytes int
	deployments   meetingDeploymentIdentities
	verifier      meetingDeploymentVerifier
}

func defaultMeetingProfileOptions() meetingProfileOptions {
	return meetingProfileOptions{
		name: "openrealtime.launch.meeting-assistant-local", revision: 1,
		tokenEnv:      "OPENREALTIME_TOKEN",
		inspectionTTL: 30_000, maxAudioBytes: 1 << 20,
	}
}

type frozenMeetingProfile struct {
	Profile       launchprofile.Document
	Plan          *graphconfig.Plan
	Values        graphvalues.Document
	Resolution    bench.LiveResolution
	Execution     bench.ExecutionRequirement
	Configuration meetingLocalConfiguration
}

func freezeProductionMeetingProfile(
	ctx context.Context, options meetingProfileOptions, executable inspect.ArtifactIdentity,
) (frozenMeetingProfile, error) {
	if ctx == nil {
		return frozenMeetingProfile{}, errors.New("freeze production Meeting Assistant profile: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return frozenMeetingProfile{}, cause
	}
	if strings.TrimSpace(options.name) == "" || options.revision == 0 ||
		strings.TrimSpace(options.tokenEnv) == "" || options.inspectionTTL == 0 ||
		options.maxAudioBytes <= 0 {
		return frozenMeetingProfile{}, errors.New("Meeting Assistant profile has invalid identity or server bounds")
	}
	if nilMeetingDeploymentInterface(options.verifier) {
		return frozenMeetingProfile{}, errors.New(
			"freeze production Meeting Assistant profile without a deployment verifier",
		)
	}
	if err := options.deployments.validate(); err != nil {
		return frozenMeetingProfile{}, err
	}
	if err := options.verifier.Verify(ctx, options.deployments); err != nil {
		return frozenMeetingProfile{}, fmt.Errorf(
			"verify Meeting Assistant deployments for profile freeze: %w", err,
		)
	}
	selected, err := newServeMeetingRegistration(
		ctx, executable, options.deployments, options.verifier,
	)
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	application, err := graphs.MeetingAssistantApplicationConfig(selectedRegistration(selected), 1)
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	payload, err := json.Marshal(application)
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	launchConfig, err := selected.Application.Factory(ctx, payload)
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	prepared, err := graphlaunch.New(ctx, launchConfig)
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	plan := prepared.Plan
	profile, err := launchprofile.Freeze(launchprofile.Document{
		FormatVersion: launchprofile.FormatVersion,
		Name:          options.name, Revision: options.revision,
		Application: launchprofile.Application{
			Selection: launchprofile.Selection{
				Reference: selected.Application.Reference, Artifact: selected.Application.Artifact,
			},
			Configuration: payload,
		},
		Plan: plan.Identity(),
		Adapter: launchprofile.AdapterSelection{
			Reference: selected.Adapter.Reference, RuntimeArtifact: selected.Adapter.RuntimeArtifact,
			ProfileName:     selected.Adapter.ProfileName,
			ProfileRevision: selected.Adapter.ProfileRevision,
		},
		Server: launchprofile.Server{
			ProfileName: "openrealtime.server.meeting-assistant-local", ProfileRevision: 1,
			ProviderArtifact: selected.Provider, GatewayArtifact: executable,
			TokenEnvironment: options.tokenEnv,
			Model:            meetingLocalModelName, TranscriptionModel: meetingLocalASRModel,
			ValidateWire: true, InspectionTokenTTLMS: options.inspectionTTL,
			MaxAudioFrameBytes: options.maxAudioBytes,
			VideoLimits:        openrealtime.DefaultLimits(),
		},
	})
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	values := graphvalues.Document{
		APIVersion: graphvalues.APIVersion, Graph: plan.Graph().ID, Nodes: plan.Values(),
	}
	bound, err := graphvalues.Bind(plan.Graph(), values)
	if err != nil || bound.Graph.Fingerprint != plan.Graph().Fingerprint {
		return frozenMeetingProfile{},
			errors.New("freeze Meeting Assistant values did not reproduce the exact bound graph")
	}
	resolution, err := probeMeetingExpectedResolution(ctx, prepared.Binding, plan, bench.ArtifactIdentity{
		ID: "values://" + plan.Graph().ID, Revision: graphvalues.APIVersion,
		Digest: bound.Fingerprint,
	})
	if err != nil {
		return frozenMeetingProfile{}, err
	}
	requirement, err := bench.RequireGraph(plan.Graph(), bench.ArtifactIdentity{
		ID: "values://" + plan.Graph().ID, Revision: graphvalues.APIVersion,
		Digest: bound.Fingerprint,
	}, resolution)
	if err != nil {
		return frozenMeetingProfile{}, fmt.Errorf("author Meeting execution requirement: %w", err)
	}
	return frozenMeetingProfile{
		Profile: profile, Plan: plan, Values: values, Resolution: resolution,
		Execution: requirement, Configuration: selected.Configuration,
	}, nil
}

func selectedRegistration(selected serveMeetingRegistration) graphs.MeetingAssistantRegistration {
	return graphs.MeetingAssistantRegistration{
		Application: selected.Application, Adapter: selected.Adapter,
	}
}

var (
	_ continuation.Provider             = meetingDormantProvider{}
	_ legacy.Binding                    = (*meetingForegroundBinding)(nil)
	_ legacy.Runtime                    = (*meetingForegroundRuntime)(nil)
	_ perceptionelements.VisualProvider = (*meetingVisualProvider)(nil)
)
