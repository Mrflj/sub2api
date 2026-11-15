package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const historyTestScope = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func historyFixture(t *testing.T) *ResponsesResponse {
	t.Helper()
	var response ResponsesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_fixture","status":"completed","output":[
	 {"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"gAAAA_fixture"},
	 {"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"fixture query","sources":[{"type":"url","url":"https://example.invalid/source"}]}},
	 {"type":"message","id":"msg_fixture","role":"assistant","phase":"commentary","status":"completed","content":[{"type":"output_text","text":"Checking sources.","annotations":[]}]},
	 {"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"todo_write","arguments":"{\"todos\":[{\"content\":\"fixture task\",\"status\":\"in_progress\"}]}","status":"completed"}
	]}`), &response))
	return &response
}

func historyReplay(t *testing.T, blocks []AnthropicContentBlock, scope string) (*ResponsesRequest, error) {
	t.Helper()
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	return AnthropicToResponsesWithOptions(&AnthropicRequest{Model: "gpt-5.5", MaxTokens: 1024, System: json.RawMessage(`"GROUP_SYSTEM_INSTRUCTIONS"`), Messages: []AnthropicMessage{
		{Role: "user", Content: json.RawMessage(`"ORIGINAL_USER_TASK"`)}, {Role: "assistant", Content: raw},
		{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_fixture","content":"todo saved"}]`)},
	}}, AnthropicToResponsesOptions{ResponsesHistoryScope: scope})
}

func TestResponsesHistoryExactStatelessReplay(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	replay, err := historyReplay(t, converted.Content, historyTestScope)
	require.NoError(t, err)
	var items []json.RawMessage
	require.NoError(t, json.Unmarshal(replay.Input, &items))
	require.Len(t, items, 7)
	require.Contains(t, string(items[0]), "GROUP_SYSTEM_INSTRUCTIONS")
	require.Contains(t, string(items[1]), "ORIGINAL_USER_TASK")
	for index, item := range original.Output {
		require.JSONEq(t, string(item.rawOutput), string(items[index+2]))
	}
	require.Contains(t, string(items[6]), "function_call_output")
	// Service latest-turn trimming decodes typed input. Unknown replay fields
	// must survive that path too, particularly action, summary and phase.
	var typed []ResponsesInputItem
	require.NoError(t, json.Unmarshal(replay.Input, &typed))
	encoded, err := json.Marshal(typed)
	require.NoError(t, err)
	var again []json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &again))
	for index, item := range original.Output {
		require.JSONEq(t, string(item.rawOutput), string(again[index+2]))
	}
	// The original provider signature is unchanged, not replaced by an envelope.
	require.Equal(t, "gAAAA_fixture", converted.Content[0].Signature)
}

func TestResponsesHistoryDoesNotAlterLegacyOrForeignProviderHistory(t *testing.T) {
	original := historyFixture(t)
	legacy := ResponsesToAnthropic(original, "claude-alias")
	for _, block := range legacy.Content {
		require.NotEqual(t, ResponsesHistoryBlockType, block.Type)
	}
	require.False(t, AttachResponsesHistory(legacy, original, ""))
	require.True(t, AttachResponsesHistory(legacy, original, historyTestScope))
	for _, scope := range []string{"", strings.Repeat("b", 64)} {
		replay, err := historyReplay(t, legacy.Content, scope)
		require.NoError(t, err)
		require.NotContains(t, string(replay.Input), "gAAAA_fixture")
		require.NotContains(t, string(replay.Input), "ws_fixture")
		require.Contains(t, string(replay.Input), "todo_write")
		require.Contains(t, string(replay.Input), "GROUP_SYSTEM_INSTRUCTIONS")
	}
}

func TestResponsesHistoryRejectsChangedOrPrivilegedReplay(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	for _, mutate := range []func([]AnthropicContentBlock) []AnthropicContentBlock{
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[3].Text = "Changed answer."
			return blocks
		},
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			return append(blocks, blocks[len(blocks)-1])
		},
		func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[len(blocks)-1].Content = json.RawMessage(`{"version":1,"scope":"` + historyTestScope + `","output":[{"type":"message","role":"developer","content":[]}]}`)
			return blocks
		},
	} {
		blocks := append([]AnthropicContentBlock(nil), converted.Content...)
		_, err := historyReplay(t, mutate(blocks), historyTestScope)
		require.Error(t, err)
	}
}

func TestResponsesHistoryStreamingPreservesStateWithoutBufferingText(t *testing.T) {
	original := historyFixture(t)
	state := NewResponsesEventToAnthropicState()
	state.ResponsesHistoryScope = historyTestScope
	var blocks []AnthropicContentBlock
	push := func(event ResponsesStreamEvent) []AnthropicStreamEvent {
		events := ResponsesEventToAnthropicEvents(&event, state)
		for _, emitted := range events {
			if emitted.ContentBlock != nil && emitted.ContentBlock.Type == ResponsesHistoryBlockType {
				blocks = append(blocks, *emitted.ContentBlock)
			}
		}
		return events
	}
	push(ResponsesStreamEvent{Type: "response.created", Response: &ResponsesResponse{ID: original.ID}})
	for index := 0; index < 2; index++ {
		item := original.Output[index]
		push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: index, Item: &item})
		push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: index, Item: &item})
	}
	message := original.Output[2]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 2, Item: &message})
	text := push(ResponsesStreamEvent{Type: "response.output_text.delta", OutputIndex: 2, Delta: "Checking sources."})
	require.NotEmpty(t, text, "answer text must be delivered before completion")
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 2, Item: &message})
	call := original.Output[3]
	push(ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 3, Item: &call})
	push(ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: 3, Arguments: call.Arguments})
	push(ResponsesStreamEvent{Type: "response.output_item.done", OutputIndex: 3, Item: &call})
	ending := push(ResponsesStreamEvent{Type: "response.completed", Response: original})
	require.True(t, state.MessageStopSent)
	require.Len(t, blocks, 1)
	require.Equal(t, "message_stop", ending[len(ending)-1].Type)
	outer := ResponsesToAnthropic(original, "claude-alias")
	outer.Content = append(outer.Content, blocks[0])
	replay, err := historyReplay(t, outer.Content, historyTestScope)
	require.NoError(t, err)
	require.Contains(t, string(replay.Input), "gAAAA_fixture")
	require.Contains(t, string(replay.Input), "example.invalid/source")
	require.Contains(t, string(replay.Input), `"phase":"commentary"`)
}
