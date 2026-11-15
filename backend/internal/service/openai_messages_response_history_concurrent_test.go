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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func messagesConcurrentHistoryUpstream(compact, refresh bool) *http.Response {
	items := []string{
		`{"type":"reasoning","id":"rs_wire","summary":[],"encrypted_content":"SYNTHETIC_TERMINAL_STATE"}`,
		`{"type":"web_search_call","id":"ws_wire_1","status":"completed","action":{"type":"search","query":"synthetic first","sources":[{"type":"url","url":"https://example.invalid/first"}]}}`,
		`{"type":"web_search_call","id":"ws_wire_2","status":"completed","action":{"type":"search","query":"synthetic second","sources":[{"type":"url","url":"https://example.invalid/second"}]}}`,
		`{"type":"function_call","id":"fc_wire","call_id":"call_wire","name":"todo_write","arguments":"{\"todos\":[]}","status":"completed"}`,
	}
	events := []string{`{"type":"response.created","response":{"id":"resp_wire","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_wire","summary":[]}}`,
	}
	// On one wire shape only the terminal snapshot carries ciphertext. The
	// compact terminal case must instead recover it from the recorded done item.
	reasoningDone := `{"type":"reasoning","id":"rs_wire","summary":[]}`
	if compact {
		reasoningDone = items[0]
	}
	if refresh {
		reasoningDone = strings.Replace(items[0], "SYNTHETIC_TERMINAL_STATE", "SYNTHETIC_DONE_STATE", 1)
	}
	events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+reasoningDone+`}`)
	for _, index := range []int{2, 1} {
		encoded, _ := json.Marshal(index)
		events = append(events, `{"type":"response.output_item.done","output_index":`+string(encoded)+`,"item":`+items[index]+`}`)
	}
	events = append(events, `{"type":"response.output_item.added","output_index":3,"item":`+items[3]+`}`,
		`{"type":"response.function_call_arguments.done","output_index":3,"arguments":"{\"todos\":[]}"}`,
		`{"type":"response.output_item.done","output_index":3,"item":`+items[3]+`}`)
	final := items
	if compact {
		final = items[3:]
	}
	events = append(events, `{"type":"response.completed","response":{"id":"resp_wire","status":"completed","output":[`+strings.Join(final, ",")+`],"usage":{"input_tokens":80,"output_tokens":20}}}`)
	var wire strings.Builder
	for _, event := range events {
		wire.WriteString("data: " + event + "\n\n")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire.String()))}
}

func TestForwardAsAnthropic_ConcurrentSearchHistoryReplaysActualStream(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		compact, refresh bool
	}{
		{name: "terminal-only-ciphertext"}, {name: "compact-terminal", compact: true}, {name: "refreshed-terminal-ciphertext", refresh: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: messagesConcurrentHistoryUpstream(scenario.compact, scenario.refresh)}
			svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
			account := &Account{ID: 94802, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "fixture-account"}}
			messages := []map[string]any{{"role": "user", "content": "ORIGINAL_TASK"}}
			send := func() *httptest.ResponseRecorder {
				body, err := json.Marshal(map[string]any{"model": "claude-alias", "stream": true, "max_tokens": 1024, "system": "GROUP_AND_NATIVE_SYSTEM", "messages": messages, "tools": []any{map[string]any{"name": "todo_write", "input_schema": map[string]any{"type": "object"}}}})
				require.NoError(t, err)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
				_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "concurrent-fixture-session", "gpt-5.5")
				require.NoError(t, err)
				require.Equal(t, 200, recorder.Code)
				return recorder
			}
			first := send()
			blocks := collectMessagesHistoryResponse(t, first, true)
			require.Equal(t, apicompat.ResponsesHistoryBlockType, blocks[len(blocks)-1].Type)
			if scenario.refresh {
				require.Equal(t, "SYNTHETIC_DONE_STATE", blocks[0].Signature)
				require.Contains(t, string(blocks[len(blocks)-1].Content), "reasoning_signatures")
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": blocks}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_wire", "content": "todo saved"}}})
			upstream.resp = openAICompatSSECompletedResponse("resp_after_wire", "gpt-5.5")
			send()
			body := string(upstream.lastBody)
			require.Contains(t, body, "ORIGINAL_TASK")
			require.Contains(t, body, "GROUP_AND_NATIVE_SYSTEM")
			require.Contains(t, body, "SYNTHETIC_TERMINAL_STATE")
			require.NotContains(t, body, "SYNTHETIC_DONE_STATE")
			require.Contains(t, body, "example.invalid/first")
			require.Contains(t, body, "example.invalid/second")
			var kinds []string
			for _, item := range gjson.Get(body, "input").Array() {
				if item.Get("type").String() == "web_search_call" {
					kinds = append(kinds, item.Get("id").String())
				}
			}
			require.Equal(t, []string{"ws_wire_1", "ws_wire_2"}, kinds)
			require.NotContains(t, body, apicompat.ResponsesHistoryBlockType)
		})
	}
}
