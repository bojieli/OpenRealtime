// Package providers is the catalogue of model providers this server can be
// pointed at, and the one place that knows how to build one.
//
// An open implementation of the Realtime API is only as useful as the set of
// models it can actually run, so the three roles a voice stack needs -
// recognition, language, and synthesis - are each backed by a named list here
// rather than by a switch statement in the command. Adding a provider is a
// table entry plus, when its wire format is genuinely its own, an adapter.
//
// Two things are deliberately separated in every entry. The **endpoint,
// credential, and wire dialect** are durable facts about a provider: they
// change on the timescale of API versions. The **default model** is a hint
// that goes stale the week a vendor ships something new. So a default model is
// always overridable, `openrealtime providers` prints when the catalogue was
// last reviewed, and `openrealtime providers -probe` asks the provider itself
// what it serves rather than trusting this file.
package providers

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Reviewed records when the default models in this catalogue were last checked
// against the providers' own documentation. It is printed by the listing
// command so a reader can judge how much to trust a default they did not set.
const Reviewed = "2026-08-22"

// Role is the part of the voice stack a provider fills.
type Role string

const (
	RoleLLM      Role = "llm"
	RoleASR      Role = "asr"
	RoleTTS      Role = "tts"
	RoleVision   Role = "vision"
	RoleUpstream Role = "upstream"
)

// Dialect names the wire contract an adapter speaks.
//
// It is the field that decides which adapter serves an entry, and it is
// separate from the provider name because most providers speak a dialect
// someone else defined. That is the whole reason this catalogue can be long:
// one adapter covers thirty vendors, and the ones with their own protocol get
// their own adapter rather than a pile of special cases inside a shared one.
type Dialect string

const (
	// DialectOpenAIChat is /v1/chat/completions with SSE streaming.
	DialectOpenAIChat Dialect = "openai-chat"
	// DialectAnthropicMessages is /v1/messages with SSE streaming.
	DialectAnthropicMessages Dialect = "anthropic-messages"
	// DialectGemini is generativelanguage :streamGenerateContent.
	DialectGemini Dialect = "gemini-generate-content"

	// DialectOpenAIRealtime is the Realtime API over a WebSocket.
	DialectOpenAIRealtime Dialect = "openai-realtime"
	// DialectGeminiLive is BidiGenerateContent, which is not a Realtime
	// dialect and is translated into one.
	DialectGeminiLive Dialect = "gemini-live"
	// DialectGPTLive is OpenAI's v1/live/sessions protocol. It shares a vendor
	// with the Realtime API and not its event contract - it is full duplex and
	// reports no turn boundaries - so it is translated rather than aliased.
	DialectGPTLive Dialect = "gpt-live"

	// DialectOpenAITranscriptions is /v1/audio/transcriptions.
	DialectOpenAITranscriptions Dialect = "openai-transcriptions"
	// DialectElevenLabsSTT is ElevenLabs /v1/speech-to-text.
	DialectElevenLabsSTT Dialect = "elevenlabs-speech-to-text"
	// DialectDeepgramListen is the Deepgram streaming WebSocket.
	DialectDeepgramListen Dialect = "deepgram-listen"
	// DialectQwenASR is the local Qwen3-ASR start/chunk/finish service.
	DialectQwenASR Dialect = "qwen-asr"
	// DialectVLLMRealtime is vLLM's /v1/realtime transcription WebSocket,
	// which streams append-only text deltas from one running generation.
	DialectVLLMRealtime Dialect = "vllm-realtime"

	// DialectOpenAISpeech is /v1/audio/speech with a PCM stream.
	DialectOpenAISpeech Dialect = "openai-speech"
	// DialectSpeechSocket is openrealtime-incremental-speech/1, the
	// same-context synthesis WebSocket of the duplex-plan TTS services.
	DialectSpeechSocket Dialect = "speech-socket"
	// DialectFishNative is the Fish Speech /v1/tts server.
	DialectFishNative Dialect = "fish-native"
	// DialectPCMPost is a POST whose response body is the PCM stream, which
	// is what Deepgram, ElevenLabs, and Cartesia each do with their own field
	// names.
	DialectPCMPost Dialect = "pcm-post"
)

// Auth names how a credential reaches a provider.
type Auth string

const (
	// AuthBearer sends Authorization: Bearer.
	AuthBearer Auth = "bearer"
	// AuthToken sends Authorization: Token, which Deepgram uses.
	AuthToken Auth = "token"
	// AuthAnthropic sends x-api-key with the version header.
	AuthAnthropic Auth = "anthropic"
	// AuthXIKey sends xi-api-key, which ElevenLabs uses.
	AuthXIKey Auth = "xi-api-key"
	// AuthAPIKeyHeader sends X-API-Key, which Cartesia uses.
	AuthAPIKeyHeader Auth = "x-api-key"
	// AuthQuery sends the key as a query parameter, which Gemini's native
	// endpoint accepts.
	AuthQuery Auth = "query"
	// AuthNone is a trusted local endpoint that wants no credential.
	AuthNone Auth = "none"
)

// ErrUnknown reports a provider name that is not in the catalogue.
var ErrUnknown = errors.New("unknown provider")

// UnknownError names the provider that was not found and the role it was
// looked up for, because "unknown provider" on its own tells an operator
// nothing about which of four flags they mistyped.
type UnknownError struct {
	Role  Role
	Name  string
	Known []string
}

func (err *UnknownError) Error() string {
	return fmt.Sprintf("unknown %s provider %q; known: %s",
		err.Role, err.Name, strings.Join(err.Known, ", "))
}

func (err *UnknownError) Unwrap() error { return ErrUnknown }

// Common is the part of an entry that every role shares.
type Common struct {
	// Name is the value a flag takes.
	Name string
	// Aliases are other spellings that resolve to this entry. They exist
	// because a provider's product name and its company name are often
	// different, and an operator should not have to know which one this file
	// chose.
	Aliases []string
	// Label is the human name.
	Label string
	// Dialect selects the adapter.
	Dialect Dialect
	// BaseURL is the default endpoint. A local provider's is a loopback
	// address; a hosted one's is the vendor's.
	BaseURL string
	// Auth is how the credential is sent.
	Auth Auth
	// KeyEnv lists environment variables holding the credential, in
	// preference order. The first one that is set wins. Several entries carry
	// more than one because vendors have renamed their own variable.
	KeyEnv []string
	// Local marks an endpoint that runs on the operator's own machine, which
	// is what makes a missing credential fine rather than an error.
	Local bool
	// Notes is a one-line caveat printed by the listing command.
	Notes string
}

// keyFromEnvironment returns the first credential that is set.
func (common Common) keyFromEnvironment() string {
	for _, name := range common.KeyEnv {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

// CredentialEnv reports the variable a credential should be put in. It is the
// first of the accepted names, which is the one worth telling an operator
// about when none of them is set.
func (common Common) CredentialEnv() string {
	if len(common.KeyEnv) == 0 {
		return ""
	}
	return common.KeyEnv[0]
}

// matches reports whether a lookup name selects this entry.
func (common Common) matches(name string) bool {
	if strings.EqualFold(common.Name, name) {
		return true
	}
	for _, alias := range common.Aliases {
		if strings.EqualFold(alias, name) {
			return true
		}
	}
	return false
}

// normalise trims and lowercases a provider name from a flag.
func normalise(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// names returns the sorted catalogue names for an error message.
func names[T any](entries []T, common func(T) Common) []string {
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, common(entry).Name)
	}
	sort.Strings(result)
	return result
}
