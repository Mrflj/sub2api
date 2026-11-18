package repository

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type inflightBalanceAuthorityStub struct {
	service.UserRepository
	balance float64
	calls   atomic.Int64
}

func (r *inflightBalanceAuthorityStub) GetByID(_ context.Context, id int64) (*service.User, error) {
	r.calls.Add(1)
	return &service.User{ID: id, Status: service.StatusActive, Balance: r.balance}, nil
}

func TestInflightReservation_RedisStaleBalanceRecheckedAtOneDollarFiftyTwo(t *testing.T) {
	_, cache, _ := newInflightTestEnv(t, true, 60)
	ctx := context.Background()
	users := &inflightBalanceAuthorityStub{balance: 1.52}
	cfg := &config.Config{}
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, ReserveEstimates: true, TTLSeconds: 60}
	svc := service.NewBillingCacheService(cache, users, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	user := &service.User{ID: 71, Status: service.StatusActive, Balance: 1.52}
	require.NoError(t, cache.SetUserBalance(ctx, user.ID, 0.25))
	first, err := svc.ReserveInflight(ctx, user, nil, nil, 0.20)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	second, err := svc.ReserveInflight(ctx, user, nil, nil, 0.50)
	require.NoError(t, err, "the production Lua script must admit using authoritative $1.52, not stale $0.25")
	require.NotNil(t, second)
	t.Cleanup(second.Release)
	require.EqualValues(t, 1, users.calls.Load())
	require.EqualValues(t, 2, inflightCount(t, cache, user.ID))
	_, err = cache.GetUserBalance(ctx, user.ID)
	require.ErrorIs(t, err, redis.Nil, "invalidate the stale cached value without racing a new SET")
	first.HandlerDone()
	second.HandlerDone()
	require.Zero(t, inflightCount(t, cache, user.ID))
	require.Equal(t, 1.52, users.balance)
}

func TestInflightReservation_RedisBusyAtOneDollarFiftyTwoReleasesWithoutRecharge(t *testing.T) {
	_, cache, svc := newInflightTestEnv(t, true, 60)
	ctx := context.Background()
	user := &service.User{ID: 72, Balance: 1.52}
	require.NoError(t, cache.SetUserBalance(ctx, user.ID, 1.52))
	first, err := svc.ReserveInflight(ctx, user, nil, nil, 0.90)
	require.NoError(t, err)
	t.Cleanup(first.Release)

	second, err := svc.ReserveInflight(ctx, user, nil, nil, 0.90)
	require.Nil(t, second)
	require.ErrorIs(t, err, service.ErrInflightBalanceBusy)
	require.NotErrorIs(t, err, service.ErrInsufficientBalance)
	balance, err := cache.GetUserBalance(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 1.52, balance)

	first.HandlerDone()
	second, err = svc.ReserveInflight(ctx, user, nil, nil, 0.90)
	require.NoError(t, err)
	second.HandlerDone()
	require.Zero(t, inflightCount(t, cache, user.ID))
}

func TestInflightReservation_RedisOldHoldsIgnoredWhenEstimatesDisabled(t *testing.T) {
	_, cache, _ := newInflightTestEnv(t, true, 60)
	ctx := context.Background()
	user := &service.User{ID: 73, Status: service.StatusActive, Balance: 10}
	require.NoError(t, cache.SetUserBalance(ctx, user.ID, 10))
	allowed, _, err := cache.ReserveInflightBalance(ctx, user.ID, "old-request", 100, 10, time.Minute)
	require.NoError(t, err)
	require.True(t, allowed)
	cfg := &config.Config{}
	cfg.Billing.InflightReservation.Enabled = true
	svc := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(svc.Stop)
	require.NoError(t, svc.CheckBillingEligibility(ctx, user, nil, nil, nil, ""))
	res, err := svc.ReserveInflight(ctx, user, nil, nil, 7)
	require.NoError(t, err)
	require.Nil(t, res)
	res.HandlerDone()
	require.EqualValues(t, 1, inflightCount(t, cache, user.ID))
	balance, err := cache.GetUserBalance(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 10.0, balance)

	_, err = svc.ReserveFixedCharge(ctx, user, nil, nil, nil, 1)
	require.ErrorIs(t, err, service.ErrInflightBalanceBusy, "known-price charges still account for existing monetary holds")
	require.EqualValues(t, 1, inflightCount(t, cache, user.ID))
}
