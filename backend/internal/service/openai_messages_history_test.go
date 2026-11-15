package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/groupprompt"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression fixtures deliberately exceed the removed twelve-message window.
const legacyAnthropicReplayWindow = 12

// More than twelve messages must not turn a stateless replay into a tail of
// successful todo updates with the actual user task missing.
func TestForwardAsAnthropic_PreservesTaskAndRepeatedToolHistory(t *testing.T) {
	for index, tc := range []struct {
		name, model, accountType, continuationError string
	}{
		{"apikey-codex", "gpt-5.3-codex", AccountTypeAPIKey, ""},
		{"apikey-astra", "gpt-6-astra", AccountTypeAPIKey, ""},
		{"oauth-codex", "gpt-5.4", AccountTypeOAuth, ""},
		{"setup-token-codex", "gpt-5.4", AccountTypeSetupToken, ""},
		{"expired-continuation", "gpt-5.3-codex", AccountTypeAPIKey, "previous_response_id is not available for this user"},
		{"unsupported-continuation", "gpt-6-astra", AccountTypeAPIKey, "previous_response_id requires an OpenAI API-key account for HTTP requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := make([]map[string]any, 0, 31)
			for i := 0; i < 7; i++ {
				role := "user"
				if i%2 != 0 {
					role = "assistant"
				}
				text := fmt.Sprintf("context-%d", i)
				if i == 2 {
					text = "ORIGINAL_TASK_MARKER: search official sources and explain the result"
				}
				messages = append(messages, map[string]any{"role": role, "content": text})
			}
			for i := 0; i < 12; i++ {
				id := fmt.Sprintf("call_todo_%02d", i)
				messages = append(messages,
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "todo_write", "input": map[string]any{"revision": i}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": fmt.Sprintf("success-%02d", i)}}},
				)
			}
			body, err := json.Marshal(map[string]any{
				"model": "claude-sonnet-4-5", "max_tokens": 128, "stream": false,
				"system": "CLIENT_SYSTEM_MARKER", "messages": messages,
				"tool_choice": map[string]any{"type": "auto"},
				"tools": []any{
					map[string]any{"name": "todo_write", "input_schema": map[string]any{"type": "object"}},
					map[string]any{"name": "web_search", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}},
				},
			})
			require.NoError(t, err)
			groupPolicy := groupprompt.Config{Enabled: true, Content: "GROUP_SYSTEM_MARKER", ModelPrompts: map[string]string{
				tc.model: "WRONG_UPSTREAM_PROMPT",
			}}
			body, err = groupprompt.Prepend(body, "messages", groupPolicy.Prompt("claude-sonnet-4-5"))
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_history", tc.model)}
			svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
			account := &Account{ID: int64(92300 + index), Platform: PlatformOpenAI, Type: tc.accountType, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com/v1", "access_token": "test-token", "chatgpt_account_id": "test-account"}}
			if tc.continuationError != "" {
				errorBody, err := json.Marshal(map[string]any{"error": map[string]string{"message": tc.continuationError, "type": "invalid_request_error"}})
				require.NoError(t, err)
				upstream.responses = []*http.Response{
					{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(errorBody)))},
					openAICompatSSECompletedResponse("resp_history", tc.model),
				}
				svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "history-regression", "resp_old")
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "history-regression", tc.model)
			require.NoError(t, err)
			require.NotNil(t, result)
			sent := upstream.lastBody
			require.Contains(t, string(sent), "ORIGINAL_TASK_MARKER")
			require.Contains(t, string(sent), "CLIENT_SYSTEM_MARKER")
			require.Equal(t, "GROUP_SYSTEM_MARKER\n\nCLIENT_SYSTEM_MARKER", gjson.GetBytes(sent, "input.0.content.0.text").String())
			require.NotContains(t, string(sent), "WRONG_UPSTREAM_PROMPT")
			require.NotContains(t, string(sent), "sub2api-claude-code-todo-guard")
			require.False(t, gjson.GetBytes(sent, "previous_response_id").Exists())
			require.Equal(t, int64(32), gjson.GetBytes(sent, "input.#").Int())
			require.Equal(t, "auto", gjson.GetBytes(sent, "tool_choice").String())
			uses, results := map[string]bool{}, map[string]bool{}
			for _, item := range gjson.GetBytes(sent, "input").Array() {
				switch item.Get("type").String() {
				case "function_call":
					id := item.Get("call_id").String()
					uses[id] = true
					require.Equal(t, "todo_write", item.Get("name").String())
					var index int
					_, err := fmt.Sscanf(id, "call_todo_%02d", &index)
					require.NoError(t, err)
					require.Equal(t, int64(index), gjson.Get(item.Get("arguments").String(), "revision").Int())
				case "function_call_output":
					id := item.Get("call_id").String()
					results[id] = true
					var index int
					_, err := fmt.Sscanf(id, "call_todo_%02d", &index)
					require.NoError(t, err)
					require.Equal(t, fmt.Sprintf("success-%02d", index), item.Get("output").String())
				}
			}
			require.Len(t, uses, 12)
			require.Equal(t, uses, results)
			require.Equal(t, "function", gjson.GetBytes(sent, "tools.1.type").String())
			require.Equal(t, "web_search", gjson.GetBytes(sent, "tools.1.name").String())
			if tc.continuationError != "" {
				require.Len(t, upstream.requests, 2)
				require.Equal(t, "resp_old", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
			}
		})
	}
}
