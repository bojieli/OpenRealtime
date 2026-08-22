package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/internal/sse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// streamEvent is the union of Messages API stream events this adapter reads.
// Events it does not name - ping, and anything added later - are ignored,
// because a stream format that grows must not break a client that was correct
// when it was written.
type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		StopReason string `json:"stop_reason"`
		Usage      usage  `json:"usage"`
	} `json:"message,omitempty"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"`
	} `json:"content_block,omitempty"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta,omitempty"`
	Usage *usage `json:"usage,omitempty"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type usage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// openBlock is one content block being assembled from deltas.
type openBlock struct {
	kind      string
	id        string
	name      string
	data      string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	arguments strings.Builder
}

// consumeStream turns the Messages event stream into portable events and a
// replayable assistant content array.
//
// The content array is rebuilt rather than captured, because the stream never
// sends the assembled message: a thinking block arrives as text deltas and
// then a signature delta, and only the pair together is replayable. Losing the
// signature would make the next continuation reject the history it was given.
func (adapter *Adapter) consumeStream(
	body io.Reader, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	completion := continuation.Completion{ProviderStateType: ProviderStateType}
	blocks := make(map[int]*openBlock)
	var order []int
	callIndex := 0
	sawToolCall := false

	finish := func(index int) error {
		block, open := blocks[index]
		if !open {
			return nil
		}
		if block.kind != "tool_use" {
			return nil
		}
		sawToolCall = true
		arguments := strings.TrimSpace(block.arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return fmt.Errorf("Anthropic tool call %q streamed invalid JSON arguments", block.name)
		}
		callID := strings.TrimSpace(block.id)
		if callID == "" {
			callIndex++
			callID = fmt.Sprintf("%s-call-%d", request.InvocationID, callIndex)
		}
		return emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: callID, Name: block.name, Arguments: json.RawMessage(arguments),
		}})
	}

	err := sse.Read(body, maxSSEEvent, func(data []byte) error {
		var event streamEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return fmt.Errorf("decode Anthropic stream event: %w", err)
		}
		switch event.Type {
		case "error":
			if event.Error != nil {
				return fmt.Errorf("Anthropic stream error %s: %s", event.Error.Type, event.Error.Message)
			}
			return fmt.Errorf("Anthropic stream reported an error without detail")
		case "message_start":
			if event.Message != nil {
				applyUsage(&completion.Usage, event.Message.Usage)
			}
		case "content_block_start":
			if event.ContentBlock == nil {
				return nil
			}
			block := &openBlock{
				kind: event.ContentBlock.Type, id: event.ContentBlock.ID,
				name: event.ContentBlock.Name, data: event.ContentBlock.Data,
			}
			blocks[event.Index] = block
			order = append(order, event.Index)
		case "content_block_delta":
			block, open := blocks[event.Index]
			if !open || event.Delta == nil {
				return nil
			}
			switch event.Delta.Type {
			case "text_delta":
				block.text.WriteString(event.Delta.Text)
				if event.Delta.Text == "" {
					return nil
				}
				return emit(continuation.Event{
					Kind: continuation.EventAssistantDelta, Text: event.Delta.Text,
				})
			case "thinking_delta":
				block.thinking.WriteString(event.Delta.Thinking)
				if event.Delta.Thinking == "" {
					return nil
				}
				return emit(continuation.Event{
					Kind: continuation.EventReasoningDelta, Text: event.Delta.Thinking,
				})
			case "signature_delta":
				block.signature.WriteString(event.Delta.Signature)
			case "input_json_delta":
				block.arguments.WriteString(event.Delta.PartialJSON)
			}
		case "content_block_stop":
			return finish(event.Index)
		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				completion.StopReason = event.Delta.StopReason
			}
			if event.Usage != nil {
				applyUsage(&completion.Usage, *event.Usage)
			}
		}
		return nil
	})

	state := assembleState(blocks, order)
	// A stream that failed after emitting a tool call has already put an
	// executable effect into the trajectory. Retaining a partial native state
	// alongside it would let the next continuation replay a turn that never
	// finished, so the portable record is what survives.
	if len(state) > 0 && !(err != nil && sawToolCall) {
		encoded, marshalErr := json.Marshal(retainedState{Model: adapter.descriptor.Model, Content: state})
		if marshalErr != nil {
			return completion, fmt.Errorf("encode retained Anthropic state: %w", marshalErr)
		}
		completion.ProviderState = encoded
	} else {
		completion.ProviderStateType = ""
	}
	return completion, err
}

// retainedState wraps an assistant content array with the model that produced
// it. Thinking signatures are model-specific, so replaying them against a
// different model is a request the provider rejects; recording the model is
// what lets the replay path notice before sending it.
type retainedState struct {
	Model   string            `json:"model"`
	Content []json.RawMessage `json:"content"`
}

// assembleState rebuilds the assistant content array in stream order.
func assembleState(blocks map[int]*openBlock, order []int) []json.RawMessage {
	var content []json.RawMessage
	for _, index := range order {
		block, open := blocks[index]
		if !open {
			continue
		}
		var encoded []byte
		var err error
		switch block.kind {
		case "text":
			if block.text.Len() == 0 {
				continue
			}
			encoded, err = json.Marshal(map[string]string{"type": "text", "text": block.text.String()})
		case "thinking":
			// A thinking block without its signature cannot be replayed, and
			// sending one is rejected rather than ignored. Dropping it keeps
			// the rest of the turn usable.
			if block.signature.Len() == 0 {
				continue
			}
			encoded, err = json.Marshal(map[string]string{
				"type": "thinking", "thinking": block.thinking.String(),
				"signature": block.signature.String(),
			})
		case "redacted_thinking":
			if block.data == "" {
				continue
			}
			encoded, err = json.Marshal(map[string]string{"type": "redacted_thinking", "data": block.data})
		case "tool_use":
			arguments := strings.TrimSpace(block.arguments.String())
			if arguments == "" {
				arguments = "{}"
			}
			if !json.Valid([]byte(arguments)) {
				continue
			}
			encoded, err = json.Marshal(map[string]any{
				"type": "tool_use", "id": block.id, "name": block.name,
				"input": json.RawMessage(arguments),
			})
		default:
			continue
		}
		if err != nil {
			continue
		}
		content = append(content, encoded)
	}
	return content
}

func applyUsage(target *continuation.Usage, reported usage) {
	if reported.InputTokens != 0 {
		target.InputTokens = reported.InputTokens
	}
	if reported.OutputTokens != 0 {
		target.OutputTokens = reported.OutputTokens
	}
	if reported.CacheReadInputTokens != nil {
		target.CachedInputTokens = *reported.CacheReadInputTokens
		target.CachedInputTokensReported = true
	}
	target.TotalTokens = target.InputTokens + target.OutputTokens
}
