//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInflightReservation_EstimatesDisabledBypassExistingHolds(t *testing.T) {
	svc, cache, key := newInflightWaitTestService(t)
	svc.cfg.Billing.InflightReservation.ReserveEstimates = false
	ctx := context.Background()
	allowed, _, err := cache.ReserveInflightBalance(ctx, key.User.ID, "old-request", 100, 10, time.Minute)
	require.NoError(t, err)
	require.True(t, allowed)
	cache.attempts.Store(0)
	require.True(t, svc.InflightReservationEnabled())
	require.False(t, svc.EstimatedInflightReservationEnabled())
	require.NoError(t, svc.CheckBillingEligibility(ctx, key.User, key, nil, nil, ""))

	res, err := svc.ReserveInflight(ctx, key.User, nil, nil, 100)
	require.NoError(t, err)
	require.Nil(t, res)
	done, err := svc.ReserveInflightBalance(ctx, key.User, nil, nil, 100)
	require.NoError(t, err)
	done()
	res, err = svc.ReserveInflightWithWait(ctx, key, nil, 100, "")
	require.NoError(t, err)
	require.Nil(t, res)
	assertInflightWaitersEmpty(t, svc)
	require.Zero(t, cache.attempts.Load(), "disabled estimates do not touch reservation admission")
	require.Zero(t, cache.releaseCt.Load(), "do not clear an unrelated request's pending hold")
	require.Zero(t, cache.renews.Load())
	require.Equal(t, 1, cache.count())
	balance, err := cache.GetUserBalance(ctx, key.User.ID)
	require.NoError(t, err)
	require.Equal(t, 10.0, balance)

	require.NoError(t, cache.DeductUserBalance(ctx, key.User.ID, 10))
	require.ErrorIs(t, svc.CheckBillingEligibility(ctx, key.User, key, nil, nil, ""), ErrInsufficientBalance,
		"the independent real-balance gate still rejects an exhausted balance")
}

func TestInflightReservation_EstimatesDisabledStillChargesActualUsageOnce(t *testing.T) {
	cache := newMemInflightCache(10)
	svc := newInflightSvc(t, cache, 60)
	svc.cfg.Billing.InflightReservation.ReserveEstimates = false
	user := &User{ID: 71, Balance: 10}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := svc.ReserveInflight(ctx, user, nil, nil, 7)
	require.NoError(t, err)
	require.Nil(t, res)
	res.HandlerDone()
	cancel()

	balanceAfterCharge := 9.75
	repo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true, NewBalance: &balanceAfterCharge}}
	params := &postUsageBillingParams{
		Cost: &CostBreakdown{ActualCost: 0.25, TotalCost: 0.25}, User: user,
		APIKey: &APIKey{ID: 9}, Account: &Account{ID: 3, Type: AccountTypeAPIKey}, AccountRateMultiplier: 1,
	}
	usage := &UsageLog{RequestID: "estimate-disabled-actual-charge", UserID: user.ID, APIKeyID: 9, AccountID: 3, ActualCost: 0.25}
	deps := &billingDeps{billingCacheService: svc, deferredService: &DeferredService{}}
	applied, err := applyUsageBilling(ctx, usage.RequestID, usage, params, deps, repo)
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, repo.lastCtxErr)
	require.Equal(t, 0.25, repo.lastCmd.BalanceCost)
	require.EqualValues(t, 1, cache.deducts.Load(), "cache settlement remains synchronous without estimated holds")
	repo.result = &UsageBillingApplyResult{Applied: false}
	applied, err = applyUsageBilling(ctx, usage.RequestID, usage, params, deps, repo)
	require.NoError(t, err)
	require.False(t, applied)
	require.EqualValues(t, 1, cache.deducts.Load(), "a duplicate billing replay cannot charge twice")
	balance, err := cache.GetUserBalance(context.Background(), user.ID)
	require.NoError(t, err)
	require.Equal(t, balanceAfterCharge, balance)
	require.Zero(t, cache.count())
	require.Zero(t, cache.releaseCt.Load())
}

func TestInflightReservation_EstimatesDisabledOverdraftStillBlocksNextRequest(t *testing.T) {
	cache := &balanceEligibilityCacheStub{balance: 0.10, cacheMissAfterInvalidate: true}
	svc := newInflightSvc(t, cache, 60)
	svc.cfg.Billing.InflightReservation.ReserveEstimates = false
	balanceAfterCharge := -0.15
	svc.userRepo = &rejectionBalanceUserRepo{balance: balanceAfterCharge}
	user := &User{ID: 71, Status: StatusActive, Balance: 0.10}
	ctx := context.Background()
	require.NoError(t, svc.CheckBillingEligibility(ctx, user, nil, nil, nil, ""))
	res, err := svc.ReserveInflight(ctx, user, nil, nil, 7)
	require.NoError(t, err)
	require.Nil(t, res, "a positive balance no longer has to cover the estimated cost")

	repo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{
		Applied: true, NewBalance: &balanceAfterCharge, BalanceOverdrafted: true,
	}}
	params := &postUsageBillingParams{
		Cost: &CostBreakdown{ActualCost: 0.25, TotalCost: 0.25}, User: user,
		APIKey: &APIKey{ID: 9}, Account: &Account{ID: 3, Type: AccountTypeAPIKey}, AccountRateMultiplier: 1,
	}
	usage := &UsageLog{RequestID: "estimate-disabled-overdraft", UserID: user.ID, APIKeyID: 9, AccountID: 3, ActualCost: 0.25}
	deps := &billingDeps{billingCacheService: svc, deferredService: &DeferredService{}}
	applied, err := applyUsageBilling(ctx, usage.RequestID, usage, params, deps, repo)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 0.25, repo.lastCmd.BalanceCost)
	require.EqualValues(t, 1, cache.invalidateCalls.Load(), "do not keep admitting against a positive stale cache")
	require.Zero(t, cache.deductCalls.Load(), "after invalidation, the next read uses authoritative balance")
	require.ErrorIs(t, svc.CheckBillingEligibility(ctx, user, nil, nil, nil, ""), ErrInsufficientBalance)
}
