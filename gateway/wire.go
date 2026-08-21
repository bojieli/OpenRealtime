package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type sessionUpdateEvent struct {
	Type    string            `json:"type"`
	Session sessionUpdateBody `json:"session"`
}

type sessionUpdateBody struct {
	Type             string           `json:"type"`
	Instructions     *string          `json:"instructions"`
	OutputModalities []string         `json:"output_modalities"`
	Tools            []wireTool       `json:"tools"`
	ToolChoice       json.RawMessage  `json:"tool_choice"`
	Audio            wireAudioConfig  `json:"audio"`
	Reasoning        *json.RawMessage `json:"reasoning,omitempty"`
	// OpenRealtime carries the protocol extension. A base-protocol client
	// never sends it, and a base-protocol server would ignore it.
	OpenRealtime *openrealtime.Request `json:"openrealtime,omitempty"`
}

type wireAudioConfig struct {
	Input  wireAudioInput  `json:"input"`
	Output wireAudioOutput `json:"output"`
}

type wireAudioInput struct {
	Format        flexibleAudioFormat `json:"format"`
	Transcription json.RawMessage     `json:"transcription"`
	TurnDetection *wireTurnDetection  `json:"turn_detection"`
}

type wireAudioOutput struct {
	Format flexibleAudioFormat `json:"format"`
	Voice  string              `json:"voice"`
	Speed  float64             `json:"speed"`
}

type flexibleAudioFormat struct{ audioFormat }

// UnmarshalJSON accepts both the GA format object and the earlier string form.
// Clients in the field send both, and refusing one of them would make the
// server incompatible with software that works against the official API.
func (format *flexibleAudioFormat) UnmarshalJSON(input []byte) error {
	var legacy string
	if err := json.Unmarshal(input, &legacy); err == nil {
		switch legacy {
		case "g711_ulaw":
			format.audioFormat = audioFormat{Type: formatPCMU}
		case "pcm16":
			format.audioFormat = audioFormat{Type: formatPCM16, Rate: 24_000}
		default:
			return fmt.Errorf("unsupported legacy audio format %q", legacy)
		}
		return nil
	}
	var object audioFormat
	if err := json.Unmarshal(input, &object); err != nil {
		return errors.New("audio format must be a format object")
	}
	if _, err := object.sampleRate(); err != nil {
		return err
	}
	format.audioFormat = object
	return nil
}

type wireTurnDetection struct {
	Type              string  `json:"type"`
	Threshold         float64 `json:"threshold"`
	PrefixPaddingMS   int     `json:"prefix_padding_ms"`
	SilenceDurationMS int     `json:"silence_duration_ms"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// OpenRealtime carries the additive per-tool declaration. Servers that do
	// not understand the key ignore it, as JSON Schema requires.
	OpenRealtime *openrealtime.ToolExtension `json:"openrealtime,omitempty"`
}

func (tool wireTool) spec() (action.ToolSpec, error) {
	if tool.Type != "function" || strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Description) == "" {
		return action.ToolSpec{}, errors.New("Realtime tools require function type, name, and description")
	}
	if len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) {
		return action.ToolSpec{}, fmt.Errorf("tool %q parameters must be valid JSON", tool.Name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(tool.Parameters, &object); err != nil || object == nil {
		return action.ToolSpec{}, fmt.Errorf("tool %q parameters must be a JSON object", tool.Name)
	}
	spec := action.ToolSpec{
		Name: strings.TrimSpace(tool.Name), Description: strings.TrimSpace(tool.Description),
		Parameters: bytes.Clone(tool.Parameters),
	}
	if tool.OpenRealtime != nil {
		confirm, err := action.ParseConfirm(tool.OpenRealtime.Confirm)
		if err != nil {
			return action.ToolSpec{}, fmt.Errorf("tool %q: %w", tool.Name, err)
		}
		spec.Confirm = confirm
		spec.Target = strings.TrimSpace(tool.OpenRealtime.Target)
	}
	return spec, nil
}

type audioAppendEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
}

type truncateEvent struct {
	Type         string `json:"type"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	AudioEndMS   int    `json:"audio_end_ms"`
}

type conversationItemCreateEvent struct {
	Type string `json:"type"`
	Item struct {
		ID      string        `json:"id"`
		Type    string        `json:"type"`
		Role    string        `json:"role"`
		CallID  string        `json:"call_id"`
		Output  string        `json:"output"`
		Content []wireContent `json:"content"`
	} `json:"item"`
}

type wireContent struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	Transcript string `json:"transcript"`
}

// text returns whatever text a content array carries, whichever field it is
// in. A client sending input_text and one sending a transcript are saying the
// same thing.
func (item conversationItemCreateEvent) text() string {
	var parts []string
	for _, content := range item.Item.Content {
		for _, candidate := range []string{content.Text, content.Transcript} {
			if strings.TrimSpace(candidate) != "" {
				parts = append(parts, candidate)
				break
			}
		}
	}
	return strings.Join(parts, " ")
}

func event(eventType, eventID string, fields map[string]any) map[string]any {
	result := map[string]any{"type": eventType, "event_id": eventID}
	for name, value := range fields {
		result[name] = value
	}
	return result
}

func responseObject(
	id, status, conversationID string, output []map[string]any,
	usage *continuation.Usage, outputFormat audioFormat, voice string, modalities []string,
) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	if len(modalities) == 0 {
		modalities = []string{"audio"}
	}
	result := map[string]any{
		"object": "realtime.response", "id": id, "status": status,
		"output": output, "conversation_id": conversationID,
		"output_modalities": modalities, "max_output_tokens": "inf",
		"audio": map[string]any{"output": map[string]any{"format": outputFormat, "voice": voice}},
	}
	if usage != nil {
		input := max(usage.InputTokens, 0)
		cachedInput := min(max(usage.CachedInputTokens, 0), input)
		outputTokens := max(usage.OutputTokens, 0)
		if outputTokens == 0 {
			outputTokens = 1
		}
		total := max(usage.TotalTokens, input+outputTokens)
		result["usage"] = map[string]any{
			"total_tokens": total, "input_tokens": input, "output_tokens": outputTokens,
			"input_token_details": map[string]any{
				"text_tokens": input, "audio_tokens": 0, "image_tokens": 0,
				"cached_tokens":         cachedInput,
				"cached_tokens_details": map[string]any{"text_tokens": cachedInput, "audio_tokens": 0, "image_tokens": 0},
			},
			"output_token_details": map[string]any{"text_tokens": outputTokens, "audio_tokens": 0},
		}
	}
	return result
}

// assistantItem renders one assistant turn as a conversation item.
//
// The content part names what the turn actually carried. An audio turn's text
// is a transcript of samples the client received; a text turn's text is the
// turn, and calling it a transcript would describe audio that does not exist.
func assistantItem(id, status, text string, textOnly bool) map[string]any {
	content := map[string]any{"type": "output_audio", "transcript": text}
	if textOnly {
		content = map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{
		"id": id, "object": "realtime.item", "type": "message", "status": status,
		"role": "assistant", "content": []map[string]any{content},
	}
}

func functionCallItem(id, status string, call trajectory.ToolCall) map[string]any {
	return map[string]any{
		"id": id, "object": "realtime.item", "type": "function_call", "status": status,
		"call_id": call.CallID, "name": call.Name, "arguments": string(call.Arguments),
	}
}

func functionOutputItem(id, callID, output string) map[string]any {
	return map[string]any{
		"id": id, "object": "realtime.item", "type": "function_call_output",
		"status": "completed", "call_id": callID, "output": output,
	}
}

func userAudioItem(id, transcript string) map[string]any {
	return map[string]any{
		"id": id, "object": "realtime.item", "type": "message", "status": "completed",
		"role": "user", "content": []map[string]any{{"type": "input_audio", "transcript": transcript}},
	}
}

func encodeToolResult(callID, name, output string) trajectory.ToolResult {
	raw := json.RawMessage(output)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(output)
	}
	return trajectory.ToolResult{CallID: callID, Name: name, Output: raw}
}
