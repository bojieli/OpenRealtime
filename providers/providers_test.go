package providers_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/providers"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A catalogue is a table, and a table is where a typo lives forever. These
// tests are the reason a reader can trust an entry they have never used.

func TestEveryNameAndAliasIsUniqueAcrossACatalogue(t *testing.T) {
	t.Parallel()
	check := func(role string, entries []providers.Common) {
		seen := make(map[string]string)
		for _, entry := range entries {
			for _, name := range append([]string{entry.Name}, entry.Aliases...) {
				lowered := strings.ToLower(name)
				if previous, duplicate := seen[lowered]; duplicate {
					t.Errorf("%s: %q resolves to both %q and %q", role, name, previous, entry.Name)
				}
				seen[lowered] = entry.Name
			}
		}
	}
	check("llm", commons(providers.LLMs(), func(entry providers.LLM) providers.Common { return entry.Common }))
	check("asr", commons(providers.ASRs(), func(entry providers.ASR) providers.Common { return entry.Common }))
	check("tts", commons(providers.TTSs(), func(entry providers.TTS) providers.Common { return entry.Common }))
}

func TestEveryEntryResolvesByItsOwnNameAndAliases(t *testing.T) {
	t.Parallel()
	for _, entry := range providers.LLMs() {
		for _, name := range append([]string{entry.Name, strings.ToUpper(entry.Name)}, entry.Aliases...) {
			resolved, err := providers.LookupLLM(name)
			if err != nil {
				t.Errorf("llm %q: %v", name, err)
				continue
			}
			if resolved.Name != entry.Name {
				t.Errorf("llm %q resolved to %q", name, resolved.Name)
			}
		}
	}
	for _, entry := range providers.ASRs() {
		for _, name := range append([]string{entry.Name}, entry.Aliases...) {
			if _, err := providers.LookupASR(name); err != nil {
				t.Errorf("asr %q: %v", name, err)
			}
		}
	}
	for _, entry := range providers.TTSs() {
		for _, name := range append([]string{entry.Name}, entry.Aliases...) {
			if _, err := providers.LookupTTS(name); err != nil {
				t.Errorf("tts %q: %v", name, err)
			}
		}
	}
}

// An endpoint that does not parse is a provider nobody can reach, and it would
// only be discovered on the first session that selected it.
func TestEveryDefaultEndpointIsAbsolute(t *testing.T) {
	t.Parallel()
	check := func(role, name, endpoint string) {
		if endpoint == "" {
			return
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			t.Errorf("%s %q has an unusable endpoint %q", role, name, endpoint)
		}
	}
	for _, entry := range providers.LLMs() {
		check("llm", entry.Name, entry.BaseURL)
	}
	for _, entry := range providers.ASRs() {
		check("asr", entry.Name, entry.BaseURL)
	}
	for _, entry := range providers.TTSs() {
		check("tts", entry.Name, entry.BaseURL)
	}
}

// Only the escape hatches and Azure may lack an endpoint, and only a
// marketplace may lack a default model. Both exceptions are deliberate, so
// they are named here rather than being whatever the table happens to hold.
func TestOnlyNamedEntriesOmitAnEndpoint(t *testing.T) {
	t.Parallel()
	allowed := map[string]bool{"azure-openai": true}
	for _, entry := range providers.LLMs() {
		if entry.BaseURL == "" && !allowed[entry.Name] {
			t.Errorf("llm %q has no endpoint and is not one of the entries allowed to omit one", entry.Name)
		}
	}
}

func TestEveryCredentialedEntryNamesAnEnvironmentVariable(t *testing.T) {
	t.Parallel()
	check := func(role string, entry providers.Common) {
		if entry.Auth == providers.AuthNone {
			return
		}
		if entry.CredentialEnv() == "" {
			t.Errorf("%s %q authenticates but names no environment variable", role, entry.Name)
		}
	}
	for _, entry := range providers.LLMs() {
		check("llm", entry.Common)
	}
	for _, entry := range providers.ASRs() {
		check("asr", entry.Common)
	}
	for _, entry := range providers.TTSs() {
		check("tts", entry.Common)
	}
}

// Every entry has to reach an adapter. A dialect with no case in the switch
// would parse, list, and then fail on the first session.
func TestEveryEntryBuildsAnAdapter(t *testing.T) {
	t.Parallel()
	for _, entry := range providers.LLMs() {
		for _, phase := range []trajectory.Phase{trajectory.PhaseFast, trajectory.PhaseSlow} {
			request := providers.LLMRequest{
				Provider: entry.Name, Phase: phase, APIKey: "test-key",
				Model: "test-model", BaseURL: "https://example.invalid/v1",
				Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
				SpeechAuthority: continuation.SpeechAuthoritySilent, Reason: providers.ReasonOn,
			}
			if phase == trajectory.PhaseFast {
				request.Effort = continuation.EffortMinimal
				request.ToolAuthority = continuation.ToolAuthorityPropose
				request.SpeechAuthority = continuation.SpeechAuthorityVoice
				request.Reason = providers.ReasonOff
			}
			provider, err := providers.NewLLM(request)
			if err != nil {
				t.Errorf("llm %q %s: %v", entry.Name, phase, err)
				continue
			}
			descriptor := provider.Descriptor()
			if descriptor.Phase != phase || descriptor.Model != "test-model" {
				t.Errorf("llm %q built the wrong descriptor: %+v", entry.Name, descriptor)
			}
			if phase == trajectory.PhaseFast &&
				descriptor.EffectiveToolAuthority() == continuation.ToolAuthorityExecute {
				t.Errorf("llm %q gave the voice executable tools", entry.Name)
			}
		}
	}
	for _, entry := range providers.ASRs() {
		factory, err := providers.NewASRFactory(providers.ASRRequest{
			Provider: entry.Name, APIKey: "test-key", Model: "test-model",
		})
		if err != nil {
			t.Errorf("asr %q: %v", entry.Name, err)
			continue
		}
		if _, err := factory(); err != nil {
			t.Errorf("asr %q factory: %v", entry.Name, err)
		}
	}
	for _, entry := range providers.TTSs() {
		// A voice is only supplied where the vendor takes one. Deepgram names
		// the voice inside the model and refuses a separate one, which is a
		// property worth preserving rather than working around.
		request := providers.TTSRequest{Provider: entry.Name, APIKey: "test-key"}
		if entry.VoiceRequired {
			request.Voice = "test-voice"
		}
		if _, err := providers.NewTTS(request); err != nil {
			t.Errorf("tts %q: %v", entry.Name, err)
		}
	}
}

// A provider with no default model must say so rather than sending a request
// to a model name that does not exist.
func TestAProviderWithNoDefaultModelRefusesRatherThanGuessing(t *testing.T) {
	t.Parallel()
	_, err := providers.NewLLM(providers.LLMRequest{
		Provider: "groq", Phase: trajectory.PhaseSlow, APIKey: "test-key",
		Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	})
	if err == nil || !strings.Contains(err.Error(), "no default") {
		t.Fatalf("a marketplace with no default model must refuse, got %v", err)
	}
}

func TestAnUnknownProviderNamesTheOnesThatExist(t *testing.T) {
	t.Parallel()
	_, err := providers.LookupLLM("definitely-not-a-provider")
	if err == nil {
		t.Fatal("an unknown provider must be refused")
	}
	if !errors.Is(err, providers.ErrUnknown) {
		t.Fatalf("error does not identify as unknown: %v", err)
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Fatalf("an unknown provider must list the known ones, got %q", err)
	}
}

// A missing credential is the most common configuration mistake, so the error
// has to name the variable that would have fixed it.
func TestAMissingCredentialNamesTheVariable(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := providers.NewLLM(providers.LLMRequest{
		Provider: "anthropic", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("a missing credential must name the variable, got %v", err)
	}
}

// A local provider is the one thing that must work with nothing configured,
// because it is what the server starts as.
func TestALocalProviderNeedsNoCredential(t *testing.T) {
	t.Parallel()
	if _, err := providers.NewLLM(providers.LLMRequest{
		Provider: "vllm", Phase: trajectory.PhaseFast, Model: "local",
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice, Reason: providers.ReasonOff,
	}); err != nil {
		t.Fatalf("a local provider must run without a credential: %v", err)
	}
}

// A voice that cannot be chosen is a provider that cannot be used, and the
// failure would otherwise be a 404 from the vendor's own router.
func TestAVoiceRequiringProviderRefusesWithoutOne(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"elevenlabs", "cartesia"} {
		if _, err := providers.NewTTS(providers.TTSRequest{
			Provider: name, APIKey: "test-key", Voice: "default",
		}); err == nil {
			t.Errorf("%s must refuse the placeholder voice", name)
		}
	}
}

// Whether a recogniser streams decides whether the stable-partial observation
// policy has anything to observe, so it is asserted rather than assumed.
func TestStreamingRecognisersAreMarkedAsSuch(t *testing.T) {
	t.Parallel()
	streaming := map[string]bool{}
	for _, entry := range providers.ASRs() {
		streaming[entry.Name] = entry.Streaming
	}
	for _, name := range []string{"qwen-asr", "deepgram"} {
		if !streaming[name] {
			t.Errorf("%s streams and must be marked as streaming", name)
		}
	}
	for _, name := range []string{"openai", "groq", "elevenlabs", "sensevoice"} {
		if streaming[name] {
			t.Errorf("%s is a batch endpoint and must not claim to stream", name)
		}
	}
}

// SenseVoice is the self-hosted recogniser a long conversation can afford.
//
// It is non-autoregressive, so recognising a growing utterance costs what the
// audio costs rather than what the transcript costs. That property is the
// whole reason the entry exists, and it survives only while the entry stays
// local, unauthenticated, and on the transcription route - three things a
// later edit could each undo without looking like it had.
func TestSenseVoiceIsALocalRecogniserOnTheTranscriptionRoute(t *testing.T) {
	t.Parallel()
	var entry providers.ASR
	for _, candidate := range providers.ASRs() {
		if candidate.Name == "sensevoice" {
			entry = candidate
		}
	}
	if entry.Name == "" {
		t.Fatal("the catalogue has no sensevoice recogniser")
	}
	if !entry.Local {
		t.Error("sensevoice runs on the operator's own machine and must be marked local")
	}
	if entry.Auth != providers.AuthNone {
		t.Errorf("a local recogniser must need no credential, got %v", entry.Auth)
	}
	if entry.Dialect != providers.DialectOpenAITranscriptions {
		t.Errorf("deploy/sensevoice serves the OpenAI transcription route, got %v", entry.Dialect)
	}
	if entry.Model == "" {
		t.Error("sensevoice names a default model")
	}
}

func commons[T any](entries []T, project func(T) providers.Common) []providers.Common {
	result := make([]providers.Common, 0, len(entries))
	for _, entry := range entries {
		result = append(result, project(entry))
	}
	return result
}

// An entry whose default endpoint is on the operator's own machine has to run
// with nothing configured. `openrealtime serve` with no flags selects three of
// them, and it is the command the project leads with.
func TestALoopbackDefaultNeedsNoCredential(t *testing.T) {
	t.Parallel()
	check := func(role, name, endpoint string, local bool) {
		if endpoint == "" || local {
			return
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return
		}
		host := parsed.Hostname()
		if host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1" {
			t.Errorf("%s %q defaults to %s and must not require a credential", role, name, endpoint)
		}
	}
	for _, entry := range providers.LLMs() {
		check("llm", entry.Name, entry.BaseURL, entry.Local)
	}
	for _, entry := range providers.ASRs() {
		check("asr", entry.Name, entry.BaseURL, entry.Local)
	}
	for _, entry := range providers.TTSs() {
		check("tts", entry.Name, entry.BaseURL, entry.Local)
	}
}
