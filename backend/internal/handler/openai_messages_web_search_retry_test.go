package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMessagesWebSearchSemanticFailureRetriesDedicatedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                  string
		stream, pool, retry502, cancel        bool
		retries, failedAttempts, wantAttempts int
		wantSuccess                           bool
		failureKind                           string
		wantErrorStatus                       int
		upstreamStatus                        int
	}{
		{name: "recover_sync", pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true},
		{name: "recover_stream", stream: true, pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true},
		{name: "ten_retries", stream: true, pool: true, retry502: true, retries: 10, failedAttempts: 10, wantAttempts: 11, wantSuccess: true},
		{name: "exhausted_sync", pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 3},
		{name: "exhausted_stream", stream: true, pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 3},
		{name: "unconfigured_status", pool: true, retries: 2, failedAttempts: 99, wantAttempts: 1},
		{name: "zero_budget", pool: true, retry502: true, failedAttempts: 99, wantAttempts: 1},
		{name: "pool_disabled", retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 1},
		{name: "canceled", cancel: true, pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 1},
		{name: "separate_completed_event", stream: true, pool: true, retry502: true, retries: 2, wantAttempts: 1, wantSuccess: true},
		{name: "failed_502_recovers_sync", pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "failed_502"},
		{name: "failed_502_ten_retries", pool: true, retry502: true, retries: 10, failedAttempts: 10, wantAttempts: 11, wantSuccess: true, failureKind: "failed_502"},
		{name: "failed_502_exhausted_stream", stream: true, pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 3, failureKind: "failed_502"},
		{name: "failed_429_recovers_stream", stream: true, pool: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "failed_429"},
		{name: "failed_429_ten_retries", stream: true, pool: true, retries: 10, failedAttempts: 10, wantAttempts: 11, wantSuccess: true, failureKind: "failed_429"},
		{name: "failed_429_exhausted_sync", pool: true, retries: 2, failedAttempts: 99, wantAttempts: 3, failureKind: "failed_429", wantErrorStatus: http.StatusTooManyRequests},
		{name: "failed_429_unconfigured", pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 1, failureKind: "failed_429", wantErrorStatus: http.StatusTooManyRequests},
		{name: "truncated_recovers", stream: true, pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "truncated"},
		{name: "malformed_completion_recovers", pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "malformed_completion"},
		{name: "read_error_recovers", stream: true, pool: true, retry502: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "read_error"},
		{name: "http_429_read_error_recovers", pool: true, retries: 2, failedAttempts: 2, wantAttempts: 3, wantSuccess: true, failureKind: "read_error", upstreamStatus: http.StatusTooManyRequests},
		{name: "http_429_read_error_unconfigured", pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 1, failureKind: "read_error", upstreamStatus: http.StatusTooManyRequests, wantErrorStatus: http.StatusTooManyRequests},
		{name: "failed_502_zero_budget", pool: true, retry502: true, failedAttempts: 99, wantAttempts: 1, failureKind: "failed_502"},
		{name: "read_error_canceled", cancel: true, pool: true, retry502: true, retries: 2, failedAttempts: 99, wantAttempts: 1, failureKind: "read_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(42)
			status := float64(http.StatusTooManyRequests)
			if tc.retry502 {
				status = http.StatusBadGateway
			}
			repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{
				{ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Priority: 100,
					Credentials: map[string]any{"api_key": "fixture-search", "base_url": "https://search.example", "pool_mode": tc.pool, "pool_mode_retry_count": float64(tc.retries), "pool_mode_retry_status_codes": []any{status}},
					Extra:       map[string]any{service.OpenAIWebSearchModeKey: service.OpenAIWebSearchModeOnly}},
				{ID: 8, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID}, Priority: 1,
					Credentials: map[string]any{"api_key": "fixture-normal", "base_url": "https://normal.example"}, Extra: map[string]any{"openai_responses_supported": true}},
			}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 2
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := &openAIConfiguredStatusRetryUpstream{responseFactory: func(attempt int) *http.Response {
				wire := "data: {\"type\":\"response.web_search_call.completed\",\"item_id\":\"ws_success\",\"output_index\":0}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Verified search output\"}]}]}}\n\n"
				if attempt <= tc.failedAttempts {
					wire = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"UNVERIFIED-ANSWER\"}]}]}}\n\n"
					switch tc.failureKind {
					case "failed_502":
						wire = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"bps_stream_error\",\"type\":\"bps_stream_error\",\"message\":\"Upstream request failed, please try again later\"}}}\n\n"
					case "failed_429":
						wire = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"type\":\"tokens\",\"message\":\"Upstream rate limit exceeded, please retry later\"}}}\n\n"
					case "truncated", "read_error":
						wire = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"UNVERIFIED-ANSWER\"}\n\n"
					case "malformed_completion":
						wire = "data: {\"type\":\"response.completed\"}\n\n"
					}
				}
				if tc.cancel {
					cancel()
				}
				var responseBody io.ReadCloser = io.NopCloser(strings.NewReader(wire))
				if attempt <= tc.failedAttempts && tc.failureKind == "read_error" {
					responseBody = &messagesRetryReadErrorBody{Reader: strings.NewReader(wire), err: io.ErrUnexpectedEOF}
				}
				status := http.StatusOK
				if attempt <= tc.failedAttempts && tc.upstreamStatus != 0 {
					status = tc.upstreamStatus
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{fmt.Sprintf("search-attempt-%d", attempt)}}, Body: responseBody}
			}}
			billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			usage := &messagesRetryUsageRecorder{}
			gateway := service.NewOpenAIGatewayService(repo, nil, usage, nil, nil, nil, nil, &openAIConfiguredStatusRetryCache{}, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billingCache, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-sol","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"latest news"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, tc.stream))
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			group := &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive, AllowMessagesDispatch: true, WebSearchAccountID: 7, RateMultiplier: 1}
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, AllowLongContext: true, GroupID: &groupID, User: &service.User{ID: 1, Status: service.StatusActive}, Group: group})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
			h.Messages(c)
			require.Len(t, upstream.accountIDs, tc.wantAttempts, rec.Body.String())
			for i, id := range upstream.accountIDs {
				require.Equal(t, int64(7), id, "search retries must never reach the normal account")
				require.Equal(t, string(upstream.requestBodies[0]), string(upstream.requestBodies[i]), "retries must preserve the request and cache identity")
			}
			require.NotContains(t, rec.Body.String(), "UNVERIFIED-ANSWER")
			if tc.failureKind == "failed_502" || tc.failureKind == "failed_429" {
				events, ok := c.Get(service.OpsUpstreamErrorsKey)
				require.True(t, ok, "failed attempts must remain observable")
				upstreamErrors, ok := events.([]*service.OpsUpstreamErrorEvent)
				require.True(t, ok)
				require.NotEmpty(t, upstreamErrors)
				message := "Upstream request failed, please try again later"
				if tc.failureKind == "failed_429" {
					message = "Upstream rate limit exceeded, please retry later"
				}
				require.Equal(t, message, upstreamErrors[0].Message)
			}
			if tc.wantSuccess {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "Verified search output")
				require.Contains(t, rec.Body.String(), "web_search_tool_result")
				require.Len(t, usage.rows, 1, "only the successful search is billable")
				require.InDelta(t, 0.01, usage.rows[0].TotalCost, 1e-9)
			} else {
				require.Empty(t, usage.rows)
				if !tc.cancel {
					wantStatus := tc.wantErrorStatus
					if wantStatus == 0 {
						wantStatus = http.StatusBadGateway
					}
					require.Equal(t, wantStatus, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
