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
			Name: "whisper-server", Aliases: []string{"whisper", "faster-whisper"},
			Label: "Local whisper server", Dialect: DialectOpenAITranscriptions,
			BaseURL: "http://127.0.0.1:8001/v1", Auth: AuthNone, Local: true,
			KeyEnv: []string{"OPENREALTIME_ASR_API_KEY"},
			Notes:  "whisper.cpp, faster-whisper-server, or anything else serving the OpenAI route.",
		},
		Model: "whisper-1",
	},
	{
		Common: Common{
			Name: "openai-compatible", Aliases: []string{"custom"},
			Label:   "Any OpenAI-compatible transcription endpoint",
			Dialect: DialectOpenAITranscriptions, BaseURL: "http://127.0.0.1:8001/v1",
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
	// PartialInterval asks a batch endpoint for hypotheses this often, by
	// re-transcribing the utterance. It is ignored by a streaming provider,
	// which produces them for free.
	PartialInterval time.Duration
	RequestTimeout  time.Duration
	Header          http.Header
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

	switch entry.Dialect {
	case DialectQwenASR:
		return func() (v1.PerceptionProvider, error) {
			return qwenasr.New(qwenasr.Config{
				BaseURL: baseURL, Model: model, BearerToken: key,
				Headers: request.Header, RequestTimeout: request.RequestTimeout,
			})
		}, nil
	case DialectDeepgramListen:
		return func() (v1.PerceptionProvider, error) {
			return deepgram.NewListener(deepgram.ListenConfig{
				URL: baseURL, Model: model, APIKey: key, Language: request.Language,
				Header: request.Header,
			})
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
