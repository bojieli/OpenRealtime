package providers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/providers"
)

// listing serves a model list in whichever envelope a dialect uses.
func listing(t *testing.T, payload any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(payload)
		}))
	t.Cleanup(server.Close)
	return server
}

// The defaults in this catalogue are the one part of it guaranteed to go
// stale, and a stale one is invisible in ordinary use: the voice keeps
// answering while the background reasoner returns 404 every turn. So the
// probe checks them, and this is the check that would have caught the
// gemini-3.5-pro that never existed.
func TestProbeReportsADefaultTheProviderDoesNotServe(t *testing.T) {
	server := listing(t, map[string]any{
		"models": []map[string]string{
			{"name": "models/gemini-3.5-flash"},
			{"name": "models/gemini-3.5-flash-lite"},
		},
	})
	result, err := providers.Probe(t.Context(), providers.LLMRequest{
		Provider: "google", BaseURL: server.URL, APIKey: "test",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(result.Defaults) != 2 {
		t.Fatalf("both phases are checked, got %d", len(result.Defaults))
	}
	for _, declared := range result.Defaults {
		if !declared.Served {
			t.Fatalf("%s=%s is served and must be reported so", declared.Phase, declared.Model)
		}
	}
	if stale := result.Stale(); len(stale) != 0 {
		t.Fatalf("nothing is stale here: %+v", stale)
	}
}

func TestProbeNamesTheStaleDefault(t *testing.T) {
	// A provider that serves everything except what the catalogue named.
	server := listing(t, map[string]any{
		"models": []map[string]string{{"name": "models/gemini-9-ultra"}},
	})
	result, err := providers.Probe(t.Context(), providers.LLMRequest{
		Provider: "google", BaseURL: server.URL, APIKey: "test",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	stale := result.Stale()
	if len(stale) != 2 {
		t.Fatalf("both declared defaults are missing, got %d", len(stale))
	}
	for _, declared := range stale {
		if declared.Served {
			t.Fatalf("%+v is not served", declared)
		}
	}
}

// One namespace arrives two ways: Gemini's OpenAI-compatibility layer answers
// with native names inside an OpenAI envelope. Reading them as different names
// reported every Gemini default as missing, which is a false alarm and would
// have trained a reader to ignore the check.
func TestAModelNamespaceIsOneNamespaceWhicheverEnvelopeCarriesIt(t *testing.T) {
	server := listing(t, map[string]any{
		"data": []map[string]string{
			{"id": "models/gemini-3.5-flash"},
			{"id": "models/gemini-3.5-flash-lite"},
		},
	})
	result, err := providers.Probe(t.Context(), providers.LLMRequest{
		Provider: "google-openai", BaseURL: server.URL, APIKey: "test",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if stale := result.Stale(); len(stale) != 0 {
		t.Fatalf("the prefix is an envelope detail, not a different model: %+v", stale)
	}
	for _, model := range result.Models {
		if strings.HasPrefix(model, "models/") {
			t.Fatalf("the listing must report bare names, got %q", model)
		}
	}
}

// A marketplace names no default, because the caller names vendor/model. It
// has nothing to check and must not invent something to fail on.
func TestAProviderWithNoDeclaredDefaultsChecksNothing(t *testing.T) {
	server := listing(t, map[string]any{
		"data": []map[string]string{{"id": "anthropic/claude-opus-5"}},
	})
	result, err := providers.Probe(t.Context(), providers.LLMRequest{
		Provider: "openrouter", BaseURL: server.URL, APIKey: "test",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(result.Defaults) != 0 || len(result.Stale()) != 0 {
		t.Fatalf("nothing was declared, so nothing is checked: %+v", result.Defaults)
	}
}
