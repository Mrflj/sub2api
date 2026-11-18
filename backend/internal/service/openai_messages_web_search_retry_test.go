package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMessagesWebSearchCompletionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, prefix string
		want         bool
	}{
		{name: "completed_event", prefix: "data: {\"type\":\"response.web_search_call.completed\",\"item_id\":\"ws_test\",\"output_index\":0}\n\n", want: true},
		{name: "completed_item", prefix: "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_test\",\"status\":\"completed\"}}\n\n", want: true},
		{name: "multiline_crlf_item", prefix: "event: response.output_item.done\r\ndata: {\"type\":\"response.output_item.done\",\r\ndata: \"item\":{\"type\":\"web_search_call\",\"status\":\"completed\"}}\r\n\r\n", want: true},
		{name: "searching_is_not_complete", prefix: "data: {\"type\":\"response.web_search_call.searching\",\"item_id\":\"ws_test\"}\n\n"},
		{name: "added_is_not_complete", prefix: "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"status\":\"in_progress\"}}\n\n"},
		{name: "failed_item", prefix: "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"web_search_call\",\"status\":\"failed\"}}\n\n"},
		{name: "other_tool", prefix: "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"file_search_call\",\"status\":\"completed\"}}\n\n"},
		{name: "missing_item_id", prefix: "data: {\"type\":\"response.web_search_call.completed\"}\n\n"},
		{name: "malformed_event", prefix: "data: {\"type\":\"response.web_search_call.completed\",\"item_id\":\"ws_test\"\n\n"},
		{name: "ordinary_answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := tc.prefix + alphaSearchResponsesSSE("answer")
			require.Equal(t, tc.want, hasCompletedMessagesWebSearchCall([]byte(wire)))
		})
	}
	require.True(t, hasCompletedMessagesWebSearchCall([]byte(messagesWebSearchResponsesSSE("answer"))))
}

func TestMessagesWebSearchMissingCallUsesConfiguredRetryAndDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name          string
		pool          bool
		retries       int
		codes         []any
		wantRetryable bool
	}{
		{name: "configured_502", pool: true, retries: 10, codes: []any{float64(502)}, wantRetryable: true},
		{name: "zero_budget", pool: true, retries: 0, codes: []any{float64(502)}},
		{name: "other_status", pool: true, retries: 2, codes: []any{float64(429)}},
		{name: "empty_statuses", pool: true, retries: 2, codes: []any{}},
		{name: "pool_disabled", retries: 2, codes: []any{float64(502)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 7, Name: "search-only", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example", "pool_mode": tc.pool, "pool_mode_retry_count": float64(tc.retries), "pool_mode_retry_status_codes": tc.codes}}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"X-Request-Id": []string{"search-attempt"}}, Body: io.NopCloser(strings.NewReader(alphaSearchResponsesSSE("UNVERIFIED-ANSWER")))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			body := []byte(`{"model":"public-search","max_tokens":64,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305"}]}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "public-search"), c, account, body, "", "")
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusBadGateway, failover.StatusCode)
			require.Equal(t, tc.wantRetryable, failover.RetryableOnSameAccount)
			_, _, healthFailure := classifyOpenAIAPIKeyHealthFailure(failover)
			require.False(t, healthFailure, "search validation must not trip the account health breaker")
			require.Equal(t, "search-attempt", failover.ResponseHeaders.Get("x-request-id"))
			require.Equal(t, "web_search_not_executed", gjson.GetBytes(failover.ResponseBody, "error.code").String())
			require.Nil(t, result)
			require.False(t, c.Writer.Written())
			require.Empty(t, rec.Body.String())
			events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
			require.Len(t, events, 1)
			require.Equal(t, account.ID, events[0].AccountID)
			require.Equal(t, http.StatusOK, events[0].UpstreamStatusCode, "retain the actual HTTP status separately from retry classification")
			require.Equal(t, "search-attempt", events[0].UpstreamRequestID)
			require.Equal(t, "web_search_not_executed", events[0].Reason)
			require.NotContains(t, events[0].Detail, "UNVERIFIED-ANSWER")
		})
	}
}

func TestMessagesWebSearchToolCompletionStillRequiresResponseCompletion(t *testing.T) {
	for _, terminal := range []string{"", "data: [DONE]\n\n", "data: {\"type\":\"response.failed\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n"} {
		wire := "data: {\"type\":\"response.web_search_call.completed\",\"item_id\":\"ws_test\"}\n\n" + terminal
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
		account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "base_url": "https://upstream.example"}}
		body := []byte(`{"model":"public-search","max_tokens":64,"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305"}]}`)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		result, err := svc.ForwardAsAnthropic(WithOpenAIMessagesWebSearch(context.Background(), 7, "public-search"), c, account, body, "", "")
		require.Error(t, err)
		require.Nil(t, result)
		require.False(t, c.Writer.Written())
	}
}
