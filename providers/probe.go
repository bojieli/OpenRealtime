package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ProbeResult is what a provider says it serves, and whether the catalogue's
// own defaults are among them.
type ProbeResult struct {
	Provider string
	Endpoint string
	Models   []string
	// Defaults is what this entry names for each phase, checked against the
	// listing above. A default that is not served is the failure this field
	// exists to make visible: it is invisible in ordinary use, because the
	// voice keeps answering while the background reasoner returns 404 on every
	// turn, and it is the one part of the catalogue guaranteed to go stale.
	Defaults []DefaultModel
}

// DefaultModel is one phase's declared model and whether the provider serves it.
type DefaultModel struct {
	Phase  string
	Model  string
	Served bool
}

// Stale returns the declared defaults this provider does not serve.
func (result ProbeResult) Stale() []DefaultModel {
	var stale []DefaultModel
	for _, declared := range result.Defaults {
		if !declared.Served {
			stale = append(stale, declared)
		}
	}
	return stale
}

// Probe asks a provider to list its models, and checks this catalogue's own
// defaults against the answer.
//
// It exists because the default models here are the one part of the catalogue
// that goes stale, and a reader with a key can get the truth in a second
// rather than trusting a constant compiled months ago. Checking the defaults
// is the same argument turned on ourselves: a compatibility claim that is not
// continuously checked decays, and so does a model name. It is a listing, not
// a health check: a provider that answers here may still refuse a completion.
func Probe(ctx context.Context, request LLMRequest) (ProbeResult, error) {
	entry, err := LookupLLM(request.Provider)
	if err != nil {
		return ProbeResult{}, err
	}
	baseURL := strings.TrimSpace(request.BaseURL)
	if baseURL == "" {
		baseURL = entry.BaseURL
	}
	if baseURL == "" {
		return ProbeResult{}, fmt.Errorf("provider %q has no default endpoint; pass a base URL", entry.Name)
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return ProbeResult{}, fmt.Errorf("probing %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}

	var endpoint string
	switch entry.Dialect {
	case DialectOpenAIChat:
		endpoint = strings.TrimRight(baseURL, "/") + "/models"
	case DialectAnthropicMessages:
		endpoint = strings.TrimRight(baseURL, "/") + "/models"
	case DialectGemini:
		endpoint = strings.TrimRight(baseURL, "/") + "/models"
	default:
		return ProbeResult{}, fmt.Errorf("provider %q does not expose a model listing", entry.Name)
	}

	timeout := request.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(probeContext, http.MethodGet, endpoint, nil)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("create model listing request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/json")
	switch entry.Auth {
	case AuthAnthropic:
		httpRequest.Header.Set("x-api-key", key)
		httpRequest.Header.Set("anthropic-version", "2023-06-01")
	case AuthQuery:
		httpRequest.Header.Set("x-goog-api-key", key)
	case AuthNone:
	default:
		if key != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+key)
		}
	}
	for name, value := range entry.Headers {
		httpRequest.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("list %s models: %w", entry.Name, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return ProbeResult{}, fmt.Errorf("read %s model listing: %w", entry.Name, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ProbeResult{}, fmt.Errorf("%s returned HTTP %d: %s",
			entry.Name, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	models, err := decodeModelListing(payload)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("decode %s model listing: %w", entry.Name, err)
	}
	sort.Strings(models)
	return ProbeResult{
		Provider: entry.Name, Endpoint: endpoint, Models: models,
		Defaults: entry.declaredDefaults(models),
	}, nil
}

// declaredDefaults checks what this entry names against what is served.
//
// A name is matched exactly. A listing that is close but not equal - a dated
// snapshot, a preview suffix - is not the model the catalogue named, and
// treating it as one is how a default that stopped existing keeps looking
// fine.
func (entry LLM) declaredDefaults(models []string) []DefaultModel {
	served := make(map[string]struct{}, len(models))
	for _, model := range models {
		served[model] = struct{}{}
	}
	var defaults []DefaultModel
	for _, declared := range []DefaultModel{
		{Phase: "fast", Model: entry.FastModel},
		{Phase: "slow", Model: entry.SlowModel},
	} {
		if strings.TrimSpace(declared.Model) == "" {
			continue
		}
		_, declared.Served = served[declared.Model]
		defaults = append(defaults, declared)
	}
	return defaults
}

// decodeModelListing reads the three shapes a listing arrives in: OpenAI's
// `data[].id`, Anthropic's `data[].id`, and Gemini's `models[].name`.
func decodeModelListing(payload []byte) ([]string, error) {
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, err
	}
	var models []string
	for _, entry := range envelope.Data {
		// Gemini's OpenAI-compatibility layer answers with its native names in
		// an OpenAI envelope, so ids arrive as "models/gemini-3.5-flash" here
		// and as "gemini-3.5-flash" on the native path. It is one namespace
		// delivered two ways, and stripping is safe because no vendor prefix
		// is "models".
		if name := strings.TrimPrefix(entry.ID, "models/"); name != "" {
			models = append(models, name)
		}
	}
	for _, entry := range envelope.Models {
		name := strings.TrimPrefix(entry.Name, "models/")
		if name != "" {
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("listing contained no models")
	}
	return models, nil
}
