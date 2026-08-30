// Package gemini implements the Google Gemini Interactions API as one
// replaceable offline benchmark-review plugin. It is deliberately not linked
// into the realtime server, graph runtime, or presentation clients.
package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	RegistrationName = "google.gemini-3.7-flash"
	ModelID          = "gemini-3.7-flash"
	APIRevision      = "v1"
	interactionsAPI  = "gemini.interactions"
	interactionsURL  = "https://generativelanguage.googleapis.com/v1/interactions"

	maximumInlineRequestBytes = 20_000_000
	maximumResponseBytes      = 8 << 20
	maximumPreparedMedia      = 256
	maximumPromptBytes        = 8 << 20
	maximumSchemaBytes        = 1 << 20
	maximumContextBytes       = 4 << 20
	maximumAPIKeyBytes        = 4096
	minimumAPIKeyBytes        = 16
	defaultRequestTimeout     = 10 * time.Minute
)

// This canonical identity covers every provider-side choice that can affect
// the request while leaving the API key out of provenance.
const configurationIdentity = `{"api_revision":"v1","background":false,"endpoint":"https://generativelanguage.googleapis.com/v1/interactions","implementation":"openrealtime.gemini-review.v1","inline_request_max_bytes":20000000,"input_shape":"one_user_input","media_order":"prompt_then_manifest_media","max_output_tokens":16384,"seed":1,"store":false,"stream":false,"thinking_level":"high"}`

type Plugin struct {
	apiKey     string
	httpClient *http.Client
	descriptor review.ProviderDescriptor
}

// APIKeySource resolves a credential only when the selected registration is
// opened. Implementations must not include credential bytes in returned
// errors; Registration replaces source errors with a non-secret diagnostic.
type APIKeySource func(context.Context) (string, error)

func Descriptor() review.ProviderDescriptor {
	return review.ProviderDescriptor{
		Provider: "google", Model: ModelID, API: interactionsAPI,
		APIRevision: APIRevision, ConfigurationSHA256: digest([]byte(configurationIdentity)),
	}
}

// New creates the exact Gemini 3.7 Flash plugin. A nil HTTP client receives a
// bounded default. Supplied clients are copied, stripped of cookies, and made
// non-redirecting so the API-key header cannot cross origins.
func New(apiKey string, client *http.Client) (*Plugin, error) {
	if err := validateAPIKey(apiKey); err != nil {
		return nil, err
	}
	snapshot := snapshotHTTPClient(client)
	return &Plugin{apiKey: apiKey, httpClient: &snapshot, descriptor: Descriptor()}, nil
}

func snapshotHTTPClient(client *http.Client) http.Client {
	var snapshot http.Client
	if client != nil {
		snapshot = *client
	}
	if snapshot.Timeout <= 0 {
		snapshot.Timeout = defaultRequestTimeout
	}
	snapshot.Jar = nil
	snapshot.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return snapshot
}

func (plugin *Plugin) Descriptor() review.ProviderDescriptor {
	if plugin == nil {
		return review.ProviderDescriptor{}
	}
	return plugin.descriptor
}

func (plugin *Plugin) Review(
	ctx context.Context, prepared review.PreparedRequest,
) (review.ProviderResponse, error) {
	if plugin == nil || plugin.httpClient == nil {
		return review.ProviderResponse{}, errors.New("Gemini review plugin is nil")
	}
	if ctx == nil {
		return review.ProviderResponse{}, errors.New("Gemini review requires a context")
	}
	if err := context.Cause(ctx); err != nil {
		return review.ProviderResponse{}, err
	}
	if err := validatePrepared(prepared); err != nil {
		return review.ProviderResponse{}, err
	}
	body, err := marshalRequest(prepared)
	if err != nil {
		return review.ProviderResponse{}, err
	}
	requestDigest := digest(body)
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, interactionsURL, bytes.NewReader(body))
	if err != nil {
		return review.ProviderResponse{}, errors.New("construct Gemini Interactions request")
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-goog-api-key", plugin.apiKey)
	httpRequest.Header.Set("User-Agent", "OpenRealtime-benchmark-review/1")

	httpResponse, err := plugin.httpClient.Do(httpRequest)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return review.ProviderResponse{}, cause
		}
		return review.ProviderResponse{}, errors.New("call Gemini Interactions API failed")
	}
	defer httpResponse.Body.Close()
	raw, err := readBounded(httpResponse.Body, maximumResponseBytes)
	if err != nil {
		return review.ProviderResponse{}, err
	}
	if httpResponse.StatusCode != http.StatusOK {
		return review.ProviderResponse{}, interactionHTTPError(
			httpResponse.StatusCode, raw, plugin.apiKey)
	}
	mediaType, _, err := mime.ParseMediaType(httpResponse.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return review.ProviderResponse{}, errors.New(
			"Gemini Interactions API returned a non-JSON content type")
	}
	if containsCredentialJSON(raw, plugin.apiKey) {
		return review.ProviderResponse{}, errors.New(
			"Gemini Interactions response contained credential material and was discarded")
	}
	output, model, requestID, err := decodeInteraction(raw)
	if err != nil {
		return review.ProviderResponse{}, err
	}
	if containsCredentialJSON(output, plugin.apiKey) {
		return review.ProviderResponse{}, errors.New(
			"Gemini Interactions output contained credential material and was discarded")
	}
	return review.ProviderResponse{
		Raw: slices.Clone(raw), Output: output, ReportedModel: model,
		RequestID: requestID, RequestSHA256: requestDigest,
	}, nil
}

// Registration contributes Gemini to the provider-neutral catalog without
// resolving the API key or constructing a provider during catalog assembly.
func Registration(source APIKeySource, client *http.Client) review.Registration {
	registration := review.Registration{Name: RegistrationName, Descriptor: Descriptor()}
	if source == nil {
		return registration
	}
	clientSnapshot := snapshotHTTPClient(client)
	registration.Factory = func(ctx context.Context) (review.Provider, error) {
		if ctx == nil {
			return nil, errors.New("open Gemini review plugin: nil context")
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		apiKey, err := source(ctx)
		if err != nil {
			return nil, errors.New("resolve Gemini API key: credential source failed")
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		return New(apiKey, &clientSnapshot)
	}
	return registration
}

// EnvironmentAPIKey is the opt-in environment credential source used by
// benchmark commands. It never returns the value in an error.
func EnvironmentAPIKey(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", errors.New("resolve GEMINI_API_KEY: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	value, exists := os.LookupEnv("GEMINI_API_KEY")
	if !exists || validateAPIKey(value) != nil {
		return "", errors.New("GEMINI_API_KEY is missing or invalid")
	}
	return value, nil
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Data      string `json:"data,omitempty"`
	MediaType string `json:"mime_type,omitempty"`
}

type userInputStep struct {
	Type    string         `json:"type"`
	Content []contentBlock `json:"content"`
}

type interactionRequest struct {
	Model            string           `json:"model"`
	Input            []userInputStep  `json:"input"`
	ResponseFormat   responseFormat   `json:"response_format"`
	GenerationConfig generationConfig `json:"generation_config"`
	Store            bool             `json:"store"`
	Stream           bool             `json:"stream"`
	Background       bool             `json:"background"`
}

type responseFormat struct {
	Type      string          `json:"type"`
	MediaType string          `json:"mime_type"`
	Schema    json.RawMessage `json:"schema"`
}

type generationConfig struct {
	ThinkingLevel   string `json:"thinking_level"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Seed            int    `json:"seed"`
}

func marshalRequest(prepared review.PreparedRequest) ([]byte, error) {
	input := make([]contentBlock, 0, len(prepared.Media)+1)
	input = append(input, contentBlock{Type: "text", Text: prepared.Prompt})
	for _, media := range prepared.Media {
		input = append(input, contentBlock{
			Type: media.Kind, Data: base64.StdEncoding.EncodeToString(media.Bytes),
			MediaType: media.MediaType,
		})
	}
	body, err := json.Marshal(interactionRequest{
		Model: ModelID, Input: []userInputStep{{Type: "user_input", Content: input}},
		ResponseFormat: responseFormat{
			Type: "text", MediaType: "application/json", Schema: slices.Clone(prepared.Schema),
		},
		GenerationConfig: generationConfig{
			ThinkingLevel: "high", MaxOutputTokens: 16_384, Seed: 1,
		},
		Store: false, Stream: false, Background: false,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Gemini Interactions request: %w", err)
	}
	if len(body) > maximumInlineRequestBytes {
		return nil, fmt.Errorf(
			"Gemini inline review request is %d bytes; maximum is %d",
			len(body), maximumInlineRequestBytes)
	}
	return body, nil
}

func validatePrepared(prepared review.PreparedRequest) error {
	if prepared.PromptVersion != review.CasePromptVersion ||
		prepared.SchemaVersion != review.CaseSchemaVersion {
		return errors.New("Gemini reviewer requires the exact standard prompt and schema versions")
	}
	if strings.TrimSpace(prepared.AttemptID) == "" ||
		strings.TrimSpace(prepared.AttemptID) != prepared.AttemptID ||
		containsUnsupportedControl(prepared.AttemptID) {
		return errors.New("Gemini reviewer requires a canonical attempt ID")
	}
	if strings.TrimSpace(prepared.Suite) == "" || strings.TrimSpace(prepared.Case) == "" ||
		strings.TrimSpace(prepared.Suite) != prepared.Suite ||
		strings.TrimSpace(prepared.Case) != prepared.Case || prepared.Trial <= 0 ||
		containsUnsupportedControl(prepared.Suite) || containsUnsupportedControl(prepared.Case) {
		return errors.New("Gemini reviewer requires canonical suite, case, and trial identities")
	}
	if len(prepared.Prompt) == 0 || len(prepared.Prompt) > maximumPromptBytes ||
		!utf8.ValidString(prepared.Prompt) {
		return errors.New("Gemini reviewer prompt is empty, oversized, or invalid UTF-8")
	}
	if len(prepared.Schema) == 0 || len(prepared.Schema) > maximumSchemaBytes ||
		strictjson.Validate(prepared.Schema) != nil {
		return errors.New("Gemini reviewer schema is empty, oversized, or invalid JSON")
	}
	if len(prepared.Context) == 0 || len(prepared.Context) > maximumContextBytes ||
		strictjson.Validate(prepared.Context) != nil || prepared.Context[0] != '{' {
		return errors.New("Gemini reviewer context is empty, oversized, or not a JSON object")
	}
	if !validDigest(prepared.RequestFingerprint) {
		return errors.New("Gemini reviewer request fingerprint is not canonical SHA-256")
	}
	if len(prepared.Media) == 0 || len(prepared.Media) > maximumPreparedMedia {
		return fmt.Errorf("Gemini reviewer requires 1..%d media artifacts", maximumPreparedMedia)
	}
	videos := 0
	for index, media := range prepared.Media {
		if !supportedMediaType(media.Kind, media.MediaType) {
			return fmt.Errorf("Gemini reviewer media %d has unsupported kind or media type", index)
		}
		if len(media.Bytes) == 0 || digest(media.Bytes) != media.SHA256 {
			return fmt.Errorf("Gemini reviewer media %d bytes do not match their digest", index)
		}
		if media.Kind == "video" {
			videos++
		}
	}
	if videos > 10 {
		return errors.New("Gemini reviewer accepts at most 10 video artifacts per request")
	}
	return nil
}

func supportedMediaType(kind, mediaType string) bool {
	var supported map[string]struct{}
	switch kind {
	case "audio":
		supported = map[string]struct{}{
			"audio/wav": {}, "audio/mp3": {}, "audio/aiff": {}, "audio/aac": {},
			"audio/ogg": {}, "audio/flac": {}, "audio/mpeg": {}, "audio/m4a": {},
			"audio/l16": {}, "audio/opus": {}, "audio/alaw": {}, "audio/mulaw": {},
			"audio/webm": {},
		}
	case "image":
		supported = map[string]struct{}{
			"image/png": {}, "image/jpeg": {}, "image/webp": {}, "image/heic": {},
			"image/heif": {}, "image/gif": {}, "image/bmp": {}, "image/tiff": {},
		}
	case "video":
		supported = map[string]struct{}{
			"video/mp4": {}, "video/mpeg": {}, "video/mov": {}, "video/avi": {},
			"video/x-flv": {}, "video/mpg": {}, "video/webm": {}, "video/wmv": {},
			"video/3gpp": {},
		}
	default:
		return false
	}
	_, exists := supported[mediaType]
	return exists
}

func decodeInteraction(raw []byte) (json.RawMessage, string, string, error) {
	if err := strictjson.Validate(raw); err != nil {
		return nil, "", "", fmt.Errorf("decode Gemini Interactions response: %w", err)
	}
	var envelope struct {
		ID         string            `json:"id"`
		Model      string            `json:"model"`
		Object     string            `json:"object"`
		Status     string            `json:"status"`
		Steps      []json.RawMessage `json:"steps"`
		Errors     []json.RawMessage `json:"errors"`
		OutputText *string           `json:"output_text"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", "", errors.New("decode Gemini Interactions response envelope")
	}
	if envelope.Object != "interaction" || envelope.Status != "completed" {
		return nil, "", "", fmt.Errorf(
			"Gemini interaction was not a completed interaction (object=%q status=%q)",
			envelope.Object, envelope.Status)
	}
	if envelope.Model != ModelID {
		return nil, "", "", fmt.Errorf(
			"Gemini interaction reported model %q, want %q", envelope.Model, ModelID)
	}
	// With store=false the Interactions API intentionally reports id:null,
	// which unmarshals to an empty string. Validate a provider ID when one is
	// present, but do not require server-side retention merely to manufacture
	// an identifier for an offline benchmark review.
	if envelope.ID != "" && validateMachineText(envelope.ID, 4096) != nil {
		return nil, "", "", errors.New("Gemini interaction has an invalid request ID")
	}
	if len(envelope.Errors) != 0 {
		return nil, "", "", errors.New("Gemini completed interaction contains provider errors")
	}
	var output json.RawMessage
	modelOutputs := 0
	for index, rawStep := range envelope.Steps {
		if err := strictjson.Validate(rawStep); err != nil {
			return nil, "", "", fmt.Errorf("Gemini interaction step %d is invalid JSON", index)
		}
		var discriminator struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rawStep, &discriminator) != nil {
			return nil, "", "", fmt.Errorf("Gemini interaction step %d is invalid", index)
		}
		switch discriminator.Type {
		case "thought":
			continue
		case "model_output":
			modelOutputs++
			var step struct {
				Type    string            `json:"type"`
				Content []json.RawMessage `json:"content"`
			}
			if json.Unmarshal(rawStep, &step) != nil || len(step.Content) != 1 {
				return nil, "", "", errors.New(
					"Gemini model output must contain exactly one content block")
			}
			if err := strictjson.Validate(step.Content[0]); err != nil {
				return nil, "", "", errors.New("Gemini model output content is invalid JSON")
			}
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(step.Content[0], &content) != nil || content.Type != "text" ||
				strings.TrimSpace(content.Text) == "" {
				return nil, "", "", errors.New(
					"Gemini model output must be one non-empty text block")
			}
			output = json.RawMessage([]byte(content.Text))
		default:
			return nil, "", "", fmt.Errorf(
				"Gemini interaction contains unexpected step type %q", discriminator.Type)
		}
	}
	if modelOutputs != 1 || len(output) == 0 {
		return nil, "", "", errors.New("Gemini interaction must contain exactly one model output")
	}
	if envelope.OutputText != nil && *envelope.OutputText != string(output) {
		return nil, "", "", errors.New("Gemini interaction output_text disagrees with its model output")
	}
	return slices.Clone(output), envelope.Model, envelope.ID, nil
}

func interactionHTTPError(statusCode int, raw []byte, credential string) error {
	prefix := fmt.Sprintf("Gemini Interactions API returned HTTP %d", statusCode)
	if containsCredentialJSON(raw, credential) || strictjson.Validate(raw) != nil {
		return errors.New(prefix)
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return errors.New(prefix)
	}
	message, messageOK := safeProviderMessage(envelope.Error.Message, credential)
	if !messageOK {
		return errors.New(prefix)
	}
	if envelope.Error.Status == "" {
		return fmt.Errorf("%s: %s", prefix, message)
	}
	if validateMachineText(envelope.Error.Status, 256) != nil {
		return errors.New(prefix)
	}
	return fmt.Errorf("%s (%s): %s", prefix, envelope.Error.Status, message)
}

func safeProviderMessage(value, credential string) (string, bool) {
	if len(value) == 0 || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.Contains(value, credential) {
		return "", false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return "", false
		}
	}
	canonical := strings.Join(strings.Fields(value), " ")
	return canonical, canonical != ""
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: maximum + 1}
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("read Gemini Interactions response")
	}
	if len(payload) == 0 || int64(len(payload)) > maximum {
		return nil, fmt.Errorf(
			"Gemini Interactions response must be 1..%d bytes", maximum)
	}
	return payload, nil
}

func validateAPIKey(value string) error {
	if len(value) < minimumAPIKeyBytes || len(value) > maximumAPIKeyBytes ||
		!utf8.ValidString(value) || strings.TrimSpace(value) != value ||
		containsUnsupportedControl(value) {
		return errors.New("Gemini API key is missing or invalid")
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			return errors.New("Gemini API key is missing or invalid")
		}
	}
	return nil
}

func validateMachineText(value string, maximum int) error {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value || containsUnsupportedControl(value) {
		return errors.New("invalid machine text")
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			return errors.New("invalid machine text")
		}
	}
	return nil
}

func containsUnsupportedControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsCredentialJSON(payload []byte, credential string) bool {
	if bytes.Contains(payload, []byte(credential)) {
		return true
	}
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return false
	}
	return valueContainsCredential(value, credential, 0)
}

func valueContainsCredential(value any, credential string, depth int) bool {
	if depth > 4 {
		return false
	}
	switch typed := value.(type) {
	case string:
		if strings.Contains(typed, credential) {
			return true
		}
		var nested any
		if json.Unmarshal([]byte(typed), &nested) == nil {
			return valueContainsCredential(nested, credential, depth+1)
		}
	case []any:
		for _, item := range typed {
			if valueContainsCredential(item, credential, depth+1) {
				return true
			}
		}
	case map[string]any:
		for key, item := range typed {
			if strings.Contains(key, credential) || valueContainsCredential(item, credential, depth+1) {
				return true
			}
		}
	}
	return false
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func digest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}
