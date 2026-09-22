package providers

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/deepgram"
	"github.com/bojieli/OpenRealtime/adapters/openaitranscribe"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	"github.com/bojieli/OpenRealtime/adapters/vllmrealtime"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// ASR is one recogniser provider.
type ASR struct {
	Common
	// Model is the default recognition model.
	Model string
	// Streaming reports whether the provider emits hypotheses before the
	// speaker stops.
	//
	// It is the single most important property of a recogniser in this
	// system, because the stable-partial observation policy has nothing to
	// observe without it, and it is a property of the vendor's protocol
	// rather than of its accuracy. A batch endpoint listed here still works;
	// it just recognises once, at the endpoint of the utterance.
	Streaming bool
	// Path overrides the transcription route for a batch endpoint.
	Path string
	// ModelField names the multipart field carrying the model.
	ModelField string
	// AuthHeader names the header the credential goes in.
	AuthHeader string
}

// asrCatalog is the recogniser catalogue.
var asrCatalog = []ASR{
	{
		Common: Common{
			Name: "qwen-asr", Aliases: []string{"qwen", "local"}, Label: "Qwen3-ASR (local service)",
			Dialect: DialectQwenASR, BaseURL: qwenasr.DefaultBaseURL, Auth: AuthBearer, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes:  "The self-hosted default: streaming, no account, no audio leaves the machine.",
		},
		Model: qwenasr.DefaultModel, Streaming: true,
	},
	{
		Common: Common{
			Name: "streaming-asr", Aliases: []string{"local-streaming"},
			Label: "Local streaming recogniser service (start/chunk/finish)", Dialect: DialectQwenASR,
			BaseURL: "http://127.0.0.1:9110", Auth: AuthBearer, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes: "The qwen-asr dialect served by tools/duplexmodels for any streaming recogniser " +
				"(Nemotron, Kyutai STT, ...); a committed prefix is reported when the service sends one.",
		},
		Model: "nvidia/nemotron-speech-streaming-en-0.6b", Streaming: true,
	},
	{
		Common: Common{
			Name: "vllm-realtime", Aliases: []string{"voxtral", "voxtral-realtime"},
			Label: "vLLM realtime transcription (local)", Dialect: DialectVLLMRealtime,
			BaseURL: vllmrealtime.DefaultURL, Auth: AuthBearer, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes: "Natively streaming recognisers served by vLLM's /v1/realtime route - Voxtral " +
				"Mini 4B Realtime by default, Qwen3-ASR with -asr-model and -asr-url. Deltas are " +
				"append-only, so every reported word is already committed.",
		},
		Model: vllmrealtime.DefaultModel, Streaming: true,
	},
	{
		Common: Common{
			Name: "deepgram", Label: "Deepgram", Dialect: DialectDeepgramListen,
			BaseURL: deepgram.DefaultListenURL, Auth: AuthToken,
			KeyEnv: []string{"DEEPGRAM_API_KEY"},
			Notes:  "Streaming over a WebSocket, with interim results; built for voice agents.",
		},
		Model: deepgram.DefaultListenModel, Streaming: true,
	},
	{
		Common: Common{
			Name: "openai", Label: "OpenAI", Dialect: DialectOpenAITranscriptions,
			BaseURL: "https://api.openai.com/v1", Auth: AuthBearer,
			KeyEnv: []string{"OPENAI_API_KEY"},
		},
		Model: "gpt-4o-transcribe",
	},
	{
		Common: Common{
			Name: "elevenlabs", Aliases: []string{"scribe"}, Label: "ElevenLabs Scribe",
			Dialect: DialectElevenLabsSTT, BaseURL: "https://api.elevenlabs.io/v1",
			Auth: AuthXIKey, KeyEnv: []string{"ELEVENLABS_API_KEY", "ELEVEN_API_KEY"},
			Notes: "Batch recognition here. ElevenLabs also has a realtime WebSocket that this " +
				"adapter does not speak.",
		},
		Model: "scribe_v2", Path: "/speech-to-text", ModelField: "model_id", AuthHeader: "xi-api-key",
	},
	{
		Common: Common{
			Name: "groq", Label: "Groq (Whisper)", Dialect: DialectOpenAITranscriptions,
			BaseURL: "https://api.groq.com/openai/v1", Auth: AuthBearer,
			KeyEnv: []string{"GROQ_API_KEY"},
			Notes:  "Batch, but fast enough that the round trip is close to a streaming endpoint's.",
		},
		Model: "whisper-large-v3-turbo",
	},
	{
		Common: Common{
			Name: "fireworks", Label: "Fireworks AI", Dialect: DialectOpenAITranscriptions,
			BaseURL: "https://api.fireworks.ai/inference/v1", Auth: AuthBearer,
			KeyEnv: []string{"FIREWORKS_API_KEY"},
		},
		Model: "whisper-v3-turbo",
	},
	{
		Common: Common{
			Name: "siliconflow", Label: "SiliconFlow", Dialect: DialectOpenAITranscriptions,
			BaseURL: "https://api.siliconflow.cn/v1", Auth: AuthBearer,
			KeyEnv: []string{"SILICONFLOW_API_KEY"},
		},
		Model: "FunAudioLLM/SenseVoiceSmall",
	},
	{
		Common: Common{
			Name: "mistral", Label: "Mistral (Voxtral)", Dialect: DialectOpenAITranscriptions,
			BaseURL: "https://api.mistral.ai/v1", Auth: AuthBearer,
			KeyEnv: []string{"MISTRAL_API_KEY"},
		},
		Model: "voxtral-mini-latest",
	},
	{
		Common: Common{
			Name: "sensevoice", Aliases: []string{"funasr", "sensevoice-small"},
			Label: "Local SenseVoice (FunASR)", Dialect: DialectOpenAITranscriptions,
			BaseURL: "http://127.0.0.1:8002/v1", Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes: "Self-hosted and non-autoregressive, so cost tracks the audio " +
				"rather than the transcript: see deploy/sensevoice.",
		},
		Model: "iic/SenseVoiceSmall",
	},
	{
		Common: Common{
			Name: "whisper-server", Aliases: []string{"whisper", "faster-whisper"},
			Label: "Local whisper server", Dialect: DialectOpenAITranscriptions,
			// :8003, not :8001: the streaming Qwen3-ASR service owns :8001 in
			// the documented local stack, and a whisper server defaulting to the
			// same port would silently answer the wrong dialect on it.
			BaseURL: "http://127.0.0.1:8003/v1", Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes:  "whisper.cpp, faster-whisper-server, or anything else serving the OpenAI route.",
		},
		Model: "whisper-1",
	},
	{
		Common: Common{
			Name: "openai-compatible", Aliases: []string{"custom"},
			Label:   "Any OpenAI-compatible transcription endpoint",
			Dialect: DialectOpenAITranscriptions, BaseURL: "http://127.0.0.1:8003/v1",
			Auth: AuthBearer, Local: true, KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes: "The escape hatch: point the base URL anywhere serving the OpenAI route.",
		},
		Model: openaitranscribe.DefaultModel,
	},
}

// ASRs returns the catalogue in listing order.
func ASRs() []ASR { return slices.Clone(asrCatalog) }

// LookupASR resolves a recogniser name or alias.
func LookupASR(name string) (ASR, error) {
	normalised := normalise(name)
	for _, entry := range asrCatalog {
		if entry.matches(normalised) {
			return entry, nil
		}
	}
	return ASR{}, &UnknownError{
		Role: RoleASR, Name: name,
		Known: names(asrCatalog, func(entry ASR) Common { return entry.Common }),
	}
}

// ASRRequest is one resolved recogniser construction.
type ASRRequest struct {
	Provider string
	Model    string
	BaseURL  string
	APIKey   string
	KeyEnv   string
	Language string
	// Keyterms are exact streaming-recognizer vocabulary hints. They are
	// currently implemented by Deepgram Nova-3 as repeated keyterm query
	// parameters and ignored by no other provider.
	Keyterms []string
	// PartialInterval asks a batch endpoint for hypotheses this often, by
	// re-transcribing the utterance. It is ignored by a streaming provider,
	// which produces them for free.
	PartialInterval time.Duration
	// Endpointing configures a streaming service's own VAD. Batch providers
	// ignore it; Deepgram honours it; the local Qwen3-ASR service, whose
	// endpoint the engine's own gate decides, refuses it.
	Endpointing time.Duration
	// EOTThreshold, EagerEOTThreshold and EOTTimeout configure Deepgram Flux's
	// model-integrated turn detection: the confidence for EndOfTurn, the
	// confidence for EagerEndOfTurn (which also enables TurnResumed), and the
	// silence after which a turn ends regardless. Zero leaves the service
	// default. Every recogniser other than a Flux model refuses them.
	EOTThreshold      float64
	EagerEOTThreshold float64
	EOTTimeout        time.Duration
	RequestTimeout    time.Duration
	Header            http.Header
}

// ASRAcceptsLanguage reports whether a recogniser forwards a language hint to
// its service. The default recogniser detects the language itself and refuses
// one; composers use this to decide whether a shared, role-agnostic language
// setting should reach recognition at all.
func ASRAcceptsLanguage(provider string) bool {
	entry, err := LookupASR(provider)
	if err != nil {
		return false
	}
	return entry.Dialect != DialectQwenASR && entry.Dialect != DialectVLLMRealtime
}

// DescribeASR validates and normalizes through the same constructor path as a
// live provider, but supplies a non-secret sentinel credential. Provider
// constructors in this package are resource-free; sockets are opened only by
// utterance methods. This prevents metadata preflight and live construction
// from accepting different endpoint, timeout, model, or dialect settings.
func DescribeASR(request ASRRequest) (v1.Descriptor, error) {
	request.APIKey = "launch-profile-preflight-sentinel"
	request.KeyEnv = ""
	factory, err := NewASRFactory(request)
	if err != nil {
		return v1.Descriptor{}, err
	}
	provider, err := factory()
	if err != nil {
		return v1.Descriptor{}, err
	}
	if closer, ok := provider.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	descriptor := provider.Descriptor()
	if err := descriptor.Validate(); err != nil {
		return v1.Descriptor{}, err
	}
	return descriptor, nil
}

// NewASRFactory returns a factory producing one recogniser per utterance.
//
// It is a factory rather than a provider because every recogniser here owns
// the state of exactly one stream: a WebSocket, a remote session, or an audio
// buffer. Sharing one across concurrent utterances would interleave them.
func NewASRFactory(request ASRRequest) (func() (v1.PerceptionProvider, error), error) {
	entry, err := LookupASR(request.Provider)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = entry.Model
	}
	baseURL := strings.TrimSpace(request.BaseURL)
	if baseURL == "" {
		baseURL = entry.BaseURL
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return nil, fmt.Errorf("recogniser %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}

	flux := entry.Dialect == DialectDeepgramListen && deepgram.IsFluxModel(model)
	if !flux && (request.EOTThreshold != 0 || request.EagerEOTThreshold != 0 || request.EOTTimeout != 0) {
		return nil, fmt.Errorf("recogniser %q model %q has no model-integrated turn detection; "+
			"end-of-turn thresholds and timeout are Deepgram Flux settings", entry.Name, model)
	}

	switch entry.Dialect {
	case DialectQwenASR:
		// The local Qwen3-ASR service detects the language itself, has no
		// vocabulary-hint parameter, and leaves endpointing to the engine's
		// own acoustic gate. Each of these options used to be accepted and
		// silently dropped, so a deployment could believe it had configured
		// recognition it had not. A recogniser that cannot honour an option
		// must say so rather than run a different configuration than it
		// reports.
		if language := strings.TrimSpace(request.Language); language != "" {
			return nil, fmt.Errorf("recogniser %q detects the language itself and cannot honour language %q; "+
				"leave -asr-language unset for it", entry.Name, language)
		}
		if len(request.Keyterms) > 0 {
			return nil, fmt.Errorf("recogniser %q has no vocabulary hints and cannot honour keyterms; "+
				"they are a Deepgram feature", entry.Name)
		}
		if request.Endpointing > 0 {
			return nil, fmt.Errorf("recogniser %q leaves endpointing to the engine's own gate and cannot honour endpointing %s; "+
				"it configures a streaming service's VAD, which only Deepgram exposes", entry.Name, request.Endpointing)
		}
		return func() (v1.PerceptionProvider, error) {
			return qwenasr.New(qwenasr.Config{
				BaseURL: baseURL, Model: model, BearerToken: key,
				Headers: request.Header, RequestTimeout: request.RequestTimeout,
			})
		}, nil
	case DialectVLLMRealtime:
		// The realtime route detects the language itself and exposes neither
		// vocabulary hints nor its own endpointing; refuse rather than drop.
		if language := strings.TrimSpace(request.Language); language != "" {
			return nil, fmt.Errorf("recogniser %q detects the language itself and cannot honour language %q; "+
				"leave -asr-language unset for it", entry.Name, language)
		}
		if len(request.Keyterms) > 0 {
			return nil, fmt.Errorf("recogniser %q has no vocabulary hints and cannot honour keyterms", entry.Name)
		}
		if request.Endpointing > 0 {
			return nil, fmt.Errorf("recogniser %q leaves endpointing to the engine's own gate and cannot honour endpointing %s",
				entry.Name, request.Endpointing)
		}
		return func() (v1.PerceptionProvider, error) {
			return vllmrealtime.New(vllmrealtime.Config{
				URL: baseURL, Model: model, BearerToken: key, Header: request.Header,
			})
		}, nil
	case DialectDeepgramListen:
		if flux {
			config, err := deepgramFluxConfig(request, model, strings.TrimSpace(request.BaseURL), key)
			if err != nil {
				return nil, err
			}
			return func() (v1.PerceptionProvider, error) {
				return deepgram.NewFluxListener(config)
			}, nil
		}
		return func() (v1.PerceptionProvider, error) {
			// One stream per session, not per utterance. The dial, the TLS
			// handshake and Deepgram's warm-up then happen once, before the
			// first word, instead of between every endpoint and the next
			// hypothesis.
			config := deepgram.ListenConfig{
				URL: baseURL, Model: model, APIKey: key, Language: request.Language,
				Keyterms: request.Keyterms, Header: request.Header, Endpointing: request.Endpointing,
				Persistent: true,
			}
			if strings.Contains(request.Language, ",") {
				languages := strings.Split(request.Language, ",")
				return deepgram.NewLanguageMux(config, languages)
			}
			return deepgram.NewListener(config)
		}, nil
	case DialectOpenAITranscriptions, DialectElevenLabsSTT:
		return func() (v1.PerceptionProvider, error) {
			return openaitranscribe.New(openaitranscribe.Config{
				BaseURL: baseURL, Path: entry.Path, Model: model, APIKey: key,
				AuthHeader: entry.AuthHeader, ModelField: entry.ModelField,
				Provider: entry.Name, Language: request.Language,
				PartialInterval: request.PartialInterval, Headers: request.Header,
				RequestTimeout: request.RequestTimeout,
			})
		}, nil
	default:
		return nil, fmt.Errorf("recogniser %q has no adapter for dialect %q", entry.Name, entry.Dialect)
	}
}

// deepgramFluxConfig maps a recogniser request onto Flux, refusing what Flux
// cannot honour rather than running without it.
//
// Flux has no Nova-3 endpointing: its turn detection is the thresholds. It has
// no locale parameter either: flux-general-en is English, and
// flux-general-multi takes language hints for its ten languages, none of which
// is Mandarin - so the English/Mandarin pair the room recognises with two
// Nova-3 lanes has no Flux equivalent.
func deepgramFluxConfig(request ASRRequest, model, baseURL, key string) (deepgram.FluxConfig, error) {
	if baseURL == "" || baseURL == deepgram.DefaultListenURL {
		if baseURL != "" {
			return deepgram.FluxConfig{}, fmt.Errorf("Deepgram Flux model %q is served on %s, not %s",
				model, deepgram.DefaultFluxURL, deepgram.DefaultListenURL)
		}
		baseURL = deepgram.DefaultFluxURL
	}
	if request.Endpointing != 0 {
		return deepgram.FluxConfig{}, fmt.Errorf("Deepgram Flux has no endpointing setting (got %s); "+
			"set its end-of-turn thresholds and timeout instead", request.Endpointing)
	}
	language := strings.TrimSpace(request.Language)
	var hints []string
	switch {
	case model == deepgram.FluxMultilingualModel:
		if language != "" && language != "multi" {
			for _, hint := range strings.Split(language, ",") {
				hints = append(hints, strings.TrimSpace(hint))
			}
		}
	case language != "" && language != "en" && language != "en-US":
		return deepgram.FluxConfig{}, fmt.Errorf("Deepgram Flux model %q transcribes English only, not %q; "+
			"%s takes language hints, and no Flux model recognises Mandarin",
			model, language, deepgram.FluxMultilingualModel)
	}
	config := deepgram.FluxConfig{
		URL: baseURL, Model: model, APIKey: key,
		EOTThreshold: request.EOTThreshold, EagerEOTThreshold: request.EagerEOTThreshold,
		EOTTimeout: request.EOTTimeout, Keyterms: slices.Clone(request.Keyterms),
		LanguageHints: hints, Header: request.Header,
	}
	if _, err := deepgram.NewFluxListener(config); err != nil {
		return deepgram.FluxConfig{}, err
	}
	return config, nil
}
