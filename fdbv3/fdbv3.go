// Package fdbv3 runs the official Full-Duplex-Bench v3 audio and tool-use
// corpus through a standard OpenAI Realtime-compatible adapter.
package fdbv3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/livebench"
)

const (
	BenchmarkName     = "Full-Duplex-Bench v3"
	BenchmarkRevision = "3e799c45a045256f47d5f1c9cda90157e2d2ec9e"
	ResultVersion     = "1.0.0"
)

var releasedDirectory = regexp.MustCompile(`^(.+)_([0-9a-f]{24})$`)

var officialToolNames = []string{
	"add_to_cart", "book_flight", "calculate_commute", "get_card_benefits",
	"get_exchange_rate", "modify_autopay", "search_apartments", "search_flights",
	"search_products", "track_order", "update_identity_doc", "update_search_filter",
}

type Source struct {
	Repository    string `json:"repository"`
	Revision      string `json:"revision"`
	AgentPath     string `json:"agent_path"`
	AgentSHA256   string `json:"agent_sha256"`
	MockAPIPath   string `json:"mock_api_path"`
	MockAPISHA256 string `json:"mock_api_sha256"`
}

type Profile struct {
	SchemaVersion string                   `json:"schema_version"`
	Profile       string                   `json:"profile"`
	Source        Source                   `json:"source"`
	Instructions  string                   `json:"instructions"`
	Tools         []livebench.RealtimeTool `json:"tools"`
	SHA256        string                   `json:"-"`
}

func LoadProfile(filename string) (Profile, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Profile{}, fmt.Errorf("read FDB v3 profile: %w", err)
	}
	var profile Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		return Profile{}, fmt.Errorf("decode FDB v3 profile: %w", err)
	}
	if profile.SchemaVersion != "1.0.0" || strings.TrimSpace(profile.Profile) == "" ||
		strings.TrimSpace(profile.Instructions) == "" || len(profile.Tools) != 12 {
		return Profile{}, errors.New("FDB v3 profile must use schema 1.0.0 and contain instructions plus exactly 12 tools")
	}
	if profile.Source.Revision != BenchmarkRevision {
		return Profile{}, fmt.Errorf("FDB v3 profile pins revision %q, want %q", profile.Source.Revision, BenchmarkRevision)
	}
	names := make([]string, 0, len(profile.Tools))
	for _, tool := range profile.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if !slices.Equal(names, officialToolNames) {
		return Profile{}, fmt.Errorf("FDB v3 profile tools are %v, want the official catalog %v", names, officialToolNames)
	}
	if _, err := livebench.NewOpenAIAdapter(livebench.OpenAIConfig{
		APIKey: "profile-validation", Tools: profile.Tools, ExecuteTool: MockExecutor,
	}); err != nil {
		return Profile{}, fmt.Errorf("validate FDB v3 Realtime tools: %w", err)
	}
	profile.SHA256 = hashBytes(data)
	return profile, nil
}

type Metadata struct {
	ID                string             `json:"id"`
	Domain            string             `json:"domain"`
	Title             string             `json:"title"`
	Difficulty        string             `json:"difficulty"`
	ExpectedToolCalls []ExpectedToolCall `json:"expected_tool_calls"`
	StateRollbackTest bool               `json:"state_rollback_test"`
}

type ExpectedToolCall struct {
	Function string         `json:"function"`
	Args     map[string]any `json:"args"`
}

type Sample struct {
	PID            string   `json:"pid"`
	ExampleID      string   `json:"example_id"`
	Directory      string   `json:"directory"`
	InputPath      string   `json:"input_path"`
	MetadataPath   string   `json:"metadata_path"`
	InputSHA256    string   `json:"input_sha256"`
	MetadataSHA256 string   `json:"metadata_sha256"`
	Metadata       Metadata `json:"metadata"`
}

func Discover(root string) ([]Sample, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read FDB v3 dataset root: %w", err)
	}
	var samples []Sample
	seenDirectories := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		matches := releasedDirectory.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		inputPath := filepath.Join(directory, "input.wav")
		metadataPath := filepath.Join(directory, "metadata.json")
		metadataData, readErr := os.ReadFile(metadataPath)
		if readErr != nil {
			return nil, fmt.Errorf("read FDB v3 metadata %s: %w", metadataPath, readErr)
		}
		var metadata Metadata
		if err := json.Unmarshal(metadataData, &metadata); err != nil {
			return nil, fmt.Errorf("decode FDB v3 metadata %s: %w", metadataPath, err)
		}
		if metadata.ID != matches[1] {
			return nil, fmt.Errorf("FDB v3 directory %s identifies %q but metadata identifies %q", entry.Name(), matches[1], metadata.ID)
		}
		inputHash, hashErr := livebench.HashFile(inputPath)
		if hashErr != nil {
			return nil, fmt.Errorf("hash FDB v3 input %s: %w", inputPath, hashErr)
		}
		if _, duplicate := seenDirectories[entry.Name()]; duplicate {
			return nil, fmt.Errorf("duplicate FDB v3 directory %q", entry.Name())
		}
		seenDirectories[entry.Name()] = struct{}{}
		samples = append(samples, Sample{
			PID: matches[2], ExampleID: matches[1], Directory: directory,
			InputPath: inputPath, MetadataPath: metadataPath,
			InputSHA256: inputHash, MetadataSHA256: hashBytes(metadataData), Metadata: metadata,
		})
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].ExampleID == samples[j].ExampleID {
			return samples[i].PID < samples[j].PID
		}
		return samples[i].ExampleID < samples[j].ExampleID
	})
	if len(samples) == 0 {
		return nil, fmt.Errorf("no released FDB v3 examples found below %s", root)
	}
	return samples, nil
}

type ActualToolCall struct {
	Function       string         `json:"function"`
	Args           map[string]any `json:"args"`
	TimestampStart float64        `json:"timestamp_start"`
	TimestampEnd   float64        `json:"timestamp_end"`
}

type Latency struct {
	InputDurationS  float64  `json:"input_duration_s"`
	OutputDurationS float64  `json:"output_duration_s"`
	FirstSpeechS    *float64 `json:"first_speech_s,omitempty"`
}

type Evidence struct {
	BenchmarkRevision string                  `json:"benchmark_revision"`
	Profile           string                  `json:"profile"`
	ProfileSHA256     string                  `json:"profile_sha256"`
	InputSHA256       string                  `json:"input_sha256"`
	MetadataSHA256    string                  `json:"metadata_sha256"`
	OutputSHA256      string                  `json:"output_sha256"`
	TranscriptSource  string                  `json:"transcript_source"`
	UserEndSource     string                  `json:"user_end_source"`
	Session           livebench.SessionResult `json:"session"`
}

type Result struct {
	SchemaVersion         string           `json:"openrealtime_schema_version"`
	PID                   string           `json:"pid"`
	ExampleID             string           `json:"example_id"`
	Category              string           `json:"category"`
	Title                 string           `json:"title"`
	Provider              string           `json:"provider"`
	EvaluatedAt           time.Time        `json:"evaluated_at"`
	Status                string           `json:"status"`
	InputTranscript       string           `json:"input_transcript"`
	InputASRChunks        []any            `json:"input_asr_chunks"`
	UserSpeechEndRelative *float64         `json:"user_speech_end_rel,omitempty"`
	Latency               Latency          `json:"latency"`
	Transcript            string           `json:"transcript"`
	ASRChunks             []any            `json:"asr_chunks"`
	PerceivedLatency      *float64         `json:"perceived_total_latency,omitempty"`
	AgentSpeechStart      *float64         `json:"audio_agent_speech_start,omitempty"`
	ActualToolCalls       []ActualToolCall `json:"actual_tool_calls"`
	Evidence              Evidence         `json:"openrealtime"`
}

func ResultPaths(sample Sample, provider string) (string, string) {
	safe := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(provider, "-")
	return filepath.Join(sample.Directory, "output_"+safe+".wav"), filepath.Join(sample.Directory, "result_"+safe+".json")
}

func LoadCompleted(sample Sample, provider, profileHash, model string) (Result, bool, error) {
	outputPath, resultPath := ResultPaths(sample, provider)
	data, err := os.ReadFile(resultPath)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read prior FDB v3 result: %w", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, false, fmt.Errorf("decode prior FDB v3 result: %w", err)
	}
	if result.Status != "completed" || result.SchemaVersion != ResultVersion || result.ExampleID != sample.ExampleID ||
		result.PID != sample.PID || result.Provider != provider || result.Evidence.BenchmarkRevision != BenchmarkRevision ||
		result.Evidence.ProfileSHA256 != profileHash || result.Evidence.InputSHA256 != sample.InputSHA256 ||
		result.Evidence.MetadataSHA256 != sample.MetadataSHA256 || result.Evidence.Session.Descriptor.Model != model {
		return Result{}, false, fmt.Errorf("prior FDB v3 result %s does not match the requested immutable run", resultPath)
	}
	outputHash, err := livebench.HashFile(outputPath)
	if err != nil {
		return Result{}, false, fmt.Errorf("verify prior FDB v3 output: %w", err)
	}
	if outputHash != result.Evidence.OutputSHA256 {
		return Result{}, false, fmt.Errorf("prior FDB v3 output hash mismatch for %s", outputPath)
	}
	return result, true, nil
}

func RunSample(ctx context.Context, adapter livebench.Adapter, sample Sample, provider string, profile Profile) (Result, error) {
	input, err := livebench.ReadWAV(sample.InputPath)
	if err != nil {
		return Result{}, fmt.Errorf("read FDB v3 input: %w", err)
	}
	session, err := adapter.Run(ctx, input)
	if err != nil {
		return Result{}, err
	}
	aligned := livebench.FitDuration(livebench.AlignChunks(session.Chunks, session.Descriptor.OutputSampleRate), input.Duration())
	outputPath, resultPath := ResultPaths(sample, provider)
	outputHash, err := livebench.WriteWAV(outputPath, aligned)
	if err != nil {
		return Result{}, err
	}
	timing := livebench.ScoreTiming(input, aligned, nil, nil)
	firstSpeechS := divideMilliseconds(timing.FirstOutputMS)
	var userEndS *float64
	if len(session.UserSpeechEndsMS) != 0 {
		value := session.UserSpeechEndsMS[0] / 1000
		userEndS = &value
	} else if session.UserSpeechEndMS != nil {
		value := *session.UserSpeechEndMS / 1000
		userEndS = &value
	}
	var perceived *float64
	if firstSpeechS != nil && userEndS != nil {
		value := *firstSpeechS - *userEndS
		perceived = &value
	}
	actualCalls := make([]ActualToolCall, 0, len(session.ToolCalls))
	for _, call := range session.ToolCalls {
		var arguments map[string]any
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			return Result{}, fmt.Errorf("decode recorded arguments for %s: %w", call.Name, err)
		}
		actualCalls = append(actualCalls, ActualToolCall{
			Function: call.Name, Args: arguments,
			TimestampStart: call.RequestedMS / 1000, TimestampEnd: call.CompletedMS / 1000,
		})
	}
	// Chunks are materialized in output_<provider>.wav. Keeping them in memory
	// would not add evidence to the JSON trace.
	session.Chunks = nil
	result := Result{
		SchemaVersion: ResultVersion, PID: sample.PID, ExampleID: sample.ExampleID,
		Category: sample.Metadata.Domain, Title: sample.Metadata.Title, Provider: provider,
		EvaluatedAt: time.Now().UTC(), Status: "completed",
		InputTranscript: strings.Join(session.InputTranscripts, " "), InputASRChunks: []any{},
		UserSpeechEndRelative: userEndS,
		Latency:               Latency{InputDurationS: seconds(input.Duration()), OutputDurationS: seconds(aligned.Duration()), FirstSpeechS: firstSpeechS},
		Transcript:            session.OutputTranscript, ASRChunks: []any{}, PerceivedLatency: perceived, AgentSpeechStart: firstSpeechS,
		ActualToolCalls: actualCalls,
		Evidence: Evidence{
			BenchmarkRevision: BenchmarkRevision, Profile: profile.Profile, ProfileSHA256: profile.SHA256,
			InputSHA256: sample.InputSHA256, MetadataSHA256: sample.MetadataSHA256, OutputSHA256: outputHash,
			TranscriptSource: "response.output_audio_transcript.delta", UserEndSource: "first input_audio_buffer.speech_stopped.audio_end_ms",
			Session: session,
		},
	}
	if err := writeJSONAtomic(resultPath, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func MockExecutor(_ context.Context, name string, raw json.RawMessage) (json.RawMessage, error) {
	arguments, err := decodeArguments(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s arguments: %w", name, err)
	}
	var result any
	switch name {
	case "search_flights":
		destination, err := requiredString(arguments, "destination")
		if err != nil {
			return nil, err
		}
		date, err := requiredString(arguments, "date")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "flights": []any{map[string]any{"flight_id": "FL123", "destination": destination, "date": date, "price": 450.0}}}
	case "book_flight":
		passenger, err := requiredString(arguments, "passenger_name")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "booking_ref": "B789", "passenger": passenger}
	case "update_identity_doc":
		docType, err := requiredString(arguments, "doc_type")
		if err != nil {
			return nil, err
		}
		docNumber, err := requiredString(arguments, "doc_number")
		if err != nil {
			return nil, err
		}
		masked := docNumber
		if len(masked) > 4 {
			masked = masked[len(masked)-4:]
		}
		result = map[string]any{"status": "success", "updated_doc": docType, "masked_number": masked}
	case "get_card_benefits":
		cardType, err := requiredString(arguments, "card_type")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "card_type": cardType, "benefits": []string{"2% Cashback", "No Foreign Transaction Fee"}}
	case "get_exchange_rate":
		amount, err := requiredFloat(arguments, "amount")
		if err != nil {
			return nil, err
		}
		from, err := requiredString(arguments, "from_currency")
		if err != nil {
			return nil, err
		}
		if _, err := requiredString(arguments, "to_currency"); err != nil {
			return nil, err
		}
		rate := 0.9
		if from == "EUR" {
			rate = 1.1
		}
		result = map[string]any{"status": "success", "converted_amount": amount * rate, "rate": rate}
	case "modify_autopay":
		bill, err := requiredString(arguments, "bill_type")
		if err != nil {
			return nil, err
		}
		source, err := requiredString(arguments, "source_account")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "autopay_enabled": true, "bill": bill, "source": source}
	case "search_apartments":
		city, err := requiredString(arguments, "city")
		if err != nil {
			return nil, err
		}
		bedrooms, err := requiredInt(arguments, "bedrooms")
		if err != nil {
			return nil, err
		}
		maxPrice, err := requiredFloat(arguments, "max_price")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "city": city, "results": []any{map[string]any{"id": "APT1", "price": maxPrice - 100, "beds": bedrooms}}}
	case "calculate_commute":
		if _, err := requiredString(arguments, "origin_address"); err != nil {
			return nil, err
		}
		if _, err := requiredString(arguments, "destination_address"); err != nil {
			return nil, err
		}
		mode := optionalString(arguments, "mode", "driving")
		result = map[string]any{"status": "success", "duration_mins": 25, "mode": mode}
	case "update_search_filter":
		filter, err := requiredString(arguments, "filter_name")
		if err != nil {
			return nil, err
		}
		value, exists := arguments["value"]
		if !exists {
			return nil, errors.New("missing required argument value")
		}
		result = map[string]any{"status": "success", "filter_updated": filter, "new_value": value}
	case "track_order":
		orderID, err := requiredString(arguments, "order_id")
		if err != nil {
			return nil, err
		}
		result = map[string]any{"status": "success", "order_id": orderID, "shipping_status": "Out for delivery"}
	case "search_products":
		query, err := requiredString(arguments, "query")
		if err != nil {
			return nil, err
		}
		price := 99.99
		if value, exists := arguments["max_price"]; exists && value != nil {
			maximum, err := numberValue(value)
			if err != nil {
				return nil, fmt.Errorf("argument max_price: %w", err)
			}
			price = maximum - 10
		}
		result = map[string]any{"status": "success", "products": []any{map[string]any{"product_id": "PROD1", "name": query + " Premium", "price": price}}}
	case "add_to_cart":
		productID, err := requiredString(arguments, "product_id")
		if err != nil {
			return nil, err
		}
		quantity := 1
		if _, exists := arguments["quantity"]; exists {
			quantity, err = requiredInt(arguments, "quantity")
			if err != nil {
				return nil, err
			}
		}
		result = map[string]any{"status": "success", "product_id": productID, "quantity": quantity, "cart_total": 99.99 * float64(quantity)}
	default:
		return nil, fmt.Errorf("unknown FDB v3 mock API %q", name)
	}
	return json.Marshal(result)
}

func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		return nil, errors.New("arguments must be an object")
	}
	return arguments, nil
}

func requiredString(arguments map[string]any, name string) (string, error) {
	value, exists := arguments[name]
	text, valid := value.(string)
	if !exists || !valid || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("argument %s must be a non-empty string", name)
	}
	return text, nil
}

func optionalString(arguments map[string]any, name, fallback string) string {
	if value, exists := arguments[name]; exists {
		if text, valid := value.(string); valid && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return fallback
}

func requiredFloat(arguments map[string]any, name string) (float64, error) {
	value, exists := arguments[name]
	if !exists {
		return 0, fmt.Errorf("missing required argument %s", name)
	}
	result, err := numberValue(value)
	if err != nil {
		return 0, fmt.Errorf("argument %s: %w", name, err)
	}
	return result, nil
}

func requiredInt(arguments map[string]any, name string) (int, error) {
	value, err := requiredFloat(arguments, name)
	if err != nil {
		return 0, err
	}
	integer := int(value)
	if float64(integer) != value {
		return 0, fmt.Errorf("argument %s must be an integer", name)
	}
	return integer, nil
}

func numberValue(value any) (float64, error) {
	switch number := value.(type) {
	case json.Number:
		return number.Float64()
	case float64:
		return number, nil
	default:
		return 0, fmt.Errorf("must be a number, got %T", value)
	}
}

func divideMilliseconds(value *float64) *float64 {
	if value == nil {
		return nil
	}
	seconds := *value / 1000
	return &seconds
}

func seconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Second)
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeJSONAtomic(filename string, value any) error {
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".fdbv3-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary FDB v3 result: %w", err)
	}
	name := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode FDB v3 result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync FDB v3 result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close FDB v3 result: %w", err)
	}
	if err := os.Rename(name, filename); err != nil {
		return fmt.Errorf("publish FDB v3 result: %w", err)
	}
	keep = true
	return nil
}

func Select(samples []Sample, exampleID, pid string, limit int) []Sample {
	selected := slices.Clone(samples)
	selected = slices.DeleteFunc(selected, func(sample Sample) bool {
		return (exampleID != "" && sample.ExampleID != exampleID) || (pid != "" && sample.PID != pid)
	})
	if limit > 0 && limit < len(selected) {
		selected = selected[:limit]
	}
	return selected
}
