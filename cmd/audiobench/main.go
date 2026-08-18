// Command audiobench runs paced local ASR and streaming TTS measurements and
// writes a secret-free JSON evidence report.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/fishaudio"
	"github.com/bojieli/OpenRealtime/adapters/openaitts"
	"github.com/bojieli/OpenRealtime/adapters/qwenasr"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/asrbuffer"
	"github.com/bojieli/OpenRealtime/audiobench"
	"github.com/bojieli/OpenRealtime/livebench"
)

const (
	ttsProviderFishNative   = "fish-native"
	ttsProviderOpenAISpeech = "openai-speech"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "audiobench:", err)
		os.Exit(1)
	}
}

type options struct {
	output           string
	stageTimeout     time.Duration
	requestTimeout   time.Duration
	asrAudio         string
	asrReference     string
	asrCase          string
	asrURL           string
	asrModel         string
	asrFrame         time.Duration
	asrProviderChunk time.Duration
	asrPaced         bool
	ttsText          string
	ttsCase          string
	ttsProvider      string
	ttsURL           string
	ttsModel         string
	ttsVoice         string
	ttsReferenceID   string
	ttsReferencePath string
	ttsReferenceText string
	ttsServerRate    uint
	ttsOutputRate    uint
	ttsInitialFrames int
	ttsWAV           string
	ttsSeed          int64
	ttsDeterministic bool
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("audiobench", flag.ContinueOnError)
	var config options
	flags.StringVar(&config.output, "output", "artifacts/audiobench/report.json", "secret-free JSON report")
	flags.DurationVar(&config.stageTimeout, "stage-timeout", 5*time.Minute, "timeout for each complete ASR or TTS stage")
	flags.DurationVar(&config.requestTimeout, "request-timeout", 2*time.Minute, "timeout for each provider request")
	flags.StringVar(&config.asrAudio, "asr-audio", "", "PCM16 WAV utterance; empty disables ASR")
	flags.StringVar(&config.asrReference, "asr-reference", "", "reference transcript for WER")
	flags.StringVar(&config.asrCase, "asr-case", "asr", "ASR case identifier")
	flags.StringVar(&config.asrURL, "asr-url", qwenasr.DefaultBaseURL, "Qwen3-ASR streaming service base URL")
	flags.StringVar(&config.asrModel, "asr-model", qwenasr.DefaultModel, "exact server-side ASR model recorded in the report")
	flags.DurationVar(&config.asrFrame, "asr-frame", 200*time.Millisecond, "scheduler input-frame duration")
	flags.DurationVar(&config.asrProviderChunk, "asr-provider-chunk", 200*time.Millisecond, "minimum provider chunk; zero sends every scheduler frame directly")
	flags.BoolVar(&config.asrPaced, "asr-paced", true, "replay ASR input at wall-clock speed")
	flags.StringVar(&config.ttsText, "tts-text", "", "text to synthesize; empty disables TTS")
	flags.StringVar(&config.ttsCase, "tts-case", "tts", "TTS case identifier")
	flags.StringVar(&config.ttsProvider, "tts-provider", ttsProviderFishNative, "TTS transport: fish-native or openai-speech")
	flags.StringVar(&config.ttsURL, "tts-url", "", "TTS endpoint; provider default when empty")
	flags.StringVar(&config.ttsModel, "tts-model", "", "exact server-side TTS model; provider default when empty")
	flags.StringVar(&config.ttsVoice, "tts-voice", "default", "OpenAI-compatible voice or uploaded voice identifier")
	flags.StringVar(&config.ttsReferenceID, "tts-reference-id", "", "optional server-side Fish voice reference ID")
	flags.StringVar(&config.ttsReferencePath, "tts-reference-audio", "", "optional OpenAI-compatible voice-reference path or URL")
	flags.StringVar(&config.ttsReferenceText, "tts-reference-text", "", "transcript for --tts-reference-audio")
	flags.UintVar(&config.ttsServerRate, "tts-server-rate", 0, "fallback raw PCM source rate; provider default when zero")
	flags.UintVar(&config.ttsOutputRate, "tts-output-rate", 24_000, "resampled PCM output rate")
	flags.IntVar(&config.ttsInitialFrames, "tts-initial-codec-frames", -1, "OpenAI-compatible first codec chunk size; -1 uses model default")
	flags.StringVar(&config.ttsWAV, "tts-wav", "", "optional synthesized PCM16 WAV output")
	flags.Int64Var(&config.ttsSeed, "tts-seed", 0, "Fish sampling seed when --tts-deterministic is set")
	flags.BoolVar(&config.ttsDeterministic, "tts-deterministic", false, "send --tts-seed for repeatable synthesis")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("audiobench accepts flags only")
	}
	if config.stageTimeout <= 0 || config.requestTimeout <= 0 {
		return errors.New("stage and request timeouts must be positive")
	}
	if config.asrFrame <= 0 || config.asrProviderChunk < 0 {
		return errors.New("ASR frame duration must be positive and provider chunk cannot be negative")
	}
	if config.ttsServerRate > math.MaxUint32 || config.ttsOutputRate == 0 || config.ttsOutputRate > math.MaxUint32 {
		return errors.New("TTS source rate must fit 32 bits and output rate must be positive")
	}
	if config.ttsInitialFrames < -1 {
		return errors.New("TTS initial codec frames must be -1 or greater")
	}
	if strings.TrimSpace(config.asrAudio) == "" && strings.TrimSpace(config.ttsText) == "" {
		return errors.New("enable at least one stage with --asr-audio or --tts-text")
	}

	report := audiobench.Report{
		SchemaVersion: audiobench.SchemaVersion,
		CreatedAt:     time.Now().UTC(),
		Runtime: map[string]string{
			"wire_protocol": "unchanged-openai-realtime",
		},
	}
	if strings.TrimSpace(config.asrAudio) != "" {
		audio, err := livebench.ReadWAV(config.asrAudio)
		if err != nil {
			return fmt.Errorf("read ASR audio: %w", err)
		}
		digest, err := livebench.HashFile(config.asrAudio)
		if err != nil {
			return fmt.Errorf("hash ASR audio: %w", err)
		}
		qwenProvider, err := qwenasr.New(qwenasr.Config{
			BaseURL: config.asrURL, Model: config.asrModel,
			BearerToken: os.Getenv("QWEN_ASR_API_KEY"), RequestTimeout: config.requestTimeout,
		})
		if err != nil {
			return fmt.Errorf("configure ASR: %w", err)
		}
		var provider v1.PerceptionProvider = qwenProvider
		if config.asrProviderChunk > 0 {
			provider, err = asrbuffer.New(asrbuffer.Config{
				Provider: qwenProvider, MinimumChunk: config.asrProviderChunk,
			})
			if err != nil {
				return fmt.Errorf("configure ASR cadence buffer: %w", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), config.stageTimeout)
		result, err := audiobench.RunASR(ctx, provider, audiobench.ASRConfig{
			CaseID: config.asrCase, InputSHA256: digest, ReferenceText: config.asrReference,
			Audio: audio, FrameDuration: config.asrFrame, Paced: config.asrPaced,
		})
		cancel()
		if err != nil {
			return err
		}
		report.ASR = &result
		report.Runtime["asr_transport"] = "qwen-asr-stateful-http"
		report.Runtime["asr_scheduler_frame"] = config.asrFrame.String()
		if config.asrProviderChunk > 0 {
			report.Runtime["asr_provider_chunk"] = config.asrProviderChunk.String()
		} else {
			report.Runtime["asr_provider_chunk"] = "direct"
		}
	}
	if strings.TrimSpace(config.ttsText) != "" {
		provider, transport, err := makeTTSProvider(config)
		if err != nil {
			return fmt.Errorf("configure TTS: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), config.stageTimeout)
		result, err := audiobench.RunTTS(ctx, provider, audiobench.TTSConfig{CaseID: config.ttsCase, Text: config.ttsText})
		cancel()
		if err != nil {
			return err
		}
		if strings.TrimSpace(config.ttsWAV) != "" {
			if err := os.MkdirAll(filepath.Dir(config.ttsWAV), 0o755); err != nil {
				return fmt.Errorf("create TTS WAV directory: %w", err)
			}
			digest, err := livebench.WriteWAV(config.ttsWAV, livebench.Audio{PCM16: result.OutputPCM16, SampleRateHz: result.OutputSampleRate})
			if err != nil {
				return fmt.Errorf("write TTS WAV: %w", err)
			}
			// The report's OutputSHA256 covers PCM; this value covers the WAV
			// container and is therefore intentionally distinct.
			report.Runtime["tts_wav_sha256"] = digest
		}
		report.TTS = &result
		report.Runtime["tts_transport"] = transport
		if config.ttsDeterministic {
			report.Runtime["tts_sampling_seed"] = fmt.Sprint(config.ttsSeed)
		}
	}
	if err := audiobench.WriteReport(config.output, report); err != nil {
		return err
	}
	summary := struct {
		Report             string   `json:"report"`
		ASRFinalTranscript string   `json:"asr_final_transcript,omitempty"`
		ASRFirstPartialMS  *float64 `json:"asr_first_partial_ms,omitempty"`
		ASREndToFinalMS    float64  `json:"asr_end_to_final_ms,omitempty"`
		TTSFirstAudioMS    float64  `json:"tts_first_audio_ms,omitempty"`
		TTSRealTimeFactor  float64  `json:"tts_real_time_factor,omitempty"`
	}{Report: config.output}
	if report.ASR != nil {
		summary.ASRFinalTranscript = report.ASR.FinalTranscript
		summary.ASRFirstPartialMS = report.ASR.FirstPartialWallMS
		summary.ASREndToFinalMS = report.ASR.EndToFinalMS
	}
	if report.TTS != nil {
		summary.TTSFirstAudioMS = report.TTS.FirstAudioMS
		summary.TTSRealTimeFactor = report.TTS.RealTimeFactor
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func makeTTSProvider(config options) (v1.StreamingSpeechProvider, string, error) {
	providerName := strings.ToLower(strings.TrimSpace(config.ttsProvider))
	switch providerName {
	case ttsProviderFishNative:
		if strings.TrimSpace(config.ttsReferencePath) != "" || strings.TrimSpace(config.ttsReferenceText) != "" {
			return nil, "", errors.New("--tts-reference-audio/text require --tts-provider openai-speech")
		}
		if config.ttsInitialFrames != -1 {
			return nil, "", errors.New("--tts-initial-codec-frames requires --tts-provider openai-speech")
		}
		var seed *int64
		if config.ttsDeterministic {
			value := config.ttsSeed
			seed = &value
		}
		sourceRate := config.ttsServerRate
		if sourceRate == 0 {
			sourceRate = 44_100
		}
		provider, err := fishaudio.New(fishaudio.Config{
			Endpoint: config.ttsURL, Model: config.ttsModel,
			BearerToken: os.Getenv("FISH_AUDIO_API_KEY"), RequestTimeout: config.requestTimeout,
			ServerSampleRateHz: uint32(sourceRate), OutputSampleRateHz: uint32(config.ttsOutputRate),
			ReferenceID: config.ttsReferenceID, Seed: seed,
		})
		return provider, "fish-speech-native-http", err
	case ttsProviderOpenAISpeech:
		if strings.TrimSpace(config.ttsReferenceID) != "" {
			return nil, "", errors.New("--tts-reference-id requires --tts-provider fish-native")
		}
		sourceRate := config.ttsServerRate
		if sourceRate == 0 {
			sourceRate = 24_000
		}
		var references []openaitts.Reference
		if strings.TrimSpace(config.ttsReferencePath) != "" || strings.TrimSpace(config.ttsReferenceText) != "" {
			references = []openaitts.Reference{{AudioPath: config.ttsReferencePath, Text: config.ttsReferenceText}}
		}
		var initialFrames *int
		if config.ttsInitialFrames >= 0 {
			value := config.ttsInitialFrames
			initialFrames = &value
		}
		var extraBody map[string]json.RawMessage
		if config.ttsDeterministic {
			extraBody = map[string]json.RawMessage{
				"seed": json.RawMessage(fmt.Sprint(config.ttsSeed)),
			}
		}
		provider, err := openaitts.New(openaitts.Config{
			Endpoint: config.ttsURL, Model: config.ttsModel, Voice: config.ttsVoice,
			BearerToken: os.Getenv("OPENAI_TTS_API_KEY"), RequestTimeout: config.requestTimeout,
			FallbackSampleRate: uint32(sourceRate), OutputSampleRateHz: uint32(config.ttsOutputRate),
			References: references, InitialChunkFrames: initialFrames, ExtraBody: extraBody,
		})
		return provider, "openai-compatible-speech-http", err
	default:
		return nil, "", fmt.Errorf("unsupported --tts-provider %q", config.ttsProvider)
	}
}
