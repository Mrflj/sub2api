//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardAsAnthropicToolExecutionSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "chat"} {
		t.Run(protocol, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4","stream":true,"max_tokens":256,"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"tools":[{"name":"mcp__fixture__lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"messages":[{"role":"user","content":"fixture task"},{"role":"assistant","content":[{"type":"tool_use","id":"call_previous","name":"mcp__fixture__lookup","input":{"query":"fixture"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_previous","is_error":true,"content":[{"type":"text","text":"fixture lookup failed"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]},{"type":"text","text":"continue fixture"}]}]}`)
			account := rawChatCompletionsTestAccount()
			mode := openai_compat.ResponsesSupportModeForceResponses
			if protocol == "chat" {
				mode = openai_compat.ResponsesSupportModeForceChatCompletions
			}
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(mode)}
			var frames []string
			if protocol == "responses" {
				frames = []string{
					`{"type":"response.created","response":{"id":"resp_fixture","model":"gpt-5.4"}}`,
					`{"type":"response.output_text.delta","output_index":0,"delta":"prefix"}`,
					`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"mcp__fixture__a"}}`,
					`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"a\":"}`,
					`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_b","name":"mcp__fixture__b"}}`,
					`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"b\":2}"}`,
					`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"1}"}`,
					`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{\"a\":1}"}`,
					`{"type":"response.function_call_arguments.done","output_index":2,"arguments":"{\"b\":2}"}`,
					`{"type":"response.output_item.added","output_index":3,"item":{"type":"custom_tool_call","call_id":"call_raw","name":"apply_patch"}}`,
					`{"type":"response.custom_tool_call_input.delta","output_index":3,"delta":"printf(\"中文\")\n"}`,
					`{"type":"response.custom_tool_call_input.done","output_index":3,"input":"printf(\"中文\")\n"}`,
					`{"type":"response.completed","response":{"id":"resp_fixture","status":"completed","usage":{"input_tokens":12,"output_tokens":4}}}`,
				}
			} else {
				frames = []string{
					`{"id":"cc_fixture","choices":[{"delta":{"content":"prefix"}}]}`,
					`{"id":"cc_fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"mcp__fixture__a","arguments":"{\"a\":"}}]}}]}`,
					`{"id":"cc_fixture","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"mcp__fixture__b","arguments":"{\"b\":2}"}}]}}]}`,
					`{"id":"cc_fixture","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
					`{"id":"cc_fixture","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
					`{"id":"cc_fixture","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":4}}`,
				}
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader("data: " + strings.Join(frames, "\n\ndata: ") + "\n\ndata: [DONE]\n\n")),
			}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "fixture-cache", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())
			require.True(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Exists())
			require.Contains(t, string(upstream.lastBody), "data:image/png;base64,AA==")
			var errorOutput string
			if protocol == "responses" {
				errorOutput = gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output").output`).String()
				require.Equal(t, "call_previous", gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output").call_id`).String())
			} else {
				errorOutput = gjson.GetBytes(upstream.lastBody, `messages.#(role=="tool").content`).String()
				require.Equal(t, "call_previous", gjson.GetBytes(upstream.lastBody, `messages.#(role=="tool").tool_call_id`).String())
			}
			require.JSONEq(t, `{"is_error":true,"content":"fixture lookup failed"}`, errorOutput)
			open, toolIDs, args := map[int]bool{}, map[int]string{}, map[string]string{}
			messageStops := 0
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				var event apicompat.AnthropicStreamEvent
				require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
				if event.Index != nil {
					switch event.Type {
					case "content_block_start":
						open[*event.Index] = true
						if event.ContentBlock.Type == "tool_use" {
							toolIDs[*event.Index] = event.ContentBlock.ID
						}
					case "content_block_delta":
						require.True(t, open[*event.Index], "delta after closed block: %s", line)
						if event.Delta.Type == "input_json_delta" {
							args[toolIDs[*event.Index]] += event.Delta.PartialJSON
						}
					case "content_block_stop":
						require.True(t, open[*event.Index])
						open[*event.Index] = false
					}
				}
				if event.Type == "message_stop" {
					messageStops++
				}
			}
			require.JSONEq(t, `{"a":1}`, args["call_a"])
			require.JSONEq(t, `{"b":2}`, args["call_b"])
			if protocol == "responses" {
				var customInput struct {
					Input string `json:"input"`
				}
				require.NoError(t, json.Unmarshal([]byte(args["call_raw"]), &customInput))
				require.Equal(t, "printf(\"中文\")\n", customInput.Input)
			}
			require.Equal(t, 1, messageStops)
			require.Equal(t, 12, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
		})
	}
}
