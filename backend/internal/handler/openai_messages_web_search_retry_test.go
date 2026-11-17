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
				}
				if tc.cancel {
					cancel()
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{fmt.Sprintf("search-attempt-%d", attempt)}}, Body: io.NopCloser(strings.NewReader(wire))}
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
			if tc.wantSuccess {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "Verified search output")
				require.Contains(t, rec.Body.String(), "web_search_tool_result")
				require.Len(t, usage.rows, 1, "only the successful search is billable")
				require.InDelta(t, 0.01, usage.rows[0].TotalCost, 1e-9)
			} else {
				require.Empty(t, usage.rows)
				if !tc.cancel {
					require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
