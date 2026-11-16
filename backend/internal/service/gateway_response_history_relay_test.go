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
	"github.com/tidwall/sjson"
)

func responsesHistoryRelayBody(t *testing.T) []byte {
	t.Helper()
	var original apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_relay","status":"completed","output":[
	 {"type":"reasoning","id":"rs_relay","summary":[],"encrypted_content":"fixture-provider-state"},
	 {"type":"web_search_call","id":"ws_relay_a","status":"completed","action":{"type":"search","query":"synthetic first","sources":[{"type":"url","url":"https://example.invalid/a"}]}},
	 {"type":"web_search_call","id":"ws_relay_b","status":"completed","action":{"type":"search","query":"synthetic second","sources":[{"type":"url","url":"https://example.invalid/b"}]}},
	 {"type":"message","id":"msg_relay","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Synthetic progress."}]},
	 {"type":"function_call","id":"fc_relay","call_id":"call_relay","name":"todo_write","arguments":"{\"id\":9007199254740993,\"todos\":[]}"}
	]}`), &original))
	response := apicompat.ResponsesToAnthropicWithPhase(&original, "claude-opus-5-5")
	require.True(t, apicompat.AttachResponsesHistory(response, &original, strings.Repeat("a", 64)))
	body, err := json.Marshal(map[string]any{"model": "claude-opus-5-5", "max_tokens": 1024, "stream": false,
		"system": "GROUP_AND_NATIVE_SYSTEM", "messages": []any{
			map[string]any{"role": "user", "content": "ORIGINAL_TASK"},
			map[string]any{"role": "assistant", "content": response.Content},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_relay", "content": "todo saved"}}},
		}})
	require.NoError(t, err)
	return body
}

func convertResponsesHistoryRelay(t *testing.T, body []byte, scopes ...string) (*apicompat.ResponsesRequest, error) {
	t.Helper()
	var request apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(body, &request))
	scope := strings.Repeat("a", 64)
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	return apicompat.AnthropicToResponsesWithOptions(&request, apicompat.AnthropicToResponsesOptions{ResponsesHistoryScope: scope})
}

func TestResponsesHistoryRelayFilterPreservesMatchedProjection(t *testing.T) {
	body := responsesHistoryRelayBody(t)
	_, err := convertResponsesHistoryRelay(t, body)
	require.NoError(t, err)
	// Reproduce B -> A: the old emulation prefix collides with Responses ws_
	// IDs, so B removed the visible pair but kept A's exact output capsule.
	legacy := FilterWebSearchHistoryBlocks(body, "claude-opus-5-5")
	_, err = convertResponsesHistoryRelay(t, legacy)
	require.ErrorContains(t, err, "Responses history does not match the assistant message")
	require.Contains(t, string(legacy), apicompat.ResponsesHistoryBlockType)
	for _, header := range []string{"", "v2", "v1"} {
		t.Run("negotiation="+header, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set(apicompat.ResponsesHistoryHeader, header)
			out := filterWebSearchHistoryBlocksForRequest(c, body, "claude-opus-5-5")
			if header != "v1" {
				require.Equal(t, legacy, out)
				return
			}
			require.Equal(t, body, out)
			replay, err := convertResponsesHistoryRelay(t, out)
			require.NoError(t, err)
			require.Contains(t, string(replay.Input), "example.invalid/a")
			require.Contains(t, string(replay.Input), "example.invalid/b")
			require.Contains(t, string(replay.Input), "9007199254740993")
			require.Contains(t, string(replay.Input), `"phase":"commentary"`)
			require.Contains(t, string(replay.Input), "ORIGINAL_TASK")
		})
	}
}

func TestResponsesHistoryRelayAdjacentAssistantBoundary(t *testing.T) {
	body := responsesHistoryRelayBody(t)
	var request apicompat.AnthropicRequest
	require.NoError(t, json.Unmarshal(body, &request))
	// Model the adapter's old same-role merge: the visible text changes while
	// the opaque capsule still describes only the original assistant turn.
	var blocks []apicompat.AnthropicContentBlock
	require.NoError(t, json.Unmarshal(request.Messages[1].Content, &blocks))
	blocks = append(blocks, apicompat.AnthropicContentBlock{Type: "text", Text: "Additional project context."})
	merged, err := json.Marshal(blocks)
	require.NoError(t, err)
	bad, err := sjson.SetRawBytes(body, "messages.1.content", merged)
	require.NoError(t, err)
	_, err = convertResponsesHistoryRelay(t, bad)
	require.ErrorContains(t, err, "Responses history does not match the assistant message")

	// Keep the additional assistant turn separate, even though roles repeat.
	messages := gjson.GetBytes(body, "messages").Array()
	separate := []json.RawMessage{json.RawMessage(messages[0].Raw), json.RawMessage(messages[1].Raw),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Additional project context."}]}`), json.RawMessage(messages[2].Raw)}
	raw, err := json.Marshal(separate)
	require.NoError(t, err)
	good, err := sjson.SetRawBytes(body, "messages", raw)
	require.NoError(t, err)
	forwarded := FilterWebSearchHistoryBlocksForRelay(good, "claude-opus-5-5")
	replay, err := convertResponsesHistoryRelay(t, forwarded)
	require.NoError(t, err)
	require.Contains(t, string(replay.Input), "Synthetic progress.")
	require.Contains(t, string(replay.Input), "Additional project context.")
	require.Contains(t, string(replay.Input), "example.invalid/a")
	require.Contains(t, string(replay.Input), "todo saved")
}

func TestResponsesHistoryRelayFilterDoesNotTrustMissingOrChangedCapsules(t *testing.T) {
	body := responsesHistoryRelayBody(t)
	for name, mutate := range map[string]func([]byte) ([]byte, error){
		"changed query": func(b []byte) ([]byte, error) {
			return sjson.SetBytes(b, "messages.1.content.1.input.query", "changed")
		},
		"changed arguments": func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "messages.1.content.6.input.id", 1) },
		"invalid version":   func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "messages.1.content.7.content.version", 2) },
		"missing capsule":   func(b []byte) ([]byte, error) { return sjson.DeleteBytes(b, "messages.1.content.7") },
		"duplicate capsule": func(b []byte) ([]byte, error) {
			return sjson.SetRawBytes(b, "messages.1.content.-1", []byte(gjson.GetBytes(b, "messages.1.content.7").Raw))
		},
		"user role": func(b []byte) ([]byte, error) { return sjson.SetBytes(b, "messages.1.role", "user") },
	} {
		t.Run(name, func(t *testing.T) {
			input, err := mutate(body)
			require.NoError(t, err)
			out := FilterWebSearchHistoryBlocksForRelay(input, "claude-opus-5-5")
			require.NotContains(t, gjson.GetBytes(out, "messages.1.content").Raw, `"type":"server_tool_use"`)
		})
	}
	// Unsupported native upstreams keep their existing filtering behavior.
	out := FilterWebSearchHistoryBlocksForRelay(body, "glm-4.7")
	require.NotContains(t, gjson.GetBytes(out, "messages.1.content").Raw, `"type":"server_tool_use"`)
}

func TestResponsesHistoryRelayFilterPreservesOtherTurnsAndLargeNumbers(t *testing.T) {
	body := responsesHistoryRelayBody(t)
	// This separate locally emulated turn has no matching capsule and must
	// still be filtered, without float64-rounding a valid earlier tool input.
	other := `{"role":"assistant","content":[{"type":"server_tool_use","id":"srvtoolu_ws_local","name":"web_search","input":{"query":"local"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_ws_local","content":[]},{"type":"text","text":"local summary"}]}`
	body, err := sjson.SetRawBytes(body, "messages.-1", []byte(other))
	require.NoError(t, err)
	out := FilterWebSearchHistoryBlocksForRelay(body, "claude-opus-5-5")
	require.Contains(t, string(out), "9007199254740993")
	require.NotContains(t, string(out), "srvtoolu_ws_local")
	require.Contains(t, string(out), "srvtoolu_ws_relay_a")
	require.Contains(t, string(out), "local summary")
	require.Equal(t, "GROUP_AND_NATIVE_SYSTEM", gjson.GetBytes(out, "system").String())
}

func TestResponsesHistoryRelayActualAnthropicForwardPreservesOriginReplay(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		t.Run(map[bool]string{true: "apikey-passthrough", false: "standard-forward"}[passthrough], func(t *testing.T) {
			body := responsesHistoryRelayBody(t)
			messages := gjson.GetBytes(body, "messages").Array()
			adjacent, err := json.Marshal([]json.RawMessage{json.RawMessage(messages[0].Raw), json.RawMessage(messages[1].Raw),
				json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"Additional project context."}]}`), json.RawMessage(messages[2].Raw)})
			require.NoError(t, err)
			body, err = sjson.SetRawBytes(body, "messages", adjacent)
			require.NoError(t, err)
			originUpstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_origin_after_relay", "gpt-5.5")}
			origin := &OpenAIGatewayService{httpUpstream: originUpstream, cfg: &config.Config{}}
			originAccount := &Account{ID: 94802, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "fixture-account"}}
			originRec := httptest.NewRecorder()
			originContext, _ := gin.CreateTestContext(originRec)
			originContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			originContext.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
			originScope := openAIMessagesResponseHistoryScope(originContext, originAccount, "gpt-5.5")
			body, err = sjson.SetBytes(body, "messages.1.content.7.content.scope", originScope)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture_answer","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`))}}
			cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
			svc := &GatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}}
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_passthrough"] = passthrough
			parsed := &ParsedRequest{Body: NewRequestBodyRef(body), Model: "claude-opus-5-5", Stream: false}
			_, err = svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.Equal(t, "v1", getHeaderRaw(upstream.lastReq.Header, apicompat.ResponsesHistoryHeader))
			replay, err := convertResponsesHistoryRelay(t, upstream.lastBody, originScope)
			require.NoError(t, err)
			require.Contains(t, string(replay.Input), "example.invalid/a")
			require.Contains(t, string(replay.Input), "example.invalid/b")
			require.Contains(t, string(replay.Input), "9007199254740993")
			require.Contains(t, string(upstream.lastBody), "GROUP_AND_NATIVE_SYSTEM")
			// Feed B's actual captured wire body through A's actual forwarding
			// transforms as well, not just through an isolated input converter.
			_, err = origin.ForwardAsAnthropic(context.Background(), originContext, originAccount, upstream.lastBody, "relay-fixture-session", "gpt-5.5")
			require.NoError(t, err)
			require.Equal(t, 200, originRec.Code)
			require.Contains(t, string(originUpstream.lastBody), "example.invalid/a")
			require.Contains(t, string(originUpstream.lastBody), "example.invalid/b")
			require.Contains(t, string(originUpstream.lastBody), "fixture-provider-state")
			require.Contains(t, string(originUpstream.lastBody), "GROUP_AND_NATIVE_SYSTEM")
			require.Contains(t, string(originUpstream.lastBody), "Additional project context.")
			require.Contains(t, string(originUpstream.lastBody), "9007199254740993")
			require.NotContains(t, string(originUpstream.lastBody), apicompat.ResponsesHistoryBlockType)
		})
	}
}
