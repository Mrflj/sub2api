package apicompat

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// This extension is explicitly negotiated by the local adapter. It is NOT a
// native Anthropic block or a fabricated thinking signature. Other clients
// receive the ordinary Messages response without this block.
const ResponsesHistoryHeader = "X-Sub2api-Responses-History"
const ResponsesHistoryBlockType = "sub2api_responses_history"
const maxResponsesHistoryBytes = 4 * 1024 * 1024
const maxResponsesHistoryItems = 256

type responsesHistoryEnvelope struct {
	Version int               `json:"version"`
	Scope   string            `json:"scope"`
	Output  []json.RawMessage `json:"output"`
}

func validResponsesHistoryScope(scope string) bool {
	if len(scope) != 64 {
		return false
	}
	return strings.IndexFunc(scope, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) == -1
}

func validResponsesHistoryOutput(output []json.RawMessage) bool {
	if len(output) == 0 || len(output) > maxResponsesHistoryItems {
		return false
	}
	for _, raw := range output {
		var item struct {
			Type, Role string
			Content    []struct{ Type string }
		}
		if json.Unmarshal(raw, &item) != nil {
			return false
		}
		switch item.Type {
		case "message":
			if item.Role != "assistant" {
				return false
			}
			for _, part := range item.Content {
				if part.Type != "output_text" && part.Type != "refusal" {
					return false
				}
			}
		case "reasoning", "web_search_call", "function_call", "custom_tool_call":
		default:
			return false
		}
	}
	return true
}

type responsesHistoryCall struct{ ID, Name, Input string }
type responsesHistoryShape struct {
	Text       string
	Calls      []responsesHistoryCall
	Signatures []string
	Searches   []string
}

func messagesHistoryShape(blocks []AnthropicContentBlock) (responsesHistoryShape, bool) {
	var shape responsesHistoryShape
	for _, block := range blocks {
		switch block.Type {
		case "text":
			shape.Text += block.Text
		case "thinking":
			if block.Signature != "" {
				shape.Signatures = append(shape.Signatures, block.Signature)
			}
		case "tool_use":
			var input any
			if json.Unmarshal(block.Input, &input) != nil {
				return shape, false
			}
			canonical, err := json.Marshal(input)
			if err != nil {
				return shape, false
			}
			shape.Calls = append(shape.Calls, responsesHistoryCall{block.ID, block.Name, string(canonical)})
		case "server_tool_use", "web_search_tool_result":
			// Compare association, query, sources and error payloads as data.
			block.CacheControl = nil
			data, err := json.Marshal(block)
			if err != nil {
				return shape, false
			}
			var canonical any
			if json.Unmarshal(data, &canonical) != nil {
				return shape, false
			}
			data, _ = json.Marshal(canonical)
			shape.Searches = append(shape.Searches, string(data))
		case ResponsesHistoryBlockType:
		default:
			return shape, false
		}
	}
	return shape, true
}

func historyOutputItems(output []json.RawMessage) ([]ResponsesOutput, bool) {
	if !validResponsesHistoryOutput(output) {
		return nil, false
	}
	items := make([]ResponsesOutput, len(output))
	for index, raw := range output {
		if json.Unmarshal(raw, &items[index]) != nil {
			return nil, false
		}
	}
	return items, true
}

func historyMatchesBlocks(output []json.RawMessage, blocks []AnthropicContentBlock) bool {
	items, ok := historyOutputItems(output)
	if !ok {
		return false
	}
	expected := ResponsesToAnthropic(&ResponsesResponse{Status: "completed", Output: items}, "")
	want, ok := messagesHistoryShape(expected.Content)
	if !ok {
		return false
	}
	got, ok := messagesHistoryShape(blocks)
	return ok && reflect.DeepEqual(want, got)
}

// AttachResponsesHistory carries actual upstream output (including opaque
// reasoning ciphertext, hosted tool actions/sources and message phase) exactly
// once. The ordinary visible answer/calls must match before state is attached.
func AttachResponsesHistory(response *AnthropicResponse, original *ResponsesResponse, scope string) bool {
	if response == nil || original == nil || original.Status != "completed" || !validResponsesHistoryScope(scope) {
		return false
	}
	output := make([]json.RawMessage, 0, len(original.Output))
	for _, item := range original.Output {
		raw := item.rawOutput
		if len(raw) == 0 {
			var err error
			raw, err = json.Marshal(item)
			if err != nil {
				return false
			}
		}
		output = append(output, raw)
	}
	return attachResponsesHistoryOutput(response, output, scope)
}

func attachResponsesHistoryOutput(response *AnthropicResponse, output []json.RawMessage, scope string) bool {
	if !validResponsesHistoryScope(scope) || !historyMatchesBlocks(output, response.Content) {
		return false
	}
	for _, block := range response.Content {
		if block.Type == ResponsesHistoryBlockType {
			return false
		}
	}
	content, err := json.Marshal(responsesHistoryEnvelope{Version: 1, Scope: scope, Output: output})
	if err != nil || len(content) > maxResponsesHistoryBytes {
		return false
	}
	response.Content = append(response.Content, AnthropicContentBlock{Type: ResponsesHistoryBlockType, Content: content})
	return true
}

func restoreResponsesHistory(blocks []AnthropicContentBlock, scope string) ([]ResponsesInputItem, bool, error) {
	var raw json.RawMessage
	for _, block := range blocks {
		if block.Type != ResponsesHistoryBlockType {
			continue
		}
		if raw != nil {
			return nil, false, unsupportedRequestConversion("duplicate Responses history state")
		}
		raw = block.Content
	}
	if raw == nil {
		return nil, false, nil
	}
	if len(raw) > maxResponsesHistoryBytes {
		return nil, false, unsupportedRequestConversion("Responses history state exceeds the supported limit")
	}
	var state responsesHistoryEnvelope
	if json.Unmarshal(raw, &state) != nil || state.Version != 1 || !validResponsesHistoryScope(state.Scope) || !validResponsesHistoryOutput(state.Output) {
		return nil, false, unsupportedRequestConversion("invalid Responses history state")
	}
	// State from another upstream account/model is opaque foreign state. Keep
	// the visible conversation but never send its ciphertext to that account.
	if !validResponsesHistoryScope(scope) || state.Scope != scope {
		return nil, false, nil
	}
	if !historyMatchesBlocks(state.Output, blocks) {
		return nil, false, unsupportedRequestConversion("Responses history does not match the assistant message")
	}
	items := make([]ResponsesInputItem, len(state.Output))
	for index, item := range state.Output {
		if err := json.Unmarshal(item, &items[index]); err != nil {
			return nil, false, unsupportedRequestConversion("invalid Responses history item")
		}
	}
	return items, true, nil
}

// Capture only original provider items. No model invocation, invented search
// result or synthetic signature is involved in preserving this state.
type responsesHistoryStream struct {
	output         map[int]json.RawMessage
	blocks         map[int]*AnthropicContentBlock
	valid          bool
	bytes          int
	deliveredBytes int
}

func (state *ResponsesEventToAnthropicState) captureResponsesHistory(event *ResponsesStreamEvent) {
	if !validResponsesHistoryScope(state.ResponsesHistoryScope) {
		return
	}
	if state.history == nil {
		state.history = &responsesHistoryStream{output: map[int]json.RawMessage{}, blocks: map[int]*AnthropicContentBlock{}, valid: true}
	}
	history := state.history
	if !history.valid {
		return
	}
	if event.Type == "response.output_item.done" && event.Item != nil && event.OutputIndex >= 0 {
		raw := event.Item.rawOutput
		if len(raw) == 0 {
			raw, _ = json.Marshal(event.Item)
		}
		history.bytes += len(raw) - len(history.output[event.OutputIndex])
		if len(history.output) >= maxResponsesHistoryItems && history.output[event.OutputIndex] == nil || history.bytes > maxResponsesHistoryBytes {
			history.valid = false
			return
		}
		history.output[event.OutputIndex] = append(json.RawMessage(nil), raw...)
	}
}

func (state *ResponsesEventToAnthropicState) withResponsesHistory(event *ResponsesStreamEvent, events []AnthropicStreamEvent) []AnthropicStreamEvent {
	history := state.history
	if history == nil || !history.valid {
		return events
	}
	for _, emitted := range events {
		if emitted.Index == nil {
			continue
		}
		index := *emitted.Index
		switch emitted.Type {
		case "content_block_start":
			if emitted.ContentBlock == nil {
				history.valid = false
				return events
			}
			copy := *emitted.ContentBlock
			history.deliveredBytes += len(copy.Text) + len(copy.Input) + len(copy.Content) + len(copy.Signature)
			if len(history.blocks) >= maxResponsesHistoryItems*3 || history.deliveredBytes > maxResponsesHistoryBytes {
				history.valid = false
				return events
			}
			history.blocks[index] = &copy
		case "content_block_delta":
			block, delta := history.blocks[index], emitted.Delta
			if block == nil || delta == nil {
				history.valid = false
				return events
			}
			history.deliveredBytes += len(delta.Text) + len(delta.Signature) + len(delta.PartialJSON)
			if history.deliveredBytes > maxResponsesHistoryBytes {
				history.valid = false
				return events
			}
			switch delta.Type {
			case "text_delta":
				block.Text += delta.Text
			case "thinking_delta": // Never copy private thought text into diagnostic state.
			case "signature_delta":
				block.Signature += delta.Signature
			case "input_json_delta":
				if bytes.Equal(bytes.TrimSpace(block.Input), []byte("{}")) {
					block.Input = nil
				}
				block.Input = append(block.Input, delta.PartialJSON...)
			}
		}
	}
	if !state.MessageStopSent || event.Response == nil || event.Response.Status != "completed" {
		return events
	}
	indexes := make([]int, 0, len(history.blocks))
	for index := range history.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	response := &AnthropicResponse{}
	for _, index := range indexes {
		response.Content = append(response.Content, *history.blocks[index])
	}
	var attached bool
	if len(event.Response.Output) > 0 {
		attached = AttachResponsesHistory(response, event.Response, state.ResponsesHistoryScope)
	} else {
		indexes = indexes[:0]
		for index := range history.output {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		output := make([]json.RawMessage, 0, len(indexes))
		for _, index := range indexes {
			output = append(output, history.output[index])
		}
		attached = attachResponsesHistoryOutput(response, output, state.ResponsesHistoryScope)
	}
	if !attached {
		return events
	}
	block := response.Content[len(response.Content)-1]
	index := state.ContentBlockIndex
	state.ContentBlockIndex++
	additional := []AnthropicStreamEvent{{Type: "content_block_start", Index: &index, ContentBlock: &block}, {Type: "content_block_stop", Index: &index}}
	for pos, emitted := range events {
		if emitted.Type == "message_delta" || emitted.Type == "message_stop" {
			out := append([]AnthropicStreamEvent(nil), events[:pos]...)
			out = append(out, additional...)
			return append(out, events[pos:]...)
		}
	}
	return events
}
