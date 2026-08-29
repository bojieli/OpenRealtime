package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	imagelib "image"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/bojieli/OpenRealtime/perception"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
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
	// TurnDetectionSet distinguishes an explicit null from an absent field.
	// They mean opposite things: absent leaves turn detection as it was, and
	// null is the client taking the floor.
	TurnDetectionSet bool `json:"-"`
}

// UnmarshalJSON records whether turn_detection was present at all.
func (input *wireAudioInput) UnmarshalJSON(data []byte) error {
	type plain wireAudioInput
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, present := fields["turn_detection"]
	*input = wireAudioInput(decoded)
	input.TurnDetectionSet = present
	return nil
}

// turnDetection renders the session's turn detection, which is an object when
// the server owns it and null when the client does.
func turnDetection(current settings) any {
	if current.manualTurns {
		return nil
	}
	return map[string]any{
		"type": "server_vad", "threshold": current.gate.Threshold,
		"prefix_padding_ms":   current.gate.PrefixPaddingMS,
		"silence_duration_ms": current.gate.SilenceDurationMS,
		"create_response":     true, "interrupt_response": true,
	}
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

// wireTurnDetection is a client's turn-detection request.
//
// The numbers are pointers for the same reason TurnDetectionSet exists one
// level up: absent and zero are different requests. A client that names a
// detector and none of its parameters is asking for the deployment's, and
// reading that as zero produces a gate with no silence duration - which the
// gate refuses, so the session fails on a field the client never set. OpenAI's
// own SDK sends exactly that: a type and nothing else.
type wireTurnDetection struct {
	Type              string   `json:"type"`
	Threshold         *float64 `json:"threshold"`
	PrefixPaddingMS   *int     `json:"prefix_padding_ms"`
	SilenceDurationMS *int     `json:"silence_duration_ms"`
}

// gate resolves what the client asked for against the deployment's defaults.
func (turn *wireTurnDetection) gate() perception.GateConfig {
	resolved := perception.DefaultGateConfig()
	if turn == nil {
		return resolved
	}
	if turn.Threshold != nil {
		resolved.Threshold = *turn.Threshold
	}
	if turn.PrefixPaddingMS != nil {
		resolved.PrefixPaddingMS = *turn.PrefixPaddingMS
	}
	if turn.SilenceDurationMS != nil {
		resolved.SilenceDurationMS = *turn.SilenceDurationMS
	}
	return resolved
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
		spec.Background = tool.OpenRealtime.Background
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
	// ImageURL carries an input_image, which clients send as a data URI.
	ImageURL string `json:"image_url"`
	// Detail is the model-side resolution hint. It is decoded so a client
	// that sends it is not refused, and ignored: what an adapter does with an
	// image is the adapter's business.
	Detail string `json:"detail,omitempty"`
}

// images decodes whatever pictures a content array carries.
//
// Clients send them as data URIs, which is what the official SDKs produce, and
// a bare base64 payload is accepted too because some clients send that. A
// content part that is neither is a client error worth reporting rather than
// an image worth guessing at.
func (item conversationItemCreateEvent) images(limit int) ([]binding.Image, error) {
	var images []binding.Image
	for _, content := range item.Item.Content {
		if content.Type != "input_image" && strings.TrimSpace(content.ImageURL) == "" {
			continue
		}
		payload, mime, err := decodeImageURL(content.ImageURL)
		if err != nil {
			return nil, err
		}
		if limit > 0 && len(payload) > limit {
			return nil, fmt.Errorf("attached image of %d bytes exceeds the %d byte limit", len(payload), limit)
		}
		image := binding.Image{Bytes: payload, MIMEType: mime}
		if config, _, err := imagelib.DecodeConfig(bytes.NewReader(payload)); err == nil {
			image.Width, image.Height = config.Width, config.Height
		}
		images = append(images, image)
	}
	return images, nil
}

func decodeImageURL(value string) ([]byte, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, "", errors.New("an input_image requires image data")
	}
	mime := "image/jpeg"
	if strings.HasPrefix(value, "data:") {
		comma := strings.Index(value, ",")
		if comma < 0 {
			return nil, "", errors.New("an input_image data URI requires a comma")
		}
		header := value[len("data:"):comma]
		value = value[comma+1:]
		if !strings.HasSuffix(header, ";base64") {
			return nil, "", errors.New("an input_image data URI must be base64")
		}
		if declared := strings.TrimSuffix(header, ";base64"); declared != "" {
			mime = declared
		}
	}
	payload, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, "", errors.New("input_image data is not valid base64")
	}
	if len(payload) == 0 {
		return nil, "", errors.New("an input_image requires image data")
	}
	return payload, mime, nil
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
	// The base Realtime function-call-output item has no error member. Clients
	// therefore use the convention shared by the OpenAI-compatible tool stacks:
	// a plain output beginning with "Error:" is a failed invocation. Normalize
	// that transport encoding here so the interaction policy can distinguish a
	// failure from a successful result and avoid retrying it without new input.
	//
	// Keep the inference deliberately narrow and anchored. An ordinary payload
	// may contain an error field, an error log, or the word later in its text;
	// none of those says that the invocation itself failed.
	trimmed := strings.TrimSpace(output)
	const errorPrefix = "error:"
	if len(trimmed) > len(errorPrefix) && strings.EqualFold(trimmed[:len(errorPrefix)], errorPrefix) {
		if message := strings.TrimSpace(trimmed[len(errorPrefix):]); message != "" {
			return trajectory.ToolResult{CallID: callID, Name: name, Error: message}
		}
	}
	raw := json.RawMessage(output)
	if !json.Valid(raw) {
		raw, _ = json.Marshal(output)
	}
	return trajectory.ToolResult{CallID: callID, Name: name, Output: raw}
}

// unappliedFields reports the session settings this deployment parses but does
// not act on.
//
// The turn-detection refusal above explains the reasoning at length and these
// follow it: everything else in the session.update applies, the field does
// not, and the client is told by name. What that reasoning rules out is the
// thing these fields were doing instead - being dropped in silence while
// session.updated reports the value actually in force, so the only way to
// discover the difference is to read a field back and notice it changed. Most
// clients never read it back.
//
// tool_choice is the one with teeth. A client that asks for no tools and gets
// tool calls anyway is not looking at a cosmetic difference; it is looking at
// behaviour it explicitly turned off. The others are quieter, and reasoning is
// quieter still, because it is not even echoed - a client setting it has no
// field to read back at all.
func unappliedFields(update sessionUpdateBody, transcriptionModel string) []clientError {
	var refused []clientError
	if choice := toolChoiceMode(update.ToolChoice); choice != "" && choice != "auto" {
		refused = append(refused, clientError{
			code:  "unsupported_value",
			param: "session.tool_choice",
			message: fmt.Sprintf(
				"tool_choice %q is not supported: which model may call tools is an authority "+
					"boundary here, not a per-session setting - the fast model proposes and "+
					"cannot execute, the background reasoner executes. The field was not "+
					"applied and the rest of the session.update was.", choice),
		})
	}
	if speed := update.Audio.Output.Speed; speed != 0 && speed != 1 {
		refused = append(refused, clientError{
			code:  "unsupported_value",
			param: "session.audio.output.speed",
			message: fmt.Sprintf(
				"speed %g is not supported: this deployment synthesises at the rate its speech "+
					"provider produces. The field was not applied and the rest of the "+
					"session.update was.", speed),
		})
	}
	if model := transcriptionModelOf(update.Audio.Input.Transcription); model != "" && model != transcriptionModel {
		refused = append(refused, clientError{
			code:  "unsupported_value",
			param: "session.audio.input.transcription.model",
			message: fmt.Sprintf(
				"transcription model %q is not supported: the recogniser is the deployment's "+
					"(%s). The field was not applied and the rest of the session.update was.",
				model, transcriptionModel),
		})
	}
	if update.Reasoning != nil && len(*update.Reasoning) > 0 && string(*update.Reasoning) != "null" {
		refused = append(refused, clientError{
			code:  "unsupported_value",
			param: "session.reasoning",
			message: "reasoning is not configurable per session: effort belongs to the provider " +
				"this deployment runs the background reasoner on. The field was not applied " +
				"and the rest of the session.update was.",
		})
	}
	return refused
}

// toolChoiceMode reads the string form of tool_choice. The object form names a
// function, which is equally unsupported, so it reports the whole value.
func toolChoiceMode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		return mode
	}
	return string(raw)
}

// transcriptionModelOf reads the model out of an input transcription config.
func transcriptionModelOf(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var config struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return ""
	}
	return config.Model
}

// outputAudioObject renders the output half of the session's audio settings.
//
// The voice is omitted when nothing can say what it is - a binding forwarding
// to a remote provider does not know the voice that provider will use, and
// naming one anyway would be a guess reported as a fact. An absent field says
// "not stated", which is true; a present one says "this is what you will
// hear", which had better be.
func outputAudioObject(format audioFormat, voice string) map[string]any {
	output := map[string]any{"format": format, "speed": 1}
	if voice != "" {
		output["voice"] = voice
	}
	return output
}

// inForce names the voice a refusal leaves the session with, when there is one
// to name.
func inForce(voice string) string {
	if voice == "" {
		return ""
	}
	return fmt.Sprintf(" and is %q", voice)
}

// maxOutputTokens renders the limit on one spoken turn.
//
// "inf" is the protocol's way of saying there is no maximum, and it is a claim
// rather than a placeholder: a turn cut short reports max_output_tokens as its
// reason, and a client told in one breath that no limit exists and in the next
// that a limit ended its turn has been given two facts that cannot both be
// true. Bindings that genuinely impose none - the ones whose model owns its
// own budget - still report "inf", because for them it is accurate.
func maxOutputTokens(limit int) any {
	if limit <= 0 {
		return "inf"
	}
	return limit
}
