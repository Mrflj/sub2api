package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func messagesHistoryUpstreamFixture() *http.Response {
	items := []string{
		`{"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"gAAAA_fixture"}`,
		`{"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"fixture query","sources":[{"type":"url","url":"https://example.invalid/original-source"}]}}`,
		`{"type":"message","id":"msg_fixture","role":"assistant","phase":"commentary","status":"completed","content":[{"type":"output_text","text":"Checking sources.","annotations":[]}]}`,
		`{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"todo_write","arguments":"{\"todos\":[]}","status":"completed"}`,
	}
	events := []string{`{"type":"response.created","response":{"id":"resp_history_fixture","model":"gpt-5.5","status":"in_progress"}}`}
	for index, item := range items {
		indexJSON, _ := json.Marshal(index)
		if index != 1 {
			events = append(events, `{"type":"response.output_item.added","output_index":`+string(indexJSON)+`,"item":`+item+`}`)
		}
		if index == 2 {
			events = append(events, `{"type":"response.output_text.delta","output_index":2,"delta":"Checking sources."}`)
		}
		if index == 3 {
			events = append(events, `{"type":"response.function_call_arguments.done","output_index":3,"arguments":"{\"todos\":[]}"}`)
		}
		events = append(events, `{"type":"response.output_item.done","output_index":`+string(indexJSON)+`,"item":`+item+`}`)
	}
	events = append(events, `{"type":"response.completed","response":{"id":"resp_history_fixture","model":"gpt-5.5","status":"completed","output":[`+strings.Join(items, ",")+`],"usage":{"input_tokens":80,"output_tokens":20}}}`)
	var sse strings.Builder
	for _, event := range events {
		sse.WriteString("data: " + event + "\n\n")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse.String()))}
}

func collectMessagesHistoryResponse(t *testing.T, recorder *httptest.ResponseRecorder, stream bool) []apicompat.AnthropicContentBlock {
	t.Helper()
	if !stream {
		var response apicompat.AnthropicResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
		return response.Content
	}
	blocks := map[int]*apicompat.AnthropicContentBlock{}
	arguments := map[int]string{}
	scanner := bufio.NewScanner(bytes.NewReader(recorder.Body.Bytes()))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event apicompat.AnthropicStreamEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		if event.Index == nil {
			continue
		}
		index := *event.Index
		if event.ContentBlock != nil {
			copy := *event.ContentBlock
			blocks[index] = &copy
		}
		if event.Delta != nil && blocks[index] != nil {
			delta, block := event.Delta, blocks[index]
			switch delta.Type {
			case "text_delta":
				block.Text += delta.Text
			case "signature_delta":
				block.Signature += delta.Signature
			case "input_json_delta":
				arguments[index] += delta.PartialJSON
			}
		}
	}
	require.NoError(t, scanner.Err())
	indexes := []int{}
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	result := []apicompat.AnthropicContentBlock{}
	for _, index := range indexes {
		if arguments[index] != "" {
			blocks[index].Input = json.RawMessage(arguments[index])
		}
		result = append(result, *blocks[index])
	}
	return result
}

func TestForwardAsAnthropic_NegotiatedSearchHistorySurvivesStatelessAndContinuationFallback(t *testing.T) {
	for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, stream := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/stream", false: "/buffered"}[stream], func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: messagesHistoryUpstreamFixture()}
				svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
				account := &Account{ID: 94801, Platform: PlatformOpenAI, Type: kind, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "https://api.openai.com/v1", "access_token": "fixture-token", "chatgpt_account_id": "fixture-account"}}
				messages := []map[string]any{{"role": "user", "content": "ORIGINAL_TASK"}}
				send := func() *httptest.ResponseRecorder {
					body, err := json.Marshal(map[string]any{"model": "claude-alias", "stream": stream, "max_tokens": 1024, "system": "GROUP_SYSTEM_AND_NATIVE_INSTRUCTIONS", "messages": messages, "tools": []any{map[string]any{"name": "todo_write", "input_schema": map[string]any{"type": "object"}}}, "tool_choice": map[string]string{"type": "auto"}})
					require.NoError(t, err)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
					c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
					_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "history-fixture-session", "gpt-5.5")
					require.NoError(t, err)
					require.Equal(t, 200, recorder.Code)
					return recorder
				}
				first := send()
				require.Equal(t, "v1", first.Header().Get(apicompat.ResponsesHistoryHeader))
				blocks := collectMessagesHistoryResponse(t, first, stream)
				require.Equal(t, apicompat.ResponsesHistoryBlockType, blocks[len(blocks)-1].Type)
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_fixture", "content": "todo saved"}}})
				if kind == AccountTypeAPIKey {
					upstream.responses = []*http.Response{{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"previous_response_id is not available for this user"}}`))}, openAICompatSSECompletedResponse("resp_after_fallback", "gpt-5.5")}
				} else {
					upstream.resp = openAICompatSSECompletedResponse("resp_after_replay", "gpt-5.5")
				}
				send()
				sent := upstream.lastBody
				require.Contains(t, string(sent), "ORIGINAL_TASK")
				require.Contains(t, string(sent), "GROUP_SYSTEM_AND_NATIVE_INSTRUCTIONS")
				require.Contains(t, string(sent), "gAAAA_fixture")
				require.Contains(t, string(sent), "example.invalid/original-source")
				require.NotContains(t, string(sent), apicompat.ResponsesHistoryBlockType)
				var kinds []string
				var phase, callID, resultID string
				for _, item := range gjson.GetBytes(sent, "input").Array() {
					switch typ := item.Get("type").String(); typ {
					case "reasoning", "web_search_call", "function_call", "function_call_output":
						kinds = append(kinds, typ)
						if typ == "function_call" {
							callID = item.Get("call_id").String()
						}
						if typ == "function_call_output" {
							resultID = item.Get("call_id").String()
						}
					case "message":
						if item.Get("role").String() == "assistant" {
							phase = item.Get("phase").String()
						}
					}
				}
				require.Equal(t, []string{"reasoning", "web_search_call", "function_call", "function_call_output"}, kinds)
				require.Equal(t, "commentary", phase)
				require.Equal(t, "call_fixture", callID)
				require.Equal(t, callID, resultID)
				require.Contains(t, gjson.GetBytes(sent, "include").String(), "web_search_call.action.sources")
			})
		}
	}
}
