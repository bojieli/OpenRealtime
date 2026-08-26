package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// resumePrompt is what a continuation is given when the trajectory ends in
// model output.
//
// It exists because assistant prefill is rejected on current Claude models: a
// request whose last message is the assistant's is not "continue from here",
// it is an error. The slow phase reaching this point means it was asked to
// carry on with work it had already started, so that is what the prompt says.
const resumePrompt = "Please finish the task."

// compiledBlock is one content block with the role it belongs to.
type compiledBlock struct {
	role string
	// toolResult marks a block that has to lead its user message.
	toolResult bool
	raw        json.RawMessage
}

func (adapter *Adapter) buildRequest(request continuation.Request) (messagesRequest, error) {
	maxTokens := request.Invocation.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	result := messagesRequest{
		Model: adapter.descriptor.Model, MaxTokens: maxTokens, Stream: true,
	}
	if err := adapter.applyThinking(&result, maxTokens); err != nil {
		return messagesRequest{}, err
	}

	system, err := adapter.systemBlocks(request)
	if err != nil {
		return messagesRequest{}, err
	}
	result.System = system

	resolved := resolvedToolCalls(request.Trajectory)
	assistantVisibility := trajectory.AssistantVisibility(request.Trajectory)
	cancelledInvocations := trajectory.CancelledAssistantInvocations(request.Trajectory)
	native, err := adapter.nativeInvocations(request, resolved)
	if err != nil {
		return messagesRequest{}, err
	}

	var blocks []compiledBlock
	consumed := make(map[string]struct{})
	elapsed := continuation.ElapsedNotes(request.Trajectory.Items)
	selectedMedia := continuation.LatestMediaHandles(request.Trajectory.Items)
	for _, item := range request.Trajectory.Items {
		if _, cancelled := cancelledInvocations[item.InvocationID]; cancelled && item.InvocationID != "" {
			continue
		}
		if item.Kind == trajectory.KindInstruction || item.Kind == trajectory.KindAssistantState {
			continue
		}
		if item.Kind == trajectory.KindAssistant && assistantVisibility[item.ID] == trajectory.VisibilityCancelled {
			continue
		}
		modelItem := isModelOutputItem(item.Kind)
		if content, retained := native[item.InvocationID]; modelItem && retained && item.InvocationID != "" {
			if _, already := consumed[item.InvocationID]; !already {
				// Retained state is what this provider itself emitted, so it
				// arrives already shaped as an assistant turn. An answer that
				// was never spoken is not one, whichever path renders it.
				if continuation.ProducedSilently(item) {
					// Retained state keeps this provider's own shape, so the
					// hint precedes it rather than rewriting it.
					raw, err := textBlock(continuation.BackgroundResultHint)
					if err != nil {
						return messagesRequest{}, err
					}
					blocks = append(blocks, compiledBlock{role: "user", raw: raw})
				}
				for _, raw := range content {
					blocks = append(blocks, compiledBlock{role: "assistant", raw: raw})
				}
				consumed[item.InvocationID] = struct{}{}
			}
			continue
		}
		if _, already := consumed[item.InvocationID]; modelItem && already && item.InvocationID != "" {
			continue
		}
		compiled, err := compileItem(
			item, request.Media, adapter.descriptor.Vision, selectedMedia, resolved, elapsed,
		)
		if err != nil {
			return messagesRequest{}, err
		}
		blocks = append(blocks, compiled...)
	}

	pending := len(trajectory.PendingRepairs(request.Trajectory)) > 0
	if pending {
		raw, err := json.Marshal(map[string]string{"type": "text", "text": continuation.PendingRepairPrompt})
		if err != nil {
			return messagesRequest{}, err
		}
		blocks = append(blocks, compiledBlock{role: "user", raw: raw})
	}
	messages, err := groupMessages(blocks)
	if err != nil {
		return messagesRequest{}, err
	}
	if len(messages) == 0 {
		return messagesRequest{}, errors.New(
			"Anthropic continuation requires an observation, a prior model item, or an instruction")
	}
	if messages[0].Role != "user" {
		return messagesRequest{}, errors.New(
			"Anthropic continuation must begin with an observation; the trajectory starts with model output")
	}
	// The Messages API has no way to say "keep going". A trailing assistant
	// message is a prefill, which current models reject outright, so a
	// continuation over model output has to ask for something.
	if messages[len(messages)-1].Role == "assistant" && !pending {
		raw, err := json.Marshal(map[string]string{"type": "text", "text": resumePrompt})
		if err != nil {
			return messagesRequest{}, err
		}
		messages = append(messages, message{Role: "user", Content: []json.RawMessage{raw}})
	}
	result.Messages = messages

	if len(request.Invocation.Tools) > 0 {
		tools := make([]toolDefinition, 0, len(request.Invocation.Tools))
		for _, tool := range request.Invocation.Tools {
			tools = append(tools, toolDefinition{
				Name: tool.Name, Description: tool.Description,
				InputSchema: append(json.RawMessage(nil), tool.Parameters...),
			})
		}
		result.Tools = tools
	}
	return result, nil
}

// applyThinking renders the extended-thinking request for this profile.
func (adapter *Adapter) applyThinking(result *messagesRequest, maxTokens int) error {
	switch adapter.config.Thinking {
	case ThinkingAdaptive:
		if adapter.config.IncludeThoughts {
			result.Thinking = json.RawMessage(`{"type":"adaptive","display":"summarized"}`)
		} else {
			result.Thinking = json.RawMessage(`{"type":"adaptive"}`)
		}
	case ThinkingDisabled:
		result.Thinking = json.RawMessage(`{"type":"disabled"}`)
	case ThinkingBudget:
		if adapter.config.ThinkingBudgetTokens >= maxTokens {
			return fmt.Errorf("Anthropic thinking budget %d must be smaller than the output limit %d",
				adapter.config.ThinkingBudgetTokens, maxTokens)
		}
		encoded, err := json.Marshal(map[string]any{
			"type": "enabled", "budget_tokens": adapter.config.ThinkingBudgetTokens,
		})
		if err != nil {
			return err
		}
		result.Thinking = encoded
	case ThinkingOmitted:
	}
	if !sendsEffort(adapter.config.Thinking) {
		return nil
	}
	if name, named := adapter.config.EffortNames[adapter.config.Effort]; named && name != "" {
		result.OutputConfig = &outputConfig{Effort: name}
	}
	return nil
}

// systemBlocks composes the instruction the model runs under.
func (adapter *Adapter) systemBlocks(request continuation.Request) ([]systemBlock, error) {
	instructions := []string{request.Invocation.Instruction}
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		instructions = append(instructions, continuation.PendingRepairInstruction)
	}
	if len(request.Invocation.Capabilities) > 0 {
		encoded, err := json.Marshal(request.Invocation.Capabilities)
		if err != nil {
			return nil, fmt.Errorf("encode capability manifest: %w", err)
		}
		instructions = append(instructions,
			"The following is the complete capability manifest for this agent. "+
				"Do not deny an available capability merely because another continuation phase executes it:\n"+
				string(encoded))
	}
	text := strings.TrimSpace(strings.Join(instructions, "\n\n"))
	if text == "" {
		return nil, nil
	}
	return []systemBlock{{Type: "text", Text: text}}, nil
}

// nativeInvocations decodes retained assistant content this adapter produced.
//
// Two conditions have to hold before retained state is replayed. The producing
// model must match, because a thinking signature is only valid for the model
// that signed it. And every tool call inside it must have a recorded result,
// because the protocol requires the answer in the very next message and the
// portable compilation is the only form that can describe a call that never
// came back.
func (adapter *Adapter) nativeInvocations(
	request continuation.Request, resolved map[string]struct{},
) (map[string][]json.RawMessage, error) {
	native := make(map[string][]json.RawMessage)
	for _, item := range request.Trajectory.Items {
		if item.ProviderStateType != ProviderStateType || len(item.ProviderState) == 0 || item.InvocationID == "" {
			continue
		}
		if item.Producer.Provider != adapter.descriptor.Provider {
			continue
		}
		if _, duplicate := native[item.InvocationID]; duplicate {
			continue
		}
		var state retainedState
		if err := json.Unmarshal(item.ProviderState, &state); err != nil || len(state.Content) == 0 {
			return nil, fmt.Errorf("decode retained Anthropic state on item %s", item.ID)
		}
		if state.Model != adapter.descriptor.Model {
			continue
		}
		if !replayable(state.Content, resolved) {
			continue
		}
		native[item.InvocationID] = state.Content
	}
	return native, nil
}

// replayable reports whether every tool call in a retained turn was answered.
func replayable(content []json.RawMessage, resolved map[string]struct{}) bool {
	for _, raw := range content {
		var block struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return false
		}
		if block.Type != "tool_use" {
			continue
		}
		if _, answered := resolved[block.ID]; !answered {
			return false
		}
	}
	return true
}

// resolvedToolCalls collects the call IDs a result was recorded for.
func resolvedToolCalls(snapshot trajectory.Snapshot) map[string]struct{} {
	resolved := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if item.Kind == trajectory.KindToolResult && item.ToolResult != nil {
			resolved[item.ToolResult.CallID] = struct{}{}
		}
	}
	return resolved
}

func isModelOutputItem(kind trajectory.Kind) bool {
	return kind == trajectory.KindReasoning || kind == trajectory.KindAssistant ||
		kind == trajectory.KindToolProposal || kind == trajectory.KindToolCall
}

// compileItem renders one trajectory item as portable content blocks.
func compileItem(
	item trajectory.Item, media continuation.MediaResolver, vision bool, selectedMedia map[string]struct{},
	resolved map[string]struct{}, elapsed map[string]string,
) ([]compiledBlock, error) {
	switch item.Kind {
	case trajectory.KindObservation:
		raw, err := textBlock(continuation.ObservationContent(item, elapsed[item.ID]))
		if err != nil {
			return nil, err
		}
		blocks := []compiledBlock{{role: "user", raw: raw}}
		if vision {
			for _, image := range attachMedia(item, media, selectedMedia) {
				blocks = append(blocks, compiledBlock{role: "user", raw: image})
			}
		}
		return blocks, nil
	case trajectory.KindReasoning:
		raw, err := textBlock(continuation.InternalStatePreamble + item.Content)
		if err != nil {
			return nil, err
		}
		return []compiledBlock{{role: "assistant", raw: raw}}, nil
	case trajectory.KindAssistant:
		if strings.TrimSpace(item.Content) == "" {
			return nil, nil
		}
		if continuation.ProducedSilently(item) {
			// A provider that cannot be heard does not take turns. This
			// dialect keeps system content out of the message list, so the
			// hint rides where every other runtime hint here does.
			raw, err := textBlock(continuation.BackgroundResultHint + item.Content)
			if err != nil {
				return nil, err
			}
			return []compiledBlock{{role: "user", raw: raw}}, nil
		}
		raw, err := textBlock(item.Content)
		if err != nil {
			return nil, err
		}
		return []compiledBlock{{role: "assistant", raw: raw}}, nil
	case trajectory.KindToolProposal:
		if item.ToolCall == nil {
			return nil, nil
		}
		return proposalBlock(*item.ToolCall)
	case trajectory.KindToolCall:
		if item.ToolCall == nil {
			return nil, nil
		}
		// A call with no recorded result cannot be sent as tool_use: the very
		// next message would have to answer it, and there is no answer. It is
		// retold as text so the model still knows the attempt happened.
		if _, answered := resolved[item.ToolCall.CallID]; !answered {
			return proposalBlock(*item.ToolCall)
		}
		var input any
		if err := json.Unmarshal(item.ToolCall.Arguments, &input); err != nil {
			return nil, fmt.Errorf("decode tool arguments on item %s: %w", item.ID, err)
		}
		raw, err := json.Marshal(map[string]any{
			"type": "tool_use", "id": item.ToolCall.CallID,
			"name": item.ToolCall.Name, "input": input,
		})
		if err != nil {
			return nil, err
		}
		return []compiledBlock{{role: "assistant", raw: raw}}, nil
	case trajectory.KindToolResult:
		if item.ToolResult == nil {
			return nil, nil
		}
		block := map[string]any{"type": "tool_result", "tool_use_id": item.ToolResult.CallID}
		if item.ToolResult.Error != "" {
			block["is_error"] = true
			block["content"] = item.ToolResult.Error
		} else {
			block["content"] = string(item.ToolResult.Output)
		}
		raw, err := json.Marshal(block)
		if err != nil {
			return nil, err
		}
		return []compiledBlock{{role: "user", toolResult: true, raw: raw}}, nil
	default:
		return nil, nil
	}
}

// proposalBlock renders a call that must not read as an executed one.
func proposalBlock(call trajectory.ToolCall) ([]compiledBlock, error) {
	encoded, err := json.Marshal(map[string]any{
		"non_executable_tool_proposal": map[string]any{
			"name": call.Name, "arguments": json.RawMessage(call.Arguments),
		},
	})
	if err != nil {
		return nil, err
	}
	raw, err := textBlock(string(encoded))
	if err != nil {
		return nil, err
	}
	return []compiledBlock{{role: "assistant", raw: raw}}, nil
}

func textBlock(text string) (json.RawMessage, error) {
	return json.Marshal(map[string]string{"type": "text", "text": text})
}

// groupMessages merges consecutive blocks of one role into one message and
// puts tool results at the front of the message they belong to, which is where
// the API requires them even when the user also spoke while the tool ran.
func groupMessages(blocks []compiledBlock) ([]message, error) {
	var messages []message
	var results []json.RawMessage
	flushResults := func(target *message) {
		if len(results) == 0 {
			return
		}
		target.Content = append(append([]json.RawMessage(nil), results...), target.Content...)
		results = nil
	}
	for _, block := range blocks {
		if len(messages) == 0 || messages[len(messages)-1].Role != block.role {
			if len(messages) > 0 {
				flushResults(&messages[len(messages)-1])
			}
			messages = append(messages, message{Role: block.role})
		}
		current := &messages[len(messages)-1]
		if block.toolResult {
			results = append(results, block.raw)
			continue
		}
		current.Content = append(current.Content, block.raw)
	}
	if len(messages) > 0 {
		flushResults(&messages[len(messages)-1])
	}
	for _, entry := range messages {
		if len(entry.Content) == 0 {
			return nil, errors.New("Anthropic message compiled with no content")
		}
	}
	return messages, nil
}

// attachMedia resolves an observation's handles into image blocks.
//
// A handle that no longer resolves is skipped rather than failing the
// continuation: retention is bounded on purpose, and a model that gets the
// narration without the image is in exactly the state this design expects
// after the window has passed.
func attachMedia(
	item trajectory.Item, media continuation.MediaResolver, selected map[string]struct{},
) []json.RawMessage {
	if media == nil || item.Observation == nil || len(item.Observation.Media) == 0 {
		return nil
	}
	var blocks []json.RawMessage
	for _, reference := range item.Observation.Media {
		if _, ok := selected[reference.Handle]; !ok {
			continue
		}
		resolved, err := media(reference.Handle)
		if err != nil || len(resolved.Bytes) == 0 {
			continue
		}
		mimeType := resolved.MIMEType
		if mimeType == "" {
			mimeType = reference.MIMEType
		}
		encoded, err := json.Marshal(map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": mimeType,
				"data": base64.StdEncoding.EncodeToString(resolved.Bytes),
			},
		})
		if err != nil {
			continue
		}
		blocks = append(blocks, encoded)
	}
	return blocks
}
