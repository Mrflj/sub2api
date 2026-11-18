package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesHistoryMismatchFallsBackToVisibleHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]AnthropicContentBlock) []AnthropicContentBlock
		want   string
	}{
		{name: "edited_answer", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[3].Text = "Edited visible answer."
			return blocks
		}, want: "Edited visible answer."},
		{name: "appended_context", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			return append(blocks, AnthropicContentBlock{Type: "text", Text: "Additional project context."})
		}, want: "Additional project context."},
		{name: "relay_removed_hosted_search", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			var visible []AnthropicContentBlock
			for _, block := range blocks {
				if block.Type != "server_tool_use" && block.Type != "web_search_tool_result" {
					visible = append(visible, block)
				}
			}
			return visible
		}, want: "Checking sources."},
		{name: "changed_tool_arguments", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[4].Input = json.RawMessage(`{"id":9007199254740993,"value":0.1234567890123456789}`)
			return blocks
		}, want: "9007199254740993"},
		{name: "changed_search_query", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[1].Input = json.RawMessage(`{"query":"updated query"}`)
			return blocks
		}, want: "updated query"},
		{name: "unrecognized_signature", mutate: func(blocks []AnthropicContentBlock) []AnthropicContentBlock {
			blocks[0].Signature = "unrecognized-provider-ciphertext"
			return blocks
		}, want: "Checking sources."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := historyFixture(t)
			converted := ResponsesToAnthropic(original, "claude-alias")
			require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
			blocks := tc.mutate(converted.Content)
			content, err := json.Marshal(blocks)
			require.NoError(t, err)
			require.False(t, ResponsesHistoryMatchesAssistantContent(content), "fallback must not authorize exact replay in a relay")
			replay, err := historyReplay(t, blocks, historyTestScope)
			require.NoError(t, err, "a stale optional history capsule must not reject the visible conversation")
			require.Contains(t, string(replay.Input), tc.want)
			require.Contains(t, string(replay.Input), "ORIGINAL_USER_TASK")
			require.Contains(t, string(replay.Input), "GROUP_SYSTEM_INSTRUCTIONS")
			require.NotContains(t, string(replay.Input), ResponsesHistoryBlockType)
			require.NotContains(t, string(replay.Input), "gAAAA_fixture")
			require.NotContains(t, string(replay.Input), "unrecognized-provider-ciphertext")
			var items []ResponsesInputItem
			require.NoError(t, json.Unmarshal(replay.Input, &items))
			var call, output *ResponsesInputItem
			for i := range items {
				switch items[i].Type {
				case "reasoning", "web_search_call":
					t.Fatalf("fallback must not replay opaque provider items: %s", items[i].Type)
				case "function_call":
					call = &items[i]
				case "function_call_output":
					output = &items[i]
				}
			}
			require.NotNil(t, call)
			require.NotNil(t, output)
			require.Equal(t, "call_fixture", call.CallID)
			require.Equal(t, call.CallID, output.CallID)
			require.Equal(t, "todo saved", output.Output)
			if tc.name == "changed_tool_arguments" {
				require.Equal(t, `{"id":9007199254740993,"value":0.1234567890123456789}`, call.Arguments)
			}
		})
	}
}

func TestResponsesHistoryForeignScopeNotifiesVisibleFallback(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	for _, scope := range []string{"", strings.Repeat("b", 64)} {
		notified := false
		items, restored, err := restoreResponsesHistoryWithFallback(converted.Content, scope, func() { notified = true })
		require.NoError(t, err)
		require.True(t, restored)
		require.True(t, notified, "foreign-scope visible replay must also invalidate cached continuation")
		for _, item := range items {
			require.NotEqual(t, "reasoning", item.Type)
		}
	}
}

func TestResponsesHistoryEmptyOrDuplicateCapsulesRemainInvalid(t *testing.T) {
	original := historyFixture(t)
	converted := ResponsesToAnthropic(original, "claude-alias")
	require.True(t, AttachResponsesHistory(converted, original, historyTestScope))
	valid := converted.Content[len(converted.Content)-1]
	empty := AnthropicContentBlock{Type: ResponsesHistoryBlockType}
	for _, tc := range []struct {
		name     string
		capsules []AnthropicContentBlock
	}{
		{name: "empty", capsules: []AnthropicContentBlock{empty}},
		{name: "empty_then_valid", capsules: []AnthropicContentBlock{empty, valid}},
		{name: "valid_then_empty", capsules: []AnthropicContentBlock{valid, empty}},
		{name: "two_empty", capsules: []AnthropicContentBlock{empty, empty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := append([]AnthropicContentBlock(nil), converted.Content[:len(converted.Content)-1]...)
			blocks = append(blocks, tc.capsules...)
			raw, err := json.Marshal(blocks)
			require.NoError(t, err)
			require.False(t, ResponsesHistoryMatchesAssistantContent(raw), "an empty capsule cannot hide a duplicate from relay validation")
			_, _, err = restoreResponsesHistory(blocks, historyTestScope)
			require.Error(t, err)
		})
	}
}
