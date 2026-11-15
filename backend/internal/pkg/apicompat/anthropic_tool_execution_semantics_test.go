package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicToolChoicePreservesSerialExecution(t *testing.T) {
	for _, tc := range []struct {
		name, choice string
		parallel     bool
	}{
		{"absent", "", true},
		{"auto default", `{"type":"auto"}`, true},
		{"auto enabled", `{"type":"auto","disable_parallel_tool_use":false}`, true},
		{"auto serial", `{"type":"auto","disable_parallel_tool_use":true}`, false},
		{"any serial", `{"type":"any","disable_parallel_tool_use":true}`, false},
		{"named serial", `{"type":"tool","name":"mcp__fixture__lookup","disable_parallel_tool_use":true}`, false},
		{"none serial", `{"type":"none","disable_parallel_tool_use":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &AnthropicRequest{Model: "gpt-5.1", ToolChoice: json.RawMessage(tc.choice),
				Tools: []AnthropicTool{{Name: "mcp__fixture__lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}}}
			responses, err := AnthropicToResponses(req)
			require.NoError(t, err)
			require.NotNil(t, responses.ParallelToolCalls)
			require.Equal(t, tc.parallel, *responses.ParallelToolCalls)
			chat, err := AnthropicToChatCompletionsRequest(req)
			require.NoError(t, err)
			require.NotNil(t, chat.ParallelToolCalls)
			require.Equal(t, tc.parallel, *chat.ParallelToolCalls)
		})
	}
}

func TestAnthropicToolResultErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		isError             bool
	}{
		{"success", `"command returned exit 0"`, "command returned exit 0", false},
		{"success containing error", `"error is a field name"`, "error is a field name", false},
		{"failure", `"command returned exit 1"`, `{"is_error":true,"content":"command returned exit 1"}`, true},
		{"empty failure", `""`, `{"is_error":true,"content":"(empty)"}`, true},
		{"failure text blocks", `[{"type":"text","text":"first"},{"type":"text","text":"second"}]`, "{\"is_error\":true,\"content\":\"first\\n\\nsecond\"}", true},
		{"idempotent", `"{\"is_error\":true,\"content\":\"failed\"}"`, `{"is_error":true,"content":"failed"}`, true},
		{"null is not our envelope", `"{\"is_error\":true,\"content\":null}"`, `{"is_error":true,"content":"{\"is_error\":true,\"content\":null}"}`, true},
		{"other JSON remains content", `"{\"is_error\":true,\"content\":\"failed\",\"extra\":1}"`, `{"is_error":true,"content":"{\"is_error\":true,\"content\":\"failed\",\"extra\":1}"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, images := convertToolResultOutput(AnthropicContentBlock{
				Type: "tool_result", Content: json.RawMessage(tc.content), IsError: tc.isError,
			})
			require.Equal(t, tc.want, text)
			require.Empty(t, images)
		})
	}
}

func TestAnthropicToolResultErrorPreservesImagesAndCallPair(t *testing.T) {
	raw := `{"model":"gpt-5.1","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_mcp_1","name":"mcp__fixture__image","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_mcp_1","is_error":true,"content":[{"type":"text","text":"partial screenshot failed"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}]}]}`
	var req AnthropicRequest
	require.NoError(t, json.Unmarshal([]byte(raw), &req))
	responses, err := AnthropicToResponses(&req)
	require.NoError(t, err)
	var input []ResponsesInputItem
	require.NoError(t, json.Unmarshal(responses.Input, &input))
	require.Len(t, input, 3)
	require.Equal(t, "call_mcp_1", input[0].CallID)
	require.Equal(t, "call_mcp_1", input[1].CallID)
	require.JSONEq(t, `{"is_error":true,"content":"partial screenshot failed"}`, input[1].Output)
	require.Contains(t, string(input[2].Content), "data:image/png;base64,AA==")
	chat, err := AnthropicToChatCompletionsRequest(&req)
	require.NoError(t, err)
	require.Equal(t, "call_mcp_1", chat.Messages[0].ToolCalls[0].ID)
	require.Equal(t, "call_mcp_1", chat.Messages[1].ToolCallID)
	var text string
	require.NoError(t, json.Unmarshal(chat.Messages[1].Content, &text))
	require.JSONEq(t, `{"is_error":true,"content":"partial screenshot failed"}`, text)
	require.Contains(t, string(chat.Messages[2].Content), "data:image/png;base64,AA==")
}
