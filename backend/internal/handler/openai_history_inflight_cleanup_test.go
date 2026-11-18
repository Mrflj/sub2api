//go:build unit

package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type historyInflightCleanupCache struct {
	*handlerInflightCache
	failReleases int32
	releases     atomic.Int32
}

func (c *historyInflightCleanupCache) ReleaseInflightBalance(ctx context.Context, userID int64, id string) error {
	if c.releases.Add(1) <= c.failReleases {
		return errors.New("injected Redis connection reset after history rejection")
	}
	return c.handlerInflightCache.ReleaseInflightBalance(ctx, userID, id)
}

func (c *historyInflightCleanupCache) DeductUserBalance(_ context.Context, _ int64, amount float64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balance -= amount
	return nil
}

func TestOpenAIMessagesInvalidHistoryReleasesInflightBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name             string
		failReleases     int32
		pendingBill      bool
		disableEstimates bool
	}{
		{name: "normal_release"},
		{name: "transient_release_failure", failReleases: 2},
		{name: "another_bill_pending", pendingBill: true},
		{name: "transient_failure_with_another_bill_pending", failReleases: 2, pendingBill: true},
		{name: "estimates_disabled", disableEstimates: true},
		{name: "estimates_disabled_with_old_hold", disableEstimates: true, pendingBill: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Default.RateMultiplier = 1
			cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, ReserveEstimates: true, TTLSeconds: 60}
			cache := &historyInflightCleanupCache{handlerInflightCache: newHandlerInflightCache(10)}
			cache.failReleases = test.failReleases
			billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingCache.Stop)
			const accountID, keyID int64 = 9511, 9512
			groupID := int64(9513)
			price := 7.0
			group := &service.Group{ID: groupID, Hydrated: true, Status: service.StatusActive,
				Platform: service.PlatformOpenAI, RateMultiplier: 1, AllowMessagesDispatch: true,
				ModelPricing: []service.ChannelModelPricing{{Models: []string{"gpt-5.1"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: &price}}}
			key := &service.APIKey{ID: keyID, UserID: 71, GroupID: &groupID, Group: group,
				User: &service.User{ID: 71, Status: service.StatusActive, Balance: 10}}
			pendingHolds := 0
			finishBill := func() {}
			if test.pendingBill {
				held, err := billingCache.ReserveInflight(context.Background(), key.User, group, nil, 1)
				require.NoError(t, err)
				finishBill = held.Acquire()
				t.Cleanup(finishBill)
				held.HandlerDone()
				pendingHolds = 1
			}
			cfg.Billing.InflightReservation.ReserveEstimates = !test.disableEstimates
			accounts := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
				ID: accountID, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID},
				Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid"},
				Extra:       map[string]any{"openai_responses_supported": true},
			}}}
			upstream := &openAIConfiguredStatusRetryUpstream{}
			billing := service.NewBillingService(cfg, nil)
			gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil,
				cfg, nil, nil, billing, nil, billingCache, upstream, &service.DeferredService{}, nil, nil,
				service.NewModelPricingResolver(nil, billing), nil, nil, nil, nil)
			t.Cleanup(gateway.CloseOpenAIWSPool)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)

			var original apicompat.ResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"original answer"}]}]}`), &original))
			response := apicompat.ResponsesToAnthropic(&original, "gpt-5.1")
			scopePayload, err := json.Marshal([]any{"responses-history-v1", accountID, keyID, "gpt-5.1"})
			require.NoError(t, err)
			scope := sha256.Sum256(scopePayload)
			require.True(t, apicompat.AttachResponsesHistory(response, &original, hex.EncodeToString(scope[:])))
			// Invalid envelopes still exercise local failure cleanup. Ordinary
			// projection mismatches now continue using visible history instead.
			var invalidState map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(response.Content[len(response.Content)-1].Content, &invalidState))
			invalidState["version"] = json.RawMessage(`2`)
			response.Content[len(response.Content)-1].Content, err = json.Marshal(invalidState)
			require.NoError(t, err)
			content, err := json.Marshal(response.Content)
			require.NoError(t, err)
			for _, stream := range []bool{false, true} {
				body, err := json.Marshal(apicompat.AnthropicRequest{Model: "gpt-5.1", MaxTokens: 32, Stream: stream,
					Messages: []apicompat.AnthropicMessage{{Role: "assistant", Content: content}, {Role: "user", Content: json.RawMessage(`"continue"`)}}})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 71})
				h.Messages(c)
				require.Equal(t, http.StatusBadRequest, rec.Code, "the next request must not be rejected for reserved balance")
				require.Contains(t, rec.Body.String(), "invalid Responses history state")
				reservation := service.InflightReservationFromContext(c.Request.Context())
				if test.disableEstimates {
					require.Nil(t, reservation, "the actual handler must not create an estimated hold by default")
				} else {
					require.Equal(t, price, reservation.Amount(), "the actual handler acquired a priced hold")
				}
				require.Equal(t, pendingHolds, cache.count(), "release only the failed request's hold, preserving pending billing")
				require.Empty(t, upstream.accountIDs, "local history rejection must never spend upstream tokens")
				require.Equal(t, 10.0, cache.balance)
			}
			finishBill()
			require.Zero(t, cache.count())
		})
	}
}

func TestOpenAIMessagesHistoryMismatchContinuesAndSettlesUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, reserveEstimates := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("estimates=%t/stream=%t", reserveEstimates, stream), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Default.RateMultiplier = 1
				cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, ReserveEstimates: reserveEstimates, TTLSeconds: 60}
				cache := &historyInflightCleanupCache{handlerInflightCache: newHandlerInflightCache(10)}
				billingCache := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billingCache.Stop)
				const accountID, keyID int64 = 9514, 9515
				groupID := int64(9516)
				price := 0.25
				group := &service.Group{ID: groupID, Hydrated: true, Status: service.StatusActive,
					Platform: service.PlatformOpenAI, RateMultiplier: 1, AllowMessagesDispatch: true,
					ModelPricing: []service.ChannelModelPricing{{Models: []string{"gpt-5.1"}, BillingMode: service.BillingModePerRequest, PerRequestPrice: &price}}}
				key := &service.APIKey{ID: keyID, UserID: 71, GroupID: &groupID, Group: group,
					User: &service.User{ID: 71, Status: service.StatusActive, Balance: 10}}
				accounts := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{{
					ID: accountID, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
					Status: service.StatusActive, Schedulable: true, GroupIDs: []int64{groupID},
					Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://example.invalid"},
					Extra:       map[string]any{"openai_responses_supported": true},
				}}}
				upstream := &openAIConfiguredStatusRetryUpstream{status: http.StatusOK, contentType: "text/event-stream",
					body: `data: {"type":"response.completed","response":{"id":"resp_history_success","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued successfully"}]}],"usage":{"input_tokens":10,"output_tokens":2}}}` + "\n\n"}
				usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
				billingRepo := &compatStreamUsageBillingRepo{}
				billing := service.NewBillingService(cfg, nil)
				gateway := service.NewOpenAIGatewayService(accounts, nil, usageRepo, billingRepo, nil, nil, nil, nil,
					cfg, nil, nil, billing, nil, billingCache, upstream, &service.DeferredService{}, nil, nil,
					service.NewModelPricingResolver(nil, billing), nil, nil, nil, nil)
				t.Cleanup(gateway.CloseOpenAIWSPool)
				h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billingCache,
					service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
				var original apicompat.ResponsesResponse
				require.NoError(t, json.Unmarshal([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"original answer"}]}]}`), &original))
				response := apicompat.ResponsesToAnthropic(&original, "gpt-5.1")
				scopePayload, err := json.Marshal([]any{"responses-history-v1", accountID, keyID, "gpt-5.1"})
				require.NoError(t, err)
				scope := sha256.Sum256(scopePayload)
				require.True(t, apicompat.AttachResponsesHistory(response, &original, hex.EncodeToString(scope[:])))
				response.Content[0].Text = "client edited answer"
				content, err := json.Marshal(response.Content)
				require.NoError(t, err)
				body, err := json.Marshal(apicompat.AnthropicRequest{Model: "gpt-5.1", MaxTokens: 32, Stream: stream,
					Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"ORIGINAL_TASK"`)},
						{Role: "assistant", Content: content}, {Role: "user", Content: json.RawMessage(`"continue"`)}}})
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set(apicompat.ResponsesHistoryHeader, "v1")
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 71})
				h.Messages(c)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "continued successfully")
				require.NotContains(t, rec.Body.String(), "Responses history does not match")
				require.Len(t, upstream.requestBodies, 1, "history fallback itself never invokes another model request")
				require.Contains(t, string(upstream.requestBodies[0]), "ORIGINAL_TASK")
				require.Contains(t, string(upstream.requestBodies[0]), "client edited answer")
				require.NotContains(t, string(upstream.requestBodies[0]), "original answer")
				require.False(t, gjson.GetBytes(upstream.requestBodies[0], "previous_response_id").Exists())
				require.Len(t, billingRepo.commands, 1)
				require.Equal(t, price, billingRepo.commands[0].BalanceCost)
				require.Len(t, usageRepo.created, 1)
				require.Zero(t, cache.count(), "settlement releases this request's optional hold")
				require.Equal(t, 9.75, cache.balance, "successful fallback is billed once at actual cost")
			})
		}
	}
}
