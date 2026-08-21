// Package openaivision describes images through an OpenAI-compatible chat
// completions endpoint.
//
// One adapter covers every vision model worth pointing a narrator at: a local
// vLLM or SGLang server, a hosted provider's compatibility endpoint, or
// anything else that speaks the same shape. The narrator does not care which,
// which is what makes the session-versus-dedicated choice a configuration
// rather than a code path.
package openaivision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/perception"
)

// DefaultBaseURL is the conventional local OpenAI-compatible location.
const DefaultBaseURL = "http://127.0.0.1:8000/v1"

// Config configures the client.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	// MaxOutputTokens bounds the narration.
	//
	// It is generous rather than tight, and that is a correction: a reasoning
	// model spends this budget on thinking before it writes anything, so a
	// limit sized for the output alone truncates the narration mid-sentence
	// and looks like a model that cannot describe a screen. The prompt is
	// what keeps narration short; this only stops it running away.
	MaxOutputTokens int
	RequestTimeout  time.Duration
	HTTPClient      *http.Client
	// Temperature is applied when non-negative. Narration is a factual task,
	// so the default is zero rather than the provider's.
	Temperature float64
}

// Client describes images.
type Client struct {
	config Config
	http   *http.Client
}

// New validates the configuration.
func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("a vision client requires a model")
	}
	if strings.TrimSpace(config.BaseURL) == "" {
		config.BaseURL = DefaultBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.MaxOutputTokens <= 0 {
		config.MaxOutputTokens = 2048
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 30 * time.Second
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.RequestTimeout}
	}
	return &Client{config: config, http: client}, nil
}

// Name identifies the model for reports.
func (client *Client) Name() string { return client.config.Model }

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Describe narrates images.
func (client *Client) Describe(ctx context.Context, images []perception.Image, prompt string) (string, error) {
	if len(images) == 0 {
		return "", nil
	}
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("a narration request requires a prompt")
	}
	parts := make([]contentPart, 0, len(images)+1)
	parts = append(parts, contentPart{Type: "text", Text: prompt})
	for index, image := range images {
		if len(image.Bytes) == 0 || strings.TrimSpace(image.MIMEType) == "" {
			return "", fmt.Errorf("image %d needs bytes and a MIME type", index)
		}
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: "data:" + image.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(image.Bytes),
			},
		})
	}
	body := chatRequest{
		Model: client.config.Model, MaxTokens: client.config.MaxOutputTokens,
		Messages: []chatMessage{{Role: "user", Content: parts}},
	}
	if client.config.Temperature >= 0 {
		temperature := client.config.Temperature
		body.Temperature = &temperature
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, client.config.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(client.config.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("narration request: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("narration returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var decoded chatResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode narration: %w", err)
	}
	if decoded.Error != nil {
		return "", errors.New(decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("narration returned no choices")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}

var _ perception.Vision = (*Client)(nil)
