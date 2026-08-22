package providers

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/fishaudio"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/pcmtts"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
)

// TTS is one speech provider.
type TTS struct {
	Common
	// Model is the default synthesis model.
	Model string
	// Voice is the default voice, where the vendor separates it from the
	// model. Empty means the vendor either names the voice in the model or
	// requires the deployment to choose one.
	Voice string
	// VoiceRequired reports that synthesis cannot start without a voice
	// identifier, because the vendor puts it in the request path.
	VoiceRequired bool
	// SampleRates are the output rates the vendor serves. Empty means it will
	// serve whatever is asked for.
	SampleRates []uint32
}

// ttsCatalog is the speech catalogue.
var ttsCatalog = []TTS{
	{
		Common: Common{
			Name: "openai-compatible", Aliases: []string{"local", "sglang"},
			Label: "OpenAI-compatible speech endpoint", Dialect: DialectOpenAISpeech,
			BaseURL: "http://127.0.0.1:8081/v1/audio/speech", Auth: AuthBearer, Local: true,
			KeyEnv: []string{"OPENREALTIME_TTS_API_KEY"},
			Notes:  "The self-hosted default: SGLang-Omni, Kokoro, or anything serving /v1/audio/speech.",
		},
		Model: openaitts.DefaultModel, Voice: "default",
	},
	{
		Common: Common{
			Name: "openai", Label: "OpenAI", Dialect: DialectOpenAISpeech,
			BaseURL: "https://api.openai.com/v1/audio/speech", Auth: AuthBearer,
			KeyEnv: []string{"OPENAI_API_KEY"},
		},
		Model: "gpt-4o-mini-tts", Voice: "alloy",
	},
	{
		Common: Common{
			Name: "fish-audio", Aliases: []string{"fishaudio", "fish"}, Label: "Fish Speech (self-hosted)",
			Dialect: DialectFishNative, BaseURL: fishaudio.DefaultEndpoint, Auth: AuthBearer,
			Local: true, KeyEnv: []string{"OPENREALTIME_TTS_API_KEY"},
			Notes: "The native Fish server, which selects its model at start-up rather than per request.",
		},
		Model: fishaudio.DefaultModel,
	},
	{
		Common: Common{
			Name: "deepgram", Label: "Deepgram Aura", Dialect: DialectPCMPost,
			BaseURL: pcmtts.DeepgramEndpoint, Auth: AuthToken,
			KeyEnv: []string{"DEEPGRAM_API_KEY"},
			Notes:  "Deepgram names the voice inside the model; set the model, not a voice.",
		},
		Model: pcmtts.DeepgramDefaultModel,
	},
	{
		Common: Common{
			Name: "elevenlabs", Aliases: []string{"eleven"}, Label: "ElevenLabs",
			Dialect: DialectPCMPost, BaseURL: pcmtts.ElevenLabsEndpoint, Auth: AuthXIKey,
			KeyEnv: []string{"ELEVENLABS_API_KEY", "ELEVEN_API_KEY"},
			Notes:  "The voice identifier is part of the request path, so it has to be set.",
		},
		Model: pcmtts.ElevenLabsDefaultModel, VoiceRequired: true,
		SampleRates: []uint32{16_000, 22_050, 24_000, 44_100},
	},
	{
		Common: Common{
			Name: "cartesia", Label: "Cartesia Sonic", Dialect: DialectPCMPost,
			BaseURL: pcmtts.CartesiaEndpoint, Auth: AuthBearer,
			KeyEnv: []string{"CARTESIA_API_KEY"},
			Notes:  "Sends a dated Cartesia-Version header; check it before a deployment.",
		},
		Model: pcmtts.CartesiaDefaultModel, VoiceRequired: true,
	},
	{
		Common: Common{
			Name: "groq", Label: "Groq (PlayAI)", Dialect: DialectOpenAISpeech,
			BaseURL: "https://api.groq.com/openai/v1/audio/speech", Auth: AuthBearer,
			KeyEnv: []string{"GROQ_API_KEY"},
		},
		Model: "playai-tts", Voice: "Fritz-PlayAI",
	},
	{
		Common: Common{
			Name: "siliconflow", Label: "SiliconFlow", Dialect: DialectOpenAISpeech,
			BaseURL: "https://api.siliconflow.cn/v1/audio/speech", Auth: AuthBearer,
			KeyEnv: []string{"SILICONFLOW_API_KEY"},
		},
		Model: "FunAudioLLM/CosyVoice2-0.5B", Voice: "alex",
	},
	{
		Common: Common{
			Name: "fish-audio-cloud", Aliases: []string{"fish-cloud"}, Label: "Fish Audio (hosted)",
			Dialect: DialectOpenAISpeech, BaseURL: "https://api.fish.audio/v1/audio/speech",
			Auth: AuthBearer, KeyEnv: []string{"FISH_AUDIO_API_KEY"},
			Notes: "Fish Audio's hosted service through its OpenAI-compatible route.",
		},
		Model: "s1", Voice: "default",
	},
}

// TTSs returns the catalogue in listing order.
func TTSs() []TTS { return slices.Clone(ttsCatalog) }

// LookupTTS resolves a speech provider name or alias.
func LookupTTS(name string) (TTS, error) {
	normalised := normalise(name)
	for _, entry := range ttsCatalog {
		if entry.matches(normalised) {
			return entry, nil
		}
	}
	return TTS{}, &UnknownError{
		Role: RoleTTS, Name: name,
		Known: names(ttsCatalog, func(entry TTS) Common { return entry.Common }),
	}
}

// TTSRequest is one resolved speech construction.
type TTSRequest struct {
	Provider string
	Model    string
	Voice    string
	BaseURL  string
	APIKey   string
	KeyEnv   string
	Language string
	// OutputSampleRateHz is the rate chunks reach the action plane at. The
	// Realtime wire carries 24 kHz, so that is what the server asks for.
	OutputSampleRateHz uint32
	RequestTimeout     time.Duration
	Header             http.Header
}

// NewTTS builds the configured speech provider.
func NewTTS(request TTSRequest) (v1.StreamingSpeechProvider, error) {
	entry, err := LookupTTS(request.Provider)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = entry.Model
	}
	voice := strings.TrimSpace(request.Voice)
	if voice == "" || strings.EqualFold(voice, "default") {
		voice = entry.Voice
	}
	if entry.VoiceRequired && (voice == "" || strings.EqualFold(voice, "default")) {
		return nil, fmt.Errorf("speech provider %q requires a voice identifier", entry.Name)
	}
	endpoint := strings.TrimSpace(request.BaseURL)
	if endpoint == "" {
		endpoint = entry.BaseURL
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return nil, fmt.Errorf("speech provider %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}
	rate := request.OutputSampleRateHz
	if rate == 0 {
		rate = 24_000
	}
	// A vendor that serves a closed set of rates is asked for the nearest one
	// it has and the shared reader resamples the difference. Asking for a rate
	// the vendor does not serve is an error there, not a silent fallback.
	requestRate := rate
	if len(entry.SampleRates) > 0 && !slices.Contains(entry.SampleRates, rate) {
		requestRate = nearestRate(entry.SampleRates, rate)
	}

	switch entry.Dialect {
	case DialectOpenAISpeech:
		return openaitts.New(openaitts.Config{
			Endpoint: endpoint, Model: model, Voice: voice, BearerToken: key,
			Headers: request.Header, RequestTimeout: request.RequestTimeout,
			FallbackSampleRate: rate, OutputSampleRateHz: rate,
		})
	case DialectFishNative:
		return fishaudio.New(fishaudio.Config{
			Endpoint: endpoint, Model: model, BearerToken: key,
			Headers: request.Header, RequestTimeout: request.RequestTimeout,
			OutputSampleRateHz: rate,
		})
	case DialectPCMPost:
		config := pcmtts.Config{
			APIKey: key, Model: model, Voice: voice, Endpoint: endpoint,
			Language: request.Language, RequestSampleRateHz: requestRate,
			OutputSampleRateHz: rate, Headers: request.Header,
			RequestTimeout: request.RequestTimeout,
		}
		switch entry.Name {
		case "deepgram":
			return pcmtts.NewDeepgram(config)
		case "elevenlabs":
			return pcmtts.NewElevenLabs(config)
		case "cartesia":
			return pcmtts.NewCartesia(config)
		}
		return nil, fmt.Errorf("speech provider %q has no PCM profile", entry.Name)
	default:
		return nil, fmt.Errorf("speech provider %q has no adapter for dialect %q", entry.Name, entry.Dialect)
	}
}

// nearestRate picks the served rate closest to what was asked for.
func nearestRate(served []uint32, wanted uint32) uint32 {
	best := served[0]
	for _, candidate := range served[1:] {
		if difference(candidate, wanted) < difference(best, wanted) {
			best = candidate
		}
	}
	return best
}

func difference(left, right uint32) uint32 {
	if left > right {
		return left - right
	}
	return right - left
}
