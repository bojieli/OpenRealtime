package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/continuation"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	serveProviderConfigurationVersion = uint64(1)
	servePolicyConfigurationVersion   = uint64(2)
)

type serveASRConfiguration struct {
	FormatVersion     uint64 `json:"format_version"`
	Model             string `json:"model"`
	BaseURL           string `json:"base_url"`
	Language          string `json:"language"`
	PartialIntervalMS int64  `json:"partial_interval_ms"`
	EndpointingMS     int64  `json:"endpointing_ms"`
	RequestTimeoutMS  int64  `json:"request_timeout_ms"`
	CadenceMS         int64  `json:"cadence_ms"`
}

type serveModelConfiguration struct {
	FormatVersion    uint64   `json:"format_version"`
	Model            string   `json:"model"`
	BaseURL          string   `json:"base_url"`
	Effort           string   `json:"effort"`
	Vision           *bool    `json:"vision"`
	Reason           string   `json:"reason"`
	RetainReasoning  *bool    `json:"retain_reasoning"`
	Temperature      *float64 `json:"temperature"`
	RequestTimeoutMS int64    `json:"request_timeout_ms"`
	SpeechAuthority  string   `json:"speech_authority"`
}

type servePolicyConfiguration struct {
	FormatVersion    uint64 `json:"format_version"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	RequestTimeoutMS int64  `json:"request_timeout_ms"`
	Vision           *bool  `json:"vision"`
	GuidedChoice     *bool  `json:"guided_choice"`
	Reasoning        string `json:"reasoning"`
	TokenEnvironment string `json:"token_environment,omitempty"`
}

type serveTTSConfiguration struct {
	FormatVersion        uint64 `json:"format_version"`
	Model                string `json:"model"`
	BaseURL              string `json:"base_url"`
	Voice                string `json:"voice"`
	Language             string `json:"language"`
	OutputSampleRateHz   uint32 `json:"output_sample_rate_hz"`
	RequestTimeoutMS     int64  `json:"request_timeout_ms"`
	SentenceWrapping     *bool  `json:"sentence_wrapping"`
	SentenceMinimumRunes int    `json:"sentence_minimum_runes"`
}

func decodeServeASRConfiguration(
	provider string, source json.RawMessage,
) (serveASRConfiguration, providers.ASRRequest, error) {
	var config serveASRConfiguration
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, providers.ASRRequest{}, fmt.Errorf("decode ASR configuration: %w", err)
	}
	if config.FormatVersion != serveProviderConfigurationVersion {
		return config, providers.ASRRequest{}, fmt.Errorf("ASR configuration format is %d, want %d", config.FormatVersion, serveProviderConfigurationVersion)
	}
	if err := exactNonempty("ASR model", config.Model); err != nil {
		return config, providers.ASRRequest{}, err
	}
	if err := exactEndpoint("ASR base_url", config.BaseURL, true); err != nil {
		return config, providers.ASRRequest{}, err
	}
	if config.Language != strings.TrimSpace(config.Language) {
		return config, providers.ASRRequest{}, errors.New("ASR language is not canonical")
	}
	if err := boundedMilliseconds("ASR partial_interval_ms", config.PartialIntervalMS, true); err != nil {
		return config, providers.ASRRequest{}, err
	}
	if err := boundedMilliseconds("ASR endpointing_ms", config.EndpointingMS, true); err != nil {
		return config, providers.ASRRequest{}, err
	}
	if err := boundedMilliseconds("ASR request_timeout_ms", config.RequestTimeoutMS, false); err != nil {
		return config, providers.ASRRequest{}, err
	}
	if err := boundedMilliseconds("ASR cadence_ms", config.CadenceMS, false); err != nil {
		return config, providers.ASRRequest{}, err
	}
	return config, providers.ASRRequest{
		Provider: provider, Model: config.Model, BaseURL: config.BaseURL,
		Language:        config.Language,
		PartialInterval: time.Duration(config.PartialIntervalMS) * time.Millisecond,
		Endpointing:     time.Duration(config.EndpointingMS) * time.Millisecond,
		RequestTimeout:  time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}, nil
}

func decodeServeModelConfiguration(
	provider string, source json.RawMessage,
) (serveModelConfiguration, providers.LLMRequest, error) {
	var config serveModelConfiguration
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, providers.LLMRequest{}, fmt.Errorf("decode model configuration: %w", err)
	}
	if config.FormatVersion != serveProviderConfigurationVersion {
		return config, providers.LLMRequest{}, fmt.Errorf("model configuration format is %d, want %d", config.FormatVersion, serveProviderConfigurationVersion)
	}
	if err := exactNonempty("model name", config.Model); err != nil {
		return config, providers.LLMRequest{}, err
	}
	if err := exactEndpoint("model base_url", config.BaseURL, false); err != nil {
		return config, providers.LLMRequest{}, err
	}
	effort, err := continuation.ParseEffort(config.Effort)
	if err != nil || string(effort) != config.Effort {
		return config, providers.LLMRequest{}, fmt.Errorf("model effort is not canonical: %w", err)
	}
	if config.Vision == nil || config.RetainReasoning == nil {
		return config, providers.LLMRequest{}, errors.New("model vision and retain_reasoning must be explicit booleans")
	}
	reason := providers.Reason(config.Reason)
	if reason != providers.ReasonOff && reason != providers.ReasonOn && reason != providers.ReasonDefault {
		return config, providers.LLMRequest{}, errors.New("model reason must be off, on, or default")
	}
	if config.Temperature != nil && (*config.Temperature < 0 || *config.Temperature > 2) {
		return config, providers.LLMRequest{}, errors.New("model temperature must be between 0 and 2")
	}
	if err := boundedMilliseconds("model request_timeout_ms", config.RequestTimeoutMS, false); err != nil {
		return config, providers.LLMRequest{}, err
	}
	speechAuthority := continuation.SpeechAuthority(config.SpeechAuthority)
	if speechAuthority != continuation.SpeechAuthorityVoice && speechAuthority != continuation.SpeechAuthoritySilent {
		return config, providers.LLMRequest{}, errors.New("model speech_authority must be voice or silent")
	}
	vision, retain := *config.Vision, *config.RetainReasoning
	return config, providers.LLMRequest{
		Provider: provider, Model: config.Model, BaseURL: config.BaseURL,
		Phase: trajectory.PhaseFast, Effort: effort,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: speechAuthority,
		Reason:          reason, Vision: &vision, RetainReasoning: retain,
		Temperature:    cloneFloat(config.Temperature),
		RequestTimeout: time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}, nil
}

func decodeServePolicyConfiguration(
	provider string, source json.RawMessage,
) (servePolicyConfiguration, policyelements.SemanticDeciderDescriptor, error) {
	var config servePolicyConfiguration
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, fmt.Errorf("decode semantic policy configuration: %w", err)
	}
	if config.FormatVersion != servePolicyConfigurationVersion {
		return config, policyelements.SemanticDeciderDescriptor{}, fmt.Errorf(
			"semantic policy configuration format is %d, want %d", config.FormatVersion, servePolicyConfigurationVersion,
		)
	}
	if err := exactNonempty("semantic policy provider", provider); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	if err := exactNonempty("semantic policy model", config.Model); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	if err := exactEndpoint("semantic policy base_url", config.BaseURL, false); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	if err := boundedMilliseconds("semantic policy request_timeout_ms", config.RequestTimeoutMS, false); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	if config.GuidedChoice == nil {
		return config, policyelements.SemanticDeciderDescriptor{}, errors.New("semantic policy guided_choice must be an explicit boolean")
	}
	if config.Vision == nil {
		return config, policyelements.SemanticDeciderDescriptor{}, errors.New("semantic policy vision must be an explicit boolean")
	}
	reasoning := openaicompat.ReasoningControl(config.Reasoning)
	switch reasoning {
	case openaicompat.ReasoningControlNone, openaicompat.ReasoningControlEffort,
		openaicompat.ReasoningControlThinkingObject, openaicompat.ReasoningControlEnableThinking,
		openaicompat.ReasoningControlTemplateKwargs:
	default:
		return config, policyelements.SemanticDeciderDescriptor{}, errors.New("semantic policy reasoning control is not canonical")
	}
	if !optionalEnvironmentName(config.TokenEnvironment) {
		return config, policyelements.SemanticDeciderDescriptor{}, errors.New("semantic policy token_environment is not canonical")
	}
	payload, err := json.Marshal(struct {
		Provider string                   `json:"provider"`
		Config   servePolicyConfiguration `json:"configuration"`
	}{Provider: provider, Config: config})
	if err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	digest := sha256.Sum256(payload)
	descriptor := policyelements.SemanticDeciderDescriptor{
		Provider: provider, Model: config.Model, Protocol: "openai-chat-completions",
		Revision: "policymodel-client-v2", ConfigurationDigest: fmt.Sprintf("sha256:%x", digest[:]),
		DecisionTimeoutMS: config.RequestTimeoutMS, Vision: *config.Vision,
	}
	if err := descriptor.Validate(); err != nil {
		return config, policyelements.SemanticDeciderDescriptor{}, err
	}
	return config, descriptor, nil
}

func decodeServeTTSConfiguration(
	provider string, source json.RawMessage,
) (serveTTSConfiguration, providers.TTSRequest, error) {
	var config serveTTSConfiguration
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, providers.TTSRequest{}, fmt.Errorf("decode TTS configuration: %w", err)
	}
	if config.FormatVersion != serveProviderConfigurationVersion {
		return config, providers.TTSRequest{}, fmt.Errorf("TTS configuration format is %d, want %d", config.FormatVersion, serveProviderConfigurationVersion)
	}
	if err := exactNonempty("TTS model", config.Model); err != nil {
		return config, providers.TTSRequest{}, err
	}
	if err := exactEndpoint("TTS base_url", config.BaseURL, false); err != nil {
		return config, providers.TTSRequest{}, err
	}
	if err := exactNonempty("TTS voice", config.Voice); err != nil {
		return config, providers.TTSRequest{}, err
	}
	if config.Language != strings.TrimSpace(config.Language) {
		return config, providers.TTSRequest{}, errors.New("TTS language is not canonical")
	}
	if config.OutputSampleRateHz != 24_000 {
		return config, providers.TTSRequest{}, errors.New("TTS output_sample_rate_hz must be 24000 for the Realtime wire")
	}
	if err := boundedMilliseconds("TTS request_timeout_ms", config.RequestTimeoutMS, false); err != nil {
		return config, providers.TTSRequest{}, err
	}
	if config.SentenceWrapping == nil {
		return config, providers.TTSRequest{}, errors.New("TTS sentence_wrapping must be an explicit boolean")
	}
	if config.SentenceMinimumRunes < 1 || config.SentenceMinimumRunes > 4096 {
		return config, providers.TTSRequest{}, errors.New("TTS sentence_minimum_runes must be between 1 and 4096")
	}
	return config, providers.TTSRequest{
		Provider: provider, Model: config.Model, BaseURL: config.BaseURL,
		Voice: config.Voice, Language: config.Language,
		OutputSampleRateHz: config.OutputSampleRateHz,
		RequestTimeout:     time.Duration(config.RequestTimeoutMS) * time.Millisecond,
	}, nil
}

func exactNonempty(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s is empty or non-canonical", label)
	}
	return nil
}

func optionalEnvironmentName(value string) bool {
	if value == "" {
		return true
	}
	if value != strings.TrimSpace(value) || len(value) > 256 {
		return false
	}
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' ||
			index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func exactEndpoint(label, value string, websocket bool) error {
	if err := exactNonempty(label, value); err != nil {
		return err
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s is not an absolute endpoint", label)
	}
	allowed := parsed.Scheme == "http" || parsed.Scheme == "https"
	if websocket {
		allowed = allowed || parsed.Scheme == "ws" || parsed.Scheme == "wss"
	}
	if !allowed {
		return fmt.Errorf("%s has unsupported scheme %q", label, parsed.Scheme)
	}
	return nil
}

func boundedMilliseconds(label string, value int64, allowZero bool) error {
	if value < 0 || (!allowZero && value == 0) || value > int64(time.Hour/time.Millisecond) {
		if allowZero {
			return fmt.Errorf("%s must be between 0 and %d", label, time.Hour/time.Millisecond)
		}
		return fmt.Errorf("%s must be between 1 and %d", label, time.Hour/time.Millisecond)
	}
	return nil
}

func cloneFloat(source *float64) *float64 {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}
