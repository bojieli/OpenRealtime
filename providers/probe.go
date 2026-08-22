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

// ProbeResult is what a provider says it serves.
type ProbeResult struct {
	Provider string
	Endpoint string
	Models   []string
}

// Probe asks a provider to list its models.
//
// It exists because the default models in this catalogue are the one part of
// it that goes stale, and a reader with a key can get the truth in a second
// rather than trusting a constant compiled months ago. It is a listing, not a
// health check: a provider that answers here may still refuse a completion.
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
	return ProbeResult{Provider: entry.Name, Endpoint: endpoint, Models: models}, nil
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
		if entry.ID != "" {
			models = append(models, entry.ID)
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
