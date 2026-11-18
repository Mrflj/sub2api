package apicompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesHistoryRestoresOriginalCallOutputIdentity(t *testing.T) {
	for _, spec := range []struct{ kind, id, wireID, outputKind string }{
		{"function_call", "call_fixture", "call_fixture", "function_call_output"},
		{"function_call", "fc_call_fixture", "call_fixture", "function_call_output"},
		{"function_call", "fc_toolu_fixture", "toolu_fixture", "function_call_output"},
		{"custom_tool_call", "call_custom", "call_custom", "custom_tool_call_output"},
	} {
		t.Run(spec.kind+"/"+spec.id, func(t *testing.T) {
			var original ResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"id":"resp_fixture","status":"completed","output":[
			 {"type":%q,"id":"item_fixture","call_id":%q,"name":"fixture_tool","arguments":"{}","input":"synthetic"}
			]}`, spec.kind, spec.id)), &original))
			response := ResponsesToAnthropic(&original, "claude-fixture")
			require.True(t, AttachResponsesHistory(response, &original, strings.Repeat("a", 64)))
			require.Equal(t, spec.wireID, response.Content[0].ID)
			content, err := json.Marshal(response.Content)
			require.NoError(t, err)
			result, err := json.Marshal([]AnthropicContentBlock{{Type: "tool_result", ToolUseID: spec.wireID, Content: json.RawMessage(`"done"`)}})
			require.NoError(t, err)
			converted, err := AnthropicToResponsesWithOptions(&AnthropicRequest{Model: "gpt-5.5", MaxTokens: 1024,
				Messages: []AnthropicMessage{{Role: "assistant", Content: content}, {Role: "user", Content: result}}},
				AnthropicToResponsesOptions{ResponsesHistoryScope: strings.Repeat("a", 64)})
			require.NoError(t, err)
			var items []ResponsesInputItem
			require.NoError(t, json.Unmarshal(converted.Input, &items))
			require.Len(t, items, 2)
			require.Equal(t, spec.id, items[0].CallID)
			require.Equal(t, spec.id, items[1].CallID)
			require.Equal(t, spec.outputKind, items[1].Type)
		})
	}
}
