//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardAsChatCompletions_GenerationControlsCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const frames = `data: {"type":"response.created","response":{"id":"resp_controls","model":"gpt-4.1"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"fixture accepted END"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_controls","model":"gpt-4.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fixture accepted END"}]}],"usage":{"input_tokens":12,"output_tokens":5,"total_tokens":17,"input_tokens_details":{"cached_tokens":4}}}}` + "\n\n"
	for _, oauth := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("oauth=%t/stream=%t", oauth, stream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-4.1","stream":%t,"messages":[{"role":"system","content":"Keep the task"},{"role":"user","content":"Continue END"},{"role":"assistant","tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"read","arguments":"{\"stop\":\"END\"}"}}]},{"role":"tool","tool_call_id":"call_fixture","content":"tool result"}],"max_completion_tokens":256,"reasoning_effort":"low","frequency_penalty":0.5,"presence_penalty":0,"seed":9007199254740993,"logit_bias":{"1":-100},"n":2,"stop":["END"],"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"stop":{"type":"string"}}}}}]}`, stream))
				originalBody := append([]byte(nil), body...)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(frames)),
				}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				account := rawChatCompletionsTestAccount()
				account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}
				if oauth {
					account.Type = AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "fixture-account"}
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")

				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Len(t, upstream.requests, 1, "ignored n must not generate extra calls or charges")
				require.Equal(t, originalBody, body)
				for _, field := range []string{"frequency_penalty", "presence_penalty", "seed", "logit_bias", "n", "stop"} {
					require.False(t, gjson.GetBytes(upstream.lastBody, field).Exists(), field)
				}
				require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
				callID := "call_fixture"
				if oauth {
					// Preserve the Codex transform's existing unsupported-field policy.
					require.False(t, gjson.GetBytes(upstream.lastBody, "max_output_tokens").Exists())
					callID = "fc_fixture"
				} else {
					require.Equal(t, int64(256), gjson.GetBytes(upstream.lastBody, "max_output_tokens").Int())
				}
				require.Equal(t, "low", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
				require.Equal(t, "string", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.properties.stop.type").String())
				require.Contains(t, string(upstream.lastBody), "Keep the task")
				var calls, results int
				for _, item := range gjson.GetBytes(upstream.lastBody, "input").Array() {
					switch item.Get("type").String() {
					case "function_call":
						calls++
						require.Equal(t, callID, item.Get("call_id").String())
						require.JSONEq(t, `{"stop":"END"}`, item.Get("arguments").String())
					case "function_call_output":
						results++
						require.Equal(t, callID, item.Get("call_id").String())
						require.Equal(t, "tool result", item.Get("output").String())
					}
				}
				require.Equal(t, 1, calls)
				require.Equal(t, 1, results)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				require.Equal(t, 4, result.Usage.CacheReadInputTokens)
				require.Contains(t, rec.Body.String(), "fixture accepted END")
				if stream {
					require.Contains(t, rec.Body.String(), "data: [DONE]")
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: {") {
							continue
						}
						for _, choice := range gjson.Get(strings.TrimPrefix(line, "data: "), "choices").Array() {
							require.Zero(t, choice.Get("index").Int())
						}
					}
				} else {
					require.Len(t, gjson.Get(rec.Body.String(), "choices").Array(), 1)
					require.Equal(t, int64(12), gjson.Get(rec.Body.String(), "usage.prompt_tokens").Int())
				}
			})
		}
	}
}
