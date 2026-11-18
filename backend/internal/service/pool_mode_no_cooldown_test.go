//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type poolNoCooldownRepo struct {
	tempUnschedScopeRepoStub
	account       *Account
	rateCalls     int
	overloadCalls int
}

func (r *poolNoCooldownRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *poolNoCooldownRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.rateCalls++
	return nil
}

func (r *poolNoCooldownRepo) SetOverloaded(context.Context, int64, time.Time) error {
	r.overloadCalls++
	return nil
}

func TestPoolModeUpstreamFailuresNeverChangeScheduling(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		for _, custom := range []bool{false, true} {
			for _, status := range []int{400, 401, 403, 404, 429, 502, 503, 529} {
				t.Run(fmt.Sprintf("%s/custom_%t/status_%d", platform, custom, status), func(t *testing.T) {
					account := newTempUnschedScopeAccount(TempUnschedScopeModel, map[string]any{"error_code": float64(status)})
					account.Platform = platform
					account.Credentials["pool_mode"] = true
					account.Credentials["custom_error_codes_enabled"] = custom
					account.Credentials["custom_error_codes"] = []any{float64(status)}
					repo := &poolNoCooldownRepo{account: account}
					blocker := &runtimeBlockRecorder{}
					svc := &RateLimitService{accountRepo: repo, runtimeBlocker: blocker}
					require.Equal(t, ErrorPolicySkipped, svc.CheckErrorPolicy(context.Background(), account, status, tempUnschedScopeBody, "model"))
					require.False(t, svc.HandleUpstreamError(context.Background(), account, status, http.Header{}, tempUnschedScopeBody, "model"))
					require.False(t, svc.HandleTempUnschedulable(context.Background(), account, status, tempUnschedScopeBody, "model"))
					gateway := &OpenAIGatewayService{rateLimitService: svc}
					require.False(t, gateway.handleOpenAIAccountUpstreamError(context.Background(), account, status, http.Header{}, tempUnschedScopeBody, "model"))
					require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
					require.Zero(t, repo.tempCalls)
					require.Zero(t, repo.errorCalls)
					require.Zero(t, repo.rateCalls)
					require.Zero(t, repo.overloadCalls)
					require.Empty(t, repo.modelRateLimitCalls)
					require.Empty(t, blocker.accounts)
					require.Equal(t, StatusActive, account.Status)
					require.True(t, account.Schedulable)
				})
			}
		}
	}
}

func TestPoolModeProbeFailureDoesNotDisableAccount(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		for _, pool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pool_%t", platform, pool), func(t *testing.T) {
				status := http.StatusForbidden
				if platform == PlatformOpenAI {
					status = http.StatusUnauthorized
				}
				upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(status, `{"error":{"message":"denied"}}`)}}
				repo := &openAIAccountTestRepo{}
				svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{}}
				account := &Account{ID: 202, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"pool_mode": pool, "api_key": "test-key", "base_url": "https://upstream.example"}}
				c, _ := newTestContext()
				var err error
				if platform == PlatformAnthropic {
					err = svc.testClaudeAccountConnection(c, account, "claude-haiku-4-5")
				} else {
					err = svc.testOpenAIAccountConnection(c, account, "gpt-5.4", "", "")
				}
				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				if pool {
					require.Zero(t, repo.setErrorID)
				} else {
					require.Equal(t, account.ID, repo.setErrorID)
				}
			})
		}
	}
}

func TestPoolModeStreamTimeoutDoesNotEscalate(t *testing.T) {
	for _, action := range []string{StreamTimeoutActionTempUnsched, StreamTimeoutActionError} {
		for _, pool := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pool_%t", action, pool), func(t *testing.T) {
				encoded, err := json.Marshal(StreamTimeoutSettings{Enabled: true, Action: action, ThresholdCount: 1, ThresholdWindowMinutes: 1, TempUnschedMinutes: 10})
				require.NoError(t, err)
				settings := NewSettingService(&openAIAPIKeyHealthSettingRepo{value: string(encoded)}, &config.Config{})
				account := newTempUnschedScopeAccount("", nil)
				account.Credentials["pool_mode"] = pool
				repo := &poolNoCooldownRepo{account: account}
				blocker := &runtimeBlockRecorder{}
				svc := &RateLimitService{accountRepo: repo, settingService: settings, runtimeBlocker: blocker}
				require.Equal(t, !pool, svc.HandleStreamTimeout(context.Background(), account, "model"))
				if pool {
					require.Zero(t, repo.tempCalls)
					require.Zero(t, repo.errorCalls)
					require.Empty(t, blocker.accounts)
				} else if action == StreamTimeoutActionTempUnsched {
					require.Equal(t, 1, repo.tempCalls)
				} else {
					require.Equal(t, 1, repo.errorCalls)
				}
			})
		}
	}
}

func TestPoolModeModelErrorsAndManualDisableRemainIsolated(t *testing.T) {
	account := newTempUnschedScopeAccount(TempUnschedScopeModel, nil)
	account.Credentials["pool_mode"] = true
	account.Credentials["custom_error_codes_enabled"] = true
	account.Schedulable = false
	repo := &poolNoCooldownRepo{account: account}
	svc := &RateLimitService{accountRepo: repo}
	gateway := &OpenAIGatewayService{accountRepo: repo}
	gateway.coolOpenAIImagesInsufficientBalance(context.Background(), account)
	gateway.coolOpenAIImagesOAuthTool(context.Background(), account)
	require.False(t, svc.HandleUpstreamModelNotFound(context.Background(), account, "gpt-5.4", 404, []byte(`{"error":{"code":"model_not_found","message":"model not found"}}`)))
	require.False(t, svc.HandleOpenAIImageRateLimit(context.Background(), account, 429, http.Header{}, []byte(`{"error":{"message":"image generation rate limit exceeded"}}`)))
	require.False(t, svc.HandleOpenAIImageCapabilityLoss(context.Background(), account, 403, []byte(`{"error":{"message":"image generation is not allowed"}}`)))
	require.False(t, svc.HandleStreamTimeout(context.Background(), account, "gpt-5.4"))
	require.False(t, svc.HandleUpstreamError(context.Background(), account, 429, http.Header{}, tempUnschedScopeBody))
	require.False(t, account.Schedulable, "pool protection must never reopen an administrator-disabled account")
	require.Zero(t, repo.tempCalls)
	require.Zero(t, repo.errorCalls)
	require.Zero(t, repo.rateCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestPoolModeRetryExhaustionDoesNotCooldown(t *testing.T) {
	for _, pool := range []bool{false, true} {
		for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway} {
			t.Run(fmt.Sprintf("pool_%t/status_%d", pool, status), func(t *testing.T) {
				account := newTempUnschedScopeAccount("", nil)
				account.Credentials["pool_mode"] = pool
				repo := &poolNoCooldownRepo{account: account}
				svc := &GatewayService{accountRepo: repo}
				svc.TempUnscheduleRetryableError(context.Background(), account.ID, &UpstreamFailoverError{StatusCode: status, RetryableOnSameAccount: true})
				if pool {
					require.Zero(t, repo.tempCalls)
				} else {
					require.Equal(t, 1, repo.tempCalls)
				}
			})
		}
	}
}

func TestPoolModeTransportFailureKeepsAccountWithoutRetryOptIn(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		for _, proxied := range []bool{false, true} {
			t.Run(fmt.Sprintf("openai_%t/proxied_%t", openAI, proxied), func(t *testing.T) {
				account := newPoolModeTransportTestAccount([]any{})
				account.Status, account.Schedulable = StatusActive, true
				if proxied {
					id := int64(7)
					account.ProxyID = &id
				}
				repo := &transportTempUnschedRepoStub{}
				c := newTransportErrorTestGin(t)
				var err error
				transportErr := errors.New("dial tcp: connection refused")
				if openAI {
					account.Platform = PlatformOpenAI
					svc := &OpenAIGatewayService{accountRepo: repo}
					err = svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, transportErr, false)
					require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
				} else {
					svc := &GatewayService{accountRepo: repo}
					err = svc.handleUpstreamTransportError(context.Background(), c, account, transportErr, OpsUpstreamErrorEvent{})
				}
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.False(t, failover.RetryableOnSameAccount, "an empty retry-code list must still disable retries")
				require.Zero(t, repo.calls)
				require.False(t, c.Writer.Written())
				require.True(t, account.Schedulable)
			})
		}
	}
}
