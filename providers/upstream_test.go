package providers_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/providers"
)

func TestEveryRealtimeEndpointResolvesAndNamesItsCredential(t *testing.T) {
	t.Parallel()
	for _, entry := range providers.Upstreams() {
		for _, name := range append([]string{entry.Name}, entry.Aliases...) {
			resolved, err := providers.LookupUpstream(name)
			if err != nil {
				t.Errorf("upstream %q: %v", name, err)
				continue
			}
			if resolved.Name != entry.Name {
				t.Errorf("upstream %q resolved to %q", name, resolved.Name)
			}
		}
		if entry.CredentialEnv() == "" {
			t.Errorf("upstream %q names no credential variable", entry.Name)
		}
		if entry.BaseURL == "" {
			continue
		}
		parsed, err := url.Parse(entry.BaseURL)
		if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
			t.Errorf("upstream %q endpoint is not a WebSocket URL: %q", entry.Name, entry.BaseURL)
		}
	}
}

// Every endpoint has to declare a hand-off channel it actually accepts. An
// endpoint with no way to be given the reasoner's answer cannot host this
// binding at all, and that is the one thing the binding is for.
func TestEveryRealtimeEndpointDeclaresAHandoff(t *testing.T) {
	t.Parallel()
	for _, entry := range providers.Upstreams() {
		switch entry.Handoff {
		case upstream.HandoffConversationItem, upstream.HandoffSessionInstruction:
		default:
			t.Errorf("upstream %q declares no usable hand-off: %q", entry.Name, entry.Handoff)
		}
	}
}

// Azure follows the OpenAI specification but reaches it differently: the
// credential is a header of its own and the model is a deployment in the URL.
func TestAzurePutsItsCredentialInAHeaderAndItsDeploymentInTheURL(t *testing.T) {
	t.Setenv("AZURE_OPENAI_API_KEY", "azure-secret")
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{
		Provider: "azure",
		URL:      "wss://example.openai.azure.com/openai/realtime?api-version=2026-05-07",
		Model:    "my-realtime-deployment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Header.Get("api-key") != "azure-secret" {
		t.Fatalf("Azure reads the credential from api-key: %v", settings.Header)
	}
	if settings.Token != "" {
		t.Fatal("the credential must not also be sent as a bearer token")
	}
	if !strings.Contains(settings.URL, "deployment=my-realtime-deployment") {
		t.Fatalf("the deployment must reach the URL: %q", settings.URL)
	}
	if !strings.Contains(settings.URL, "api-version=2026-05-07") {
		t.Fatalf("the caller's api-version must survive: %q", settings.URL)
	}
	// The client appends ?model= when it is given a model. Azure has no such
	// parameter, so it must be given none.
	if settings.Model != "" {
		t.Fatalf("Azure addresses a deployment, not a model: %q", settings.Model)
	}
	if _, err := providers.ResolveUpstream(providers.UpstreamRequest{Provider: "azure"}); err == nil {
		t.Fatal("Azure has no default endpoint and must say so")
	}
}

// The bearer endpoints keep the ordinary path.
func TestBearerEndpointsSendTheirCredentialAsABearerToken(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "openai-secret")
	settings, err := providers.ResolveUpstream(providers.UpstreamRequest{Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if settings.Token != "openai-secret" || settings.Model == "" {
		t.Fatalf("settings: %+v", settings)
	}
	if !strings.HasPrefix(settings.URL, "wss://api.openai.com/") {
		t.Fatalf("endpoint: %q", settings.URL)
	}
}

// The alias table exists so an endpoint whose only deviation is a spelling
// costs a table entry rather than an adapter. If it were empty for Qwen, its
// audio would silently never arrive.
func TestQwenRenamesThePreGAAudioEvents(t *testing.T) {
	t.Parallel()
	entry, err := providers.LookupUpstream("qwen-omni")
	if err != nil {
		t.Fatal(err)
	}
	for from, to := range map[string]string{
		"response.audio.delta":            "response.output_audio.delta",
		"response.audio_transcript.delta": "response.output_audio_transcript.delta",
		"response.audio_transcript.done":  "response.output_audio_transcript.done",
		// The text pair renames for the same reason and was missing for a
		// reason worth keeping a test about: nothing downstream handled a text
		// response, so there was no name to rename onto, and the gap stayed
		// invisible for exactly as long as the capability did. A rename table
		// ages with the thing it feeds.
		"response.text.delta": "response.output_text.delta",
		"response.text.done":  "response.output_text.done",
	} {
		if entry.EventAliases[from] != to {
			t.Errorf("alias %q = %q, want %q", from, entry.EventAliases[from], to)
		}
	}
	if entry.Handoff != upstream.HandoffSessionInstruction {
		t.Errorf("Qwen reserves conversation items for tool results: %q", entry.Handoff)
	}
}

func TestAnUnknownRealtimeEndpointNamesTheOnesThatExist(t *testing.T) {
	t.Parallel()
	_, err := providers.LookupUpstream("not-a-realtime-vendor")
	if err == nil || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("an unknown endpoint must list the known ones, got %v", err)
	}
}

// The verification column is a claim about evidence, so it has to be a value
// that means something and it has to be justified by what the entry carries.
//
// The specific rule worth enforcing: an entry claiming a live turn must have a
// default endpoint and model, because a turn cannot have been run without
// both. Nothing here can prove a probe was actually run - only a person with a
// credential can raise a level - but a claim that is impossible on its face
// should not survive review.
func TestTheVerificationClaimIsAValueAndIsNotImpossible(t *testing.T) {
	t.Parallel()
	for _, entry := range providers.Upstreams() {
		switch entry.Verified {
		case providers.VerifiedLiveTurn:
			if entry.BaseURL == "" || entry.Model == "" {
				t.Errorf("upstream %q claims a live turn but has no default %s",
					entry.Name, missingPiece(entry))
			}
		case providers.VerifiedReachable:
			if entry.BaseURL == "" {
				t.Errorf("upstream %q claims to be reachable but has no endpoint to reach",
					entry.Name)
			}
		case providers.VerifiedDocumented:
		default:
			t.Errorf("upstream %q has no verification level: %q", entry.Name, entry.Verified)
		}
	}
}

func missingPiece(entry providers.Upstream) string {
	if entry.BaseURL == "" {
		return "endpoint"
	}
	return "model"
}
