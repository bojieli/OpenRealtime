package providers

import (
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/perception"
)

// VisionRequest is one resolved narrator construction.
type VisionRequest struct {
	Provider        string
	Model           string
	BaseURL         string
	APIKey          string
	KeyEnv          string
	MaxOutputTokens int
	RequestTimeout  time.Duration
}

// NewVision builds the client a video observer narrates through.
//
// It resolves against the language-model catalogue rather than a separate one,
// because a narrator is a vision-capable chat model and every provider that
// serves one already has an entry. What it adds is the refusal: a provider
// whose catalogue entry does not declare vision, or that does not speak Chat
// Completions, cannot narrate, and saying so here is better than a runtime
// error on the first frame.
func NewVision(request VisionRequest) (perception.Vision, error) {
	entry, err := LookupLLM(request.Provider)
	if err != nil {
		return nil, err
	}
	if entry.Dialect != DialectOpenAIChat {
		return nil, fmt.Errorf(
			"narration needs an OpenAI-compatible endpoint; provider %q speaks %s. "+
				"Use google-openai for Gemini, or point -vision-provider at a compatible gateway",
			entry.Name, entry.Dialect)
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = entry.FastModel
	}
	if model == "" {
		return nil, fmt.Errorf("provider %q has no default vision model; pass -vision-model", entry.Name)
	}
	baseURL := strings.TrimSpace(request.BaseURL)
	if baseURL == "" {
		baseURL = entry.BaseURL
	}
	key := request.APIKey
	if key == "" {
		key = entry.credential(request.KeyEnv)
	}
	if key == "" && !entry.Local && entry.Auth != AuthNone {
		return nil, fmt.Errorf("narration provider %q needs a credential in %s",
			entry.Name, entry.credentialHint(request.KeyEnv))
	}
	return openaivision.New(openaivision.Config{
		BaseURL: baseURL, Model: model, APIKey: key,
		MaxOutputTokens: request.MaxOutputTokens, RequestTimeout: request.RequestTimeout,
	})
}
