package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicHistoryFiltersPreserveJSONNumbers(t *testing.T) {
	for _, number := range []string{"1", "1.0", "1e3", "-0", "9007199254740993", "0.1234567890123456789"} {
		for name, filter := range map[string]func([]byte) []byte{
			"empty-text":     StripEmptyTextBlocks,
			"thinking":       func(body []byte) []byte { return FilterThinkingBlocks(body, "claude-sonnet-4-5") },
			"thinking-retry": func(body []byte) []byte { return FilterThinkingBlocksForRetry(body, "claude-sonnet-4-5") },
		} {
			t.Run(name+"/"+number, func(t *testing.T) {
				arguments := `{"value":` + number + `}`
				encodedArguments, err := json.Marshal(arguments)
				require.NoError(t, err)
				var original apicompat.ResponsesResponse
				require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"id":"fixture","status":"completed","output":[
				 {"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"fixture-signature"},
				 {"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"fixture_read","arguments":%s}
				]}`, encodedArguments)), &original))
				response := apicompat.ResponsesToAnthropic(&original, "claude-sonnet-4-5")
				require.True(t, apicompat.AttachResponsesHistory(response, &original, strings.Repeat("a", 64)))
				body, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 8192, "messages": []any{
					map[string]any{"role": "assistant", "content": response.Content},
					map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": ""}, map[string]string{"type": "text", "text": "continue"}}},
				}})
				require.NoError(t, err)
				filtered := filter(body)
				for _, message := range gjson.GetBytes(filtered, "messages").Array() {
					for _, block := range message.Get("content").Array() {
						if block.Get("type").String() == "tool_use" {
							require.Equal(t, number, block.Get("input.value").Raw)
						}
					}
				}
				_, err = convertResponsesHistoryRelay(t, filtered)
				require.NoError(t, err)
			})
		}
	}
}
