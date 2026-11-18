package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

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

func TestForwardAsAnthropic_ChangedHistoryBypassesCachedContinuation(t *testing.T) {
	for _, variant := range []string{"edited", "account_changed", "model_changed", "account_ticket"} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			if variant == "account_ticket" && kind != AccountTypeOAuth {
				continue
			}
			for _, stream := range []bool{false, true} {
				t.Run(variant+"/"+kind+map[bool]string{true: "/stream", false: "/buffered"}[stream], func(t *testing.T) {
					upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_after_visible_fallback", "gpt-5.5")}
					svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
					account := &Account{ID: 94803, Platform: PlatformOpenAI, Type: kind, Concurrency: 1,
						Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "https://api.openai.com/v1", "access_token": "fixture-token", "chatgpt_account_id": "fixture-account"}}
					expectedState := ""
					if variant == "account_ticket" {
						svc.cfg.Gateway.OpenAICodexTicket = config.OpenAICodexTicketConfig{Enabled: true, TargetLength: 292, Models: []string{"gpt-5.5"}}
						expectedState = fakeCodexTicketState(292)
						require.NoError(t, svc.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
							AccountID: account.ID, Model: "gpt-5.5", State: expectedState, Length: 292,
							CapturedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
						}))
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
					c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
					c.Request.Header.Set(openAICodexTurnStateHeader, "stale-client-turn-state")
					var original apicompat.ResponsesResponse
					require.NoError(t, json.Unmarshal([]byte(`{"status":"completed","output":[
				 {"type":"reasoning","id":"rs_old","encrypted_content":"provider-cipher-before-edit"},
				 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Old answer."}]},
				 {"type":"function_call","call_id":"fc_call_fixture","name":"fixture_read","arguments":"{\"id\":9007199254740993}"}
				]}`), &original))
					visible := apicompat.ResponsesToAnthropic(&original, "claude-alias")
					source := *account
					sourceModel := "gpt-5.5"
					wantAnswer := "Old answer."
					switch variant {
					case "account_changed":
						source.ID--
					case "model_changed":
						sourceModel = "gpt-5.4"
					}
					// Attach to the original projection, then apply any client edit.
					require.True(t, apicompat.AttachResponsesHistory(visible, &original, openAIMessagesResponseHistoryScope(c, &source, sourceModel)))
					if variant == "edited" || variant == "account_ticket" {
						wantAnswer = "Client edited answer and context."
						visible.Content[1].Text = wantAnswer
					}
					body, err := json.Marshal(map[string]any{"model": "claude-alias", "stream": stream, "max_tokens": 1024,
						"system": "CURRENT_SYSTEM_INSTRUCTIONS", "messages": []any{
							map[string]any{"role": "user", "content": "ORIGINAL_TASK"},
							map[string]any{"role": "assistant", "content": visible.Content},
							map[string]any{"role": "user", "content": []any{map[string]string{"type": "tool_result", "tool_use_id": "call_fixture", "content": "actual tool result"}}},
						}})
					require.NoError(t, err)
					svc.bindOpenAICompatSessionResponseID(context.Background(), c, account, "fallback-session", "resp_before_edit")
					svc.bindOpenAICompatSessionTurnState(context.Background(), c, account, "fallback-session", "stale-cached-turn-state")
					_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "fallback-session", "gpt-5.5")
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Len(t, upstream.requests, 1)
					sent := string(upstream.lastBody)
					require.False(t, gjson.Get(sent, "previous_response_id").Exists(), "do not trim changed history against a stale continuation")
					require.Contains(t, sent, "ORIGINAL_TASK")
					require.Contains(t, sent, "CURRENT_SYSTEM_INSTRUCTIONS")
					require.Contains(t, sent, wantAnswer)
					require.Contains(t, sent, "actual tool result")
					require.Contains(t, sent, "9007199254740993")
					require.NotContains(t, sent, "provider-cipher-before-edit")
					if variant == "edited" || variant == "account_ticket" {
						require.NotContains(t, sent, "Old answer.")
					}
					require.NotContains(t, sent, apicompat.ResponsesHistoryBlockType)
					require.Equal(t, expectedState, upstream.lastReq.Header.Get(openAICodexTurnStateHeader), "only the current account's configured ticket can replace stale turn state")
					require.Empty(t, svc.getOpenAICompatSessionTurnState(context.Background(), c, account, "fallback-session"))
					if kind == AccountTypeOAuth {
						require.Equal(t, "Bearer fixture-token", upstream.lastReq.Header.Get("Authorization"))
					}
					var calls, outputs []string
					for _, item := range gjson.Get(sent, "input").Array() {
						switch item.Get("type").String() {
						case "function_call":
							calls = append(calls, item.Get("call_id").String())
						case "function_call_output":
							outputs = append(outputs, item.Get("call_id").String())
						}
					}
					require.Equal(t, []string{"call_fixture"}, calls)
					require.Equal(t, calls, outputs)
				})
			}
		}
	}
}

func TestForwardAsAnthropic_FailedHistoryFallbackDoesNotReuseOldContinuation(t *testing.T) {
	upstream := &httpUpstreamRecorder{err: io.ErrUnexpectedEOF}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
	account := &Account{ID: 94804, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-fixture", "base_url": "https://api.openai.com/v1"}}
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
		return c
	}
	c := newContext()
	scope := openAIMessagesResponseHistoryScope(c, account, "gpt-5.5")
	body := []byte(fmt.Sprintf(`{"model":"claude-alias","max_tokens":1024,"messages":[
	 {"role":"user","content":"ORIGINAL_TASK"},
	 {"role":"assistant","content":[{"type":"text","text":"Edited answer."},{"type":"sub2api_responses_history","content":{"version":1,"scope":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Old answer."}]}]}}]},
	 {"role":"user","content":"continue"}]}`, scope))
	svc.bindOpenAICompatSessionResponseID(context.Background(), c, account, "failure-session", "resp_stale_before_failure")
	svc.bindOpenAICompatSessionTurnState(context.Background(), c, account, "failure-session", "state_stale_before_failure")
	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "failure-session", "gpt-5.5")
	require.Error(t, err)
	require.Empty(t, svc.getOpenAICompatSessionResponseID(context.Background(), c, account, "failure-session"))
	require.Empty(t, svc.getOpenAICompatSessionTurnState(context.Background(), c, account, "failure-session"))

	upstream.err = nil
	upstream.resp = openAICompatSSECompletedResponse("resp_after_full_retry", "gpt-5.5")
	plain := []byte(`{"model":"claude-alias","max_tokens":1024,"messages":[{"role":"user","content":"ORIGINAL_TASK"},{"role":"assistant","content":"Edited answer."},{"role":"user","content":"retry without capsule"}]}`)
	_, err = svc.ForwardAsAnthropic(context.Background(), newContext(), account, plain, "failure-session", "gpt-5.5")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 2)
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
	require.Contains(t, string(upstream.lastBody), "ORIGINAL_TASK")
	require.Contains(t, string(upstream.lastBody), "Edited answer.")
}

func TestInvalidateOpenAICompatSessionHistoryPreservesPolicyAndIsolation(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuation_disabled=%t", disabled), func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 94805}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("api_key", &APIKey{ID: 7})
			other, _ := gin.CreateTestContext(httptest.NewRecorder())
			other.Set("api_key", &APIKey{ID: 8})
			binding := openAICompatSessionResponseBinding{
				ResponseID: "resp_stale", TurnState: "state_stale", ContinuationDisabled: disabled, ExpiresAt: time.Now().Add(time.Hour),
			}
			key := openAICompatSessionResponseKey(c, account, "same-session")
			otherKeys := []string{
				openAICompatSessionResponseKey(other, account, "same-session"),
				openAICompatSessionResponseKey(c, &Account{ID: account.ID + 1}, "same-session"),
				openAICompatSessionResponseKey(c, account, "other-session"),
			}
			svc.openaiCompatSessionResponses.Store(key, binding)
			for _, otherKey := range otherKeys {
				svc.openaiCompatSessionResponses.Store(otherKey, binding)
			}
			svc.invalidateOpenAICompatSessionHistory(context.Background(), c, account, "same-session")
			raw, ok := svc.openaiCompatSessionResponses.Load(key)
			require.True(t, ok)
			cleared := raw.(openAICompatSessionResponseBinding)
			require.Empty(t, cleared.ResponseID)
			require.Empty(t, cleared.TurnState)
			require.Equal(t, disabled, cleared.ContinuationDisabled)
			require.Equal(t, binding.ExpiresAt, cleared.ExpiresAt)
			for _, otherKey := range otherKeys {
				unchanged, ok := svc.openaiCompatSessionResponses.Load(otherKey)
				require.True(t, ok)
				require.Equal(t, binding, unchanged)
			}
		})
	}
}
