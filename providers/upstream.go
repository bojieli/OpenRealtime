package providers

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/adapters/geminilive"
	"github.com/bojieli/OpenRealtime/adapters/gptlive"
	"github.com/bojieli/OpenRealtime/binding/upstream"
)

// Upstream is one remote realtime endpoint the upstream binding can run behind.
//
// This role is deliberately narrower than the others. It lists realtime *model*
// APIs - an endpoint that owns perception, a voice, and action, and lacks only a
// second model reasoning alongside it. It does not list agent platforms such as
// Deepgram's Voice Agent or ElevenLabs Agents: those already run their own agent
// loop, and putting this binding's reasoner behind one would be two
// orchestrators arguing over the same conversation rather than a seam.
type Upstream struct {
	Common
	// Model is the default realtime model.
	Model string
	// ModelQuery names the query parameter carrying the model. Empty selects
	// "model"; Azure addresses a deployment instead.
	ModelQuery string
	// EventAliases renames this endpoint's server events onto the ones the
	// mirror reads, which are OpenAI's current spellings.
	EventAliases map[string]string
	// Handoff is how a completed answer reaches this endpoint's voice.
	Handoff upstream.Handoff
	// AuthHeader overrides the Authorization: Bearer default.
	AuthHeader string
	// Verified records how far this entry has been checked against the real
	// endpoint, as opposed to against a fake built from its documentation.
	Verified Verification
}

// Verification is how much of an entry is known to be true.
//
// It exists because a fake server built from a vendor's documentation proves
// only that this code does what the documentation was read to say. That is
// worth having and it is not the same as working. Recording the difference in
// the table - rather than in a paragraph that rots - keeps the listing honest
// about which entries have actually been run.
//
// Raising a level requires a credential for that vendor and the probe command;
// nobody should raise one from reading.
type Verification string

const (
	// VerifiedLiveTurn means a real session completed a turn: the endpoint
	// heard something, answered, and its events arrived under the names this
	// catalogue expects.
	VerifiedLiveTurn Verification = "live-turn"
	// VerifiedReachable means the real endpoint answered on this URL and
	// evaluated a credential sent this way - so the address and the
	// authentication are right - but no turn has been run for want of a
	// working credential. The event names and the hand-off are still only as
	// good as the documentation.
	VerifiedReachable Verification = "reachable"
	// VerifiedDocumented means the entry is built from the vendor's
	// specification and has been run against a fake, and nothing else.
	VerifiedDocumented Verification = "documented"
)

// preGANames maps the Realtime protocol as it stood before OpenAI renamed its
// audio events at general availability.
//
// It is a rename table rather than an adapter because that rename is the whole
// difference: an endpoint built against the earlier specification sends the
// same fields under the earlier names, so the mirror needs the names changed
// and nothing else.
//
// The text pair is here for the same reason as the audio ones and was missing
// for a reason worth remembering: nothing downstream handled a text response,
// so there was no name for the table to rename it onto, and the gap was
// invisible for exactly as long as the capability was. A rename table ages
// with the thing it feeds.
var preGANames = map[string]string{
	"response.audio.delta":            "response.output_audio.delta",
	"response.audio.done":             "response.output_audio.done",
	"response.audio_transcript.delta": "response.output_audio_transcript.delta",
	"response.audio_transcript.done":  "response.output_audio_transcript.done",
	"response.text.delta":             "response.output_text.delta",
	"response.text.done":              "response.output_text.done",
}

// upstreamCatalog is the realtime-endpoint catalogue.
var upstreamCatalog = []Upstream{
	{
		Common: Common{
			Name: "openai", Label: "OpenAI Realtime", Dialect: DialectOpenAIRealtime,
			BaseURL: "wss://api.openai.com/v1/realtime", Auth: AuthBearer,
			KeyEnv: []string{"OPENAI_API_KEY"},
			Notes:  "The reference implementation: this project's mirror reads its current event names.",
		},
		Model: "gpt-realtime-2.1", Handoff: upstream.HandoffConversationItem,
		// The handshake succeeds and the credential is evaluated; the account
		// this was checked from has no credit, so no turn has been run.
		Verified: VerifiedReachable,
	},
	{
		Common: Common{
			Name: "openai-live", Aliases: []string{"gpt-live", "live"},
			Label: "OpenAI GPT-Live", Dialect: DialectGPTLive,
			BaseURL: gptlive.DefaultURL, Auth: AuthBearer,
			KeyEnv: []string{"OPENAI_API_KEY"},
			Notes: "Same vendor as Realtime, different protocol: full duplex, no turn " +
				"boundaries, and a delegation seam this binding's reasoner fills. " +
				"Translated into a Realtime dialect.",
		},
		Model: gptlive.DefaultModel,
		// Live has no conversation items and no writable session instruction.
		// The translator turns a hand-off into a commentary append against the
		// delegation the endpoint opened, which is the native equivalent and
		// the channel the vendor built for returning backend results - so the
		// strategy declared here is the portable one, as it is for Gemini.
		Handoff: upstream.HandoffConversationItem,
		// A real session completed a turn: the endpoint started the session,
		// accepted the hand-off as a commentary append, spoke it, and its
		// events arrived under the names this catalogue expects.
		//
		// Two things only the real endpoint could teach are in the adapter
		// because of that run, and both fail silently. Live runs on an audio
		// clock, so a session with no input frames arriving never injects an
		// append, never speaks and never reports an error; and its output is a
		// continuous carrier rather than a per-response burst, so an utterance
		// bounded by "audio stopped" never ends.
		Verified: VerifiedLiveTurn,
	},
	{
		Common: Common{
			Name: "xai", Aliases: []string{"grok"}, Label: "xAI Grok speech-to-speech",
			Dialect: DialectOpenAIRealtime, BaseURL: "wss://api.x.ai/v1/realtime",
			Auth: AuthBearer, KeyEnv: []string{"XAI_API_KEY"},
			Notes: "xAI documents compatibility with the Realtime API; it reports user transcripts " +
				"as .updated, which is aliased onto .completed here.",
		},
		Model: "grok-voice-latest", Handoff: upstream.HandoffConversationItem,
		// The endpoint answers on this URL and reads this credential; the key
		// available when it was checked was not valid.
		Verified: VerifiedReachable,
		EventAliases: map[string]string{
			"conversation.item.input_audio_transcription.updated": "conversation.item.input_audio_transcription.completed",
		},
	},
	{
		Common: Common{
			Name: "azure-openai", Aliases: []string{"azure"}, Label: "Azure OpenAI Realtime",
			Dialect: DialectOpenAIRealtime, Auth: AuthAPIKeyHeader,
			KeyEnv: []string{"AZURE_OPENAI_API_KEY"},
			Notes: "Follows the OpenAI specification. Set the full URL, " +
				"wss://RESOURCE.openai.azure.com/openai/realtime?api-version=...&deployment=NAME.",
		},
		Handoff: upstream.HandoffConversationItem, ModelQuery: "deployment", AuthHeader: "api-key",
		// Azure has no endpoint without a resource name, so there is nothing
		// to reach without an account.
		Verified: VerifiedDocumented,
	},
	{
		Common: Common{
			Name: "qwen-omni", Aliases: []string{"dashscope", "qwen"},
			Label: "Alibaba Qwen-Omni-Realtime", Dialect: DialectOpenAIRealtime,
			BaseURL: "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime",
			Auth:    AuthBearer, KeyEnv: []string{"DASHSCOPE_API_KEY"},
			Notes: "Implements the pre-GA event names, and reserves conversation items for tool " +
				"results - so the answer is handed over in the session instruction. " +
				"Mainland-China accounts use wss://dashscope.aliyuncs.com/api-ws/v1/realtime.",
		},
		Model: "qwen3.5-omni-flash-realtime", Handoff: upstream.HandoffSessionInstruction,
		// Both regional endpoints answer on this path and read this
		// credential; no key was available to run a turn.
		Verified:     VerifiedReachable,
		EventAliases: preGANames,
	},
	{
		Common: Common{
			Name: "google", Aliases: []string{"gemini", "gemini-live"}, Label: "Google Gemini Live",
			Dialect: DialectGeminiLive,
			BaseURL: "wss://generativelanguage.googleapis.com/ws/" +
				"google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
			Auth: AuthQuery, KeyEnv: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"},
			Notes: "Not a Realtime dialect at all - BidiGenerateContent is translated into one.",
		},
		Model: geminilive.DefaultModel,
		// Gemini has no conversation items and no session-instruction
		// rewrite. The translator turns a handoff into a client turn, which is
		// the native equivalent, so the strategy declared here is the portable
		// one and the translator is what makes it mean something.
		Handoff: upstream.HandoffConversationItem,
		// The only entry here that has actually held a conversation.
		Verified: VerifiedLiveTurn,
	},
}

// Upstreams returns the catalogue in listing order.
func Upstreams() []Upstream { return slices.Clone(upstreamCatalog) }

// LookupUpstream resolves a realtime endpoint by name or alias.
func LookupUpstream(name string) (Upstream, error) {
	normalised := normalise(name)
	for _, entry := range upstreamCatalog {
		if entry.matches(normalised) {
			return entry, nil
		}
	}
	return Upstream{}, &UnknownError{
		Role: RoleUpstream, Name: name,
		Known: names(upstreamCatalog, func(entry Upstream) Common { return entry.Common }),
	}
}

// UpstreamRequest is one resolved realtime-endpoint construction.
type UpstreamRequest struct {
	Provider string
	Model    string
	URL      string
	APIKey   string
	KeyEnv   string
	Header   http.Header
}

// UpstreamSettings is what the binding needs to reach an endpoint.
type UpstreamSettings struct {
	Dialect      Dialect
	URL          string
	Model        string
	Token        string
	Header       http.Header
	EventAliases map[string]string
	Handoff      upstream.Handoff
	// Dial is set for an endpoint whose protocol has to be translated. Nil
	// means the endpoint speaks the Realtime protocol itself.
	Dial upstream.Dialer
}

// gptLiveDialer translates OpenAI's Live API into the Realtime protocol.
func gptLiveDialer(ctx context.Context, config upstream.Config) (upstream.RemoteConn, error) {
	return gptlive.Dial(ctx, gptlive.Config{
		URL: config.URL, APIKey: config.Token, Model: config.Model, Header: config.Header,
		Store: config.Store, SessionFormat: config.AudioFormat,
	})
}

// geminiDialer translates Google's Live API into the Realtime protocol.
func geminiDialer(ctx context.Context, config upstream.Config) (upstream.RemoteConn, error) {
	return geminilive.Dial(ctx, geminilive.Config{
		URL: config.URL, APIKey: config.Token, Model: config.Model, Header: config.Header,
	})
}

// ResolveUpstream turns a provider name into everything the binding needs.
//
// It returns settings rather than a connection because the binding dials at
// session start, not at configuration time - a server that could not start
// until a remote answered would be a server that a remote outage stops from
// booting.
func ResolveUpstream(request UpstreamRequest) (UpstreamSettings, error) {
	entry, err := LookupUpstream(request.Provider)
	if err != nil {
		return UpstreamSettings{}, err
	}
	endpoint := strings.TrimSpace(request.URL)
	if endpoint == "" {
		endpoint = entry.BaseURL
	}
	if endpoint == "" {
		return UpstreamSettings{}, fmt.Errorf(
			"realtime endpoint %q has no default URL; pass one explicitly", entry.Name)
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = entry.Model
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return UpstreamSettings{}, fmt.Errorf("realtime endpoint %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}

	header := request.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	settings := UpstreamSettings{
		Dialect: entry.Dialect, URL: endpoint, Model: model, Token: key,
		Header: header, EventAliases: maps.Clone(entry.EventAliases), Handoff: entry.Handoff,
	}
	switch entry.Dialect {
	case DialectGeminiLive:
		settings.Dial = geminiDialer
	case DialectGPTLive:
		settings.Dial = gptLiveDialer
	}
	// A vendor that reads the credential from somewhere other than the
	// bearer header gets it put there instead, and the token is cleared so the
	// client does not send both.
	if entry.AuthHeader != "" && !strings.EqualFold(entry.AuthHeader, "Authorization") {
		header.Set(entry.AuthHeader, key)
		settings.Token = ""
	}
	// The model reaches an OpenAI-shaped endpoint as a query parameter, and
	// the client appends it. Azure names that parameter differently, so it is
	// appended here and the client is given nothing to append.
	if entry.ModelQuery != "" && entry.ModelQuery != "model" {
		if model != "" && !strings.Contains(endpoint, entry.ModelQuery+"=") {
			separator := "?"
			if strings.Contains(endpoint, "?") {
				separator = "&"
			}
			settings.URL = endpoint + separator + entry.ModelQuery + "=" + model
		}
		settings.Model = ""
	}
	return settings, nil
}
